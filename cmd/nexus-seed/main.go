// Command nexus-seed populates a local NEXUS with a demonstration estate.
//
// The data it writes is real system state, not fixtures rendered by the console: runs
// have jobs with attempt histories and log lines, deployments have health-check
// evidence, and the audit trail records who did what. The console reads it through the
// same queries it uses for live traffic, because there is no other path.
//
// It is idempotent. Running it twice does not double the estate; it detects the seeded
// organisation and stops unless --reset is passed.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Keathan2609/nexus/internal/auth"
	"github.com/Keathan2609/nexus/internal/core/domain"
	"github.com/Keathan2609/nexus/internal/pipelines"
	"github.com/Keathan2609/nexus/internal/platform/config"
	"github.com/Keathan2609/nexus/internal/platform/database"
)

const (
	seedOrgSlug  = "nexus-demo"
	adminEmail   = "admin@nexus.local"
	devEmail     = "dev@nexus.local"
	viewerEmail  = "viewer@nexus.local"
	seedPassword = "nexus-demo-password"
)

func main() {
	reset := flag.Bool("reset", false, "delete the existing demo organisation before seeding")
	flag.Parse()

	if err := run(*reset); err != nil {
		fmt.Fprintf(os.Stderr, "nexus-seed: %v\n", err)
		os.Exit(1)
	}
}

func run(reset bool) error {
	cfg, err := config.Load("nexus-seed")
	if err != nil {
		return err
	}
	ctx := context.Background()

	if err := database.Migrate(ctx, cfg.DatabaseURL); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	db, err := database.Open(ctx, database.DefaultConfig(cfg.DatabaseURL))
	if err != nil {
		return err
	}
	defer db.Close()

	var existing domain.ID
	err = db.QueryRow(ctx, `SELECT id FROM organisations WHERE slug=$1`, seedOrgSlug).Scan(&existing)
	switch {
	case err == nil && !reset:
		fmt.Println("The demo estate already exists. Re-seed with --reset to replace it.")
		return nil
	case err == nil && reset:
		// ON DELETE CASCADE carries this through every dependent table, which is worth
		// having asserted: if a foreign key were missing, this would leave orphans.
		if _, err := db.Exec(ctx, `DELETE FROM organisations WHERE id=$1`, existing); err != nil {
			return fmt.Errorf("reset: %w", err)
		}
		fmt.Println("Removed the previous demo estate.")
	case !errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("check for existing estate: %w", err)
	}

	s := &seeder{db: db, ctx: ctx, rng: rand.New(rand.NewSource(20260903))}
	if err := s.seed(); err != nil {
		return err
	}

	fmt.Printf(`
NEXUS demo estate created.

  Console   http://localhost:3100
  API       http://localhost:8080

  Sign in with any of:
    %-22s %s   (full access)
    %-22s %s   (deploy and roll back, cannot delete)
    %-22s %s   (read only)
`, adminEmail, seedPassword, devEmail, seedPassword, viewerEmail, seedPassword)
	return nil
}

type seeder struct {
	db  *database.DB
	ctx context.Context
	rng *rand.Rand

	orgID   domain.ID
	adminID domain.ID
	devID   domain.ID
}

func (s *seeder) seed() error {
	if err := s.organisation(); err != nil {
		return err
	}

	projects := []struct {
		name, slug, desc, runtime, repo string
		port                            int
	}{
		{"Checkout API", "checkout-api", "Payment capture and order confirmation.", "GO", "acme/checkout-api", 8080},
		{"Ledger Service", "ledger-service", "Double-entry ledger and reconciliation.", "GO", "acme/ledger-service", 8081},
		{"Storefront Web", "storefront-web", "Customer-facing storefront.", "NODE", "acme/storefront-web", 3000},
	}

	for i, p := range projects {
		projectID, defID, err := s.project(p.name, p.slug, p.desc, p.runtime, p.repo, p.port)
		if err != nil {
			return err
		}
		envs, err := s.environments(projectID, p.slug, i)
		if err != nil {
			return err
		}
		if err := s.history(projectID, defID, envs, i); err != nil {
			return err
		}
	}

	if err := s.workers(); err != nil {
		return err
	}
	fmt.Println("Seeded 3 projects with environments, run history, deployments and audit records.")
	return nil
}

