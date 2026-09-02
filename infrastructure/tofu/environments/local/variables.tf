# ---------------------------------------------------------------------------
# Cluster access
# ---------------------------------------------------------------------------

variable "kube_context" {
  description = "kubeconfig context to deploy into. Required with no default on purpose: defaulting to the current context means this applies to whatever kubectl happened to be pointed at last, and the whole design of this root module assumes a throwaway cluster."
  type        = string

  validation {
    # A guard rail, not a security control - anyone can rename a context. It
    # exists to catch the specific accident of running the local stack against
    # a shared cluster, which is easy to do and unpleasant to undo. If your
    # local cluster genuinely has another name, widen this pattern in the same
    # commit that explains why.
    condition     = can(regex("^(kind-|k3d-|minikube$|docker-desktop$|rancher-desktop$|colima$|orbstack$)", var.kube_context))
    error_message = "kube_context does not look like a local cluster (expected kind-*, k3d-*, minikube, docker-desktop, rancher-desktop, colima or orbstack). This root module creates a single-replica database with no backups; point it at a real cluster only after reading environments/production."
  }
}

variable "kubeconfig_path" {
  description = "Path to the kubeconfig file. Tilde is expanded, so the default works on Windows, macOS and Linux alike."
  type        = string
  default     = "~/.kube/config"
}

# ---------------------------------------------------------------------------
# Placement and sizing
# ---------------------------------------------------------------------------

variable "namespace" {
  description = "Namespace for the NEXUS workloads and their datastores."
  type        = string
  default     = "nexus"

  validation {
    condition     = can(regex("^[a-z0-9]([-a-z0-9]*[a-z0-9])?$", var.namespace)) && length(var.namespace) <= 63
    error_message = "namespace must be a valid RFC 1123 DNS label."
  }
}

variable "storage_class_name" {
  description = "StorageClass for every PVC in the stack. null uses the cluster default, which is what kind and Docker Desktop ship with."
  type        = string
  default     = null
}

variable "image_registry" {
  description = "Registry prefix for the NEXUS images. The default assumes images were built locally and side-loaded (`kind load docker-image nexus/nexus-api:dev`), so nothing needs to be pushed anywhere."
  type        = string
  default     = "nexus"
}

variable "image_tag" {
  description = "Tag for all four NEXUS images."
  type        = string
  default     = "dev"

  validation {
    condition     = var.image_tag != "latest"
    error_message = "image_tag must not be 'latest'; a floating tag makes the running version unknowable."
  }
}

variable "api_replicas" {
  description = "nexus-api replicas. One is plenty locally; two is useful for watching a rolling update behave."
  type        = number
  default     = 1

  validation {
    condition     = var.api_replicas >= 1 && var.api_replicas <= 5
    error_message = "api_replicas must be between 1 and 5 in the local environment. Higher numbers just starve the node."
  }
}

variable "worker_replicas" {
  description = "nexus-worker replicas."
  type        = number
  default     = 1

  validation {
    condition     = var.worker_replicas >= 1 && var.worker_replicas <= 5
    error_message = "worker_replicas must be between 1 and 5 in the local environment."
  }
}

variable "web_replicas" {
  description = "nexus-web replicas."
  type        = number
  default     = 1

  validation {
    condition     = var.web_replicas >= 1 && var.web_replicas <= 3
    error_message = "web_replicas must be between 1 and 3 in the local environment."
  }
}

variable "log_level" {
  description = "NEXUS_LOG_LEVEL. debug is a defensible default locally."
  type        = string
  default     = "debug"

  validation {
    condition     = contains(["debug", "info", "warn", "error"], var.log_level)
    error_message = "log_level must be one of: debug, info, warn, error."
  }
}

variable "worker_concurrency" {
  description = "NEXUS_WORKER_CONCURRENCY. Lower than the production default because a laptop runs the whole cluster."
  type        = number
  default     = 4

  validation {
    condition     = var.worker_concurrency >= 1 && var.worker_concurrency <= 32
    error_message = "worker_concurrency must be between 1 and 32 in the local environment."
  }
}

# ---------------------------------------------------------------------------
# Feature switches
# ---------------------------------------------------------------------------

variable "deploy_workloads" {
  description = "Install the NEXUS Helm release. Set false to bring up only the namespace, datastores and secrets - which is what you want when running the Go services on the host against the in-cluster database."
  type        = bool
  default     = true
}

variable "chart_path" {
  description = "Override the path to the NEXUS Helm chart. null resolves to deployments/helm/nexus in this repository, which is the normal case."
  type        = string
  default     = null
}

variable "enable_observability" {
  description = "Install Prometheus and Grafana. Off by default: it roughly doubles the memory the stack needs, and the first thing most people want is a working API, not a dashboard."
  type        = bool
  default     = false
}

variable "postgres_storage_size" {
  description = "PVC size for PostgreSQL."
  type        = string
  default     = "4Gi"

  validation {
    condition     = can(regex("^[0-9]+(Mi|Gi)$", var.postgres_storage_size))
    error_message = "postgres_storage_size must be a Kubernetes binary quantity such as 4Gi."
  }
}

variable "nats_storage_size" {
  description = "PVC size for the JetStream file store."
  type        = string
  default     = "2Gi"

  validation {
    condition     = can(regex("^[0-9]+(Mi|Gi)$", var.nats_storage_size))
    error_message = "nats_storage_size must be a Kubernetes binary quantity such as 2Gi."
  }
}
