package v1

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Keathan2609/nexus/internal/core/events"
)

// Hub fans control-plane events out to connected console clients.
//
// Server-Sent Events rather than WebSockets. The traffic is entirely one-directional —
// the console renders state, it does not push it — and SSE is plain HTTP, so it passes
// through the same authentication middleware, the same proxies and the same logging as
// every other request, and reconnects on its own. A WebSocket would buy bidirectional
// capability that nothing here needs, in exchange for a second protocol to secure.
type Hub struct {
	mu          sync.RWMutex
	subscribers map[int64]*subscriber
	nextID      int64
}

// maxSubscribers bounds concurrent event-stream connections to this replica.
//
// Each subscriber holds a 64-slot buffer of serialised events, and an SSE connection is
// by design long-lived, so nothing reclaims one until the client goes away. Without a
// ceiling a client that opens streams in a loop grows the map and the buffers until the
// process fails; refusing the connection instead is visible, recoverable, and leaves
// every already-connected console working.
const maxSubscribers = 512

type subscriber struct {
	ch chan []byte
	// filter limits a subscriber to one project. Clients watching a single project
	// should not receive, or be able to observe, traffic from the rest of the estate.
	projectID string
}

func NewHub() *Hub {
	return &Hub{subscribers: map[int64]*subscriber{}}
}

func (h *Hub) subscribe(projectID string) (int64, <-chan []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.subscribers) >= maxSubscribers {
		return 0, nil
	}
	h.nextID++
	id := h.nextID
	// Buffered: a slow client must not block the publisher. If the buffer fills, that
	// client's events are dropped rather than every other client being held up.
	s := &subscriber{ch: make(chan []byte, 64), projectID: projectID}
	h.subscribers[id] = s
	return id, s.ch
}

func (h *Hub) unsubscribe(id int64) {
	if id == 0 {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if s, ok := h.subscribers[id]; ok {
		close(s.ch)
		delete(h.subscribers, id)
	}
}

// Publish delivers an event to every interested subscriber. It never blocks.
func (h *Hub) Publish(env events.Envelope) {
	payload, err := json.Marshal(map[string]any{
		"id":             env.ID,
		"type":           env.Type,
		"aggregate_type": env.AggregateType,
		"aggregate_id":   env.AggregateID,
		"correlation_id": env.CorrelationID,
		"occurred_at":    env.OccurredAt,
		"payload":        env.Payload,
	})
	if err != nil {
		return
	}

	projectID := projectIDOf(env)

	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, s := range h.subscribers {
		if s.projectID != "" && projectID != "" && s.projectID != projectID {
			continue
		}
		select {
		case s.ch <- payload:
		default:
			// Dropped. The console reconciles by refetching on reconnect, so a dropped
			// notification costs a moment of staleness, never correctness — which is
			// only true because Postgres, not this channel, holds the truth.
		}
	}
}

func projectIDOf(env events.Envelope) string {
	var body struct {
		ProjectID string `json:"project_id"`
	}
	if err := json.Unmarshal(env.Payload, &body); err != nil {
		return ""
	}
	return body.ProjectID
}

func (h *Hub) Subscribers() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.subscribers)
}

// ServeHTTP streams events to one client until it disconnects.
func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming is not supported by this connection", http.StatusInternalServerError)
		return
	}

	// The subscription is taken before any header is written, so a refusal can still be
	// expressed as a status code. Once 200 and the SSE content type are on the wire the
	// only way to say no is to hang up, which a client cannot distinguish from a crash.
	projectID := r.URL.Query().Get("project_id")
	if len(projectID) > 64 {
		http.Error(w, "project_id is not a valid identifier", http.StatusBadRequest)
		return
	}
	id, ch := h.subscribe(projectID)
	if ch == nil {
		w.Header().Set("Retry-After", "5")
		http.Error(w, "the event stream is at capacity; retry shortly", http.StatusServiceUnavailable)
		return
	}
	defer h.unsubscribe(id)

	header := w.Header()
	header.Set("Content-Type", "text/event-stream")
	header.Set("Cache-Control", "no-cache, no-transform")
	header.Set("Connection", "keep-alive")
	// Nginx and several ingress controllers buffer responses by default, which turns a
	// live stream into a long silence followed by a burst. This disables that.
	header.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	// Tell the client the stream is live before anything happens, so the console can
	// distinguish "connected, nothing has occurred" from "still connecting".
	_, _ = fmt.Fprintf(w, "event: ready\ndata: {\"subscribed\":true}\n\n")
	flusher.Flush()

	// Heartbeat: an idle SSE connection is indistinguishable from a dead one to any
	// intermediary, and proxies close silent connections. A comment frame keeps it open
	// and costs nothing.
	heartbeat := time.NewTicker(20 * time.Second)
	defer heartbeat.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			_, _ = fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		case payload, open := <-ch:
			if !open {
				return
			}
			// SSE frames are newline-delimited, so any newline inside the payload has
			// to be escaped or the frame is truncated. JSON encoding already does that,
			// but a defensive replace costs nothing and guards against a future change.
			_, _ = fmt.Fprintf(w, "event: nexus\ndata: %s\n\n",
				strings.ReplaceAll(string(payload), "\n", ""))
			flusher.Flush()
		}
	}
}

// Bridge subscribes the hub to the event transport.
//
// The hub is fed from NATS rather than from the outbox writer directly, so every API
// replica broadcasts every event to its own connected clients. Without that, a console
// connected to replica B would miss events produced on replica A.
func (h *Hub) Bridge(ctx context.Context) func(context.Context, events.Envelope) error {
	return func(_ context.Context, env events.Envelope) error {
		h.Publish(env)
		return nil
	}
}
