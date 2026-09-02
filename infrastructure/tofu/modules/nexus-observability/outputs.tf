output "namespace" {
  description = "Namespace Prometheus and Grafana run in."
  value       = local.namespace_name
}

output "prometheus_url" {
  description = "Cluster-internal Prometheus base URL. This is what Grafana queries and what any additional datasource should point at."
  value       = local.prometheus_url
}

output "prometheus_service_name" {
  description = "Kubernetes Service in front of the Prometheus server."
  value       = local.prometheus_service
}

output "grafana_enabled" {
  description = "Whether Grafana was installed."
  value       = var.enable_grafana
}

output "grafana_url" {
  description = "Cluster-internal Grafana base URL, or null when Grafana is disabled. There is no Ingress: reach it with the port-forward command below."
  value       = var.enable_grafana ? "http://${var.grafana_release_name}.${local.namespace_name}.svc.cluster.local" : null
}

output "grafana_admin_user" {
  description = "Grafana admin username."
  value       = var.enable_grafana ? var.grafana_admin_user : null
}

output "grafana_admin_password" {
  description = "Grafana admin password, generated unless one was supplied. Read it with: tofu output -raw grafana_admin_password"
  value       = local.grafana_admin_password
  sensitive   = true
}

output "grafana_admin_secret_name" {
  description = "Secret the Grafana admin credentials live in, for anyone who would rather read them with kubectl than with tofu output."
  value       = var.enable_grafana ? local.grafana_secret_name : null
}

output "port_forward_commands" {
  description = "Ready-to-paste kubectl commands for reaching the observability stack from the host."
  value = {
    prometheus = "kubectl -n ${local.namespace_name} port-forward svc/${local.prometheus_service} 9090:80"
    grafana    = var.enable_grafana ? "kubectl -n ${local.namespace_name} port-forward svc/${var.grafana_release_name} 3001:80" : null
  }
}

output "scrape_contract" {
  description = "What a workload must do to be scraped. Prometheus discovers pods by annotation rather than by ServiceMonitor, so a pod missing these annotations is invisible no matter how healthy it is."
  value = {
    "prometheus.io/scrape" = "true"
    "prometheus.io/port"   = "9090"
    "prometheus.io/path"   = "/metrics"
  }
}
