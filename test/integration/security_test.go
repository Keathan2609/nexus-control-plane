//go:build integration

// Security tests exercise the API as a hostile client rather than as the console.
//
// Every test here asks the same question in a different place: is the check on the
// server? A UI that hides a button proves nothing, and neither does a handler that
// validates the shape of an identifier without asking who owns it. These drive real
// HTTP requests through the real router, middleware chain and store.
package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	v1 "github.com/Keathan2609/nexus/internal/api/v1"
	"github.com/Keathan2609/nexus/internal/audit"
	"github.com/Keathan2609/nexus/internal/auth"
	"github.com/Keathan2609/nexus/internal/core/domain"
	"github.com/Keathan2609/nexus/internal/deployments"
	"github.com/Keathan2609/nexus/internal/platform/config"
	"github.com/Keathan2609/nexus/internal/platform/database"
	"github.com/Keathan2609/nexus/internal/platform/telemetry"
	"github.com/Keathan2609/nexus/internal/rbac"
	"github.com/Keathan2609/nexus/internal/scheduler"
	"github.com/Keathan2609/nexus/internal/store/postgres"
	"github.com/Keathan2609/nexus/internal/webhooks"
)

// apiHarness is a real server on a real database, reachable over real HTTP.
type apiHarness struct {
	srv  *httptest.Server
	db   *database.DB
	auth *auth.Service
}

func newAPIHarness(t *testing.T, db *database.DB) *apiHarness {
	t.Helper()

	cfg := config.Config{
		Environment:    "test",
		ServiceName:    "nexus-api-test",
		ServiceVersion: "test",
		TokenTTL:       time.Hour,
		CORSOrigins:    []string{"http://localhost:3100"},
		WebhookSecret:  "test-webhook-secret",
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	metrics := telemetry.NewMetrics()

	runs := postgres.NewRunStore(db)
	jobs := postgres.NewJobStore(db)
	authSvc := auth.NewService(db, []byte("test-signing-key-at-least-32-bytes-long"), time.Hour)

	server := v1.NewServer(v1.Deps{
		Config: cfg, DB: db, Log: log, Metrics: metrics,
		Auth: authSvc, Audit: audit.NewRecorder(db), Runs: runs, Jobs: jobs,
		Scheduler:   scheduler.New(db, runs, jobs),
		Deployments: deployments.NewService(db, nil, nil, log, metrics),
		Webhooks:    webhooks.NewIngester(db, runs, cfg.WebhookSecret, log, metrics),
		Hub:         v1.NewHub(),
		Provider:    "docker",
	})

	srv := httptest.NewServer(server.Routes())
	t.Cleanup(srv.Close)
	return &apiHarness{srv: srv, db: db, auth: authSvc}
}

// tokenFor mints a credential for a fixture's organisation at the given role, so a test
// can act as a real principal without going through a password login.
func (h *apiHarness) tokenFor(t *testing.T, f fixture, role rbac.Role) string {
	t.Helper()
	token, err := h.auth.Issue(&auth.Principal{
		UserID:         f.UserID,
		OrganisationID: f.OrgID,
		Email:          "tester@example.test",
		Name:           "Tester",
		Role:           role,
	})
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}
	return token
}

func (h *apiHarness) do(t *testing.T, method, path, token string, body any) *http.Response {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, h.srv.URL+path, reader)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("request %s %s: %v", method, path, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func bodyOf(t *testing.T, resp *http.Response) string {
	t.Helper()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(raw)
}

// --- tenant isolation ----------------------------------------------------------
//
// Each of these holds a valid, well-formed identifier belonging to somebody else. A
// correctly scoped endpoint cannot tell the difference between "does not exist" and
// "exists in another organisation", and must answer 404 to both.

func TestForeignRunJobsAreNotReadable(t *testing.T) {
	db := openDB(t)
	h := newAPIHarness(t, db)

	victim := newFixture(t, db)
	victim.queueJob(t, db, "build", 1)
	attacker := newFixture(t, db)

	token := h.tokenFor(t, attacker, rbac.RoleAdmin)
	resp := h.do(t, http.MethodGet, "/api/v1/runs/"+victim.RunID.String()+"/jobs", token, nil)

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("reading another organisation's run jobs must be 404, got %d: %s",
			resp.StatusCode, bodyOf(t, resp))
	}
}

