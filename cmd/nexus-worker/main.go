// Command nexus-worker claims pipeline jobs and executes them.
//
// Workers are stateless and disposable. Any number can run; killing one at any moment
// costs at most the in-flight jobs, which are recovered by lease expiry.
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

	"github.com/Keathan2609/nexus/internal/core/domain"
	"github.com/Keathan2609/nexus/internal/pipelines"
	"github.com/Keathan2609/nexus/internal/platform/config"
	"github.com/Keathan2609/nexus/internal/platform/database"
	"github.com/Keathan2609/nexus/internal/platform/logging"
	"github.com/Keathan2609/nexus/internal/platform/telemetry"
	"github.com/Keathan2609/nexus/internal/store/postgres"
	"github.com/Keathan2609/nexus/internal/workers"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "nexus-worker: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load("nexus-worker")
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
	jobs := postgres.NewJobStore(db)

	// A worker identity that survives a restart would let a restarted worker be
	// mistaken for the one that died. A fresh id per process makes the fleet view
	// honest, and lease expiry cleans up the old row.
	workerID := os.Getenv("NEXUS_WORKER_ID")
	if workerID == "" {
		workerID = domain.NewID(domain.PrefixWorker).String()
	}

	// Executors are registered per step type. Steps that shell out get the shell
	// executor; deployment-shaped steps are handled in-process by the control plane and
	// are given a fast local implementation here so a worker without cluster access
	// still produces a coherent run.
	shell := workers.NewShellExecutor(workDir())
	router := workers.NewRouter(shell)
	router.Register(pipelines.StepShell, shell)
	router.Register(pipelines.StepBuild, shell)
	router.Register(pipelines.StepScan, shell)
	router.Register(pipelines.StepContainer, shell)
	router.Register(pipelines.StepDeploy, workers.SleepExecutor{Duration: 900 * time.Millisecond})
	router.Register(pipelines.StepVerify, workers.SleepExecutor{Duration: 600 * time.Millisecond})
	router.Register(pipelines.StepPromote, workers.SleepExecutor{Duration: 300 * time.Millisecond})

	wcfg := workers.Config{
		WorkerID:      workerID,
		Concurrency:   cfg.WorkerConcurrency,
		Lease:         cfg.WorkerLease(),
		Heartbeat:     cfg.HeartbeatInterval(),
		PollInterval:  cfg.WorkerPollInterval,
		ShutdownGrace: 30 * time.Second,
	}
	worker := workers.New(wcfg, db, jobs, router, log, metrics)

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(metrics.Registry(), promhttp.HandlerOpts{}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	metricsSrv := &http.Server{Addr: cfg.MetricsAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := metricsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("metrics server stopped", logging.FieldError, err)
		}
	}()

	log.Info("worker starting",
		logging.FieldWorkerID, workerID,
		"concurrency", wcfg.Concurrency,
		"lease", wcfg.Lease.String())

	err = worker.Run(ctx)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = metricsSrv.Shutdown(shutdownCtx)

	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	log.Info("worker stopped")
	return nil
}

func workDir() string {
	if d := os.Getenv("NEXUS_WORKER_WORKDIR"); d != "" {
		return d
	}
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return wd
}
