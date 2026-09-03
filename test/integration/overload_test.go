//go:build integration

// Overload tests assert the shape of failure under pressure.
//
// The property being defended is not "NEXUS is fast". It is that work arriving faster
// than NEXUS can execute it becomes queue depth or a refusal, and never an unbounded
// number of concurrent processes. A system that accepts everything and falls over is
// worse than one that says no.
package integration

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Keathan2609/nexus/internal/core/domain"
	"github.com/Keathan2609/nexus/internal/rbac"
	"github.com/Keathan2609/nexus/internal/store/postgres"
)

// Concurrent claims must never hand the same job to two workers. This is the property
// the whole execution model rests on: without it, "at least once" becomes "twice, in
// parallel, both writing results".
func TestConcurrentClaimsNeverDuplicateAJob(t *testing.T) {
	db := openDB(t)
	f := newFixture(t, db)

	const jobs = 40
	const workers = 16

	queued := make(map[domain.ID]bool, jobs)
	for i := 0; i < jobs; i++ {
		queued[f.queueJob(t, db, "step-"+string(rune('a'+i%26))+string(rune('0'+i/26)), 1)] = true
	}

	store := postgres.NewJobStore(db)
	var (
		mu      sync.Mutex
		claimed = map[domain.ID]string{}
		wg      sync.WaitGroup
		dupes   atomic.Int64
	)

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(workerID string) {
			defer wg.Done()
			for {
				job, err := store.Claim(context.Background(), workerID, 60*time.Second)
				if err != nil || job == nil {
					return
				}
				mu.Lock()
				if _, seen := claimed[job.ID]; seen {
					dupes.Add(1)
				}
				claimed[job.ID] = workerID
				mu.Unlock()
			}
		}("wkr_load" + string(rune('a'+w)))
	}
	wg.Wait()

	if n := dupes.Load(); n != 0 {
		t.Fatalf("%d jobs were claimed more than once", n)
	}

	got := 0
	for id := range claimed {
		if queued[id] {
			got++
		}
	}
	if got != jobs {
		t.Fatalf("claimed %d of this fixture's %d jobs", got, jobs)
	}
}

// Work beyond capacity must sit in the queue, not execute. A worker with N slots must
// hold exactly N jobs at once no matter how many are waiting.
func TestQueueAbsorbsWorkBeyondCapacity(t *testing.T) {
	db := openDB(t)
	f := newFixture(t, db)

	const total = 50
	const slots = 4

	for i := 0; i < total; i++ {
		f.queueJob(t, db, "burst-"+string(rune('a'+i%26))+string(rune('0'+i/26)), 1)
	}

	store := postgres.NewJobStore(db)

	// Claim exactly as many as a worker with `slots` slots would hold at once.
	var held []domain.ID
	for i := 0; i < slots; i++ {
		job, err := store.Claim(context.Background(), "wkr_bounded", 60*time.Second)
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		if job == nil {
			t.Fatalf("expected a claimable job at slot %d", i)
		}
		held = append(held, job.ID)
	}

	if len(held) != slots {
		t.Fatalf("held %d jobs, expected %d", len(held), slots)
	}

	// Everything else is still queued and untouched — absorbed, not executed.
	var stillQueued int
	if err := db.QueryRow(context.Background(),
		`SELECT count(*) FROM jobs WHERE run_id=$1 AND state='QUEUED'`, f.RunID).Scan(&stillQueued); err != nil {
		t.Fatalf("count queued: %v", err)
	}
	if stillQueued != total-slots {
		t.Fatalf("expected %d jobs still queued, found %d", total-slots, stillQueued)
	}

	var running int
	if err := db.QueryRow(context.Background(),
		`SELECT count(*) FROM jobs WHERE run_id=$1 AND state='RUNNING'`, f.RunID).Scan(&running); err != nil {
		t.Fatalf("count running: %v", err)
	}
	if running != slots {
		t.Fatalf("expected exactly %d jobs running, found %d", slots, running)
	}
}

// A burst of API reads must not produce errors, and the rate limiter must express
// refusal as 429 rather than by failing.
func TestAPIAbsorbsConcurrentReads(t *testing.T) {
	db := openDB(t)
	h := newAPIHarness(t, db)
	f := newFixture(t, db)
	token := h.tokenFor(t, f, rbac.RoleAdmin)

	const requests = 300
	const parallel = 30

	var (
		ok, limited, failed atomic.Int64
		wg                  sync.WaitGroup
	)
	work := make(chan int, requests)
	for i := 0; i < requests; i++ {
		work <- i
	}
	close(work)

	started := time.Now()
	for i := 0; i < parallel; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range work {
				resp := h.do(t, http.MethodGet, "/api/v1/runs?limit=10", token, nil)
				switch {
				case resp.StatusCode == http.StatusOK:
					ok.Add(1)
				case resp.StatusCode == http.StatusTooManyRequests:
					limited.Add(1)
				default:
					failed.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(started)

	t.Logf("%d requests in %s: %d ok, %d rate-limited, %d failed",
		requests, elapsed.Round(time.Millisecond), ok.Load(), limited.Load(), failed.Load())

	if failed.Load() != 0 {
		t.Fatalf("%d requests failed with an unexpected status", failed.Load())
	}
	if ok.Load()+limited.Load() != requests {
		t.Fatalf("accounted for %d of %d requests", ok.Load()+limited.Load(), requests)
	}
	// Every outcome must be a deliberate one. A saturated API answers, it does not hang.
	if ok.Load() == 0 {
		t.Fatal("no request succeeded; the limiter is refusing everything")
	}
}

// The login limiter must not be bypassable by varying a client-controlled header.
func TestLoginRateLimitIsNotBypassableByHeader(t *testing.T) {
	db := openDB(t)
	h := newAPIHarness(t, db)

	var limited int
	for i := 0; i < 40; i++ {
		req, err := newJSONRequest(http.MethodPost, h.srv.URL+"/api/v1/auth/login",
			map[string]any{"email": "nobody@example.test", "password": "wrong"})
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		// A new forged origin on every attempt. If this were believed, each attempt
		// would land in a fresh bucket and the limiter would never engage.
		req.Header.Set("X-Forwarded-For", "203.0.113."+itoa(i))
		resp, err := h.srv.Client().Do(req)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests {
			limited++
		}
	}

	if limited == 0 {
		t.Fatal("password guessing was never rate-limited; a spoofed header defeated the limiter")
	}
	t.Logf("%d of 40 forged-origin login attempts were rate-limited", limited)
}

// --- helpers -------------------------------------------------------------------

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
