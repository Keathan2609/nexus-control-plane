// Package deployments owns the deployment lifecycle: requesting a release into an
// environment, applying the desired state, verifying that what came up is actually
// healthy, and rolling back when it is not.
//
// The rule this package exists to enforce is that health is observed, never assumed. A
// successful apply means the cluster accepted the manifests; it says nothing about
// whether the process inside the container can serve a request. Every route to HEALTHY
// runs through verification, and the probe results are persisted as evidence rather
// than collapsed into a boolean.
package deployments

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Keathan2609/nexus/internal/core/domain"
	"github.com/Keathan2609/nexus/internal/core/events"
	"github.com/Keathan2609/nexus/internal/core/outbox"
	"github.com/Keathan2609/nexus/internal/platform/database"
	"github.com/Keathan2609/nexus/internal/platform/logging"
	"github.com/Keathan2609/nexus/internal/platform/telemetry"
)

// Desired is the state NEXUS wants an environment to be in.
type Desired struct {
	ProjectID     domain.ID
	EnvironmentID domain.ID
	Namespace     string
	Revision      string
	Image         string
	ServiceName   string
	Port          int
	Replicas      int
	Manifests     string
}

// Applier turns desired state into running state.
//
// Behind this interface sit two implementations with very different mechanics — a
// Docker applier for local development and a GitOps applier that writes desired state
// for Argo CD to reconcile — and the lifecycle above does not care which is in use.
type Applier interface {
	// Apply makes the desired state real and returns the revision now running.
	Apply(ctx context.Context, d Desired) (string, error)
	// Observe reports the revision actually running, which may differ from what was
	// last applied. That difference is drift.
	Observe(ctx context.Context, environmentID domain.ID) (string, error)
	Destroy(ctx context.Context, environmentID domain.ID) error
}

type ProbeResult struct {
	Status     string // PASS | FAIL | TIMEOUT
	HTTPStatus int
	LatencyMS  int
	Message    string
}

type Prober interface {
	Probe(ctx context.Context, target string) (ProbeResult, error)
}

// Verification bounds how hard NEXUS tries to convince itself a deployment is healthy.
type Verification struct {
	// RequiredPasses is consecutive, not cumulative. A target that alternates between
	// serving and refusing connections is not healthy, and a cumulative count would
	// eventually accept it.
	RequiredPasses int
	MaxAttempts    int
	Interval       time.Duration
	Timeout        time.Duration
}

func DefaultVerification() Verification {
	return Verification{
		RequiredPasses: 3,
		MaxAttempts:    30,
		Interval:       2 * time.Second,
		Timeout:        2 * time.Minute,
	}
}

var (
	// ErrVerificationFailed means the deployment applied but never became healthy.
	ErrVerificationFailed = errors.New("deployment failed health verification")
	// ErrNoHealthyRelease means there is nothing to roll back to.
	ErrNoHealthyRelease = errors.New("no earlier healthy release to roll back to")
)

type Service struct {
	db           *database.DB
	Applier      Applier
	Prober       Prober
	Verification Verification
	log          *slog.Logger
	metrics      *telemetry.Metrics
}

func NewService(db *database.DB, applier Applier, prober Prober, log *slog.Logger, m *telemetry.Metrics) *Service {
	return &Service{
		db: db, Applier: applier, Prober: prober,
		Verification: DefaultVerification(), log: log, metrics: m,
	}
}

type RequestParams struct {
	ProjectID     domain.ID
	EnvironmentID domain.ID
	ReleaseID     domain.ID
	RunID         domain.ID
	RequestedBy   domain.ID
	Reason        string
	RollbackOf    domain.ID
	CorrelationID string
}

type Deployment struct {
	ID              domain.ID
	ProjectID       domain.ID
	EnvironmentID   domain.ID
	ReleaseID       domain.ID
	Number          int64
	State           domain.DeploymentState
	DesiredRevision string
	ActualRevision  string
	SyncStatus      string
	HealthStatus    string
	RequestedAt     time.Time
	FinishedAt      *time.Time
}

