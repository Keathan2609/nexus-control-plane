# ---------------------------------------------------------------------------
# Placement
# ---------------------------------------------------------------------------

variable "namespace" {
  description = "Namespace the NEXUS workloads run in."
  type        = string
  default     = "nexus"

  validation {
    condition     = can(regex("^[a-z0-9]([-a-z0-9]*[a-z0-9])?$", var.namespace)) && length(var.namespace) <= 63
    error_message = "namespace must be a valid RFC 1123 DNS label: lowercase alphanumerics and dashes, max 63 chars."
  }
}

variable "create_namespace" {
  description = "Create the namespace. Set false when the namespace is owned elsewhere - a cluster-bootstrap stack, or a platform team that pre-provisions namespaces with quotas and policies already attached."
  type        = bool
  default     = true
}

variable "pod_security_standard" {
  description = "Pod Security Admission level enforced on the namespace. Only applied when this module creates the namespace."
  type        = string
  default     = "baseline"

  validation {
    condition     = contains(["privileged", "baseline", "restricted"], var.pod_security_standard)
    error_message = "pod_security_standard must be one of: privileged, baseline, restricted."
  }
}

variable "release_name" {
  description = "Helm release name, and the prefix for the ConfigMap and Secret this module owns."
  type        = string
  default     = "nexus"

  validation {
    condition     = can(regex("^[a-z0-9]([-a-z0-9]*[a-z0-9])?$", var.release_name)) && length(var.release_name) <= 40
    error_message = "release_name must be a valid RFC 1123 DNS label of at most 40 chars, so generated object names stay under the 63-char limit."
  }
}

variable "environment" {
  description = "Value of NEXUS_ENVIRONMENT. NEXUS uses it for log tagging, metric labels and to decide whether development-only endpoints are exposed, so a typo here is not cosmetic."
  type        = string

  validation {
    condition     = contains(["local", "development", "staging", "production"], var.environment)
    error_message = "environment must be one of: local, development, staging, production. The set is closed because NEXUS branches on these exact strings; a typo would silently land in the permissive default."
  }
}

variable "labels" {
  description = "Extra labels merged into every object this module owns."
  type        = map(string)
  default     = {}
}

# ---------------------------------------------------------------------------
# Chart
# ---------------------------------------------------------------------------

variable "chart_path" {
  description = "Path to the NEXUS Helm chart, normally deployments/helm/nexus in this repo. Pass an ABSOLUTE path: Helm resolves a relative chart path against the process working directory, not the module directory, so a relative value works until someone runs tofu from a different folder. Root modules build it with abspath(). An OCI or HTTP chart reference works too, in which case set chart_version."
  type        = string
}

variable "chart_version" {
  description = "Chart version constraint. Only meaningful for a chart pulled from a repository; ignored for a local directory."
  type        = string
  default     = null
}

variable "deploy_workloads" {
  description = "Install the Helm release. Setting this false still creates the namespace and the Secret, which is how you stand up the surrounding infrastructure before the chart is ready (or during an incident when you want the credentials in place but the pods gone). The ConfigMap is rendered by the chart, so it is absent while this is false."
  type        = bool
  default     = true
}

variable "helm_timeout_seconds" {
  description = "How long Helm waits for the release to become ready. The default is generous because a cold local cluster pulls four images before anything reports Ready."
  type        = number
  default     = 600

  validation {
    condition     = var.helm_timeout_seconds >= 60 && var.helm_timeout_seconds <= 3600
    error_message = "helm_timeout_seconds must be between 60 and 3600."
  }
}

variable "atomic" {
  description = "Roll the release back if it fails to become ready. True is right for CI and production. Turn it off when debugging a broken deploy, because a rollback deletes the very pods whose logs you need."
  type        = bool
  default     = true
}

variable "extra_helm_values" {
  description = "Arbitrary values merged over the ones this module computes. Escape hatch for chart features the module does not model yet; anything used more than twice should become a real variable."
  type        = any
  default     = {}
}

# ---------------------------------------------------------------------------
# Images
# ---------------------------------------------------------------------------

variable "image_registry" {
  description = "Registry and repository prefix for the four NEXUS images. For a kind cluster this is usually a bare local prefix whose images were side-loaded with `kind load docker-image`."
  type        = string
  default     = "ghcr.io/nexus"
}

variable "image_tag" {
  description = "Tag applied to all four NEXUS images. Keeping them on one tag is intentional: the API, worker and reconciler share database schema and NATS subjects, so mixing versions across them is a migration, not a deploy."
  type        = string
  default     = "0.1.0"

  validation {
    condition     = var.image_tag != "latest"
    error_message = "image_tag must not be 'latest'. A floating tag makes the running version unknowable and turns any pod restart into an unplanned upgrade."
  }
}

