// Command loadgen drives NEXUS under sustained concurrent load and reports what
// actually happened.
//
// It exists to answer one question honestly: when work arrives faster than NEXUS can
// execute it, does the excess become queue depth and refusals, or does it become
// resource exhaustion? Throughput numbers on their own would not answer that, so this
// records the status distribution and samples queue depth and worker saturation from
// the database while the load is running.
//
// Written against the standard library rather than k6 or vegeta. The scenarios need to
// read control-plane state mid-flight, which an external HTTP load tool cannot do, and
// a load harness that needs its own runtime installed is one that does not get run.
//
// Usage:
//
//	go run ./test/load -scenario=api      -requests=2000 -concurrency=50
//	go run ./test/load -scenario=pipeline -requests=200  -concurrency=20
//	go run ./test/load -scenario=mixed    -duration=30s  -concurrency=40
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

func main() {
	var (
		base        = flag.String("api", envOr("NEXUS_API_URL", "http://localhost:8080"), "API base URL")
		email       = flag.String("email", envOr("NEXUS_DEMO_EMAIL", "admin@nexus.local"), "login email")
		password    = flag.String("password", envOr("NEXUS_DEMO_PASSWORD", "nexus-demo-password"), "login password")
		scenario    = flag.String("scenario", "api", "api | pipeline | mixed | stream")
		requests    = flag.Int("requests", 1000, "total requests (ignored when -duration is set)")
		concurrency = flag.Int("concurrency", 25, "concurrent callers")
		duration    = flag.Duration("duration", 0, "run for a fixed duration instead of a request count")
		timeout     = flag.Duration("timeout", 30*time.Second, "per-request timeout")
	)
	flag.Parse()

	if *concurrency < 1 {
		fmt.Fprintln(os.Stderr, "concurrency must be at least 1")
		os.Exit(2)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	client := &http.Client{
		Timeout: *timeout,
		Transport: &http.Transport{
			// The default of 2 idle connections per host turns a concurrency test into
			// a connection-churn test: every caller beyond the second reconnects, and
			// the numbers measure TCP setup rather than the control plane.
			MaxIdleConns:        *concurrency * 2,
			MaxIdleConnsPerHost: *concurrency * 2,
			MaxConnsPerHost:     *concurrency * 2,
		},
	}

	token, err := login(ctx, client, *base, *email, *password)
	if err != nil {
		fmt.Fprintf(os.Stderr, "login failed: %v\n", err)
		fmt.Fprintf(os.Stderr, "is the API running at %s?\n", *base)
		os.Exit(1)
	}

	run := &runner{
		client: client, base: *base, token: token,
		concurrency: *concurrency, requests: *requests, duration: *duration,
	}

	fmt.Printf("NEXUS load: scenario=%s concurrency=%d target=%s\n",
		*scenario, *concurrency, describeTarget(*requests, *duration))
	fmt.Printf("api=%s\n\n", *base)

	var report *Report
	switch *scenario {
	case "api":
		report = run.execute(ctx, "API read", run.readOverview)
	case "pipeline":
		report = run.execute(ctx, "Pipeline submission", run.triggerRun)
	case "mixed":
		report = run.execute(ctx, "Mixed read/write", run.mixed)
	case "stream":
		report = run.execute(ctx, "Event stream connect", run.openStream)
	default:
		fmt.Fprintf(os.Stderr, "unknown scenario %q\n", *scenario)
		os.Exit(2)
	}

	report.Print()

	// A run that produced unexplained failures is a failed run. Rate limiting and
	// deliberate load shedding are not failures — they are the system working.
	if report.Unexpected > 0 {
		os.Exit(1)
	}
}

// --- runner ---------------------------------------------------------------------

type runner struct {
	client      *http.Client
	base, token string
	concurrency int
	requests    int
	duration    time.Duration

	projectID atomic.Value // string, resolved lazily for write scenarios
}

// attempt is one measured request.
type attempt struct {
	status  int
	latency time.Duration
	err     error
}

func (r *runner) execute(ctx context.Context, name string, fn func(context.Context) attempt) *Report {
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		latencies []time.Duration
		byStatus  = map[int]int64{}
		errs      = map[string]int64{}
		issued    atomic.Int64
	)

	deadline := time.Time{}
	if r.duration > 0 {
		deadline = time.Now().Add(r.duration)
	}

	// Queue depth and worker saturation are sampled while the load runs, because both
	// are only meaningful during it. Reading them afterwards shows a drained system.
	samples := newSampler(r)
	sampleCtx, stopSampling := context.WithCancel(ctx)
	go samples.run(sampleCtx)

	started := time.Now()
	for i := 0; i < r.concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				if deadline.IsZero() {
					if issued.Add(1) > int64(r.requests) {
						return
					}
				} else if time.Now().After(deadline) {
					return
				} else {
					issued.Add(1)
				}
				if ctx.Err() != nil {
					return
				}

				a := fn(ctx)

				mu.Lock()
				latencies = append(latencies, a.latency)
				if a.err != nil {
					errs[a.err.Error()]++
				} else {
					byStatus[a.status]++
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(started)
	stopSampling()

	return buildReport(name, elapsed, latencies, byStatus, errs, samples.snapshot())
}

// --- scenarios ------------------------------------------------------------------

func (r *runner) readOverview(ctx context.Context) attempt {
	return r.call(ctx, http.MethodGet, "/api/v1/runs?limit=20", nil)
}

func (r *runner) triggerRun(ctx context.Context) attempt {
	id, err := r.someProject(ctx)
	if err != nil {
		return attempt{err: err}
	}
	return r.call(ctx, http.MethodPost, "/api/v1/projects/"+id+"/runs",
		map[string]any{"branch": "main"})
}

// mixed weights reads far above writes, which is the shape of real console traffic:
// many people watching, a few people triggering.
func (r *runner) mixed(ctx context.Context) attempt {
	if time.Now().UnixNano()%10 == 0 {
		return r.triggerRun(ctx)
	}
	return r.readOverview(ctx)
}

func (r *runner) openStream(ctx context.Context) attempt {
	started := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.base+"/api/v1/stream", nil)
	if err != nil {
		return attempt{err: err, latency: time.Since(started)}
	}
	req.Header.Set("Authorization", "Bearer "+r.token)
	resp, err := r.client.Do(req)
	if err != nil {
		return attempt{err: err, latency: time.Since(started)}
	}
	// Read only the first frame, then hang up. Holding every connection open would
	// measure the machine's file-descriptor limit rather than the hub's.
	buf := make([]byte, 128)
	_, _ = resp.Body.Read(buf)
	resp.Body.Close()
	return attempt{status: resp.StatusCode, latency: time.Since(started)}
}

