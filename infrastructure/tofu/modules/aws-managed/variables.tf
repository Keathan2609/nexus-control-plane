# ---------------------------------------------------------------------------
# OPTIONAL MODULE. Nothing in environments/local references it.
#
# Everything declared here costs money the moment it is applied. See the cost
# section of ../../README.md for current order-of-magnitude figures.
# ---------------------------------------------------------------------------

variable "name_prefix" {
  description = "Prefix for every AWS resource name. Include the environment if one account hosts more than one NEXUS installation."
  type        = string
  default     = "nexus"

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{1,24}$", var.name_prefix))
    error_message = "name_prefix must be lowercase alphanumerics and dashes, 2-25 chars, starting with a letter. RDS identifiers and ECR repository names are both fussy, and 25 chars leaves room for suffixes."
  }
}

variable "environment" {
  description = "Environment this deployment serves. Tagged onto everything and used in resource names."
  type        = string

  validation {
    condition     = contains(["development", "staging", "production"], var.environment)
    error_message = "environment must be one of: development, staging, production. 'local' is not valid here - the local environment does not use AWS."
  }
}

variable "tags" {
  description = "Tags applied to every resource. Cost allocation in AWS is tag-driven, so an untagged resource is one nobody can attribute a bill to."
  type        = map(string)
  default     = {}
}

# ---------------------------------------------------------------------------
# Network. This module attaches to an existing VPC; it does not create one.
# ---------------------------------------------------------------------------

variable "vpc_id" {
  description = "VPC the database lives in. Deliberately an input: VPCs, subnets and routing outlive individual applications and normally belong to a separate network stack."
  type        = string

  validation {
    condition     = can(regex("^vpc-[0-9a-f]{8,17}$", var.vpc_id))
    error_message = "vpc_id must look like vpc-0123456789abcdef0."
  }
}

variable "database_subnet_ids" {
  description = "Subnets for the RDS subnet group. Must be PRIVATE subnets in at least two availability zones - RDS requires two AZs even for a single-AZ instance, so that a failover has somewhere to go."
  type        = list(string)

  validation {
    condition     = length(var.database_subnet_ids) >= 2
    error_message = "database_subnet_ids needs at least two subnets in different availability zones; RDS rejects a subnet group with fewer."
  }
}

variable "allowed_security_group_ids" {
  description = "Security groups permitted to reach PostgreSQL on 5432 - normally the EKS node group's security group. Preferred over CIDR blocks because it survives subnet renumbering."
  type        = list(string)
  default     = []
}

variable "allowed_cidr_blocks" {
  description = "CIDR blocks permitted to reach PostgreSQL. Use sparingly; a security group reference is more precise and does not silently widen when a subnet is resized."
  type        = list(string)
  default     = []

  validation {
    condition     = alltrue([for c in var.allowed_cidr_blocks : !startswith(c, "0.0.0.0")])
    error_message = "allowed_cidr_blocks must not contain 0.0.0.0/0. A database open to the internet is never the intended configuration."
  }
}

# ---------------------------------------------------------------------------
# RDS
# ---------------------------------------------------------------------------

variable "postgres_version" {
  description = "PostgreSQL engine version. A bare major ('16') lets RDS pick the current minor at create time; pin the full version once the installation is real, so that a rebuild reproduces what is running."
  type        = string
  default     = "16"

  validation {
    condition     = startswith(var.postgres_version, "16")
    error_message = "postgres_version must be a 16.x version; the NEXUS migrations assume PostgreSQL 16."
  }
}

variable "instance_class" {
  description = "RDS instance class. db.t4g.micro is the cheapest thing that works and is the right size for a trial, not for production traffic."
  type        = string
  default     = "db.t4g.micro"

  validation {
    condition     = can(regex("^db[.][a-z0-9]+[.][a-z0-9]+$", var.instance_class))
    error_message = "instance_class must look like db.t4g.micro or db.m7g.large."
  }
}

variable "allocated_storage_gb" {
  description = "Initial storage in GiB. gp3 volumes grow via max_allocated_storage_gb without downtime, so start small."
  type        = number
  default     = 20

  validation {
    condition     = var.allocated_storage_gb >= 20 && var.allocated_storage_gb <= 65536
    error_message = "allocated_storage_gb must be between 20 (the RDS minimum for gp3) and 65536."
  }
}

variable "max_allocated_storage_gb" {
  description = "Ceiling for RDS storage autoscaling. Set equal to allocated_storage_gb to disable autoscaling entirely."
  type        = number
  default     = 100

  validation {
    condition     = var.max_allocated_storage_gb >= 20
    error_message = "max_allocated_storage_gb must be at least 20."
  }
}

variable "multi_az" {
  description = "Run a synchronous standby in a second AZ. Roughly doubles the instance cost and is the difference between a failover measured in minutes and one measured in however long a restore takes."
  type        = bool
  default     = false
}

variable "backup_retention_days" {
  description = "Days of automated backups. 0 disables them, which also disables point-in-time recovery."
  type        = number
  default     = 7

  validation {
    condition     = var.backup_retention_days >= 0 && var.backup_retention_days <= 35
    error_message = "backup_retention_days must be between 0 and 35 (the RDS maximum for automated backups)."
  }
}

variable "deletion_protection" {
  description = "Refuse to delete the instance until this is turned off. On by default: `tofu destroy` should not be able to take the system of record with it in one command."
  type        = bool
  default     = true
}

variable "skip_final_snapshot" {
  description = "Skip the final snapshot on delete. True is only defensible for a scratch environment you are certain about."
  type        = bool
  default     = false
}

variable "performance_insights_enabled" {
  description = "Enable Performance Insights. The 7-day retention tier is free on supported instance classes; longer retention is billed per vCPU."
  type        = bool
  default     = true
}

