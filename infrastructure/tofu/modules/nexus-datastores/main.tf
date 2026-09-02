# ---------------------------------------------------------------------------
# PostgreSQL 16 and NATS 2.10 (JetStream) for a LOCAL cluster.
#
# These are deliberately plain Kubernetes objects rather than upstream Helm
# charts. Two reasons, both about the local path staying reliable:
#
#   1. A chart repository is a network dependency that changes underneath you.
#      Several popular database charts have changed image registries, tags and
#      licensing terms with no notice; a laptop cluster that used to come up
#      then stops coming up, and the failure looks like our bug.
#   2. Fifty lines of StatefulSet is legible. A chart's values.yaml is an
#      abstraction over a thing you still have to understand when it breaks.
#
# The trade-off is that this is a single-node, no-backup, no-failover database.
# That is the correct shape for a development cluster and the wrong shape for
# production, which is exactly why the AWS path uses managed RDS instead.
# ---------------------------------------------------------------------------

locals {
  postgres_name      = "${var.name_prefix}-postgres"
  nats_name          = "${var.name_prefix}-nats"
  nats_headless_name = "${var.name_prefix}-nats-headless"

  common_labels = merge(
    {
      "app.kubernetes.io/part-of"    = "nexus"
      "app.kubernetes.io/managed-by" = "opentofu"
    },
    var.labels,
  )

  postgres_labels = merge(local.common_labels, {
    "app.kubernetes.io/name"      = "postgresql"
    "app.kubernetes.io/instance"  = local.postgres_name
    "app.kubernetes.io/component" = "database"
  })

  postgres_selector = {
    "app.kubernetes.io/name"      = "postgresql"
    "app.kubernetes.io/instance"  = local.postgres_name
    "app.kubernetes.io/component" = "database"
  }

  nats_labels = merge(local.common_labels, {
    "app.kubernetes.io/name"      = "nats"
    "app.kubernetes.io/instance"  = local.nats_name
    "app.kubernetes.io/component" = "messaging"
  })

  nats_selector = {
    "app.kubernetes.io/name"      = "nats"
    "app.kubernetes.io/instance"  = local.nats_name
    "app.kubernetes.io/component" = "messaging"
  }

  # Fully-qualified names. Short names resolve fine from inside the same
  # namespace, but the DSN and NATS URL end up in a ConfigMap that a human may
  # copy into a port-forward or another namespace, so spell them out.
  postgres_fqdn = "${local.postgres_name}.${var.namespace}.svc.cluster.local"
  nats_fqdn     = "${local.nats_name}.${var.namespace}.svc.cluster.local"

  postgres_password = var.postgres_password != null ? var.postgres_password : random_password.postgres.result

  database_url = format(
    "postgres://%s:%s@%s:5432/%s?sslmode=%s",
    var.postgres_username,
    local.postgres_password,
    local.postgres_fqdn,
    var.postgres_database,
    var.sslmode,
  )

  nats_url = "nats://${local.nats_fqdn}:4222"

  # Raft peers for a clustered JetStream. Each server needs the full route list;
  # they de-duplicate their own address at startup.
  nats_routes = [
    for i in range(var.nats_replicas) :
    "nats://${local.nats_name}-${i}.${local.nats_headless_name}.${var.namespace}.svc.cluster.local:6222"
  ]

  # The cluster stanza, rendered whether or not it ends up being used. It
  # lives in its own local because HCL cannot parse a heredoc as a branch of
  # a conditional: the closing marker has to be the last thing on its line,
  # so there is nowhere left to put the ": otherwise" half of the expression.
  nats_cluster_stanza = <<-EOT
    cluster {
      name: ${var.name_prefix}
      port: 6222
      routes: [
        ${join(",\n        ", local.nats_routes)}
      ]
    }
  EOT

  # Only emit it for a real quorum. A single server with a cluster block still
  # works, but it advertises a route nobody dials and clutters the logs.
  nats_cluster_block = var.nats_replicas > 1 ? local.nats_cluster_stanza : ""

  # $POD_NAME is expanded by nats-server from the environment, not by OpenTofu.
  # Stable per-pod server names are what let JetStream recognise a restarted
  # peer as the same Raft member instead of a new one.
  #
  # max_file_store is intentionally unset: JetStream then sizes itself from the
  # free space on store_dir, and store_dir is the PVC, so the PVC size is the
  # single place storage is capped.
  nats_conf = <<-EOT
    server_name: $POD_NAME
    port: 4222
    http: 8222

    jetstream {
      store_dir: "/data/jetstream"
    }

    ${local.nats_cluster_block}
  EOT
}

