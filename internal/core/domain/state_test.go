package domain_test

import (
	"testing"

	"github.com/Keathan2609/nexus/internal/core/domain"
)

func TestRunStateLegalTransition(t *testing.T) {
	next, err := domain.RunQueued.TransitionTo(domain.RunRunning)
	if err != nil {
		t.Fatalf("QUEUED -> RUNNING should be legal, got error: %v", err)
	}
	if next != domain.RunRunning {
		t.Fatalf("expected RUNNING, got %s", next)
	}
}

func TestRunStateIllegalTransitionIsRejected(t *testing.T) {
	_, err := domain.RunSucceeded.TransitionTo(domain.RunRunning)
	if err == nil {
		t.Fatal("SUCCEEDED -> RUNNING must be rejected, got nil error")
	}
}

func TestRunStateCannotSkipRunning(t *testing.T) {
	_, err := domain.RunPending.TransitionTo(domain.RunSucceeded)
	if err == nil {
		t.Fatal("PENDING -> SUCCEEDED must be rejected: a run cannot succeed without running")
	}
}

func TestRunTerminalStatesAreTerminal(t *testing.T) {
	terminal := []domain.RunState{
		domain.RunSucceeded, domain.RunFailed, domain.RunCancelled, domain.RunTimedOut,
	}
	for _, s := range terminal {
		if !s.IsTerminal() {
			t.Errorf("%s must be terminal", s)
		}
		// A terminal state accepts no outgoing transition at all.
		for _, to := range domain.AllRunStates() {
			if _, err := s.TransitionTo(to); err == nil {
				t.Errorf("terminal %s must reject transition to %s", s, to)
			}
		}
	}
}

func TestRunNonTerminalStatesAreNotTerminal(t *testing.T) {
	for _, s := range []domain.RunState{domain.RunPending, domain.RunQueued, domain.RunRunning} {
		if s.IsTerminal() {
			t.Errorf("%s must not be terminal", s)
		}
	}
}

func TestRunCanBeCancelledBeforeCompletion(t *testing.T) {
	for _, s := range []domain.RunState{domain.RunPending, domain.RunQueued, domain.RunRunning} {
		if _, err := s.TransitionTo(domain.RunCancelled); err != nil {
			t.Errorf("%s -> CANCELLED must be legal: %v", s, err)
		}
	}
}

func TestJobRetryReturnsToQueued(t *testing.T) {
	// A failed attempt that still has budget goes back to QUEUED for another attempt.
	if _, err := domain.JobRunning.TransitionTo(domain.JobQueued); err != nil {
		t.Fatalf("RUNNING -> QUEUED (retry) must be legal: %v", err)
	}
}

func TestJobBlockedBecomesQueuedWhenDependenciesSatisfied(t *testing.T) {
	if _, err := domain.JobBlocked.TransitionTo(domain.JobQueued); err != nil {
		t.Fatalf("BLOCKED -> QUEUED must be legal: %v", err)
	}
}

func TestJobBlockedCanBeSkipped(t *testing.T) {
	// When an upstream job fails, downstream blocked jobs are skipped, not failed.
	if _, err := domain.JobBlocked.TransitionTo(domain.JobSkipped); err != nil {
		t.Fatalf("BLOCKED -> SKIPPED must be legal: %v", err)
	}
}

func TestDeploymentMustVerifyBeforeHealthy(t *testing.T) {
	if _, err := domain.DeployDeploying.TransitionTo(domain.DeployHealthy); err == nil {
		t.Fatal("DEPLOYING -> HEALTHY must be rejected: health must be verified first")
	}
	if _, err := domain.DeployVerifying.TransitionTo(domain.DeployHealthy); err != nil {
		t.Fatalf("VERIFYING -> HEALTHY must be legal: %v", err)
	}
}

func TestDeploymentRollbackPath(t *testing.T) {
	if _, err := domain.DeployFailed.TransitionTo(domain.DeployRollingBack); err != nil {
		t.Fatalf("FAILED -> ROLLING_BACK must be legal: %v", err)
	}
	if _, err := domain.DeployRollingBack.TransitionTo(domain.DeployRolledBack); err != nil {
		t.Fatalf("ROLLING_BACK -> ROLLED_BACK must be legal: %v", err)
	}
}

func TestDeploymentDegradedCanRecoverOrFail(t *testing.T) {
	if _, err := domain.DeployDegraded.TransitionTo(domain.DeployHealthy); err != nil {
		t.Fatalf("DEGRADED -> HEALTHY (recovery) must be legal: %v", err)
	}
	if _, err := domain.DeployDegraded.TransitionTo(domain.DeployFailed); err != nil {
		t.Fatalf("DEGRADED -> FAILED must be legal: %v", err)
	}
}

func TestInvalidTransitionErrorIdentifiesBothStates(t *testing.T) {
	_, err := domain.RunSucceeded.TransitionTo(domain.RunRunning)
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	if !contains(msg, "SUCCEEDED") || !contains(msg, "RUNNING") {
		t.Fatalf("error must name both states, got: %s", msg)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}