variable "database_name" {
  description = "Database created on the instance."
  type        = string
  default     = "nexus"

  validation {
    condition     = can(regex("^[a-z_][a-z0-9_]*$", var.database_name))
    error_message = "database_name must be a lowercase identifier usable unquoted in SQL."
  }
}

variable "master_username" {
  description = "Master username for the instance. Not 'postgres' or 'admin' by default, since those are the two names every scanner tries first."
  type        = string
  default     = "nexus_admin"

  validation {
    condition     = can(regex("^[a-z_][a-z0-9_]{2,62}$", var.master_username)) && !contains(["postgres", "admin", "root", "rdsadmin"], var.master_username)
    error_message = "master_username must be a lowercase identifier and must not be postgres, admin, root or rdsadmin (rdsadmin is reserved by RDS)."
  }
}

variable "master_password" {
  description = "Master password. Leave null to generate one with random_password. Supply it only when an existing secret store is the source of truth."
  type        = string
  default     = null
  sensitive   = true

  validation {
    condition     = var.master_password == null ? true : length(var.master_password) >= 16
    error_message = "master_password must be at least 16 characters when supplied explicitly."
  }
}

variable "store_password_in_secrets_manager" {
  description = "Also write the DSN and credentials to a Secrets Manager secret. Worth the small monthly cost: it gives anything outside this state file (an external-secrets operator, a break-glass session) a way to read the credential without reading Tofu state."
  type        = bool
  default     = true
}

variable "secret_recovery_window_days" {
  description = "Days a deleted Secrets Manager secret stays recoverable. 0 deletes immediately, which is what you want in a scratch account where a lingering secret blocks recreating one with the same name."
  type        = number
  default     = 7

  validation {
    condition     = var.secret_recovery_window_days == 0 || (var.secret_recovery_window_days >= 7 && var.secret_recovery_window_days <= 30)
    error_message = "secret_recovery_window_days must be 0 (immediate deletion) or between 7 and 30."
  }
}

variable "kms_key_arn" {
  description = "Customer-managed KMS key for RDS storage and the Secrets Manager secret. null uses the AWS-managed key, which is free but cannot have its own key policy or rotation schedule."
  type        = string
  default     = null
}

# ---------------------------------------------------------------------------
# ECR
# ---------------------------------------------------------------------------

variable "ecr_repositories" {
  description = "Image names to create repositories for. Defaults to the four images this repository builds."
  type        = list(string)
  default     = ["nexus-api", "nexus-worker", "nexus-reconciler", "nexus-web"]

  validation {
    condition     = length(var.ecr_repositories) > 0
    error_message = "ecr_repositories must not be empty."
  }

  validation {
    condition     = alltrue([for r in var.ecr_repositories : can(regex("^[a-z0-9][a-z0-9._/-]*$", r))])
    error_message = "ECR repository names must be lowercase and may contain only letters, digits, dots, dashes, underscores and slashes."
  }
}

variable "ecr_image_tag_mutability" {
  description = "IMMUTABLE prevents a tag from being repointed at a different image. Keep it: a mutable tag means the digest running in production can change without any deploy, which breaks both rollback and audit."
  type        = string
  default     = "IMMUTABLE"

  validation {
    condition     = contains(["IMMUTABLE", "MUTABLE"], var.ecr_image_tag_mutability)
    error_message = "ecr_image_tag_mutability must be IMMUTABLE or MUTABLE."
  }
}

variable "ecr_keep_last_images" {
  description = "How many images to retain per repository before the lifecycle policy expires the oldest. Storage is billed per GB-month and container images are not small."
  type        = number
  default     = 30

  validation {
    condition     = var.ecr_keep_last_images >= 5 && var.ecr_keep_last_images <= 1000
    error_message = "ecr_keep_last_images must be between 5 and 1000. Fewer than about five leaves no room to roll back."
  }
}

variable "ecr_scan_on_push" {
  description = "Run the basic ECR vulnerability scan on push. Free, and the results are the cheapest security signal available."
  type        = bool
  default     = true
}

variable "ecr_force_delete" {
  description = "Allow deleting a repository that still contains images. Dangerous outside a scratch account: it destroys the only copy of every image you might want to roll back to."
  type        = bool
  default     = false
}

# ---------------------------------------------------------------------------
# IRSA
# ---------------------------------------------------------------------------

variable "eks_oidc_provider_arn" {
  description = "ARN of the cluster's IAM OIDC provider. null skips creating IRSA roles entirely, which is what you want if the cluster is not EKS or IRSA is not set up yet."
  type        = string
  default     = null
}

variable "eks_oidc_provider_url" {
  description = "Issuer URL of the cluster's IAM OIDC provider, without the https:// scheme (e.g. oidc.eks.eu-west-1.amazonaws.com/id/ABC123). Required when eks_oidc_provider_arn is set."
  type        = string
  default     = null

  validation {
    condition     = var.eks_oidc_provider_url == null ? true : !startswith(var.eks_oidc_provider_url, "https://")
    error_message = "eks_oidc_provider_url must not include the https:// scheme; IAM trust-policy conditions compare against the bare issuer host and path."
  }
}

variable "kubernetes_namespace" {
  description = "Namespace the NEXUS service accounts live in. Part of the IRSA trust condition, so a role cannot be assumed from another namespace."
  type        = string
  default     = "nexus"
}

variable "irsa_service_accounts" {
  description = "Service accounts that get an IAM role. The default gives the API, worker and reconciler each their own identity; nexus-web is excluded because a Next.js frontend has no business holding AWS credentials."
  type        = list(string)
  default     = ["nexus-api", "nexus-worker", "nexus-reconciler"]
}