# ---------------------------------------------------------------------------
# Credentials
# ---------------------------------------------------------------------------

# special = false is a deliberate choice, not laziness. This password is
# embedded in a URL-shaped DSN; punctuation there has to be percent-encoded and
# every layer that re-parses the DSN is a chance to get that wrong. 32 chars of
# base62 is ~190 bits of entropy, which is far more than the threat model here
# (an in-cluster database with a NetworkPolicy in front of it) requires.
resource "random_password" "postgres" {
  length  = 32
  special = false
}

resource "kubernetes_secret_v1" "postgres" {
  metadata {
    name      = local.postgres_name
    namespace = var.namespace
    labels    = local.postgres_labels
  }

  type = "Opaque"

  # The container reads POSTGRES_*; NEXUS reads the assembled DSN. Both live
  # here so a human debugging with `kubectl exec` has everything in one object.
  data = {
    POSTGRES_USER      = var.postgres_username
    POSTGRES_PASSWORD  = local.postgres_password
    POSTGRES_DB        = var.postgres_database
    NEXUS_DATABASE_URL = local.database_url
    POSTGRES_HOST      = local.postgres_fqdn
    POSTGRES_PORT      = "5432"
  }
}

# ---------------------------------------------------------------------------
# PostgreSQL
# ---------------------------------------------------------------------------

# Headless: there is exactly one server and clients want its address, not a
# load-balanced VIP. It also gives the pod a stable DNS name of its own.
resource "kubernetes_service_v1" "postgres" {
  metadata {
    name      = local.postgres_name
    namespace = var.namespace
    labels    = local.postgres_labels
  }

  spec {
    cluster_ip = "None"
    selector   = local.postgres_selector

    port {
      name        = "postgres"
      port        = 5432
      target_port = "postgres"
      protocol    = "TCP"
    }
  }
}

