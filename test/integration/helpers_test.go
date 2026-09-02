//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/Keathan2609/nexus/internal/core/domain"
	"github.com/Keathan2609/nexus/internal/platform/database"
	"github.com/Keathan2609/nexus/internal/store/postgres"
)

// openDB returns a migrated database handle scoped to the test.
func openDB(t *testing.T) *database.DB {
	t.Helper()
	ctx := context.Background()
	url := dsn(t)
	if err := database.Migrate(ctx, url); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	db, err := database.Open(ctx, database.DefaultConfig(url))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(db.Close)
	return db
}

// fixture is a minimal object graph: one organisation, project, pipeline definition and
// run, into which tests insert jobs.
type fixture struct {
	OrgID        domain.ID
	UserID       domain.ID
	ProjectID    domain.ID
	DefinitionID domain.ID
	RunID        domain.ID
}

// newFixture creates a fresh, isolated object graph. Every test gets its own
// organisation and project, so tests never contend for the same rows and can run
// against a database that already holds data from earlier runs.
func newFixture(t *testing.T, db *database.DB) fixture {
	t.Helper()

	f := fixture{
		OrgID:        domain.NewID(domain.PrefixOrganisation),
		UserID:       domain.NewID(domain.PrefixUser),
		ProjectID:    domain.NewID(domain.PrefixProject),
		DefinitionID: domain.NewID(domain.PrefixPipeline),
		RunID:        domain.NewID(domain.PrefixRun),
	}
	// Slugs must satisfy the schema CHECK constraints, so derive them from the ULID
	// body (lowercase alphanumeric) rather than from the prefixed id.
	slug := "t" + string(f.OrgID)[len(f.OrgID)-16:]

	mustExec(t, db, `INSERT INTO organisations (id, name, slug) VALUES ($1,$2,$3)`,
		f.OrgID, "Test Org", slug)
	mustExec(t, db, `INSERT INTO users (id, organisation_id, email, name, password_hash)
	                 VALUES ($1,$2,$3,$4,$5)`,
		f.UserID, f.OrgID, slug+"@example.test", "Test User", "x")
	mustExec(t, db, `INSERT INTO projects (id, organisation_id, name, slug, runtime, created_by)
	                 VALUES ($1,$2,$3,$4,$5,$6)`,
		f.ProjectID, f.OrgID, "Test Project", slug, "GO", f.UserID)
	// next_number means "the next number to hand out". The fixture consumes 1 for its
	// own seeded run below, so the counter must already point at 2.
	mustExec(t, db, `INSERT INTO run_counters (project_id, next_number) VALUES ($1, 2)`, f.ProjectID)
	mustExec(t, db, `INSERT INTO pipeline_definitions (id, project_id, key, name, spec)
	                 VALUES ($1,$2,$3,$4,$5)`,
		f.DefinitionID, f.ProjectID, "ci", "CI", `{"steps":[]}`)
	mustExec(t, db, `INSERT INTO pipeline_runs
	                 (id, project_id, definition_id, number, state, trigger, commit_sha, branch, queued_at)
	                 VALUES ($1,$2,$3,$4,'RUNNING','MANUAL','abc1234','main', now())`,
		f.RunID, f.ProjectID, f.DefinitionID, 1)

	return f
}

// queueJob inserts a job already in QUEUED state, ready to be claimed.
func (f fixture) queueJob(t *testing.T, db *database.DB, stepKey string, maxAttempts int) domain.ID {
	t.Helper()
	id := domain.NewID(domain.PrefixJob)
	mustExec(t, db, `INSERT INTO jobs
	    (id, run_id, project_id, step_key, name, type, state, max_attempts, run_at, timeout_seconds)
	    VALUES ($1,$2,$3,$4,$5,'SHELL','QUEUED',$6, now(), 600)`,
		id, f.RunID, f.ProjectID, stepKey, stepKey, maxAttempts)
	return id
}

func mustExec(t *testing.T, db *database.DB, sql string, args ...any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := db.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("exec failed: %v\nSQL: %s", err, sql)
	}
}

func jobState(t *testing.T, db *database.DB, id domain.ID) string {
	t.Helper()
	var state string
	if err := db.QueryRow(context.Background(), `SELECT state FROM jobs WHERE id=$1`, id).Scan(&state); err != nil {
		t.Fatalf("read job state: %v", err)
	}
	return state
}

// leaseShort is the lease tests take when they do not care about expiry. Long enough
// that no test is reaped mid-assertion, short enough that a leak is obvious.
const leaseShort = 60 * time.Second

func runState(t *testing.T, db *database.DB, id domain.ID) string {
	t.Helper()
	var state string
	if err := db.QueryRow(context.Background(),
		`SELECT state FROM pipeline_runs WHERE id=$1`, id).Scan(&state); err != nil {
		t.Fatalf("read run state: %v", err)
	}
	return state
}

func jobStatesByStep(t *testing.T, db *database.DB, runID domain.ID) map[string]string {
	t.Helper()
	rows, err := db.Query(context.Background(),
		`SELECT step_key, state FROM jobs WHERE run_id=$1`, runID)
	if err != nil {
		t.Fatalf("read job states: %v", err)
	}
	defer rows.Close()

	out := map[string]string{}
	for rows.Next() {
		var step, state string
		if err := rows.Scan(&step, &state); err != nil {
			t.Fatalf("scan job state: %v", err)
		}
		out[step] = state
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate job states: %v", err)
	}
	return out
}

// mustCreateRun creates a run from the standard CI definition, failing the test on
// error so callers never dereference a nil run.
func mustCreateRun(t *testing.T, runs *postgres.RunStore, f fixture) *postgres.Run {
	t.Helper()
	run, err := runs.CreateRun(context.Background(), postgres.CreateRunParams{
		ProjectID:    f.ProjectID,
		DefinitionID: f.DefinitionID,
		Definition:   ciDefinition(),
		Trigger:      "MANUAL",
		CommitSHA:    "a84fc12",
		Branch:       "main",
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	return run
}

// claimAndComplete takes whatever job is next claimable in the project and succeeds it.
// Used by scheduler tests that care about DAG progression rather than about which
// specific job ran.
func claimAndComplete(t *testing.T, jobs *postgres.JobStore, f fixture, workerID string) {
	t.Helper()
	ctx := context.Background()
	job, err := jobs.ClaimForProject(ctx, f.ProjectID, workerID, leaseShort)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if job == nil {
		t.Fatal("expected a claimable job, queue was empty")
	}
	if err := jobs.Complete(ctx, job.ID, workerID); err != nil {
		t.Fatalf("complete %s: %v", job.StepKey, err)
	}
}

func jobAttempt(t *testing.T, db *database.DB, id domain.ID) int {
	t.Helper()
	var n int
	if err := db.QueryRow(context.Background(), `SELECT attempt FROM jobs WHERE id=$1`, id).Scan(&n); err != nil {
		t.Fatalf("read job attempt: %v", err)
	}
	return n
}
