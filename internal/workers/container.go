package workers

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"time"

	"github.com/Keathan2609/nexus/internal/build"
	"github.com/Keathan2609/nexus/internal/core/domain"
	"github.com/Keathan2609/nexus/internal/platform/database"
)

// ContainerExecutor runs the containerise step: it builds a real image and records what
// it produced.
//
// The image it builds is the image the deployment path then runs. Nothing regenerates a
// reference later or substitutes a different tag — the artifact row written here is what
// the release points at, which is what makes "roll back to the previous image" a
// well-defined operation rather than a hope.
type ContainerExecutor struct {
	builder build.Builder
	db      *database.DB
	// Repository is the local image repository images are built into. Local because
	// there is no registry in the demonstration path; kind loads images directly from
	// the daemon.
	Repository string
	// WorkDir anchors relative build contexts.
	WorkDir string
}

func NewContainerExecutor(builder build.Builder, db *database.DB, repository, workDir string) *ContainerExecutor {
	if repository == "" {
		repository = "nexus-demo"
	}
	return &ContainerExecutor{builder: builder, db: db, Repository: repository, WorkDir: workDir}
}

func (c *ContainerExecutor) Execute(ctx context.Context, ex Execution) error {
	var spec StepSpec
	if len(ex.Job.Spec) > 0 {
		if err := json.Unmarshal(ex.Job.Spec, &spec); err != nil {
			return fmt.Errorf("decode step spec: %w", err)
		}
	}

	// A containerise step with no build context is a definition error, not a build
	// failure. Saying so is more useful than a Docker error about a missing Dockerfile.
	if spec.BuildContext == "" {
		return fmt.Errorf("step %q has no build_context; a containerise step must say what to build", ex.Job.StepKey)
	}

	commit, err := c.commitFor(ctx, ex.Job.RunID)
	if err != nil {
		return err
	}

	repository := c.Repository
	if spec.Image != "" {
		repository = spec.Image
	}

	contextDir := spec.BuildContext
	if !filepath.IsAbs(contextDir) && c.WorkDir != "" {
		contextDir = filepath.Join(c.WorkDir, contextDir)
	}

	req := build.Request{
		ContextDir: contextDir,
		Dockerfile: spec.Dockerfile,
		Repository: repository,
		Tag:        build.TagForCommit(commit),
		BuildArgs:  map[string]string{"VERSION": build.TagForCommit(commit)},
		// Provenance on the image itself, so an image found on a node can be traced
		// back to the run that produced it without consulting NEXUS.
		Labels: map[string]string{
			"nexus.run_id":     ex.Job.RunID.String(),
			"nexus.job_id":     ex.Job.ID.String(),
			"nexus.project_id": ex.Job.ProjectID.String(),
			"nexus.commit":     commit,
			"nexus.built_at":   time.Now().UTC().Format(time.RFC3339),
		},
	}

	result, err := c.builder.Build(ctx, req, ex.Log)
	if err != nil {
		return err
	}

	if err := c.recordArtifact(ctx, ex.Job.ProjectID, ex.Job.RunID, commit, result); err != nil {
		// The image exists but NEXUS could not record it. Failing the step is correct:
		// an unrecorded image cannot be deployed or rolled back to, so reporting success
		// would leave the run claiming an artifact the rest of the system cannot find.
		return fmt.Errorf("record image metadata: %w", err)
	}

	ex.Log("system", fmt.Sprintf("image %s recorded (%s, %.1f MB)",
		result.Reference, shortDigest(result.Digest), float64(result.SizeBytes)/(1024*1024)))
	return nil
}

// commitFor reads the commit the run was created from, so the tag is derived from the
// source rather than from when the build happened.
func (c *ContainerExecutor) commitFor(ctx context.Context, runID domain.ID) (string, error) {
	var commit string
	if err := c.db.QueryRow(ctx,
		`SELECT commit_sha FROM pipeline_runs WHERE id = $1`, runID).Scan(&commit); err != nil {
		return "", fmt.Errorf("load run commit: %w", err)
	}
	return commit, nil
}

// recordArtifact persists the image and the release that references it.
//
// Both in one transaction: a release pointing at an artifact row that does not exist
// would be a foreign key violation at deploy time, discovered long after the build.
func (c *ContainerExecutor) recordArtifact(ctx context.Context, projectID, runID domain.ID, commit string, res *build.Result) error {
	tx, err := c.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	artifactID := domain.NewID(domain.PrefixArtifact)
	if _, err := tx.Exec(ctx,
		`INSERT INTO artifacts (id, project_id, run_id, kind, reference, digest, size_bytes)
		 VALUES ($1,$2,$3,'CONTAINER_IMAGE',$4,$5,NULLIF($6,0))`,
		artifactID, projectID, runID, res.Reference, res.Digest, res.SizeBytes); err != nil {
		return fmt.Errorf("insert artifact: %w", err)
	}

	// The release version is the run number, which is the number a person refers to.
	var number int64
	if err := tx.QueryRow(ctx,
		`SELECT number FROM pipeline_runs WHERE id=$1`, runID).Scan(&number); err != nil {
		return fmt.Errorf("load run number: %w", err)
	}
	version := fmt.Sprintf("r%d", number)

	if _, err := tx.Exec(ctx,
		`INSERT INTO releases (id, project_id, version, commit_sha, artifact_id, run_id)
		 VALUES ($1,$2,$3,$4,$5,$6)
		 ON CONFLICT (project_id, version) DO UPDATE
		   SET artifact_id = EXCLUDED.artifact_id, commit_sha = EXCLUDED.commit_sha`,
		domain.NewID(domain.PrefixRelease), projectID, version, commit, artifactID, runID); err != nil {
		return fmt.Errorf("insert release: %w", err)
	}

	return tx.Commit(ctx)
}

func shortDigest(d string) string {
	if len(d) > 19 {
		return d[:19]
	}
	if d == "" {
		return "no digest"
	}
	return d
}