func TestForeignJobLogsAreNotReadable(t *testing.T) {
	db := openDB(t)
	h := newAPIHarness(t, db)

	victim := newFixture(t, db)
	jobID := victim.queueJob(t, db, "build", 1)
	mustExec(t, db, `INSERT INTO job_logs (job_id, seq, attempt, stream, message)
	                 VALUES ($1,1,1,'stdout',$2)`, jobID, "AWS_SECRET_ACCESS_KEY leaked in build output")

	attacker := newFixture(t, db)
	token := h.tokenFor(t, attacker, rbac.RoleAdmin)
	resp := h.do(t, http.MethodGet, "/api/v1/jobs/"+jobID.String()+"/logs", token, nil)

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("reading another organisation's job logs must be 404, got %d", resp.StatusCode)
	}
	if strings.Contains(bodyOf(t, resp), "leaked in build output") {
		t.Fatal("another organisation's build output was disclosed")
	}
}

func TestForeignProjectCannotBeTriggered(t *testing.T) {
	db := openDB(t)
	h := newAPIHarness(t, db)

	victim := newFixture(t, db)
	attacker := newFixture(t, db)

	token := h.tokenFor(t, attacker, rbac.RoleAdmin)
	resp := h.do(t, http.MethodPost,
		"/api/v1/projects/"+victim.ProjectID.String()+"/runs", token,
		map[string]any{"branch": "main"})

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("triggering another organisation's pipeline must be 404, got %d: %s",
			resp.StatusCode, bodyOf(t, resp))
	}

	var runs int
	if err := db.QueryRow(context.Background(),
		`SELECT count(*) FROM pipeline_runs WHERE project_id=$1`, victim.ProjectID).Scan(&runs); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if runs != 1 {
		t.Fatalf("no run may be created in another organisation's project, found %d", runs)
	}
}

func TestForeignEnvironmentCannotBeRolledBack(t *testing.T) {
	db := openDB(t)
	h := newAPIHarness(t, db)

	victim := newFixture(t, db)
	envID := victim.environment(t, db, "production")
	attacker := newFixture(t, db)

	token := h.tokenFor(t, attacker, rbac.RoleAdmin)
	resp := h.do(t, http.MethodPost,
		"/api/v1/environments/"+envID.String()+"/rollback", token,
		map[string]any{"reason": "hostile"})

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("rolling back another organisation's environment must be 404, got %d: %s",
			resp.StatusCode, bodyOf(t, resp))
	}
}

func TestForeignDeploymentIsNotReadable(t *testing.T) {
	db := openDB(t)
	h := newAPIHarness(t, db)

	victim := newFixture(t, db)
	envID := victim.environment(t, db, "production")
	depID := victim.deployment(t, db, envID)
	attacker := newFixture(t, db)

	token := h.tokenFor(t, attacker, rbac.RoleAdmin)
	resp := h.do(t, http.MethodGet, "/api/v1/deployments/"+depID.String(), token, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("reading another organisation's deployment must be 404, got %d", resp.StatusCode)
	}
}

func TestForeignRunIsNotReadable(t *testing.T) {
	db := openDB(t)
	h := newAPIHarness(t, db)

	victim := newFixture(t, db)
	attacker := newFixture(t, db)

	token := h.tokenFor(t, attacker, rbac.RoleAdmin)
	resp := h.do(t, http.MethodGet, "/api/v1/runs/"+victim.RunID.String(), token, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("reading another organisation's run must be 404, got %d", resp.StatusCode)
	}
}

