// Package pipelines owns pipeline definitions and the planning of a definition into the
// concrete set of jobs that a run will execute.
//
// A definition is a DAG, not a list. Real delivery pipelines fan out — lint, unit tests
// and a dependency scan have no reason to wait for one another — and a control plane
// that models them as a sequence throws away most of the available parallelism. The
// planner here turns the declared dependency edges into execution waves and, more
// importantly, refuses definitions that could never terminate.
package pipelines

import (
	"fmt"
	"sort"
	"strings"
)

type StepType string

const (
	StepShell     StepType = "SHELL"
	StepBuild     StepType = "BUILD"
	StepContainer StepType = "CONTAINER"
	StepScan      StepType = "SCAN"
	StepDeploy    StepType = "DEPLOY"
	StepVerify    StepType = "VERIFY"
	StepPromote   StepType = "PROMOTE"
)

// RetryPolicy bounds how hard the engine tries before giving up on a step.
type RetryPolicy struct {
	MaxAttempts   int
	BackoffBase   int // seconds
	BackoffFactor float64
	MaxBackoff    int // seconds
	Jitter        bool
}

// DefaultRetryPolicy is applied to steps that do not declare one. One attempt means no
// retry: steps opt in to being retried, because retrying a non-idempotent step by
// default is how a delivery system double-deploys.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{MaxAttempts: 1, BackoffBase: 2, BackoffFactor: 2, MaxBackoff: 60, Jitter: true}
}

type StepDef struct {
	Key       string
	Name      string
	Type      StepType
	DependsOn []string
	Command   string
	Image     string
	// BuildContext and Dockerfile drive the containerise step. They are on the step
	// rather than on the project because a monorepo builds several images from one
	// repository, each from its own context.
	BuildContext   string
	Dockerfile     string
	TimeoutSeconds int
	Retry          *RetryPolicy
	Env            map[string]string
	// ContinueOnError lets a step fail without failing the run. Used for advisory
	// steps such as an informational scan.
	ContinueOnError bool
}

func (s StepDef) RetryPolicyOrDefault() RetryPolicy {
	if s.Retry == nil {
		return DefaultRetryPolicy()
	}
	return *s.Retry
}

type Definition struct {
	Key   string
	Name  string
	Steps []StepDef
}

// CycleError names the steps that form a dependency cycle, so the API can tell the user
// which steps to look at rather than only that the pipeline is invalid.
type CycleError struct{ Cycle []string }

func (e *CycleError) Error() string {
	return fmt.Sprintf("dependency cycle among steps: %s", strings.Join(e.Cycle, " -> "))
}

// PlannedJob is one job the run will create, together with whether it starts blocked.
type PlannedJob struct {
	StepKey   string
	Name      string
	Type      StepType
	DependsOn []string
	Blocked   bool
	Wave      int
}

// Validate checks the structural rules a definition must satisfy before any run is
// created from it. Rejecting here is much cheaper than discovering a cycle halfway
// through an execution that can never finish.
func (d Definition) Validate() error {
	if len(d.Steps) == 0 {
		return fmt.Errorf("pipeline %q: a pipeline must declare at least one step", d.Key)
	}

	index := make(map[string]StepDef, len(d.Steps))
	for _, s := range d.Steps {
		if strings.TrimSpace(s.Key) == "" {
			return fmt.Errorf("pipeline %q: every step must have a key", d.Key)
		}
		if _, dup := index[s.Key]; dup {
			return fmt.Errorf("pipeline %q: duplicate step key %q", d.Key, s.Key)
		}
		index[s.Key] = s
	}

	for _, s := range d.Steps {
		for _, dep := range s.DependsOn {
			if dep == s.Key {
				return fmt.Errorf("pipeline %q: step %q depends on itself", d.Key, s.Key)
			}
			if _, ok := index[dep]; !ok {
				return fmt.Errorf("pipeline %q: step %q depends on undeclared step %q", d.Key, s.Key, dep)
			}
		}
	}

	// A DAG has a cycle exactly when a topological sort cannot consume every node, so
	// Waves doubles as cycle detection.
	if _, err := d.waves(index); err != nil {
		return err
	}
	return nil
}

