package build_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Keathan2609/nexus/internal/build"
)

func TestTagForCommitIsDeterministic(t *testing.T) {
	// The same commit must always produce the same tag. If it did not, the image a
	// deployment references could not be re-derived from the release it came from.
	a := build.TagForCommit("a84fc1249b0e7d3f21")
	b := build.TagForCommit("a84fc1249b0e7d3f21")
	if a != b {
		t.Fatalf("tag is not deterministic: %s != %s", a, b)
	}
	if a != "a84fc12" {
		t.Fatalf("expected the short commit as the tag, got %s", a)
	}
}

func TestTagForCommitRejectsLatest(t *testing.T) {
	// An empty commit must not silently become "latest". A mutable tag is exactly what
	// makes a deployment un-reproducible and a rollback ambiguous.
	tag := build.TagForCommit("")
	if tag == "latest" || tag == "" {
		t.Fatalf("an empty commit must not yield a mutable or empty tag, got %q", tag)
	}
	if !strings.HasPrefix(tag, "build-") {
		t.Fatalf("expected a generated immutable tag, got %q", tag)
	}
}

func TestTagForCommitSanitisesUnsafeCharacters(t *testing.T) {
	// Branch-derived or user-supplied values reach this. A tag containing a slash or a
	// colon would produce a reference Docker parses as a different repository.
	tag := build.TagForCommit("feat/Some Branch:v1")
	for _, bad := range []string{"/", ":", " "} {
		if strings.Contains(tag, bad) {
			t.Fatalf("tag %q must not contain %q", tag, bad)
		}
	}
	if tag == "" {
		t.Fatal("sanitising must not produce an empty tag")
	}
}

func TestTagForCommitIsUniquePerBuildWhenThereIsNoRealCommit(t *testing.T) {
	// A manual trigger has no VCS commit, so the API synthesises one such as
	// "manual0903072400". Truncating that to seven characters yields "manual0" for
	// every manual run ever made - so each build would overwrite the previous image
	// under the same tag, and "roll back to the previous image" would have no answer.
	//
	// Anything that is not a plausible commit hash therefore falls back to a
	// timestamped tag, which is unique per build.
	first := build.TagForCommit("manual0903072400")
	if first == "manual0" {
		t.Fatalf("a synthesised commit must not truncate to a shared tag, got %q", first)
	}
	if !strings.HasPrefix(first, "build-") {
		t.Fatalf("expected a timestamped fallback tag, got %q", first)
	}
}

func TestTagForCommitKeepsRealCommitHashes(t *testing.T) {
	// A genuine hash still produces the short-commit tag, which is the readable form
	// and the whole point of deriving the tag from source.
	if got := build.TagForCommit("a84fc1249b0e7d3f21c4"); got != "a84fc12" {
		t.Fatalf("expected a84fc12, got %s", got)
	}
	if got := build.TagForCommit("0e36b67"); got != "0e36b67" {
		t.Fatalf("a short hash must be kept as-is, got %s", got)
	}
}

func TestReferenceCombinesRepositoryAndTag(t *testing.T) {
	req := build.Request{Repository: "nexus-demo", Tag: "a84fc12"}
	if got := req.Reference(); got != "nexus-demo:a84fc12" {
		t.Fatalf("expected nexus-demo:a84fc12, got %s", got)
	}
}

func TestValidateRequiresContextDirectory(t *testing.T) {
	err := build.Request{Repository: "nexus-demo", Tag: "abc1234"}.Validate()
	if err == nil {
		t.Fatal("a build with no context directory must be rejected")
	}
}

func TestValidateRequiresRepository(t *testing.T) {
	err := build.Request{ContextDir: ".", Tag: "abc1234"}.Validate()
	if err == nil {
		t.Fatal("a build with no repository must be rejected")
	}
}

func TestValidateRejectsUppercaseRepository(t *testing.T) {
	// Docker rejects uppercase in a repository name, and it does so at push time rather
	// than build time, which is a long way from the cause.
	err := build.Request{ContextDir: ".", Repository: "Nexus-Demo", Tag: "abc"}.Validate()
	if err == nil {
		t.Fatal("an uppercase repository name must be rejected")
	}
}

func TestValidateAcceptsAWellFormedRequest(t *testing.T) {
	req := build.Request{ContextDir: ".", Repository: "nexus-demo", Tag: "a84fc12"}
	if err := req.Validate(); err != nil {
		t.Fatalf("expected a valid request, got %v", err)
	}
}

