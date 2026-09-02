// Package scheduler advances pipeline runs through their DAG.
//
// The scheduler owns exactly one question: given what has finished, what may start now?
// It does not execute anything — workers do that — and it holds no in-memory state, so
// any replica can advance any run and a restart loses nothing.
//
// Every decision is expressed as a conditional SQL statement evaluated against the jobs
// of one run. That is what makes Advance safe to call concurrently and repeatedly: the
// database, not the caller, decides whether a job is eligible to be released.
package scheduler

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/Keathan2609/nexus/internal/core/domain"
	"github.com/Keathan2609/nexus/internal/core/events"
	"github.com/Keathan2609/nexus/internal/core/outbox"
	"github.com/Keathan2609/nexus/internal/platform/database"
	"github.com/Keathan2609/nexus/internal/store/postgres"
)

type Scheduler struct {
	db   *database.DB
	runs *postgres.RunStore
	jobs *postgres.JobStore
}

func New(db *database.DB, runs *postgres.RunStore, jobs *postgres.JobStore) *Scheduler {
	return &Scheduler{db: db, runs: runs, jobs: jobs}
}

// Result reports what one Advance actually changed. Callers use Released to decide
// whether anything is worth publishing, and tests use it to assert idempotency.
type Result struct {
	Released int
	Skipped  int
	Finished bool
	State    domain.RunState
}

// terminalStates is repeated in SQL below rather than interpolated, because these are
// the values the CHECK constraints enforce and a mismatch should fail loudly at the
// database rather than silently match nothing.
const (
	// A dependency counts as satisfied when it succeeded, or when it failed but was
	// declared advisory. That second case is why this cannot be a simple state check.
	depSatisfied = `(d.state = 'SUCCEEDED' OR (d.state IN ('FAILED','TIMED_OUT') AND d.continue_on_error))`
	// A dependency blocks its dependents forever when it reached a terminal state that
	// was not a success and was not advisory.
	depDoomed = `(d.state IN ('FAILED','CANCELLED','SKIPPED','TIMED_OUT') AND NOT (d.state IN ('FAILED','TIMED_OUT') AND d.continue_on_error))`
)