func (r *runner) someProject(ctx context.Context) (string, error) {
	if v, ok := r.projectID.Load().(string); ok && v != "" {
		return v, nil
	}
	body, _, err := r.raw(ctx, http.MethodGet, "/api/v1/projects?limit=1", nil)
	if err != nil {
		return "", err
	}
	var page struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		return "", err
	}
	if len(page.Data) == 0 {
		return "", fmt.Errorf("no projects exist; run the seeder first")
	}
	r.projectID.Store(page.Data[0].ID)
	return page.Data[0].ID, nil
}

// --- http ----------------------------------------------------------------------

func (r *runner) call(ctx context.Context, method, path string, body any) attempt {
	started := time.Now()
	_, status, err := r.raw(ctx, method, path, body)
	return attempt{status: status, latency: time.Since(started), err: err}
}

func (r *runner) raw(ctx context.Context, method, path string, body any) ([]byte, int, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, r.base+path, reader)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+r.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	// The body must be drained even when it is not used, or the connection cannot be
	// reused and the pool sizing above achieves nothing.
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return raw, resp.StatusCode, nil
}

func login(ctx context.Context, client *http.Client, base, email, password string) (string, error) {
	payload, _ := json.Marshal(map[string]string{"email": email, "password": password})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/v1/auth/login",
		bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("login returned %d", resp.StatusCode)
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.Token, nil
}

// --- sampling -------------------------------------------------------------------

// sampler polls control-plane state during the run. It reads through the API rather
// than opening its own database connection, so the load harness needs no credentials
// beyond the one it already logged in with.
type sampler struct {
	r  *runner
	mu sync.Mutex
	qd []int
	wa []int
}

func newSampler(r *runner) *sampler { return &sampler{r: r} }

func (s *sampler) run(ctx context.Context) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			body, status, err := s.r.raw(ctx, http.MethodGet, "/api/v1/overview", nil)
			if err != nil || status != http.StatusOK {
				continue
			}
			var ov struct {
				Metrics struct {
					QueueDepth  int `json:"queue_depth"`
					JobsRunning int `json:"jobs_running"`
					OutboxDepth int `json:"outbox_pending"`
				} `json:"metrics"`
				Workers []struct {
					ActiveJobs  int `json:"active_jobs"`
					Concurrency int `json:"concurrency"`
				} `json:"workers"`
			}
			if json.Unmarshal(body, &ov) != nil {
				continue
			}
			active, capacity := 0, 0
			for _, w := range ov.Workers {
				active += w.ActiveJobs
				capacity += w.Concurrency
			}
			saturation := 0
			if capacity > 0 {
				saturation = active * 100 / capacity
			}
			s.mu.Lock()
			s.qd = append(s.qd, ov.Metrics.QueueDepth)
			s.wa = append(s.wa, saturation)
			s.mu.Unlock()
		}
	}
}

type samples struct{ queueDepth, saturation []int }

