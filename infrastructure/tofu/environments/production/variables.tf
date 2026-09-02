# ---------------------------------------------------------------------------
# Account and cluster
#
# Nothing here has a credential in it. AWS authentication comes from the
# ambient chain (SSO profile, environment variables, CI role); see providers.tf.
# ---------------------------------------------------------------------------

variable "aws_region" {
  description = "Region every AWS resource is created in. No default: a defaulted region is how a database ends up in an account's leftover us-east-1 instead of next to the cluster that queries it."
  type        = string

  validation {
    condition     = can(regex("^[a-z]{2}(-gov)?-(north|south|east|west|central|northeast|northwest|southeast|southwest)-[0-9]$", var.aws_region))
    error_message = "aws_region must be a region identifier such as eu-west-1 or us-east-2."
  }
}

variable "environment" {
  description = "Which cloud environment this root module manages. Sets NEXUS_ENVIRONMENT, tags every AWS resource, and forms part of the RDS identifier, so changing it on an existing installation replaces the database."
  type        = string
  default     = "production"

  validation {
    # 'local' is excluded: it is the name of the other root module and would
    # tell NEXUS to expose development-only endpoints on a public installation.
    condition     = contains(["development", "staging", "production"], var.environment)
    error_message = "environment must be one of: development, staging, production. Use environments/local for a laptop cluster."
  }
}

variable "eks_cluster_name" {
  description = "Name of an EXISTING EKS cluster to deploy into. This root module does not create a cluster: control plane lifecycle (version upgrades, node group rotation) runs on a different cadence from application deploys, and coupling them means a routine release can roll a control plane."
  type        = string

  validation {
    condition     = can(regex("^[a-zA-Z0-9][a-zA-Z0-9-_]{0,99}$", var.eks_cluster_name))
    error_message = "eks_cluster_name must be a valid EKS cluster name: alphanumerics, dashes and underscores, up to 100 characters."
  }
}

variable "name_prefix" {
  description = "Prefix for every AWS resource name. Include a distinguishing word if one account hosts more than one NEXUS installation, because RDS identifiers and ECR repository names are account-unique."
  type        = string
  default     = "nexus"

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{1,24}$", var.name_prefix))
    error_message = "name_prefix must be lowercase alphanumerics and dashes, 2-25 chars, starting with a letter."
  }
}

variable "namespace" {
  description = "Kubernetes namespace for the NEXUS workloads. Also part of the IRSA trust condition, so a role issued for this namespace cannot be assumed from another one."
  type        = string
  default     = "nexus"

  validation {
    condition     = can(regex("^[a-z0-9]([-a-z0-9]*[a-z0-9])?$", var.namespace)) && length(var.namespace) <= 63
    error_message = "namespace must be a valid RFC 1123 DNS label."
  }
}

# ---------------------------------------------------------------------------
# Network
#
# The VPC and its subnets are inputs. They outlive any single application and
# normally belong to a separate network stack; a module that can delete a VPC
# is a module nobody wants to run.
# ---------------------------------------------------------------------------

variable "vpc_id" {
  description = "VPC the RDS instance lives in. Must be the VPC the EKS nodes run in, or peered to it."
  type        = string

  validation {
    condition     = can(regex("^vpc-[0-9a-f]{8,17}$", var.vpc_id))
    error_message = "vpc_id must look like vpc-0123456789abcdef0."
  }
}

variable "database_subnet_ids" {
  description = "PRIVATE subnets for the RDS subnet group, in at least two availability zones. RDS requires two AZs even for a single-AZ instance, so that a failover has somewhere to go."
  type        = list(string)

  validation {
    condition     = length(var.database_subnet_ids) >= 2
    error_message = "database_subnet_ids needs at least two subnets in different availability zones; RDS rejects a subnet group with fewer."
  }

  validation {
    condition     = alltrue([for s in var.database_subnet_ids : can(regex("^subnet-[0-9a-f]{8,17}$", s))])
    error_message = "each entry in database_subnet_ids must look like subnet-0123456789abcdef0."
  }
}

variable "node_security_group_ids" {
  description = "Security groups allowed to reach PostgreSQL on 5432 - normally the EKS node group's security group. Preferred over CIDR blocks because it survives subnet renumbering. Read it from the cluster with: aws eks describe-cluster --name <cluster> --query 'cluster.resourcesVpcConfig.securityGroupIds'."
  type        = list(string)
  default     = []

  validation {
    condition     = alltrue([for s in var.node_security_group_ids : can(regex("^sg-[0-9a-f]{8,17}$", s))])
    error_message = "each entry in node_security_group_ids must look like sg-0123456789abcdef0."
  }
}

