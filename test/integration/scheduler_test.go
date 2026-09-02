//go:build integration

package integration

import (
	"context"
	"testing"

	"github.com/Keathan2609/nexus/internal/pipelines"
	"github.com/Keathan2609/nexus/internal/scheduler"
	"github.com/Keathan2609/nexus/internal/store/postgres"
)

// ciDefinition is the canonical shape: a linear head, a parallel middle, a join.
//
//	checkout -> {lint, test, scan} -> build -> deploy
func ciDefinition() pipelines.Definition {
	return pipelines.Definition{
		Key: "ci", Name: "CI",
		Steps: []pipelines.StepDef{
			{Key: "checkout", Name: "checkout", Type: pipelines.StepShell},
			{Key: "lint", Name: "lint", Type: pipelines.StepShell, DependsOn: []string{"checkout"}},
			{Key: "test", Name: "test", Type: pipelines.StepShell, DependsOn: []string{"checkout"}},
			{Key: "scan", Name: "scan", Type: pipelines.StepScan, DependsOn: []string{"checkout"}},
			{Key: "build", Name: "build", Type: pipelines.StepBuild, DependsOn: []string{"lint", "test", "scan"}},
			{Key: "deploy", Name: "deploy", Type: pipelines.StepDeploy, DependsOn: []string{"build"}},
		},
	}
}

