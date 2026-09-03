package deployments

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"
)

// HTTPProber verifies a deployment by asking it to serve a request.
//
// The probe is a real HTTP GET against the service's health endpoint rather than an
// inspection of container status, because those answer different questions. A container
// can be Running while the process inside it is deadlocked, out of file descriptors, or
// unable to reach its database — all states in which "the deployment succeeded" is false.
type HTTPProber struct {
	client *http.Client
}

func NewHTTPProber(timeout time.Duration) *HTTPProber {
	return &HTTPProber{
		client: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				DialContext:         (&net.Dialer{Timeout: 2 * time.Second}).DialContext,
				TLSHandshakeTimeout: 3 * time.Second,
				// Connection reuse is deliberately disabled. A pooled connection can
				// succeed against a process that has already stopped accepting new
				// ones, which would let a dying service keep passing its health check.
				DisableKeepAlives: true,
			},
		},
	}
}

func (p *HTTPProber) Probe(ctx context.Context, target string) (ProbeResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return ProbeResult{Status: "FAIL", Message: fmt.Sprintf("bad target %q", target)}, nil
	}
	req.Header.Set("User-Agent", "nexus-health-verifier/1")

	started := time.Now()
	resp, err := p.client.Do(req)
	latency := int(time.Since(started).Milliseconds())

	if err != nil {
		if ctx.Err() != nil {
			return ProbeResult{Status: "TIMEOUT", LatencyMS: latency, Message: "probe deadline exceeded"}, nil
		}
		return ProbeResult{Status: "FAIL", LatencyMS: latency, Message: err.Error()}, nil
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return ProbeResult{Status: "PASS", HTTPStatus: resp.StatusCode, LatencyMS: latency}, nil
	}
	return ProbeResult{
		Status:     "FAIL",
		HTTPStatus: resp.StatusCode,
		LatencyMS:  latency,
		Message:    fmt.Sprintf("unhealthy status %d", resp.StatusCode),
	}, nil
}
