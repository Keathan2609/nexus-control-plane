package deployments

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"

	"github.com/Keathan2609/nexus/internal/core/domain"
)

// DockerApplier runs deployed services as Docker containers on the local machine.
//
// This is the applier that makes NEXUS demonstrable without a Kubernetes cluster, and it
// is a real applier rather than a simulation: it genuinely starts containers, genuinely
// publishes ports, and the health verifier genuinely probes them over HTTP. A rollback
// really does replace the running container with an earlier image.
//
// It is deliberately not a general-purpose orchestrator. Anything beyond "one container
// per environment, one port, replaced on deploy" belongs in Kubernetes, which is what
// the GitOps applier targets.
type DockerApplier struct {
	// Network attaches deployed containers to a named Docker network so the control
	// plane can reach them by container name when it runs in a container itself.
	Network string
	// HostBindAddr is the interface published ports bind to.
	HostBindAddr string

	mu    sync.Mutex
	ports map[domain.ID]int
	next  int
}

func NewDockerApplier(network string) *DockerApplier {
	return &DockerApplier{
		Network:      network,
		HostBindAddr: "127.0.0.1",
		ports:        map[domain.ID]int{},
		// Well clear of the ports the local stack itself uses.
		next: 31000,
	}
}

// containerName derives a stable name from the environment id, so a redeploy replaces
// the same container rather than accumulating one per attempt.
func containerName(environmentID domain.ID) string {
	id := string(environmentID)
	if i := strings.LastIndexByte(id, '_'); i >= 0 {
		id = id[i+1:]
	}
	if len(id) > 12 {
		id = id[len(id)-12:]
	}
	return "nexus-app-" + id
}

// PortFor returns the host port allocated to an environment, allocating one if needed.
func (a *DockerApplier) PortFor(environmentID domain.ID) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	if p, ok := a.ports[environmentID]; ok {
		return p
	}
	a.next++
	a.ports[environmentID] = a.next
	return a.next
}

func (a *DockerApplier) Apply(ctx context.Context, d Desired) (string, error) {
	name := containerName(d.EnvironmentID)
	port := a.PortFor(d.EnvironmentID)

	// Replacing rather than updating: Docker has no rolling update, and stopping the
	// old container before starting the new one is the honest local analogue of a
	// Recreate strategy. The brief unavailability is real and is exactly what health
	// verification then has to wait through.
	_ = run(ctx, "docker", "rm", "-f", name)

	args := []string{
		"run", "-d",
		"--name", name,
		"--label", "nexus.managed=true",
		"--label", "nexus.environment=" + string(d.EnvironmentID),
		"--label", "nexus.revision=" + d.Revision,
		"-p", fmt.Sprintf("%s:%d:%d", a.HostBindAddr, port, d.Port),
		"--restart", "no",
	}
	if a.Network != "" {
		args = append(args, "--network", a.Network)
	}
	args = append(args, d.Image)

	out, err := output(ctx, "docker", args...)
	if err != nil {
		return "", fmt.Errorf("start container %s from %s: %w: %s", name, d.Image, err, out)
	}
	return d.Revision, nil
}

// Observe reads the revision label off the running container.
//
// Reading it back from the container rather than returning what was applied is what
// makes drift detectable: if someone replaces the container by hand, the label no longer
// matches what NEXUS believes it deployed, and the reconciler notices.
func (a *DockerApplier) Observe(ctx context.Context, environmentID domain.ID) (string, error) {
	name := containerName(environmentID)
	out, err := output(ctx, "docker", "inspect",
		"--format", "{{index .Config.Labels \"nexus.revision\"}}", name)
	if err != nil {
		// No container is not an error: an environment that has never been deployed to,
		// or whose container was removed, simply has no observed revision.
		return "", nil
	}
	return strings.TrimSpace(out), nil
}

func (a *DockerApplier) Destroy(ctx context.Context, environmentID domain.ID) error {
	if err := run(ctx, "docker", "rm", "-f", containerName(environmentID)); err != nil {
		// Already gone is success: Destroy is called by the reconciler on a tick and
		// must be idempotent.
		return nil
	}
	a.mu.Lock()
	delete(a.ports, environmentID)
	a.mu.Unlock()
	return nil
}

// HealthTarget is the URL the verifier should probe for an environment.
func (a *DockerApplier) HealthTarget(environmentID domain.ID, path string) string {
	if path == "" {
		path = "/healthz"
	}
	return "http://" + a.HostBindAddr + ":" + strconv.Itoa(a.PortFor(environmentID)) + path
}

func run(ctx context.Context, name string, args ...string) error {
	_, err := output(ctx, name, args...)
	return err
}

func output(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}

// NoopApplier records desired state without touching any runtime.
//
// Used when NEXUS runs without Docker available, and by the seeder, which needs to
// create historical deployments without starting containers for releases that were
// notionally superseded weeks ago.
type NoopApplier struct {
	mu       sync.Mutex
	revision map[domain.ID]string
}

func NewNoopApplier() *NoopApplier {
	return &NoopApplier{revision: map[domain.ID]string{}}
}

func (n *NoopApplier) Apply(ctx context.Context, d Desired) (string, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.revision[d.EnvironmentID] = d.Revision
	return d.Revision, nil
}

func (n *NoopApplier) Observe(ctx context.Context, environmentID domain.ID) (string, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.revision[environmentID], nil
}

func (n *NoopApplier) Destroy(ctx context.Context, environmentID domain.ID) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.revision, environmentID)
	return nil
}

// SetObserved forces the observed revision to a value, which is how the failure demo
// injects drift without anyone having to edit a live container by hand.
func (n *NoopApplier) SetObserved(environmentID domain.ID, revision string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.revision[environmentID] = revision
}
