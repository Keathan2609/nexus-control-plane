# ---------------------------------------------------------------------------
# NEXUS on AWS: managed PostgreSQL, ECR, IRSA roles, and the four workloads on
# an existing EKS cluster.
#
# THIS ONE COSTS MONEY. Every apply here creates billed resources - see
# ../../README.md for current order-of-magnitude figures before running it.
# environments/local is the default path and needs no account anywhere.
#
# What this root module owns:
#   - RDS PostgreSQL 16, private, encrypted, backed up      (modules/aws-managed)
#   - four ECR repositories with immutable tags and a lifecycle policy
#   - one IRSA role per NEXUS service account
#   - the nexus namespace, the NEXUS Secret, and the Helm release
#
# What it deliberately does NOT own, because each has a lifecycle of its own
# and shared infrastructure should not be destroyable by an application deploy:
#   - the VPC and its subnets              (var.vpc_id, var.database_subnet_ids)
#   - the EKS cluster                      (var.eks_cluster_name)
#   - the NATS/JetStream cluster           (var.nats_url)
#   - DNS records and TLS certificates     (cert-manager / external-dns)
# ---------------------------------------------------------------------------

locals {
  # Helm resolves a relative chart path against the process working directory
  # rather than the module directory, so a local path is made absolute here.
  # A non-null var.chart_path is passed through untouched, since it may be an
  # OCI reference rather than a directory.
  chart_path = var.chart_path != null ? var.chart_path : abspath("${path.module}/../../../../deployments/helm/nexus")

  common_labels = {
    "nexus.dev/environment" = var.environment
    "nexus.dev/managed-by"  = "opentofu"
  }

  # null means "use the ECR repositories created below". Resolving it here
  # keeps the conditional out of the module call.
  image_registry = var.image_registry != null ? var.image_registry : module.aws_managed.ecr_registry

  # Rough worst case: every API, worker and reconciler replica holding a full
  # pool. Used only for the plan-time check below.
  expected_db_connections = (var.api_replicas + var.worker_replicas + 1) * 10
}

# ---------------------------------------------------------------------------
# AWS: database, registries, identities
# ---------------------------------------------------------------------------

module "aws_managed" {
  source = "../../modules/aws-managed"

  name_prefix = var.name_prefix
  environment = var.environment
  tags        = var.extra_tags

  vpc_id                     = var.vpc_id
  database_subnet_ids        = var.database_subnet_ids
  allowed_security_group_ids = var.node_security_group_ids
  allowed_cidr_blocks        = var.database_allowed_cidr_blocks

  instance_class           = var.db_instance_class
  allocated_storage_gb     = var.db_allocated_storage_gb
  max_allocated_storage_gb = var.db_max_allocated_storage_gb
  multi_az                 = var.db_multi_az

  backup_retention_days        = var.db_backup_retention_days
  deletion_protection          = var.db_deletion_protection
  skip_final_snapshot          = var.db_skip_final_snapshot
  performance_insights_enabled = var.db_performance_insights_enabled

  kms_key_arn = var.kms_key_arn

  # The password is generated inside the module and never leaves it except
  # through a sensitive output and the Secrets Manager entry. No password
  # variable is set here on purpose - a value passed in from this root module
  # would have to come from a tfvars file, which is exactly where credentials
  # must not live.
  store_password_in_secrets_manager = true

  ecr_keep_last_images     = var.ecr_keep_last_images
  ecr_image_tag_mutability = "IMMUTABLE"
  ecr_force_delete         = false

  eks_oidc_provider_arn = var.eks_oidc_provider_arn
  eks_oidc_provider_url = var.eks_oidc_provider_url
  kubernetes_namespace  = var.namespace
}

# ---------------------------------------------------------------------------
# Namespace
#
# Owned by the root rather than by a module for the same reason as in
# environments/local: more than one module needs it to exist, and having a
# module own it would mean `tofu destroy` of that module takes the namespace -
# and everything in it - with it.
# ---------------------------------------------------------------------------

