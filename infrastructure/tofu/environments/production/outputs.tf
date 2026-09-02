# ---------------------------------------------------------------------------
# Outputs are the supported interface to this stack for anything downstream:
# CI pipelines, runbooks, and the person on call at 3am. Sensitive values are
# marked as such - the marking suppresses console output, it does not encrypt
# anything, and every one of them is stored in clear text in state.
# ---------------------------------------------------------------------------

output "environment" {
  description = "Environment this root module manages."
  value       = var.environment
}

output "namespace" {
  description = "Namespace the NEXUS workloads run in."
  value       = kubernetes_namespace_v1.nexus.metadata[0].name
}

output "workloads_deployed" {
  description = "Whether the Helm release was installed. False means the AWS resources and the NEXUS Secret exist but no pods do."
  value       = module.platform.deployed
}

# ---------------------------------------------------------------------------
# Database
# ---------------------------------------------------------------------------

output "database_endpoint" {
  description = "RDS endpoint in host:port form."
  value       = module.aws_managed.database_endpoint
}

output "database_name" {
  description = "Database NEXUS connects to."
  value       = module.aws_managed.database_name
}

output "database_security_group_id" {
  description = "Security group in front of RDS. Add ingress rules here when a new client needs access, rather than editing the module."
  value       = module.aws_managed.database_security_group_id
}

output "database_secret_arn" {
  description = "Secrets Manager secret holding the credentials and the assembled DSN. This is what an External Secrets Operator SecretStore should point at, and what a break-glass session should read instead of the Tofu state."
  value       = module.aws_managed.database_secret_arn
}

output "database_url" {
  description = "Full PostgreSQL DSN as handed to the workloads. Prefer reading it from Secrets Manager; this output exists for wiring a migration job or a one-off psql through a bastion."
  value       = module.aws_managed.database_url
  sensitive   = true
}

# ---------------------------------------------------------------------------
# Registries and identity
# ---------------------------------------------------------------------------

output "ecr_registry" {
  description = "Registry host and prefix the images are pulled from. Combine with the component name, e.g. <registry>/nexus-api:<tag>."
  value       = module.aws_managed.ecr_registry
}

output "ecr_repository_urls" {
  description = "Map of image name to ECR repository URL, for docker push in CI."
  value       = module.aws_managed.ecr_repository_urls
}

output "ecr_push_policy_arn" {
  description = "IAM policy granting push access to these repositories. Attach it to whatever identity CI runs as - a GitHub Actions OIDC role, a CodeBuild project. This stack deliberately does not create that identity."
  value       = module.aws_managed.ecr_push_policy_arn
}

output "irsa_role_arns" {
  description = "Map of Kubernetes service account name to IAM role ARN. Empty when eks_oidc_provider_arn is unset."
  value       = module.aws_managed.irsa_role_arns
}

# The chart exposes serviceAccount.annotations as one map applied to all four
# service accounts, so per-component role ARNs cannot be wired through it
# without annotating every account with the same role. Until the chart grows a
# per-component annotations value, apply these by hand or with a controller:
#
#   kubectl -n nexus annotate serviceaccount nexus-api \
#     eks.amazonaws.com/role-arn=<the api entry from irsa_role_arns>
output "irsa_service_account_annotations" {
  description = "Ready-made annotation maps keyed by service account name. See the note in outputs.tf: these are not applied automatically, because the chart's serviceAccount.annotations value is shared across all four components."
  value       = module.aws_managed.irsa_service_account_annotations
}

# ---------------------------------------------------------------------------
# Application
# ---------------------------------------------------------------------------

output "api_internal_url" {
  description = "Cluster-internal base URL for nexus-api."
  value       = module.platform.api_internal_url
}

output "web_internal_url" {
  description = "Cluster-internal base URL for the NEXUS console."
  value       = module.platform.web_internal_url
}

output "health_endpoints" {
  description = "Liveness and readiness paths on nexus-api, for an external probe or a load balancer target group."
  value       = module.platform.health_endpoints
}

output "public_url" {
  description = "Externally reachable console URL, or null when no Ingress is created here. DNS for this host is not managed by this stack."
  value       = var.ingress_enabled ? format("%s://%s", var.ingress_tls_secret_name != null ? "https" : "http", var.ingress_host) : null
}

output "nexus_secret_name" {
  description = "Kubernetes Secret holding NEXUS_DATABASE_URL, NEXUS_AUTH_JWT_SECRET and NEXUS_WEBHOOK_SECRET."
  value       = module.platform.secret_name
}

output "webhook_secret" {
  description = "Generated webhook HMAC secret. Read it once with `tofu output -raw webhook_secret` and configure the same value in GitHub or GitLab; every existing webhook fails verification until both sides match."
  value       = module.platform.webhook_secret
  sensitive   = true
}

output "auth_jwt_secret" {
  description = "Generated JWT signing key. Regenerating it logs every console and API session out."
  value       = module.platform.auth_jwt_secret
  sensitive   = true
}

# ---------------------------------------------------------------------------
# Observability
# ---------------------------------------------------------------------------

output "prometheus_url" {
  description = "Cluster-internal Prometheus URL, or null when observability is disabled."
  value       = one(module.observability[*].prometheus_url)
}

output "grafana_url" {
  description = "Cluster-internal Grafana URL, or null when observability is disabled. There is no Ingress for it: reach it with a port-forward or put it behind your existing SSO proxy."
  value       = one(module.observability[*].grafana_url)
}

output "grafana_admin_password" {
  description = "Generated Grafana admin password, or null when observability is disabled."
  value       = one(module.observability[*].grafana_admin_password)
  sensitive   = true
}

# ---------------------------------------------------------------------------
# Operational summary
# ---------------------------------------------------------------------------

output "next_steps" {
  description = "The commands most often needed straight after an apply."
  value = {
    kubeconfig     = "aws eks update-kubeconfig --name ${var.eks_cluster_name} --region ${var.aws_region}"
    pods           = "kubectl -n ${kubernetes_namespace_v1.nexus.metadata[0].name} get pods -l app.kubernetes.io/part-of=nexus"
    rollout        = "kubectl -n ${kubernetes_namespace_v1.nexus.metadata[0].name} rollout status deploy/nexus-api"
    console        = module.platform.port_forward_commands.web
    ecr_login      = "aws ecr get-login-password --region ${var.aws_region} | docker login --username AWS --password-stdin ${module.aws_managed.ecr_registry}"
    webhook_secret = "tofu output -raw webhook_secret"
    db_credential  = "aws secretsmanager get-secret-value --region ${var.aws_region} --secret-id ${coalesce(module.aws_managed.database_secret_arn, "<secrets manager disabled>")}"
  }
}
