//go:build integration

// Package integration exercises NEXUS against real PostgreSQL and NATS instances.
//
// These tests are guarded by the `integration` build tag so `go test ./...` stays fast
// and dependency-free. Run them with:
//
//	go test -tags=integration ./test/integration/...
package integration

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/Keathan2609/nexus/internal/platform/database"
)

func dsn(t *testing.T) string {
	t.Helper()
	url := os.Getenv("NEXUS_TEST_DATABASE_URL")
	if url == "" {
		url = "postgres://nexus:nexus@localhost:5436/nexus?sslmode=disable"
	}
	return url
}

func TestMigrationsApply(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := database.Migrate(ctx, dsn(t)); err != nil {
		t.Fatalf("migrations failed to apply: %v", err)
	}

	v, err := database.Version(ctx, dsn(t))
	if err != nil {
		t.Fatalf("could not read schema version: %v", err)
	}
	if v < 5 {
		t.Fatalf("expected at least schema version 5, got %d", v)
	}
}

func TestMigrationsAreIdempotent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Applying an already-current schema must be a no-op, not an error. Every API
	// replica runs this on boot.
	for i := 0; i < 3; i++ {
		if err := database.Migrate(ctx, dsn(t)); err != nil {
			t.Fatalf("re-applying migrations failed on pass %d: %v", i+1, err)
		}
	}
}

func TestExpectedTablesExist(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := database.Migrate(ctx, dsn(t)); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	db, err := database.Open(ctx, database.DefaultConfig(dsn(t)))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	want := []string{
		"organisations", "users", "teams", "memberships", "api_tokens",
		"projects", "repositories", "environments", "services",
		"pipeline_definitions", "pipeline_runs", "run_counters", "jobs",
		"job_attempts", "job_logs", "workers",
		"artifacts", "releases", "deployments", "deployment_counters",
		"health_checks", "gitops_revisions", "drift_records", "incidents",
		"outbox", "processed_events", "webhook_events", "audit_events",
		"secret_references",
	}
	for _, table := range want {
		var exists bool
		err := db.QueryRow(ctx,
			`SELECT EXISTS (
			   SELECT 1 FROM information_schema.tables
			   WHERE table_schema='public' AND table_name=$1
			 )`, table).Scan(&exists)
		if err != nil {
			t.Fatalf("checking %s: %v", table, err)
		}
		if !exists {
			t.Errorf("expected table %q to exist after migration", table)
		}
	}
}

func TestClaimIndexExists(t *testing.T) {
	// The partial index the dispatch query depends on. If someone removes it, job
	// claiming silently degrades to a sequential scan under load rather than failing,
	// so it is worth asserting on directly.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := database.Migrate(ctx, dsn(t)); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	db, err := database.Open(ctx, database.DefaultConfig(dsn(t)))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	var indexdef string
	err = db.QueryRow(ctx,
		`SELECT indexdef FROM pg_indexes
		 WHERE schemaname='public' AND indexname='jobs_claimable_idx'`).Scan(&indexdef)
	if err != nil {
		t.Fatalf("jobs_claimable_idx is missing: %v", err)
	}
	if indexdef == "" {
		t.Fatal("empty index definition")
	}
	t.Logf("claim index: %s", indexdef)
}

func TestOneLiveDeploymentPerEnvironmentIsEnforced(t *testing.T) {
	// This constraint is the database-level guard against two concurrent promotions
	// both believing they own an environment. Asserting it here means a future
	// migration cannot quietly drop it.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := database.Migrate(ctx, dsn(t)); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	db, err := database.Open(ctx, database.DefaultConfig(dsn(t)))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	var exists bool
	err = db.QueryRow(ctx,
		`SELECT EXISTS (
		   SELECT 1 FROM pg_indexes
		   WHERE schemaname='public' AND indexname='deployments_one_live_per_environment'
		 )`).Scan(&exists)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if !exists {
		t.Fatal("deployments_one_live_per_environment index is missing")
	}
}
