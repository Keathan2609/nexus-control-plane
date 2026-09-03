// Command nexus-api is the control plane: the REST API, the webhook receiver, the event
// stream the console subscribes to, and the outbox relay that feeds it.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Keathan2609/nexus/internal/api/v1"
	"github.com/Keathan2609/nexus/internal/audit"
	"github.com/Keathan2609/nexus/internal/auth"
	"github.com/Keathan2609/nexus/internal/core/events"
	"github.com/Keathan2609/nexus/internal/core/outbox"
	"github.com/Keathan2609/nexus/internal/deployments"
	"github.com/Keathan2609/nexus/internal/platform/config"
	"github.com/Keathan2609/nexus/internal/platform/database"
	"github.com/Keathan2609/nexus/internal/platform/logging"
	"github.com/Keathan2609/nexus/internal/platform/telemetry"
	"github.com/Keathan2609/nexus/internal/scheduler"
	"github.com/Keathan2609/nexus/internal/store/postgres"
	"github.com/Keathan2609/nexus/internal/webhooks"
)

func main() {
	// A distroless image has no shell, no wget and no curl, so a container healthcheck
	// cannot be a command line. Having the binary probe itself is the standard answer:
	// nothing has to be added to the image, and the check exercises the same address the
	// server is actually bound to.
	if len(os.Args) > 1 && os.Args[1] == "-healthcheck" {
		if err := healthcheck(); err != nil {
			fmt.Fprintf(os.Stderr, "unhealthy: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "nexus-api: %v\n", err)
		os.Exit(1)
	}
}

// healthcheck probes the local readiness endpoint and reports through the exit code.
func healthcheck() error {
	addr := os.Getenv("NEXUS_HTTP_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + addr + "/readyz")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("readiness returned %d", resp.StatusCode)
	}
	return nil
}

func run() error {
	cfg, err := config.Load("nexus-api")
	if err != nil {
		return err
	}
	log := logging.New(cfg.ServiceName, cfg.LogLevel, cfg.LogFormat)

	// Signals are handled before anything is opened, so a Ctrl+C during startup still
	// unwinds cleanly rather than leaving a half-initialised process behind.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	metrics := telemetry.NewMetrics()
	tracing, err := telemetry.SetupTracing(ctx, cfg.ServiceName, cfg.ServiceVersion, cfg.OTELEndpoint, cfg.TraceSampleRate)
	if err != nil {
		log.Warn("tracing is disabled", logging.FieldError, err)
	} else {
		defer func() { _ = tracing.Shutdown(context.Background()) }()
	}

	if cfg.AutoMigrate {
		if err := database.Migrate(ctx, cfg.DatabaseURL); err != nil {
			return fmt.Errorf("apply migrations: %w", err)
		}
		log.Info("schema is up to date")
	}

	db, err := database.Open(ctx, database.DefaultConfig(cfg.DatabaseURL))
	if err != nil {
		return err
	}
	defer db.Close()

	// NATS is optional at startup. The control plane is fully functional without it —
	// runs still execute, because Postgres holds the work — so an unreachable broker
	// degrades realtime updates rather than preventing the API from serving.
	hub := v1.NewHub()
	var publisher outbox.Publisher = outbox.PublisherFunc(
		func(context.Context, events.Envelope) error { return nil })

	nc, natsPub, err := events.Connect(ctx, cfg.NATSURL)
	if err != nil {
		log.Warn("event transport unavailable; realtime updates will be degraded",
			logging.FieldError, err)
	} else {
		defer nc.Close()
		publisher = natsPub
		if err := events.Subscribe(ctx, nc, "nexus-console", []string{events.SubjectAll}, hub.Bridge(ctx)); err != nil {
			log.Warn("could not subscribe the console stream", logging.FieldError, err)
		}
		log.Info("connected to the event transport", "url", cfg.NATSURL)
	}

	// The relay is instrumented so a publisher failure is visible as a metric rather
	// than only as a growing table.
	instrumented := outbox.PublisherFunc(func(ctx context.Context, e events.Envelope) error {
		if err := publisher.Publish(ctx, e); err != nil {
			metrics.OutboxFailures.Inc()
			return err
		}
		metrics.EventsPublished.WithLabelValues(string(e.Type)).Inc()
		// Also fan out locally, so a console attached to this replica sees the event
		// even when the broker is down.
		hub.Publish(e)
		return nil
	})
	relay := outbox.NewRelay(db, instrumented)

	runs := postgres.NewRunStore(db)
	jobs := postgres.NewJobStore(db)
	sched := scheduler.New(db, runs, jobs)
	authSvc := auth.NewService(db, cfg.JWTSecretOrDevDefault(), cfg.TokenTTL)
	auditor := audit.NewRecorder(db)

	applier, provider := deployments.NewApplier(ctx,
		deployments.Provider(cfg.DeployProvider),
		cfg.KubeContext, cfg.KindCluster, "nexus_default", log)
	deploySvc := deployments.NewService(db, applier,
		deployments.NewHTTPProber(5*time.Second), log, metrics)
	log.Info("deployment provider selected", "provider", string(provider))

	server := v1.NewServer(v1.Deps{
		Config: cfg, DB: db, Log: log, Metrics: metrics,
		Auth: authSvc, Audit: auditor, Runs: runs, Jobs: jobs,
		Scheduler: sched, Deployments: deploySvc,
		Webhooks: webhooks.NewIngester(db, runs, cfg.WebhookSecret, log, metrics),
		Hub:      hub,
		Provider: string(provider),
	})

	go func() {
		if err := relay.Run(ctx, cfg.OutboxInterval, cfg.OutboxBatchSize); err != nil &&
			!errors.Is(err, context.Canceled) {
			log.Error("outbox relay stopped", logging.FieldError, err)
		}
	}()

	api := &http.Server{
		Addr:    cfg.HTTPAddr,
		Handler: server.Routes(),
		// ReadHeaderTimeout specifically, not ReadTimeout: a WriteTimeout or a blanket
		// ReadTimeout would sever the SSE stream on a fixed schedule, which is the one
		// connection that is supposed to stay open indefinitely.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	metricsSrv := &http.Server{
		Addr:              cfg.MetricsAddr,
		Handler:           server.MetricsHandler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 2)
	go func() {
		log.Info("api listening", "addr", cfg.HTTPAddr, "environment", cfg.Environment)
		if err := api.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("api server: %w", err)
		}
	}()
	go func() {
		log.Info("metrics listening", "addr", cfg.MetricsAddr)
		if err := metricsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("metrics server: %w", err)
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Info("shutdown signal received")
	}

	// Drain with a bounded grace period. In-flight requests finish; SSE connections are
	// closed by the shutdown itself and the console reconnects.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if err := api.Shutdown(shutdownCtx); err != nil {
		log.Warn("api did not shut down cleanly", logging.FieldError, err)
	}
	if err := metricsSrv.Shutdown(shutdownCtx); err != nil {
		log.Warn("metrics server did not shut down cleanly", logging.FieldError, err)
	}

	// One last drain, so events produced by requests served during shutdown are not
	// left sitting in the outbox until the next replica happens to pick them up.
	if _, err := relay.Drain(shutdownCtx, cfg.OutboxBatchSize); err != nil {
		log.Warn("final outbox drain failed", logging.FieldError, err)
	}

	log.Info("stopped")
	return nil
}