variable "database_allowed_cidr_blocks" {
  description = "Additional CIDR blocks allowed to reach PostgreSQL. Use sparingly - a security group reference is more precise and does not silently widen when a subnet is resized."
  type        = list(string)
  default     = []

  validation {
    condition     = alltrue([for c in var.database_allowed_cidr_blocks : !startswith(c, "0.0.0.0")])
    error_message = "database_allowed_cidr_blocks must not contain 0.0.0.0/0. A database open to the internet is never the intended configuration."
  }
}

# ---------------------------------------------------------------------------
# IRSA
# ---------------------------------------------------------------------------

variable "eks_oidc_provider_arn" {
  description = "ARN of the cluster's IAM OIDC provider. null skips IRSA role creation entirely, which is correct when the provider has not been registered yet. Without it the pods have no AWS identity and cannot read the database credential from Secrets Manager."
  type        = string
  default     = null

  validation {
    condition     = var.eks_oidc_provider_arn == null ? true : can(regex("^arn:aws[a-z-]*:iam::[0-9]{12}:oidc-provider/", var.eks_oidc_provider_arn))
    error_message = "eks_oidc_provider_arn must be an IAM OIDC provider ARN, e.g. arn:aws:iam::123456789012:oidc-provider/oidc.eks.eu-west-1.amazonaws.com/id/ABC123."
  }
}

variable "eks_oidc_provider_url" {
  description = "Issuer URL of the cluster's IAM OIDC provider WITHOUT the https:// scheme, e.g. oidc.eks.eu-west-1.amazonaws.com/id/ABC123. Required whenever eks_oidc_provider_arn is set; IAM trust conditions compare against the bare host and path."
  type        = string
  default     = null

  validation {
    condition     = var.eks_oidc_provider_url == null ? true : !startswith(var.eks_oidc_provider_url, "https://")
    error_message = "eks_oidc_provider_url must not include the https:// scheme."
  }
}

# ---------------------------------------------------------------------------
# Images
# ---------------------------------------------------------------------------

variable "image_tag" {
  description = "Version tag for all four NEXUS images. No default and no fallback: the running version of a production system is a decision, not a default. The API, worker and reconciler share database schema and NATS subjects, so mixing versions between them is a migration rather than a deploy."
  type        = string

  validation {
    condition     = var.image_tag != "latest"
    error_message = "image_tag must not be 'latest'. The chart rejects it too, so this would fail at install time anyway - a floating tag makes the running version unknowable and turns any pod restart into an unplanned upgrade."
  }

  validation {
    condition     = can(regex("^[a-zA-Z0-9_][a-zA-Z0-9._-]{0,126}$", var.image_tag))
    error_message = "image_tag must be a valid OCI tag: alphanumerics, dots, dashes and underscores."
  }
}

variable "image_registry" {
  description = "Registry and repository prefix for the NEXUS images. null uses the ECR repositories this root module creates, which is the intended path. Set it to pull from a mirror or from GHCR instead."
  type        = string
  default     = null
}

variable "image_pull_secrets" {
  description = "Names of pre-existing imagePullSecrets in the namespace. Empty for ECR, where the node role grants pull access without a secret."
  type        = list(string)
  default     = []
}

# ---------------------------------------------------------------------------
# RDS sizing and durability
#
# Every value here has a price attached. See README.md for figures.
# ---------------------------------------------------------------------------

variable "db_instance_class" {
  description = "RDS instance class. The default is a deliberate starting point rather than a recommendation: size it from observed connection count and working set, not from a guess."
  type        = string
  default     = "db.t4g.small"

  validation {
    condition     = can(regex("^db[.][a-z0-9]+[.][a-z0-9]+$", var.db_instance_class))
    error_message = "db_instance_class must look like db.t4g.small or db.m7g.large."
  }
}

variable "db_allocated_storage_gb" {
  description = "Initial RDS storage in GiB. gp3 volumes grow through db_max_allocated_storage_gb without downtime, so starting small costs nothing but a later autoscale event."
  type        = number
  default     = 50

  validation {
    condition     = var.db_allocated_storage_gb >= 20 && var.db_allocated_storage_gb <= 65536
    error_message = "db_allocated_storage_gb must be between 20 (the RDS gp3 minimum) and 65536."
  }
}

variable "db_max_allocated_storage_gb" {
  description = "Ceiling for RDS storage autoscaling. Set equal to db_allocated_storage_gb to disable autoscaling; leaving headroom is what prevents a full disk from becoming an outage at 3am."
  type        = number
  default     = 200

  validation {
    condition     = var.db_max_allocated_storage_gb >= 20
    error_message = "db_max_allocated_storage_gb must be at least 20."
  }
}