// A list endpoint must not leak across the boundary either, even unfiltered.
func TestListRunsNeverCrossesOrganisations(t *testing.T) {
	db := openDB(t)
	h := newAPIHarness(t, db)

	victim := newFixture(t, db)
	attacker := newFixture(t, db)

	token := h.tokenFor(t, attacker, rbac.RoleAdmin)
	resp := h.do(t, http.MethodGet, "/api/v1/runs?limit=100", token, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("listing runs failed: %d", resp.StatusCode)
	}
	if strings.Contains(bodyOf(t, resp), victim.RunID.String()) {
		t.Fatal("another organisation's run appeared in the list")
	}
}

// Filtering by a foreign project id must return nothing rather than that project's runs.
func TestListRunsFilteredByForeignProjectReturnsNothing(t *testing.T) {
	db := openDB(t)
	h := newAPIHarness(t, db)

	victim := newFixture(t, db)
	attacker := newFixture(t, db)

	token := h.tokenFor(t, attacker, rbac.RoleAdmin)
	resp := h.do(t, http.MethodGet,
		"/api/v1/runs?project_id="+victim.ProjectID.String(), token, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected status %d", resp.StatusCode)
	}
	if strings.Contains(bodyOf(t, resp), victim.RunID.String()) {
		t.Fatal("filtering by a foreign project id disclosed its runs")
	}
}

// --- role enforcement ----------------------------------------------------------

func TestViewerCannotRollBack(t *testing.T) {
	db := openDB(t)
	h := newAPIHarness(t, db)

	f := newFixture(t, db)
	envID := f.environment(t, db, "production")

	token := h.tokenFor(t, f, rbac.RoleViewer)
	resp := h.do(t, http.MethodPost,
		"/api/v1/environments/"+envID.String()+"/rollback", token,
		map[string]any{"reason": "should be refused"})

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a viewer must be refused rollback with 403, got %d", resp.StatusCode)
	}
}

func TestViewerCannotTriggerPipeline(t *testing.T) {
	db := openDB(t)
	h := newAPIHarness(t, db)
	f := newFixture(t, db)

	token := h.tokenFor(t, f, rbac.RoleViewer)
	resp := h.do(t, http.MethodPost,
		"/api/v1/projects/"+f.ProjectID.String()+"/runs", token, map[string]any{"branch": "main"})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a viewer must be refused pipeline:run with 403, got %d", resp.StatusCode)
	}
}

func TestViewerCannotCreateProject(t *testing.T) {
	db := openDB(t)
	h := newAPIHarness(t, db)
	f := newFixture(t, db)

	token := h.tokenFor(t, f, rbac.RoleViewer)
	resp := h.do(t, http.MethodPost, "/api/v1/projects", token,
		map[string]any{"name": "Refused"})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a viewer must be refused project:create with 403, got %d", resp.StatusCode)
	}
}

func TestViewerCannotReadAuditTrail(t *testing.T) {
	db := openDB(t)
	h := newAPIHarness(t, db)
	f := newFixture(t, db)

	token := h.tokenFor(t, f, rbac.RoleViewer)
	resp := h.do(t, http.MethodGet, "/api/v1/audit", token, nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a viewer must be refused audit:read with 403, got %d", resp.StatusCode)
	}
}

func TestUnauthenticatedRequestsAreRejected(t *testing.T) {
	db := openDB(t)
	h := newAPIHarness(t, db)

	for _, path := range []string{
		"/api/v1/projects", "/api/v1/runs", "/api/v1/deployments",
		"/api/v1/workers", "/api/v1/audit", "/api/v1/overview",
	} {
		resp := h.do(t, http.MethodGet, path, "", nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s without a credential must be 401, got %d", path, resp.StatusCode)
		}
	}
}

