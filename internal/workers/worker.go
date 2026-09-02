// Package workers is the execution plane: the runtime that claims jobs, runs them, and
// reports what happened.
//
// A worker holds no authority. It cannot decide that it owns a job, only ask; it cannot
// decide a job has failed permanently, only report the attempt. Every such decision is
// made by a conditional statement in the database. That is what allows workers to be
// killed at any moment without a supervisor, a drain protocol, or a consistent view of
// the fleet.
package workers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/Keathan2609/nexus/internal/core/domain"
	"github.com/Keathan2609/nexus/internal/platform/database"
	"github.com/Keathan2609/nexus/internal/platform/logging"
	"github.com/Keathan2609/nexus/internal/platform/telemetry"
	"github.com/Keathan2609/nexus/internal/store/postgres"
)

// Executor runs one job to completion. Implementations are in executor.go; the
// interface exists so the runtime can be tested without spawning processes.
type Executor interface {
	Execute(ctx context.Context, ex Execution) error
}

// Execution is everything an executor is given.
type Execution struct {
	Job postgres.ClaimedJob
	// Log streams a line of output. Implementations must tolerate being called from
	// several goroutines.
	Log func(stream, line string)
}

type Config struct {
	WorkerID    string
	Concurrency int
	Lease       time.Duration
	// Heartbeat should be comfortably shorter than Lease so a single slow round trip
	// does not cost a worker a job it is actively running.
	Heartbeat    time.Duration
	PollInterval time.Duration
	// ShutdownGrace bounds how long Run waits for in-flight jobs after the context is
	// cancelled before abandoning them to lease expiry.
	ShutdownGrace time.Duration
}

func DefaultConfig(workerID string) Config {
	return Config{
		WorkerID:      workerID,
		Concurrency:   4,
		Lease:         30 * time.Second,
		Heartbeat:     10 * time.Second,
		PollInterval:  500 * time.Millisecond,
		ShutdownGrace: 30 * time.Second,
	}
}

type Worker struct {
	cfg      Config
	db       *database.DB
	jobs     *postgres.JobStore
	executor Executor
	log      *slog.Logger
	metrics  *telemetry.Metrics

	active sync.WaitGroup
	// inFlight counts jobs currently executing, for the metrics gauge and for tests.
	mu       sync.Mutex
	inFlight int
}

func New(cfg Config, db *database.DB, jobs *postgres.JobStore, executor Executor, log *slog.Logger, m *telemetry.Metrics) *Worker {
	return &Worker{cfg: cfg, db: db, jobs: jobs, executor: executor, log: log, metrics: m}
}

// Register records the worker so the console can show the fleet. Registration is
// convenience, not correctness: nothing in job dispatch consults this table, so a worker
// that fails to register still works, and a registered worker that dies is still
// recovered by lease expiry rather than by anything noticing its row.
func (w *Worker) Register(ctx context.Context) error {
	host, _ := os.Hostname()
	_, err := w.db.Exec(ctx,
		`INSERT INTO workers (id, hostname, concurrency, state, last_heartbeat_at)
		 VALUES ($1,$2,$3,'ACTIVE', now())
		 ON CONFLICT (id) DO UPDATE
		   SET state='ACTIVE', last_heartbeat_at=now(), concurrency=EXCLUDED.concurrency`,
		w.cfg.WorkerID, host, w.cfg.Concurrency)
	if err != nil {
		return fmt.Errorf("register worker: %w", err)
	}
	return nil
}

// Deregister marks the worker stopped. Best effort: a worker that is killed never gets
// here, which is exactly the case lease expiry exists to handle.
func (w *Worker) Deregister(ctx context.Context) {
	_, _ = w.db.Exec(ctx, `UPDATE workers SET state='STOPPED' WHERE id=$1`, w.cfg.WorkerID)
}

