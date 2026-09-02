// Package domain holds the vocabulary the rest of NEXUS is written in: identifiers,
// lifecycle states and the rules governing movement between them.
//
// State lives here, and only here, because a control plane whose states are bare
// strings compared at call sites will eventually compare them inconsistently. Every
// lifecycle in NEXUS is a closed set of states plus an explicit transition table, and
// the only way to move between them is TransitionTo, which refuses illegal moves.
package domain

import "fmt"

// InvalidTransitionError reports an attempt to move a lifecycle into a state it cannot
// legally reach from where it is. It names both ends so the error is actionable in a
// log line without needing the surrounding context.
type InvalidTransitionError struct {
	Lifecycle string
	From      string
	To        string
}

func (e *InvalidTransitionError) Error() string {
	return fmt.Sprintf("%s: illegal transition %s -> %s", e.Lifecycle, e.From, e.To)
}

// transition is the shared implementation behind every TransitionTo below. The table is
// keyed by current state; a state present with an empty set is terminal and accepts
// nothing.
func transition[S ~string](lifecycle string, table map[S][]S, from, to S) (S, error) {
	for _, allowed := range table[from] {
		if allowed == to {
			return to, nil
		}
	}
	var zero S
	return zero, &InvalidTransitionError{
		Lifecycle: lifecycle,
		From:      string(from),
		To:        string(to),
	}
}

func isTerminal[S ~string](table map[S][]S, s S) bool {
	return len(table[s]) == 0
}

// --- pipeline run -----------------------------------------------------------
//
// A run is the execution of a pipeline definition against one commit. It is PENDING
// while its DAG is planned, QUEUED once jobs exist, RUNNING while any job is in flight,
// and then exactly one terminal state.

type RunState string

const (
	RunPending   RunState = "PENDING"
	RunQueued    RunState = "QUEUED"
	RunRunning   RunState = "RUNNING"
	RunSucceeded RunState = "SUCCEEDED"
	RunFailed    RunState = "FAILED"
	RunCancelled RunState = "CANCELLED"
	RunTimedOut  RunState = "TIMED_OUT"
)

// runTransitions is the whole truth about how a run may move.
//
// Note what is absent: there is no PENDING -> SUCCEEDED edge. A run cannot report
// success without having run, even if its DAG turned out to be empty. That case still
// passes through RUNNING so duration and the job set are recorded honestly.
var runTransitions = map[RunState][]RunState{
	RunPending: {RunQueued, RunCancelled, RunFailed},
	RunQueued:  {RunRunning, RunCancelled, RunFailed, RunTimedOut},
	RunRunning: {RunSucceeded, RunFailed, RunCancelled, RunTimedOut},

	RunSucceeded: {},
	RunFailed:    {},
	RunCancelled: {},
	RunTimedOut:  {},
}

func AllRunStates() []RunState {
	return []RunState{
		RunPending, RunQueued, RunRunning,
		RunSucceeded, RunFailed, RunCancelled, RunTimedOut,
	}
}

func (s RunState) IsTerminal() bool { return isTerminal(runTransitions, s) }

func (s RunState) TransitionTo(next RunState) (RunState, error) {
	return transition("pipeline_run", runTransitions, s, next)
}

// IsSuccessful reports whether a terminal run state represents a good outcome. Used by
// metrics and by the promotion gate.
func (s RunState) IsSuccessful() bool { return s == RunSucceeded }

// --- job --------------------------------------------------------------------
//
// A job is one unit of work in the DAG of a run. BLOCKED means "waiting on upstream
// jobs" and is the initial state for anything with dependencies.

type JobState string

const (
	JobBlocked   JobState = "BLOCKED"
	JobQueued    JobState = "QUEUED"
	JobRunning   JobState = "RUNNING"
	JobSucceeded JobState = "SUCCEEDED"
	JobFailed    JobState = "FAILED"
	JobCancelled JobState = "CANCELLED"
	JobTimedOut  JobState = "TIMED_OUT"
	JobSkipped   JobState = "SKIPPED"
)

