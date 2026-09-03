package deployments

import (
	"context"
	"fmt"
	"log/slog"
)

// Provider names how deployments are made real.
type Provider string

const (
	ProviderDocker     Provider = "docker"
	ProviderKubernetes Provider = "kubernetes"
)

// NewApplier builds the applier for the configured provider.
//
// Both providers genuinely deploy: Docker starts containers on the local daemon,
// Kubernetes applies manifests to a cluster and waits for the rollout. Neither
// simulates. The choice is about what the workload runs on, not about whether the
// deployment is real.
//
// Selection falls back rather than failing: a machine with no cluster should still be
// able to demonstrate the delivery lifecycle, and refusing to start because kubectl
// cannot reach a cluster would make Kubernetes a hard dependency of a system that does
// not need one.
func NewApplier(ctx context.Context, provider Provider, kubeContext, kindCluster, dockerNetwork string, log *slog.Logger) (Applier, Provider) {
	switch provider {
	case ProviderKubernetes:
		k := NewKubernetesApplier(kubeContext, kindCluster)
		if k.Available(ctx) {
			log.Info("using the kubernetes deployment provider",
				"context", kubeContext, "kind_cluster", kindCluster)
			return k, ProviderKubernetes
		}
		log.Warn("kubernetes was requested but the cluster is unreachable; falling back to docker",
			"context", kubeContext,
			"hint", "create it with scripts/kind.ps1 bootstrap")
		return NewDockerApplier(dockerNetwork), ProviderDocker
	default:
		return NewDockerApplier(dockerNetwork), ProviderDocker
	}
}

// Describe reports what a provider is, for the console's infrastructure view.
func Describe(p Provider) string {
	switch p {
	case ProviderKubernetes:
		return "Kubernetes — manifests applied to a cluster, rollout awaited, node port probed"
	case ProviderDocker:
		return "Docker — containers on the local daemon, published port probed"
	default:
		return fmt.Sprintf("unknown provider %q", p)
	}
}