// Run claims and executes jobs until ctx is cancelled, then drains.
//
// Cancellation is deliberately two-phase. The claim loop stops immediately so no new
// work is taken, but jobs already running are given ShutdownGrace to finish. That turns
// an ordinary deploy or scale-down from "N jobs get reaped and retried" into "N jobs
// finish normally", which is the difference between a rolling restart being invisible
// and being a burst of duplicated builds.
func (w *Worker) Run(ctx context.Context) error {
	if err := w.Register(ctx); err != nil {
		return err
	}

	slots := make(chan struct{}, w.cfg.Concurrency)
	for i := 0; i < w.cfg.Concurrency; i++ {
		slots <- struct{}{}
	}

	heartbeatCtx, stopFleetHeartbeat := context.WithCancel(context.Background())
	go w.fleetHeartbeat(heartbeatCtx)
	defer stopFleetHeartbeat()

	ticker := time.NewTicker(w.cfg.PollInterval)
	defer ticker.Stop()

claimLoop:
	for {
		select {
		case <-ctx.Done():
			break claimLoop
		case <-slots:
		}

		job, err := w.jobs.Claim(context.WithoutCancel(ctx), w.cfg.WorkerID, w.cfg.Lease)
		if err != nil {
			w.log.Error("claim failed", logging.FieldError, err)
			slots <- struct{}{}
			if !w.wait(ctx, ticker) {
				break claimLoop
			}
			continue
		}
		if job == nil {
			slots <- struct{}{}
			if !w.wait(ctx, ticker) {
				break claimLoop
			}
			continue
		}

		w.active.Add(1)
		go func(j postgres.ClaimedJob) {
			defer w.active.Done()
			defer func() { slots <- struct{}{} }()
			w.runJob(j)
		}(*job)
	}

	// Drain.
	done := make(chan struct{})
	go func() { w.active.Wait(); close(done) }()
	select {
	case <-done:
		w.log.Info("all in-flight jobs finished")
	case <-time.After(w.cfg.ShutdownGrace):
		w.log.Warn("shutdown grace elapsed with jobs still running; their leases will expire and they will be retried",
			"in_flight", w.InFlight())
	}

	w.Deregister(context.WithoutCancel(ctx))
	return nil
}

func (w *Worker) wait(ctx context.Context, ticker *time.Ticker) bool {
	select {
	case <-ctx.Done():
		return false
	case <-ticker.C:
		return true
	}
}

func (w *Worker) InFlight() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.inFlight
}

// runJob executes one claimed job under a timeout, renewing its lease as it goes.
func (w *Worker) runJob(job postgres.ClaimedJob) {
	w.mu.Lock()
	w.inFlight++
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		w.inFlight--
		w.mu.Unlock()
	}()
	if w.metrics != nil {
		w.metrics.WorkerJobsActive.WithLabelValues(w.cfg.WorkerID).Set(float64(w.InFlight()))
	}

	log := w.log.With(
		logging.FieldJobID, job.ID.String(),
		logging.FieldRunID, job.RunID.String(),
		logging.FieldWorkerID, w.cfg.WorkerID,
		"step", job.StepKey,
		"attempt", job.Attempt,
	)

	timeout := time.Duration(job.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// The heartbeat goroutine is also the cancellation channel. If the lease can no
	// longer be renewed - because the job was reaped, or the run was cancelled - the
	// job's context is cancelled and the executor is torn down. That is how
	// cancellation propagates from an API call to a running process without the worker
	// needing to subscribe to anything.
	go w.heartbeat(ctx, cancel, job, log)

	started := time.Now()
	var seq int
	var logMu sync.Mutex
	appendLog := func(stream, line string) {
		logMu.Lock()
		seq++
		n := seq
		logMu.Unlock()
		if err := w.jobs.AppendLog(context.WithoutCancel(ctx), job.ID, job.Attempt, n, stream, line); err != nil {
			log.Debug("could not persist log line", logging.FieldError, err)
		}
	}

	log.Info("job started")
	appendLog("system", fmt.Sprintf("step %s starting (attempt %d/%d)", job.StepKey, job.Attempt, job.MaxAttempts))

	err := w.executor.Execute(ctx, Execution{Job: job, Log: appendLog})
	elapsed := time.Since(started)

	// Report against a context that is not the job's own: the job context may already
	// be cancelled, and the result still has to be written.
	reportCtx, reportCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer reportCancel()

	switch {
	case err == nil:
		if cerr := w.jobs.Complete(reportCtx, job.ID, w.cfg.WorkerID); cerr != nil {
			if errors.Is(cerr, postgres.ErrNotOwner) {
				log.Warn("job finished but the lease had already been reassigned; discarding the result")
			} else {
				log.Error("could not record success", logging.FieldError, cerr)
			}
			return
		}
		appendLog("system", fmt.Sprintf("step %s succeeded in %s", job.StepKey, elapsed.Round(time.Millisecond)))
		log.Info("job succeeded", "duration_ms", elapsed.Milliseconds())
		if w.metrics != nil {
			w.metrics.ObserveJob(job.Type, "SUCCEEDED", elapsed)
		}

	default:
		code, message := classify(err)
		appendLog("stderr", message)
		backoff := Backoff(job.Attempt)
		requeued, ferr := w.jobs.Fail(reportCtx, job.ID, w.cfg.WorkerID, code, message, backoff)
		if ferr != nil {
			if errors.Is(ferr, postgres.ErrNotOwner) {
				log.Warn("job failed but the lease had already been reassigned; discarding the result")
			} else {
				log.Error("could not record failure", logging.FieldError, ferr)
			}
			return
		}
		state := "FAILED"
		if requeued {
			state = "RETRYING"
			appendLog("system", fmt.Sprintf("retrying in %s", backoff.Round(time.Second)))
			if w.metrics != nil {
				w.metrics.JobRetries.WithLabelValues(job.Type, code).Inc()
			}
		}
		log.Warn("job did not succeed", "state", state, "code", code,
			"duration_ms", elapsed.Milliseconds(), logging.FieldError, message)
		if w.metrics != nil {
			w.metrics.ObserveJob(job.Type, state, elapsed)
		}
	}
}