// fakeRunner stands in for the Docker CLI so the builder's own logic can be tested
// without needing a daemon.
type fakeRunner struct {
	output   []string
	inspect  map[string]string
	err      error
	lastArgs []string
	delay    time.Duration
}

func (f *fakeRunner) Run(ctx context.Context, args []string, log func(string, string)) error {
	f.lastArgs = args
	if f.delay > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(f.delay):
		}
	}
	for _, line := range f.output {
		log("stdout", line)
	}
	return f.err
}

func (f *fakeRunner) Output(ctx context.Context, args []string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	key := strings.Join(args, " ")
	for k, v := range f.inspect {
		if strings.Contains(key, k) {
			return v, nil
		}
	}
	return "", nil
}

func TestBuildReturnsMetadataOnSuccess(t *testing.T) {
	runner := &fakeRunner{
		output: []string{"#1 [internal] load build definition", "#8 exporting layers done"},
		inspect: map[string]string{
			"RepoDigests": "nexus-demo@sha256:1f4c9a2b",
			"Size":        "18400000",
		},
		// A real build is never instantaneous. Without a delay the elapsed time rounds
		// to zero on a platform whose clock resolution is coarser than the fake, and
		// the duration assertion would be testing the clock rather than the builder.
		delay: 5 * time.Millisecond,
	}
	builder := build.NewBuilderWithRunner(runner)

	res, err := builder.Build(context.Background(), build.Request{
		ContextDir: ".", Repository: "nexus-demo", Tag: "a84fc12",
	}, func(string, string) {})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if res.Reference != "nexus-demo:a84fc12" {
		t.Fatalf("expected the built reference, got %s", res.Reference)
	}
	if res.Digest == "" {
		t.Fatal("a successful build must record a digest")
	}
	if res.SizeBytes <= 0 {
		t.Fatalf("expected a positive image size, got %d", res.SizeBytes)
	}
	if res.Duration <= 0 {
		t.Fatal("a build must record how long it took")
	}
}

func TestBuildStreamsOutput(t *testing.T) {
	// A build that produces nothing until it finishes is not diagnosable. Output must
	// reach the caller as it is produced.
	runner := &fakeRunner{output: []string{"step 1", "step 2", "step 3"}}
	builder := build.NewBuilderWithRunner(runner)

	var lines []string
	_, err := builder.Build(context.Background(), build.Request{
		ContextDir: ".", Repository: "nexus-demo", Tag: "abc1234",
	}, func(_, line string) { lines = append(lines, line) })
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(lines) < 3 {
		t.Fatalf("expected the build output to be streamed, saw %d lines", len(lines))
	}
}

func TestBuildFailureIsReportedWithContext(t *testing.T) {
	runner := &fakeRunner{err: errors.New("exit status 1")}
	builder := build.NewBuilderWithRunner(runner)

	_, err := builder.Build(context.Background(), build.Request{
		ContextDir: ".", Repository: "nexus-demo", Tag: "abc1234",
	}, func(string, string) {})
	if err == nil {
		t.Fatal("a failed build must return an error")
	}
	var buildErr *build.Error
	if !errors.As(err, &buildErr) {
		t.Fatalf("expected a build.Error so the step can classify it, got %T", err)
	}
	if !strings.Contains(buildErr.Error(), "nexus-demo:abc1234") {
		t.Fatalf("the error must name the image being built, got %q", buildErr.Error())
	}
}

func TestBuildRespectsCancellation(t *testing.T) {
	runner := &fakeRunner{delay: 5 * time.Second}
	builder := build.NewBuilderWithRunner(runner)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	started := time.Now()
	_, err := builder.Build(ctx, build.Request{
		ContextDir: ".", Repository: "nexus-demo", Tag: "abc1234",
	}, func(string, string) {})
	if err == nil {
		t.Fatal("a cancelled build must return an error")
	}
	if time.Since(started) > 2*time.Second {
		t.Fatal("cancellation must abort the build rather than waiting for it")
	}
}

func TestBuildPassesLabelsAndArgs(t *testing.T) {
	// Provenance labels are what let an image found on a node be traced back to the run
	// that produced it.
	runner := &fakeRunner{}
	builder := build.NewBuilderWithRunner(runner)

	_, err := builder.Build(context.Background(), build.Request{
		ContextDir: ".", Repository: "nexus-demo", Tag: "abc1234",
		Labels:    map[string]string{"nexus.run_id": "run_123"},
		BuildArgs: map[string]string{"VERSION": "1.2.3"},
	}, func(string, string) {})
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	joined := strings.Join(runner.lastArgs, " ")
	if !strings.Contains(joined, "nexus.run_id=run_123") {
		t.Fatalf("labels must be passed to the build, got: %s", joined)
	}
	if !strings.Contains(joined, "VERSION=1.2.3") {
		t.Fatalf("build args must be passed to the build, got: %s", joined)
	}
	if !strings.Contains(joined, "nexus-demo:abc1234") {
		t.Fatalf("the tag must be passed to the build, got: %s", joined)
	}
}

