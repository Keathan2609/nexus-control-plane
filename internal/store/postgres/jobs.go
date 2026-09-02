// Package postgres implements the storage interfaces the domain packages declare.
package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Keathan2609/nexus/internal/core/domain"
	"github.com/Keathan2609/nexus/internal/platform/database"
)

// JobStore is the execution plane's entire interface to durable job state.
//
// Everything here is expressed as a single conditional statement rather than as
// read-then-write. That is deliberate: a read-modify-write over a job row is a race
// between however many workers and reconcilers happen to be running, and no amount of
// application-level care closes it. Making each operation one statement pushes the
// concurrency control into Postgres, where it belongs.
type JobStore struct{ db *database.DB }

func NewJobStore(db *database.DB) *JobStore { return &JobStore{db: db} }

// ErrNotOwner is returned when a worker acts on a job it does not currently hold the
// lease for — typically because the job was reaped and reassigned while the worker was
// partitioned. The worker's correct response is to abandon the work, not to retry.
var ErrNotOwner = errors.New("job is not held by this worker")

type ClaimedJob struct {
	ID             domain.ID
	RunID          domain.ID
	ProjectID      domain.ID
	StepKey        string
	Name           string
	Type           string
	Attempt        int
	MaxAttempts    int
	TimeoutSeconds int
	Spec           json.RawMessage
	LeaseExpiresAt time.Time
}

// claimSQL is the heart of the execution plane.
//
// The inner SELECT ... FOR UPDATE SKIP LOCKED is what makes a worker fleet safe without
// any distributed lock, leader election or external queue. Each worker locks one
// candidate row; SKIP LOCKED means a worker that finds a row already locked moves past
// it to the next rather than blocking. Twelve workers hitting this simultaneously get
// twelve different jobs, and the loser of any race is never even delayed.
//
// The ORDER BY includes id as a final tiebreak so that two jobs queued in the same
// millisecond with equal priority are still claimed in a stable, defined order.
//
// %s is the optional project predicate, not user input — see claim().
const claimSQL = `
UPDATE jobs SET
    state            = 'RUNNING',
    worker_id        = $1,
    attempt          = attempt + 1,
    lease_expires_at = now() + make_interval(secs => $2),
    heartbeat_at     = now(),
    started_at       = COALESCE(started_at, now()),
    error_code       = NULL,
    error_message    = NULL
WHERE id = (
    SELECT id FROM jobs
    WHERE state = 'QUEUED'
      AND run_at <= now()
      %s
    ORDER BY priority DESC, run_at, id
    FOR UPDATE SKIP LOCKED
    LIMIT 1
)
RETURNING id, run_id, project_id, step_key, name, type,
          attempt, max_attempts, timeout_seconds, spec, lease_expires_at`

// Claim takes the next eligible job from anywhere in the system.
func (s *JobStore) Claim(ctx context.Context, workerID string, lease time.Duration) (*ClaimedJob, error) {
	return s.claim(ctx, "", workerID, lease)
}

// ClaimForProject restricts the claim to one project. Used by tests and by any future
// per-tenant worker pool.
func (s *JobStore) ClaimForProject(ctx context.Context, projectID domain.ID, workerID string, lease time.Duration) (*ClaimedJob, error) {
	return s.claim(ctx, projectID, workerID, lease)
}

