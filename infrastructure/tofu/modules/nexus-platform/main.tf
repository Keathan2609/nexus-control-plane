# ---------------------------------------------------------------------------
# The NEXUS platform itself: namespace, credentials, and the Helm release that
# runs nexus-api, nexus-worker, nexus-reconciler and nexus-web.
#
# Division of responsibility, on purpose:
#   - This module owns the Secret. Credentials outlive any single release, must
#     be readable by an operator without `helm get values`, and must never be
#     handed to Helm as values - anything passed to Helm is stored in the
#     release object in clear text, for every revision it keeps.
#   - The chart at deployments/helm/nexus owns the ConfigMap, Deployments,
#     Services, probes and PodDisruptionBudgets.
#
# The chart is referenced by path and never modified from here. Its values
# schema sets additionalProperties:false at every level, so a key this module
# invents is a hard install-time failure rather than a silently ignored value.
# Everything below is written against that schema deliberately.
# ---------------------------------------------------------------------------

locals {
  # Mirrors the chart's nexus.fullname helper. Object names templated by the
  # chart (Services, ServiceAccounts, its ConfigMap) are derived from it, so
  # re-deriving it here is what keeps this module's outputs truthful rather
  # than merely plausible.
  chart_fullname = strcontains(var.release_name, "nexus") ? var.release_name : "${var.release_name}-nexus"

  secret_name = "${local.chart_fullname}-secrets"

  # The chart always renders its own ConfigMap under this name. It is
  # deliberately not created here: two controllers owning one object means Helm
  # refuses the install with an ownership error, and if it did not, whichever
  # applied last would silently win.
  config_map_name = "${local.chart_fullname}-config"

  common_labels = merge(
    {
      "app.kubernetes.io/name"       = "nexus"
      "app.kubernetes.io/instance"   = var.release_name
      "app.kubernetes.io/part-of"    = "nexus"
      "app.kubernetes.io/managed-by" = "opentofu"
      "nexus.dev/environment"        = var.environment
    },
    var.labels,
  )

  auth_jwt_secret = var.auth_jwt_secret != null ? var.auth_jwt_secret : random_password.auth_jwt.result
  webhook_secret  = var.webhook_secret != null ? var.webhook_secret : random_password.webhook.result

  secret_data = merge(
    {
      NEXUS_DATABASE_URL    = var.database_url
      NEXUS_AUTH_JWT_SECRET = local.auth_jwt_secret
      NEXUS_WEBHOOK_SECRET  = local.webhook_secret
    },
    var.extra_secret_data,
  )

  # Non-secret configuration, handed to the chart's config.* block. This is
  # what the chart renders into its ConfigMap. NEXUS_NATS_URL,
  # NEXUS_WORKER_CONCURRENCY and NEXUS_WORKER_LEASE_SECONDS are derived by the
  # chart from nats.url and worker.*, so they are not repeated here.
  chart_config = {
    logLevel    = var.log_level
    environment = var.environment

    # Bind on all interfaces inside the pod network namespace. The pod is
    # already isolated by NetworkPolicy, so this is not the exposure the same
    # bind would be on a VM.
    httpAddr    = ":${var.http_port}"
    metricsAddr = ":${var.metrics_port}"

    otelEndpoint = var.otel_endpoint
  }

  # A ConfigMap update restarts nothing, and environment variables (unlike
  # mounted files) are never refreshed in a running container. Stamping a
  # checksum of the configuration onto the pod template is what turns a config
  # change into a rollout.
  #
  # There is deliberately no equivalent checksum over the Secret: it would
  # publish a hash of credential material into an annotation that anyone with
  # pod-read can see. Rotate secrets with an explicit rollout instead.
  config_checksum_annotations = {
    "nexus.dev/config-checksum" = sha256(jsonencode(merge(local.chart_config, {
      natsUrl      = var.nats_url
      concurrency  = var.worker_concurrency
      leaseSeconds = var.worker_lease_seconds
    })))
  }

  # Each component's image is assembled here rather than through the chart's
  # global.imageRegistry prefix. That value prepends to the chart's default
  # repository, which already carries a registry host, and would yield
  # "my-registry/ghcr.io/keathan2609/nexus-api".
  image_repositories = {
    api        = "${var.image_registry}/nexus-api"
    worker     = "${var.image_registry}/nexus-worker"
    reconciler = "${var.image_registry}/nexus-reconciler"
    web        = "${var.image_registry}/nexus-web"
  }

  # The values contract expected by deployments/helm/nexus. Every key here is
  # one the chart's values.schema.json permits.
  helm_values = {
    global = {
      # Empty: the per-component repositories above are already fully
      # qualified, and this would prefix them a second time.
      imageRegistry    = ""
      imagePullSecrets = [for s in var.image_pull_secrets : { name = s }]
    }

    config = local.chart_config

    # create=false, plus the Secret this module owns. Passing the credentials
    # as chart values instead would write them into the Helm release object in
    # clear text, where anyone with secret-read in the namespace can recover
    # every past revision of them.
    secrets = {
      create         = false
      existingSecret = local.secret_name
    }

    # Both datastores come from the caller: nexus-datastores locally, RDS and
    # an external NATS in the cloud path. The chart's bundled copies are sized
    # for evaluation, and a StatefulSet in the application chart is not where a
    # system of record belongs.
    postgresql = {
      enabled = false
    }

    nats = {
      enabled = false
      url     = var.nats_url
    }

    api = {
      replicaCount = var.api_replicas
      image = {
        repository = local.image_repositories.api
        tag        = var.image_tag
        pullPolicy = var.image_pull_policy
      }
      resources = var.resources.api
      service = {
        port        = var.http_port
        metricsPort = var.metrics_port
      }

      # The chart defaults to minAvailable 2, which on a single-replica install
      # is a budget that can never be satisfied: every voluntary eviction is
      # refused and a node drain hangs indefinitely. Derive it from the replica
      # count instead, and skip the budget entirely when there is only one pod
      # that could be disrupted.
      podDisruptionBudget = {
        enabled      = var.api_replicas > 1
        minAvailable = max(1, var.api_replicas - 1)
      }

      podAnnotations = local.config_checksum_annotations
    }

    worker = {
      replicaCount = var.worker_replicas
      image = {
        repository = local.image_repositories.worker
        tag        = var.image_tag
        pullPolicy = var.image_pull_policy
      }
      resources = var.resources.worker

      # The chart renders these two into NEXUS_WORKER_CONCURRENCY and
      # NEXUS_WORKER_LEASE_SECONDS.
      concurrency  = var.worker_concurrency
      leaseSeconds = var.worker_lease_seconds

      podAnnotations = local.config_checksum_annotations
    }

    # No replicaCount here, and not by omission: the chart's schema rejects the
    # key outright because this component is a singleton control loop with no
    # leader election. var.reconciler_replicas exists to be validated at 1 and
    # to state that constraint at the module boundary.
    reconciler = {
      image = {
        repository = local.image_repositories.reconciler
        tag        = var.image_tag
        pullPolicy = var.image_pull_policy
      }
      resources      = var.resources.reconciler
      podAnnotations = local.config_checksum_annotations
    }

    web = {
      replicaCount = var.web_replicas
      image = {
        repository = local.image_repositories.web
        tag        = var.image_tag
        pullPolicy = var.image_pull_policy
      }
      resources = var.resources.web
      service = {
        port = var.web_port
      }
      podAnnotations = local.config_checksum_annotations
    }
  }

  namespace_name = var.create_namespace ? kubernetes_namespace_v1.this[0].metadata[0].name : var.namespace
}

