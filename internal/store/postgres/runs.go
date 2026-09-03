package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Keathan2609/nexus/internal/core/domain"
	"github.com/Keathan2609/nexus/internal/core/events"
	"github.com/Keathan2609/nexus/internal/core/outbox"
	"github.com/Keathan2609/nexus/internal/pipelines"
	"github.com/Keathan2609/nexus/internal/platform/database"
)

type RunStore struct{ db *database.DB }

func NewRunStore(db *database.DB) *RunStore { return &RunStore{db: db} }

type CreateRunParams struct {
	ProjectID      domain.ID
	DefinitionID   domain.ID
	EnvironmentID  domain.ID
	Definition     pipelines.Definition
	Trigger        string
	TriggeredBy    domain.ID
	CommitSHA      string
	CommitMessage  string
	CommitAuthor   string
	Branch         string
	PullRequest    int
	WebhookEventID string
	CorrelationID  string
}

type Run struct {
	ID         domain.ID
	ProjectID  domain.ID
	Number     int64
	State      domain.RunState
	Trigger    string
	CommitSHA  string
	Branch     string
	JobCount   int
	CreatedAt  time.Time
	StartedAt  *time.Time
	FinishedAt *time.Time
}

// CreateRun plans a definition into jobs and persists the whole run atomically.
//
// Everything here happens in one transaction: the run number is allocated, the run row
// is written, every job is inserted, and the pipeline.queued event is placed in the
// outbox. A partially created run — a run row with no jobs, or jobs with no run — is
// not a state this function can produce.
func (s *RunStore) CreateRun(ctx context.Context, p CreateRunParams) (*Run, error) {
	planned, err := p.Definition.Plan()
	if err != nil {
		return nil, fmt.Errorf("plan pipeline: %w", err)
	}

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin create run: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Allocate the human-facing run number.
	//
	// The UPSERT makes this work for a project whose counter row does not exist yet,
	// and the UPDATE ... RETURNING is atomic, so two concurrent triggers on the same
	// project cannot both be given #184. The row lock is held only until this
	// transaction commits.
	var number int64
	if err := tx.QueryRow(ctx,
		`INSERT INTO run_counters (project_id, next_number) VALUES ($1, 2)
		 ON CONFLICT (project_id) DO UPDATE SET next_number = run_counters.next_number + 1
		 RETURNING next_number - 1`, p.ProjectID).Scan(&number); err != nil {
		return nil, fmt.Errorf("allocate run number: %w", err)
	}

	runID := domain.NewID(domain.PrefixRun)
	run := &Run{
		ID: runID, ProjectID: p.ProjectID, Number: number,
		State: domain.RunQueued, Trigger: p.Trigger,
		CommitSHA: p.CommitSHA, Branch: p.Branch, JobCount: len(planned),
	}

	if err := tx.QueryRow(ctx,
		`INSERT INTO pipeline_runs
		   (id, project_id, definition_id, environment_id, number, state, trigger,
		    triggered_by, commit_sha, commit_message, commit_author, branch,
		    pull_request_number, webhook_event_id, queued_at)
		 VALUES ($1,$2,$3,NULLIF($4,''),$5,'QUEUED',$6,NULLIF($7,''),$8,$9,$10,$11,
		         NULLIF($12,0),NULLIF($13,''), now())
		 RETURNING created_at`,
		runID, p.ProjectID, p.DefinitionID, p.EnvironmentID, number, p.Trigger,
		p.TriggeredBy, p.CommitSHA, p.CommitMessage, p.CommitAuthor, p.Branch,
		p.PullRequest, p.WebhookEventID,
	).Scan(&run.CreatedAt); err != nil {
		return nil, fmt.Errorf("insert run: %w", err)
	}

	for _, pj := range planned {
		step, ok := p.Definition.Step(pj.StepKey)
		if !ok {
			return nil, fmt.Errorf("planned step %q missing from definition", pj.StepKey)
		}
		retry := step.RetryPolicyOrDefault()

		state := domain.JobQueued
		if pj.Blocked {
			state = domain.JobBlocked
		}
		timeout := step.TimeoutSeconds
		if timeout <= 0 {
			timeout = 600
		}
		spec, err := json.Marshal(map[string]any{
			"command":       step.Command,
			"image":         step.Image,
			"env":           step.Env,
			"build_context": step.BuildContext,
			"dockerfile":    step.Dockerfile,
		})
		if err != nil {
			return nil, fmt.Errorf("marshal step spec: %w", err)
		}

		// depends_on is NOT NULL with a '{}' default, and a nil Go slice encodes as SQL
		// NULL rather than an empty array. A root step has no dependencies, so this is
		// the common case, not an edge case.
		dependsOn := pj.DependsOn
		if dependsOn == nil {
			dependsOn = []string{}
		}

		if _, err := tx.Exec(ctx,
			`INSERT INTO jobs
			   (id, run_id, project_id, step_key, name, type, state, depends_on, wave,
			    max_attempts, timeout_seconds, continue_on_error, spec)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
			domain.NewID(domain.PrefixJob), runID, p.ProjectID, step.Key, step.Name,
			string(step.Type), string(state), dependsOn, pj.Wave,
			retry.MaxAttempts, timeout, step.ContinueOnError, spec,
		); err != nil {
			return nil, fmt.Errorf("insert job %s: %w", step.Key, err)
		}
	}

	env := events.New(events.PipelineQueued, "pipeline_run", runID, map[string]any{
		"project_id": p.ProjectID.String(),
		"number":     number,
		"trigger":    p.Trigger,
		"commit_sha": p.CommitSHA,
		"branch":     p.Branch,
		"job_count":  len(planned),
	}).WithCorrelation(p.CorrelationID)
	if err := outbox.Write(ctx, tx, env); err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit create run: %w", err)
	}
	return run, nil
}

// Get loads a run by id.
func (s *RunStore) Get(ctx context.Context, id domain.ID) (*Run, error) {
	var r Run
	err := s.db.QueryRow(ctx,
		`SELECT id, project_id, number, state, trigger, commit_sha, branch,
		        created_at, started_at, finished_at,
		        (SELECT count(*) FROM jobs WHERE run_id = pipeline_runs.id)
		   FROM pipeline_runs WHERE id = $1`, id).
		Scan(&r.ID, &r.ProjectID, &r.Number, &r.State, &r.Trigger, &r.CommitSHA,
			&r.Branch, &r.CreatedAt, &r.StartedAt, &r.FinishedAt, &r.JobCount)
	if err != nil {
		return nil, fmt.Errorf("get run %s: %w", id, err)
	}
	return &r, nil
}