func TestForgedTokenIsRejected(t *testing.T) {
	db := openDB(t)
	h := newAPIHarness(t, db)

	// A structurally plausible JWT signed with the wrong key.
	forged := "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9." +
		"eyJzdWIiOiJ1c3JfMDAwMDAwMDAwMDAwMDAwMDAwMDAwMCIsInJvbGUiOiJBRE1JTiJ9." +
		"ZmFrZXNpZ25hdHVyZQ"
	resp := h.do(t, http.MethodGet, "/api/v1/overview", forged, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a forged token must be 401, got %d", resp.StatusCode)
	}
}

// --- field tampering / mass assignment ------------------------------------------

// The decoder rejects unknown fields, so an attempt to set a server-managed column is a
// 400 naming the field rather than a silently accepted privilege change.
func TestServerManagedFieldsCannotBeSetOnCreate(t *testing.T) {
	db := openDB(t)
	h := newAPIHarness(t, db)
	f := newFixture(t, db)
	token := h.tokenFor(t, f, rbac.RoleAdmin)

	for _, field := range []string{
		"id", "organisation_id", "created_by", "created_at", "archived_at",
	} {
		body := map[string]any{"name": "Tamper " + field, field: "tampered"}
		resp := h.do(t, http.MethodPost, "/api/v1/projects", token, body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("supplying %q must be rejected with 400, got %d: %s",
				field, resp.StatusCode, bodyOf(t, resp))
		}
	}
}

// A project created legitimately must belong to the caller's organisation, whatever the
// request said.
func TestCreatedProjectBelongsToCallersOrganisation(t *testing.T) {
	db := openDB(t)
	h := newAPIHarness(t, db)
	owner := newFixture(t, db)
	other := newFixture(t, db)

	token := h.tokenFor(t, owner, rbac.RoleAdmin)
	resp := h.do(t, http.MethodPost, "/api/v1/projects", token,
		map[string]any{"name": "Scoped Project", "slug": "scoped-" + strings.ToLower(string(owner.OrgID)[len(owner.OrgID)-8:])})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create project failed: %d %s", resp.StatusCode, bodyOf(t, resp))
	}

	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(bodyOf(t, resp)), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}

	var orgID string
	if err := db.QueryRow(context.Background(),
		`SELECT organisation_id FROM projects WHERE id=$1`, created.ID).Scan(&orgID); err != nil {
		t.Fatalf("read project: %v", err)
	}
	if orgID != owner.OrgID.String() {
		t.Fatalf("project landed in organisation %s, expected %s", orgID, owner.OrgID)
	}
	if orgID == other.OrgID.String() {
		t.Fatal("project was created in an unrelated organisation")
	}
}

// --- input validation -----------------------------------------------------------

func TestMalformedIdentifiersAreRejected(t *testing.T) {
	db := openDB(t)
	h := newAPIHarness(t, db)
	f := newFixture(t, db)
	token := h.tokenFor(t, f, rbac.RoleAdmin)

	// A wrong-prefix id, an injection attempt and an oversized value must all be
	// refused by parsing rather than reaching the database.
	for _, id := range []string{
		"not-an-id",
		"prj_' OR 1=1--",
		"run_" + strings.Repeat("A", 500),
		"../../etc/passwd",
	} {
		resp := h.do(t, http.MethodGet, "/api/v1/runs/"+id, token, nil)
		if resp.StatusCode != http.StatusBadRequest && resp.StatusCode != http.StatusNotFound {
			t.Fatalf("id %q must be refused, got %d", id, resp.StatusCode)
		}
	}
}

func TestOversizedBodyIsRejected(t *testing.T) {
	db := openDB(t)
	h := newAPIHarness(t, db)
	f := newFixture(t, db)
	token := h.tokenFor(t, f, rbac.RoleAdmin)

	// Comfortably beyond the 1 MiB decoder limit.
	huge := map[string]any{"name": strings.Repeat("A", 2<<20)}
	resp := h.do(t, http.MethodPost, "/api/v1/projects", token, huge)
	if resp.StatusCode != http.StatusBadRequest && resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("an oversized body must be refused, got %d", resp.StatusCode)
	}
}