variable "image_pull_policy" {
  description = "Image pull policy. IfNotPresent is required for images side-loaded into kind, which have no registry to be pulled from."
  type        = string
  default     = "IfNotPresent"

  validation {
    condition     = contains(["Always", "IfNotPresent", "Never"], var.image_pull_policy)
    error_message = "image_pull_policy must be one of: Always, IfNotPresent, Never."
  }
}

variable "image_pull_secrets" {
  description = "Names of pre-existing imagePullSecrets in the namespace. Empty for public or side-loaded images."
  type        = list(string)
  default     = []
}

# ---------------------------------------------------------------------------
# Workload sizing
# ---------------------------------------------------------------------------

variable "api_replicas" {
  description = "nexus-api replicas. Stateless HTTP, so scale it for request volume and for surviving a node drain."
  type        = number
  default     = 2

  validation {
    condition     = var.api_replicas >= 1 && var.api_replicas <= 50 && floor(var.api_replicas) == var.api_replicas
    error_message = "api_replicas must be a whole number between 1 and 50."
  }
}

variable "worker_replicas" {
  description = "nexus-worker replicas. Each worker leases jobs independently, so this scales horizontally - but watch postgres_max_connections, since every replica opens a pool."
  type        = number
  default     = 2

  validation {
    condition     = var.worker_replicas >= 1 && var.worker_replicas <= 100 && floor(var.worker_replicas) == var.worker_replicas
    error_message = "worker_replicas must be a whole number between 1 and 100."
  }
}

variable "reconciler_replicas" {
  description = "nexus-reconciler replicas. This is a single-writer control loop with no leader election, so the only supported value is 1."
  type        = number
  default     = 1

  validation {
    # Deliberately not a range. Two reconcilers both believe they own the
    # desired state and will fight over the same rows, producing a flapping
    # loop that looks like a NEXUS bug rather than a deployment mistake. The
    # day leader election lands, this validation is the thing that gets
    # relaxed, and reviewers will see why.
    condition     = var.reconciler_replicas == 1
    error_message = "reconciler_replicas must be 1: nexus-reconciler is a single control loop with no leader election, and a second replica causes duplicate reconciliation."
  }
}

variable "web_replicas" {
  description = "nexus-web (Next.js console) replicas."
  type        = number
  default     = 1

  validation {
    condition     = var.web_replicas >= 1 && var.web_replicas <= 20 && floor(var.web_replicas) == var.web_replicas
    error_message = "web_replicas must be a whole number between 1 and 20."
  }
}

variable "resources" {
  description = "Per-component CPU and memory requests/limits, keyed by component name (api, worker, reconciler, web)."
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

  # Defaults target a laptop cluster. The worker gets the largest allowance
  # because it forks pipeline steps; the reconciler is a poll loop and needs
  # very little.
  default = {
    api = {
      requests = { cpu = "100m", memory = "128Mi" }
      limits   = { cpu = "1000m", memory = "512Mi" }
    }
    worker = {
      requests = { cpu = "200m", memory = "256Mi" }
      limits   = { cpu = "2000m", memory = "1Gi" }
    }
    reconciler = {
      requests = { cpu = "50m", memory = "64Mi" }
      limits   = { cpu = "500m", memory = "256Mi" }
    }
    web = {
      requests = { cpu = "100m", memory = "128Mi" }
      limits   = { cpu = "500m", memory = "512Mi" }
    }
  }

  validation {
    condition     = length(setsubtract(keys(var.resources), ["api", "worker", "reconciler", "web"])) == 0
    error_message = "resources may only contain the keys api, worker, reconciler and web. A key for a component that does not exist is silently ignored by the chart, so it is rejected here instead."
  }

  validation {
    condition     = length(setsubtract(["api", "worker", "reconciler", "web"], keys(var.resources))) == 0
    error_message = "resources must define all four components: api, worker, reconciler, web."
  }

  validation {
    condition = alltrue([
      for c in values(var.resources) : alltrue([
        can(regex("^[0-9]+m?$", c.requests.cpu)),
        can(regex("^[0-9]+m?$", c.limits.cpu)),
        can(regex("^[0-9]+(Mi|Gi)$", c.requests.memory)),
        can(regex("^[0-9]+(Mi|Gi)$", c.limits.memory)),
      ])
    ])
    error_message = "CPU quantities must look like 100m or 2, and memory quantities like 128Mi or 1Gi."
  }
}

# ---------------------------------------------------------------------------
# Application configuration (rendered into the ConfigMap)
# ---------------------------------------------------------------------------

variable "http_port" {
  description = "Port nexus-api listens on, used for both NEXUS_HTTP_ADDR and the Service."
  type        = number
  default     = 8080

  validation {
    condition     = var.http_port > 1024 && var.http_port < 65536
    error_message = "http_port must be between 1025 and 65535. Ports below 1024 need CAP_NET_BIND_SERVICE, and these containers run as non-root without it."
  }
}

