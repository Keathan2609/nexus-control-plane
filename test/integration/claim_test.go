//go:build integration

package integration

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Keathan2609/nexus/internal/core/domain"
	"github.com/Keathan2609/nexus/internal/store/postgres"
)

func TestClaimReturnsNilWhenNoWorkAvailable(t *testing.T) {
	db := openDB(t)
	store := postgres.NewJobStore(db)
	f := newFixture(t, db)
	_ = f

	// A brand-new fixture has no queued jobs of its own, but the shared database may
	// hold queued jobs from other tests. Scope the claim to this project.
	job, err := store.ClaimForProject(context.Background(), f.ProjectID, "wkr_none", 30*time.Second)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if job != nil {
		t.Fatalf("expected no job, got %s", job.ID)
	}
}

func TestClaimTransitionsJobToRunningAndTakesLease(t *testing.T) {
	db := openDB(t)
	store := postgres.NewJobStore(db)
	f := newFixture(t, db)
	id := f.queueJob(t, db, "build", 3)

	job, err := store.ClaimForProject(context.Background(), f.ProjectID, "wkr_a", 30*time.Second)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if job == nil {
		t.Fatal("expected to claim the queued job")
	}
	if job.ID != id {
		t.Fatalf("claimed the wrong job: %s", job.ID)
	}
	if got := jobState(t, db, id); got != "RUNNING" {
		t.Fatalf("expected RUNNING after claim, got %s", got)
	}
	if job.Attempt != 1 {
		t.Fatalf("first claim must be attempt 1, got %d", job.Attempt)
	}
	if job.LeaseExpiresAt.Before(time.Now()) {
		t.Fatal("claimed job must hold a lease in the future")
	}
}

// The central concurrency guarantee: many workers racing for one job produce exactly
// one winner. Without FOR UPDATE SKIP LOCKED this test fails by handing the same job to
// several workers, each of which would then run the same build.
func TestConcurrentClaimsNeverDoubleAssignAJob(t *testing.T) {
	db := openDB(t)
	store := postgres.NewJobStore(db)
	f := newFixture(t, db)

	const jobs = 20
	const workers = 12
	for i := 0; i < jobs; i++ {
		f.queueJob(t, db, "step-"+string(rune('a'+i)), 1)
	}

	var (
		mu       sync.Mutex
		claimed  = map[domain.ID]string{}
		conflict []string
		wg       sync.WaitGroup
	)

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(workerID string) {
			defer wg.Done()
			for {
				job, err := store.ClaimForProject(context.Background(), f.ProjectID, workerID, 30*time.Second)
				if err != nil {
					mu.Lock()
					conflict = append(conflict, "claim error: "+err.Error())
					mu.Unlock()
					return
				}
				if job == nil {
					return // backlog drained
				}
				mu.Lock()
				if prev, dup := claimed[job.ID]; dup {
					conflict = append(conflict,
						"job "+job.ID.String()+" claimed by both "+prev+" and "+workerID)
				}
				claimed[job.ID] = workerID
				mu.Unlock()
			}
		}("wkr_" + string(rune('a'+w)))
	}
	wg.Wait()

	if len(conflict) > 0 {
		t.Fatalf("double assignment detected: %v", conflict)
	}
	if len(claimed) != jobs {
		t.Fatalf("expected all %d jobs claimed exactly once, got %d", jobs, len(claimed))
	}
}

func TestHeartbeatExtendsTheLease(t *testing.T) {
	db := openDB(t)
	store := postgres.NewJobStore(db)
	f := newFixture(t, db)
	f.queueJob(t, db, "build", 3)

	job, err := store.ClaimForProject(context.Background(), f.ProjectID, "wkr_a", 2*time.Second)
	if err != nil || job == nil {
		t.Fatalf("claim failed: %v", err)
	}
	first := job.LeaseExpiresAt

	time.Sleep(200 * time.Millisecond)
	if err := store.Heartbeat(context.Background(), job.ID, "wkr_a", 30*time.Second); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}

	var extended time.Time
	if err := db.QueryRow(context.Background(),
		`SELECT lease_expires_at FROM jobs WHERE id=$1`, job.ID).Scan(&extended); err != nil {
		t.Fatalf("read lease: %v", err)
	}
	if !extended.After(first) {
		t.Fatalf("heartbeat did not extend the lease: %v -> %v", first, extended)
	}
}

func TestHeartbeatFromAnotherWorkerIsRejected(t *testing.T) {
	// A worker that lost its lease and had the job reaped must not be able to keep
	// renewing it, or two workers could believe they own the same job.
	db := openDB(t)
	store := postgres.NewJobStore(db)
	f := newFixture(t, db)
	f.queueJob(t, db, "build", 3)

	job, _ := store.ClaimForProject(context.Background(), f.ProjectID, "wkr_owner", 30*time.Second)
	if job == nil {
		t.Fatal("claim failed")
	}
	err := store.Heartbeat(context.Background(), job.ID, "wkr_impostor", 30*time.Second)
	if err == nil {
		t.Fatal("a heartbeat from a non-owner must be rejected")
	}
}

