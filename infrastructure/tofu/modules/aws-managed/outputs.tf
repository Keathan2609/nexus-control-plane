output "database_endpoint" {
  description = "RDS endpoint in host:port form."
  value       = aws_db_instance.this.endpoint
}

output "database_address" {
  description = "RDS hostname without the port."
  value       = aws_db_instance.this.address
}

output "database_port" {
  description = "RDS port."
  value       = aws_db_instance.this.port
}

output "database_name" {
  description = "Database created on the instance."
  value       = aws_db_instance.this.db_name
}

output "database_username" {
  description = "Master username."
  value       = var.master_username
}

output "database_password" {
  description = "Master password, generated unless one was supplied."
  value       = local.master_password
  sensitive   = true
}

# Wired straight into the nexus-platform module's database_url input. sslmode
# is require because the parameter group sets rds.force_ssl.
output "database_url" {
  description = "Complete PostgreSQL DSN for NEXUS_DATABASE_URL."
  value       = local.database_url
  sensitive   = true
}

output "database_secret_arn" {
  description = "Secrets Manager secret holding the credentials and DSN, or null when Secrets Manager storage is disabled. This is what an external-secrets operator should point at."
  value       = one(aws_secretsmanager_secret.database[*].arn)
}

output "database_security_group_id" {
  description = "Security group in front of the database. Add ingress rules here rather than editing the module when a new client needs access."
  value       = aws_security_group.database.id
}

output "ecr_repository_urls" {
  description = "Map of image name to ECR repository URL, for docker push and for the chart's image registry setting."
  value       = { for k, r in aws_ecr_repository.this : k => r.repository_url }
}

output "ecr_repository_arns" {
  description = "Map of image name to repository ARN, for attaching additional repository policies."
  value       = { for k, r in aws_ecr_repository.this : k => r.arn }
}

output "ecr_registry" {
  description = "Registry host and prefix to hand to the platform module as image_registry."
  value       = length(var.ecr_repositories) > 0 ? dirname(values(aws_ecr_repository.this)[0].repository_url) : null
}

output "ecr_push_policy_arn" {
  description = "IAM policy granting push access to these repositories. Attach it to whatever identity CI uses; this module intentionally does not create that identity."
  value       = aws_iam_policy.ecr_push.arn
}

output "irsa_role_arns" {
  description = "Map of Kubernetes service account name to IAM role ARN. Each one goes on the matching ServiceAccount as the eks.amazonaws.com/role-arn annotation."
  value       = { for k, r in aws_iam_role.irsa : k => r.arn }
}

output "irsa_service_account_annotations" {
  description = "Ready-made annotation maps, keyed by service account name, to merge into the chart's serviceAccount.annotations values."
  value = {
    for k, r in aws_iam_role.irsa : k => {
      "eks.amazonaws.com/role-arn" = r.arn
    }
  }
}