func (s *seeder) organisation() error {
	s.orgID = domain.NewID(domain.PrefixOrganisation)
	if _, err := s.db.Exec(s.ctx,
		`INSERT INTO organisations (id, name, slug) VALUES ($1,'Acme Engineering',$2)`,
		s.orgID, seedOrgSlug); err != nil {
		return fmt.Errorf("create organisation: %w", err)
	}

	hash, err := auth.HashPassword(seedPassword)
	if err != nil {
		return err
	}

	users := []struct {
		email, name, role string
		target            *domain.ID
	}{
		{adminEmail, "Ada Okafor", "ADMIN", &s.adminID},
		{devEmail, "Sam Reyes", "DEVELOPER", &s.devID},
		{viewerEmail, "Jo Mbeki", "VIEWER", nil},
	}
	for _, u := range users {
		id := domain.NewID(domain.PrefixUser)
		if _, err := s.db.Exec(s.ctx,
			`INSERT INTO users (id, organisation_id, email, name, password_hash, last_login_at)
			 VALUES ($1,$2,$3,$4,$5, now() - interval '2 hours')`,
			id, s.orgID, u.email, u.name, hash); err != nil {
			return fmt.Errorf("create user %s: %w", u.email, err)
		}
		if _, err := s.db.Exec(s.ctx,
			`INSERT INTO memberships (id, user_id, team_id, role) VALUES ($1,$2,NULL,$3)`,
			domain.NewID(domain.PrefixTeam), id, u.role); err != nil {
			return fmt.Errorf("grant role to %s: %w", u.email, err)
		}
		if u.target != nil {
			*u.target = id
		}
	}
	return nil
}

func (s *seeder) project(name, slug, desc, runtime, repo string, port int) (domain.ID, domain.ID, error) {
	projectID := domain.NewID(domain.PrefixProject)
	if _, err := s.db.Exec(s.ctx,
		`INSERT INTO projects (id, organisation_id, name, slug, description, runtime, created_by, created_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7, now() - interval '40 days')`,
		projectID, s.orgID, name, slug, desc, runtime, s.adminID); err != nil {
		return "", "", fmt.Errorf("create project: %w", err)
	}
	for _, table := range []string{"run_counters", "deployment_counters"} {
		if _, err := s.db.Exec(s.ctx,
			fmt.Sprintf(`INSERT INTO %s (project_id, next_number) VALUES ($1,1)`, table),
			projectID); err != nil {
			return "", "", err
		}
	}

	owner, repoName := splitRepo(repo)
	if _, err := s.db.Exec(s.ctx,
		`INSERT INTO repositories (id, project_id, provider, owner, name, clone_url, default_branch)
		 VALUES ($1,$2,'GITHUB',$3,$4,$5,'main')`,
		domain.NewID(domain.PrefixRepository), projectID, owner, repoName,
		"https://github.com/"+repo); err != nil {
		return "", "", err
	}
	if _, err := s.db.Exec(s.ctx,
		`INSERT INTO services (id, project_id, name, port, health_path)
		 VALUES ($1,$2,$3,$4,'/healthz')`,
		domain.NewID(domain.PrefixService), projectID, slug, port); err != nil {
		return "", "", err
	}

	spec, err := pipelines.MarshalSpec(demoPipeline(runtime))
	if err != nil {
		return "", "", err
	}
	defID := domain.NewID(domain.PrefixPipeline)
	if _, err := s.db.Exec(s.ctx,
		`INSERT INTO pipeline_definitions (id, project_id, key, name, spec, version)
		 VALUES ($1,$2,'ci','Continuous delivery',$3,1)`,
		defID, projectID, spec); err != nil {
		return "", "", err
	}

	if err := s.audit(projectID, "project.created", "project", projectID.String(), s.adminID,
		map[string]any{"name": name}, 40*24*time.Hour); err != nil {
		return "", "", err
	}
	if err := s.audit(projectID, "repository.connected", "repository", repo, s.adminID,
		map[string]any{"provider": "GITHUB"}, 40*24*time.Hour); err != nil {
		return "", "", err
	}
	return projectID, defID, nil
}

