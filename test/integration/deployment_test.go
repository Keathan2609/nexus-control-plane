//go:build integration

package integration

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Keathan2609/nexus/internal/core/domain"
	"github.com/Keathan2609/nexus/internal/deployments"
	"github.com/Keathan2609/nexus/internal/platform/database"
	"github.com/Keathan2609/nexus/internal/store/postgres"
)

// fakeApplier stands in for Kubernetes. It records what it was asked to run and can be
// told to fail, so the deployment lifecycle can be exercised without a cluster.
type fakeApplier struct {
	applied  []string
	applyErr error
}

func (f *fakeApplier) Apply(ctx context.Context, d deployments.Desired) (string, error) {
	if f.applyErr != nil {
		return "", f.applyErr
	}
	f.applied = append(f.applied, d.Revision)
	return d.Revision, nil
}

func (f *fakeApplier) Observe(ctx context.Context, environmentID domain.ID) (string, error) {
	if len(f.applied) == 0 {
		return "", nil
	}
	return f.applied[len(f.applied)-1], nil
}

func (f *fakeApplier) Destroy(ctx context.Context, environmentID domain.ID) error { return nil }

// fakeProber returns a scripted sequence of health results.
type fakeProber struct {
	results []bool
	calls   int
}

func (p *fakeProber) Probe(ctx context.Context, target string) (deployments.ProbeResult, error) {
	i := p.calls
	p.calls++
	if i >= len(p.results) {
		i = len(p.results) - 1
	}
	if i < 0 || !p.results[i] {
		return deployments.ProbeResult{Status: "FAIL", Message: "connection refused"}, nil
	}
	return deployments.ProbeResult{Status: "PASS", HTTPStatus: 200, LatencyMS: 4}, nil
}

func deploymentFixture(t *testing.T, db *database.DB, f fixture) (envID, releaseID domain.ID) {
	t.Helper()
	envID = domain.NewID(domain.PrefixEnvironment)
	artifactID := domain.NewID(domain.PrefixArtifact)
	releaseID = domain.NewID(domain.PrefixRelease)

	mustExec(t, db, `INSERT INTO environments (id, project_id, name, kind, namespace, url)
	                 VALUES ($1,$2,'production','PRODUCTION',$3,'http://localhost:9999')`,
		envID, f.ProjectID, "nexus-prod-"+string(envID)[len(envID)-8:])
	mustExec(t, db, `INSERT INTO artifacts (id, project_id, kind, reference, digest)
	                 VALUES ($1,$2,'CONTAINER_IMAGE',$3,$4)`,
		artifactID, f.ProjectID, "demo:1.0.0", "sha256:aaa")
	mustExec(t, db, `INSERT INTO releases (id, project_id, version, commit_sha, artifact_id)
	                 VALUES ($1,$2,'1.0.0','a84fc12',$3)`,
		releaseID, f.ProjectID, artifactID)
	mustExec(t, db, `INSERT INTO deployment_counters (project_id, next_number) VALUES ($1,1)
	                 ON CONFLICT DO NOTHING`, f.ProjectID)
	return envID, releaseID
}

func newRelease(t *testing.T, db *database.DB, f fixture, version, sha string) domain.ID {
	t.Helper()
	artifactID := domain.NewID(domain.PrefixArtifact)
	releaseID := domain.NewID(domain.PrefixRelease)
	mustExec(t, db, `INSERT INTO artifacts (id, project_id, kind, reference, digest)
	                 VALUES ($1,$2,'CONTAINER_IMAGE',$3,$4)`,
		artifactID, f.ProjectID, "demo:"+version, "sha256:"+version)
	mustExec(t, db, `INSERT INTO releases (id, project_id, version, commit_sha, artifact_id)
	                 VALUES ($1,$2,$3,$4,$5)`, releaseID, f.ProjectID, version, sha, artifactID)
	return releaseID
}

