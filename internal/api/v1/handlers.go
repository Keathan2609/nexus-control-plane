package v1

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/Keathan2609/nexus/internal/api/middleware"
	"github.com/Keathan2609/nexus/internal/audit"
	"github.com/Keathan2609/nexus/internal/auth"
	"github.com/Keathan2609/nexus/internal/core/domain"
	"github.com/Keathan2609/nexus/internal/deployments"
	"github.com/Keathan2609/nexus/internal/pipelines"
	"github.com/Keathan2609/nexus/internal/platform/httpx"
	"github.com/Keathan2609/nexus/internal/rbac"
	"github.com/Keathan2609/nexus/internal/store/postgres"
	"github.com/Keathan2609/nexus/internal/webhooks"
)

// maxTokenDays caps an API token's lifetime at roughly a year. Long enough to be
// practical for a CI credential, short enough that every token is rotated eventually.
const maxTokenDays = 365

// --- auth ---------------------------------------------------------------------

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) error {
	var body loginRequest
	if err := httpx.DecodeJSON(r, &body); err != nil {
		return err
	}
	if body.Email == "" || body.Password == "" {
		return httpx.Validation("email and password are required",
			httpx.FieldError{Field: "email", Message: "required"},
			httpx.FieldError{Field: "password", Message: "required"})
	}

	token, principal, err := s.auth.Login(r.Context(), body.Email, body.Password)
	if err != nil {
		if errors.Is(err, auth.ErrInvalidCredentials) || errors.Is(err, auth.ErrInactiveUser) {
			// Both cases return the same message so the response does not reveal
			// whether an address is registered.
			return httpx.Unauthorised("invalid email or password")
		}
		return httpx.Internal(err)
	}

	_ = s.audit.Record(r.Context(), audit.Params{
		Principal: principal, Action: audit.UserLoggedIn,
		ResourceType: "user", ResourceID: principal.UserID.String(),
		CorrelationID: middleware.CorrelationIDFrom(r.Context()),
	})

	httpx.JSON(w, http.StatusOK, map[string]any{
		"token":       token,
		"expires_in":  int(s.cfg.TokenTTL.Seconds()),
		"user":        userPayload(principal),
		"permissions": rbac.Permissions(principal.Role),
	})
	return nil
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) error {
	p, err := principal(r)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, map[string]any{
		"user":        userPayload(p),
		"permissions": rbac.Permissions(p.Role),
	})
	return nil
}

func userPayload(p *auth.Principal) map[string]any {
	return map[string]any{
		"id":              p.UserID,
		"email":           p.Email,
		"name":            p.Name,
		"role":            p.Role,
		"organisation_id": p.OrganisationID,
	}
}

