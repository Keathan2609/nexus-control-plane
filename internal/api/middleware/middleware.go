// Package middleware holds the cross-cutting concerns every API request passes through:
// identity, correlation, logging, metrics, protection and recovery.
package middleware

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/Keathan2609/nexus/internal/auth"
	"github.com/Keathan2609/nexus/internal/core/domain"
	"github.com/Keathan2609/nexus/internal/platform/httpx"
	"github.com/Keathan2609/nexus/internal/platform/logging"
	"github.com/Keathan2609/nexus/internal/platform/telemetry"
	"github.com/Keathan2609/nexus/internal/rbac"
)

type ctxKey int

const (
	requestIDKey ctxKey = iota
	correlationIDKey
)

func RequestIDFrom(ctx context.Context) string {
	v, _ := ctx.Value(requestIDKey).(string)
	return v
}

func CorrelationIDFrom(ctx context.Context) string {
	v, _ := ctx.Value(correlationIDKey).(string)
	return v
}

func RequestIDOf(r *http.Request) string { return RequestIDFrom(r.Context()) }

// Identify assigns a request id and establishes the correlation id.
//
// The distinction matters. The request id identifies this HTTP call. The correlation id
// identifies the whole operation it belongs to, which may span a webhook, a pipeline
// run, several worker processes and a deployment. A client that already has one — the
// CLI following a run, or a retry of an earlier call — sends it and it is honoured.
func Identify(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := r.Header.Get("X-Request-Id")
		if requestID == "" {
			requestID = domain.NewID(domain.PrefixEvent).String()
		}
		correlationID := r.Header.Get("X-Correlation-Id")
		if correlationID == "" {
			correlationID = requestID
		}

		ctx := context.WithValue(r.Context(), requestIDKey, requestID)
		ctx = context.WithValue(ctx, correlationIDKey, correlationID)

		w.Header().Set("X-Request-Id", requestID)
		w.Header().Set("X-Correlation-Id", correlationID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// statusRecorder captures the status code and byte count for logging and metrics.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += n
	return n, err
}

// Flush forwards to the underlying writer so SSE streaming still works through the
// middleware chain. Without this, a wrapped ResponseWriter silently swallows Flush and
// events buffer until the connection closes.
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Observe logs and measures every request.
func Observe(log *slog.Logger, m *telemetry.Metrics) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			started := time.Now()
			rec := &statusRecorder{ResponseWriter: w, status: 0}

			reqLog := log.With(
				logging.FieldRequestID, RequestIDFrom(r.Context()),
				logging.FieldCorrelationID, CorrelationIDFrom(r.Context()),
			)
			ctx := logging.Into(r.Context(), reqLog)

			if m != nil {
				m.HTTPInFlight.Inc()
				defer m.HTTPInFlight.Dec()
			}

			next.ServeHTTP(rec, r.WithContext(ctx))

			if rec.status == 0 {
				rec.status = http.StatusOK
			}
			elapsed := time.Since(started)

			// The chi route pattern, not the raw path: labelling metrics with
			// /api/v1/projects/prj_01J.../runs would create a new time series per
			// project and eventually take Prometheus down.
			route := chi.RouteContext(r.Context()).RoutePattern()
			if route == "" {
				route = "unmatched"
			}
			if m != nil {
				m.ObserveHTTP(r.Method, route, rec.status, elapsed)
			}

			attrs := []any{
				"method", r.Method,
				"path", r.URL.Path,
				"route", route,
				"status", rec.status,
				"duration_ms", elapsed.Milliseconds(),
				"bytes", rec.bytes,
			}
			switch {
			case rec.status >= 500:
				reqLog.Error("request failed", attrs...)
			case rec.status >= 400:
				reqLog.Warn("request rejected", attrs...)
			default:
				reqLog.Info("request", attrs...)
			}
		})
	}
}

