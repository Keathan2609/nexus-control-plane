# ---------------------------------------------------------------------------
# Prometheus and Grafana for the NEXUS control plane.
#
# Unlike the datastores module, this one does use upstream Helm charts. The
# trade-off runs the other way here: nobody wants to hand-maintain the
# kubernetes_sd scrape configuration, the RBAC that goes with it, and
# kube-state-metrics, and none of it holds persistent state we would cry over.
#
# The plain prometheus-community/prometheus chart is used rather than
# kube-prometheus-stack because the latter installs the Prometheus Operator and
# its CRDs. CRDs are cluster-scoped, outlive the release, and are a genuine
# nuisance to remove from a cluster you share with other work - too much
# footprint for a local dev loop. The cost of that choice is that we discover
# targets through prometheus.io/* pod annotations instead of ServiceMonitors,
# which is exactly what the NEXUS pod templates already carry.
# ---------------------------------------------------------------------------

locals {
  common_labels = merge(
    {
      "app.kubernetes.io/part-of"    = "nexus"
      "app.kubernetes.io/managed-by" = "opentofu"
      "nexus.dev/environment"        = var.environment
    },
    var.labels,
  )

  namespace_name = var.create_namespace ? kubernetes_namespace_v1.this[0].metadata[0].name : var.namespace

  # The prometheus chart names its server Service "<release>-server" and
  # fronts it on port 80.
  prometheus_service = "${var.prometheus_release_name}-server"
  prometheus_url     = "http://${local.prometheus_service}.${local.namespace_name}.svc.cluster.local"

  # one() yields null for an empty list, so this stays evaluable when Grafana
  # is disabled and no password resource exists.
  grafana_admin_password = var.grafana_admin_password != null ? var.grafana_admin_password : one(random_password.grafana_admin[*].result)

  grafana_secret_name = "${var.grafana_release_name}-admin"

  prometheus_values = {
    server = {
      retention = var.prometheus_retention

      persistentVolume = {
        enabled      = var.prometheus_persistence_enabled
        size         = var.prometheus_storage_size
        storageClass = var.storage_class_name
      }

      resources = var.prometheus_resources

      # 30s rather than the 15s default. On a laptop cluster the extra
      # resolution buys nothing and the TSDB writes double.
      global = {
        scrape_interval     = "30s"
        evaluation_interval = "30s"
      }
    }

    alertmanager = {
      enabled = var.enable_alertmanager
    }

    "kube-state-metrics" = {
      enabled = var.enable_kube_state_metrics
    }

    "prometheus-node-exporter" = {
      enabled = var.enable_node_exporter
    }

    # A pushgateway is for batch jobs that die before they can be scraped.
    # NEXUS workers are long-lived processes that expose /metrics themselves,
    # so it would only ever hold stale series.
    "prometheus-pushgateway" = {
      enabled = false
    }
  }

  grafana_values = {
    admin = {
      existingSecret = local.grafana_secret_name
      userKey        = "admin-user"
      passwordKey    = "admin-password"
    }

    persistence = {
      enabled          = var.grafana_persistence_enabled
      size             = var.grafana_storage_size
      storageClassName = var.storage_class_name
    }

    resources = var.grafana_resources

    # Provisioned as config, not clicked in the UI: a datasource that only
    # exists because someone set it up by hand is a datasource that disappears
    # the next time the pod is rescheduled without a PVC.
    datasources = {
      "datasources.yaml" = {
        apiVersion = 1
        datasources = [
          {
            name      = "Prometheus"
            type      = "prometheus"
            url       = local.prometheus_url
            access    = "proxy"
            isDefault = true
          },
        ]
      }
    }

    # The sidecar watches for ConfigMaps carrying the label below and drops
    # their contents into Grafana's dashboard directory. It means a dashboard
    # can be added by anyone who can create a ConfigMap, without touching this
    # module or restarting Grafana.
    sidecar = {
      dashboards = {
        enabled         = true
        label           = "grafana_dashboard"
        searchNamespace = local.namespace_name
      }
    }

    "grafana.ini" = {
      analytics = {
        reporting_enabled = false
        check_for_updates = false
      }
      users = {
        # No public sign-up on a stack that is port-forwarded to a laptop.
        allow_sign_up = false
      }
    }
  }

  # Deliberately built from metrics the Go Prometheus client exposes for free,
  # so the dashboard is useful on day one and does not silently break if a
  # NEXUS-specific metric is renamed. Add domain panels (pipeline duration,
  # queue depth) here once those metric names are stable.
  nexus_dashboard = {
    title         = "NEXUS control plane"
    uid           = "nexus-control-plane"
    timezone      = "browser"
    schemaVersion = 39
    refresh       = "30s"
    time = {
      from = "now-1h"
      to   = "now"
    }
    panels = [
      {
        type    = "stat"
        title   = "Services up"
        id      = 1
        gridPos = { h = 6, w = 8, x = 0, y = 0 }
        targets = [
          {
            expr         = "sum by (app_kubernetes_io_component) (up{namespace=~\"${join("|", var.monitored_namespaces)}\"})"
            legendFormat = "{{app_kubernetes_io_component}}"
          },
        ]
      },
      {
        type    = "timeseries"
        title   = "Goroutines"
        id      = 2
        gridPos = { h = 6, w = 8, x = 8, y = 0 }
        targets = [
          {
            expr         = "go_goroutines{namespace=~\"${join("|", var.monitored_namespaces)}\"}"
            legendFormat = "{{pod}}"
          },
        ]
      },
      {
        type    = "timeseries"
        title   = "Resident memory"
        id      = 3
        gridPos = { h = 6, w = 8, x = 16, y = 0 }
        targets = [
          {
            expr         = "process_resident_memory_bytes{namespace=~\"${join("|", var.monitored_namespaces)}\"}"
            legendFormat = "{{pod}}"
          },
        ]
      },
    ]
  }
}

