variable "kubeconfig_path" {
  description = "Path to the kubeconfig used by the kubernetes and helm providers."
  type        = string
  default     = "~/.kube/config"
}

variable "namespace" {
  description = "Namespace for the gateway."
  type        = string
  default     = "edgevision"
}

variable "image" {
  description = "Gateway container image repository."
  type        = string
  default     = "edgevisiongateway"
}

variable "image_tag" {
  description = "Gateway image tag."
  type        = string
  default     = "dev"
}

variable "replicas" {
  description = "Initial gateway replica count (the HPA owns it after apply)."
  type        = number
  default     = 2
}

variable "min_replicas" {
  description = "HPA minimum replicas."
  type        = number
  default     = 2
}

variable "max_replicas" {
  description = "HPA maximum replicas."
  type        = number
  default     = 20
}

variable "queue_depth_target" {
  description = "HPA target average queue depth per pod (custom metric evg_queue_depth)."
  type        = string
  default     = "400"
}

variable "cpu_request" {
  type    = string
  default = "500m"
}

variable "memory_request" {
  type    = string
  default = "512Mi"
}

variable "cpu_limit" {
  type    = string
  default = "2"
}

variable "memory_limit" {
  type    = string
  default = "2Gi"
}

variable "postgres_dsn" {
  description = "PostgreSQL DSN (stored in a Kubernetes Secret)."
  type        = string
  sensitive   = true
  default     = "postgres://evg:evg@evg-postgres:5432/evg?sslmode=disable"
}

variable "redis_addr" {
  description = "Redis host:port."
  type        = string
  default     = "evg-redis:6379"
}

variable "device_secret" {
  description = "HMAC secret for device tokens."
  type        = string
  sensitive   = true
  default     = "dev-secret-change-me"
}

variable "deploy_redis_with_helm" {
  description = "Deploy a standalone Redis with the Bitnami Helm chart (otherwise an evg-redis Service must already exist)."
  type        = bool
  default     = false
}