// Advance releases every job whose dependencies are now satisfied, skips every job whose
// dependencies can no longer be satisfied, and finalises the run if nothing is left.
//
// Release and skip run in a loop rather than once. Skipping cascades: when build is
// skipped because test failed, deploy must then be skipped because build was skipped,
// and that is a second pass over the same table. Looping until a pass changes nothing
// is simpler and more obviously correct than a recursive CTE, and the DAGs involved are
// small enough that the extra round trips do not matter.
func (s *Scheduler) Advance(ctx context.Context, runID domain.ID) (Result, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("begin advance: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var res Result

	for pass := 0; pass < 64; pass++ {
		released, err := tx.Exec(ctx,
			`UPDATE jobs j
			    SET state = 'QUEUED', run_at = now()
			  WHERE j.run_id = $1
			    AND j.state = 'BLOCKED'
			    AND NOT EXISTS (
			          SELECT 1 FROM jobs d
			           WHERE d.run_id = j.run_id
			             AND d.step_key = ANY(j.depends_on)
			             AND NOT `+depSatisfied+`
			        )`, runID)
		if err != nil {
			return Result{}, fmt.Errorf("release jobs: %w", err)
		}

		skipped, err := tx.Exec(ctx,
			`UPDATE jobs j
			    SET state = 'SKIPPED', finished_at = now(),
			        error_code = 'E_UPSTREAM_FAILED',
			        error_message = 'an upstream job did not succeed'
			  WHERE j.run_id = $1
			    AND j.state = 'BLOCKED'
			    AND EXISTS (
			          SELECT 1 FROM jobs d
			           WHERE d.run_id = j.run_id
			             AND d.step_key = ANY(j.depends_on)
			             AND `+depDoomed+`
			        )`, runID)
		if err != nil {
			return Result{}, fmt.Errorf("skip jobs: %w", err)
		}

		res.Released += int(released.RowsAffected())
		res.Skipped += int(skipped.RowsAffected())

		if released.RowsAffected() == 0 && skipped.RowsAffected() == 0 {
			break
		}
	}

	// Count what remains. `pending` is jobs that could still do something; `failed` is
	// jobs that ended badly and were not advisory.
	var pending, failed, started int
	if err := tx.QueryRow(ctx,
		`SELECT
		   count(*) FILTER (WHERE state IN ('BLOCKED','QUEUED','RUNNING')),
		   count(*) FILTER (WHERE state IN ('FAILED','TIMED_OUT') AND NOT continue_on_error),
		   count(*) FILTER (WHERE state NOT IN ('BLOCKED','QUEUED'))
		 FROM jobs WHERE run_id = $1`, runID).Scan(&pending, &failed, &started); err != nil {
		return Result{}, fmt.Errorf("count jobs: %w", err)
	}

	var current domain.RunState
	if err := tx.QueryRow(ctx,
		`SELECT state FROM pipeline_runs WHERE id = $1`, runID).Scan(&current); err != nil {
		return Result{}, fmt.Errorf("read run state: %w", err)
	}
	res.State = current

	if current.IsTerminal() {
		if err := tx.Commit(ctx); err != nil {
			return Result{}, fmt.Errorf("commit advance: %w", err)
		}
		res.Finished = true
		return res, nil
	}

	// A run becomes RUNNING as soon as any job has left the queue. The state machine
	// forbids QUEUED -> SUCCEEDED, so this transition also has to happen before a run
	// that finished very fast can be finalised.
	if current == domain.RunQueued && started > 0 {
		next, err := current.TransitionTo(domain.RunRunning)
		if err != nil {
			return Result{}, err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE pipeline_runs SET state='RUNNING', started_at=COALESCE(started_at, now())
			  WHERE id=$1`, runID); err != nil {
			return Result{}, fmt.Errorf("mark run running: %w", err)
		}
		current = next
		res.State = next

		if err := outbox.Write(ctx, tx, events.New(events.PipelineStarted, "pipeline_run", runID,
			map[string]any{"run_id": runID.String()})); err != nil {
			return Result{}, err
		}
	}

	if pending == 0 {
		target := domain.RunSucceeded
		evt := events.PipelineCompleted
		if failed > 0 {
			target = domain.RunFailed
			evt = events.PipelineFailed
		}
		if _, err := current.TransitionTo(target); err != nil {
			return Result{}, err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE pipeline_runs
			    SET state = $2,
			        started_at = COALESCE(started_at, now()),
			        finished_at = now(),
			        version = version + 1
			  WHERE id = $1`, runID, string(target)); err != nil {
			return Result{}, fmt.Errorf("finalise run: %w", err)
		}
		if err := outbox.Write(ctx, tx, events.New(evt, "pipeline_run", runID,
			map[string]any{"run_id": runID.String(), "state": string(target)})); err != nil {
			return Result{}, err
		}
		res.Finished = true
		res.State = target
	}

	if err := tx.Commit(ctx); err != nil {
		return Result{}, fmt.Errorf("commit advance: %w", err)
	}
	return res, nil
}

// Cancel stops a run and everything still outstanding in it.
//
// Cancellation propagates to jobs in the same transaction as the run's own state change,
// so there is no window in which a cancelled run still has claimable work. A RUNNING job
// is marked cancelled here too; its worker discovers this on its next heartbeat, which
// returns ErrNotOwner, and abandons the step.
func (s *Scheduler) Cancel(ctx context.Context, runID domain.ID, reason string) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin cancel: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var current domain.RunState
	if err := tx.QueryRow(ctx,
		`SELECT state FROM pipeline_runs WHERE id=$1 FOR UPDATE`, runID).Scan(&current); err != nil {
		return fmt.Errorf("lock run: %w", err)
	}
	if current.IsTerminal() {
		// Cancelling a finished run is a no-op rather than an error: the user's intent
		// is already satisfied, and racing a completion should not surface as a failure.
		return tx.Commit(ctx)
	}
	if _, err := current.TransitionTo(domain.RunCancelled); err != nil {
		return err
	}

	if _, err := tx.Exec(ctx,
		`UPDATE jobs
		    SET state = 'CANCELLED', finished_at = now(),
		        worker_id = NULL, lease_expires_at = NULL,
		        error_code = 'E_CANCELLED', error_message = $2
		  WHERE run_id = $1 AND state IN ('BLOCKED','QUEUED','RUNNING')`,
		runID, reason); err != nil {
		return fmt.Errorf("cancel jobs: %w", err)
	}

	if _, err := tx.Exec(ctx,
		`UPDATE pipeline_runs
		    SET state='CANCELLED',
		        started_at = COALESCE(started_at, now()),
		        finished_at = now(),
		        version = version + 1
		  WHERE id=$1`, runID); err != nil {
		return fmt.Errorf("cancel run: %w", err)
	}

	if err := outbox.Write(ctx, tx, events.New(events.PipelineCancelled, "pipeline_run", runID,
		map[string]any{"run_id": runID.String(), "reason": reason})); err != nil {
		return err
	}

	return tx.Commit(ctx)
}

// AdvanceActive advances every run that is not yet terminal.
//
// This is the safety net behind event-driven advancement: if an event is lost, delayed
// or never consumed, the next tick of this loop still moves the run forward. It is the
// reason a NATS outage degrades latency rather than stalling delivery.
func (s *Scheduler) AdvanceActive(ctx context.Context, limit int) (int, error) {
	rows, err := s.db.Query(ctx,
		`SELECT id FROM pipeline_runs
		  WHERE state IN ('PENDING','QUEUED','RUNNING')
		  ORDER BY id
		  LIMIT $1`, limit)
	if err != nil {
		return 0, fmt.Errorf("list active runs: %w", err)
	}
	var ids []domain.ID
	for rows.Next() {
		var id domain.ID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	advanced := 0
	for _, id := range ids {
		if _, err := s.Advance(ctx, id); err != nil {
			// One poisoned run must not stop the loop from advancing the others.
			continue
		}
		advanced++
	}
	return advanced, nil
}

var _ = pgx.ErrNoRows