func deploymentState(t *testing.T, db *database.DB, id domain.ID) string {
	t.Helper()
	var s string
	if err := db.QueryRow(context.Background(),
		`SELECT state FROM deployments WHERE id=$1`, id).Scan(&s); err != nil {
		t.Fatalf("read deployment state: %v", err)
	}
	return s
}

func newService(db *database.DB, applier deployments.Applier, prober deployments.Prober) *deployments.Service {
	svc := deployments.NewService(db, applier, prober, quietLogger(), nil)
	// Keep verification fast in tests; production defaults are in DefaultVerification.
	svc.Verification = deployments.Verification{
		RequiredPasses: 2,
		MaxAttempts:    4,
		Interval:       10 * time.Millisecond,
		Timeout:        5 * time.Second,
	}
	return svc
}

func TestDeploymentReachesHealthyOnlyAfterVerification(t *testing.T) {
	db := openDB(t)
	f := newFixture(t, db)
	envID, releaseID := deploymentFixture(t, db, f)
	ctx := context.Background()

	svc := newService(db, &fakeApplier{}, &fakeProber{results: []bool{true, true}})

	dep, err := svc.Request(ctx, deployments.RequestParams{
		ProjectID: f.ProjectID, EnvironmentID: envID, ReleaseID: releaseID,
		RequestedBy: f.UserID, Reason: "initial",
	})
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if got := deploymentState(t, db, dep.ID); got != "REQUESTED" {
		t.Fatalf("a requested deployment starts REQUESTED, got %s", got)
	}

	if err := svc.Execute(ctx, dep.ID); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if got := deploymentState(t, db, dep.ID); got != "HEALTHY" {
		t.Fatalf("expected HEALTHY, got %s", got)
	}

	// The evidence must be recorded, not just the conclusion.
	var checks int
	if err := db.QueryRow(ctx,
		`SELECT count(*) FROM health_checks WHERE deployment_id=$1`, dep.ID).Scan(&checks); err != nil {
		t.Fatalf("count health checks: %v", err)
	}
	if checks < 2 {
		t.Fatalf("expected health check evidence to be persisted, found %d rows", checks)
	}
}

// The rule the whole system is built around: a deployment that applied cleanly but
// cannot be verified is a failure, not a success.
func TestDeploymentFailsWhenHealthVerificationNeverPasses(t *testing.T) {
	db := openDB(t)
	f := newFixture(t, db)
	envID, releaseID := deploymentFixture(t, db, f)
	ctx := context.Background()

	applier := &fakeApplier{}
	svc := newService(db, applier, &fakeProber{results: []bool{false}})

	dep, err := svc.Request(ctx, deployments.RequestParams{
		ProjectID: f.ProjectID, EnvironmentID: envID, ReleaseID: releaseID, RequestedBy: f.UserID,
	})
	if err != nil {
		t.Fatalf("request: %v", err)
	}

	err = svc.Execute(ctx, dep.ID)
	if err == nil {
		t.Fatal("a deployment that never verifies must report an error")
	}
	if got := deploymentState(t, db, dep.ID); got != "FAILED" {
		t.Fatalf("expected FAILED, got %s", got)
	}
	// It applied successfully — the failure is specifically the verification.
	if len(applier.applied) == 0 {
		t.Fatal("the apply should still have happened")
	}
}

func TestDeploymentFailsWhenApplyFails(t *testing.T) {
	db := openDB(t)
	f := newFixture(t, db)
	envID, releaseID := deploymentFixture(t, db, f)
	ctx := context.Background()

	svc := newService(db,
		&fakeApplier{applyErr: errors.New("cluster unreachable")},
		&fakeProber{results: []bool{true, true}})

	dep, _ := svc.Request(ctx, deployments.RequestParams{
		ProjectID: f.ProjectID, EnvironmentID: envID, ReleaseID: releaseID, RequestedBy: f.UserID,
	})
	if err := svc.Execute(ctx, dep.ID); err == nil {
		t.Fatal("expected an error when apply fails")
	}
	if got := deploymentState(t, db, dep.ID); got != "FAILED" {
		t.Fatalf("expected FAILED, got %s", got)
	}
}