func (s *JobStore) claim(ctx context.Context, projectID domain.ID, workerID string, lease time.Duration) (*ClaimedJob, error) {
	var (
		query string
		args  []any
	)
	if projectID == "" {
		query = fmt.Sprintf(claimSQL, "")
		args = []any{workerID, lease.Seconds()}
	} else {
		query = fmt.Sprintf(claimSQL, "AND project_id = $3")
		args = []any{workerID, lease.Seconds(), projectID}
	}

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin claim: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var j ClaimedJob
	err = tx.QueryRow(ctx, query, args...).Scan(
		&j.ID, &j.RunID, &j.ProjectID, &j.StepKey, &j.Name, &j.Type,
		&j.Attempt, &j.MaxAttempts, &j.TimeoutSeconds, &j.Spec, &j.LeaseExpiresAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		// An empty queue is the normal case, not an error. Returning (nil, nil) keeps
		// the worker loop free of sentinel-error handling.
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("claim job: %w", err)
	}

	// Attempt history is written in the same transaction as the claim, so an attempt
	// row exists for every attempt that was ever started — including ones whose worker
	// then died without reporting anything.
	reason := "INITIAL"
	if j.Attempt > 1 {
		reason = "RETRY"
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO job_attempts (id, job_id, run_id, attempt, state, worker_id, retry_reason)
		 VALUES ($1,$2,$3,$4,'RUNNING',$5,$6)
		 ON CONFLICT (job_id, attempt) DO UPDATE SET worker_id = EXCLUDED.worker_id, state = 'RUNNING'`,
		domain.NewID(domain.PrefixAttempt), j.ID, j.RunID, j.Attempt, workerID, reason,
	); err != nil {
		return nil, fmt.Errorf("record attempt: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit claim: %w", err)
	}
	return &j, nil
}

// Heartbeat extends the lease on a job the worker still holds.
//
// The worker_id predicate is the safety property: a worker whose job was reaped and
// handed to someone else cannot resurrect its ownership by continuing to heartbeat. It
// gets ErrNotOwner and stops.
func (s *JobStore) Heartbeat(ctx context.Context, jobID domain.ID, workerID string, lease time.Duration) error {
	tag, err := s.db.Exec(ctx,
		`UPDATE jobs
		    SET lease_expires_at = now() + make_interval(secs => $3),
		        heartbeat_at     = now()
		  WHERE id = $1 AND worker_id = $2 AND state = 'RUNNING'`,
		jobID, workerID, lease.Seconds())
	if err != nil {
		return fmt.Errorf("heartbeat: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotOwner
	}
	return nil
}

// ReapExpiredLeases returns jobs whose worker stopped renewing to the queue, or fails
// them if they have no attempts left.
//
// This is the whole of NEXUS's worker-failure handling. There is no supervisor, no
// worker registry to keep consistent, and no distinction between a crash, a kill, a
// network partition and a graceful stop that did not finish: all of them are simply the
// absence of a heartbeat, and all of them are recovered by this query.
//
// backoff delays the retry, so a worker that is crash-looping on one poisonous job does
// not spin.
func (s *JobStore) ReapExpiredLeases(ctx context.Context, backoff time.Duration) (int, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin reap: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Requeue those with budget remaining.
	requeued, err := tx.Exec(ctx,
		`UPDATE jobs
		    SET state = 'QUEUED',
		        worker_id = NULL,
		        lease_expires_at = NULL,
		        run_at = now() + make_interval(secs => $1),
		        error_code = 'E_LEASE_EXPIRED',
		        error_message = 'worker stopped renewing its lease'
		  WHERE state = 'RUNNING'
		    AND lease_expires_at < now()
		    AND attempt < max_attempts`,
		backoff.Seconds())
	if err != nil {
		return 0, fmt.Errorf("requeue expired: %w", err)
	}

	// Fail those that have exhausted it.
	failed, err := tx.Exec(ctx,
		`UPDATE jobs
		    SET state = 'FAILED',
		        worker_id = NULL,
		        lease_expires_at = NULL,
		        finished_at = now(),
		        error_code = 'E_LEASE_EXPIRED',
		        error_message = 'worker stopped renewing its lease and no attempts remain'
		  WHERE state = 'RUNNING'
		    AND lease_expires_at < now()
		    AND attempt >= max_attempts`)
	if err != nil {
		return 0, fmt.Errorf("fail expired: %w", err)
	}

	// Close out the corresponding attempt rows so attempt history does not show an
	// attempt that is still RUNNING on a job that is not.
	if _, err := tx.Exec(ctx,
		`UPDATE job_attempts a
		    SET state = 'FAILED',
		        finished_at = now(),
		        error_code = 'E_LEASE_EXPIRED',
		        error_message = 'worker stopped renewing its lease'
		  FROM jobs j
		 WHERE a.job_id = j.id
		   AND a.state = 'RUNNING'
		   AND j.state <> 'RUNNING'`); err != nil {
		return 0, fmt.Errorf("close attempts: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit reap: %w", err)
	}
	return int(requeued.RowsAffected() + failed.RowsAffected()), nil
}

// Complete marks a job successful. Only the lease holder may.
func (s *JobStore) Complete(ctx context.Context, jobID domain.ID, workerID string) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin complete: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var attempt int
	err = tx.QueryRow(ctx,
		`UPDATE jobs
		    SET state = 'SUCCEEDED',
		        finished_at = now(),
		        worker_id = NULL,
		        lease_expires_at = NULL
		  WHERE id = $1 AND worker_id = $2 AND state = 'RUNNING'
		  RETURNING attempt`,
		jobID, workerID).Scan(&attempt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotOwner
	}
	if err != nil {
		return fmt.Errorf("complete job: %w", err)
	}

	if _, err := tx.Exec(ctx,
		`UPDATE job_attempts SET state='SUCCEEDED', finished_at=now()
		  WHERE job_id=$1 AND attempt=$2`, jobID, attempt); err != nil {
		return fmt.Errorf("close attempt: %w", err)
	}
	return tx.Commit(ctx)
}

// Fail records a failed attempt. It reports whether the job was requeued for another
// attempt (true) or has reached a terminal FAILED state (false).
//
// The retry decision is made inside the statement, from the row's own attempt counters,
// rather than read out and decided in Go. That keeps the decision atomic with the write
// and means two concurrent failure reports cannot both conclude "one attempt left".
func (s *JobStore) Fail(ctx context.Context, jobID domain.ID, workerID, code, message string, backoff time.Duration) (bool, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin fail: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		newState string
		attempt  int
	)
	err = tx.QueryRow(ctx,
		`UPDATE jobs
		    SET state = CASE WHEN attempt < max_attempts THEN 'QUEUED' ELSE 'FAILED' END,
		        run_at = CASE WHEN attempt < max_attempts
		                      THEN now() + make_interval(secs => $5)
		                      ELSE run_at END,
		        finished_at = CASE WHEN attempt < max_attempts THEN NULL ELSE now() END,
		        worker_id = NULL,
		        lease_expires_at = NULL,
		        error_code = $3,
		        error_message = $4
		  WHERE id = $1 AND worker_id = $2 AND state = 'RUNNING'
		  RETURNING state, attempt`,
		jobID, workerID, code, message, backoff.Seconds()).Scan(&newState, &attempt)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrNotOwner
	}
	if err != nil {
		return false, fmt.Errorf("fail job: %w", err)
	}

	if _, err := tx.Exec(ctx,
		`UPDATE job_attempts
		    SET state='FAILED', finished_at=now(), error_code=$3, error_message=$4
		  WHERE job_id=$1 AND attempt=$2`, jobID, attempt, code, message); err != nil {
		return false, fmt.Errorf("close attempt: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit fail: %w", err)
	}
	return newState == "QUEUED", nil
}

// AppendLog stores one line of job output.
func (s *JobStore) AppendLog(ctx context.Context, jobID domain.ID, attempt, seq int, stream, message string) error {
	_, err := s.db.Exec(ctx,
		`INSERT INTO job_logs (job_id, attempt, seq, stream, message)
		 VALUES ($1,$2,$3,$4,$5)
		 ON CONFLICT (job_id, attempt, seq) DO NOTHING`,
		jobID, attempt, seq, stream, message)
	if err != nil {
		return fmt.Errorf("append log: %w", err)
	}
	return nil
}
