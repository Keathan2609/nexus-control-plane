package pipelines_test

import (
	"errors"
	"testing"

	"github.com/Keathan2609/nexus/internal/pipelines"
)

func def(steps ...pipelines.StepDef) pipelines.Definition {
	return pipelines.Definition{Key: "ci", Name: "CI", Steps: steps}
}

func step(key string, deps ...string) pipelines.StepDef {
	return pipelines.StepDef{Key: key, Name: key, Type: pipelines.StepShell, DependsOn: deps}
}

func TestValidateRejectsEmptyDefinition(t *testing.T) {
	if err := def().Validate(); err == nil {
		t.Fatal("a pipeline with no steps must be rejected")
	}
}

func TestValidateRejectsDuplicateStepKeys(t *testing.T) {
	err := def(step("build"), step("build")).Validate()
	if err == nil {
		t.Fatal("duplicate step keys must be rejected")
	}
}

func TestValidateRejectsUnknownDependency(t *testing.T) {
	err := def(step("build", "nonexistent")).Validate()
	if err == nil {
		t.Fatal("a dependency on an undeclared step must be rejected")
	}
}

func TestValidateRejectsSelfDependency(t *testing.T) {
	err := def(step("build", "build")).Validate()
	if err == nil {
		t.Fatal("a step depending on itself must be rejected")
	}
}

func TestValidateDetectsCycle(t *testing.T) {
	// a -> b -> c -> a
	err := def(
		step("a", "c"),
		step("b", "a"),
		step("c", "b"),
	).Validate()
	if err == nil {
		t.Fatal("a dependency cycle must be rejected")
	}
	var cyc *pipelines.CycleError
	if !errors.As(err, &cyc) {
		t.Fatalf("expected a CycleError so the UI can name the cycle, got %T: %v", err, err)
	}
	if len(cyc.Cycle) == 0 {
		t.Fatal("CycleError must report the participating steps")
	}
}

func TestValidateAcceptsDiamond(t *testing.T) {
	//     lint
	//    /    \
	// checkout  -> build
	//    \    /
	//     test
	err := def(
		step("checkout"),
		step("lint", "checkout"),
		step("test", "checkout"),
		step("build", "lint", "test"),
	).Validate()
	if err != nil {
		t.Fatalf("a diamond is a valid DAG: %v", err)
	}
}

func TestPlanOrdersLinearChain(t *testing.T) {
	waves, err := def(
		step("checkout"),
		step("test", "checkout"),
		step("build", "test"),
	).Waves()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(waves) != 3 {
		t.Fatalf("a linear chain of 3 must produce 3 waves, got %d: %v", len(waves), waves)
	}
	if waves[0][0] != "checkout" || waves[1][0] != "test" || waves[2][0] != "build" {
		t.Fatalf("wrong order: %v", waves)
	}
}

func TestPlanGroupsIndependentStepsIntoOneWave(t *testing.T) {
	waves, err := def(
		step("checkout"),
		step("lint", "checkout"),
		step("test", "checkout"),
		step("scan", "checkout"),
		step("build", "lint", "test", "scan"),
	).Waves()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(waves) != 3 {
		t.Fatalf("expected 3 waves, got %d: %v", len(waves), waves)
	}
	if len(waves[1]) != 3 {
		t.Fatalf("lint, test and scan are independent and must share a wave, got %v", waves[1])
	}
}

func TestWavesAreDeterministic(t *testing.T) {
	// Ordering must not depend on Go map iteration order, or the UI would redraw the
	// graph differently on every request.
	d := def(
		step("checkout"),
		step("zeta", "checkout"),
		step("alpha", "checkout"),
		step("mid", "checkout"),
	)
	first, err := d.Waves()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for i := 0; i < 50; i++ {
		again, err := d.Waves()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		for w := range first {
			for s := range first[w] {
				if first[w][s] != again[w][s] {
					t.Fatalf("wave order is not deterministic: %v vs %v", first, again)
				}
			}
		}
	}
}

func TestInitialStatesBlockDependents(t *testing.T) {
	plan, err := def(
		step("checkout"),
		step("test", "checkout"),
	).Plan()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	byKey := map[string]pipelines.PlannedJob{}
	for _, j := range plan {
		byKey[j.StepKey] = j
	}
	if byKey["checkout"].Blocked {
		t.Fatal("a step with no dependencies must start unblocked (QUEUED)")
	}
	if !byKey["test"].Blocked {
		t.Fatal("a step with dependencies must start blocked")
	}
}

func TestDependentsLookup(t *testing.T) {
	d := def(
		step("checkout"),
		step("lint", "checkout"),
		step("test", "checkout"),
	)
	deps := d.Dependents("checkout")
	if len(deps) != 2 {
		t.Fatalf("checkout has 2 dependents, got %v", deps)
	}
}

func TestValidateRejectsBlankStepKey(t *testing.T) {
	if err := def(pipelines.StepDef{Key: "", Name: "x", Type: pipelines.StepShell}).Validate(); err == nil {
		t.Fatal("a blank step key must be rejected")
	}
}