resource "kubernetes_namespace_v1" "nexus" {
  metadata {
    name = var.namespace

    labels = merge(local.common_labels, {
      "app.kubernetes.io/name"       = "nexus"
      "app.kubernetes.io/part-of"    = "nexus"
      "app.kubernetes.io/managed-by" = "opentofu"

      # restricted, unlike the local environment. There are no in-cluster
      # datastores here - PostgreSQL is RDS and NATS is external - and all four
      # NEXUS workloads already run as non-root with every capability dropped
      # and a RuntimeDefault seccomp profile. Nothing in this namespace needs
      # the baseline exemption, so enforcement is set where it belongs.
      "pod-security.kubernetes.io/enforce" = "restricted"
      "pod-security.kubernetes.io/audit"   = "restricted"
      "pod-security.kubernetes.io/warn"    = "restricted"
    })
  }
}

# ---------------------------------------------------------------------------
# Workloads
# ---------------------------------------------------------------------------

module "platform" {
  source = "../../modules/nexus-platform"

  namespace        = kubernetes_namespace_v1.nexus.metadata[0].name
  create_namespace = false

  environment  = var.environment
  release_name = "nexus"
  labels       = local.common_labels

  chart_path       = local.chart_path
  chart_version    = var.chart_version
  deploy_workloads = var.deploy_workloads

  helm_timeout_seconds = var.helm_timeout_seconds

  # Roll back automatically on a failed release. The opposite of the local
  # environment: here a half-applied release is worse than losing the logs, and
  # the logs of a rolled-back deploy are still in the cluster's log pipeline.
  atomic = true

  image_registry = local.image_registry
  image_tag      = var.image_tag

  # Always, not IfNotPresent. Tags are immutable in ECR so there is no risk of
  # silently picking up different content, and Always means a node that has
  # cached a deleted or re-pushed tag cannot serve stale bytes.
  image_pull_policy  = "Always"
  image_pull_secrets = var.image_pull_secrets

  api_replicas    = var.api_replicas
  worker_replicas = var.worker_replicas
  web_replicas    = var.web_replicas

  # Not configurable, here or anywhere: nexus-reconciler is a single control
  # loop with no leader election, and the chart's schema rejects the key.
  reconciler_replicas = 1

  resources = var.resources

  log_level            = var.log_level
  otel_endpoint        = var.otel_endpoint
  worker_concurrency   = var.worker_concurrency
  worker_lease_seconds = var.worker_lease_seconds

  # The DSN carries the generated RDS password and sslmode=require, which the
  # instance's parameter group enforces server-side via rds.force_ssl.
  database_url = module.aws_managed.database_url
  nats_url     = var.nats_url

  # auth_jwt_secret and webhook_secret are deliberately not set: the module
  # generates them and writes them only to the Kubernetes Secret and to state.
  #
  # Setting them from a variable would mean the value lives in a tfvars file.
  # Read the generated webhook secret once with
  # `tofu output -raw webhook_secret` and paste it into the SCM webhook
  # configuration; rotating it is a deliberate change on both sides.

  # Chart features this module does not model as first-class variables. The
  # escape hatch is used rather than widened because ingress and NetworkPolicy
  # shapes are controller-specific and would not survive being generalised.
  extra_helm_values = {
    # Sent in full whether or not the Ingress is enabled. The chart's schema
    # types host and secretName as strings, and its template gates every use of
    # them on `enabled`, so the placeholders below are inert rather than
    # conditional - and a conditional would have to produce the same object
    # shape in both branches anyway. .invalid is the TLD reserved for exactly
    # this kind of never-resolvable placeholder.
    ingress = {
      enabled   = var.ingress_enabled
      className = var.ingress_class_name
      host      = coalesce(var.ingress_host, "nexus.invalid")
      tls = {
        enabled    = var.ingress_enabled && var.ingress_tls_secret_name != null
        secretName = coalesce(var.ingress_tls_secret_name, "nexus-tls")
      }
    }

    networkPolicy = {
      enabled                    = var.network_policy_enabled
      ingressControllerNamespace = var.ingress_controller_namespace
      monitoringNamespace        = var.enable_observability ? "observability" : "monitoring"
      observabilityNamespace     = "observability"

      # Workers clone from SCM and push to registries, so outbound TLS is
      # required. The chart's rule excludes RFC1918 and link-local ranges, so
      # this cannot become a path to the node metadata endpoint.
      allowExternalEgress = true
    }

    api = {
      autoscaling = {
        enabled                        = var.api_autoscaling.enabled
        minReplicas                    = var.api_autoscaling.min_replicas
        maxReplicas                    = var.api_autoscaling.max_replicas
        targetCPUUtilizationPercentage = var.api_autoscaling.target_cpu
      }
    }

    worker = {
      autoscaling = {
        enabled                        = var.worker_autoscaling.enabled
        minReplicas                    = var.worker_autoscaling.min_replicas
        maxReplicas                    = var.worker_autoscaling.max_replicas
        targetCPUUtilizationPercentage = var.worker_autoscaling.target_cpu
      }
    }
  }
}