// Waves returns the steps grouped into execution waves: everything in wave N may run
// concurrently once every step in waves 0..N-1 has finished.
//
// Waves are a planning view, not the scheduler's runtime model. The scheduler does not
// wait for a whole wave — it releases each job the moment that job's own dependencies
// are satisfied. Waves exist because they are the right shape for rendering the graph
// and for reasoning about the critical path.
func (d Definition) Waves() ([][]string, error) {
	index := make(map[string]StepDef, len(d.Steps))
	for _, s := range d.Steps {
		index[s.Key] = s
	}
	return d.waves(index)
}

// waves is Kahn's algorithm, taking one full layer of zero-indegree nodes at a time.
//
// Every iteration sorts the frontier. Go randomises map iteration deliberately, so
// without this the same definition would produce differently ordered waves on
// successive calls and the console would redraw the graph in a new arrangement on every
// poll. Determinism here is a user-visible property, not tidiness.
func (d Definition) waves(index map[string]StepDef) ([][]string, error) {
	indegree := make(map[string]int, len(index))
	dependents := make(map[string][]string, len(index))

	for key, s := range index {
		if _, ok := indegree[key]; !ok {
			indegree[key] = 0
		}
		for _, dep := range s.DependsOn {
			indegree[key]++
			dependents[dep] = append(dependents[dep], key)
		}
	}

	var frontier []string
	for key, deg := range indegree {
		if deg == 0 {
			frontier = append(frontier, key)
		}
	}
	sort.Strings(frontier)

	var out [][]string
	consumed := 0
	for len(frontier) > 0 {
		wave := make([]string, len(frontier))
		copy(wave, frontier)
		out = append(out, wave)
		consumed += len(wave)

		var next []string
		for _, key := range wave {
			deps := append([]string(nil), dependents[key]...)
			sort.Strings(deps)
			for _, dep := range deps {
				indegree[dep]--
				if indegree[dep] == 0 {
					next = append(next, dep)
				}
			}
		}
		sort.Strings(next)
		frontier = next
	}

	if consumed != len(index) {
		// Whatever is left still has an unsatisfied dependency, which for a finite graph
		// means those nodes depend on each other.
		var stuck []string
		for key, deg := range indegree {
			if deg > 0 {
				stuck = append(stuck, key)
			}
		}
		sort.Strings(stuck)
		return nil, &CycleError{Cycle: stuck}
	}

	return out, nil
}

// Plan expands the definition into the jobs a run will persist, in wave order, each
// marked with whether it starts blocked on upstream work.
func (d Definition) Plan() ([]PlannedJob, error) {
	if err := d.Validate(); err != nil {
		return nil, err
	}
	index := make(map[string]StepDef, len(d.Steps))
	for _, s := range d.Steps {
		index[s.Key] = s
	}
	waves, err := d.waves(index)
	if err != nil {
		return nil, err
	}

	var jobs []PlannedJob
	for wave, keys := range waves {
		for _, key := range keys {
			s := index[key]
			jobs = append(jobs, PlannedJob{
				StepKey:   s.Key,
				Name:      s.Name,
				Type:      s.Type,
				DependsOn: append([]string(nil), s.DependsOn...),
				Blocked:   len(s.DependsOn) > 0,
				Wave:      wave,
			})
		}
	}
	return jobs, nil
}

// Dependents returns the steps that declare a dependency on key, sorted. The scheduler
// uses this to decide which jobs to unblock when one finishes.
func (d Definition) Dependents(key string) []string {
	var out []string
	for _, s := range d.Steps {
		for _, dep := range s.DependsOn {
			if dep == key {
				out = append(out, s.Key)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

// Step looks a step up by key.
func (d Definition) Step(key string) (StepDef, bool) {
	for _, s := range d.Steps {
		if s.Key == key {
			return s, true
		}
	}
	return StepDef{}, false
}