// A flapping target must not be declared healthy on the strength of one lucky probe.
func TestVerificationRequiresConsecutivePasses(t *testing.T) {
	db := openDB(t)
	f := newFixture(t, db)
	envID, releaseID := deploymentFixture(t, db, f)
	ctx := context.Background()

	// pass, fail, pass, fail — never two in a row within the attempt budget.
	svc := newService(db, &fakeApplier{}, &fakeProber{results: []bool{true, false, true, false}})

	dep, _ := svc.Request(ctx, deployments.RequestParams{
		ProjectID: f.ProjectID, EnvironmentID: envID, ReleaseID: releaseID, RequestedBy: f.UserID,
	})
	if err := svc.Execute(ctx, dep.ID); err == nil {
		t.Fatal("a flapping target must not be accepted as healthy")
	}
	if got := deploymentState(t, db, dep.ID); got != "FAILED" {
		t.Fatalf("expected FAILED, got %s", got)
	}
}

func TestSuccessfulDeploymentSupersedesThePreviousOne(t *testing.T) {
	db := openDB(t)
	f := newFixture(t, db)
	envID, firstRelease := deploymentFixture(t, db, f)
	ctx := context.Background()
	svc := newService(db, &fakeApplier{}, &fakeProber{results: []bool{true, true}})

	first, _ := svc.Request(ctx, deployments.RequestParams{
		ProjectID: f.ProjectID, EnvironmentID: envID, ReleaseID: firstRelease, RequestedBy: f.UserID,
	})
	if err := svc.Execute(ctx, first.ID); err != nil {
		t.Fatalf("first deploy: %v", err)
	}

	secondRelease := newRelease(t, db, f, "2.0.0", "b95fd23")
	second, _ := svc.Request(ctx, deployments.RequestParams{
		ProjectID: f.ProjectID, EnvironmentID: envID, ReleaseID: secondRelease, RequestedBy: f.UserID,
	})
	if err := svc.Execute(ctx, second.ID); err != nil {
		t.Fatalf("second deploy: %v", err)
	}

	if got := deploymentState(t, db, first.ID); got != "SUPERSEDED" {
		t.Fatalf("the previous deployment must be SUPERSEDED, got %s", got)
	}
	if got := deploymentState(t, db, second.ID); got != "HEALTHY" {
		t.Fatalf("the new deployment must be HEALTHY, got %s", got)
	}
}

// Rollback restores the last known-good release and records who asked for it.
func TestRollbackRestoresThePreviousHealthyRelease(t *testing.T) {
	db := openDB(t)
	f := newFixture(t, db)
	envID, goodRelease := deploymentFixture(t, db, f)
	ctx := context.Background()

	applier := &fakeApplier{}
	prober := &fakeProber{results: []bool{true, true}}
	svc := newService(db, applier, prober)

	good, _ := svc.Request(ctx, deployments.RequestParams{
		ProjectID: f.ProjectID, EnvironmentID: envID, ReleaseID: goodRelease, RequestedBy: f.UserID,
	})
	if err := svc.Execute(ctx, good.ID); err != nil {
		t.Fatalf("deploy good release: %v", err)
	}

	// A bad release goes out and fails verification.
	badRelease := newRelease(t, db, f, "2.0.0-bad", "deadbee")
	svc.Prober = &fakeProber{results: []bool{false}}
	bad, _ := svc.Request(ctx, deployments.RequestParams{
		ProjectID: f.ProjectID, EnvironmentID: envID, ReleaseID: badRelease, RequestedBy: f.UserID,
	})
	if err := svc.Execute(ctx, bad.ID); err == nil {
		t.Fatal("the bad deployment should have failed verification")
	}

	// Roll back.
	svc.Prober = &fakeProber{results: []bool{true, true}}
	rb, err := svc.Rollback(ctx, deployments.RollbackParams{
		EnvironmentID: envID, RequestedBy: f.UserID, Reason: "verification failed",
	})
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if got := deploymentState(t, db, rb.ID); got != "HEALTHY" {
		t.Fatalf("the rollback deployment must verify healthy, got %s", got)
	}

	var restored domain.ID
	var requestedBy domain.ID
	if err := db.QueryRow(ctx,
		`SELECT release_id, requested_by FROM deployments WHERE id=$1`, rb.ID).
		Scan(&restored, &requestedBy); err != nil {
		t.Fatalf("read rollback: %v", err)
	}
	if restored != goodRelease {
		t.Fatalf("rollback must restore the last healthy release, restored %s", restored)
	}
	if requestedBy != f.UserID {
		t.Fatal("rollback must record who requested it")
	}
	if applier.applied[len(applier.applied)-1] == "" {
		t.Fatal("rollback must actually re-apply the earlier revision")
	}
}