// Request records the intent to deploy. It does not deploy: intent and execution are
// separated so a request can be made inside the transaction that produced it (a pipeline
// finishing, a webhook arriving) while the slow, failure-prone work happens elsewhere.
func (s *Service) Request(ctx context.Context, p RequestParams) (*Deployment, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin request: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var number int64
	if err := tx.QueryRow(ctx,
		`INSERT INTO deployment_counters (project_id, next_number) VALUES ($1, 2)
		 ON CONFLICT (project_id) DO UPDATE SET next_number = deployment_counters.next_number + 1
		 RETURNING next_number - 1`, p.ProjectID).Scan(&number); err != nil {
		return nil, fmt.Errorf("allocate deployment number: %w", err)
	}

	// The desired revision is derived from the release, so the same release always
	// produces the same revision string and a redeploy is recognisably a no-op.
	var version, commitSHA string
	if err := tx.QueryRow(ctx,
		`SELECT version, commit_sha FROM releases WHERE id=$1`, p.ReleaseID).
		Scan(&version, &commitSHA); err != nil {
		return nil, fmt.Errorf("load release %s: %w", p.ReleaseID, err)
	}
	revision := version + "@" + shortSHA(commitSHA)

	id := domain.NewID(domain.PrefixDeployment)
	dep := &Deployment{
		ID: id, ProjectID: p.ProjectID, EnvironmentID: p.EnvironmentID,
		ReleaseID: p.ReleaseID, Number: number, State: domain.DeployRequested,
		DesiredRevision: revision, SyncStatus: "OUT_OF_SYNC", HealthStatus: "UNKNOWN",
	}

	if err := tx.QueryRow(ctx,
		`INSERT INTO deployments
		   (id, project_id, environment_id, release_id, run_id, number, state,
		    desired_revision, requested_by, reason, rollback_of)
		 VALUES ($1,$2,$3,$4,NULLIF($5,''),$6,'REQUESTED',$7,NULLIF($8,''),$9,NULLIF($10,''))
		 RETURNING requested_at`,
		id, p.ProjectID, p.EnvironmentID, p.ReleaseID, p.RunID, number,
		revision, p.RequestedBy, p.Reason, p.RollbackOf).Scan(&dep.RequestedAt); err != nil {
		return nil, fmt.Errorf("insert deployment: %w", err)
	}

	if err := outbox.Write(ctx, tx, events.New(events.DeploymentRequested, "deployment", id,
		map[string]any{
			"project_id":     p.ProjectID.String(),
			"environment_id": p.EnvironmentID.String(),
			"release_id":     p.ReleaseID.String(),
			"revision":       revision,
			"number":         number,
			"rollback":       p.RollbackOf != "",
		}).WithCorrelation(p.CorrelationID)); err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit request: %w", err)
	}
	return dep, nil
}

