output "namespace" {
  description = "Namespace the NEXUS workloads run in."
  value       = local.namespace_name
}

output "release_name" {
  description = "Helm release name, or the name that would be used if deploy_workloads were true."
  value       = var.release_name
}

output "deployed" {
  description = "Whether the Helm release was actually installed. False means the namespace, ConfigMap and Secret exist but no pods were created."
  value       = var.deploy_workloads
}

output "config_map_name" {
  description = "ConfigMap holding the non-secret NEXUS_* environment. Rendered by the chart, not by this module, so it exists only when deploy_workloads is true."
  value       = local.config_map_name
}

output "secret_name" {
  description = "Secret holding NEXUS_DATABASE_URL, NEXUS_AUTH_JWT_SECRET and NEXUS_WEBHOOK_SECRET."
  value       = kubernetes_secret_v1.secrets.metadata[0].name
}

output "api_service_name" {
  description = "Service name for nexus-api, as templated by the chart from the release name."
  value       = "${local.chart_fullname}-api"
}

output "api_internal_url" {
  description = "Cluster-internal base URL for the NEXUS API."
  value       = "http://${local.chart_fullname}-api.${local.namespace_name}.svc.cluster.local:${var.http_port}"
}

output "web_internal_url" {
  description = "Cluster-internal base URL for the NEXUS console."
  value       = "http://${local.chart_fullname}-web.${local.namespace_name}.svc.cluster.local:${var.web_port}"
}

output "health_endpoints" {
  description = "Liveness and readiness paths on nexus-api, for anyone wiring an external probe or load balancer."
  value = {
    liveness  = "http://${local.chart_fullname}-api.${local.namespace_name}.svc.cluster.local:${var.http_port}/healthz"
    readiness = "http://${local.chart_fullname}-api.${local.namespace_name}.svc.cluster.local:${var.http_port}/readyz"
  }
}

output "metrics_port" {
  description = "Port every NEXUS service exposes /metrics on. Prometheus discovers these through the prometheus.io/* pod annotations rather than a static target list."
  value       = var.metrics_port
}

output "port_forward_commands" {
  description = "Ready-to-paste kubectl commands for reaching the console and API from the host. This is the local access path; there is no Ingress by default because kind and Docker Desktop have no controller installed."
  value = {
    web = "kubectl -n ${local.namespace_name} port-forward svc/${local.chart_fullname}-web ${var.web_port}:${var.web_port}"
    api = "kubectl -n ${local.namespace_name} port-forward svc/${local.chart_fullname}-api ${var.http_port}:${var.http_port}"
  }
}

output "auth_jwt_secret" {
  description = "The JWT signing key, generated or supplied. Exposed so a developer can mint a local token; treat any consumer of this output as holding a credential."
  value       = local.auth_jwt_secret
  sensitive   = true
}

output "webhook_secret" {
  description = "The webhook HMAC secret, generated or supplied. Configure the same value in the SCM provider."
  value       = local.webhook_secret
  sensitive   = true
}
