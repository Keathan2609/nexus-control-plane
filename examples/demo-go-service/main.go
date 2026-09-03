// Command demo-go-service is a workload for NEXUS to deploy.
//
// It exists so the delivery system has something real to build, ship, probe and roll
// back — and so failure can be demonstrated deterministically rather than by waiting for
// something to go wrong. It is deliberately small; it is not the portfolio piece.
//
// Failure injection:
//
//	FAIL_HEALTHCHECK=true    /healthz returns 503 from the start. Deploying this build
//	                         is how the "bad release" demonstration works: the deployment
//	                         applies cleanly and then fails verification.
//	FAIL_AFTER=30s           /healthz serves normally, then starts failing. This is the
//	                         harder case: the deployment verifies healthy and degrades
//	                         afterwards, which is what the reconciler has to catch.
package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"sync/atomic"
	"time"
)

var version = "dev"

func main() {
	port := env("PORT", "8080")
	failHealth := env("FAIL_HEALTHCHECK", "") == "true"

	var degraded atomic.Bool
	degraded.Store(failHealth)

	if after := env("FAIL_AFTER", ""); after != "" {
		d, err := time.ParseDuration(after)
		if err != nil {
			log.Fatalf("FAIL_AFTER is not a duration: %v", err)
		}
		go func() {
			time.Sleep(d)
			degraded.Store(true)
			log.Printf("degrading deliberately after %s", d)
		}()
	}

	started := time.Now()
	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if degraded.Load() {
			// A real unhealthy service returns a status, not a connection refusal, and
			// says why. NEXUS records this message alongside the probe result.
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{
				"status": "unhealthy",
				"reason": "dependency check failed",
			})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"status":  "ok",
			"version": version,
			"uptime":  time.Since(started).Round(time.Second).String(),
		})
	})

	// Readiness and liveness answer different questions, so they are different
	// endpoints: liveness asks whether to restart the process, readiness whether to
	// send it traffic.
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if degraded.Load() {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "not ready"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "ready"})
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"service": "demo-go-service",
			"version": version,
			"message": "deployed by NEXUS",
		})
	})

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("demo-go-service %s listening on :%s (degraded=%v)", version, port, degraded.Load())
	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