resource "kubernetes_stateful_set_v1" "postgres" {
  metadata {
    name      = local.postgres_name
    namespace = var.namespace
    labels    = local.postgres_labels
  }

  spec {
    service_name = kubernetes_service_v1.postgres.metadata[0].name

    # Not a variable. Two PostgreSQL pods sharing nothing is not a cluster, it
    # is two databases and a split brain. Replication belongs to a managed
    # service or an operator, neither of which is in scope for a laptop.
    replicas = 1

    selector {
      match_labels = local.postgres_selector
    }

    template {
      metadata {
        labels = local.postgres_labels
      }

      spec {
        # Nothing in this pod talks to the Kubernetes API, so it has no reason
        # to carry a mountable API token.
        automount_service_account_token = false

        security_context {
          run_as_non_root = true
          run_as_user     = var.postgres_run_as_user
          run_as_group    = var.postgres_run_as_user
          # fs_group makes the kubelet chgrp the PVC on mount. Without it the
          # volume comes up root-owned and initdb fails with a permissions
          # error that reads like a bug in the image.
          fs_group = var.postgres_run_as_user
        }

        container {
          name  = "postgres"
          image = var.postgres_image

          # Replaces the image CMD (`postgres`), so the entrypoint script still
          # runs. Tuning via flags avoids shipping a whole postgresql.conf.
          args = [
            "postgres",
            "-c", "max_connections=${var.postgres_max_connections}",
            "-c", "shared_buffers=128MB",
            # Log anything slower than a second. On a dev cluster the useful
            # default is "tell me when I wrote a bad query", not "log nothing".
            "-c", "log_min_duration_statement=1000",
          ]

          port {
            name           = "postgres"
            container_port = 5432
            protocol       = "TCP"
          }

          env {
            name = "POSTGRES_USER"
            value_from {
              secret_key_ref {
                name = kubernetes_secret_v1.postgres.metadata[0].name
                key  = "POSTGRES_USER"
              }
            }
          }

          env {
            name = "POSTGRES_PASSWORD"
            value_from {
              secret_key_ref {
                name = kubernetes_secret_v1.postgres.metadata[0].name
                key  = "POSTGRES_PASSWORD"
              }
            }
          }

          env {
            name = "POSTGRES_DB"
            value_from {
              secret_key_ref {
                name = kubernetes_secret_v1.postgres.metadata[0].name
                key  = "POSTGRES_DB"
              }
            }
          }

          # initdb refuses to run in a directory that is not empty, and some
          # provisioners leave lost+found at the volume root. Putting the data
          # directory one level down sidesteps that entirely.
          env {
            name  = "PGDATA"
            value = "/var/lib/postgresql/data/pgdata"
          }

          volume_mount {
            name       = "data"
            mount_path = "/var/lib/postgresql/data"
          }

          resources {
            requests = {
              cpu    = var.postgres_resources.requests.cpu
              memory = var.postgres_resources.requests.memory
            }
            limits = {
              cpu    = var.postgres_resources.limits.cpu
              memory = var.postgres_resources.limits.memory
            }
          }

          # pg_isready over the unix socket, not TCP: it answers "can this
          # server accept a query", which is the question Endpoints should be
          # gated on. A TCP check goes green while the server is still
          # replaying WAL.
          readiness_probe {
            exec {
              command = ["pg_isready", "-U", var.postgres_username, "-d", var.postgres_database]
            }
            initial_delay_seconds = 5
            period_seconds        = 10
            timeout_seconds       = 3
            failure_threshold     = 6
          }

          liveness_probe {
            exec {
              command = ["pg_isready", "-U", var.postgres_username, "-d", var.postgres_database]
            }
            # Generous, because restarting a database that is merely busy is
            # worse than waiting. Recovery after an unclean shutdown can take
            # a while and must not be interrupted.
            initial_delay_seconds = 30
            period_seconds        = 20
            timeout_seconds       = 5
            failure_threshold     = 6
          }
        }

        # Time for a clean shutdown checkpoint. A SIGKILL mid-checkpoint means
        # crash recovery on the next boot, which is survivable but slow.
        termination_grace_period_seconds = 60
      }
    }

    volume_claim_template {
      metadata {
        name   = "data"
        labels = local.postgres_labels
      }

      spec {
        access_modes       = ["ReadWriteOnce"]
        storage_class_name = var.storage_class_name

        resources {
          requests = {
            storage = var.postgres_storage_size
          }
        }
      }
    }
  }
}

# ---------------------------------------------------------------------------
# NATS with JetStream
# ---------------------------------------------------------------------------

resource "kubernetes_config_map_v1" "nats" {
  metadata {
    name      = local.nats_name
    namespace = var.namespace
    labels    = local.nats_labels
  }

  data = {
    "nats.conf" = local.nats_conf
  }
}

# Peer discovery. Routes must address individual pods, which only a headless
# service provides.
resource "kubernetes_service_v1" "nats_headless" {
  metadata {
    name      = local.nats_headless_name
    namespace = var.namespace
    labels    = local.nats_labels
  }

  spec {
    cluster_ip = "None"
    selector   = local.nats_selector

    # Peers have to find each other before any of them is Ready, otherwise the
    # cluster deadlocks waiting for a quorum that DNS refuses to describe.
    publish_not_ready_addresses = true

    port {
      name = "client"
      port = 4222
    }

    port {
      name = "cluster"
      port = 6222
    }

    port {
      name = "monitor"
      port = 8222
    }
  }
}

