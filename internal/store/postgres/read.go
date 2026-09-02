package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Keathan2609/nexus/internal/core/domain"
	"github.com/Keathan2609/nexus/internal/platform/database"
)

// ReadStore holds the queries that serve the console and the CLI.
//
// Reads are separated from the command-side stores because they answer different
// questions and change for different reasons. A list endpoint wants a denormalised row
// shaped for a table; the scheduler wants the minimum needed to make a decision.
// Forcing both through one set of entity structs makes the read path do joins it does
// not need and the write path carry fields it never uses.
type ReadStore struct{ db *database.DB }

func NewReadStore(db *database.DB) *ReadStore { return &ReadStore{db: db} }

type ProjectSummary struct {
	ID          domain.ID `json:"id"`
	Name        string    `json:"name"`
	Slug        string    `json:"slug"`
	Description string    `json:"description"`
	Runtime     string    `json:"runtime"`
	Repository  string    `json:"repository,omitempty"`
	Branch      string    `json:"default_branch,omitempty"`
	CreatedAt   time.Time `json:"created_at"`

	// Denormalised delivery status, computed in one query rather than N+1 per project.
	Environments     int        `json:"environment_count"`
	LastRunNumber    *int64     `json:"last_run_number,omitempty"`
	LastRunState     *string    `json:"last_run_state,omitempty"`
	LastRunAt        *time.Time `json:"last_run_at,omitempty"`
	ProductionRev    *string    `json:"production_revision,omitempty"`
	ProductionHealth *string    `json:"production_health,omitempty"`
	SuccessRate      float64    `json:"success_rate"`
}