func TestBuildResolvesDockerfileRelativeToTheContext(t *testing.T) {
	// docker resolves a relative --file against the process working directory, not
	// against the build context. A worker running from the repository root with a
	// context of examples/demo-go-service would therefore look for a Dockerfile at the
	// root and fail with "no such file or directory" - which is exactly what happened
	// the first time this ran for real.
	runner := &fakeRunner{}
	builder := build.NewBuilderWithRunner(runner)

	_, err := builder.Build(context.Background(), build.Request{
		ContextDir: filepath.Join("some", "context"),
		Dockerfile: "Dockerfile",
		Repository: "nexus-demo", Tag: "abc1234",
	}, func(string, string) {})
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	joined := strings.Join(runner.lastArgs, " ")
	want := filepath.Join("some", "context", "Dockerfile")
	if !strings.Contains(joined, want) {
		t.Fatalf("expected the dockerfile resolved under the context (%s), got: %s", want, joined)
	}
}

func TestBuildLeavesAnAbsoluteDockerfileAlone(t *testing.T) {
	// An absolute path is already unambiguous and may legitimately sit outside the
	// context, so it must not be rewritten.
	runner := &fakeRunner{}
	builder := build.NewBuilderWithRunner(runner)

	abs := filepath.Join(t.TempDir(), "Dockerfile.custom")
	_, err := builder.Build(context.Background(), build.Request{
		ContextDir: "ctx", Dockerfile: abs,
		Repository: "nexus-demo", Tag: "abc1234",
	}, func(string, string) {})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !strings.Contains(strings.Join(runner.lastArgs, " "), abs) {
		t.Fatal("an absolute dockerfile path must be passed through unchanged")
	}
}

func TestBuildDoesNotLeakBuildArgsIntoLogs(t *testing.T) {
	// Build args are a common way for a token to reach a build. They are passed to
	// Docker but must never be echoed into the job log, which is stored and displayed.
	runner := &fakeRunner{}
	builder := build.NewBuilderWithRunner(runner)

	var logged []string
	_, err := builder.Build(context.Background(), build.Request{
		ContextDir: ".", Repository: "nexus-demo", Tag: "abc1234",
		BuildArgs: map[string]string{"NPM_TOKEN": "s3cr3t-value"},
	}, func(_, line string) { logged = append(logged, line) })
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	for _, line := range logged {
		if strings.Contains(line, "s3cr3t-value") {
			t.Fatalf("a build arg value leaked into the log: %q", line)
		}
	}
}

// A job log is persisted, rendered in the console and, for a portfolio project,
// screenshotted. The absolute build-context path puts the operator's home directory —
// username included — into all three. The command is logged with a path relative to the
// working directory instead.
func TestBuildLogsRelativeContextPath(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("working directory: %v", err)
	}
	absolute := filepath.Join(wd, "examples", "demo-go-service")

	fake := &fakeRunner{}
	b := build.NewBuilderWithRunner(fake)

	var logged []string
	_, err = b.Build(context.Background(), build.Request{
		ContextDir: absolute,
		Repository: "nexus-demo",
		Tag:        "abc1234",
	}, func(_, line string) { logged = append(logged, line) })
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	joined := strings.Join(logged, "\n")
	if strings.Contains(joined, wd) {
		t.Fatalf("the build log contains the absolute working directory:\n%s", joined)
	}
	if !strings.Contains(joined, "examples") {
		t.Fatalf("the build log lost the context path entirely:\n%s", joined)
	}
}

// A context outside the working directory cannot be made relative usefully, and must
// still be logged rather than dropped.
func TestBuildLogsUnrelatedContextPathUnchanged(t *testing.T) {
	fake := &fakeRunner{}
	b := build.NewBuilderWithRunner(fake)

	var logged []string
	_, err := b.Build(context.Background(), build.Request{
		ContextDir: "relative/context",
		Repository: "nexus-demo",
		Tag:        "abc1234",
	}, func(_, line string) { logged = append(logged, line) })
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !strings.Contains(strings.Join(logged, "\n"), "relative/context") {
		t.Fatalf("a relative context path was not logged: %v", logged)
	}
}