resource "kubernetes_namespace_v1" "this" {
  count = var.create_namespace ? 1 : 0

  metadata {
    name   = var.namespace
    labels = local.common_labels
  }
}

# ---------------------------------------------------------------------------
# Prometheus
# ---------------------------------------------------------------------------

resource "helm_release" "prometheus" {
  name       = var.prometheus_release_name
  repository = "https://prometheus-community.github.io/helm-charts"
  chart      = "prometheus"
  version    = var.prometheus_chart_version
  namespace  = local.namespace_name

  create_namespace = false
  atomic           = true
  cleanup_on_fail  = true
  wait             = true
  timeout          = var.helm_timeout_seconds

  values = [yamlencode(local.prometheus_values)]

  depends_on = [kubernetes_namespace_v1.this]
}

# ---------------------------------------------------------------------------
# Grafana
# ---------------------------------------------------------------------------

resource "random_password" "grafana_admin" {
  count = var.enable_grafana && var.grafana_admin_password == null ? 1 : 0

  length  = 24
  special = false
}

resource "kubernetes_secret_v1" "grafana_admin" {
  count = var.enable_grafana ? 1 : 0

  metadata {
    name      = local.grafana_secret_name
    namespace = local.namespace_name
    labels    = local.common_labels
  }

  type = "Opaque"

  data = {
    "admin-user"     = var.grafana_admin_user
    "admin-password" = local.grafana_admin_password
  }
}

resource "kubernetes_config_map_v1" "nexus_dashboard" {
  count = var.enable_grafana ? 1 : 0

  metadata {
    name      = "${var.grafana_release_name}-nexus-dashboard"
    namespace = local.namespace_name

    # This label is the entire contract with the Grafana sidecar.
    labels = merge(local.common_labels, {
      grafana_dashboard = "1"
    })
  }

  data = {
    "nexus-control-plane.json" = jsonencode(local.nexus_dashboard)
  }
}

resource "helm_release" "grafana" {
  count = var.enable_grafana ? 1 : 0

  name       = var.grafana_release_name
  repository = "https://grafana.github.io/helm-charts"
  chart      = "grafana"
  version    = var.grafana_chart_version
  namespace  = local.namespace_name

  create_namespace = false
  atomic           = true
  cleanup_on_fail  = true
  wait             = true
  timeout          = var.helm_timeout_seconds

  values = [yamlencode(local.grafana_values)]

  # Grafana starts fine without Prometheus and shows a broken datasource, which
  # is a confusing first impression. Ordering them costs nothing.
  depends_on = [
    helm_release.prometheus,
    kubernetes_secret_v1.grafana_admin,
    kubernetes_config_map_v1.nexus_dashboard,
  ]
}