# What clients connect to.
resource "kubernetes_service_v1" "nats" {
  metadata {
    name      = local.nats_name
    namespace = var.namespace
    labels    = local.nats_labels
  }

  spec {
    type     = "ClusterIP"
    selector = local.nats_selector

    port {
      name        = "client"
      port        = 4222
      target_port = "client"
    }

    port {
      name        = "monitor"
      port        = 8222
      target_port = "monitor"
    }
  }
}

resource "kubernetes_stateful_set_v1" "nats" {
  metadata {
    name      = local.nats_name
    namespace = var.namespace
    labels    = local.nats_labels
  }

  spec {
    service_name = kubernetes_service_v1.nats_headless.metadata[0].name
    replicas     = var.nats_replicas

    selector {
      match_labels = local.nats_selector
    }

    # Roll one server at a time so the Raft group never loses quorum during an
    # upgrade.
    update_strategy {
      type = "RollingUpdate"
      rolling_update {
        partition = 0
      }
    }

    template {
      metadata {
        labels = local.nats_labels

        annotations = {
          # A ConfigMap change does not restart pods by itself. Hashing the
          # rendered config into the pod template makes a config edit produce a
          # rollout, which is what anyone editing it expects.
          "nexus.dev/config-checksum" = sha256(local.nats_conf)

          "prometheus.io/scrape" = "false"
        }
      }

      spec {
        automount_service_account_token = false

        security_context {
          run_as_non_root = true
          run_as_user     = 1000
          run_as_group    = 1000
          fs_group        = 1000
        }

        container {
          name  = "nats"
          image = var.nats_image
          args  = ["-c", "/etc/nats/nats.conf"]

          port {
            name           = "client"
            container_port = 4222
          }

          port {
            name           = "cluster"
            container_port = 6222
          }

          port {
            name           = "monitor"
            container_port = 8222
          }

          env {
            name = "POD_NAME"
            value_from {
              field_ref {
                field_path = "metadata.name"
              }
            }
          }

          volume_mount {
            name       = "config"
            mount_path = "/etc/nats"
            read_only  = true
          }

          volume_mount {
            name       = "data"
            mount_path = "/data"
          }

          resources {
            requests = {
              cpu    = var.nats_resources.requests.cpu
              memory = var.nats_resources.requests.memory
            }
            limits = {
              cpu    = var.nats_resources.limits.cpu
              memory = var.nats_resources.limits.memory
            }
          }

          # /healthz with no arguments reports "serving and JetStream is
          # current". The bare monitoring root would go green before JetStream
          # finished recovering its streams.
          readiness_probe {
            http_get {
              path = "/healthz"
              port = "monitor"
            }
            initial_delay_seconds = 5
            period_seconds        = 10
            failure_threshold     = 6
          }

          liveness_probe {
            http_get {
              path = "/healthz"
              port = "monitor"
            }
            initial_delay_seconds = 20
            period_seconds        = 20
            failure_threshold     = 6
          }

          # Lets nats-server drain its client connections and flush JetStream
          # state instead of being killed mid-write.
          lifecycle {
            pre_stop {
              exec {
                command = ["nats-server", "--signal", "ldm=1"]
              }
            }
          }
        }

        volume {
          name = "config"
          config_map {
            name = kubernetes_config_map_v1.nats.metadata[0].name
          }
        }

        termination_grace_period_seconds = 60
      }
    }

    volume_claim_template {
      metadata {
        name   = "data"
        labels = local.nats_labels
      }

      spec {
        access_modes       = ["ReadWriteOnce"]
        storage_class_name = var.storage_class_name

        resources {
          requests = {
            storage = var.nats_storage_size
          }
        }
      }
    }
  }
}