func (s *Server) handleCreateToken(w http.ResponseWriter, r *http.Request) error {
	p, err := principal(r)
	if err != nil {
		return err
	}
	var body struct {
		Name string `json:"name"`
		Days int    `json:"days"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		return err
	}
	if strings.TrimSpace(body.Name) == "" {
		return httpx.Validation("a token name is required",
			httpx.FieldError{Field: "name", Message: "required"})
	}
	if len(body.Name) > 128 {
		return httpx.Validation("the token could not be issued",
			httpx.FieldError{Field: "name", Message: "must be at most 128 characters"})
	}
	// A lifetime is mandatory and bounded. Zero previously produced a zero TTL, and a
	// credential that never meaningfully expires is one that outlives the person who
	// created it and the reason it was created.
	if body.Days < 1 || body.Days > maxTokenDays {
		return httpx.Validation("the token could not be issued",
			httpx.FieldError{Field: "days",
				Message: "must be between 1 and " + strconv.Itoa(maxTokenDays)})
	}
	ttl := time.Duration(body.Days) * 24 * time.Hour

	token, err := s.auth.IssueAPIToken(r.Context(), p.UserID, body.Name, ttl)
	if err != nil {
		return httpx.Internal(err)
	}
	_ = s.audit.Record(r.Context(), audit.Params{
		Principal: p, Action: audit.TokenIssued,
		ResourceType: "api_token", ResourceID: body.Name,
		CorrelationID: middleware.CorrelationIDFrom(r.Context()),
	})

	// The plaintext is returned once and never again; the response says so, because a
	// client that assumes it can re-read it will build a broken flow.
	httpx.JSON(w, http.StatusCreated, map[string]any{
		"token": token,
		"name":  body.Name,
		"note":  "store this now; it cannot be retrieved again",
	})
	return nil
}

// --- overview -----------------------------------------------------------------

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) error {
	p, err := principal(r)
	if err != nil {
		return err
	}
	overview, err := s.read.Overview(r.Context(), p.OrganisationID)
	if err != nil {
		return httpx.Internal(err)
	}
	recentRuns, err := s.read.ListRuns(r.Context(), p.OrganisationID, "", "", 8, "")
	if err != nil {
		return httpx.Internal(err)
	}
	recentDeploys, err := s.read.ListDeployments(r.Context(), p.OrganisationID, "", "", 6, "")
	if err != nil {
		return httpx.Internal(err)
	}
	workers, err := s.read.ListWorkers(r.Context())
	if err != nil {
		return httpx.Internal(err)
	}
	environments, err := s.read.ListEnvironments(r.Context(), p.OrganisationID, "")
	if err != nil {
		return httpx.Internal(err)
	}

	httpx.JSON(w, http.StatusOK, map[string]any{
		"metrics":            overview,
		"recent_runs":        recentRuns,
		"recent_deployments": recentDeploys,
		"workers":            workers,
		"environments":       environments,
		// Reported rather than assumed: the configured provider and the one actually in
		// use differ whenever a cluster was requested and could not be reached.
		"deploy_provider": s.provider,
	})
	return nil
}

// --- projects -----------------------------------------------------------------

func (s *Server) handleListProjects(w http.ResponseWriter, r *http.Request) error {
	p, err := principal(r)
	if err != nil {
		return err
	}
	limit := httpx.Limit(r, 25, 100)
	// Fetch one more than the page size to know whether another page exists without a
	// second COUNT query.
	items, err := s.read.ListProjects(r.Context(), p.OrganisationID, limit+1, httpx.Cursor(r))
	if err != nil {
		return httpx.Internal(err)
	}
	httpx.JSON(w, http.StatusOK, httpx.NewPage(items, limit,
		func(x postgres.ProjectSummary) string { return x.ID.String() }))
	return nil
}

func (s *Server) handleGetProject(w http.ResponseWriter, r *http.Request) error {
	p, err := principal(r)
	if err != nil {
		return err
	}
	id, err := domain.ParseID(domain.PrefixProject, chi.URLParam(r, "projectID"))
	if err != nil {
		return httpx.BadRequest("malformed project id")
	}

	project, err := s.read.GetProject(r.Context(), p.OrganisationID, id)
	if err != nil {
		return notFoundOr(err, "project")
	}
	environments, err := s.read.ListEnvironments(r.Context(), p.OrganisationID, id)
	if err != nil {
		return httpx.Internal(err)
	}
	runs, err := s.read.ListRuns(r.Context(), p.OrganisationID, id, "", 10, "")
	if err != nil {
		return httpx.Internal(err)
	}
	deploys, err := s.read.ListDeployments(r.Context(), p.OrganisationID, id, "", 10, "")
	if err != nil {
		return httpx.Internal(err)
	}

	httpx.JSON(w, http.StatusOK, map[string]any{
		"project":            project,
		"environments":       environments,
		"recent_runs":        runs,
		"recent_deployments": deploys,
	})
	return nil
}

type createProjectRequest struct {
	Name          string `json:"name"`
	Slug          string `json:"slug"`
	Description   string `json:"description"`
	RepositoryURL string `json:"repository_url"`
	DefaultBranch string `json:"default_branch"`
	Runtime       string `json:"runtime"`
}

func (s *Server) handleCreateProject(w http.ResponseWriter, r *http.Request) error {
	p, err := principal(r)
	if err != nil {
		return err
	}
	var body createProjectRequest
	if err := httpx.DecodeJSON(r, &body); err != nil {
		return err
	}

	var fields []httpx.FieldError
	if strings.TrimSpace(body.Name) == "" {
		fields = append(fields, httpx.FieldError{Field: "name", Message: "required"})
	}
	if body.Slug == "" {
		body.Slug = slugify(body.Name)
	}
	if !validSlug(body.Slug) {
		fields = append(fields, httpx.FieldError{Field: "slug",
			Message: "must be lowercase alphanumeric with hyphens"})
	}
	if len(fields) > 0 {
		return httpx.Validation("the project could not be created", fields...)
	}
	if body.DefaultBranch == "" {
		body.DefaultBranch = "main"
	}
	runtime := strings.ToUpper(body.Runtime)
	switch runtime {
	case "NODE", "GO", "PYTHON", "UNKNOWN", "":
		if runtime == "" {
			runtime = "UNKNOWN"
		}
	default:
		return httpx.Validation("unsupported runtime",
			httpx.FieldError{Field: "runtime", Message: "must be NODE, GO or PYTHON"})
	}

	tx, err := s.db.Begin(r.Context())
	if err != nil {
		return httpx.Internal(err)
	}
	defer func() { _ = tx.Rollback(r.Context()) }()

	projectID := domain.NewID(domain.PrefixProject)
	if _, err := tx.Exec(r.Context(),
		`INSERT INTO projects (id, organisation_id, name, slug, description, runtime, created_by)
		 VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		projectID, p.OrganisationID, body.Name, body.Slug, body.Description, runtime, p.UserID); err != nil {
		if strings.Contains(err.Error(), "projects_org_slug_key") {
			return httpx.Conflict("a project with that slug already exists")
		}
		return httpx.Internal(err)
	}
	if _, err := tx.Exec(r.Context(),
		`INSERT INTO run_counters (project_id, next_number) VALUES ($1,1)`, projectID); err != nil {
		return httpx.Internal(err)
	}
	if _, err := tx.Exec(r.Context(),
		`INSERT INTO deployment_counters (project_id, next_number) VALUES ($1,1)`, projectID); err != nil {
		return httpx.Internal(err)
	}

	if body.RepositoryURL != "" {
		owner, name, ok := parseRepositoryURL(body.RepositoryURL)
		if !ok {
			return httpx.Validation("repository_url must be a GitHub URL",
				httpx.FieldError{Field: "repository_url", Message: "expected github.com/owner/name"})
		}
		if _, err := tx.Exec(r.Context(),
			`INSERT INTO repositories (id, project_id, provider, owner, name, clone_url, default_branch)
			 VALUES ($1,$2,'GITHUB',$3,$4,$5,$6)`,
			domain.NewID(domain.PrefixRepository), projectID, owner, name,
			body.RepositoryURL, body.DefaultBranch); err != nil {
			return httpx.Internal(err)
		}
	}

	// The audit record shares the transaction with the change it describes, so the two
	// cannot disagree.
	if err := audit.RecordTx(r.Context(), tx, audit.Params{
		Principal: p, Action: audit.ProjectCreated,
		ResourceType: "project", ResourceID: projectID.String(), ProjectID: projectID,
		CorrelationID: middleware.CorrelationIDFrom(r.Context()),
		Metadata:      map[string]any{"name": body.Name, "slug": body.Slug},
	}); err != nil {
		return httpx.Internal(err)
	}

	if err := tx.Commit(r.Context()); err != nil {
		return httpx.Internal(err)
	}

	httpx.JSON(w, http.StatusCreated, map[string]any{
		"id": projectID, "name": body.Name, "slug": body.Slug, "runtime": runtime,
	})
	return nil
}

