output "namespace" {
  description = "Namespace the NEXUS stack was deployed into."
  value       = kubernetes_namespace_v1.nexus.metadata[0].name
}

output "workloads_deployed" {
  description = "Whether the Helm release was installed. False means the namespace, datastores, ConfigMap and Secret exist but no NEXUS pods do."
  value       = module.platform.deployed
}

output "api_internal_url" {
  description = "Cluster-internal base URL for nexus-api."
  value       = module.platform.api_internal_url
}

output "web_internal_url" {
  description = "Cluster-internal base URL for the NEXUS console."
  value       = module.platform.web_internal_url
}

output "nats_url" {
  description = "NATS client URL the workloads use."
  value       = module.datastores.nats_url
}

output "postgres_host" {
  description = "In-cluster DNS name of PostgreSQL."
  value       = module.datastores.postgres_host
}

# Marked sensitive because it embeds the generated password. Read it
# deliberately with `tofu output -raw database_url` when you want to point a
# psql or a locally-run Go binary at the cluster database.
output "database_url" {
  description = "Full PostgreSQL DSN, as handed to the workloads."
  value       = module.datastores.database_url
  sensitive   = true
}

output "postgres_password" {
  description = "Generated PostgreSQL password."
  value       = module.datastores.postgres_password
  sensitive   = true
}

output "auth_jwt_secret" {
  description = "Generated JWT signing key, for minting a token against the local API."
  value       = module.platform.auth_jwt_secret
  sensitive   = true
}

output "webhook_secret" {
  description = "Generated webhook HMAC secret, for signing test webhook deliveries."
  value       = module.platform.webhook_secret
  sensitive   = true
}

output "grafana_url" {
  description = "Cluster-internal Grafana URL, or null when observability is disabled."
  value       = one(module.observability[*].grafana_url)
}

output "grafana_admin_password" {
  description = "Generated Grafana admin password, or null when observability is disabled."
  value       = one(module.observability[*].grafana_admin_password)
  sensitive   = true
}

# The single output most people actually want after an apply: how do I look at
# the thing I just deployed.
output "next_steps" {
  description = "Commands for reaching the stack from the host. There is no Ingress by default because a bare kind or Docker Desktop cluster has no ingress controller installed."
  value = {
    console    = module.platform.port_forward_commands.web
    api        = module.platform.port_forward_commands.api
    database   = "kubectl -n ${kubernetes_namespace_v1.nexus.metadata[0].name} port-forward svc/${module.datastores.postgres_service_name} 5432:5432"
    nats       = "kubectl -n ${kubernetes_namespace_v1.nexus.metadata[0].name} port-forward svc/${module.datastores.nats_service_name} 4222:4222"
    prometheus = try(module.observability[0].port_forward_commands.prometheus, "observability disabled: set enable_observability = true")
    grafana    = try(module.observability[0].port_forward_commands.grafana, "observability disabled: set enable_observability = true")
    dsn        = "tofu output -raw database_url"
  }
}
