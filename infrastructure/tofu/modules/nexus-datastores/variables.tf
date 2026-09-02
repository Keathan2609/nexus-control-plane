variable "namespace" {
  description = "Kubernetes namespace the datastores are created in. Must already exist; this module deliberately does not create it so that namespace ownership stays with the platform module."
  type        = string

  validation {
    condition     = can(regex("^[a-z0-9]([-a-z0-9]*[a-z0-9])?$", var.namespace)) && length(var.namespace) <= 63
    error_message = "namespace must be a valid RFC 1123 DNS label: lowercase alphanumerics and dashes, max 63 chars."
  }
}

variable "name_prefix" {
  description = "Prefix for every object this module creates. Lets two independent stacks coexist in one namespace during a migration."
  type        = string
  default     = "nexus"

  validation {
    condition     = can(regex("^[a-z0-9]([-a-z0-9]*[a-z0-9])?$", var.name_prefix))
    error_message = "name_prefix must be a valid RFC 1123 DNS label."
  }
}

variable "labels" {
  description = "Extra labels merged into every object. Use it for cost-centre or ownership labels; the app.kubernetes.io/* set is managed by the module."
  type        = map(string)
  default     = {}
}

# ---------------------------------------------------------------------------
# PostgreSQL
# ---------------------------------------------------------------------------

variable "postgres_image" {
  description = "PostgreSQL container image. NEXUS targets PostgreSQL 16; running anything else is unsupported."
  type        = string
  default     = "postgres:16-alpine"

  validation {
    # Catches the most common mistake - somebody bumping to 15 or 17 to match
    # whatever is on their laptop - without pretending to parse image refs.
    condition     = can(regex(":16", var.postgres_image))
    error_message = "postgres_image must be a PostgreSQL 16 tag; the NEXUS migrations assume 16.x."
  }
}

variable "postgres_database" {
  description = "Database created on first boot and used by every NEXUS service."
  type        = string
  default     = "nexus"

  validation {
    condition     = can(regex("^[a-z_][a-z0-9_]*$", var.postgres_database))
    error_message = "postgres_database must be a lowercase identifier usable unquoted in SQL."
  }
}

variable "postgres_username" {
  description = "Role created on first boot. NEXUS runs its own migrations, so it needs DDL rights on its database."
  type        = string
  default     = "nexus"

  validation {
    condition     = can(regex("^[a-z_][a-z0-9_]*$", var.postgres_username))
    error_message = "postgres_username must be a lowercase identifier usable unquoted in SQL."
  }
}

variable "postgres_password" {
  description = "Password for postgres_username. Leave null and one is generated with random_password and stored only in the Kubernetes Secret. Set it only when something outside this stack must know the value up front."
  type        = string
  default     = null
  sensitive   = true

  validation {
    condition     = var.postgres_password == null ? true : length(var.postgres_password) >= 16
    error_message = "postgres_password must be at least 16 characters when supplied explicitly."
  }
}

variable "postgres_storage_size" {
  description = "PersistentVolumeClaim size for the PostgreSQL data directory."
  type        = string
  default     = "8Gi"

  validation {
    condition     = can(regex("^[0-9]+(Mi|Gi|Ti)$", var.postgres_storage_size))
    error_message = "postgres_storage_size must be a Kubernetes binary quantity such as 512Mi, 8Gi or 1Ti."
  }
}

variable "postgres_resources" {
  description = "CPU and memory requests/limits for the PostgreSQL container."
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

  # Sized for a laptop cluster. kind and Docker Desktop give the whole VM a
  # couple of cores; asking for more here just leaves the pod Pending forever,
  # which is a baffling first experience for someone following the README.
  default = {
    requests = {
      cpu    = "100m"
      memory = "256Mi"
    }
    limits = {
      cpu    = "1000m"
      memory = "1Gi"
    }
  }

  validation {
    condition     = alltrue([for v in [var.postgres_resources.requests.cpu, var.postgres_resources.limits.cpu] : can(regex("^[0-9]+m?$", v))])
    error_message = "CPU quantities must look like 100m or 2."
  }

  validation {
    condition     = alltrue([for v in [var.postgres_resources.requests.memory, var.postgres_resources.limits.memory] : can(regex("^[0-9]+(Mi|Gi)$", v))])
    error_message = "Memory quantities must look like 256Mi or 1Gi."
  }
}

