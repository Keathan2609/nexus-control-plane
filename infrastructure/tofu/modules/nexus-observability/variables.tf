variable "namespace" {
  description = "Namespace for Prometheus and Grafana. Kept separate from the NEXUS namespace so that deleting the application stack does not take the monitoring that explains the deletion with it."
  type        = string
  default     = "observability"

  validation {
    condition     = can(regex("^[a-z0-9]([-a-z0-9]*[a-z0-9])?$", var.namespace)) && length(var.namespace) <= 63
    error_message = "namespace must be a valid RFC 1123 DNS label."
  }
}

variable "create_namespace" {
  description = "Create the observability namespace."
  type        = bool
  default     = true
}

variable "environment" {
  description = "Environment label applied to the stack and used as the Grafana instance name."
  type        = string

  validation {
    condition     = contains(["local", "development", "staging", "production"], var.environment)
    error_message = "environment must be one of: local, development, staging, production."
  }
}

variable "monitored_namespaces" {
  description = "Namespaces whose annotated pods should be scraped. Used to build the NEXUS dashboard queries and to document scope; the Prometheus chart's pod discovery is cluster-wide and driven by the prometheus.io/scrape annotation."
  type        = list(string)
  default     = ["nexus"]

  validation {
    condition     = length(var.monitored_namespaces) > 0
    error_message = "monitored_namespaces must list at least one namespace."
  }
}

variable "labels" {
  description = "Extra labels merged into every object this module owns."
  type        = map(string)
  default     = {}
}

# ---------------------------------------------------------------------------
# Prometheus
# ---------------------------------------------------------------------------

variable "prometheus_release_name" {
  description = "Helm release name for Prometheus. Service names are derived from it, so changing it changes the Grafana datasource URL too."
  type        = string
  default     = "prometheus"

  validation {
    condition     = can(regex("^[a-z0-9]([-a-z0-9]*[a-z0-9])?$", var.prometheus_release_name))
    error_message = "prometheus_release_name must be a valid RFC 1123 DNS label."
  }
}

variable "prometheus_chart_version" {
  description = "Version of the prometheus-community/prometheus chart. null takes whatever the repository currently offers, which is acceptable for a laptop and not acceptable for anything shared - pin it from `helm search repo prometheus-community/prometheus --versions` before this is used by more than one person."
  type        = string
  default     = null
}

variable "prometheus_retention" {
  description = "How long Prometheus keeps samples. Short by default: on a local cluster the storage is a hostPath on your own disk."
  type        = string
  default     = "7d"

  validation {
    condition     = can(regex("^[0-9]+(h|d|w|y)$", var.prometheus_retention))
    error_message = "prometheus_retention must look like 12h, 7d, 4w or 1y."
  }
}

variable "prometheus_storage_size" {
  description = "PersistentVolumeClaim size for the Prometheus TSDB."
  type        = string
  default     = "8Gi"

  validation {
    condition     = can(regex("^[0-9]+(Mi|Gi|Ti)$", var.prometheus_storage_size))
    error_message = "prometheus_storage_size must be a Kubernetes binary quantity such as 8Gi."
  }
}

variable "prometheus_persistence_enabled" {
  description = "Give Prometheus a PVC. Turning it off makes the whole stack ephemeral, which is a reasonable choice on a cluster you recreate daily and a bad one anywhere else."
  type        = bool
  default     = true
}

variable "prometheus_resources" {
  description = "CPU and memory requests/limits for the Prometheus server container."
  type = object({
    requests = object({
      cpu    = string
      memory = string
    })
    limits = object({
      cpu    = string
      memory = string
    })
  })

  default = {
    requests = {
      cpu    = "100m"
      memory = "512Mi"
    }
    limits = {
      cpu    = "1000m"
      memory = "2Gi"
    }
  }

  validation {
    condition = alltrue([
      can(regex("^[0-9]+m?$", var.prometheus_resources.requests.cpu)),
      can(regex("^[0-9]+m?$", var.prometheus_resources.limits.cpu)),
      can(regex("^[0-9]+(Mi|Gi)$", var.prometheus_resources.requests.memory)),
      can(regex("^[0-9]+(Mi|Gi)$", var.prometheus_resources.limits.memory)),
    ])
    error_message = "CPU quantities must look like 100m or 2, and memory quantities like 512Mi or 2Gi."
  }
}

