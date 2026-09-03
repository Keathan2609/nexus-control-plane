// Package build produces container images from a workload's source.
//
// The boundary exists so that domain code never shells out to Docker directly. A
// `docker build` invocation scattered through the worker would make the container step
// untestable without a daemon, and would make swapping BuildKit for Kaniko or a remote
// builder a change to the pipeline engine rather than to one file.
//
// It is deliberately not a registry platform. It builds an image, tags it immutably,
// reports what it produced, and stops.
package build

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Request describes one image build.
type Request struct {
	// ContextDir is the build context, relative to the worker's working directory or
	// absolute.
	ContextDir string
	// Dockerfile is relative to ContextDir. Empty means "Dockerfile".
	Dockerfile string
	Repository string
	Tag        string
	BuildArgs  map[string]string
	Labels     map[string]string
	// Platform pins the target architecture. Empty means the daemon's default, which is
	// what a local kind cluster wants.
	Platform string
}

// Reference is the immutable image reference this request will produce.
func (r Request) Reference() string { return r.Repository + ":" + r.Tag }

func (r Request) Validate() error {
	if strings.TrimSpace(r.ContextDir) == "" {
		return fmt.Errorf("build: a context directory is required")
	}
	if strings.TrimSpace(r.Repository) == "" {
		return fmt.Errorf("build: a repository name is required")
	}
	// Docker rejects uppercase in a repository name, and it does so at push time rather
	// than at build time, which is a long way from the cause.
	if r.Repository != strings.ToLower(r.Repository) {
		return fmt.Errorf("build: repository %q must be lowercase", r.Repository)
	}
	if strings.TrimSpace(r.Tag) == "" {
		return fmt.Errorf("build: a tag is required")
	}
	return nil
}

// Result is what a successful build produced.
type Result struct {
	Reference string
	// Digest is the content identity of the image. Two builds of identical content
	// produce the same digest, which is what makes "is this the image we deployed"
	// answerable.
	Digest    string
	SizeBytes int64
	Duration  time.Duration
	BuiltAt   time.Time
}

// Error is a build that ran and failed, as distinct from NEXUS failing to run it. The
// step classifier uses the difference.
type Error struct {
	Reference string
	Cause     error
}

func (e *Error) Error() string {
	return fmt.Sprintf("building %s: %v", e.Reference, e.Cause)
}

func (e *Error) Unwrap() error { return e.Cause }

// Builder produces images.
type Builder interface {
	Build(ctx context.Context, req Request, log func(stream, line string)) (*Result, error)
}

// runner is the seam over the Docker CLI, so the builder's own logic is testable
// without a daemon.
type runner interface {
	Run(ctx context.Context, args []string, log func(stream, line string)) error
	Output(ctx context.Context, args []string) (string, error)
}

// DockerBuilder builds with the local Docker daemon.
//
// It shells out to the CLI rather than using the Engine API client. The CLI is what
// selects BuildKit, resolves the builder, and renders progress, and reimplementing that
// against the raw API to avoid one subprocess would be a poor trade.
type DockerBuilder struct {
	run runner
}

func NewBuilder() *DockerBuilder { return &DockerBuilder{run: cliRunner{}} }

// NewBuilderWithRunner injects a runner. Used by tests.
func NewBuilderWithRunner(r runner) *DockerBuilder { return &DockerBuilder{run: r} }