# ---------------------------------------------------------------------------
# Optional in-cluster monitoring
# ---------------------------------------------------------------------------

module "observability" {
  source = "../../modules/nexus-observability"
  count  = var.enable_observability ? 1 : 0

  environment          = var.environment
  namespace            = "observability"
  create_namespace     = true
  monitored_namespaces = [kubernetes_namespace_v1.nexus.metadata[0].name]
  labels               = local.common_labels

  # Longer than the local environment's three days, and persisted. Still not a
  # long-term store: point a remote-write target at this Prometheus if you need
  # history beyond a fortnight.
  prometheus_retention           = "15d"
  prometheus_storage_size        = "100Gi"
  prometheus_persistence_enabled = true

  grafana_persistence_enabled = true

  enable_alertmanager  = true
  enable_node_exporter = true
}

# ---------------------------------------------------------------------------
# Plan-time checks
#
# These fail the plan rather than the apply. A check that only fires at apply
# time has already created half the resources by the time it speaks up.
# ---------------------------------------------------------------------------

check "irsa_configuration" {
  assert {
    condition     = var.eks_oidc_provider_arn == null || var.eks_oidc_provider_url != null
    error_message = "eks_oidc_provider_url must be set whenever eks_oidc_provider_arn is set; the IAM trust policy conditions are built from the issuer URL."
  }

  assert {
    # Without IRSA the pods have no AWS identity, so nothing can read the
    # database credential out of Secrets Manager. The stack still works - the
    # DSN is in the Kubernetes Secret - but the break-glass and rotation paths
    # described in the module do not exist. Worth knowing before an incident.
    condition     = var.eks_oidc_provider_arn != null
    error_message = "eks_oidc_provider_arn is not set, so no IRSA roles were created. NEXUS pods will have no AWS identity and cannot read the database credential from Secrets Manager. Register the cluster's OIDC provider and set both eks_oidc_provider_arn and eks_oidc_provider_url."
  }
}

check "database_connection_budget" {
  assert {
    condition     = local.expected_db_connections <= var.postgres_max_expected_connections
    error_message = "api_replicas + worker_replicas imply roughly ${local.expected_db_connections} database connections, above the budget of ${var.postgres_max_expected_connections}. RDS derives max_connections from instance memory, so either raise db_instance_class, lower the replica counts, or raise postgres_max_expected_connections once you have confirmed the instance can take it."
  }
}

check "ingress_configuration" {
  assert {
    condition     = !var.ingress_enabled || var.ingress_host != null
    error_message = "ingress_host must be set when ingress_enabled is true; an Ingress with no host matches every request that reaches the controller."
  }

  assert {
    condition     = !var.ingress_enabled || var.ingress_tls_secret_name != null
    error_message = "ingress_enabled is true but ingress_tls_secret_name is null, so the console and API are served over plaintext HTTP. Acceptable only when TLS terminates at a load balancer in front of the ingress controller."
  }
}

check "autoscaling_floor" {
  assert {
    # An HPA takes ownership of the scale subresource. If its floor is below
    # the declared replica count, the first reconciliation scales the
    # deployment DOWN to that floor - which looks like a mysterious capacity
    # loss minutes after an otherwise clean deploy.
    condition     = !var.api_autoscaling.enabled || var.api_autoscaling.min_replicas >= var.api_replicas
    error_message = "api_autoscaling.min_replicas (${var.api_autoscaling.min_replicas}) is below api_replicas (${var.api_replicas}). The HPA will scale the deployment down to its floor shortly after apply."
  }

  assert {
    condition     = !var.worker_autoscaling.enabled || var.worker_autoscaling.min_replicas >= var.worker_replicas
    error_message = "worker_autoscaling.min_replicas (${var.worker_autoscaling.min_replicas}) is below worker_replicas (${var.worker_replicas}). The HPA will scale the deployment down to its floor shortly after apply."
  }
}