// Recover turns a panic into a 500 rather than a dropped connection, and logs it with
// the request id so it can be tied to the client's report.
func Recover(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					requestID := RequestIDFrom(r.Context())
					log.Error("panic recovered",
						logging.FieldRequestID, requestID,
						"panic", rec,
						"path", r.URL.Path)
					httpx.Error(w, requestID, httpx.Internal(nil))
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// SecureHeaders sets the response headers that cost nothing and remove whole classes of
// problem. The CSP is strict because the API serves JSON, not documents: nothing it
// returns should ever be executed or framed.
func SecureHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		h.Set("Permissions-Policy", "geolocation=(), microphone=(), camera=()")
		next.ServeHTTP(w, r)
	})
}

// CORS allows the console origin only.
//
// An explicit allowlist rather than reflecting the Origin header: reflecting it with
// credentials enabled is equivalent to allowing every origin, which is the common way
// this gets quietly wrong.
func CORS(allowed []string) func(http.Handler) http.Handler {
	set := make(map[string]bool, len(allowed))
	for _, o := range allowed {
		set[strings.TrimRight(o, "/")] = true
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := strings.TrimRight(r.Header.Get("Origin"), "/")
			if origin != "" && set[origin] {
				h := w.Header()
				h.Set("Access-Control-Allow-Origin", origin)
				h.Set("Access-Control-Allow-Credentials", "true")
				h.Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
				h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Request-Id, X-Correlation-Id")
				h.Set("Access-Control-Max-Age", "600")
				h.Add("Vary", "Origin")
			}
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// Authenticate resolves a bearer token to a principal and attaches it to the context.
//
// Both token forms are accepted on the same header: a JWT for the console and an nxs_
// API token for the CLI. The prefix disambiguates them, so there is one auth header and
// one code path rather than two schemes.
func Authenticate(svc *auth.Service) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var token string
			if header := r.Header.Get("Authorization"); header != "" {
				bearer, ok := strings.CutPrefix(header, "Bearer ")
				if !ok {
					httpx.Error(w, RequestIDOf(r), httpx.Unauthorised("expected a Bearer token"))
					return
				}
				token = strings.TrimSpace(bearer)
			} else if q := r.URL.Query().Get("token"); q != "" && isStreamRequest(r) {
				// EventSource cannot set request headers, so the event stream — and only
				// the event stream — accepts its credential as a query parameter. The
				// request logger records URL.Path and never RawQuery, so this does not
				// put a token into the logs.
				token = strings.TrimSpace(q)
			}

			if token == "" {
				httpx.Error(w, RequestIDOf(r), httpx.Unauthorised("authentication is required"))
				return
			}

			var (
				principal *auth.Principal
				err       error
			)
			if strings.HasPrefix(token, "nxs_") {
				principal, err = svc.VerifyAPIToken(r.Context(), token)
			} else {
				principal, err = svc.Verify(token)
			}
			if err != nil {
				httpx.Error(w, RequestIDOf(r), httpx.Unauthorised("invalid or expired credentials"))
				return
			}

			ctx := auth.WithPrincipal(r.Context(), principal)
			ctx = logging.Into(ctx, logging.From(ctx).With("user_id", principal.UserID.String()))
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RequirePermission rejects a request whose principal lacks a permission.
func RequirePermission(p rbac.Permission) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			principal, ok := auth.FromContext(r.Context())
			if !ok {
				httpx.Error(w, RequestIDOf(r), httpx.Unauthorised("authentication is required"))
				return
			}
			if err := rbac.Require(principal.Role, p); err != nil {
				httpx.Error(w, RequestIDOf(r),
					httpx.Forbidden("this action requires the "+string(p)+" permission"))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// --- rate limiting -------------------------------------------------------------

// RateLimiter is a fixed-capacity token bucket per client key.
//
// In-process, which means N API replicas allow N times the configured rate. That is
// stated rather than hidden: the purpose here is to stop one misbehaving client from
// saturating a replica, not to enforce a global quota, and a shared counter in Redis
// would add a dependency and a failure mode for a guarantee NEXUS does not need.
type RateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	rate    float64
	burst   float64
	// trustProxy governs whether X-Forwarded-For is believed. Off by default: a header
	// the client controls cannot be a rate-limit key, because the client then simply
	// varies it and every request lands in a fresh bucket with a full allowance.
	trustProxy bool
}

type bucket struct {
	tokens float64
	last   time.Time
}

// maxBuckets caps how many distinct clients are tracked. The eviction loop runs every
// few minutes, which is ample time for a burst of unique keys to exhaust memory in
// between; refusing to grow past this bound turns that from an outage into a shared
// allowance for whoever arrives after the cap.
const maxBuckets = 20_000

func NewRateLimiter(perSecond, burst float64) *RateLimiter {
	rl := &RateLimiter{buckets: map[string]*bucket{}, rate: perSecond, burst: burst}
	go rl.evictLoop()
	return rl
}

// TrustProxyHeaders makes the limiter honour X-Forwarded-For. Enable it only when NEXUS
// genuinely sits behind a proxy that overwrites the header, never merely appends to it.
func (rl *RateLimiter) TrustProxyHeaders(trust bool) *RateLimiter {
	rl.trustProxy = trust
	return rl
}

// evictLoop drops idle buckets so the map does not grow without bound as clients come
// and go.
func (rl *RateLimiter) evictLoop() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		cutoff := time.Now().Add(-10 * time.Minute)
		rl.mu.Lock()
		for k, b := range rl.buckets {
			if b.last.Before(cutoff) {
				delete(rl.buckets, k)
			}
		}
		rl.mu.Unlock()
	}
}

