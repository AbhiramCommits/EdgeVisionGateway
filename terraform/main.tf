locals {
  labels = {
    "app.kubernetes.io/name"       = "edgevisiongateway"
    "app.kubernetes.io/part-of"    = "edgevisiongateway"
    "app.kubernetes.io/managed-by" = "terraform"
  }
  selector = {
    app = "evg-gateway"
  }
  config = {
    EVG_STORE          = "postgres"
    EVG_HTTP_ADDR      = ":8080"
    EVG_GRPC_ADDR      = ":9090"
    EVG_WORKER_COUNT   = "8"
    EVG_QUEUE_CAPACITY = "2048"
    EVG_SHED_THRESHOLD = "0.85"
    EVG_BATCH_MAX      = "500"
    EVG_FLUSH_INTERVAL = "20ms"
    EVG_CACHE_TTL      = "5m"
    EVG_CACHE_WINDOW   = "2m"
    EVG_HEARTBEAT_TTL  = "30s"
    EVG_DRAIN_DEADLINE = "25s"
    EVG_LOG_LEVEL      = "info"
    EVG_AUTO_REGISTER  = "false"
  }
}

resource "kubernetes_namespace_v1" "evg" {
  metadata {
    name   = var.namespace
    labels = local.labels
  }
}

resource "kubernetes_config_map_v1" "evg" {
  metadata {
    name      = "evg-config"
    namespace = kubernetes_namespace_v1.evg.metadata[0].name
    labels    = local.labels
  }
  data = local.config
}

resource "kubernetes_secret_v1" "evg" {
  metadata {
    name      = "evg-secrets"
    namespace = kubernetes_namespace_v1.evg.metadata[0].name
    labels    = local.labels
  }
  type = "Opaque"
  data = {
    postgres-dsn  = var.postgres_dsn
    redis-addr    = var.redis_addr
    device-secret = var.device_secret
  }
}

resource "kubernetes_deployment_v1" "gateway" {
  metadata {
    name      = "evg-gateway"
    namespace = kubernetes_namespace_v1.evg.metadata[0].name
    labels    = merge(local.labels, local.selector)
  }
  spec {
    replicas = var.replicas
    selector {
      match_labels = local.selector
    }
    template {
      metadata {
        labels = local.selector
        annotations = {
          "prometheus.io/scrape" = "true"
          "prometheus.io/port"   = "8080"
          "prometheus.io/path"   = "/metrics/prometheus"
        }
      }
      spec {
        termination_grace_period_seconds = 40
        security_context {
          run_as_non_root = true
          seccomp_profile {
            type = "RuntimeDefault"
          }
        }
        container {
          name  = "gateway"
          image = "${var.image}:${var.image_tag}"
          port {
            name           = "http"
            container_port = 8080
          }
          port {
            name           = "grpc"
            container_port = 9090
          }
          env_from {
            config_map_ref {
              name = kubernetes_config_map_v1.evg.metadata[0].name
            }
          }
          env {
            name = "EVG_POSTGRES_DSN"
            value_from {
              secret_key_ref {
                name = kubernetes_secret_v1.evg.metadata[0].name
                key  = "postgres-dsn"
              }
            }
          }
          env {
            name = "EVG_REDIS_ADDR"
            value_from {
              secret_key_ref {
                name = kubernetes_secret_v1.evg.metadata[0].name
                key  = "redis-addr"
              }
            }
          }
          env {
            name = "EVG_DEVICE_SECRET"
            value_from {
              secret_key_ref {
                name = kubernetes_secret_v1.evg.metadata[0].name
                key  = "device-secret"
              }
            }
          }
          resources {
            requests = {
              cpu    = var.cpu_request
              memory = var.memory_request
            }
            limits = {
              cpu    = var.cpu_limit
              memory = var.memory_limit
            }
          }
          liveness_probe {
            http_get {
              path = "/healthz"
              port = "http"
            }
            initial_delay_seconds = 5
            period_seconds        = 10
          }
          readiness_probe {
            http_get {
              path = "/readyz"
              port = "http"
            }
            initial_delay_seconds = 3
            period_seconds        = 5
          }
          security_context {
            allow_privilege_escalation = false
            read_only_root_filesystem  = true
            capabilities {
              drop = ["ALL"]
            }
          }
        }
      }
    }
  }
}

resource "kubernetes_service_v1" "gateway" {
  metadata {
    name      = "evg-gateway"
    namespace = kubernetes_namespace_v1.evg.metadata[0].name
    labels    = merge(local.labels, local.selector)
  }
  spec {
    selector = local.selector
    port {
      name        = "http"
      port        = 8080
      target_port = "http"
    }
    port {
      name        = "grpc"
      port        = 9090
      target_port = "grpc"
    }
  }
}

resource "kubernetes_horizontal_pod_autoscaler_v2" "gateway" {
  metadata {
    name      = "evg-gateway"
    namespace = kubernetes_namespace_v1.evg.metadata[0].name
    labels    = local.labels
  }
  spec {
    min_replicas = var.min_replicas
    max_replicas = var.max_replicas
    scale_target_ref {
      api_version = "apps/v1"
      kind        = "Deployment"
      name        = kubernetes_deployment_v1.gateway.metadata[0].name
    }
    metric {
      type = "Pods"
      pods {
        metric {
          name = "evg_queue_depth"
        }
        target {
          type          = "AverageValue"
          average_value = var.queue_depth_target
        }
      }
    }
    behavior {
      scale_up {
        stabilization_window_seconds = 30
        policy {
          type           = "Pods"
          value          = 4
          period_seconds = 60
        }
      }
      scale_down {
        stabilization_window_seconds = 300
        policy {
          type           = "Pods"
          value          = 1
          period_seconds = 60
        }
      }
    }
  }
}

resource "helm_release" "redis" {
  count      = var.deploy_redis_with_helm ? 1 : 0
  name       = "evg-redis"
  namespace  = kubernetes_namespace_v1.evg.metadata[0].name
  repository = "https://charts.bitnami.com/bitnami"
  chart      = "redis"
  version    = "20.6.1"
  values = [yamlencode({
    architecture = "standalone"
    auth         = { enabled = false }
    master       = { persistence = { enabled = false } }
  })]
}