// --- runs ---------------------------------------------------------------------

func (s *Server) handleListRuns(w http.ResponseWriter, r *http.Request) error {
	p, err := principal(r)
	if err != nil {
		return err
	}
	limit := httpx.Limit(r, 25, 100)
	var projectID domain.ID
	if raw := r.URL.Query().Get("project_id"); raw != "" {
		id, err := domain.ParseID(domain.PrefixProject, raw)
		if err != nil {
			return httpx.BadRequest("malformed project_id")
		}
		projectID = id
	}
	state := strings.ToUpper(r.URL.Query().Get("state"))

	items, err := s.read.ListRuns(r.Context(), p.OrganisationID, projectID, state, limit+1, httpx.Cursor(r))
	if err != nil {
		return httpx.Internal(err)
	}
	httpx.JSON(w, http.StatusOK, httpx.NewPage(items, limit,
		func(x postgres.RunSummary) string { return x.ID.String() }))
	return nil
}

func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) error {
	p, err := principal(r)
	if err != nil {
		return err
	}
	id, err := domain.ParseID(domain.PrefixRun, chi.URLParam(r, "runID"))
	if err != nil {
		return httpx.BadRequest("malformed run id")
	}
	run, err := s.read.GetRun(r.Context(), p.OrganisationID, id)
	if err != nil {
		return notFoundOr(err, "run")
	}
	jobs, err := s.read.ListJobs(r.Context(), p.OrganisationID, id)
	if err != nil {
		return notFoundOr(err, "run")
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"run": run, "jobs": jobs})
	return nil
}

