//go:build integration

package integration

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/Keathan2609/nexus/internal/core/domain"
	"github.com/Keathan2609/nexus/internal/store/postgres"
	"github.com/Keathan2609/nexus/internal/workers"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// fakeExecutor lets a test control exactly what a step does: how long it takes, whether
// it fails, and when it is allowed to finish.
type fakeExecutor struct {
	mu sync.Mutex
	// startsByJob counts executions per job id. A per-job count rather than a total,
	// because the worker's claim loop is global by design and will also pick up jobs
	// that other tests left queued in the shared database.
	startsByJob map[string]int
	release     chan struct{}
	err         error
	duration    time.Duration
	onStart     func()
}

func (f *fakeExecutor) Execute(ctx context.Context, ex workers.Execution) error {
	f.mu.Lock()
	if f.startsByJob == nil {
		f.startsByJob = map[string]int{}
	}
	f.startsByJob[ex.Job.ID.String()]++
	onStart := f.onStart
	f.mu.Unlock()
	if onStart != nil {
		onStart()
	}
	ex.Log("stdout", "working on "+ex.Job.StepKey)

	if f.release != nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-f.release:
		}
	} else if f.duration > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(f.duration):
		}
	}
	return f.err
}

func (f *fakeExecutor) startsFor(jobID domain.ID) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.startsByJob[jobID.String()]
}

func workerConfig(id string) workers.Config {
	cfg := workers.DefaultConfig(id)
	cfg.Concurrency = 2
	cfg.Lease = 5 * time.Second
	cfg.Heartbeat = 500 * time.Millisecond
	cfg.PollInterval = 50 * time.Millisecond
	cfg.ShutdownGrace = 5 * time.Second
	return cfg
}

func TestWorkerExecutesAJobAndMarksItSucceeded(t *testing.T) {
	db := openDB(t)
	f := newFixture(t, db)
	jobs := postgres.NewJobStore(db)
	id := f.queueJob(t, db, "build", 3)

	exec := &fakeExecutor{duration: 10 * time.Millisecond}
	w := workers.New(workerConfig("wkr_exec"), db, jobs, exec, quietLogger(), nil)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	runWorkerUntil(t, w, ctx, func() bool { return jobState(t, db, id) == "SUCCEEDED" })

	if got := jobState(t, db, id); got != "SUCCEEDED" {
		t.Fatalf("expected SUCCEEDED, got %s", got)
	}
	if n := exec.startsFor(id); n != 1 {
		t.Fatalf("the job must execute exactly once, executed %d times", n)
	}
}

func TestWorkerWritesJobLogs(t *testing.T) {
	db := openDB(t)
	f := newFixture(t, db)
	jobs := postgres.NewJobStore(db)
	id := f.queueJob(t, db, "build", 1)

	exec := &fakeExecutor{duration: 5 * time.Millisecond}
	w := workers.New(workerConfig("wkr_logs"), db, jobs, exec, quietLogger(), nil)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	runWorkerUntil(t, w, ctx, func() bool { return jobState(t, db, id) == "SUCCEEDED" })

	var lines int
	if err := db.QueryRow(context.Background(),
		`SELECT count(*) FROM job_logs WHERE job_id=$1`, id).Scan(&lines); err != nil {
		t.Fatalf("count logs: %v", err)
	}
	if lines == 0 {
		t.Fatal("the worker must persist step output")
	}
}

// A failing step must consume attempts and end FAILED once the budget is exhausted.
func TestWorkerRetriesThenFailsTerminally(t *testing.T) {
	db := openDB(t)
	f := newFixture(t, db)
	jobs := postgres.NewJobStore(db)
	id := f.queueJob(t, db, "flaky", 2)

	exec := &fakeExecutor{err: errors.New("step blew up")}
	cfg := workerConfig("wkr_retry")
	w := workers.New(cfg, db, jobs, exec, quietLogger(), nil)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	runWorkerUntil(t, w, ctx, func() bool { return jobState(t, db, id) == "FAILED" })

	if got := jobState(t, db, id); got != "FAILED" {
		t.Fatalf("expected FAILED after exhausting attempts, got %s", got)
	}
	if got := jobAttempt(t, db, id); got != 2 {
		t.Fatalf("expected 2 attempts to have been consumed, got %d", got)
	}
	// Attempt history must record both, not just the last.
	var attempts int
	if err := db.QueryRow(context.Background(),
		`SELECT count(*) FROM job_attempts WHERE job_id=$1`, id).Scan(&attempts); err != nil {
		t.Fatalf("count attempts: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("expected 2 attempt rows in history, got %d", attempts)
	}
}

// Graceful shutdown: a worker told to stop finishes what it is already running rather
// than abandoning it to lease expiry.
func TestGracefulShutdownFinishesInFlightWork(t *testing.T) {
	db := openDB(t)
	f := newFixture(t, db)
	jobs := postgres.NewJobStore(db)
	id := f.queueJob(t, db, "long", 3)

	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	exec := &fakeExecutor{
		release: release,
		onStart: func() { once.Do(func() { close(started) }) },
	}

	w := workers.New(workerConfig("wkr_drain"), db, jobs, exec, quietLogger(), nil)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	select {
	case <-started:
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("worker never started the job")
	}

	// Ask the worker to stop while the job is still running.
	cancel()
	// Then let the step finish, inside the shutdown grace window.
	time.Sleep(100 * time.Millisecond)
	close(release)

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("worker did not shut down")
	}

	if got := jobState(t, db, id); got != "SUCCEEDED" {
		t.Fatalf("a job in flight at shutdown must be allowed to finish, got %s", got)
	}
}