func (s *ReadStore) ListProjects(ctx context.Context, orgID domain.ID, limit int, cursor string) ([]ProjectSummary, error) {
	rows, err := s.db.Query(ctx,
		`SELECT p.id, p.name, p.slug, p.description, p.runtime, p.created_at,
		        COALESCE(r.owner || '/' || r.name, ''),
		        COALESCE(r.default_branch, ''),
		        (SELECT count(*) FROM environments e
		          WHERE e.project_id = p.id AND e.destroyed_at IS NULL),
		        lr.number, lr.state, lr.created_at,
		        pd.desired_revision, pd.health_status,
		        COALESCE((
		          SELECT round(
		                   100.0 * count(*) FILTER (WHERE state = 'SUCCEEDED')
		                   / NULLIF(count(*), 0), 1)
		            FROM (SELECT state FROM pipeline_runs
		                   WHERE project_id = p.id
		                     AND state IN ('SUCCEEDED','FAILED','TIMED_OUT')
		                   ORDER BY id DESC LIMIT 20) recent
		        ), 0)
		   FROM projects p
		   LEFT JOIN repositories r ON r.project_id = p.id
		   LEFT JOIN LATERAL (
		        SELECT number, state, created_at FROM pipeline_runs
		         WHERE project_id = p.id ORDER BY id DESC LIMIT 1
		   ) lr ON true
		   LEFT JOIN LATERAL (
		        SELECT d.desired_revision, d.health_status
		          FROM deployments d JOIN environments e ON e.id = d.environment_id
		         WHERE d.project_id = p.id AND e.kind = 'PRODUCTION'
		           AND d.state IN ('HEALTHY','DEGRADED')
		         ORDER BY d.id DESC LIMIT 1
		   ) pd ON true
		  WHERE p.organisation_id = $1
		    AND p.archived_at IS NULL
		    AND ($2 = '' OR p.id < $2)
		  ORDER BY p.id DESC
		  LIMIT $3`, orgID, cursor, limit)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	defer rows.Close()

	var out []ProjectSummary
	for rows.Next() {
		var p ProjectSummary
		if err := rows.Scan(&p.ID, &p.Name, &p.Slug, &p.Description, &p.Runtime, &p.CreatedAt,
			&p.Repository, &p.Branch, &p.Environments, &p.LastRunNumber, &p.LastRunState,
			&p.LastRunAt, &p.ProductionRev, &p.ProductionHealth, &p.SuccessRate); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *ReadStore) GetProject(ctx context.Context, orgID, id domain.ID) (*ProjectSummary, error) {
	list, err := s.db.Query(ctx,
		`SELECT p.id, p.name, p.slug, p.description, p.runtime, p.created_at,
		        COALESCE(r.owner || '/' || r.name, ''), COALESCE(r.default_branch, '')
		   FROM projects p LEFT JOIN repositories r ON r.project_id = p.id
		  WHERE p.organisation_id = $1 AND p.id = $2`, orgID, id)
	if err != nil {
		return nil, fmt.Errorf("get project: %w", err)
	}
	defer list.Close()
	if !list.Next() {
		return nil, ErrNotFound
	}
	var p ProjectSummary
	if err := list.Scan(&p.ID, &p.Name, &p.Slug, &p.Description, &p.Runtime,
		&p.CreatedAt, &p.Repository, &p.Branch); err != nil {
		return nil, err
	}
	return &p, nil
}

type RunSummary struct {
	ID            domain.ID  `json:"id"`
	ProjectID     domain.ID  `json:"project_id"`
	ProjectName   string     `json:"project_name,omitempty"`
	Number        int64      `json:"number"`
	State         string     `json:"state"`
	Trigger       string     `json:"trigger"`
	CommitSHA     string     `json:"commit_sha"`
	CommitMessage string     `json:"commit_message"`
	CommitAuthor  string     `json:"commit_author"`
	Branch        string     `json:"branch"`
	PullRequest   *int       `json:"pull_request,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	StartedAt     *time.Time `json:"started_at,omitempty"`
	FinishedAt    *time.Time `json:"finished_at,omitempty"`
	DurationMS    *int64     `json:"duration_ms,omitempty"`
	JobsTotal     int        `json:"jobs_total"`
	JobsDone      int        `json:"jobs_done"`
}

func (s *ReadStore) ListRuns(ctx context.Context, orgID domain.ID, projectID domain.ID, state string, limit int, cursor string) ([]RunSummary, error) {
	rows, err := s.db.Query(ctx,
		`SELECT r.id, r.project_id, p.name, r.number, r.state, r.trigger,
		        r.commit_sha, r.commit_message, r.commit_author, r.branch,
		        r.pull_request_number, r.created_at, r.started_at, r.finished_at,
		        (SELECT count(*) FROM jobs j WHERE j.run_id = r.id),
		        (SELECT count(*) FROM jobs j WHERE j.run_id = r.id
		          AND j.state IN ('SUCCEEDED','FAILED','CANCELLED','SKIPPED','TIMED_OUT'))
		   FROM pipeline_runs r JOIN projects p ON p.id = r.project_id
		  WHERE p.organisation_id = $1
		    AND ($2 = '' OR r.project_id = $2)
		    AND ($3 = '' OR r.state = $3)
		    AND ($4 = '' OR r.id < $4)
		  ORDER BY r.id DESC
		  LIMIT $5`, orgID, projectID, state, cursor, limit)
	if err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}
	defer rows.Close()
	return scanRuns(rows)
}

func (s *ReadStore) GetRun(ctx context.Context, orgID, id domain.ID) (*RunSummary, error) {
	rows, err := s.db.Query(ctx,
		`SELECT r.id, r.project_id, p.name, r.number, r.state, r.trigger,
		        r.commit_sha, r.commit_message, r.commit_author, r.branch,
		        r.pull_request_number, r.created_at, r.started_at, r.finished_at,
		        (SELECT count(*) FROM jobs j WHERE j.run_id = r.id),
		        (SELECT count(*) FROM jobs j WHERE j.run_id = r.id
		          AND j.state IN ('SUCCEEDED','FAILED','CANCELLED','SKIPPED','TIMED_OUT'))
		   FROM pipeline_runs r JOIN projects p ON p.id = r.project_id
		  WHERE p.organisation_id = $1 AND r.id = $2`, orgID, id)
	if err != nil {
		return nil, fmt.Errorf("get run: %w", err)
	}
	defer rows.Close()
	runs, err := scanRuns(rows)
	if err != nil {
		return nil, err
	}
	if len(runs) == 0 {
		return nil, ErrNotFound
	}
	return &runs[0], nil
}

func scanRuns(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
}) ([]RunSummary, error) {
	var out []RunSummary
	for rows.Next() {
		var r RunSummary
		if err := rows.Scan(&r.ID, &r.ProjectID, &r.ProjectName, &r.Number, &r.State,
			&r.Trigger, &r.CommitSHA, &r.CommitMessage, &r.CommitAuthor, &r.Branch,
			&r.PullRequest, &r.CreatedAt, &r.StartedAt, &r.FinishedAt,
			&r.JobsTotal, &r.JobsDone); err != nil {
			return nil, err
		}
		if r.StartedAt != nil {
			end := time.Now()
			if r.FinishedAt != nil {
				end = *r.FinishedAt
			}
			ms := end.Sub(*r.StartedAt).Milliseconds()
			r.DurationMS = &ms
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

type JobView struct {
	ID           domain.ID       `json:"id"`
	RunID        domain.ID       `json:"run_id"`
	StepKey      string          `json:"step_key"`
	Name         string          `json:"name"`
	Type         string          `json:"type"`
	State        string          `json:"state"`
	DependsOn    []string        `json:"depends_on"`
	Wave         int             `json:"wave"`
	Attempt      int             `json:"attempt"`
	MaxAttempts  int             `json:"max_attempts"`
	WorkerID     *string         `json:"worker_id,omitempty"`
	ErrorCode    *string         `json:"error_code,omitempty"`
	ErrorMessage *string         `json:"error_message,omitempty"`
	StartedAt    *time.Time      `json:"started_at,omitempty"`
	FinishedAt   *time.Time      `json:"finished_at,omitempty"`
	DurationMS   *int64          `json:"duration_ms,omitempty"`
	Attempts     []AttemptView   `json:"attempts,omitempty"`
	Spec         json.RawMessage `json:"-"`
}

type AttemptView struct {
	Attempt      int        `json:"attempt"`
	State        string     `json:"state"`
	WorkerID     *string    `json:"worker_id,omitempty"`
	RetryReason  *string    `json:"retry_reason,omitempty"`
	ErrorCode    *string    `json:"error_code,omitempty"`
	ErrorMessage *string    `json:"error_message,omitempty"`
	StartedAt    time.Time  `json:"started_at"`
	FinishedAt   *time.Time `json:"finished_at,omitempty"`
}

func (s *ReadStore) ListJobs(ctx context.Context, runID domain.ID) ([]JobView, error) {
	rows, err := s.db.Query(ctx,
		`SELECT id, run_id, step_key, name, type, state, depends_on, wave,
		        attempt, max_attempts, worker_id, error_code, error_message,
		        started_at, finished_at
		   FROM jobs WHERE run_id = $1
		  ORDER BY wave, step_key`, runID)
	if err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
	}
	defer rows.Close()

	var out []JobView
	index := map[domain.ID]int{}
	for rows.Next() {
		var j JobView
		if err := rows.Scan(&j.ID, &j.RunID, &j.StepKey, &j.Name, &j.Type, &j.State,
			&j.DependsOn, &j.Wave, &j.Attempt, &j.MaxAttempts, &j.WorkerID,
			&j.ErrorCode, &j.ErrorMessage, &j.StartedAt, &j.FinishedAt); err != nil {
			return nil, err
		}
		if j.StartedAt != nil {
			end := time.Now()
			if j.FinishedAt != nil {
				end = *j.FinishedAt
			}
			ms := end.Sub(*j.StartedAt).Milliseconds()
			j.DurationMS = &ms
		}
		index[j.ID] = len(out)
		out = append(out, j)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Attempt history in one further query rather than one per job.
	arows, err := s.db.Query(ctx,
		`SELECT job_id, attempt, state, worker_id, retry_reason, error_code,
		        error_message, started_at, finished_at
		   FROM job_attempts WHERE run_id = $1 ORDER BY job_id, attempt`, runID)
	if err != nil {
		return nil, fmt.Errorf("list attempts: %w", err)
	}
	defer arows.Close()
	for arows.Next() {
		var jobID domain.ID
		var a AttemptView
		if err := arows.Scan(&jobID, &a.Attempt, &a.State, &a.WorkerID, &a.RetryReason,
			&a.ErrorCode, &a.ErrorMessage, &a.StartedAt, &a.FinishedAt); err != nil {
			return nil, err
		}
		if i, ok := index[jobID]; ok {
			out[i].Attempts = append(out[i].Attempts, a)
		}
	}
	return out, arows.Err()
}

type LogLine struct {
	Seq      int       `json:"seq"`
	Attempt  int       `json:"attempt"`
	Stream   string    `json:"stream"`
	Message  string    `json:"message"`
	LoggedAt time.Time `json:"logged_at"`
}

func (s *ReadStore) JobLogs(ctx context.Context, jobID domain.ID, attempt, limit int) ([]LogLine, error) {
	rows, err := s.db.Query(ctx,
		`SELECT seq, attempt, stream, message, logged_at
		   FROM job_logs
		  WHERE job_id = $1 AND ($2 = 0 OR attempt = $2)
		  ORDER BY attempt, seq
		  LIMIT $3`, jobID, attempt, limit)
	if err != nil {
		return nil, fmt.Errorf("job logs: %w", err)
	}
	defer rows.Close()

	out := []LogLine{}
	for rows.Next() {
		var l LogLine
		if err := rows.Scan(&l.Seq, &l.Attempt, &l.Stream, &l.Message, &l.LoggedAt); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

type EnvironmentView struct {
	ID           domain.ID  `json:"id"`
	ProjectID    domain.ID  `json:"project_id"`
	ProjectName  string     `json:"project_name,omitempty"`
	Name         string     `json:"name"`
	Kind         string     `json:"kind"`
	Namespace    string     `json:"namespace"`
	URL          *string    `json:"url,omitempty"`
	State        string     `json:"state"`
	PullRequest  *int       `json:"pull_request,omitempty"`
	SourceBranch *string    `json:"source_branch,omitempty"`
	ExpiresAt    *time.Time `json:"expires_at,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	Revision     *string    `json:"revision,omitempty"`
	ActualRev    *string    `json:"actual_revision,omitempty"`
	HealthStatus *string    `json:"health_status,omitempty"`
	SyncStatus   *string    `json:"sync_status,omitempty"`
	DeployedAt   *time.Time `json:"deployed_at,omitempty"`
	OpenDrift    int        `json:"open_drift"`
}

func (s *ReadStore) ListEnvironments(ctx context.Context, orgID, projectID domain.ID) ([]EnvironmentView, error) {
	rows, err := s.db.Query(ctx,
		`SELECT e.id, e.project_id, p.name, e.name, e.kind, e.namespace, e.url, e.state,
		        e.pull_request_number, e.source_branch, e.expires_at, e.created_at,
		        d.desired_revision, d.actual_revision, d.health_status, d.sync_status, d.finished_at,
		        (SELECT count(*) FROM drift_records dr
		          WHERE dr.environment_id = e.id AND dr.resolved_at IS NULL)
		   FROM environments e
		   JOIN projects p ON p.id = e.project_id
		   LEFT JOIN LATERAL (
		        SELECT desired_revision, actual_revision, health_status, sync_status, finished_at
		          FROM deployments
		         WHERE environment_id = e.id AND state IN ('HEALTHY','DEGRADED')
		         ORDER BY id DESC LIMIT 1
		   ) d ON true
		  WHERE p.organisation_id = $1
		    AND ($2 = '' OR e.project_id = $2)
		    AND e.destroyed_at IS NULL
		  ORDER BY
		    CASE e.kind WHEN 'PRODUCTION' THEN 0 WHEN 'STAGING' THEN 1
		                WHEN 'DEVELOPMENT' THEN 2 ELSE 3 END,
		    e.name`, orgID, projectID)
	if err != nil {
		return nil, fmt.Errorf("list environments: %w", err)
	}
	defer rows.Close()

	out := []EnvironmentView{}
	for rows.Next() {
		var e EnvironmentView
		if err := rows.Scan(&e.ID, &e.ProjectID, &e.ProjectName, &e.Name, &e.Kind,
			&e.Namespace, &e.URL, &e.State, &e.PullRequest, &e.SourceBranch, &e.ExpiresAt,
			&e.CreatedAt, &e.Revision, &e.ActualRev, &e.HealthStatus, &e.SyncStatus,
			&e.DeployedAt, &e.OpenDrift); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

type DeploymentView struct {
	ID              domain.ID  `json:"id"`
	ProjectID       domain.ID  `json:"project_id"`
	EnvironmentID   domain.ID  `json:"environment_id"`
	EnvironmentName string     `json:"environment_name"`
	Number          int64      `json:"number"`
	State           string     `json:"state"`
	ReleaseVersion  string     `json:"release_version"`
	CommitSHA       string     `json:"commit_sha"`
	Image           string     `json:"image"`
	DesiredRevision string     `json:"desired_revision"`
	ActualRevision  *string    `json:"actual_revision,omitempty"`
	SyncStatus      string     `json:"sync_status"`
	HealthStatus    string     `json:"health_status"`
	RequestedBy     *string    `json:"requested_by,omitempty"`
	Reason          string     `json:"reason"`
	IsRollback      bool       `json:"is_rollback"`
	RequestedAt     time.Time  `json:"requested_at"`
	StartedAt       *time.Time `json:"started_at,omitempty"`
	VerifiedAt      *time.Time `json:"verified_at,omitempty"`
	FinishedAt      *time.Time `json:"finished_at,omitempty"`
	DurationMS      *int64     `json:"duration_ms,omitempty"`
}

func (s *ReadStore) ListDeployments(ctx context.Context, orgID, projectID, environmentID domain.ID, limit int, cursor string) ([]DeploymentView, error) {
	rows, err := s.db.Query(ctx,
		`SELECT d.id, d.project_id, d.environment_id, e.name, d.number, d.state,
		        rel.version, rel.commit_sha, a.reference,
		        d.desired_revision, d.actual_revision, d.sync_status, d.health_status,
		        u.email, d.reason, (d.rollback_of IS NOT NULL),
		        d.requested_at, d.started_at, d.verified_at, d.finished_at
		   FROM deployments d
		   JOIN environments e ON e.id = d.environment_id
		   JOIN projects p ON p.id = d.project_id
		   JOIN releases rel ON rel.id = d.release_id
		   JOIN artifacts a ON a.id = rel.artifact_id
		   LEFT JOIN users u ON u.id = d.requested_by
		  WHERE p.organisation_id = $1
		    AND ($2 = '' OR d.project_id = $2)
		    AND ($3 = '' OR d.environment_id = $3)
		    AND ($4 = '' OR d.id < $4)
		  ORDER BY d.id DESC
		  LIMIT $5`, orgID, projectID, environmentID, cursor, limit)
	if err != nil {
		return nil, fmt.Errorf("list deployments: %w", err)
	}
	defer rows.Close()

	out := []DeploymentView{}
	for rows.Next() {
		var d DeploymentView
		if err := rows.Scan(&d.ID, &d.ProjectID, &d.EnvironmentID, &d.EnvironmentName,
			&d.Number, &d.State, &d.ReleaseVersion, &d.CommitSHA, &d.Image,
			&d.DesiredRevision, &d.ActualRevision, &d.SyncStatus, &d.HealthStatus,
			&d.RequestedBy, &d.Reason, &d.IsRollback, &d.RequestedAt, &d.StartedAt,
			&d.VerifiedAt, &d.FinishedAt); err != nil {
			return nil, err
		}
		if d.StartedAt != nil && d.FinishedAt != nil {
			ms := d.FinishedAt.Sub(*d.StartedAt).Milliseconds()
			d.DurationMS = &ms
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

type HealthCheckView struct {
	Attempt    int       `json:"attempt"`
	Status     string    `json:"status"`
	HTTPStatus *int      `json:"http_status,omitempty"`
	LatencyMS  *int      `json:"latency_ms,omitempty"`
	Message    string    `json:"message"`
	CheckedAt  time.Time `json:"checked_at"`
}

func (s *ReadStore) HealthChecks(ctx context.Context, deploymentID domain.ID) ([]HealthCheckView, error) {
	rows, err := s.db.Query(ctx,
		`SELECT attempt, status, http_status, latency_ms, message, checked_at
		   FROM health_checks WHERE deployment_id = $1 ORDER BY attempt`, deploymentID)
	if err != nil {
		return nil, fmt.Errorf("health checks: %w", err)
	}
	defer rows.Close()

	out := []HealthCheckView{}
	for rows.Next() {
		var h HealthCheckView
		if err := rows.Scan(&h.Attempt, &h.Status, &h.HTTPStatus, &h.LatencyMS,
			&h.Message, &h.CheckedAt); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

type WorkerView struct {
	ID          domain.ID `json:"id"`
	Hostname    string    `json:"hostname"`
	Concurrency int       `json:"concurrency"`
	ActiveJobs  int       `json:"active_jobs"`
	State       string    `json:"state"`
	StartedAt   time.Time `json:"started_at"`
	LastSeen    time.Time `json:"last_heartbeat_at"`
	// Live is derived rather than stored, so a worker that died without deregistering
	// is still correctly reported as gone.
	Live bool `json:"live"`
}

func (s *ReadStore) ListWorkers(ctx context.Context) ([]WorkerView, error) {
	rows, err := s.db.Query(ctx,
		`SELECT id, hostname, concurrency, active_jobs, state, started_at,
		        last_heartbeat_at, (last_heartbeat_at > now() - interval '45 seconds')
		   FROM workers
		  WHERE state <> 'STOPPED' OR last_heartbeat_at > now() - interval '10 minutes'
		  ORDER BY last_heartbeat_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list workers: %w", err)
	}
	defer rows.Close()

	out := []WorkerView{}
	for rows.Next() {
		var w WorkerView
		if err := rows.Scan(&w.ID, &w.Hostname, &w.Concurrency, &w.ActiveJobs, &w.State,
			&w.StartedAt, &w.LastSeen, &w.Live); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// Overview is the dashboard payload, deliberately assembled in a small number of
// queries rather than one per tile.
type Overview struct {
	Projects            int      `json:"projects"`
	Environments        int      `json:"environments"`
	RunsLast24h         int      `json:"runs_last_24h"`
	RunsRunning         int      `json:"runs_running"`
	RunsFailedLast24h   int      `json:"runs_failed_last_24h"`
	SuccessRate         float64  `json:"success_rate"`
	MedianRunSeconds    *float64 `json:"median_run_seconds,omitempty"`
	DeploymentsLast24h  int      `json:"deployments_last_24h"`
	MedianDeploySeconds *float64 `json:"median_deploy_seconds,omitempty"`
	HealthyEnvironments int      `json:"healthy_environments"`
	DegradedDeployments int      `json:"degraded_deployments"`
	QueueDepth          int      `json:"queue_depth"`
	JobsRunning         int      `json:"jobs_running"`
	WorkersLive         int      `json:"workers_live"`
	OpenDrift           int      `json:"open_drift"`
	OpenIncidents       int      `json:"open_incidents"`
	OutboxPending       int      `json:"outbox_pending"`
}

func (s *ReadStore) Overview(ctx context.Context, orgID domain.ID) (*Overview, error) {
	var o Overview
	err := s.db.QueryRow(ctx,
		`SELECT
		   (SELECT count(*) FROM projects WHERE organisation_id=$1 AND archived_at IS NULL),
		   (SELECT count(*) FROM environments e JOIN projects p ON p.id=e.project_id
		     WHERE p.organisation_id=$1 AND e.destroyed_at IS NULL),
		   (SELECT count(*) FROM pipeline_runs r JOIN projects p ON p.id=r.project_id
		     WHERE p.organisation_id=$1 AND r.created_at > now() - interval '24 hours'),
		   (SELECT count(*) FROM pipeline_runs r JOIN projects p ON p.id=r.project_id
		     WHERE p.organisation_id=$1 AND r.state IN ('QUEUED','RUNNING')),
		   (SELECT count(*) FROM pipeline_runs r JOIN projects p ON p.id=r.project_id
		     WHERE p.organisation_id=$1 AND r.state IN ('FAILED','TIMED_OUT')
		       AND r.created_at > now() - interval '24 hours'),
		   COALESCE((SELECT round(100.0 * count(*) FILTER (WHERE r.state='SUCCEEDED')
		                          / NULLIF(count(*),0), 1)
		               FROM pipeline_runs r JOIN projects p ON p.id=r.project_id
		              WHERE p.organisation_id=$1
		                AND r.state IN ('SUCCEEDED','FAILED','TIMED_OUT')
		                AND r.created_at > now() - interval '7 days'), 0),
		   (SELECT percentile_cont(0.5) WITHIN GROUP (ORDER BY extract(epoch FROM (r.finished_at - r.started_at)))
		      FROM pipeline_runs r JOIN projects p ON p.id=r.project_id
		     WHERE p.organisation_id=$1 AND r.finished_at IS NOT NULL
		       AND r.created_at > now() - interval '7 days'),
		   (SELECT count(*) FROM deployments d JOIN projects p ON p.id=d.project_id
		     WHERE p.organisation_id=$1 AND d.requested_at > now() - interval '24 hours'),
		   (SELECT percentile_cont(0.5) WITHIN GROUP (ORDER BY extract(epoch FROM (d.finished_at - d.started_at)))
		      FROM deployments d JOIN projects p ON p.id=d.project_id
		     WHERE p.organisation_id=$1 AND d.finished_at IS NOT NULL
		       AND d.requested_at > now() - interval '7 days'),
		   (SELECT count(*) FROM deployments d JOIN projects p ON p.id=d.project_id
		     WHERE p.organisation_id=$1 AND d.state='HEALTHY'),
		   (SELECT count(*) FROM deployments d JOIN projects p ON p.id=d.project_id
		     WHERE p.organisation_id=$1 AND d.state='DEGRADED'),
		   (SELECT count(*) FROM jobs WHERE state='QUEUED'),
		   (SELECT count(*) FROM jobs WHERE state='RUNNING'),
		   (SELECT count(*) FROM workers WHERE last_heartbeat_at > now() - interval '45 seconds'),
		   (SELECT count(*) FROM drift_records WHERE resolved_at IS NULL),
		   (SELECT count(*) FROM incidents i JOIN projects p ON p.id=i.project_id
		     WHERE p.organisation_id=$1 AND i.state <> 'RESOLVED'),
		   (SELECT count(*) FROM outbox WHERE published_at IS NULL)`, orgID).
		Scan(&o.Projects, &o.Environments, &o.RunsLast24h, &o.RunsRunning,
			&o.RunsFailedLast24h, &o.SuccessRate, &o.MedianRunSeconds,
			&o.DeploymentsLast24h, &o.MedianDeploySeconds, &o.HealthyEnvironments,
			&o.DegradedDeployments, &o.QueueDepth, &o.JobsRunning, &o.WorkersLive,
			&o.OpenDrift, &o.OpenIncidents, &o.OutboxPending)
	if err != nil {
		return nil, fmt.Errorf("overview: %w", err)
	}
	return &o, nil
}

// ErrNotFound is returned when a read finds nothing. Handlers map it to a 404.
var ErrNotFound = fmt.Errorf("not found")

// GetDeployment loads one deployment for the console's deployment detail screen.
func (s *ReadStore) GetDeployment(ctx context.Context, orgID, id domain.ID) (*DeploymentView, error) {
	rows, err := s.db.Query(ctx,
		`SELECT d.id, d.project_id, d.environment_id, e.name, d.number, d.state,
		        rel.version, rel.commit_sha, a.reference,
		        d.desired_revision, d.actual_revision, d.sync_status, d.health_status,
		        u.email, d.reason, (d.rollback_of IS NOT NULL),
		        d.requested_at, d.started_at, d.verified_at, d.finished_at
		   FROM deployments d
		   JOIN environments e ON e.id = d.environment_id
		   JOIN projects p ON p.id = d.project_id
		   JOIN releases rel ON rel.id = d.release_id
		   JOIN artifacts a ON a.id = rel.artifact_id
		   LEFT JOIN users u ON u.id = d.requested_by
		  WHERE p.organisation_id = $1 AND d.id = $2`, orgID, id)
	if err != nil {
		return nil, fmt.Errorf("get deployment: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, ErrNotFound
	}
	var d DeploymentView
	if err := rows.Scan(&d.ID, &d.ProjectID, &d.EnvironmentID, &d.EnvironmentName,
		&d.Number, &d.State, &d.ReleaseVersion, &d.CommitSHA, &d.Image,
		&d.DesiredRevision, &d.ActualRevision, &d.SyncStatus, &d.HealthStatus,
		&d.RequestedBy, &d.Reason, &d.IsRollback, &d.RequestedAt, &d.StartedAt,
		&d.VerifiedAt, &d.FinishedAt); err != nil {
		return nil, err
	}
	if d.StartedAt != nil && d.FinishedAt != nil {
		ms := d.FinishedAt.Sub(*d.StartedAt).Milliseconds()
		d.DurationMS = &ms
	}
	return &d, nil
}