func TestRollbackFailsWhenThereIsNoEarlierHealthyRelease(t *testing.T) {
	db := openDB(t)
	f := newFixture(t, db)
	envID, _ := deploymentFixture(t, db, f)
	ctx := context.Background()

	svc := newService(db, &fakeApplier{}, &fakeProber{results: []bool{true, true}})
	_, err := svc.Rollback(ctx, deployments.RollbackParams{
		EnvironmentID: envID, RequestedBy: f.UserID, Reason: "nothing to go back to",
	})
	if err == nil {
		t.Fatal("rollback with no prior healthy release must fail explicitly")
	}
}

func TestDeploymentHistoryIsRetained(t *testing.T) {
	db := openDB(t)
	f := newFixture(t, db)
	envID, releaseID := deploymentFixture(t, db, f)
	ctx := context.Background()
	svc := newService(db, &fakeApplier{}, &fakeProber{results: []bool{true, true}})

	first, _ := svc.Request(ctx, deployments.RequestParams{
		ProjectID: f.ProjectID, EnvironmentID: envID, ReleaseID: releaseID, RequestedBy: f.UserID,
	})
	_ = svc.Execute(ctx, first.ID)

	second := newRelease(t, db, f, "3.0.0", "cafe123")
	dep2, _ := svc.Request(ctx, deployments.RequestParams{
		ProjectID: f.ProjectID, EnvironmentID: envID, ReleaseID: second, RequestedBy: f.UserID,
	})
	_ = svc.Execute(ctx, dep2.ID)

	history, err := svc.History(ctx, envID, 10)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(history) < 2 {
		t.Fatalf("expected at least 2 deployments in history, got %d", len(history))
	}
	// Newest first.
	if history[0].ID != dep2.ID {
		t.Fatalf("history must be newest first, got %s", history[0].ID)
	}
}

func TestDeploymentEmitsEvents(t *testing.T) {
	db := openDB(t)
	f := newFixture(t, db)
	envID, releaseID := deploymentFixture(t, db, f)
	ctx := context.Background()
	svc := newService(db, &fakeApplier{}, &fakeProber{results: []bool{true, true}})

	dep, _ := svc.Request(ctx, deployments.RequestParams{
		ProjectID: f.ProjectID, EnvironmentID: envID, ReleaseID: releaseID, RequestedBy: f.UserID,
	})
	if err := svc.Execute(ctx, dep.ID); err != nil {
		t.Fatalf("execute: %v", err)
	}

	var types []string
	rows, err := db.Query(ctx,
		`SELECT event_type FROM outbox WHERE aggregate_id=$1 ORDER BY id`, dep.ID)
	if err != nil {
		t.Fatalf("read outbox: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan: %v", err)
		}
		types = append(types, s)
	}
	if len(types) < 2 {
		t.Fatalf("a deployment must emit events for its lifecycle, got %v", types)
	}
}

var _ = postgres.NewJobStore