variable "postgres_run_as_user" {
  description = "UID/GID the PostgreSQL container runs as. This is image-specific: the Alpine images use 70, the Debian-based ones use 999. Getting it wrong shows up as initdb failing on volume permissions, so it moves with postgres_image."
  type        = number
  default     = 70

  validation {
    condition     = var.postgres_run_as_user > 0 && var.postgres_run_as_user < 65536
    error_message = "postgres_run_as_user must be a non-root UID below 65536."
  }
}

variable "postgres_max_connections" {
  description = "PostgreSQL max_connections. Must comfortably exceed (api replicas + worker replicas + reconciler) multiplied by the per-process pool size."
  type        = number
  default     = 100

  validation {
    condition     = var.postgres_max_connections >= 25 && var.postgres_max_connections <= 1000
    error_message = "postgres_max_connections must be between 25 and 1000."
  }
}

# ---------------------------------------------------------------------------
# NATS / JetStream
# ---------------------------------------------------------------------------

variable "nats_image" {
  description = "NATS server image. NEXUS targets NATS 2.10 with JetStream enabled."
  type        = string
  default     = "nats:2.10-alpine"

  validation {
    condition     = can(regex(":2[.]10", var.nats_image))
    error_message = "nats_image must be a NATS 2.10 tag; the JetStream APIs NEXUS uses need 2.10+."
  }
}

variable "nats_replicas" {
  description = "NATS server count. 1 is right for local work. JetStream replication is Raft-based, so a real cluster needs an odd number greater than one."
  type        = number
  default     = 1

  validation {
    condition     = contains([1, 3, 5], var.nats_replicas)
    error_message = "nats_replicas must be 1 (single node, no replication) or 3/5 (Raft quorum). Even counts cannot form a quorum, and 2 is strictly worse than 1."
  }
}

variable "nats_storage_size" {
  description = "PersistentVolumeClaim size for the JetStream file store, per server."
  type        = string
  default     = "4Gi"

  validation {
    condition     = can(regex("^[0-9]+(Mi|Gi|Ti)$", var.nats_storage_size))
    error_message = "nats_storage_size must be a Kubernetes binary quantity such as 512Mi or 4Gi."
  }
}

variable "nats_resources" {
  description = "CPU and memory requests/limits for the NATS container."
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
    condition     = alltrue([for v in [var.nats_resources.requests.cpu, var.nats_resources.limits.cpu] : can(regex("^[0-9]+m?$", v))])
    error_message = "CPU quantities must look like 50m or 1."
  }

  validation {
    condition     = alltrue([for v in [var.nats_resources.requests.memory, var.nats_resources.limits.memory] : can(regex("^[0-9]+(Mi|Gi)$", v))])
    error_message = "Memory quantities must look like 128Mi or 1Gi."
  }
}

variable "storage_class_name" {
  description = "StorageClass for both PVCs. null uses the cluster default, which is what kind (rancher.io/local-path) and Docker Desktop (hostpath) already provide."
  type        = string
  default     = null
}

variable "sslmode" {
  description = "sslmode used in the generated DSN. The in-cluster PostgreSQL here serves plaintext, so a local cluster needs 'disable'. Anything reachable off-node should use require or stronger."
  type        = string
  default     = "disable"

  validation {
    condition     = contains(["disable", "require", "verify-ca", "verify-full"], var.sslmode)
    error_message = "sslmode must be one of: disable, require, verify-ca, verify-full."
  }
}