variable "metrics_port" {
  description = "Port every Go service exposes /metrics on, used for NEXUS_METRICS_ADDR."
  type        = number
  default     = 9090

  validation {
    condition     = var.metrics_port > 1024 && var.metrics_port < 65536
    error_message = "metrics_port must be between 1025 and 65535."
  }
}

variable "web_port" {
  description = "Port the Next.js console listens on."
  type        = number
  default     = 3000

  validation {
    condition     = var.web_port > 1024 && var.web_port < 65536
    error_message = "web_port must be between 1025 and 65535."
  }
}

variable "log_level" {
  description = "NEXUS_LOG_LEVEL."
  type        = string
  default     = "info"

  validation {
    condition     = contains(["debug", "info", "warn", "error"], var.log_level)
    error_message = "log_level must be one of: debug, info, warn, error."
  }
}

variable "otel_endpoint" {
  description = "NEXUS_OTEL_ENDPOINT, an OTLP gRPC collector address. Empty string disables tracing export, which is the right default for a laptop with no collector running."
  type        = string
  default     = ""

  validation {
    condition     = var.otel_endpoint == "" || can(regex("^(http://|https://|[a-z0-9.-]+:[0-9]+)", var.otel_endpoint))
    error_message = "otel_endpoint must be empty, an http(s) URL, or a host:port pair."
  }
}

variable "worker_concurrency" {
  description = "NEXUS_WORKER_CONCURRENCY - in-flight jobs per worker replica. Pipeline work is IO-bound (git, registry, network), so this can exceed the CPU limit without thrashing; raise it together with the worker memory limit."
  type        = number
  default     = 8

  validation {
    condition     = var.worker_concurrency >= 1 && var.worker_concurrency <= 256 && floor(var.worker_concurrency) == var.worker_concurrency
    error_message = "worker_concurrency must be a whole number between 1 and 256."
  }
}

variable "worker_lease_seconds" {
  description = "NEXUS_WORKER_LEASE_SECONDS - how long a job stays leased before another worker may reclaim it. Must outlive the worst-case pause between heartbeats but stay under the pod termination grace period, so jobs from a killed pod are picked up promptly rather than stranded."
  type        = number
  default     = 60

  validation {
    condition     = var.worker_lease_seconds >= 15 && var.worker_lease_seconds <= 900 && floor(var.worker_lease_seconds) == var.worker_lease_seconds
    error_message = "worker_lease_seconds must be a whole number between 15 and 900. Below 15s a slow GC pause looks like a dead worker and jobs get run twice."
  }
}

# ---------------------------------------------------------------------------
# Secrets. No defaults, no literals in this file.
# ---------------------------------------------------------------------------

variable "database_url" {
  description = "NEXUS_DATABASE_URL. Normally wired straight from the nexus-datastores module output, or from RDS in the cloud path."
  type        = string
  sensitive   = true

  validation {
    condition     = can(regex("^postgres(ql)?://", var.database_url))
    error_message = "database_url must be a postgres:// or postgresql:// DSN."
  }
}

variable "nats_url" {
  description = "NEXUS_NATS_URL. Comma-separated seed URLs are accepted for a clustered NATS."
  type        = string

  validation {
    condition     = can(regex("^nats://", var.nats_url))
    error_message = "nats_url must start with nats://."
  }
}

variable "auth_jwt_secret" {
  description = "NEXUS_AUTH_JWT_SECRET, the symmetric signing key for console and API sessions. Leave null to generate one. Set it explicitly only when tokens must stay valid across a rebuild of this stack, or when another system verifies the same tokens - regenerating it logs everyone out."
  type        = string
  default     = null
  sensitive   = true

  validation {
    condition     = var.auth_jwt_secret == null ? true : length(var.auth_jwt_secret) >= 32
    error_message = "auth_jwt_secret must be at least 32 characters when supplied explicitly."
  }
}

variable "webhook_secret" {
  description = "NEXUS_WEBHOOK_SECRET, the shared secret for inbound SCM webhook HMAC verification. Leave null to generate one, then configure the same value in GitHub/GitLab. Changing it makes every existing webhook fail verification until it is updated on both sides."
  type        = string
  default     = null
  sensitive   = true

  validation {
    condition     = var.webhook_secret == null ? true : length(var.webhook_secret) >= 32
    error_message = "webhook_secret must be at least 32 characters when supplied explicitly."
  }
}

variable "extra_secret_data" {
  description = "Additional key/value pairs merged into the NEXUS Secret, for credentials this module does not model (registry tokens, SCM app keys). Values are written to the Secret verbatim."
  type        = map(string)
  default     = {}
  sensitive   = true
}
