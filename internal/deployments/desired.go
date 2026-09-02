package deployments

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/Keathan2609/nexus/internal/core/domain"
)

// desiredFor renders the state NEXUS wants an environment to be in for this deployment.
//
// Rendering is a pure function of the release, the environment and the service
// definition, which is what makes GitOps work at all: the same inputs always produce
// byte-identical manifests, so a differing manifest hash means something genuinely
// changed rather than that the renderer is non-deterministic.
func (s *Service) desiredFor(ctx context.Context, dep *Deployment, env *Environment) (Desired, error) {
	var image string
	if err := s.db.QueryRow(ctx,
		`SELECT a.reference
		   FROM releases r JOIN artifacts a ON a.id = r.artifact_id
		  WHERE r.id = $1`, dep.ReleaseID).Scan(&image); err != nil {
		return Desired{}, fmt.Errorf("resolve release image: %w", err)
	}

	serviceName := env.Name
	port := 8080
	replicas := 1
	var svcName string
	var svcPort int
	err := s.db.QueryRow(ctx,
		`SELECT name, port FROM services WHERE project_id=$1 ORDER BY id LIMIT 1`,
		dep.ProjectID).Scan(&svcName, &svcPort)
	if err == nil {
		serviceName = svcName
		port = svcPort
	}
	// Production carries more than one replica; ephemeral environments do not need to.
	if env.Kind == "PRODUCTION" {
		replicas = 2
	}

	d := Desired{
		ProjectID:     dep.ProjectID,
		EnvironmentID: env.ID,
		Namespace:     env.Namespace,
		Revision:      dep.DesiredRevision,
		Image:         image,
		ServiceName:   serviceName,
		Port:          port,
		Replicas:      replicas,
	}
	d.Manifests = renderManifests(d)
	return d, nil
}

// renderManifests produces the Kubernetes objects for a deployed service.
//
// Written as a template rather than assembled through the Kubernetes client libraries
// because the output is the artefact: it is what gets committed for Argo CD to
// reconcile, what is shown in the console under desired state, and what is diffed to
// detect drift. Text in, text out, no client-go object graph in between.
func renderManifests(d Desired) string {
	var b strings.Builder
	fmt.Fprintf(&b, `apiVersion: apps/v1
kind: Deployment
metadata:
  name: %[1]s
  namespace: %[2]s
  labels:
    app.kubernetes.io/name: %[1]s
    app.kubernetes.io/managed-by: nexus
    nexus.dev/revision: "%[3]s"
spec:
  replicas: %[4]d
  selector:
    matchLabels:
      app.kubernetes.io/name: %[1]s
  template:
    metadata:
      labels:
        app.kubernetes.io/name: %[1]s
        nexus.dev/revision: "%[3]s"
    spec:
      securityContext:
        runAsNonRoot: true
        seccompProfile:
          type: RuntimeDefault
      containers:
        - name: %[1]s
          image: %[5]s
          ports:
            - containerPort: %[6]d
          readinessProbe:
            httpGet:
              path: /healthz
              port: %[6]d
            initialDelaySeconds: 2
            periodSeconds: 5
          livenessProbe:
            httpGet:
              path: /healthz
              port: %[6]d
            initialDelaySeconds: 10
            periodSeconds: 10
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities:
              drop: ["ALL"]
          resources:
            requests:
              cpu: 50m
              memory: 64Mi
            limits:
              cpu: 500m
              memory: 256Mi
---
apiVersion: v1
kind: Service
metadata:
  name: %[1]s
  namespace: %[2]s
  labels:
    app.kubernetes.io/name: %[1]s
    app.kubernetes.io/managed-by: nexus
spec:
  selector:
    app.kubernetes.io/name: %[1]s
  ports:
    - port: 80
      targetPort: %[6]d
`, d.ServiceName, d.Namespace, d.Revision, d.Replicas, d.Image, d.Port)
	return b.String()
}

// ManifestHash is the content identity of a rendered desired state.
func ManifestHash(manifests string) string {
	sum := sha256.Sum256([]byte(manifests))
	return hex.EncodeToString(sum[:])
}

// recordDesiredState persists the rendered manifests as a GitOps revision.
//
// Recording desired state separately from the deployment row is what allows the console
// to answer "what did NEXUS want at that moment" for a deployment that has since been
// superseded, and gives the reconciler something concrete to diff observed state against.
func (s *Service) recordDesiredState(ctx context.Context, dep *Deployment, d Desired) error {
	hash := ManifestHash(d.Manifests)
	_, err := s.db.Exec(ctx,
		`INSERT INTO gitops_revisions
		   (id, project_id, environment_id, deployment_id, revision, manifest_hash, manifests)
		 VALUES ($1,$2,$3,$4,$5,$6,$7)
		 ON CONFLICT (environment_id, revision) DO UPDATE
		   SET manifest_hash = EXCLUDED.manifest_hash,
		       manifests     = EXCLUDED.manifests,
		       deployment_id = EXCLUDED.deployment_id`,
		domain.NewID(domain.PrefixArtifact), dep.ProjectID, dep.EnvironmentID, dep.ID,
		d.Revision, hash, d.Manifests)
	if err != nil {
		return fmt.Errorf("record desired state: %w", err)
	}
	return nil
}
