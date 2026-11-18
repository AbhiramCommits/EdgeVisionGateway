output "namespace" {
  description = "Namespace containing the gateway."
  value       = kubernetes_namespace_v1.evg.metadata[0].name
}

output "service_name" {
  description = "ClusterIP Service exposing REST (8080) and gRPC (9090)."
  value       = kubernetes_service_v1.gateway.metadata[0].name
}

output "hpa_name" {
  description = "Queue-depth HorizontalPodAutoscaler."
  value       = kubernetes_horizontal_pod_autoscaler_v2.gateway.metadata[0].name
}

output "image" {
  description = "Deployed image reference."
  value       = "${var.image}:${var.image_tag}"
}