func (s *Server) handleRunJobs(w http.ResponseWriter, r *http.Request) error {
	p, err := principal(r)
	if err != nil {
		return err
	}
	id, err := domain.ParseID(domain.PrefixRun, chi.URLParam(r, "runID"))
	if err != nil {
		return httpx.BadRequest("malformed run id")
	}
	jobs, err := s.read.ListJobs(r.Context(), p.OrganisationID, id)
	if err != nil {
		return notFoundOr(err, "run")
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"data": jobs})
	return nil
}

func (s *Server) handleJobLogs(w http.ResponseWriter, r *http.Request) error {
	p, err := principal(r)
	if err != nil {
		return err
	}
	id, err := domain.ParseID(domain.PrefixJob, chi.URLParam(r, "jobID"))
	if err != nil {
		return httpx.BadRequest("malformed job id")
	}
	attempt, err := attemptParam(r)
	if err != nil {
		return err
	}
	logs, err := s.read.JobLogs(r.Context(), p.OrganisationID, id, attempt, httpx.Limit(r, 500, 5000))
	if err != nil {
		return notFoundOr(err, "job")
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"data": logs})
	return nil
}

// attemptParam reads the optional attempt filter. Zero means every attempt; a negative
// value is a client mistake rather than a filter, and is rejected instead of quietly
// matching nothing.
func attemptParam(r *http.Request) (int, error) {
	raw := r.URL.Query().Get("attempt")
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0, httpx.BadRequest("attempt must be a non-negative integer")
	}
	return n, nil
}

type triggerRunRequest struct {
	Branch    string `json:"branch"`
	CommitSHA string `json:"commit_sha"`
	Message   string `json:"message"`
}

// validate bounds what a caller may put into a run.
//
// These values are not inert: the branch and commit reach a worker that checks out a
// repository, and all three are persisted, rendered in the console and used to derive an
// image tag. Length limits and a character allowlist keep a shell-shaped or
// absurdly-long value from ever getting that far.
func (t triggerRunRequest) validate() error {
	var fields []httpx.FieldError

	if len(t.Branch) > 255 {
		fields = append(fields, httpx.FieldError{Field: "branch", Message: "must be at most 255 characters"})
	} else if t.Branch != "" && !validRef(t.Branch) {
		fields = append(fields, httpx.FieldError{Field: "branch", Message: "contains characters that are not valid in a git ref"})
	}

	if len(t.CommitSHA) > 64 {
		fields = append(fields, httpx.FieldError{Field: "commit_sha", Message: "must be at most 64 characters"})
	} else if t.CommitSHA != "" && !isHex(t.CommitSHA) {
		fields = append(fields, httpx.FieldError{Field: "commit_sha", Message: "must be hexadecimal"})
	}

	if len(t.Message) > 4096 {
		fields = append(fields, httpx.FieldError{Field: "message", Message: "must be at most 4096 characters"})
	}

	if len(fields) > 0 {
		return httpx.Validation("the run could not be triggered", fields...)
	}
	return nil
}