// demoPipeline is the definition every seeded project uses.
//
// The commands are deliberately trivial and portable: the point of the demo is the
// control plane's behaviour, not the build tool, and a seeded pipeline that needs a Go
// or Node toolchain installed would fail on a clean machine for reasons that say
// nothing about NEXUS.
func demoPipeline(runtime string) pipelines.Definition {
	retry := pipelines.DefaultRetryPolicy()
	retry.MaxAttempts = 3

	echo := func(msg string, seconds int) string {
		if seconds <= 0 {
			return "echo " + msg
		}
		return fmt.Sprintf("echo %s && sleep %d", msg, seconds)
	}

	return pipelines.Definition{
		Key: "ci", Name: "Continuous delivery",
		Steps: []pipelines.StepDef{
			{Key: "checkout", Name: "checkout", Type: pipelines.StepShell,
				Command: echo("fetching sources", 1), TimeoutSeconds: 60},
			{Key: "lint", Name: "lint", Type: pipelines.StepShell, DependsOn: []string{"checkout"},
				Command: echo("linting", 1), TimeoutSeconds: 120},
			{Key: "test", Name: "test", Type: pipelines.StepShell, DependsOn: []string{"checkout"},
				Command: echo("running tests", 2), TimeoutSeconds: 300, Retry: &retry},
			{Key: "security-scan", Name: "security-scan", Type: pipelines.StepScan,
				DependsOn: []string{"checkout"}, Command: echo("scanning dependencies", 1),
				TimeoutSeconds: 180},
			{Key: "build", Name: "build", Type: pipelines.StepBuild,
				DependsOn: []string{"lint", "test", "security-scan"},
				Command:   echo("compiling "+runtime, 2), TimeoutSeconds: 600},
			{Key: "containerise", Name: "containerise", Type: pipelines.StepContainer,
				DependsOn: []string{"build"}, Command: echo("building image", 1),
				TimeoutSeconds: 600},
			{Key: "deploy-staging", Name: "deploy-staging", Type: pipelines.StepDeploy,
				DependsOn: []string{"containerise"}, TimeoutSeconds: 600},
			{Key: "smoke-test", Name: "smoke-test", Type: pipelines.StepVerify,
				DependsOn: []string{"deploy-staging"}, TimeoutSeconds: 300},
			{Key: "promote", Name: "promote", Type: pipelines.StepPromote,
				DependsOn: []string{"smoke-test"}, TimeoutSeconds: 300},
		},
	}
}

type seededEnv struct {
	id   domain.ID
	name string
	kind string
}

func (s *seeder) environments(projectID domain.ID, slug string, index int) ([]seededEnv, error) {
	defs := []struct{ name, kind string }{
		{"development", "DEVELOPMENT"},
		{"staging", "STAGING"},
		{"production", "PRODUCTION"},
	}
	var out []seededEnv
	for _, d := range defs {
		id := domain.NewID(domain.PrefixEnvironment)
		url := fmt.Sprintf("http://%s.%s.local", slug, d.name)
		if _, err := s.db.Exec(s.ctx,
			`INSERT INTO environments (id, project_id, name, kind, namespace, url, created_at)
			 VALUES ($1,$2,$3,$4,$5,$6, now() - interval '38 days')`,
			id, projectID, d.name, d.kind, fmt.Sprintf("%s-%s", slug, d.name), url); err != nil {
			return nil, fmt.Errorf("create environment: %w", err)
		}
		out = append(out, seededEnv{id: id, name: d.name, kind: d.kind})
	}

	// The first project also carries a live preview environment, so the pull-request
	// flow is visible without waiting for a webhook.
	if index == 0 {
		id := domain.NewID(domain.PrefixEnvironment)
		if _, err := s.db.Exec(s.ctx,
			`INSERT INTO environments
			   (id, project_id, name, kind, namespace, url, pull_request_number,
			    source_branch, expires_at, created_at)
			 VALUES ($1,$2,'pr-184','PREVIEW',$3,$4,184,'feat/idempotent-capture',
			         now() + interval '18 hours', now() - interval '6 hours')`,
			id, projectID, slug+"-pr-184",
			fmt.Sprintf("http://%s-pr-184.preview.local", slug)); err != nil {
			return nil, fmt.Errorf("create preview environment: %w", err)
		}
		out = append(out, seededEnv{id: id, name: "pr-184", kind: "PREVIEW"})
	}
	return out, nil
}

