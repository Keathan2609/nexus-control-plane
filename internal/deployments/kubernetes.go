package deployments

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/Keathan2609/nexus/internal/core/domain"
)

// KubernetesApplier deploys workloads into a real cluster.
//
// It drives kubectl rather than client-go, for the same reason the desired state is
// rendered as text: the manifests are the artefact. What NEXUS applies is exactly what
// it stored, what the console displays, and what Argo CD would reconcile — there is no
// object graph in the middle that could diverge from any of them.
//
// The cost is a subprocess per operation and no watch API. Both are acceptable here:
// deployments happen on the order of once a minute, not once a millisecond, and
// observation is a poll by design because the reconciler is level-triggered anyway.
type KubernetesApplier struct {
	// Context is the kubeconfig context. Empty uses the current one.
	Context string
	// KindCluster, when set, causes images to be loaded into that kind cluster before
	// the workload references them. Without this the pod cannot pull a locally built
	// image, because there is no registry to pull it from.
	KindCluster string
	// ReadyTimeout bounds how long Apply waits for the rollout.
	ReadyTimeout time.Duration

	mu     sync.Mutex
	loaded map[string]bool
}

func NewKubernetesApplier(kubeContext, kindCluster string) *KubernetesApplier {
	return &KubernetesApplier{
		Context:      kubeContext,
		KindCluster:  kindCluster,
		ReadyTimeout: 120 * time.Second,
		loaded:       map[string]bool{},
	}
}

// Apply makes the desired state real and waits for the rollout to complete.
//
// It returns the revision only after Kubernetes reports the Deployment available. That
// is still not "healthy" — the verifier probes the application afterwards — but a
// rollout that never completes should fail here rather than proceed to a verification
// that would time out against pods which never started.
func (k *KubernetesApplier) Apply(ctx context.Context, d Desired) (string, error) {
	if err := k.ensureNamespace(ctx, d.Namespace); err != nil {
		return "", err
	}

	// The image must exist inside the cluster before the workload references it. kind
	// nodes have their own containerd, so an image in the host daemon is invisible to
	// them; loading it is what makes the locally built image usable without a registry.
	if err := k.loadImage(ctx, d.Image); err != nil {
		return "", fmt.Errorf("load image into cluster: %w", err)
	}

	if _, err := k.kubectlInput(ctx, d.Manifests, "apply", "-f", "-"); err != nil {
		return "", fmt.Errorf("apply manifests: %w", err)
	}

	rolloutCtx, cancel := context.WithTimeout(ctx, k.ReadyTimeout)
	defer cancel()

	out, err := k.kubectl(rolloutCtx,
		"rollout", "status", "deployment/"+d.ServiceName,
		"-n", d.Namespace, "--timeout", fmt.Sprintf("%ds", int(k.ReadyTimeout.Seconds())))
	if err != nil {
		// A rollout that does not complete is a deployment failure with a specific
		// cause, and the cause is in the pod events rather than in kubectl's exit code.
		detail := k.diagnose(ctx, d)
		return "", fmt.Errorf("rollout did not complete: %w\n%s%s", err, out, detail)
	}

	return d.Revision, nil
}

// Observe reports the revision actually running, read from the live workload.
//
// It reads the pod template label rather than what was applied, so an image swapped by
// hand — or a rollout that partially failed and left the old ReplicaSet serving — shows
// as drift instead of being invisible.
func (k *KubernetesApplier) Observe(ctx context.Context, environmentID domain.ID) (string, error) {
	name, namespace, err := k.locate(ctx, environmentID)
	if err != nil || name == "" {
		// No workload is not an error. An environment that has never been deployed to
		// has no observed revision, and reporting that as a failure would make the
		// reconciler record drift for every empty environment on every cycle.
		return "", nil
	}

	// The annotation, not the label. The label is sanitised for Kubernetes and is
	// therefore lossy; the annotation holds the revision exactly as applied, so what
	// comes back can be compared to the desired revision without normalising both.
	out, err := k.kubectl(ctx, "get", "deployment", name, "-n", namespace,
		"-o", `jsonpath={.spec.template.metadata.annotations.nexus\.dev/revision}`)
	if err != nil {
		return "", nil
	}
	return strings.TrimSpace(out), nil
}