// validRef accepts the characters git permits in a branch name and nothing else. It is
// deliberately stricter than git itself; a legal but hostile ref is not worth supporting.
func validRef(s string) bool {
	if strings.HasPrefix(s, "-") || strings.HasPrefix(s, "/") ||
		strings.Contains(s, "..") || strings.HasSuffix(s, "/") {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '/', r == '.':
		default:
			return false
		}
	}
	return true
}

func isHex(s string) bool {
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
}

func (s *Server) handleTriggerRun(w http.ResponseWriter, r *http.Request) error {
	p, err := principal(r)
	if err != nil {
		return err
	}
	projectID, err := domain.ParseID(domain.PrefixProject, chi.URLParam(r, "projectID"))
	if err != nil {
		return httpx.BadRequest("malformed project id")
	}
	// Ownership before anything else. Without this a valid project id from another
	// organisation started a real pipeline run in that organisation's project.
	if err := s.read.AssertProjectVisible(r.Context(), p.OrganisationID, projectID); err != nil {
		return httpx.NotFound("project")
	}

	var body triggerRunRequest
	if r.ContentLength > 0 {
		if err := httpx.DecodeJSON(r, &body); err != nil {
			return err
		}
	}
	if err := body.validate(); err != nil {
		return err
	}

	definitionID, definition, err := s.loadDefinition(r.Context(), projectID)
	if err != nil {
		return err
	}
	if body.Branch == "" {
		body.Branch = "main"
	}
	if body.CommitSHA == "" {
		body.CommitSHA = "manual" + time.Now().UTC().Format("0102150405")
	}

	run, err := s.runs.CreateRun(r.Context(), postgres.CreateRunParams{
		ProjectID: projectID, DefinitionID: definitionID, Definition: definition,
		Trigger: "MANUAL", TriggeredBy: p.UserID,
		CommitSHA: body.CommitSHA, CommitMessage: body.Message,
		CommitAuthor: p.Email, Branch: body.Branch,
		CorrelationID: middleware.CorrelationIDFrom(r.Context()),
	})
	if err != nil {
		return httpx.Internal(err)
	}

	_ = s.audit.Record(r.Context(), audit.Params{
		Principal: p, Action: audit.PipelineTriggered,
		ResourceType: "pipeline_run", ResourceID: run.ID.String(), ProjectID: projectID,
		CorrelationID: middleware.CorrelationIDFrom(r.Context()),
		Metadata:      map[string]any{"number": run.Number, "branch": body.Branch},
	})

	httpx.JSON(w, http.StatusAccepted, map[string]any{
		"id": run.ID, "number": run.Number, "state": run.State,
	})
	return nil
}

func (s *Server) handleCancelRun(w http.ResponseWriter, r *http.Request) error {
	p, err := principal(r)
	if err != nil {
		return err
	}
	id, err := domain.ParseID(domain.PrefixRun, chi.URLParam(r, "runID"))
	if err != nil {
		return httpx.BadRequest("malformed run id")
	}
	run, err := s.read.GetRun(r.Context(), p.OrganisationID, id)
	if err != nil {
		return notFoundOr(err, "run")
	}

	if err := s.sched.Cancel(r.Context(), id, "cancelled by "+p.Email); err != nil {
		var invalid *domain.InvalidTransitionError
		if errors.As(err, &invalid) {
			return httpx.InvalidState("this run has already finished")
		}
		return httpx.Internal(err)
	}

	_ = s.audit.Record(r.Context(), audit.Params{
		Principal: p, Action: audit.PipelineCancelled,
		ResourceType: "pipeline_run", ResourceID: id.String(), ProjectID: run.ProjectID,
		CorrelationID: middleware.CorrelationIDFrom(r.Context()),
	})
	httpx.JSON(w, http.StatusOK, map[string]any{"id": id, "state": "CANCELLED"})
	return nil
}

// --- environments and deployments ----------------------------------------------