// history writes the run and deployment record for one project.
//
// The shape is chosen so the console has something honest to show in every state it can
// render: successes, one failure with a real error on a real step, a run still in
// flight, and a production deployment that was rolled back.
func (s *seeder) history(projectID, defID domain.ID, envs []seededEnv, index int) error {
	def := demoPipeline("GO")
	staging := envByKind(envs, "STAGING")
	production := envByKind(envs, "PRODUCTION")

	runCount := 12 + index*3

	// Work out the outcome of every run up front, because the deployment records
	// depend on knowing which run is the newest successful one. Only that run's
	// deployment may be left live: deployments_one_live_per_environment is a partial
	// unique index, so seeding two HEALTHY deployments into one environment is
	// rejected by the database exactly as a double promotion would be in production.
	outcomes := make([]string, runCount)
	lastSuccess, lastPromoted := -1, -1
	for i := 0; i < runCount; i++ {
		switch {
		case i == runCount-1 && index == 0:
			outcomes[i] = "RUNNING"
		case i == runCount-3 && index == 0:
			outcomes[i] = "FAILED"
		case i%9 == 4:
			outcomes[i] = "FAILED"
		default:
			outcomes[i] = "SUCCEEDED"
			lastSuccess = i
			if i%3 == 0 {
				lastPromoted = i
			}
		}
	}

	for i := 0; i < runCount; i++ {
		age := time.Duration(runCount-i) * 7 * time.Hour
		outcome := outcomes[i]

		runID, number, err := s.run(projectID, defID, def, outcome, age, i)
		if err != nil {
			return err
		}

		if outcome == "SUCCEEDED" {
			release, artifact, err := s.release(projectID, runID, i, age)
			if err != nil {
				return err
			}
			stagingState := "SUPERSEDED"
			if i == lastSuccess {
				stagingState = "HEALTHY"
			}
			if err := s.deployment(projectID, staging, release, artifact, runID, stagingState, age, false); err != nil {
				return err
			}
			// Only some staging deployments are promoted to production, which is what
			// makes the production revision meaningfully lag staging.
			if i%3 == 0 {
				state := "SUPERSEDED"
				if i == lastPromoted {
					state = "HEALTHY"
				}
				if err := s.deployment(projectID, production, release, artifact, runID, state, age, false); err != nil {
					return err
				}
				if err := s.audit(projectID, "deployment.requested", "deployment", release.String(),
					s.devID, map[string]any{"environment": "production", "run": number}, age); err != nil {
					return err
				}
			}
		}
	}

	// One rolled-back production deployment per project, with the audit record that
	// goes with it.
	if index == 1 {
		release, artifact, err := s.release(projectID, "", 99, 30*time.Hour)
		if err != nil {
			return err
		}
		if err := s.deployment(projectID, production, release, artifact, "", "ROLLED_BACK", 30*time.Hour, true); err != nil {
			return err
		}
		if err := s.audit(projectID, "rollback.requested", "deployment", release.String(), s.devID,
			map[string]any{"reason": "health verification failed after deploy", "environment": "production"},
			30*time.Hour); err != nil {
			return err
		}
	}
	return nil
}

