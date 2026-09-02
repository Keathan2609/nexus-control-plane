output "namespace" {
  description = "Namespace the datastores were created in."
  value       = var.namespace
}

output "postgres_host" {
  description = "In-cluster DNS name of the PostgreSQL service."
  value       = local.postgres_fqdn
}

output "postgres_port" {
  description = "PostgreSQL TCP port."
  value       = 5432
}

output "postgres_database" {
  description = "Database name NEXUS connects to."
  value       = var.postgres_database
}

output "postgres_username" {
  description = "Role NEXUS connects as."
  value       = var.postgres_username
}

output "postgres_password" {
  description = "Generated or supplied password for postgres_username."
  value       = local.postgres_password
  sensitive   = true
}

# The whole point of the module: hand the caller something it can drop straight
# into NEXUS_DATABASE_URL without reassembling the parts and getting the
# sslmode or the port wrong.
output "database_url" {
  description = "Complete PostgreSQL DSN for NEXUS_DATABASE_URL."
  value       = local.database_url
  sensitive   = true
}

output "postgres_secret_name" {
  description = "Kubernetes Secret holding the PostgreSQL credentials and DSN. Useful for envFrom in a workload this module does not own."
  value       = kubernetes_secret_v1.postgres.metadata[0].name
}

output "nats_url" {
  description = "Client URL for NEXUS_NATS_URL."
  value       = local.nats_url
}

output "nats_host" {
  description = "In-cluster DNS name of the NATS client service."
  value       = local.nats_fqdn
}

output "nats_monitoring_url" {
  description = "NATS monitoring endpoint (/healthz, /varz, /jsz). Not scraped by Prometheus without the separate exporter sidecar."
  value       = "http://${local.nats_fqdn}:8222"
}

output "nats_replicas" {
  description = "Number of NATS servers. 1 means JetStream streams cannot be replicated, whatever a stream config asks for."
  value       = var.nats_replicas
}

output "jetstream_replication_available" {
  description = "Whether the NATS deployment can host replicated JetStream streams. False on the default single-node local setup."
  value       = var.nats_replicas > 1
}

output "postgres_service_name" {
  description = "Kubernetes Service in front of PostgreSQL, for port-forwarding from a host."
  value       = kubernetes_service_v1.postgres.metadata[0].name
}

output "nats_service_name" {
  description = "Kubernetes Service in front of NATS, for port-forwarding from a host."
  value       = kubernetes_service_v1.nats.metadata[0].name
}