func (s *Server) handleListEnvironments(w http.ResponseWriter, r *http.Request) error {
	p, err := principal(r)
	if err != nil {
		return err
	}
	var projectID domain.ID
	if raw := r.URL.Query().Get("project_id"); raw != "" {
		id, err := domain.ParseID(domain.PrefixProject, raw)
		if err != nil {
			return httpx.BadRequest("malformed project_id")
		}
		projectID = id
	}
	items, err := s.read.ListEnvironments(r.Context(), p.OrganisationID, projectID)
	if err != nil {
		return httpx.Internal(err)
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"data": items})
	return nil
}

func (s *Server) handleListDeployments(w http.ResponseWriter, r *http.Request) error {
	p, err := principal(r)
	if err != nil {
		return err
	}
	limit := httpx.Limit(r, 25, 100)
	var projectID, environmentID domain.ID
	if raw := r.URL.Query().Get("project_id"); raw != "" {
		id, err := domain.ParseID(domain.PrefixProject, raw)
		if err != nil {
			return httpx.BadRequest("malformed project_id")
		}
		projectID = id
	}
	if raw := r.URL.Query().Get("environment_id"); raw != "" {
		id, err := domain.ParseID(domain.PrefixEnvironment, raw)
		if err != nil {
			return httpx.BadRequest("malformed environment_id")
		}
		environmentID = id
	}

	items, err := s.read.ListDeployments(r.Context(), p.OrganisationID, projectID, environmentID, limit+1, httpx.Cursor(r))
	if err != nil {
		return httpx.Internal(err)
	}
	httpx.JSON(w, http.StatusOK, httpx.NewPage(items, limit,
		func(x postgres.DeploymentView) string { return x.ID.String() }))
	return nil
}

func (s *Server) handleGetDeployment(w http.ResponseWriter, r *http.Request) error {
	p, err := principal(r)
	if err != nil {
		return err
	}
	id, err := domain.ParseID(domain.PrefixDeployment, chi.URLParam(r, "deploymentID"))
	if err != nil {
		return httpx.BadRequest("malformed deployment id")
	}
	dep, err := s.read.GetDeployment(r.Context(), p.OrganisationID, id)
	if err != nil {
		return notFoundOr(err, "deployment")
	}
	checks, err := s.read.HealthChecks(r.Context(), id)
	if err != nil {
		return httpx.Internal(err)
	}
	httpx.JSON(w, http.StatusOK, map[string]any{
		"deployment":    dep,
		"health_checks": checks,
	})
	return nil
}

type deployRequest struct {
	ReleaseID string `json:"release_id"`
	Reason    string `json:"reason"`
}

func (s *Server) handleDeploy(w http.ResponseWriter, r *http.Request) error {
	p, err := principal(r)
	if err != nil {
		return err
	}
	environmentID, err := domain.ParseID(domain.PrefixEnvironment, chi.URLParam(r, "environmentID"))
	if err != nil {
		return httpx.BadRequest("malformed environment id")
	}
	var body deployRequest
	if err := httpx.DecodeJSON(r, &body); err != nil {
		return err
	}
	releaseID, err := domain.ParseID(domain.PrefixRelease, body.ReleaseID)
	if err != nil {
		return httpx.Validation("a release_id is required",
			httpx.FieldError{Field: "release_id", Message: "must be a release identifier"})
	}

	projectID, err := s.read.EnvironmentProject(r.Context(), p.OrganisationID, environmentID)
	if err != nil {
		return httpx.NotFound("environment")
	}

	dep, err := s.deploys.Request(r.Context(), deployments.RequestParams{
		ProjectID: projectID, EnvironmentID: environmentID, ReleaseID: releaseID,
		RequestedBy: p.UserID, Reason: body.Reason,
		CorrelationID: middleware.CorrelationIDFrom(r.Context()),
	})
	if err != nil {
		return httpx.Internal(err)
	}

	_ = s.audit.Record(r.Context(), audit.Params{
		Principal: p, Action: audit.DeploymentRequested,
		ResourceType: "deployment", ResourceID: dep.ID.String(), ProjectID: projectID,
		CorrelationID: middleware.CorrelationIDFrom(r.Context()),
		Metadata:      map[string]any{"release_id": releaseID.String(), "reason": body.Reason},
	})

	// Execution is deliberately asynchronous: applying and verifying takes minutes and
	// must not be tied to the lifetime of an HTTP request. The client follows progress
	// on the event stream.
	go s.executeDeployment(dep.ID)

	httpx.JSON(w, http.StatusAccepted, map[string]any{
		"id": dep.ID, "number": dep.Number, "state": dep.State,
	})
	return nil
}