// Worker death is modelled as a lease that stops being renewed. This is the recovery
// path for a crashed, killed or partitioned worker.
func TestExpiredLeaseReturnsJobToQueue(t *testing.T) {
	db := openDB(t)
	store := postgres.NewJobStore(db)
	f := newFixture(t, db)
	id := f.queueJob(t, db, "build", 3)

	job, err := store.ClaimForProject(context.Background(), f.ProjectID, "wkr_doomed", 30*time.Second)
	if err != nil || job == nil {
		t.Fatalf("claim failed: %v", err)
	}

	// Simulate the worker dying: expire the lease without releasing the job.
	mustExec(t, db, `UPDATE jobs SET lease_expires_at = now() - interval '1 minute' WHERE id=$1`, id)

	reaped, err := store.ReapExpiredLeases(context.Background(), 0)
	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	if reaped < 1 {
		t.Fatalf("expected at least one job reaped, got %d", reaped)
	}
	if got := jobState(t, db, id); got != "QUEUED" {
		t.Fatalf("reaped job must return to QUEUED, got %s", got)
	}
	// The attempt already consumed must be preserved, or a crash-looping worker would
	// retry forever.
	if got := jobAttempt(t, db, id); got != 1 {
		t.Fatalf("attempt count must survive reaping, got %d", got)
	}
}

func TestReapingDoesNotTouchLiveLeases(t *testing.T) {
	db := openDB(t)
	store := postgres.NewJobStore(db)
	f := newFixture(t, db)
	id := f.queueJob(t, db, "build", 3)

	if _, err := store.ClaimForProject(context.Background(), f.ProjectID, "wkr_alive", 5*time.Minute); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := store.ReapExpiredLeases(context.Background(), 0); err != nil {
		t.Fatalf("reap: %v", err)
	}
	if got := jobState(t, db, id); got != "RUNNING" {
		t.Fatalf("a job with a live lease must stay RUNNING, got %s", got)
	}
}

// A job that has exhausted its attempt budget must not be requeued forever.
func TestReapingFailsJobThatExhaustedItsAttempts(t *testing.T) {
	db := openDB(t)
	store := postgres.NewJobStore(db)
	f := newFixture(t, db)
	id := f.queueJob(t, db, "build", 1) // one attempt only

	if _, err := store.ClaimForProject(context.Background(), f.ProjectID, "wkr_doomed", 30*time.Second); err != nil {
		t.Fatalf("claim: %v", err)
	}
	mustExec(t, db, `UPDATE jobs SET lease_expires_at = now() - interval '1 minute' WHERE id=$1`, id)

	if _, err := store.ReapExpiredLeases(context.Background(), 0); err != nil {
		t.Fatalf("reap: %v", err)
	}
	if got := jobState(t, db, id); got != "FAILED" {
		t.Fatalf("a reaped job with no attempts left must FAIL, got %s", got)
	}
}

func TestClaimRespectsRunAtBackoff(t *testing.T) {
	db := openDB(t)
	store := postgres.NewJobStore(db)
	f := newFixture(t, db)
	id := f.queueJob(t, db, "build", 3)
	mustExec(t, db, `UPDATE jobs SET run_at = now() + interval '1 hour' WHERE id=$1`, id)

	job, err := store.ClaimForProject(context.Background(), f.ProjectID, "wkr_a", 30*time.Second)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if job != nil {
		t.Fatal("a job whose backoff has not elapsed must not be claimable")
	}
}

func TestCompleteMarksJobSucceeded(t *testing.T) {
	db := openDB(t)
	store := postgres.NewJobStore(db)
	f := newFixture(t, db)
	id := f.queueJob(t, db, "build", 3)

	job, _ := store.ClaimForProject(context.Background(), f.ProjectID, "wkr_a", 30*time.Second)
	if job == nil {
		t.Fatal("claim failed")
	}
	if err := store.Complete(context.Background(), job.ID, "wkr_a"); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if got := jobState(t, db, id); got != "SUCCEEDED" {
		t.Fatalf("expected SUCCEEDED, got %s", got)
	}
}

func TestFailWithRemainingAttemptsRequeuesWithBackoff(t *testing.T) {
	db := openDB(t)
	store := postgres.NewJobStore(db)
	f := newFixture(t, db)
	id := f.queueJob(t, db, "build", 3)

	job, _ := store.ClaimForProject(context.Background(), f.ProjectID, "wkr_a", 30*time.Second)
	if job == nil {
		t.Fatal("claim failed")
	}
	requeued, err := store.Fail(context.Background(), job.ID, "wkr_a", "E_STEP", "boom", 5*time.Second)
	if err != nil {
		t.Fatalf("fail: %v", err)
	}
	if !requeued {
		t.Fatal("a job with attempts remaining must be requeued")
	}
	if got := jobState(t, db, id); got != "QUEUED" {
		t.Fatalf("expected QUEUED after retryable failure, got %s", got)
	}
	var runAt time.Time
	if err := db.QueryRow(context.Background(), `SELECT run_at FROM jobs WHERE id=$1`, id).Scan(&runAt); err != nil {
		t.Fatalf("read run_at: %v", err)
	}
	if !runAt.After(time.Now()) {
		t.Fatal("backoff must push run_at into the future")
	}
}

func TestFailWithNoAttemptsRemainingIsTerminal(t *testing.T) {
	db := openDB(t)
	store := postgres.NewJobStore(db)
	f := newFixture(t, db)
	id := f.queueJob(t, db, "build", 1)

	job, _ := store.ClaimForProject(context.Background(), f.ProjectID, "wkr_a", 30*time.Second)
	if job == nil {
		t.Fatal("claim failed")
	}
	requeued, err := store.Fail(context.Background(), job.ID, "wkr_a", "E_STEP", "boom", 5*time.Second)
	if err != nil {
		t.Fatalf("fail: %v", err)
	}
	if requeued {
		t.Fatal("a job with no attempts left must not be requeued")
	}
	if got := jobState(t, db, id); got != "FAILED" {
		t.Fatalf("expected FAILED, got %s", got)
	}
}