variable "enable_alertmanager" {
  description = "Install Alertmanager. Off by default because nothing on a laptop is on call, and an Alertmanager with no receivers configured is just another pod competing for the same two cores."
  type        = bool
  default     = false
}

variable "enable_node_exporter" {
  description = "Install node-exporter. Off by default: it runs as a DaemonSet with hostPath mounts and host networking, which is a lot of privilege to grant for host metrics you can already see in Docker Desktop on a single-node cluster."
  type        = bool
  default     = false
}

variable "enable_kube_state_metrics" {
  description = "Install kube-state-metrics. On by default - it is what turns 'the pod is unhealthy' into 'the pod is CrashLoopBackOff and has restarted 14 times', which is most of the value of this module."
  type        = bool
  default     = true
}

# ---------------------------------------------------------------------------
# Grafana
# ---------------------------------------------------------------------------

variable "enable_grafana" {
  description = "Install Grafana. Prometheus alone is usable through its own expression browser, so this can be turned off on a memory-constrained machine."
  type        = bool
  default     = true
}

variable "grafana_release_name" {
  description = "Helm release name for Grafana."
  type        = string
  default     = "grafana"

  validation {
    condition     = can(regex("^[a-z0-9]([-a-z0-9]*[a-z0-9])?$", var.grafana_release_name))
    error_message = "grafana_release_name must be a valid RFC 1123 DNS label."
  }
}

variable "grafana_chart_version" {
  description = "Version of the grafana/grafana chart. null takes whatever the repository currently offers; pin it for anything shared."
  type        = string
  default     = null
}

variable "grafana_admin_user" {
  description = "Grafana admin username."
  type        = string
  default     = "admin"
}

variable "grafana_admin_password" {
  description = "Grafana admin password. Leave null to generate one; read it back from the module output. There is no default password in this repository, because a default password is the one that never gets changed."
  type        = string
  default     = null
  sensitive   = true

  validation {
    condition     = var.grafana_admin_password == null ? true : length(var.grafana_admin_password) >= 12
    error_message = "grafana_admin_password must be at least 12 characters when supplied explicitly."
  }
}

variable "grafana_persistence_enabled" {
  description = "Give Grafana a PVC. Dashboards provisioned by this module are recreated on every start, so this only matters for dashboards someone edits in the UI."
  type        = bool
  default     = false
}

variable "grafana_storage_size" {
  description = "PersistentVolumeClaim size for Grafana, when persistence is enabled."
  type        = string
  default     = "2Gi"

  validation {
    condition     = can(regex("^[0-9]+(Mi|Gi)$", var.grafana_storage_size))
    error_message = "grafana_storage_size must be a Kubernetes binary quantity such as 2Gi."
  }
}

variable "grafana_resources" {
  description = "CPU and memory requests/limits for Grafana."
  type = object({
    requests = object({
      cpu    = string
      memory = string
    })
    limits = object({
      cpu    = string
      memory = string
    })
  })

  default = {
    requests = {
      cpu    = "50m"
      memory = "128Mi"
    }
    limits = {
      cpu    = "500m"
      memory = "512Mi"
    }
  }

  validation {
    condition = alltrue([
      can(regex("^[0-9]+m?$", var.grafana_resources.requests.cpu)),
      can(regex("^[0-9]+m?$", var.grafana_resources.limits.cpu)),
      can(regex("^[0-9]+(Mi|Gi)$", var.grafana_resources.requests.memory)),
      can(regex("^[0-9]+(Mi|Gi)$", var.grafana_resources.limits.memory)),
    ])
    error_message = "CPU quantities must look like 50m or 1, and memory quantities like 128Mi or 512Mi."
  }
}

variable "storage_class_name" {
  description = "StorageClass for the Prometheus and Grafana PVCs. null uses the cluster default."
  type        = string
  default     = null
}

variable "helm_timeout_seconds" {
  description = "How long Helm waits for each observability release to become ready."
  type        = number
  default     = 600

  validation {
    condition     = var.helm_timeout_seconds >= 60 && var.helm_timeout_seconds <= 3600
    error_message = "helm_timeout_seconds must be between 60 and 3600."
  }
}