variable "db_multi_az" {
  description = "Run a synchronous standby in a second AZ. Roughly doubles the instance cost and is the difference between a failover measured in a minute or two and one measured in however long a restore takes."
  type        = bool
  default     = true
}

variable "db_backup_retention_days" {
  description = "Days of automated backups, which is also the point-in-time recovery window. 0 disables both."
  type        = number
  default     = 14

  validation {
    condition     = var.db_backup_retention_days >= 1 && var.db_backup_retention_days <= 35
    error_message = "db_backup_retention_days must be between 1 and 35 here. 0 is accepted by RDS but disables point-in-time recovery, which is not a defensible setting for the system of record."
  }
}

variable "db_deletion_protection" {
  description = "Refuse to delete the RDS instance until this is turned off. On by default: `tofu destroy` should not be able to take the system of record with it in one command."
  type        = bool
  default     = true
}

variable "db_skip_final_snapshot" {
  description = "Skip the final snapshot on delete. Leave false: the final snapshot is the last copy of the data if a destroy turns out to have been a mistake."
  type        = bool
  default     = false
}

variable "db_performance_insights_enabled" {
  description = "Enable RDS Performance Insights. The 7-day retention tier is free on supported instance classes and is usually the fastest way to find which query caused an incident."
  type        = bool
  default     = true
}

variable "kms_key_arn" {
  description = "Customer-managed KMS key for RDS storage, the Secrets Manager secret and ECR. null uses AWS-managed keys, which are free but cannot carry their own key policy or rotation schedule."
  type        = string
  default     = null

  validation {
    condition     = var.kms_key_arn == null ? true : can(regex("^arn:aws[a-z-]*:kms:", var.kms_key_arn))
    error_message = "kms_key_arn must be a KMS key ARN."
  }
}

variable "ecr_keep_last_images" {
  description = "Images retained per ECR repository before the lifecycle policy expires the oldest. Storage is billed per GB-month and container images are not small, but fewer than about five leaves no room to roll back."
  type        = number
  default     = 50

  validation {
    condition     = var.ecr_keep_last_images >= 5 && var.ecr_keep_last_images <= 1000
    error_message = "ecr_keep_last_images must be between 5 and 1000."
  }
}

# ---------------------------------------------------------------------------
# Event bus
# ---------------------------------------------------------------------------

variable "nats_url" {
  description = "NEXUS_NATS_URL for an existing NATS 2.10 cluster with JetStream. Required and not created here: a Raft-quorum JetStream cluster is a stateful system with its own upgrade and backup lifecycle, and the copy bundled in the application chart is sized for evaluation. Run it with the NATS operator, or point this at a managed endpoint. Comma-separated seed URLs are accepted."
  type        = string

  validation {
    condition     = can(regex("^nats://", var.nats_url))
    error_message = "nats_url must start with nats://."
  }
}

# ---------------------------------------------------------------------------
# Workload sizing
# ---------------------------------------------------------------------------

variable "api_replicas" {
  description = "nexus-api replicas. Stateless HTTP: scale for request volume and for surviving a node drain. Three spreads across AZs without a tie."
  type        = number
  default     = 3

  validation {
    condition     = var.api_replicas >= 2 && var.api_replicas <= 50 && floor(var.api_replicas) == var.api_replicas
    error_message = "api_replicas must be a whole number between 2 and 50. One replica means every deploy and every node drain is an outage."
  }
}

variable "worker_replicas" {
  description = "nexus-worker replicas. Each worker leases jobs independently, so this scales horizontally - but every replica opens a database pool, so raise it with the instance class in mind."
  type        = number
  default     = 3

  validation {
    condition     = var.worker_replicas >= 1 && var.worker_replicas <= 100 && floor(var.worker_replicas) == var.worker_replicas
    error_message = "worker_replicas must be a whole number between 1 and 100."
  }
}

variable "web_replicas" {
  description = "nexus-web (Next.js console) replicas."
  type        = number
  default     = 2

  validation {
    condition     = var.web_replicas >= 2 && var.web_replicas <= 20 && floor(var.web_replicas) == var.web_replicas
    error_message = "web_replicas must be a whole number between 2 and 20. One replica means every deploy is a visible console outage."
  }
}