func TestPaginationLimitIsBounded(t *testing.T) {
	db := openDB(t)
	h := newAPIHarness(t, db)
	f := newFixture(t, db)
	token := h.tokenFor(t, f, rbac.RoleAdmin)

	// A caller asking for a million rows must be clamped, not obeyed.
	for _, q := range []string{"limit=1000000", "limit=-1", "limit=abc", "limit=0"} {
		resp := h.do(t, http.MethodGet, "/api/v1/runs?"+q, token, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s must be handled, got %d", q, resp.StatusCode)
		}
		var page struct {
			Data []json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal([]byte(bodyOf(t, resp)), &page); err != nil {
			t.Fatalf("decode page: %v", err)
		}
		if len(page.Data) > 100 {
			t.Fatalf("%s returned %d rows, above the maximum page size", q, len(page.Data))
		}
	}
}

func TestAPITokenLifetimeIsBounded(t *testing.T) {
	db := openDB(t)
	h := newAPIHarness(t, db)
	f := newFixture(t, db)
	token := h.tokenFor(t, f, rbac.RoleAdmin)

	// A token that never meaningfully expires is a credential that outlives the person
	// who made it. Both extremes must be refused.
	for _, days := range []int{0, -1, 100000} {
		resp := h.do(t, http.MethodPost, "/api/v1/auth/tokens", token,
			map[string]any{"name": "bounded", "days": days})
		if resp.StatusCode != http.StatusBadRequest && resp.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("days=%d must be refused, got %d: %s", days, resp.StatusCode, bodyOf(t, resp))
		}
	}
}

// --- error hygiene ---------------------------------------------------------------

// An error must not hand the caller the internals: no SQL, no driver text, no paths.
func TestErrorsDoNotLeakInternals(t *testing.T) {
	db := openDB(t)
	h := newAPIHarness(t, db)
	f := newFixture(t, db)
	token := h.tokenFor(t, f, rbac.RoleAdmin)

	resp := h.do(t, http.MethodGet, "/api/v1/runs/run_doesnotexist000000000000", token, nil)
	body := strings.ToLower(bodyOf(t, resp))

	for _, leak := range []string{
		"select ", "insert ", "pgx", "postgres://", "sqlstate",
		"goroutine", ".go:", "c:\\users", "/home/",
	} {
		if strings.Contains(body, leak) {
			t.Fatalf("error response leaked %q: %s", leak, body)
		}
	}
}

// --- webhook security -------------------------------------------------------------

func TestWebhookRejectsMissingSignature(t *testing.T) {
	db := openDB(t)
	h := newAPIHarness(t, db)

	req, err := http.NewRequest(http.MethodPost, h.srv.URL+"/api/v1/webhooks/github",
		strings.NewReader(`{"ref":"refs/heads/main"}`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-GitHub-Delivery", domain.NewID(domain.PrefixEvent).String())
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("an unsigned delivery must be 401, got %d", resp.StatusCode)
	}
}

// A delivery missing the headers that identify it is a client error, not a server
// error: answering 500 both misreports whose fault it is and tells GitHub to retry
// something that can never succeed.
func TestWebhookRejectsMissingHeadersAsClientError(t *testing.T) {
	db := openDB(t)
	h := newAPIHarness(t, db)

	req, err := http.NewRequest(http.MethodPost, h.srv.URL+"/api/v1/webhooks/github",
		strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a delivery without identifying headers must be 400, got %d", resp.StatusCode)
	}
}

func TestWebhookRejectsWrongSignature(t *testing.T) {
	db := openDB(t)
	h := newAPIHarness(t, db)

	payload := `{"ref":"refs/heads/main"}`
	req, err := http.NewRequest(http.MethodPost, h.srv.URL+"/api/v1/webhooks/github",
		strings.NewReader(payload))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-GitHub-Delivery", domain.NewID(domain.PrefixEvent).String())
	req.Header.Set("X-Hub-Signature-256", "sha256="+strings.Repeat("0", 64))
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a delivery signed with the wrong key must be 401, got %d", resp.StatusCode)
	}
}