// jobTransitions encodes three decisions worth pointing at:
//
//   - RUNNING -> QUEUED is the retry edge. A failed attempt that still has budget does
//     not pass through FAILED and come back, which keeps FAILED meaning "this job is
//     finished and it did not work".
//   - BLOCKED -> SKIPPED exists because dependents of a failed job did not fail; they
//     never ran. Recording them as FAILED would make every failure count lie.
//   - TIMED_OUT -> QUEUED is legal: a timeout is a retryable outcome like any other.
var jobTransitions = map[JobState][]JobState{
	JobBlocked:  {JobQueued, JobSkipped, JobCancelled},
	JobQueued:   {JobRunning, JobCancelled, JobSkipped, JobTimedOut},
	JobRunning:  {JobSucceeded, JobFailed, JobQueued, JobCancelled, JobTimedOut},
	JobTimedOut: {JobQueued, JobFailed},

	JobSucceeded: {},
	JobFailed:    {},
	JobCancelled: {},
	JobSkipped:   {},
}

func AllJobStates() []JobState {
	return []JobState{
		JobBlocked, JobQueued, JobRunning, JobSucceeded,
		JobFailed, JobCancelled, JobTimedOut, JobSkipped,
	}
}

func (s JobState) IsTerminal() bool { return isTerminal(jobTransitions, s) }

func (s JobState) TransitionTo(next JobState) (JobState, error) {
	return transition("job", jobTransitions, s, next)
}

// --- deployment -------------------------------------------------------------
//
// This lifecycle carries the single most important rule in NEXUS: there is no
// DEPLOYING -> HEALTHY edge. Health is a verified observation, never an assumption that
// follows from a successful apply. The only route to HEALTHY runs through VERIFYING.

type DeploymentState string

const (
	DeployRequested    DeploymentState = "REQUESTED"
	DeployProvisioning DeploymentState = "PROVISIONING"
	DeployDeploying    DeploymentState = "DEPLOYING"
	DeployVerifying    DeploymentState = "VERIFYING"
	DeployHealthy      DeploymentState = "HEALTHY"
	DeployDegraded     DeploymentState = "DEGRADED"
	DeployFailed       DeploymentState = "FAILED"
	DeployRollingBack  DeploymentState = "ROLLING_BACK"
	DeployRolledBack   DeploymentState = "ROLLED_BACK"
	DeploySuperseded   DeploymentState = "SUPERSEDED"
)

var deploymentTransitions = map[DeploymentState][]DeploymentState{
	DeployRequested:    {DeployProvisioning, DeployFailed, DeployRolledBack},
	DeployProvisioning: {DeployDeploying, DeployFailed},
	DeployDeploying:    {DeployVerifying, DeployFailed},
	DeployVerifying:    {DeployHealthy, DeployDegraded, DeployFailed},
	// A healthy deployment can later degrade (the reconciler observed it), be rolled
	// back deliberately, or be superseded by a newer release.
	DeployHealthy:     {DeployDegraded, DeployRollingBack, DeploySuperseded, DeployFailed},
	DeployDegraded:    {DeployHealthy, DeployFailed, DeployRollingBack},
	DeployFailed:      {DeployRollingBack, DeploySuperseded},
	DeployRollingBack: {DeployRolledBack, DeployFailed},

	DeployRolledBack: {},
	DeploySuperseded: {},
}

func AllDeploymentStates() []DeploymentState {
	return []DeploymentState{
		DeployRequested, DeployProvisioning, DeployDeploying, DeployVerifying,
		DeployHealthy, DeployDegraded, DeployFailed, DeployRollingBack,
		DeployRolledBack, DeploySuperseded,
	}
}

func (s DeploymentState) IsTerminal() bool { return isTerminal(deploymentTransitions, s) }

func (s DeploymentState) TransitionTo(next DeploymentState) (DeploymentState, error) {
	return transition("deployment", deploymentTransitions, s, next)
}

// IsLive reports whether a deployment is currently serving, or trying to. Used to decide
// which deployment a new release supersedes.
func (s DeploymentState) IsLive() bool {
	return s == DeployHealthy || s == DeployDegraded
}
