// Package v1 is the HTTP surface of the control plane.
//
// Routes are organised around the workflows the console and CLI actually perform —
// trigger a run, watch it, deploy a release, verify it, roll back — rather than around
// the entity list. There is no endpoint here that exists only because a table does.
package v1

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/Keathan2609/nexus/internal/api/middleware"
	"github.com/Keathan2609/nexus/internal/audit"
	"github.com/Keathan2609/nexus/internal/auth"
	"github.com/Keathan2609/nexus/internal/deployments"
	"github.com/Keathan2609/nexus/internal/platform/config"
	"github.com/Keathan2609/nexus/internal/platform/database"
	"github.com/Keathan2609/nexus/internal/platform/httpx"
	"github.com/Keathan2609/nexus/internal/platform/telemetry"
	"github.com/Keathan2609/nexus/internal/rbac"
	"github.com/Keathan2609/nexus/internal/scheduler"
	"github.com/Keathan2609/nexus/internal/store/postgres"
	"github.com/Keathan2609/nexus/internal/webhooks"

	"log/slog"
)

type Server struct {
	cfg      config.Config
	db       *database.DB
	log      *slog.Logger
	metrics  *telemetry.Metrics
	auth     *auth.Service
	audit    *audit.Recorder
	read     *postgres.ReadStore
	runs     *postgres.RunStore
	jobs     *postgres.JobStore
	sched    *scheduler.Scheduler
	deploys  *deployments.Service
	webhooks *webhooks.Ingester
	hub      *Hub
	provider string

	// deployAdmission bounds how many deployments this replica drives at once.
	deployAdmission *admission
}

type Deps struct {
	Config      config.Config
	DB          *database.DB
	Log         *slog.Logger
	Metrics     *telemetry.Metrics
	Auth        *auth.Service
	Audit       *audit.Recorder
	Runs        *postgres.RunStore
	Jobs        *postgres.JobStore
	Scheduler   *scheduler.Scheduler
	Deployments *deployments.Service
	Webhooks    *webhooks.Ingester
	Hub         *Hub
	Provider    string
}

func NewServer(d Deps) *Server {
	return &Server{
		cfg: d.Config, db: d.DB, log: d.Log, metrics: d.Metrics,
		auth: d.Auth, audit: d.Audit, read: postgres.NewReadStore(d.DB),
		runs: d.Runs, jobs: d.Jobs, sched: d.Scheduler,
		deploys: d.Deployments, webhooks: d.Webhooks, hub: d.Hub,
		provider:        d.Provider,
		deployAdmission: newAdmission(d.Config.MaxConcurrentDeployments),
	}
}

func (s *Server) Hub() *Hub { return s.hub }