func (s *Server) handleRollback(w http.ResponseWriter, r *http.Request) error {
	p, err := principal(r)
	if err != nil {
		return err
	}
	environmentID, err := domain.ParseID(domain.PrefixEnvironment, chi.URLParam(r, "environmentID"))
	if err != nil {
		return httpx.BadRequest("malformed environment id")
	}

	// The same ownership check the deploy path makes, through the same helper. When the
	// two were written separately this one was simply missing, and a valid environment
	// id was enough to roll back another organisation's production environment.
	if _, err := s.read.EnvironmentProject(r.Context(), p.OrganisationID, environmentID); err != nil {
		return httpx.NotFound("environment")
	}

	var body struct {
		Reason    string `json:"reason"`
		ToRelease string `json:"to_release"`
	}
	if r.ContentLength > 0 {
		if err := httpx.DecodeJSON(r, &body); err != nil {
			return err
		}
	}
	if len(body.Reason) > 1024 {
		return httpx.Validation("the rollback could not be requested",
			httpx.FieldError{Field: "reason", Message: "must be at most 1024 characters"})
	}
	if body.Reason == "" {
		body.Reason = "rollback requested by " + p.Email
	}

	var target domain.ID
	if body.ToRelease != "" {
		id, err := domain.ParseID(domain.PrefixRelease, body.ToRelease)
		if err != nil {
			return httpx.BadRequest("malformed to_release")
		}
		target = id
	}

	dep, err := s.deploys.Rollback(r.Context(), deployments.RollbackParams{
		EnvironmentID: environmentID, RequestedBy: p.UserID, Reason: body.Reason,
		ToRelease: target, CorrelationID: middleware.CorrelationIDFrom(r.Context()),
	})
	if err != nil {
		if errors.Is(err, deployments.ErrNoHealthyRelease) {
			return httpx.InvalidState("there is no earlier healthy release to roll back to")
		}
		if errors.Is(err, deployments.ErrVerificationFailed) {
			// The rollback ran and did not come up healthy. That is reported honestly
			// rather than as a success, because the environment is still broken.
			return &httpx.APIError{
				Status:  http.StatusConflict,
				Code:    httpx.CodeInvalidState,
				Message: "the rollback deployment failed health verification",
				Cause:   err,
			}
		}
		return httpx.Internal(err)
	}

	_ = s.audit.Record(r.Context(), audit.Params{
		Principal: p, Action: audit.RollbackRequested,
		ResourceType: "deployment", ResourceID: dep.ID.String(), ProjectID: dep.ProjectID,
		CorrelationID: middleware.CorrelationIDFrom(r.Context()),
		Metadata:      map[string]any{"reason": body.Reason},
	})

	httpx.JSON(w, http.StatusOK, map[string]any{
		"id": dep.ID, "state": dep.State, "release_id": dep.ReleaseID,
	})
	return nil
}

// executeDeployment runs the apply-and-verify cycle outside the request lifetime.
//
// context.Background rather than the request context: the client disconnecting must not
// abort a deployment that is already changing a live environment.
func (s *Server) executeDeployment(id domain.ID) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	if err := s.deploys.Execute(ctx, id); err != nil {
		s.log.Warn("deployment did not succeed", "deployment_id", id.String(), "error", err)
	}
}

// --- workers and audit ----------------------------------------------------------

func (s *Server) handleListWorkers(w http.ResponseWriter, r *http.Request) error {
	items, err := s.read.ListWorkers(r.Context())
	if err != nil {
		return httpx.Internal(err)
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"data": items})
	return nil
}