func (rl *RateLimiter) allow(key string) bool {
	now := time.Now()
	rl.mu.Lock()
	defer rl.mu.Unlock()

	b, ok := rl.buckets[key]
	if !ok {
		if len(rl.buckets) >= maxBuckets {
			// At capacity the limiter stops tracking new keys and refuses them. Failing
			// closed is the right direction here: the alternative is letting an
			// unbounded set of keys allocate without limit.
			return false
		}
		rl.buckets[key] = &bucket{tokens: rl.burst - 1, last: now}
		return true
	}
	b.tokens += now.Sub(b.last).Seconds() * rl.rate
	if b.tokens > rl.burst {
		b.tokens = rl.burst
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// Limit rejects requests over the rate, keyed by principal where one exists and by
// client address otherwise. Keying by principal means one user's CLI loop cannot
// exhaust the allowance of everyone behind the same NAT.
func (rl *RateLimiter) Limit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := clientIP(r, rl.trustProxy)
		if p, ok := auth.FromContext(r.Context()); ok {
			key = "user:" + p.UserID.String()
		}
		if !rl.allow(key) {
			w.Header().Set("Retry-After", "1")
			httpx.Error(w, RequestIDOf(r), &httpx.APIError{
				Status:  http.StatusTooManyRequests,
				Code:    httpx.CodeRateLimited,
				Message: "too many requests",
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// clientIP identifies the caller for rate-limiting purposes.
//
// X-Forwarded-For is honoured only when the deployment has declared that it sits behind
// a trusted proxy. Reading it unconditionally is a rate-limit bypass rather than a
// convenience: the header is attacker-controlled, so a client that varies it on every
// request never shares a bucket with itself, and the login limiter in particular stops
// slowing down password guessing at all. The socket address cannot be forged over TCP
// and so is the default.
func clientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			// The left-most entry is the original client, but every entry is
			// attacker-supplied except the one the trusted proxy appended. The
			// right-most is the only one that proxy actually observed.
			if i := strings.LastIndexByte(xff, ','); i >= 0 {
				return strings.TrimSpace(xff[i+1:])
			}
			return strings.TrimSpace(xff)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// isStreamRequest reports whether this is the Server-Sent Events endpoint, which is the
// only route permitted to authenticate by query parameter.
func isStreamRequest(r *http.Request) bool {
	return strings.HasSuffix(r.URL.Path, "/api/v1/stream")
}
