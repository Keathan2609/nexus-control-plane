# Both providers read an existing kubeconfig. Nothing here reaches a cloud API,
# authenticates against a registry, or needs a credential file, which is what
# keeps `tofu init` and `tofu validate` working on a machine with no accounts
# at all.
#
# config_context is set explicitly rather than inherited: a provider that
# silently follows the current context turns `kubectl config use-context` into
# an undocumented input to this configuration.

provider "kubernetes" {
  config_path    = pathexpand(var.kubeconfig_path)
  config_context = var.kube_context
}

provider "helm" {
  kubernetes {
    config_path    = pathexpand(var.kubeconfig_path)
    config_context = var.kube_context
  }
}