// heartbeat renews the lease until the job finishes, and cancels the job if it cannot.
func (w *Worker) heartbeat(ctx context.Context, cancel context.CancelFunc, job postgres.ClaimedJob, log *slog.Logger) {
	ticker := time.NewTicker(w.cfg.Heartbeat)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			hbCtx, hbCancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := w.jobs.Heartbeat(hbCtx, job.ID, w.cfg.WorkerID, w.cfg.Lease)
			hbCancel()

			if errors.Is(err, postgres.ErrNotOwner) {
				// Someone else owns this job now, or the run was cancelled. Continuing
				// would mean two workers running the same step.
				log.Warn("lost the lease; cancelling execution")
				cancel()
				return
			}
			if err != nil {
				// A transient database error is not grounds for abandoning work: the
				// lease still has time on it, so try again next tick.
				log.Debug("heartbeat failed; will retry", logging.FieldError, err)
			}
		}
	}
}

// fleetHeartbeat keeps the worker's own row fresh, so the console can distinguish a
// live worker from one that stopped without deregistering.
func (w *Worker) fleetHeartbeat(ctx context.Context) {
	ticker := time.NewTicker(w.cfg.Heartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_, _ = w.db.Exec(ctx,
				`UPDATE workers SET last_heartbeat_at=now(), active_jobs=$2 WHERE id=$1`,
				w.cfg.WorkerID, w.InFlight())
		}
	}
}

// classify turns an execution error into a stable code plus a human message. Codes are
// what metrics and alerts group on, so they must not contain anything variable.
func classify(err error) (code, message string) {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "E_TIMEOUT", "step exceeded its timeout"
	case errors.Is(err, context.Canceled):
		return "E_CANCELLED", "step was cancelled"
	}
	var ex *ExitError
	if errors.As(err, &ex) {
		return "E_EXIT", fmt.Sprintf("step exited with status %d", ex.Code)
	}
	return "E_STEP", err.Error()
}

// Backoff is exponential with jitter, bounded.
//
// The jitter matters at fleet scale: without it, N jobs that failed together because a
// shared dependency was down all retry at the same instant and knock it over again the
// moment it recovers.
func Backoff(attempt int) time.Duration {
	const (
		base = 2 * time.Second
		max  = 5 * time.Minute
	)
	if attempt < 1 {
		attempt = 1
	}
	d := base << (attempt - 1)
	if d > max || d <= 0 {
		d = max
	}
	// Up to 20% either way.
	jitter := time.Duration(float64(d) * 0.2 * (2*randFloat() - 1))
	out := d + jitter
	if out < 0 {
		return base
	}
	return out
}

var _ = domain.NewID
