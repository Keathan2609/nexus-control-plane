package workers

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/Keathan2609/nexus/internal/pipelines"
)

// ExitError reports a step whose process ran and returned a non-zero status. It is
// distinguished from other failures because it means the step itself failed, not that
// NEXUS failed to run it.
type ExitError struct {
	Code int
}

func (e *ExitError) Error() string { return fmt.Sprintf("step exited with status %d", e.Code) }

// StepSpec is the executable part of a job, stored on the job row at planning time.
type StepSpec struct {
	Command      string            `json:"command"`
	Image        string            `json:"image"`
	Env          map[string]string `json:"env"`
	BuildContext string            `json:"build_context"`
	Dockerfile   string            `json:"dockerfile"`
}

// Router dispatches a job to the executor registered for its step type.
//
// Types that have no registered executor are a configuration error, not a job failure,
// and are reported as such — silently succeeding an unimplemented step would let a
// pipeline claim it deployed something it never built.
type Router struct {
	executors map[pipelines.StepType]Executor
	fallback  Executor
}

func NewRouter(fallback Executor) *Router {
	return &Router{executors: map[pipelines.StepType]Executor{}, fallback: fallback}
}

func (r *Router) Register(t pipelines.StepType, e Executor) { r.executors[t] = e }

func (r *Router) Execute(ctx context.Context, ex Execution) error {
	if e, ok := r.executors[pipelines.StepType(ex.Job.Type)]; ok {
		return e.Execute(ctx, ex)
	}
	if r.fallback != nil {
		return r.fallback.Execute(ctx, ex)
	}
	return fmt.Errorf("no executor registered for step type %q", ex.Job.Type)
}

// ShellExecutor runs a step's command as a subprocess, streaming its output.
//
// Output is streamed line by line as it is produced rather than captured and written at
// the end. A build that hangs after twenty seconds of output is diagnosable; a build
// that produces nothing until it finishes is not.
type ShellExecutor struct {
	// WorkDir is the directory commands run in.
	WorkDir string
	// Env is added to every step's environment.
	Env map[string]string
}

func NewShellExecutor(workDir string) *ShellExecutor {
	return &ShellExecutor{WorkDir: workDir, Env: map[string]string{}}
}

func (s *ShellExecutor) Execute(ctx context.Context, ex Execution) error {
	var spec StepSpec
	if len(ex.Job.Spec) > 0 {
		if err := json.Unmarshal(ex.Job.Spec, &spec); err != nil {
			return fmt.Errorf("decode step spec: %w", err)
		}
	}
	if strings.TrimSpace(spec.Command) == "" {
		return fmt.Errorf("step %q has no command", ex.Job.StepKey)
	}

	cmd := shellCommand(ctx, spec.Command)
	cmd.Dir = s.WorkDir

	env := make([]string, 0, len(s.Env)+len(spec.Env)+4)
	for k, v := range s.Env {
		env = append(env, k+"="+v)
	}
	for k, v := range spec.Env {
		env = append(env, k+"="+v)
	}
	env = append(env,
		"NEXUS_JOB_ID="+ex.Job.ID.String(),
		"NEXUS_RUN_ID="+ex.Job.RunID.String(),
		"NEXUS_STEP="+ex.Job.StepKey,
		"NEXUS_ATTEMPT="+fmt.Sprint(ex.Job.Attempt),
	)
	cmd.Env = append(inheritedEnv(), env...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("attach stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("attach stderr: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start step: %w", err)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); stream(stdout, "stdout", ex.Log) }()
	go func() { defer wg.Done(); stream(stderr, "stderr", ex.Log) }()
	wg.Wait()

	err = cmd.Wait()
	// A cancelled context is reported as cancellation rather than as the exit status
	// the kill produced, so retry logic sees the real cause.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if err != nil {
		var ee *exec.ExitError
		if ok := asExitError(err, &ee); ok {
			return &ExitError{Code: ee.ExitCode()}
		}
		return err
	}
	return nil
}

func stream(r io.Reader, name string, log func(stream, line string)) {
	sc := bufio.NewScanner(r)
	// Build output routinely contains lines longer than the 64KB default, and a
	// scanner that overflows stops silently rather than erroring visibly.
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		log(name, sc.Text())
	}
}

func shellCommand(ctx context.Context, command string) *exec.Cmd {
	if runtime.GOOS == "windows" {
		return exec.CommandContext(ctx, "cmd", "/C", command)
	}
	return exec.CommandContext(ctx, "/bin/sh", "-c", command)
}

// FuncExecutor adapts a plain function, used for step types implemented in Go rather
// than as a subprocess — deployment, verification and promotion.
type FuncExecutor struct {
	Fn func(ctx context.Context, ex Execution) error
}

func (f FuncExecutor) Execute(ctx context.Context, ex Execution) error { return f.Fn(ctx, ex) }

// SleepExecutor is used by the seeded demo pipelines to produce runs with realistic
// durations without requiring a toolchain to be installed.
type SleepExecutor struct{ Duration time.Duration }

func (s SleepExecutor) Execute(ctx context.Context, ex Execution) error {
	ex.Log("stdout", fmt.Sprintf("%s: working", ex.Job.StepKey))
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(s.Duration):
		return nil
	}
}

func inheritedEnv() []string {
	// PATH and the platform essentials are inherited; the rest of the parent
	// environment is not, so a secret exported into the worker process does not
	// silently become visible to every step it runs.
	keep := []string{"PATH", "HOME", "SYSTEMROOT", "TEMP", "TMP", "USERPROFILE", "COMSPEC"}
	var out []string
	for _, k := range keep {
		if v, ok := lookupEnv(k); ok {
			out = append(out, k+"="+v)
		}
	}
	return out
}

// randFloat returns a float in [0,1) from the crypto source, avoiding a package-level
// math/rand seed shared across goroutines.
func randFloat() float64 {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0.5
	}
	return float64(binary.BigEndian.Uint64(b[:])>>11) / float64(1<<53)
}

func lookupEnv(k string) (string, bool) { return os.LookupEnv(k) }

func asExitError(err error, target **exec.ExitError) bool { return errors.As(err, target) }
