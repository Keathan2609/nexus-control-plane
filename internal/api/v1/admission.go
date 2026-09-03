package v1

import (
	"net/http"
	"sync/atomic"

	"github.com/Keathan2609/nexus/internal/platform/httpx"
)

// admission bounds work the API performs outside a request's lifetime.
//
// A deployment is applied and verified asynchronously, which is correct — it takes
// minutes and must not be tied to an HTTP connection — but the obvious way to write
// that is `go execute(...)` per request, and that is unbounded. A caller with a valid
// credential and a loop then converts N requests into N concurrent deployments, each
// holding a twenty-minute context, a database connection and a kubectl invocation,
// until the process runs out of something.
//
// A slot must be taken before the deployment row is created, not after. Shedding later
// would leave a persisted deployment nothing is driving, which the reconciler
// eventually marks FAILED — a worse outcome than telling the caller plainly that the
// control plane is busy and the request was not accepted.
//
// This is admission control, not a queue. Queueing deployments in memory would lose
// them on restart; the honest answer to "more concurrent deployments than this replica
// can drive" is to refuse and let the client retry.
type admission struct {
	slots    chan struct{}
	capacity int
	inFlight atomic.Int64
	// rejected counts shed requests, so saturation is visible as a number rather than
	// as a support question about intermittent 503s.
	rejected atomic.Int64
}

func newAdmission(capacity int) *admission {
	if capacity < 1 {
		capacity = 1
	}
	return &admission{slots: make(chan struct{}, capacity), capacity: capacity}
}

// acquire takes a slot without waiting. Blocking here would only move the queue into
// the HTTP layer, where it would consume a connection and eventually time out anyway.
func (a *admission) acquire() bool {
	select {
	case a.slots <- struct{}{}:
		a.inFlight.Add(1)
		return true
	default:
		a.rejected.Add(1)
		return false
	}
}

func (a *admission) release() {
	select {
	case <-a.slots:
		a.inFlight.Add(-1)
	default:
	}
}

func (a *admission) InFlight() int64 { return a.inFlight.Load() }
func (a *admission) Rejected() int64 { return a.rejected.Load() }
func (a *admission) Capacity() int   { return a.capacity }

// busy is the response for a shed request. 503 with Retry-After rather than 429: the
// caller has not exceeded a quota, the control plane is simply at capacity, and the
// distinction tells a client whether backing off will help.
func busy(resource string) error {
	return &httpx.APIError{
		Status:  http.StatusServiceUnavailable,
		Code:    httpx.CodeUnavailable,
		Message: "the control plane is at capacity for " + resource + "; retry shortly",
	}
}