func (b *DockerBuilder) Build(ctx context.Context, req Request, log func(stream, line string)) (*Result, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	if log == nil {
		log = func(string, string) {}
	}

	args := []string{"build", "--tag", req.Reference(), "--progress", "plain"}
	if req.Dockerfile != "" {
		// docker resolves a relative --file against the process working directory, not
		// against the build context. A worker running from the repository root with a
		// context of examples/demo-go-service would otherwise look for a Dockerfile at
		// the root and fail with "no such file or directory" — which is exactly what
		// happened the first time this ran against a real daemon.
		dockerfile := req.Dockerfile
		if !filepath.IsAbs(dockerfile) {
			dockerfile = filepath.Join(req.ContextDir, dockerfile)
		}
		args = append(args, "--file", dockerfile)
	}
	if req.Platform != "" {
		args = append(args, "--platform", req.Platform)
	}
	// Sorted so the command is deterministic, which matters when the arguments end up
	// in a log that someone is diffing between two runs.
	for _, k := range sortedKeys(req.Labels) {
		args = append(args, "--label", k+"="+req.Labels[k])
	}
	for _, k := range sortedKeys(req.BuildArgs) {
		args = append(args, "--build-arg", k+"="+req.BuildArgs[k])
	}
	args = append(args, req.ContextDir)

	// Build-arg values are never echoed. They are a common route for a token to reach a
	// build, and the job log is persisted and displayed.
	//
	// The context is logged relative to the working directory for the same reason. An
	// absolute path here puts the operator's home directory, and therefore their
	// username, into a log that is stored, rendered in the console and screenshotted.
	log("system", "docker build --tag "+req.Reference()+" "+displayPath(req.ContextDir))

	started := time.Now()
	if err := b.run.Run(ctx, args, log); err != nil {
		return nil, &Error{Reference: req.Reference(), Cause: err}
	}
	elapsed := time.Since(started)

	res := &Result{
		Reference: req.Reference(),
		Duration:  elapsed,
		BuiltAt:   time.Now().UTC(),
	}

	// Metadata is read back from the daemon rather than assumed, for the same reason
	// deployment health is observed rather than assumed: the build reporting success
	// and the image existing are two different claims.
	if digest, err := b.run.Output(ctx, []string{
		"image", "inspect", req.Reference(), "--format", "{{if .RepoDigests}}{{index .RepoDigests 0}}{{else}}{{.Id}}{{end}}",
	}); err == nil {
		res.Digest = strings.TrimSpace(digest)
	}
	if size, err := b.run.Output(ctx, []string{
		"image", "inspect", req.Reference(), "--format", "{{.Size}}",
	}); err == nil {
		if n, convErr := strconv.ParseInt(strings.TrimSpace(size), 10, 64); convErr == nil {
			res.SizeBytes = n
		}
	}

	log("system", fmt.Sprintf("built %s in %s", res.Reference, elapsed.Round(time.Millisecond)))
	return res, nil
}

// TagForCommit derives an immutable tag from a commit.
//
// Never "latest". A mutable tag makes a deployment un-reproducible and a rollback
// ambiguous: "roll back to the previous image" has no answer if both releases point at
// the same moving reference.
func TagForCommit(commit string) string {
	cleaned := sanitiseTag(commit)

	// Only a plausible commit hash is truncated. A synthesised value such as
	// "manual0903072400" would truncate to "manual0" for every manual run ever made,
	// so each build would overwrite the previous image under the same tag and rollback
	// would have nothing distinct to return to.
	if !looksLikeCommit(cleaned) {
		return "build-" + time.Now().UTC().Format("20060102-150405")
	}
	if len(cleaned) > 7 {
		cleaned = cleaned[:7]
	}
	return cleaned
}

// looksLikeCommit reports whether a value is hexadecimal and long enough to be a commit
// hash rather than a placeholder.
func looksLikeCommit(s string) bool {
	if len(s) < 7 {
		return false
	}
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
}

// displayPath renders a build context for a log line.
//
// Relative to the working directory where that is meaningful, and unchanged otherwise —
// a context on another volume, or one that would resolve to a chain of "..", is clearer
// as it was given than as a relative path nobody can follow.
func displayPath(dir string) string {
	if !filepath.IsAbs(dir) {
		return dir
	}
	wd, err := os.Getwd()
	if err != nil {
		return filepath.Base(dir)
	}
	rel, err := filepath.Rel(wd, dir)
	if err != nil || strings.HasPrefix(rel, "..") {
		return filepath.Base(dir)
	}
	return rel
}

// sanitiseTag strips anything Docker would reject in a tag. A slash would make the
// reference parse as a different repository, and a colon as a different tag.
func sanitiseTag(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			b.WriteRune(r)
		}
	}
	return strings.Trim(b.String(), ".-_")
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// Insertion sort: these maps hold a handful of entries and this avoids the import.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// cliRunner executes the Docker CLI, streaming its output.
type cliRunner struct{}

func (cliRunner) Run(ctx context.Context, args []string, log func(stream, line string)) error {
	cmd := exec.CommandContext(ctx, "docker", args...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}

	var wg sync.WaitGroup
	wg.Add(2)
	// BuildKit writes its progress to stderr, so both streams are surfaced. Marking
	// them separately keeps genuine errors distinguishable in the console.
	go func() { defer wg.Done(); stream(stdout, "stdout", log) }()
	go func() { defer wg.Done(); stream(stderr, "stdout", log) }()
	wg.Wait()

	if err := cmd.Wait(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return err
	}
	return nil
}

func (cliRunner) Output(ctx context.Context, args []string) (string, error) {
	out, err := exec.CommandContext(ctx, "docker", args...).Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func stream(r io.Reader, name string, log func(stream, line string)) {
	sc := bufio.NewScanner(r)
	// Build output routinely exceeds the 64KB default, and a scanner that overflows
	// stops silently rather than erroring visibly.
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		log(name, sc.Text())
	}
}