variable "resources" {
  description = "Per-component CPU and memory requests/limits, keyed by api, worker, reconciler and web. Defaults are sized for a real node group rather than a laptop."
  type = map(object({
    requests = object({
      cpu    = string
      memory = string
    })
    limits = object({
      cpu    = string
      memory = string
    })
  }))

  default = {
    api = {
      requests = { cpu = "250m", memory = "256Mi" }
      limits   = { cpu = "1000m", memory = "512Mi" }
    }
    worker = {
      requests = { cpu = "500m", memory = "512Mi" }
      limits   = { cpu = "2000m", memory = "2Gi" }
    }
    reconciler = {
      requests = { cpu = "50m", memory = "128Mi" }
      limits   = { cpu = "500m", memory = "256Mi" }
    }
    web = {
      requests = { cpu = "100m", memory = "256Mi" }
      limits   = { cpu = "500m", memory = "512Mi" }
    }
  }

  validation {
    condition     = length(setsubtract(["api", "worker", "reconciler", "web"], keys(var.resources))) == 0
    error_message = "resources must define all four components: api, worker, reconciler, web."
  }

  validation {
    condition     = length(setsubtract(keys(var.resources), ["api", "worker", "reconciler", "web"])) == 0
    error_message = "resources may only contain the keys api, worker, reconciler and web."
  }
}

# ---------------------------------------------------------------------------
# Application configuration
# ---------------------------------------------------------------------------

variable "log_level" {
  description = "NEXUS_LOG_LEVEL. debug on a busy installation is expensive in log storage and can leak request detail into a log aggregator, so info is the default."
  type        = string
  default     = "info"

  validation {
    condition     = contains(["debug", "info", "warn", "error"], var.log_level)
    error_message = "log_level must be one of: debug, info, warn, error."
  }
}

variable "otel_endpoint" {
  description = "NEXUS_OTEL_ENDPOINT, an OTLP gRPC collector address. Empty disables trace export; an unreachable endpoint is worse than none, because every export retries."
  type        = string
  default     = "http://otel-collector.observability.svc.cluster.local:4317"

  validation {
    condition     = var.otel_endpoint == "" || can(regex("^(http://|https://|[a-z0-9.-]+:[0-9]+)", var.otel_endpoint))
    error_message = "otel_endpoint must be empty, an http(s) URL, or a host:port pair."
  }
}

variable "worker_concurrency" {
  description = "NEXUS_WORKER_CONCURRENCY - in-flight jobs per worker replica. Pipeline work is IO-bound (git, registry, network), so this can exceed the CPU limit; raise it together with the worker memory limit."
  type        = number
  default     = 8

  validation {
    condition     = var.worker_concurrency >= 1 && var.worker_concurrency <= 256 && floor(var.worker_concurrency) == var.worker_concurrency
    error_message = "worker_concurrency must be a whole number between 1 and 256."
  }
}

variable "worker_lease_seconds" {
  description = "NEXUS_WORKER_LEASE_SECONDS - how long a job stays leased before another worker may reclaim it. Must outlive the worst-case pause between heartbeats and stay well under the worker's termination grace period."
  type        = number
  default     = 60

  validation {
    condition     = var.worker_lease_seconds >= 15 && var.worker_lease_seconds <= 900 && floor(var.worker_lease_seconds) == var.worker_lease_seconds
    error_message = "worker_lease_seconds must be a whole number between 15 and 900."
  }
}

variable "postgres_max_expected_connections" {
  description = "Connection budget used only as a plan-time sanity check against the replica counts, not sent to RDS. RDS derives max_connections from instance memory; this catches the case where the replica counts alone would exhaust a small instance."
  type        = number
  default     = 200

  validation {
    condition     = var.postgres_max_expected_connections >= 25 && var.postgres_max_expected_connections <= 5000
    error_message = "postgres_max_expected_connections must be between 25 and 5000."
  }
}

# ---------------------------------------------------------------------------
# Exposure
# ---------------------------------------------------------------------------

variable "ingress_enabled" {
  description = "Create the chart's Ingress. Off by default: it depends on an ingress controller and a DNS record that this root module does not manage, and an Ingress pointing at neither is a confusing no-op."
  type        = bool
  default     = false
}

variable "ingress_host" {
  description = "Hostname for the NEXUS console and API. The chart serves both from one host under a path prefix so the browser treats them as same-origin - no CORS preflight on every API call, and server-sent events work without credential-mode gymnastics. Required when ingress_enabled is true."
  type        = string
  default     = null

  validation {
    condition     = var.ingress_host == null ? true : can(regex("^[a-z0-9]([a-z0-9-]*[a-z0-9])?([.][a-z0-9]([a-z0-9-]*[a-z0-9])?)+$", var.ingress_host))
    error_message = "ingress_host must be a lowercase fully-qualified DNS name such as nexus.example.com."
  }
}

variable "ingress_class_name" {
  description = "IngressClass to use. The chart's default annotations are nginx-specific; on another controller they are ignored and the SSE timeouts must be set its own way."
  type        = string
  default     = "nginx"
}

