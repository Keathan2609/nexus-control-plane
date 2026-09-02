# ---------------------------------------------------------------------------
# NEXUS on a local Kubernetes cluster (kind, k3d, minikube, Docker Desktop).
#
# This is the default path and the only one that needs no account anywhere:
# `tofu apply` here talks to a cluster on your own machine and creates nothing
# that costs money.
#
# What you get:
#   - a namespace with Pod Security Admission labels
#   - PostgreSQL 16 and NATS 2.10 with JetStream, single node, PVC-backed
#   - a generated database password and JWT/webhook secrets, in Secrets only
#   - the four NEXUS workloads via deployments/helm/nexus
#   - optionally Prometheus and Grafana
#
# See ../production for the shape of the cloud path.
# ---------------------------------------------------------------------------

locals {
  environment = "local"

  # The chart lives in this repository and is maintained alongside the Go
  # services. Helm resolves a relative chart path against the process working
  # directory rather than the module directory, so it is made absolute here -
  # otherwise this works when run from this folder and mysteriously does not
  # when run from the repository root.
  chart_path = var.chart_path != null ? var.chart_path : abspath("${path.module}/../../../../deployments/helm/nexus")

  common_labels = {
    "nexus.dev/environment" = local.environment
    "nexus.dev/managed-by"  = "opentofu"
  }
}

# The namespace is created here rather than inside either module because both
# of them need it. If nexus-platform created it, nexus-datastores would have to
# depend on nexus-platform, while nexus-platform already depends on
# nexus-datastores for the database URL - a cycle. Owning the namespace at the
# root is the cheapest way to break it, and it also means `tofu destroy` of a
# single module cannot take the namespace (and everything else in it) with it.
resource "kubernetes_namespace_v1" "nexus" {
  metadata {
    name = var.namespace

    labels = merge(local.common_labels, {
      "app.kubernetes.io/name"       = "nexus"
      "app.kubernetes.io/part-of"    = "nexus"
      "app.kubernetes.io/managed-by" = "opentofu"

      # baseline, not restricted: the bundled PostgreSQL and NATS run as
      # non-root but do not drop every capability or set a seccomp profile.
      # warn/audit still hold the higher bar, so the gap shows up in kubectl
      # output instead of silently passing.
      "pod-security.kubernetes.io/enforce" = "baseline"
      "pod-security.kubernetes.io/audit"   = "restricted"
      "pod-security.kubernetes.io/warn"    = "restricted"
    })
  }
}

module "datastores" {
  source = "../../modules/nexus-datastores"

  namespace   = kubernetes_namespace_v1.nexus.metadata[0].name
  name_prefix = "nexus"
  labels      = local.common_labels

  postgres_storage_size = var.postgres_storage_size
  nats_storage_size     = var.nats_storage_size
  storage_class_name    = var.storage_class_name

  # A single NATS server: JetStream replication needs a Raft quorum, and three
  # servers on a laptop buys nothing but memory pressure.
  nats_replicas = 1

  # The bundled PostgreSQL serves plaintext. Asking for TLS here would fail the
  # connection rather than secure it; the traffic never leaves the node.
  sslmode = "disable"

  # Every NEXUS process opens a pool, and the local defaults are small, so the
  # PostgreSQL default of 100 is plenty.
  postgres_max_connections = 50
}

module "platform" {
  source = "../../modules/nexus-platform"

  # See the namespace resource above: the root owns it, so the module must not
  # try to create it.
  namespace        = kubernetes_namespace_v1.nexus.metadata[0].name
  create_namespace = false

  environment  = local.environment
  release_name = "nexus"
  labels       = local.common_labels

  chart_path       = local.chart_path
  deploy_workloads = var.deploy_workloads

  # Images are expected to be side-loaded into the cluster (kind load /
  # minikube image load), so there is nothing to pull and no registry
  # credential to hold.
  image_registry    = var.image_registry
  image_tag         = var.image_tag
  image_pull_policy = "IfNotPresent"

  api_replicas        = var.api_replicas
  worker_replicas     = var.worker_replicas
  reconciler_replicas = 1
  web_replicas        = var.web_replicas

  log_level          = var.log_level
  worker_concurrency = var.worker_concurrency

  # No OTLP collector runs by default on a laptop; an unreachable endpoint
  # means every span export retries and fills the logs with noise.
  otel_endpoint = ""

  # Wired straight from the datastores module. These are the only two values
  # that have to cross between the modules, and both are computed rather than
  # written down anywhere.
  database_url = module.datastores.database_url
  nats_url     = module.datastores.nats_url

  # auth_jwt_secret and webhook_secret are deliberately not set: the module
  # generates them. Nothing outside this cluster verifies these tokens, so
  # there is no reason for a human to ever see the values, let alone commit
  # them.

  # Local sizing. Small requests so a two-core node can actually schedule the
  # whole stack alongside the datastores.
  resources = {
    api = {
      requests = { cpu = "50m", memory = "96Mi" }
      limits   = { cpu = "500m", memory = "384Mi" }
    }
    worker = {
      requests = { cpu = "50m", memory = "128Mi" }
      limits   = { cpu = "1000m", memory = "512Mi" }
    }
    reconciler = {
      requests = { cpu = "25m", memory = "64Mi" }
      limits   = { cpu = "250m", memory = "192Mi" }
    }
    web = {
      requests = { cpu = "50m", memory = "128Mi" }
      limits   = { cpu = "500m", memory = "384Mi" }
    }
  }

  # Rolling back on failure hides the logs you need when a chart is new and
  # breaking. Turn it back on once the chart is boring.
  atomic = false
}

module "observability" {
  source = "../../modules/nexus-observability"
  count  = var.enable_observability ? 1 : 0

  environment          = local.environment
  namespace            = "observability"
  create_namespace     = true
  monitored_namespaces = [kubernetes_namespace_v1.nexus.metadata[0].name]
  labels               = local.common_labels

  storage_class_name = var.storage_class_name

  # Short retention and no persistence for Grafana: this is a dev loop, and
  # a laptop disk is not where a month of samples should live.
  prometheus_retention           = "3d"
  prometheus_storage_size        = "4Gi"
  prometheus_persistence_enabled = true
  grafana_persistence_enabled    = false

  # Nobody is paged from a laptop, and node-exporter's hostPath mounts are a
  # lot of privilege for metrics Docker Desktop already shows you.
  enable_alertmanager  = false
  enable_node_exporter = false
}