func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) error {
	p, err := principal(r)
	if err != nil {
		return err
	}
	limit := httpx.Limit(r, 50, 200)
	var projectID domain.ID
	if raw := r.URL.Query().Get("project_id"); raw != "" {
		id, err := domain.ParseID(domain.PrefixProject, raw)
		if err != nil {
			return httpx.BadRequest("malformed project_id")
		}
		projectID = id
	}

	entries, err := s.audit.List(r.Context(), audit.ListParams{
		OrganisationID: p.OrganisationID,
		ProjectID:      projectID,
		Action:         r.URL.Query().Get("action"),
		ResourceID:     r.URL.Query().Get("resource_id"),
		Limit:          limit + 1,
		Cursor:         httpx.Cursor(r),
	})
	if err != nil {
		return httpx.Internal(err)
	}
	httpx.JSON(w, http.StatusOK, httpx.NewPage(entries, limit,
		func(e audit.Entry) string { return e.ID.String() }))
	return nil
}

// --- helpers --------------------------------------------------------------------

// loadDefinition returns the newest pipeline definition for a project, falling back to
// the built-in default so a freshly imported project can be run immediately rather than
// requiring a definition to be authored first.
func (s *Server) loadDefinition(ctx context.Context, projectID domain.ID) (domain.ID, pipelines.Definition, error) {
	var (
		id   domain.ID
		spec []byte
	)
	err := s.db.QueryRow(ctx,
		`SELECT id, spec FROM pipeline_definitions
		  WHERE project_id = $1 ORDER BY version DESC LIMIT 1`, projectID).Scan(&id, &spec)
	if err != nil {
		return "", pipelines.Definition{}, httpx.NotFound("pipeline definition")
	}

	def, err := pipelines.ParseSpec(spec)
	if err != nil {
		return "", pipelines.Definition{}, httpx.Internal(err)
	}
	return id, def, nil
}

func slugify(name string) string {
	var b strings.Builder
	prevHyphen := false
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevHyphen = false
		case r == ' ' || r == '-' || r == '_':
			if !prevHyphen && b.Len() > 0 {
				b.WriteByte('-')
				prevHyphen = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

func validSlug(s string) bool {
	if s == "" || len(s) > 63 {
		return false
	}
	for i, r := range s {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || (r == '-' && i > 0)
		if !ok {
			return false
		}
	}
	return true
}

// parseRepositoryURL accepts the forms a person actually pastes.
func parseRepositoryURL(raw string) (owner, name string, ok bool) {
	s := strings.TrimSuffix(strings.TrimSpace(raw), ".git")
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "http://")
	s = strings.TrimPrefix(s, "git@")
	s = strings.ReplaceAll(s, "github.com:", "github.com/")
	if !strings.HasPrefix(s, "github.com/") {
		return "", "", false
	}
	parts := strings.Split(strings.TrimPrefix(s, "github.com/"), "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// --- webhooks -------------------------------------------------------------------

// handleGitHubWebhook accepts a repository delivery.
//
// The response is always fast and always 2xx for a delivery that was accepted, even when
// it triggered nothing. GitHub disables a webhook whose endpoint keeps erroring, so
// returning 500 because a repository is not tracked would eventually silence a hook that
// is working exactly as intended.
func (s *Server) handleGitHubWebhook(w http.ResponseWriter, r *http.Request) error {
	result, err := s.webhooks.Handle(r.Context(), r)
	if err != nil {
		switch {
		case errors.Is(err, webhooks.ErrInvalidSignature):
			return httpx.Unauthorised("signature verification failed")
		case errors.Is(err, webhooks.ErrMalformedDelivery):
			return httpx.BadRequest("the delivery is missing required headers")
		case errors.Is(err, webhooks.ErrUnknownRepo):
			httpx.JSON(w, http.StatusAccepted, map[string]any{"outcome": "IGNORED"})
			return nil
		default:
			return httpx.Internal(err)
		}
	}

	body := map[string]any{"outcome": result.Outcome}
	if result.RunID != "" {
		body["run_id"] = result.RunID
		body["run_number"] = result.RunNumber
	}
	httpx.JSON(w, http.StatusAccepted, body)
	return nil
}