func (s *seeder) run(projectID, defID domain.ID, def pipelines.Definition, outcome string, age time.Duration, index int) (domain.ID, int64, error) {
	var number int64
	if err := s.db.QueryRow(s.ctx,
		`UPDATE run_counters SET next_number = next_number + 1
		  WHERE project_id=$1 RETURNING next_number - 1`, projectID).Scan(&number); err != nil {
		return "", 0, err
	}

	runID := domain.NewID(domain.PrefixRun)
	sha := randomSHA(s.rng)
	branch, trigger, pr := "main", "PUSH", any(nil)
	if index%7 == 3 {
		branch, trigger, pr = "feat/idempotent-capture", "PULL_REQUEST", 184
	}

	duration := time.Duration(160+s.rng.Intn(220)) * time.Second
	started := time.Now().Add(-age)
	var finished any
	if outcome != "RUNNING" {
		finished = started.Add(duration)
	}

	if _, err := s.db.Exec(s.ctx,
		`INSERT INTO pipeline_runs
		   (id, project_id, definition_id, number, state, trigger, triggered_by,
		    commit_sha, commit_message, commit_author, branch, pull_request_number,
		    queued_at, started_at, finished_at, created_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$13,$14,$13)`,
		runID, projectID, defID, number, outcome, trigger, s.devID,
		sha, commitMessages[index%len(commitMessages)], "sam", branch, pr,
		started, finished); err != nil {
		return "", 0, fmt.Errorf("insert run: %w", err)
	}

	planned, err := def.Plan()
	if err != nil {
		return "", 0, err
	}
	// The step a failing run fails on. Choosing a real step rather than the last one
	// means the skipped-downstream behaviour is visible in the console.
	failAt := "test"
	if index%2 == 0 {
		failAt = "security-scan"
	}

	failedYet := false
	cursor := started
	for _, pj := range planned {
		step, _ := def.Step(pj.StepKey)
		state := "SUCCEEDED"
		var errCode, errMsg any
		stepDur := time.Duration(4+s.rng.Intn(40)) * time.Second

		switch {
		case outcome == "FAILED" && pj.StepKey == failAt:
			state, failedYet = "FAILED", true
			errCode = "E_EXIT"
			errMsg = failureMessages[index%len(failureMessages)]
		case outcome == "FAILED" && failedYet:
			state = "SKIPPED"
		case outcome == "RUNNING" && pj.Wave >= 2:
			state = "BLOCKED"
		case outcome == "RUNNING" && pj.Wave == 1:
			state = "RUNNING"
		}

		jobID := domain.NewID(domain.PrefixJob)
		attempt := 1
		// The test step occasionally needs a second attempt, which gives the attempt
		// history in the console something real to show.
		if pj.StepKey == "test" && index%5 == 2 {
			attempt = 2
		}
		maxAttempts := step.RetryPolicyOrDefault().MaxAttempts

		dependsOn := pj.DependsOn
		if dependsOn == nil {
			dependsOn = []string{}
		}

		var jobStart, jobFinish any
		if state != "BLOCKED" {
			jobStart = cursor
			if state != "RUNNING" {
				jobFinish = cursor.Add(stepDur)
			}
		}

		// The jobs_running_has_lease constraint requires a RUNNING job to be owned by a
		// worker and to hold an unexpired lease. Honouring that here rather than
		// disabling the constraint keeps the seeded estate in a state the reconciler
		// would accept: a seeded RUNNING job looks exactly like a real one, and would
		// be reaped correctly if its lease lapsed.
		var workerID, leaseExpires any
		if state == "RUNNING" {
			workerID = "wkr_demo_1"
			leaseExpires = time.Now().Add(30 * time.Second)
		}

		if _, err := s.db.Exec(s.ctx,
			`INSERT INTO jobs
			   (id, run_id, project_id, step_key, name, type, state, depends_on, wave,
			    attempt, max_attempts, timeout_seconds, spec, error_code, error_message,
			    started_at, finished_at, created_at, worker_id, lease_expires_at, heartbeat_at)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$16)`,
			jobID, runID, projectID, step.Key, step.Name, string(step.Type), state,
			dependsOn, pj.Wave, attemptOrZero(state, attempt), maxAttempts,
			step.TimeoutSeconds, specJSON(step), errCode, errMsg,
			jobStart, jobFinish, started, workerID, leaseExpires); err != nil {
			return "", 0, fmt.Errorf("insert job %s: %w", step.Key, err)
		}

		if state != "BLOCKED" {
			for a := 1; a <= attemptOrZero(state, attempt); a++ {
				aState := "SUCCEEDED"
				var aCode, aMsg, reason any = nil, nil, "INITIAL"
				if a < attempt {
					aState, aCode, aMsg = "FAILED", "E_EXIT", "flaky integration test timed out"
				} else if state == "FAILED" {
					aState, aCode, aMsg = "FAILED", errCode, errMsg
				} else if state == "RUNNING" {
					aState = "RUNNING"
				}
				if a > 1 {
					reason = "RETRY"
				}
				if _, err := s.db.Exec(s.ctx,
					`INSERT INTO job_attempts
					   (id, job_id, run_id, attempt, state, worker_id, error_code,
					    error_message, retry_reason, started_at, finished_at)
					 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
					domain.NewID(domain.PrefixAttempt), jobID, runID, a, aState,
					fmt.Sprintf("wkr_demo_%d", a%3), aCode, aMsg, reason,
					cursor, jobFinish); err != nil {
					return "", 0, err
				}
			}
			for n, line := range logLinesFor(step.Key, state) {
				if _, err := s.db.Exec(s.ctx,
					`INSERT INTO job_logs (job_id, attempt, seq, stream, message, logged_at)
					 VALUES ($1,$2,$3,$4,$5,$6)`,
					jobID, attemptOrZero(state, attempt), n+1, line.stream, line.text,
					cursor.Add(time.Duration(n)*time.Second)); err != nil {
					return "", 0, err
				}
			}
			cursor = cursor.Add(stepDur)
		}
	}

	if trigger == "PULL_REQUEST" {
		_ = s.audit(projectID, "pipeline.triggered", "pipeline_run", runID.String(), s.devID,
			map[string]any{"number": number, "branch": branch}, age)
	}
	return runID, number, nil
}

func (s *seeder) release(projectID, runID domain.ID, index int, age time.Duration) (domain.ID, domain.ID, error) {
	artifactID := domain.NewID(domain.PrefixArtifact)
	releaseID := domain.NewID(domain.PrefixRelease)
	version := fmt.Sprintf("1.%d.%d", index/5, index%5)
	digest := "sha256:" + randomSHA(s.rng) + randomSHA(s.rng)

	var run any
	if runID != "" {
		run = runID
	}
	if _, err := s.db.Exec(s.ctx,
		`INSERT INTO artifacts (id, project_id, run_id, kind, reference, digest, size_bytes, created_at)
		 VALUES ($1,$2,$3,'CONTAINER_IMAGE',$4,$5,$6, now() - $7::interval)`,
		artifactID, projectID, run,
		fmt.Sprintf("ghcr.io/acme/service:%s", version), digest,
		18_000_000+s.rng.Intn(40_000_000), age.String()); err != nil {
		return "", "", fmt.Errorf("insert artifact: %w", err)
	}
	if _, err := s.db.Exec(s.ctx,
		`INSERT INTO releases (id, project_id, version, commit_sha, artifact_id, run_id, created_at)
		 VALUES ($1,$2,$3,$4,$5,$6, now() - $7::interval)`,
		releaseID, projectID, version, randomSHA(s.rng), artifactID, run, age.String()); err != nil {
		return "", "", fmt.Errorf("insert release: %w", err)
	}
	return releaseID, artifactID, nil
}

func (s *seeder) deployment(projectID domain.ID, env seededEnv, releaseID, artifactID, runID domain.ID, state string, age time.Duration, rollback bool) error {
	if env.id == "" {
		return nil
	}
	var number int64
	if err := s.db.QueryRow(s.ctx,
		`UPDATE deployment_counters SET next_number = next_number + 1
		  WHERE project_id=$1 RETURNING next_number - 1`, projectID).Scan(&number); err != nil {
		return err
	}

	var version string
	if err := s.db.QueryRow(s.ctx,
		`SELECT version FROM releases WHERE id=$1`, releaseID).Scan(&version); err != nil {
		return err
	}
	revision := version + "@" + randomSHA(s.rng)[:7]

	health, sync := "HEALTHY", "SYNCED"
	if state == "ROLLED_BACK" {
		health, sync = "UNHEALTHY", "OUT_OF_SYNC"
	}
	var run any
	if runID != "" {
		run = runID
	}
	depID := domain.NewID(domain.PrefixDeployment)

	if _, err := s.db.Exec(s.ctx,
		`INSERT INTO deployments
		   (id, project_id, environment_id, release_id, run_id, number, state,
		    desired_revision, actual_revision, sync_status, health_status,
		    requested_by, reason, requested_at, started_at, verified_at, finished_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,
		         now() - $14::interval, now() - $14::interval,
		         now() - $14::interval + interval '40 seconds',
		         now() - $14::interval + interval '52 seconds')`,
		depID, projectID, env.id, releaseID, run, number, state,
		revision, revision, sync, health, s.devID,
		reasonFor(state, rollback), age.String()); err != nil {
		return fmt.Errorf("insert deployment: %w", err)
	}

	// Health-check evidence. A deployment that claims to be healthy without probe rows
	// behind it would be exactly the thing this project argues against.
	passes := 3
	if state == "ROLLED_BACK" {
		passes = 0
	}
	for a := 1; a <= 4; a++ {
		status, code := "PASS", 200
		if a <= 4-passes {
			status, code = "FAIL", 503
		}
		msg := ""
		if status == "FAIL" {
			msg = "service returned 503 during readiness"
		}
		if _, err := s.db.Exec(s.ctx,
			`INSERT INTO health_checks
			   (id, deployment_id, target, attempt, status, http_status, latency_ms, message, checked_at)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8, now() - $9::interval)`,
			domain.NewID(domain.PrefixHealthCheck), depID,
			fmt.Sprintf("http://%s.local/healthz", env.name), a, status, code,
			3+s.rng.Intn(28), msg, age.String()); err != nil {
			return err
		}
	}

	manifests := fmt.Sprintf("# desired state for %s at %s\nrevision: %s\n", env.name, revision, revision)
	if _, err := s.db.Exec(s.ctx,
		`INSERT INTO gitops_revisions
		   (id, project_id, environment_id, deployment_id, revision, manifest_hash, manifests, created_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7, now() - $8::interval)
		 ON CONFLICT (environment_id, revision) DO NOTHING`,
		domain.NewID(domain.PrefixArtifact), projectID, env.id, depID, revision,
		randomSHA(s.rng)+randomSHA(s.rng), manifests, age.String()); err != nil {
		return err
	}
	return nil
}

func (s *seeder) workers() error {
	for i := 1; i <= 3; i++ {
		state, seen := "ACTIVE", "5 seconds"
		if i == 3 {
			state, seen = "LOST", "4 minutes"
		}
		if _, err := s.db.Exec(s.ctx,
			`INSERT INTO workers (id, hostname, version, concurrency, active_jobs, state,
			                      started_at, last_heartbeat_at)
			 VALUES ($1,$2,'1.0.0',4,$3,$4, now() - interval '3 hours', now() - $5::interval)
			 ON CONFLICT (id) DO NOTHING`,
			fmt.Sprintf("wkr_demo_%d", i), fmt.Sprintf("nexus-worker-%d", i),
			i%3, state, seen); err != nil {
			return err
		}
	}
	return nil
}

func (s *seeder) audit(projectID domain.ID, action, resourceType, resourceID string, actor domain.ID, meta map[string]any, age time.Duration) error {
	blob, err := jsonBytes(meta)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(s.ctx,
		`INSERT INTO audit_events
		   (id, organisation_id, actor_id, actor_type, actor_label, action,
		    resource_type, resource_id, project_id, correlation_id, metadata, occurred_at)
		 VALUES ($1,$2,$3,'USER',$4,$5,$6,$7,$8,'', $9, now() - $10::interval)`,
		domain.NewID(domain.PrefixAudit), s.orgID, actor,
		labelFor(actor, s.adminID), action, resourceType, resourceID, projectID,
		blob, age.String())
	if err != nil {
		return fmt.Errorf("insert audit event: %w", err)
	}
	return nil
}