// --- response hygiene ---------------------------------------------------------------

// Nothing that identifies a stored credential may travel to a client.
func TestNoCredentialMaterialInResponses(t *testing.T) {
	db := openDB(t)
	h := newAPIHarness(t, db)
	f := newFixture(t, db)
	token := h.tokenFor(t, f, rbac.RoleAdmin)

	for _, path := range []string{"/api/v1/overview", "/api/v1/auth/me", "/api/v1/workers"} {
		resp := h.do(t, http.MethodGet, path, token, nil)
		body := strings.ToLower(bodyOf(t, resp))
		for _, forbidden := range []string{
			"password_hash", "$argon2", "token_hash", "jwt_secret", "webhook_secret",
		} {
			if strings.Contains(body, forbidden) {
				t.Fatalf("%s disclosed %q", path, forbidden)
			}
		}
	}
}

// Secure headers must be present on every response, including error responses.
func TestSecurityHeadersArePresent(t *testing.T) {
	db := openDB(t)
	h := newAPIHarness(t, db)

	resp := h.do(t, http.MethodGet, "/api/v1/overview", "", nil)
	for header, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "no-referrer",
	} {
		if got := resp.Header.Get(header); got != want {
			t.Fatalf("%s = %q, want %q", header, got, want)
		}
	}
}

// CORS must not reflect an arbitrary origin back with credentials enabled.
func TestCORSDoesNotReflectUnknownOrigins(t *testing.T) {
	db := openDB(t)
	h := newAPIHarness(t, db)

	req, err := http.NewRequest(http.MethodGet, h.srv.URL+"/api/v1/overview", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Origin", "https://evil.example")
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("an unlisted origin was allowed: %q", got)
	}
}

// --- fixture helpers ------------------------------------------------------------

// environment adds an environment to a fixture's project.
func (f fixture) environment(t *testing.T, db *database.DB, kind string) domain.ID {
	t.Helper()
	id := domain.NewID(domain.PrefixEnvironment)
	namespace := fmt.Sprintf("ns-%s", strings.ToLower(string(id)[len(id)-10:]))
	mustExec(t, db, `INSERT INTO environments (id, project_id, name, kind, namespace)
	                 VALUES ($1,$2,$3,$4,$5)`,
		id, f.ProjectID, strings.ToLower(kind), strings.ToUpper(kind), namespace)
	return id
}

// deployment adds a completed deployment, with the release and artifact it points at.
func (f fixture) deployment(t *testing.T, db *database.DB, envID domain.ID) domain.ID {
	t.Helper()
	artifactID := domain.NewID(domain.PrefixArtifact)
	releaseID := domain.NewID(domain.PrefixRelease)
	depID := domain.NewID(domain.PrefixDeployment)

	mustExec(t, db, `INSERT INTO artifacts (id, project_id, run_id, kind, reference, digest)
	                 VALUES ($1,$2,$3,'CONTAINER_IMAGE','demo:test','sha256:test')`,
		artifactID, f.ProjectID, f.RunID)
	mustExec(t, db, `INSERT INTO releases (id, project_id, artifact_id, version, commit_sha)
	                 VALUES ($1,$2,$3,'r1','abc1234')`,
		releaseID, f.ProjectID, artifactID)
	mustExec(t, db, `INSERT INTO deployment_counters (project_id, next_number) VALUES ($1,2)
	                 ON CONFLICT (project_id) DO NOTHING`, f.ProjectID)
	mustExec(t, db, `INSERT INTO deployments
	    (id, project_id, environment_id, release_id, number, state, strategy,
	     desired_revision, actual_revision, sync_status, health_status, requested_by)
	    VALUES ($1,$2,$3,$4,1,'HEALTHY','ROLLING','r1@abc1234','r1@abc1234','SYNCED','HEALTHY',$5)`,
		depID, f.ProjectID, envID, releaseID, f.UserID)
	return depID
}
