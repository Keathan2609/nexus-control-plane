// Command nexus-reconciler runs the control loops that keep recorded state and real
// state in agreement.
//
// It is the component that makes the rest of the system tolerant of lost events and
// dead processes. Nothing here is triggered by anything; every loop reads the world and
// corrects it.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/Keathan2609/nexus/internal/core/events"
	"github.com/Keathan2609/nexus/internal/core/outbox"
	"github.com/Keathan2609/nexus/internal/deployments"
	"github.com/Keathan2609/nexus/internal/platform/config"
	"github.com/Keathan2609/nexus/internal/platform/database"
	"github.com/Keathan2609/nexus/internal/platform/logging"
	"github.com/Keathan2609/nexus/internal/platform/telemetry"
	"github.com/Keathan2609/nexus/internal/reconciler"
	"github.com/Keathan2609/nexus/internal/scheduler"
	"github.com/Keathan2609/nexus/internal/store/postgres"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "nexus-reconciler: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load("nexus-reconciler")
	if err != nil {
		return err
	}
	log := logging.New(cfg.ServiceName, cfg.LogLevel, cfg.LogFormat)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := database.Open(ctx, database.DefaultConfig(cfg.DatabaseURL))
	if err != nil {
		return err
	}
	defer db.Close()

	metrics := telemetry.NewMetrics()

	var publisher outbox.Publisher = outbox.PublisherFunc(
		func(context.Context, events.Envelope) error { return nil })
	if nc, natsPub, err := events.Connect(ctx, cfg.NATSURL); err != nil {
		log.Warn("event transport unavailable", logging.FieldError, err)
	} else {
		defer nc.Close()
		publisher = natsPub
	}

	runs := postgres.NewRunStore(db)
	jobs := postgres.NewJobStore(db)
	sched := scheduler.New(db, runs, jobs)
	relay := outbox.NewRelay(db, publisher)

	deploySvc := deployments.NewService(db,
		deployments.NewDockerApplier("nexus_default"),
		deployments.NewHTTPProber(5*time.Second), log, metrics)

	rcfg := reconciler.DefaultConfig()
	rcfg.Interval = cfg.ReconcileInterval
	rec := reconciler.New(rcfg, db, jobs, sched, deploySvc, relay, log, metrics)

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(metrics.Registry(), promhttp.HandlerOpts{}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	srv := &http.Server{Addr: cfg.MetricsAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("metrics server stopped", logging.FieldError, err)
		}
	}()

	err = rec.Run(ctx)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)

	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	log.Info("reconciler stopped")
	return nil
}