// Execute drives a requested deployment through provisioning, applying and verification.
//
// Each transition is persisted as it happens rather than at the end, so a control plane
// that is killed halfway leaves a deployment visibly stuck in DEPLOYING rather than
// silently in REQUESTED. The reconciler picks those up.
func (s *Service) Execute(ctx context.Context, id domain.ID) error {
	dep, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	log := s.log.With(logging.FieldDeploymentID, id.String(),
		logging.FieldEnvironmentID, dep.EnvironmentID.String())

	env, err := s.loadEnvironment(ctx, dep.EnvironmentID)
	if err != nil {
		return err
	}
	started := time.Now()

	if err := s.transition(ctx, dep, domain.DeployProvisioning, nil); err != nil {
		return err
	}

	desired, err := s.desiredFor(ctx, dep, env)
	if err != nil {
		return s.fail(ctx, dep, started, env, fmt.Errorf("render desired state: %w", err))
	}
	if err := s.recordDesiredState(ctx, dep, desired); err != nil {
		return s.fail(ctx, dep, started, env, err)
	}

	if err := s.transition(ctx, dep, domain.DeployDeploying, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE deployments SET started_at=now() WHERE id=$1`, dep.ID)
		return err
	}); err != nil {
		return err
	}

	actual, err := s.Applier.Apply(ctx, desired)
	if err != nil {
		log.Error("apply failed", logging.FieldError, err)
		return s.fail(ctx, dep, started, env, fmt.Errorf("apply: %w", err))
	}

	if err := s.transition(ctx, dep, domain.DeployVerifying, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE deployments SET actual_revision=$2, sync_status='SYNCED' WHERE id=$1`,
			dep.ID, actual)
		return err
	}); err != nil {
		return err
	}
	if err := s.emit(ctx, dep, events.DeploymentVerifying, map[string]any{"revision": actual}); err != nil {
		return err
	}

	if err := s.verify(ctx, dep, env); err != nil {
		log.Warn("verification failed", logging.FieldError, err)
		return s.fail(ctx, dep, started, env, err)
	}

	// Healthy. Supersede whatever was live before this, inside the same transaction
	// that marks this one healthy, so the partial unique index on live deployments is
	// never momentarily violated.
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin promote: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx,
		`UPDATE deployments SET state='SUPERSEDED', finished_at=COALESCE(finished_at, now())
		  WHERE environment_id=$1 AND id <> $2 AND state IN ('HEALTHY','DEGRADED')`,
		dep.EnvironmentID, dep.ID); err != nil {
		return fmt.Errorf("supersede previous deployment: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE deployments
		    SET state='HEALTHY', health_status='HEALTHY', sync_status='SYNCED',
		        verified_at=now(), finished_at=now(), version=version+1
		  WHERE id=$1`, dep.ID); err != nil {
		return fmt.Errorf("mark healthy: %w", err)
	}
	if err := outbox.Write(ctx, tx, events.New(events.DeploymentHealthy, "deployment", dep.ID,
		map[string]any{
			"environment_id": dep.EnvironmentID.String(),
			"revision":       dep.DesiredRevision,
		})); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit promote: %w", err)
	}

	if s.metrics != nil {
		s.metrics.ObserveDeployment(env.Name, "HEALTHY", time.Since(started))
	}
	log.Info("deployment healthy", "revision", dep.DesiredRevision,
		"duration_ms", time.Since(started).Milliseconds())
	return nil
}

// verify probes the target until it passes RequiredPasses times consecutively, or until
// the budget runs out. Every probe is persisted.
func (s *Service) verify(ctx context.Context, dep *Deployment, env *Environment) error {
	if err := s.setState(ctx, dep, domain.DeployVerifying); err != nil {
		return err
	}

	vctx, cancel := context.WithTimeout(ctx, s.Verification.Timeout)
	defer cancel()

	target := env.URL
	if target == "" {
		target = fmt.Sprintf("http://localhost:%d/healthz", env.Port)
	}

	consecutive := 0
	for attempt := 1; attempt <= s.Verification.MaxAttempts; attempt++ {
		if vctx.Err() != nil {
			break
		}
		res, err := s.Prober.Probe(vctx, target)
		if err != nil {
			res = ProbeResult{Status: "FAIL", Message: err.Error()}
		}

		if _, dberr := s.db.Exec(ctx,
			`INSERT INTO health_checks
			   (id, deployment_id, target, attempt, status, http_status, latency_ms, message)
			 VALUES ($1,$2,$3,$4,$5,NULLIF($6,0),NULLIF($7,0),$8)`,
			domain.NewID(domain.PrefixHealthCheck), dep.ID, target, attempt,
			res.Status, res.HTTPStatus, res.LatencyMS, res.Message); dberr != nil {
			return fmt.Errorf("record health check: %w", dberr)
		}
		if s.metrics != nil {
			s.metrics.HealthChecks.WithLabelValues(res.Status).Inc()
		}

		if res.Status == "PASS" {
			consecutive++
			if consecutive >= s.Verification.RequiredPasses {
				return nil
			}
		} else {
			// A single failure resets the streak. This is what makes the check
			// meaningful for a target that is flapping rather than simply slow to start.
			consecutive = 0
		}

		select {
		case <-vctx.Done():
		case <-time.After(s.Verification.Interval):
		}
	}
	return fmt.Errorf("%w after %d attempts", ErrVerificationFailed, s.Verification.MaxAttempts)
}

type RollbackParams struct {
	EnvironmentID domain.ID
	RequestedBy   domain.ID
	Reason        string
	// ToRelease pins a specific release. Empty means the most recent healthy one.
	ToRelease     domain.ID
	CorrelationID string
	Automatic     bool
}

// Rollback redeploys an earlier known-good release.
//
// Rollback is an ordinary deployment of an older release, not a special reverse
// operation. That means it goes through the same apply and the same verification, so a
// rollback that does not itself come up healthy is reported as a failed rollback rather
// than assumed to have worked because it used to.
func (s *Service) Rollback(ctx context.Context, p RollbackParams) (*Deployment, error) {
	target := p.ToRelease
	var projectID domain.ID
	var supersede domain.ID

	if target == "" {
		err := s.db.QueryRow(ctx,
			`SELECT d.release_id, d.project_id
			   FROM deployments d
			  WHERE d.environment_id = $1
			    AND d.state IN ('HEALTHY','SUPERSEDED')
			    AND d.health_status = 'HEALTHY'
			  ORDER BY d.id DESC
			  LIMIT 1`, p.EnvironmentID).Scan(&target, &projectID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNoHealthyRelease
		}
		if err != nil {
			return nil, fmt.Errorf("find last healthy release: %w", err)
		}
	} else {
		if err := s.db.QueryRow(ctx,
			`SELECT project_id FROM environments WHERE id=$1`, p.EnvironmentID).Scan(&projectID); err != nil {
			return nil, fmt.Errorf("load environment: %w", err)
		}
	}

	_ = s.db.QueryRow(ctx,
		`SELECT id FROM deployments
		  WHERE environment_id=$1 AND state IN ('FAILED','DEGRADED','HEALTHY')
		  ORDER BY id DESC LIMIT 1`, p.EnvironmentID).Scan(&supersede)

	dep, err := s.Request(ctx, RequestParams{
		ProjectID: projectID, EnvironmentID: p.EnvironmentID, ReleaseID: target,
		RequestedBy: p.RequestedBy, Reason: p.Reason, RollbackOf: supersede,
		CorrelationID: p.CorrelationID,
	})
	if err != nil {
		return nil, err
	}
	if _, err := s.db.Exec(ctx,
		`UPDATE deployments SET rolled_back_to=$2 WHERE id=$1`, dep.ID, target); err != nil {
		return nil, fmt.Errorf("record rollback target: %w", err)
	}

	initiator := "manual"
	if p.Automatic {
		initiator = "automatic"
	}
	if s.metrics != nil {
		if env, err := s.loadEnvironment(ctx, p.EnvironmentID); err == nil {
			s.metrics.Rollbacks.WithLabelValues(env.Name, initiator).Inc()
		}
	}

	if err := s.Execute(ctx, dep.ID); err != nil {
		return dep, err
	}
	if _, err := s.db.Exec(ctx,
		`UPDATE deployments SET state='ROLLED_BACK' WHERE id=$1 AND state='FAILED'`, supersede); err != nil {
		return dep, nil
	}
	return s.Get(ctx, dep.ID)
}

// History returns the deployments for an environment, newest first.
func (s *Service) History(ctx context.Context, environmentID domain.ID, limit int) ([]Deployment, error) {
	rows, err := s.db.Query(ctx,
		`SELECT id, project_id, environment_id, release_id, number, state,
		        desired_revision, COALESCE(actual_revision,''), sync_status, health_status,
		        requested_at, finished_at
		   FROM deployments
		  WHERE environment_id = $1
		  ORDER BY id DESC
		  LIMIT $2`, environmentID, limit)
	if err != nil {
		return nil, fmt.Errorf("deployment history: %w", err)
	}
	defer rows.Close()

	var out []Deployment
	for rows.Next() {
		var d Deployment
		if err := rows.Scan(&d.ID, &d.ProjectID, &d.EnvironmentID, &d.ReleaseID, &d.Number,
			&d.State, &d.DesiredRevision, &d.ActualRevision, &d.SyncStatus, &d.HealthStatus,
			&d.RequestedAt, &d.FinishedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Service) Get(ctx context.Context, id domain.ID) (*Deployment, error) {
	var d Deployment
	err := s.db.QueryRow(ctx,
		`SELECT id, project_id, environment_id, release_id, number, state,
		        desired_revision, COALESCE(actual_revision,''), sync_status, health_status,
		        requested_at, finished_at
		   FROM deployments WHERE id=$1`, id).
		Scan(&d.ID, &d.ProjectID, &d.EnvironmentID, &d.ReleaseID, &d.Number, &d.State,
			&d.DesiredRevision, &d.ActualRevision, &d.SyncStatus, &d.HealthStatus,
			&d.RequestedAt, &d.FinishedAt)
	if err != nil {
		return nil, fmt.Errorf("get deployment %s: %w", id, err)
	}
	return &d, nil
}

type Environment struct {
	ID        domain.ID
	ProjectID domain.ID
	Name      string
	Kind      string
	Namespace string
	URL       string
	Port      int
}

func (s *Service) loadEnvironment(ctx context.Context, id domain.ID) (*Environment, error) {
	var e Environment
	err := s.db.QueryRow(ctx,
		`SELECT id, project_id, name, kind, namespace, COALESCE(url,'')
		   FROM environments WHERE id=$1`, id).
		Scan(&e.ID, &e.ProjectID, &e.Name, &e.Kind, &e.Namespace, &e.URL)
	if err != nil {
		return nil, fmt.Errorf("load environment %s: %w", id, err)
	}
	return &e, nil
}

// transition moves a deployment to a new state, refusing illegal moves, and optionally
// running extra work in the same transaction.
func (s *Service) transition(ctx context.Context, dep *Deployment, to domain.DeploymentState, extra func(pgx.Tx) error) error {
	if _, err := dep.State.TransitionTo(to); err != nil {
		return err
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transition: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx,
		`UPDATE deployments SET state=$2, version=version+1 WHERE id=$1`, dep.ID, string(to)); err != nil {
		return fmt.Errorf("transition to %s: %w", to, err)
	}
	if extra != nil {
		if err := extra(tx); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transition: %w", err)
	}
	dep.State = to
	return nil
}

func (s *Service) setState(ctx context.Context, dep *Deployment, to domain.DeploymentState) error {
	if dep.State == to {
		return nil
	}
	return s.transition(ctx, dep, to, nil)
}

// fail records a terminal failure and emits the corresponding event.
func (s *Service) fail(ctx context.Context, dep *Deployment, started time.Time, env *Environment, cause error) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin fail: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx,
		`UPDATE deployments
		    SET state='FAILED', health_status='UNHEALTHY',
		        finished_at=now(), started_at=COALESCE(started_at, now()), version=version+1
		  WHERE id=$1`, dep.ID); err != nil {
		return fmt.Errorf("mark failed: %w", err)
	}
	if err := outbox.Write(ctx, tx, events.New(events.DeploymentFailed, "deployment", dep.ID,
		map[string]any{
			"environment_id": dep.EnvironmentID.String(),
			"revision":       dep.DesiredRevision,
			"reason":         cause.Error(),
		})); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit fail: %w", err)
	}

	dep.State = domain.DeployFailed
	if s.metrics != nil && env != nil {
		s.metrics.ObserveDeployment(env.Name, "FAILED", time.Since(started))
	}
	return cause
}

func (s *Service) emit(ctx context.Context, dep *Deployment, t events.Type, payload map[string]any) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := outbox.Write(ctx, tx, events.New(t, "deployment", dep.ID, payload)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