// Routes assembles the router.
func (s *Server) Routes() http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.Identify)
	r.Use(middleware.Recover(s.log))
	r.Use(middleware.SecureHeaders)
	r.Use(middleware.Observe(s.log, s.metrics))
	r.Use(middleware.CORS(s.cfg.CORSOrigins))

	// Operational endpoints sit outside authentication: a readiness probe that needs a
	// credential is a readiness probe that fails during a credential rotation.
	r.Get("/healthz", s.handleLive)
	r.Get("/readyz", s.handleReady)

	// Webhooks authenticate by signature, not by bearer token, so they are mounted
	// before the authenticated group.
	//
	// The endpoint is unauthenticated and publicly reachable, so it carries its own
	// limiter. Signature verification rejects a forged delivery, but only after reading
	// and hashing the body; without a limit, an attacker who cannot forge a signature
	// can still make the process do that work indefinitely. The allowance is generous
	// because a busy repository genuinely does burst.
	webhookLimiter := middleware.NewRateLimiter(20, 100).
		TrustProxyHeaders(s.cfg.TrustProxyHeaders)
	r.Route("/api/v1/webhooks", func(r chi.Router) {
		r.With(webhookLimiter.Limit).Post("/github", s.wrap(s.handleGitHubWebhook))
	})

	r.Route("/api/v1", func(r chi.Router) {
		loginLimiter := middleware.NewRateLimiter(1, 10).
			TrustProxyHeaders(s.cfg.TrustProxyHeaders)
		r.With(loginLimiter.Limit).Post("/auth/login", s.wrap(s.handleLogin))

		r.Group(func(r chi.Router) {
			r.Use(middleware.Authenticate(s.auth))
			// Keyed per principal, so one client's loop cannot consume everybody
			// else's allowance. Setting the rate to zero disables it, which is how a
			// capacity measurement gets the limiter out of the way.
			if s.cfg.APIRateLimit > 0 {
				apiLimiter := middleware.NewRateLimiter(s.cfg.APIRateLimit, s.cfg.APIRateBurst).
					TrustProxyHeaders(s.cfg.TrustProxyHeaders)
				r.Use(apiLimiter.Limit)
			}

			r.Get("/auth/me", s.wrap(s.handleMe))
			r.Post("/auth/tokens", s.wrap(s.handleCreateToken))

			r.Get("/overview", s.wrap(s.handleOverview))
			r.Get("/stream", s.hub.ServeHTTP)

			r.With(middleware.RequirePermission(rbac.ProjectRead)).
				Get("/projects", s.wrap(s.handleListProjects))
			r.With(middleware.RequirePermission(rbac.ProjectCreate)).
				Post("/projects", s.wrap(s.handleCreateProject))
			r.With(middleware.RequirePermission(rbac.ProjectRead)).
				Get("/projects/{projectID}", s.wrap(s.handleGetProject))
			r.With(middleware.RequirePermission(rbac.PipelineRun)).
				Post("/projects/{projectID}/runs", s.wrap(s.handleTriggerRun))

			r.With(middleware.RequirePermission(rbac.PipelineRead)).
				Get("/runs", s.wrap(s.handleListRuns))
			r.With(middleware.RequirePermission(rbac.PipelineRead)).
				Get("/runs/{runID}", s.wrap(s.handleGetRun))
			r.With(middleware.RequirePermission(rbac.PipelineRead)).
				Get("/runs/{runID}/jobs", s.wrap(s.handleRunJobs))
			r.With(middleware.RequirePermission(rbac.PipelineCancel)).
				Post("/runs/{runID}/cancel", s.wrap(s.handleCancelRun))
			r.With(middleware.RequirePermission(rbac.PipelineRead)).
				Get("/jobs/{jobID}/logs", s.wrap(s.handleJobLogs))

			r.With(middleware.RequirePermission(rbac.EnvironmentRead)).
				Get("/environments", s.wrap(s.handleListEnvironments))

			r.With(middleware.RequirePermission(rbac.DeploymentRead)).
				Get("/deployments", s.wrap(s.handleListDeployments))
			r.With(middleware.RequirePermission(rbac.DeploymentRead)).
				Get("/deployments/{deploymentID}", s.wrap(s.handleGetDeployment))
			r.With(middleware.RequirePermission(rbac.DeploymentCreate)).
				Post("/environments/{environmentID}/deployments", s.wrap(s.handleDeploy))
			r.With(middleware.RequirePermission(rbac.DeploymentRollback)).
				Post("/environments/{environmentID}/rollback", s.wrap(s.handleRollback))

			r.With(middleware.RequirePermission(rbac.WorkerRead)).
				Get("/workers", s.wrap(s.handleListWorkers))
			r.With(middleware.RequirePermission(rbac.AuditRead)).
				Get("/audit", s.wrap(s.handleAudit))
		})
	})

	return r
}

// MetricsHandler is served on a separate port so the scrape endpoint is not exposed
// through the public ingress alongside the API.
func (s *Server) MetricsHandler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(s.metrics.Registry(), promhttp.HandlerOpts{}))
	mux.HandleFunc("/healthz", s.handleLive)
	return mux
}

func (s *Server) wrap(h httpx.Handler) http.HandlerFunc {
	return httpx.Wrap(h, middleware.RequestIDOf)
}

// handleLive answers whether the process is running. It deliberately touches nothing
// else: a liveness probe that checks the database restarts the API during a database
// blip, which is precisely when restarting it helps least.
func (s *Server) handleLive(w http.ResponseWriter, r *http.Request) {
	httpx.JSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"service": s.cfg.ServiceName,
		"version": s.cfg.ServiceVersion,
	})
}

// handleReady answers whether the process can serve traffic, which does depend on the
// database.
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	if err := s.db.Ping(ctx); err != nil {
		httpx.JSON(w, http.StatusServiceUnavailable, map[string]any{
			"status": "unavailable",
			"checks": map[string]string{"database": "unreachable"},
		})
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{
		"status": "ready",
		"checks": map[string]string{"database": "ok"},
	})
}

// principal pulls the authenticated identity, which the middleware guarantees exists on
// every route inside the authenticated group.
func principal(r *http.Request) (*auth.Principal, error) {
	p, ok := auth.FromContext(r.Context())
	if !ok {
		return nil, httpx.Unauthorised("authentication is required")
	}
	return p, nil
}

// notFoundOr maps a store error to the right API error.
func notFoundOr(err error, resource string) error {
	if errors.Is(err, postgres.ErrNotFound) {
		return httpx.NotFound(resource)
	}
	return httpx.Internal(err)
}