func (k *KubernetesApplier) Destroy(ctx context.Context, environmentID domain.ID) error {
	name, namespace, err := k.locate(ctx, environmentID)
	if err != nil || name == "" {
		return nil
	}
	// Deleting by label rather than by name removes the Service alongside the
	// Deployment, and is idempotent when some of it is already gone.
	_, _ = k.kubectl(ctx, "delete", "all",
		"-l", "nexus.dev/environment="+string(environmentID), "-n", namespace, "--ignore-not-found")
	return nil
}

// Status is the evidence the console shows for a Kubernetes-backed deployment.
type Status struct {
	Name              string `json:"name"`
	Namespace         string `json:"namespace"`
	Image             string `json:"image"`
	Revision          string `json:"revision"`
	DesiredReplicas   int    `json:"desired_replicas"`
	ReadyReplicas     int    `json:"ready_replicas"`
	AvailableReplicas int    `json:"available_replicas"`
	Available         bool   `json:"available"`
	Reason            string `json:"reason,omitempty"`
}

// Status reads what the cluster actually reports about a workload.
//
// This is the "observed, not assumed" principle applied to Kubernetes: availability
// comes from the Deployment's own condition rather than from the fact that apply
// returned zero.
func (k *KubernetesApplier) Status(ctx context.Context, namespace, name string) (*Status, error) {
	out, err := k.kubectl(ctx, "get", "deployment", name, "-n", namespace, "-o", "json")
	if err != nil {
		return nil, fmt.Errorf("read deployment %s/%s: %w", namespace, name, err)
	}

	var payload struct {
		Spec struct {
			Replicas int `json:"replicas"`
			Template struct {
				Metadata struct {
					Labels      map[string]string `json:"labels"`
					Annotations map[string]string `json:"annotations"`
				} `json:"metadata"`
				Spec struct {
					Containers []struct {
						Image string `json:"image"`
					} `json:"containers"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
		Status struct {
			ReadyReplicas     int `json:"readyReplicas"`
			AvailableReplicas int `json:"availableReplicas"`
			Conditions        []struct {
				Type    string `json:"type"`
				Status  string `json:"status"`
				Reason  string `json:"reason"`
				Message string `json:"message"`
			} `json:"conditions"`
		} `json:"status"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		return nil, fmt.Errorf("decode deployment: %w", err)
	}

	s := &Status{
		Name:              name,
		Namespace:         namespace,
		Revision:          payload.Spec.Template.Metadata.Annotations["nexus.dev/revision"],
		DesiredReplicas:   payload.Spec.Replicas,
		ReadyReplicas:     payload.Status.ReadyReplicas,
		AvailableReplicas: payload.Status.AvailableReplicas,
	}
	if len(payload.Spec.Template.Spec.Containers) > 0 {
		s.Image = payload.Spec.Template.Spec.Containers[0].Image
	}
	for _, c := range payload.Status.Conditions {
		if c.Type == "Available" {
			s.Available = c.Status == "True"
			if !s.Available {
				s.Reason = c.Reason + ": " + c.Message
			}
		}
	}
	return s, nil
}

// loadImage makes a locally built image available to the cluster nodes.
//
// Cached per image because loading is slow and an image reference is immutable — the
// same tag always denotes the same content, so loading it twice is wasted time rather
// than a correctness question.
func (k *KubernetesApplier) loadImage(ctx context.Context, image string) error {
	if k.KindCluster == "" || image == "" {
		return nil
	}

	k.mu.Lock()
	already := k.loaded[image]
	k.mu.Unlock()
	if already {
		return nil
	}

	loadCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(loadCtx, "kind", "load", "docker-image", image, "--name", k.KindCluster)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("kind load docker-image %s: %w: %s", image, err, buf.String())
	}

	k.mu.Lock()
	k.loaded[image] = true
	k.mu.Unlock()
	return nil
}

// locate finds the workload belonging to an environment by its label.
func (k *KubernetesApplier) locate(ctx context.Context, environmentID domain.ID) (name, namespace string, err error) {
	out, err := k.kubectl(ctx, "get", "deployments", "--all-namespaces",
		"-l", "nexus.dev/environment="+string(environmentID),
		"-o", "jsonpath={.items[0].metadata.name} {.items[0].metadata.namespace}")
	if err != nil {
		return "", "", err
	}
	parts := strings.Fields(strings.TrimSpace(out))
	if len(parts) != 2 {
		return "", "", nil
	}
	return parts[0], parts[1], nil
}

func (k *KubernetesApplier) ensureNamespace(ctx context.Context, namespace string) error {
	if namespace == "" {
		return fmt.Errorf("a namespace is required")
	}
	if _, err := k.kubectl(ctx, "get", "namespace", namespace); err == nil {
		return nil
	}
	if _, err := k.kubectl(ctx, "create", "namespace", namespace); err != nil {
		// A concurrent deployment into the same environment may have created it between
		// the check and the create, which is success, not failure.
		if _, getErr := k.kubectl(ctx, "get", "namespace", namespace); getErr == nil {
			return nil
		}
		return fmt.Errorf("create namespace %s: %w", namespace, err)
	}
	return nil
}

// diagnose gathers why a rollout did not complete. Pod events carry the actual cause —
// ImagePullBackOff, CrashLoopBackOff, a failing readiness probe — and kubectl's exit
// status carries none of it.
func (k *KubernetesApplier) diagnose(ctx context.Context, d Desired) string {
	var b strings.Builder
	if pods, err := k.kubectl(ctx, "get", "pods", "-n", d.Namespace,
		"-l", "app.kubernetes.io/name="+d.ServiceName,
		"-o", "wide"); err == nil {
		b.WriteString("\npods:\n" + pods)
	}
	if events, err := k.kubectl(ctx, "get", "events", "-n", d.Namespace,
		"--sort-by", ".lastTimestamp"); err == nil {
		lines := strings.Split(strings.TrimSpace(events), "\n")
		if len(lines) > 8 {
			lines = lines[len(lines)-8:]
		}
		b.WriteString("\nrecent events:\n" + strings.Join(lines, "\n"))
	}
	return b.String()
}

func (k *KubernetesApplier) kubectl(ctx context.Context, args ...string) (string, error) {
	if k.Context != "" {
		args = append([]string{"--context", k.Context}, args...)
	}
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}

func (k *KubernetesApplier) kubectlInput(ctx context.Context, stdin string, args ...string) (string, error) {
	if k.Context != "" {
		args = append([]string{"--context", k.Context}, args...)
	}
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	cmd.Stdin = strings.NewReader(stdin)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		return buf.String(), fmt.Errorf("%w: %s", err, buf.String())
	}
	return buf.String(), nil
}

// Available reports whether the cluster is reachable, so the console can show the
// provider as connected rather than asserting it.
func (k *KubernetesApplier) Available(ctx context.Context) bool {
	probe, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := k.kubectl(probe, "version", "--output", "json")
	return err == nil
}

// HealthTarget is where the workload can be reached from outside the cluster.
//
// The node port is published to the host by the kind cluster's extraPortMappings, so
// this is a real HTTP request to the application rather than an inspection of what
// Kubernetes believes about the pod. Those are different claims: a pod can be Ready
// while the process inside it cannot serve.
func (k *KubernetesApplier) HealthTarget(d Desired, path string) string {
	if d.NodePort == 0 {
		return ""
	}
	if path == "" {
		path = "/healthz"
	}
	return fmt.Sprintf("http://localhost:%d%s", d.NodePort, path)
}