func (s *sampler) snapshot() samples {
	s.mu.Lock()
	defer s.mu.Unlock()
	return samples{queueDepth: append([]int(nil), s.qd...), saturation: append([]int(nil), s.wa...)}
}

// --- reporting ------------------------------------------------------------------

type Report struct {
	Name               string
	Elapsed            time.Duration
	Total              int
	Throughput         float64
	P50, P95, P99, Max time.Duration
	ByStatus           map[int]int64
	Errors             map[string]int64
	Unexpected         int64
	Samples            samples
}

func buildReport(name string, elapsed time.Duration, latencies []time.Duration,
	byStatus map[int]int64, errs map[string]int64, s samples) *Report {

	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })

	r := &Report{
		Name: name, Elapsed: elapsed, Total: len(latencies),
		ByStatus: byStatus, Errors: errs, Samples: s,
	}
	if elapsed > 0 {
		r.Throughput = float64(len(latencies)) / elapsed.Seconds()
	}
	if len(latencies) > 0 {
		r.P50 = percentile(latencies, 50)
		r.P95 = percentile(latencies, 95)
		r.P99 = percentile(latencies, 99)
		r.Max = latencies[len(latencies)-1]
	}

	// 2xx is success. 429 and 503 are the system deliberately protecting itself and are
	// reported separately rather than as failures. Everything else is unexpected.
	for status, n := range byStatus {
		switch {
		case status >= 200 && status < 300:
		case status == http.StatusTooManyRequests, status == http.StatusServiceUnavailable:
		default:
			r.Unexpected += n
		}
	}
	for _, n := range errs {
		r.Unexpected += n
	}
	return r
}

func percentile(sorted []time.Duration, p int) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(math.Ceil(float64(p)/100*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func (r *Report) Print() {
	fmt.Printf("%s\n", r.Name)
	fmt.Printf("%s\n", line(60))
	fmt.Printf("  requests      %d in %s\n", r.Total, r.Elapsed.Round(time.Millisecond))
	fmt.Printf("  throughput    %.0f req/s\n", r.Throughput)
	fmt.Printf("  latency       p50 %-9s p95 %-9s p99 %-9s max %s\n",
		r.P50.Round(time.Microsecond), r.P95.Round(time.Microsecond),
		r.P99.Round(time.Microsecond), r.Max.Round(time.Microsecond))

	fmt.Printf("\n  responses\n")
	codes := make([]int, 0, len(r.ByStatus))
	for c := range r.ByStatus {
		codes = append(codes, c)
	}
	sort.Ints(codes)
	for _, c := range codes {
		fmt.Printf("    %-3d %-20s %d\n", c, meaning(c), r.ByStatus[c])
	}
	if len(r.Errors) > 0 {
		fmt.Printf("\n  transport errors\n")
		for e, n := range r.Errors {
			fmt.Printf("    %-24s %d\n", truncate(e, 60), n)
		}
	}

	if len(r.Samples.queueDepth) > 0 {
		fmt.Printf("\n  queue depth   min %d  max %d  final %d  (%d samples)\n",
			minOf(r.Samples.queueDepth), maxOf(r.Samples.queueDepth),
			r.Samples.queueDepth[len(r.Samples.queueDepth)-1], len(r.Samples.queueDepth))
	}
	if len(r.Samples.saturation) > 0 {
		fmt.Printf("  worker slots  peak %d%% utilised\n", maxOf(r.Samples.saturation))
	}

	fmt.Printf("\n  %s\n", verdict(r))
}

func verdict(r *Report) string {
	if r.Unexpected > 0 {
		return fmt.Sprintf("FAIL: %d responses were neither success nor deliberate backpressure", r.Unexpected)
	}
	shed := r.ByStatus[http.StatusTooManyRequests] + r.ByStatus[http.StatusServiceUnavailable]
	if shed > 0 {
		return fmt.Sprintf("PASS: %d requests served, %d shed deliberately, 0 failures",
			int64(r.Total)-shed, shed)
	}
	return fmt.Sprintf("PASS: all %d requests served, 0 failures", r.Total)
}

func meaning(code int) string {
	switch code {
	case 200:
		return "ok"
	case 201:
		return "created"
	case 202:
		return "accepted"
	case 400:
		return "bad request"
	case 401:
		return "unauthorised"
	case 403:
		return "forbidden"
	case 404:
		return "not found"
	case 429:
		return "rate limited"
	case 503:
		return "shed (at capacity)"
	default:
		return ""
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func describeTarget(requests int, d time.Duration) string {
	if d > 0 {
		return d.String()
	}
	return fmt.Sprintf("%d requests", requests)
}

func line(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = '-'
	}
	return string(b)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func minOf(xs []int) int {
	m := xs[0]
	for _, x := range xs {
		if x < m {
			m = x
		}
	}
	return m
}

func maxOf(xs []int) int {
	m := xs[0]
	for _, x := range xs {
		if x > m {
			m = x
		}
	}
	return m
}