variable "ingress_tls_secret_name" {
  description = "Secret holding the TLS certificate for ingress_host, usually written by cert-manager. null serves plaintext, which is only acceptable when TLS terminates at a load balancer in front of the controller."
  type        = string
  default     = null
}

variable "network_policy_enabled" {
  description = "Apply the chart's NetworkPolicies. On by default here, unlike in the chart: a production namespace should be default-deny. Confirm your CNI actually enforces NetworkPolicy first - Calico and Cilium do, some managed defaults do not - because a policy the CNI ignores reads as protection in review and provides none at runtime."
  type        = bool
  default     = true
}

variable "ingress_controller_namespace" {
  description = "Namespace of the ingress controller permitted to reach the API and console when NetworkPolicies are on."
  type        = string
  default     = "ingress-nginx"
}

# ---------------------------------------------------------------------------
# Autoscaling
# ---------------------------------------------------------------------------

variable "api_autoscaling" {
  description = "HorizontalPodAutoscaler for nexus-api. min_replicas should equal api_replicas: once an HPA owns the scale subresource, the replica count in the chart is only the value used before the HPA's first observation."
  type = object({
    enabled      = bool
    min_replicas = number
    max_replicas = number
    target_cpu   = number
  })

  default = {
    enabled      = false
    min_replicas = 3
    max_replicas = 10
    target_cpu   = 70
  }

  validation {
    condition     = var.api_autoscaling.min_replicas >= 1 && var.api_autoscaling.max_replicas >= var.api_autoscaling.min_replicas
    error_message = "api_autoscaling.max_replicas must be greater than or equal to min_replicas, and min_replicas at least 1."
  }

  validation {
    condition     = var.api_autoscaling.target_cpu > 0 && var.api_autoscaling.target_cpu <= 100
    error_message = "api_autoscaling.target_cpu must be a percentage between 1 and 100."
  }
}

variable "worker_autoscaling" {
  description = "HorizontalPodAutoscaler for nexus-worker. CPU is a poor proxy for queue depth on an IO-bound worker: if you run KEDA or the Prometheus adapter, scale on JetStream pending messages instead and leave this off."
  type = object({
    enabled      = bool
    min_replicas = number
    max_replicas = number
    target_cpu   = number
  })

  default = {
    enabled      = false
    min_replicas = 3
    max_replicas = 20
    target_cpu   = 75
  }

  validation {
    condition     = var.worker_autoscaling.min_replicas >= 1 && var.worker_autoscaling.max_replicas >= var.worker_autoscaling.min_replicas
    error_message = "worker_autoscaling.max_replicas must be greater than or equal to min_replicas, and min_replicas at least 1."
  }

  validation {
    condition     = var.worker_autoscaling.target_cpu > 0 && var.worker_autoscaling.target_cpu <= 100
    error_message = "worker_autoscaling.target_cpu must be a percentage between 1 and 100."
  }
}

# ---------------------------------------------------------------------------
# Feature switches
# ---------------------------------------------------------------------------

variable "deploy_workloads" {
  description = "Install the NEXUS Helm release. Set false for the first apply against a new account: it creates RDS, ECR, IAM and the Secret without trying to pull images that have not been pushed yet."
  type        = bool
  default     = true
}

variable "chart_path" {
  description = "Override the path to the NEXUS Helm chart. null resolves to deployments/helm/nexus in this repository. Set it to an OCI reference plus chart_version to deploy a packaged, versioned chart, which is what a real release pipeline should do."
  type        = string
  default     = null
}

variable "chart_version" {
  description = "Chart version constraint. Only meaningful for a chart pulled from a repository; ignored for a local directory."
  type        = string
  default     = null
}

variable "enable_observability" {
  description = "Install the bundled Prometheus and Grafana. Off by default: most installations with a real AWS account already have monitoring, and a second unmanaged Prometheus is cost and confusion rather than insight."
  type        = bool
  default     = false
}

variable "helm_timeout_seconds" {
  description = "How long Helm waits for the release to become ready before failing. Generous enough to cover pulling four images onto a cold node."
  type        = number
  default     = 900

  validation {
    condition     = var.helm_timeout_seconds >= 60 && var.helm_timeout_seconds <= 3600
    error_message = "helm_timeout_seconds must be between 60 and 3600."
  }
}

variable "extra_tags" {
  description = "Tags merged onto every AWS resource, on top of the provider default_tags. Use it for cost centre, owner or ticket references - cost allocation in AWS is tag-driven, so an untagged resource is one nobody can attribute a bill to."
  type        = map(string)
  default     = {}
}
