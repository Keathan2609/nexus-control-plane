// Package reconciler is the control loop that closes gaps.
//
// Everything else in NEXUS is event-driven and therefore only as reliable as the events
// reaching their consumer. This package assumes nothing arrives. Each loop reads the
// database, compares what is true with what should be true, and corrects the difference
// — which makes it the component that turns a lost event, a killed worker or a partial
// deployment from a permanent inconsistency into a delay of one interval.
//
// This is the same shape as a Kubernetes controller: level-triggered, not
// edge-triggered. It does not care why the world diverged, only that it has.
package reconciler

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/Keathan2609/nexus/internal/core/domain"
	"github.com/Keathan2609/nexus/internal/core/events"
	"github.com/Keathan2609/nexus/internal/core/outbox"
	"github.com/Keathan2609/nexus/internal/deployments"
	"github.com/Keathan2609/nexus/internal/platform/database"
	"github.com/Keathan2609/nexus/internal/platform/logging"
	"github.com/Keathan2609/nexus/internal/platform/telemetry"
	"github.com/Keathan2609/nexus/internal/scheduler"
	"github.com/Keathan2609/nexus/internal/store/postgres"
)

type Config struct {
	Interval time.Duration
	// LeaseBackoff delays a reaped job's retry, so a worker crash-looping on one
	// poisonous job does not spin the whole fleet.
	LeaseBackoff time.Duration
	// StuckDeploymentAge is how long a deployment may sit mid-flight before it is
	// assumed to belong to a control plane that died and is failed.
	StuckDeploymentAge time.Duration
	OutboxRetention    time.Duration
	ProcessedRetention time.Duration
}

func DefaultConfig() Config {
	return Config{
		Interval:           10 * time.Second,
		LeaseBackoff:       5 * time.Second,
		StuckDeploymentAge: 30 * time.Minute,
		OutboxRetention:    48 * time.Hour,
		ProcessedRetention: 24 * time.Hour,
	}
}

type Reconciler struct {
	cfg     Config
	db      *database.DB
	jobs    *postgres.JobStore
	sched   *scheduler.Scheduler
	deploys *deployments.Service
	relay   *outbox.Relay
	log     *slog.Logger
	metrics *telemetry.Metrics
}

func New(cfg Config, db *database.DB, jobs *postgres.JobStore, sched *scheduler.Scheduler,
	deploys *deployments.Service, relay *outbox.Relay, log *slog.Logger, m *telemetry.Metrics) *Reconciler {
	return &Reconciler{cfg: cfg, db: db, jobs: jobs, sched: sched,
		deploys: deploys, relay: relay, log: log, metrics: m}
}

// Run executes every loop on a tick until the context is cancelled.
//
// One slow or failing loop must not stop the others, so each is called independently and
// its error is logged rather than returned. A reconciler that stops reconciling because
// drift detection hit a transient error would also stop reaping leases, which is the
// loop that recovers crashed workers.
func (r *Reconciler) Run(ctx context.Context) error {
	ticker := time.NewTicker(r.cfg.Interval)
	defer ticker.Stop()

	r.log.Info("reconciler started", "interval", r.cfg.Interval.String())
	for {
		select {
		case <-ctx.Done():
			r.log.Info("reconciler stopping")
			return ctx.Err()
		case <-ticker.C:
			r.tick(ctx)
		}
	}
}

func (r *Reconciler) tick(ctx context.Context) {
	type loop struct {
		name string
		fn   func(context.Context) error
	}
	for _, l := range []loop{
		{"reap_leases", r.reapLeases},
		{"enforce_timeouts", r.enforceTimeouts},
		{"advance_runs", r.advanceRuns},
		{"recover_stuck_deployments", r.recoverStuckDeployments},
		{"detect_drift", r.detectDrift},
		{"expire_previews", r.expirePreviewEnvironments},
		{"mark_lost_workers", r.markLostWorkers},
		{"refresh_gauges", r.refreshGauges},
		{"prune", r.prune},
	} {
		if err := l.fn(ctx); err != nil {
			if errors.Is(err, context.Canceled) {
				return
			}
			r.log.Error("reconcile loop failed", "loop", l.name, logging.FieldError, err)
		}
	}
}

// reapLeases returns jobs whose worker stopped renewing.
func (r *Reconciler) reapLeases(ctx context.Context) error {
	n, err := r.jobs.ReapExpiredLeases(ctx, r.cfg.LeaseBackoff)
	if err != nil {
		return err
	}
	if n > 0 {
		r.log.Warn("recovered jobs from workers that stopped heartbeating", "count", n)
		if r.metrics != nil {
			r.metrics.LeasesReaped.Add(float64(n))
		}
	}
	return nil
}