func TestPlanningCreatesJobsWithCorrectInitialStates(t *testing.T) {
	db := openDB(t)
	f := newFixture(t, db)
	runs := postgres.NewRunStore(db)
	ctx := context.Background()

	run, err := runs.CreateRun(ctx, postgres.CreateRunParams{
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

	states := jobStatesByStep(t, db, run.ID)
	if states["checkout"] != "QUEUED" {
		t.Errorf("checkout has no dependencies and must start QUEUED, got %s", states["checkout"])
	}
	for _, blocked := range []string{"lint", "test", "scan", "build", "deploy"} {
		if states[blocked] != "BLOCKED" {
			t.Errorf("%s depends on upstream work and must start BLOCKED, got %s", blocked, states[blocked])
		}
	}
}

func TestRunNumbersAreSequentialPerProject(t *testing.T) {
	db := openDB(t)
	f := newFixture(t, db)
	runs := postgres.NewRunStore(db)
	ctx := context.Background()

	var numbers []int64
	for i := 0; i < 3; i++ {
		run, err := runs.CreateRun(ctx, postgres.CreateRunParams{
			ProjectID: f.ProjectID, DefinitionID: f.DefinitionID,
			Definition: ciDefinition(), Trigger: "MANUAL",
			CommitSHA: "a84fc12", Branch: "main",
		})
		if err != nil {
			t.Fatalf("create run %d: %v", i, err)
		}
		numbers = append(numbers, run.Number)
	}
	// The fixture already consumed number 1 for its seeded run, so assert the shape
	// rather than the absolute values.
	for i := 1; i < len(numbers); i++ {
		if numbers[i] != numbers[i-1]+1 {
			t.Fatalf("run numbers must be consecutive, got %v", numbers)
		}
	}
}

// Completing a job must release exactly the dependents whose dependencies are now all
// satisfied, and no others.
func TestCompletingAJobUnblocksOnlyItsSatisfiedDependents(t *testing.T) {
	db := openDB(t)
	f := newFixture(t, db)
	runs := postgres.NewRunStore(db)
	jobs := postgres.NewJobStore(db)
	sched := scheduler.New(db, runs, jobs)
	ctx := context.Background()

	run, err := runs.CreateRun(ctx, postgres.CreateRunParams{
		ProjectID: f.ProjectID, DefinitionID: f.DefinitionID,
		Definition: ciDefinition(), Trigger: "MANUAL",
		CommitSHA: "a84fc12", Branch: "main",
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	// Finish checkout.
	claimAndComplete(t, jobs, f, "wkr_a")
	if _, err := sched.Advance(ctx, run.ID); err != nil {
		t.Fatalf("advance: %v", err)
	}

	states := jobStatesByStep(t, db, run.ID)
	for _, released := range []string{"lint", "test", "scan"} {
		if states[released] != "QUEUED" {
			t.Errorf("%s depends only on checkout and must be released, got %s", released, states[released])
		}
	}
	if states["build"] != "BLOCKED" {
		t.Errorf("build still has unfinished dependencies and must stay BLOCKED, got %s", states["build"])
	}
}

func TestJoinStepWaitsForEveryDependency(t *testing.T) {
	db := openDB(t)
	f := newFixture(t, db)
	runs := postgres.NewRunStore(db)
	jobs := postgres.NewJobStore(db)
	sched := scheduler.New(db, runs, jobs)
	ctx := context.Background()

	run := mustCreateRun(t, runs, f)

	claimAndComplete(t, jobs, f, "wkr_a") // checkout
	_, _ = sched.Advance(ctx, run.ID)

	// Complete two of the three parallel steps.
	claimAndComplete(t, jobs, f, "wkr_a")
	claimAndComplete(t, jobs, f, "wkr_b")
	_, _ = sched.Advance(ctx, run.ID)

	if s := jobStatesByStep(t, db, run.ID)["build"]; s != "BLOCKED" {
		t.Fatalf("build must wait for all three dependencies, got %s", s)
	}

	claimAndComplete(t, jobs, f, "wkr_c") // the third
	_, _ = sched.Advance(ctx, run.ID)

	if s := jobStatesByStep(t, db, run.ID)["build"]; s != "QUEUED" {
		t.Fatalf("build must be released once every dependency succeeded, got %s", s)
	}
}

// A failed job does not fail its dependents — they never ran. They are SKIPPED, and the
// run as a whole fails.
func TestFailedJobSkipsDependentsAndFailsRun(t *testing.T) {
	db := openDB(t)
	f := newFixture(t, db)
	runs := postgres.NewRunStore(db)
	jobs := postgres.NewJobStore(db)
	sched := scheduler.New(db, runs, jobs)
	ctx := context.Background()

	run := mustCreateRun(t, runs, f)

	job, err := jobs.ClaimForProject(ctx, f.ProjectID, "wkr_a", leaseShort)
	if err != nil || job == nil {
		t.Fatalf("claim checkout: %v", err)
	}
	if _, err := jobs.Fail(ctx, job.ID, "wkr_a", "E_STEP", "checkout blew up", 0); err != nil {
		t.Fatalf("fail: %v", err)
	}

	if _, err := sched.Advance(ctx, run.ID); err != nil {
		t.Fatalf("advance: %v", err)
	}

	states := jobStatesByStep(t, db, run.ID)
	for _, step := range []string{"lint", "test", "scan", "build", "deploy"} {
		if states[step] != "SKIPPED" {
			t.Errorf("%s never ran and must be SKIPPED, not %s", step, states[step])
		}
	}
	if got := runState(t, db, run.ID); got != "FAILED" {
		t.Fatalf("run must be FAILED, got %s", got)
	}
}

func TestRunSucceedsWhenEveryJobSucceeds(t *testing.T) {
	db := openDB(t)
	f := newFixture(t, db)
	runs := postgres.NewRunStore(db)
	jobs := postgres.NewJobStore(db)
	sched := scheduler.New(db, runs, jobs)
	ctx := context.Background()

	run := mustCreateRun(t, runs, f)

	// Drain the whole DAG.
	for i := 0; i < 20; i++ {
		job, err := jobs.ClaimForProject(ctx, f.ProjectID, "wkr_a", leaseShort)
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		if job == nil {
			if _, err := sched.Advance(ctx, run.ID); err != nil {
				t.Fatalf("advance: %v", err)
			}
			if runState(t, db, run.ID) == "SUCCEEDED" {
				return
			}
			continue
		}
		if err := jobs.Complete(ctx, job.ID, "wkr_a"); err != nil {
			t.Fatalf("complete: %v", err)
		}
		if _, err := sched.Advance(ctx, run.ID); err != nil {
			t.Fatalf("advance: %v", err)
		}
	}
	t.Fatalf("run did not reach SUCCEEDED, ended in %s", runState(t, db, run.ID))
}

func TestCancellingRunCancelsOutstandingJobs(t *testing.T) {
	db := openDB(t)
	f := newFixture(t, db)
	runs := postgres.NewRunStore(db)
	jobs := postgres.NewJobStore(db)
	sched := scheduler.New(db, runs, jobs)
	ctx := context.Background()

	run := mustCreateRun(t, runs, f)

	if err := sched.Cancel(ctx, run.ID, "user asked"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if got := runState(t, db, run.ID); got != "CANCELLED" {
		t.Fatalf("run must be CANCELLED, got %s", got)
	}
	for step, state := range jobStatesByStep(t, db, run.ID) {
		if state != "CANCELLED" && state != "SKIPPED" {
			t.Errorf("job %s should not remain %s after cancellation", step, state)
		}
	}
}

func TestAdvanceIsIdempotent(t *testing.T) {
	// The scheduler runs on a tick and also in response to events, so it will be called
	// repeatedly for the same run. Calling it twice must not double-release anything.
	db := openDB(t)
	f := newFixture(t, db)
	runs := postgres.NewRunStore(db)
	jobs := postgres.NewJobStore(db)
	sched := scheduler.New(db, runs, jobs)
	ctx := context.Background()

	run := mustCreateRun(t, runs, f)
	claimAndComplete(t, jobs, f, "wkr_a")

	first, err := sched.Advance(ctx, run.ID)
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	second, err := sched.Advance(ctx, run.ID)
	if err != nil {
		t.Fatalf("advance again: %v", err)
	}
	if first.Released == 0 {
		t.Fatal("the first advance should have released the parallel steps")
	}
	if second.Released != 0 {
		t.Fatalf("a repeated advance must release nothing, released %d", second.Released)
	}
}