# ---------------------------------------------------------------------------
# Namespace
# ---------------------------------------------------------------------------

resource "kubernetes_namespace_v1" "this" {
  count = var.create_namespace ? 1 : 0

  metadata {
    name   = var.namespace
    labels = merge(local.common_labels, local.pod_security_labels)
  }
}

locals {
  # Pod Security Admission is namespace-scoped and enforced by the API server,
  # so it costs nothing and catches a privileged pod before it starts.
  #
  # `baseline` rather than `restricted` is the default because the in-cluster
  # PostgreSQL and NATS in nexus-datastores run as non-root but do not drop all
  # capabilities or set a seccomp profile, which `restricted` requires. Audit
  # and warn stay at restricted, so the gap is visible in the API server audit
  # log and on every kubectl apply without breaking the local path. The four
  # NEXUS workloads are themselves restricted-compliant, so an environment with
  # no in-cluster datastores should enforce restricted.
  pod_security_labels = {
    "pod-security.kubernetes.io/enforce" = var.pod_security_standard
    "pod-security.kubernetes.io/audit"   = "restricted"
    "pod-security.kubernetes.io/warn"    = "restricted"
  }
}

# ---------------------------------------------------------------------------
# Generated credentials
# ---------------------------------------------------------------------------

# 64 alphanumeric characters, ~380 bits. special = false because these values
# are copied by hand into CI settings and SCM webhook forms often enough that
# shell-hostile punctuation causes more incidents than the extra entropy per
# character prevents.
resource "random_password" "auth_jwt" {
  length  = 64
  special = false
}

resource "random_password" "webhook" {
  length  = 64
  special = false
}

# ---------------------------------------------------------------------------
# Secrets
#
# Created unconditionally, including when deploy_workloads is false. That is
# the point of the flag: credentials and datastores can be stood up and checked
# before the chart is installed, and during an incident the configuration can
# stay in place while the pods are gone.
# ---------------------------------------------------------------------------

resource "kubernetes_secret_v1" "secrets" {
  metadata {
    name      = local.secret_name
    namespace = local.namespace_name
    labels    = merge(local.common_labels, { "app.kubernetes.io/component" = "config" })

    annotations = {
      "nexus.dev/rotation-policy" = "JWT and webhook secrets rotate quarterly; database credential on lease expiry"
    }
  }

  type = "Opaque"
  data = local.secret_data
}

# ---------------------------------------------------------------------------
# Workloads
# ---------------------------------------------------------------------------

resource "helm_release" "nexus" {
  count = var.deploy_workloads ? 1 : 0

  name      = var.release_name
  chart     = var.chart_path
  version   = var.chart_version
  namespace = local.namespace_name

  # The namespace is a first-class resource above so it can carry labels and
  # Pod Security Admission settings, and so it survives a `helm uninstall`.
  create_namespace = false

  atomic          = var.atomic
  cleanup_on_fail = true
  wait            = true
  wait_for_jobs   = true
  timeout         = var.helm_timeout_seconds

  # Two documents rather than one merged map: Helm merges values documents
  # deeply and in order, whereas OpenTofu's merge() is shallow and would let a
  # single override key wipe out a whole computed block.
  values = [
    yamlencode(local.helm_values),
    yamlencode(var.extra_helm_values),
  ]

  # Helm will happily start pods that then CrashLoop because the Secret they
  # reference does not exist yet. The dependency is on object existence, not on
  # any attribute, so it has to be explicit.
  depends_on = [
    kubernetes_secret_v1.secrets,
  ]
}