// enforceTimeouts fails jobs that have exceeded their declared timeout.
//
// The worker enforces its own timeout too. This exists for the case where the worker is
// gone: without it, a job whose worker died would be reaped and retried forever rather
// than eventually being called what it is.
func (r *Reconciler) enforceTimeouts(ctx context.Context) error {
	tag, err := r.db.Exec(ctx,
		`UPDATE jobs
		    SET state = CASE WHEN attempt < max_attempts THEN 'QUEUED' ELSE 'TIMED_OUT' END,
		        finished_at = CASE WHEN attempt < max_attempts THEN NULL ELSE now() END,
		        worker_id = NULL,
		        lease_expires_at = NULL,
		        error_code = 'E_TIMEOUT',
		        error_message = 'exceeded the declared step timeout'
		  WHERE state = 'RUNNING'
		    AND started_at IS NOT NULL
		    AND started_at < now() - make_interval(secs => timeout_seconds)`)
	if err != nil {
		return err
	}
	if n := tag.RowsAffected(); n > 0 {
		r.log.Warn("jobs exceeded their timeout", "count", n)
	}
	return nil
}

// advanceRuns is the safety net behind event-driven scheduling.
func (r *Reconciler) advanceRuns(ctx context.Context) error {
	_, err := r.sched.AdvanceActive(ctx, 200)
	return err
}