// Worker death: the job is abandoned without any report, and recovery happens purely
// because the lease stops being renewed.
func TestJobIsRecoveredAfterWorkerDiesMidExecution(t *testing.T) {
	db := openDB(t)
	f := newFixture(t, db)
	jobs := postgres.NewJobStore(db)
	id := f.queueJob(t, db, "doomed", 3)

	// Claim as if a worker took the job, then never speak again.
	job, err := jobs.ClaimForProject(context.Background(), f.ProjectID, "wkr_dead", 1*time.Second)
	if err != nil || job == nil {
		t.Fatalf("claim: %v", err)
	}
	if got := jobState(t, db, id); got != "RUNNING" {
		t.Fatalf("expected RUNNING, got %s", got)
	}

	// Wait for the lease to lapse, then reap as the reconciler would.
	time.Sleep(1200 * time.Millisecond)
	reaped, err := jobs.ReapExpiredLeases(context.Background(), 0)
	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	if reaped < 1 {
		t.Fatalf("expected the abandoned job to be reaped, got %d", reaped)
	}

	// A live worker then picks it up and finishes it.
	exec := &fakeExecutor{duration: 10 * time.Millisecond}
	w := workers.New(workerConfig("wkr_rescue"), db, jobs, exec, quietLogger(), nil)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	runWorkerUntil(t, w, ctx, func() bool { return jobState(t, db, id) == "SUCCEEDED" })

	if got := jobState(t, db, id); got != "SUCCEEDED" {
		t.Fatalf("a recovered job must be able to succeed, got %s", got)
	}
	if got := jobAttempt(t, db, id); got != 2 {
		t.Fatalf("the abandoned attempt must still count, expected attempt 2, got %d", got)
	}
}

// A step that runs past its timeout is cancelled and recorded as a timeout, not as a
// generic failure.
func TestJobExceedingItsTimeoutIsCancelled(t *testing.T) {
	db := openDB(t)
	f := newFixture(t, db)
	jobs := postgres.NewJobStore(db)
	id := f.queueJob(t, db, "slow", 1)
	mustExec(t, db, `UPDATE jobs SET timeout_seconds = 1 WHERE id=$1`, id)

	exec := &fakeExecutor{duration: 30 * time.Second}
	w := workers.New(workerConfig("wkr_timeout"), db, jobs, exec, quietLogger(), nil)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	runWorkerUntil(t, w, ctx, func() bool { return jobState(t, db, id) == "FAILED" })

	if got := jobState(t, db, id); got != "FAILED" {
		t.Fatalf("expected FAILED, got %s", got)
	}
	var code string
	if err := db.QueryRow(context.Background(),
		`SELECT COALESCE(error_code,'') FROM jobs WHERE id=$1`, id).Scan(&code); err != nil {
		t.Fatalf("read error code: %v", err)
	}
	if code != "E_TIMEOUT" {
		t.Fatalf("a timeout must be reported as E_TIMEOUT, got %q", code)
	}
}

func TestWorkerRegistersAndDeregisters(t *testing.T) {
	db := openDB(t)
	jobs := postgres.NewJobStore(db)
	w := workers.New(workerConfig("wkr_register"), db, jobs, &fakeExecutor{}, quietLogger(), nil)

	if err := w.Register(context.Background()); err != nil {
		t.Fatalf("register: %v", err)
	}
	var state string
	if err := db.QueryRow(context.Background(),
		`SELECT state FROM workers WHERE id=$1`, "wkr_register").Scan(&state); err != nil {
		t.Fatalf("read worker: %v", err)
	}
	if state != "ACTIVE" {
		t.Fatalf("expected ACTIVE, got %s", state)
	}

	w.Deregister(context.Background())
	if err := db.QueryRow(context.Background(),
		`SELECT state FROM workers WHERE id=$1`, "wkr_register").Scan(&state); err != nil {
		t.Fatalf("read worker: %v", err)
	}
	if state != "STOPPED" {
		t.Fatalf("expected STOPPED after deregister, got %s", state)
	}
}

// runWorkerUntil runs a worker until cond holds or the context expires, then shuts it
// down and waits for it. Centralising this keeps every worker test from reimplementing
// the same start/poll/stop dance.
func runWorkerUntil(t *testing.T, w *workers.Worker, ctx context.Context, cond func() bool) {
	t.Helper()
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- w.Run(runCtx) }()

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			break
		}
		select {
		case <-ctx.Done():
			cancel()
			<-done
			t.Fatal("context expired before the condition held")
		case <-time.After(25 * time.Millisecond):
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("worker did not stop")
	}
}