// recoverStuckDeployments fails deployments abandoned mid-flight.
//
// A deployment in PROVISIONING, DEPLOYING or VERIFYING is being driven by a goroutine in
// some API replica. If that replica is killed, nothing else is watching, and the row
// stays mid-flight forever. Failing it after a bounded age turns an invisible orphan
// into a visible failure that a person can roll back from.
func (r *Reconciler) recoverStuckDeployments(ctx context.Context) error {
	rows, err := r.db.Query(ctx,
		`SELECT id, environment_id FROM deployments
		  WHERE state IN ('REQUESTED','PROVISIONING','DEPLOYING','VERIFYING')
		    AND requested_at < now() - make_interval(secs => $1)`,
		r.cfg.StuckDeploymentAge.Seconds())
	if err != nil {
		return err
	}
	type stuck struct{ id, env domain.ID }
	var found []stuck
	for rows.Next() {
		var s stuck
		if err := rows.Scan(&s.id, &s.env); err != nil {
			rows.Close()
			return err
		}
		found = append(found, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for _, s := range found {
		tx, err := r.db.Begin(ctx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE deployments
			    SET state='FAILED', health_status='UNKNOWN', finished_at=now(),
			        started_at=COALESCE(started_at, now()), version=version+1
			  WHERE id=$1 AND state IN ('REQUESTED','PROVISIONING','DEPLOYING','VERIFYING')`,
			s.id); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		if err := outbox.Write(ctx, tx, events.New(events.DeploymentFailed, "deployment", s.id,
			map[string]any{
				"environment_id": s.env.String(),
				"reason":         "the control plane driving this deployment stopped before it finished",
			})); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
		r.log.Warn("failed a deployment abandoned mid-flight", logging.FieldDeploymentID, s.id.String())
	}
	return nil
}

// detectDrift compares desired state with what the applier actually observes.
//
// This is the loop that makes NEXUS a control plane rather than a deployment script. It
// does not assume that what it applied is what is running: someone can edit a live
// object, a node can fail, an operator can scale something by hand. Drift is recorded
// as a fact and surfaced, and the reconciliation policy decides what to do about it.
func (r *Reconciler) detectDrift(ctx context.Context) error {
	if r.deploys == nil || r.deploys.Applier == nil {
		return nil
	}

	rows, err := r.db.Query(ctx,
		`SELECT d.id, d.environment_id, d.desired_revision, COALESCE(d.actual_revision,'')
		   FROM deployments d
		  WHERE d.state = 'HEALTHY'
		  ORDER BY d.id DESC
		  LIMIT 50`)
	if err != nil {
		return err
	}
	type live struct {
		id      domain.ID
		env     domain.ID
		desired string
		actual  string
	}
	var current []live
	for rows.Next() {
		var l live
		if err := rows.Scan(&l.id, &l.env, &l.desired, &l.actual); err != nil {
			rows.Close()
			return err
		}
		current = append(current, l)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for _, l := range current {
		observed, err := r.deploys.Applier.Observe(ctx, l.env)
		if err != nil {
			// An applier that cannot be reached is not evidence of drift, so this is
			// not recorded as drift. Claiming drift on every network blip would train
			// people to ignore the signal.
			continue
		}
		if observed == "" || observed == l.desired {
			// Back in sync: close any open drift record for this environment.
			if _, err := r.db.Exec(ctx,
				`UPDATE drift_records SET resolved_at=now(), resolution='RECONCILED'
				  WHERE environment_id=$1 AND resolved_at IS NULL`, l.env); err != nil {
				return err
			}
			continue
		}

		// Only record drift that is not already open, so one persistent divergence
		// produces one record rather than one per tick.
		var exists bool
		if err := r.db.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM drift_records
			                 WHERE environment_id=$1 AND resolved_at IS NULL
			                   AND desired_revision=$2)`, l.env, l.desired).Scan(&exists); err != nil {
			return err
		}
		if exists {
			continue
		}

		tx, err := r.db.Begin(ctx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO drift_records
			   (id, environment_id, deployment_id, desired_revision, actual_revision, detail)
			 VALUES ($1,$2,$3,$4,$5,$6)`,
			domain.NewID(domain.PrefixIncident), l.env, l.id, l.desired, observed,
			"observed revision does not match the revision NEXUS applied"); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE deployments SET sync_status='DRIFTED' WHERE id=$1`, l.id); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		if err := outbox.Write(ctx, tx, events.New(events.DriftDetected, "environment", l.env,
			map[string]any{"desired": l.desired, "actual": observed})); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}

		if r.metrics != nil {
			r.metrics.DriftDetected.Inc()
		}
		r.log.Warn("drift detected", logging.FieldEnvironmentID, l.env.String(),
			"desired", l.desired, "actual", observed)
	}
	return nil
}

// expirePreviewEnvironments destroys preview environments past their expiry.
//
// Preview environments are created per pull request and are the one resource in NEXUS
// that accumulates silently: nobody notices thirty stale namespaces until a cluster runs
// out of room. Expiry is enforced here rather than relying on the pull-request close
// event, because that event can be missed and the environment would then live forever.
func (r *Reconciler) expirePreviewEnvironments(ctx context.Context) error {
	rows, err := r.db.Query(ctx,
		`SELECT id, project_id FROM environments
		  WHERE kind='PREVIEW' AND state='ACTIVE'
		    AND expires_at IS NOT NULL AND expires_at < now()
		  LIMIT 20`)
	if err != nil {
		return err
	}
	type env struct{ id, project domain.ID }
	var expired []env
	for rows.Next() {
		var e env
		if err := rows.Scan(&e.id, &e.project); err != nil {
			rows.Close()
			return err
		}
		expired = append(expired, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for _, e := range expired {
		if r.deploys != nil && r.deploys.Applier != nil {
			if err := r.deploys.Applier.Destroy(ctx, e.id); err != nil {
				r.log.Warn("could not destroy preview environment resources",
					logging.FieldEnvironmentID, e.id.String(), logging.FieldError, err)
				continue
			}
		}
		tx, err := r.db.Begin(ctx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE environments SET state='DESTROYED', destroyed_at=now() WHERE id=$1`, e.id); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		if err := outbox.Write(ctx, tx, events.New(events.EnvironmentDestroyed, "environment", e.id,
			map[string]any{"project_id": e.project.String(), "reason": "preview environment expired"})); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
		r.log.Info("destroyed expired preview environment", logging.FieldEnvironmentID, e.id.String())
	}
	return nil
}

// markLostWorkers flags workers that stopped heartbeating.
//
// Purely cosmetic: their jobs are already recovered by reapLeases, which does not
// consult this table at all. This exists so the console does not show a fleet of workers
// that no longer exist.
func (r *Reconciler) markLostWorkers(ctx context.Context) error {
	_, err := r.db.Exec(ctx,
		`UPDATE workers SET state='LOST'
		  WHERE state='ACTIVE' AND last_heartbeat_at < now() - interval '90 seconds'`)
	return err
}

func (r *Reconciler) refreshGauges(ctx context.Context) error {
	if r.metrics == nil {
		return nil
	}
	var queued, running, workers, pending int
	if err := r.db.QueryRow(ctx,
		`SELECT (SELECT count(*) FROM jobs WHERE state='QUEUED'),
		        (SELECT count(*) FROM jobs WHERE state='RUNNING'),
		        (SELECT count(*) FROM workers WHERE last_heartbeat_at > now() - interval '45 seconds'),
		        (SELECT count(*) FROM outbox WHERE published_at IS NULL)`).
		Scan(&queued, &running, &workers, &pending); err != nil {
		return err
	}
	r.metrics.QueueDepth.WithLabelValues("QUEUED").Set(float64(queued))
	r.metrics.QueueDepth.WithLabelValues("RUNNING").Set(float64(running))
	r.metrics.WorkersActive.Set(float64(workers))
	r.metrics.OutboxPending.Set(float64(pending))
	return nil
}

// prune keeps unbounded tables bounded.
func (r *Reconciler) prune(ctx context.Context) error {
	if r.relay != nil {
		if _, err := r.relay.Prune(ctx, r.cfg.OutboxRetention); err != nil {
			return err
		}
	}
	// The processed-events guard only needs to cover the redelivery window, not all of
	// history, so anything older than the retention period is dead weight.
	_, err := r.db.Exec(ctx,
		`DELETE FROM processed_events
		  WHERE processed_at < now() - make_interval(secs => $1)`,
		r.cfg.ProcessedRetention.Seconds())
	return err
}
