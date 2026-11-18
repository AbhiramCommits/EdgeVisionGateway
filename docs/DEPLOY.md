# Deployment

Three paths are supported: local Docker Compose (default for development and
benchmarks), Kubernetes via Kustomize, and Kubernetes via Terraform. Terraform
and the manifests were validated in this build; neither was applied to a
production cluster.

## 1. Local Docker Compose

```sh
docker compose up -d          # Postgres 16 + Redis 7, both healthchecked
make build
EVG_AUTO_REGISTER=true ./bin/gateway
```

Defaults: REST `:8080`, gRPC `:9090`, Postgres
`postgres://evg:evg@localhost:5432/evg`, Redis `localhost:6379`, 8 workers,
queue capacity 4096. The gateway applies `internal/storage/schema.sql` and
pre-creates today ± 1 day partitions on startup.

`EVG_AUTO_REGISTER=true` is for the simulated fleet only. In production,
pre-register devices (the `devices` table plus the Redis hash) and leave it off.
The compose stack is development-grade: no TLS, no volume encryption, known
passwords.

## 2. Kubernetes (kind / minikube)

```sh
# build and load the image into the local cluster
docker build -t edgevisiongateway:dev .
kind create cluster --name evg
kind load docker-image edgevisiongateway:dev --name evg

# edit deploy/k8s/secret.example.yaml first (or create evg-secrets yourself)
kubectl apply -k deploy/k8s
kubectl -n edgevision rollout status deploy/evg-gateway

kubectl -n edgevision port-forward svc/evg-gateway 8080:8080 9090:9090
curl -s http://localhost:8080/healthz
```

What the kustomization creates: namespace, config map, example secret,
Postgres StatefulSet (5 GiB PVC) + headless Service, Redis Deployment +
Service, gateway Deployment (2 replicas), ClusterIP Service (REST + gRPC), and
the queue-depth HPA. `hpa-cpu.yaml` is an alternative that is deliberately not
listed in `kustomization.yaml`.

Validation performed in this build:

```console
$ kubectl apply --dry-run=client -k deploy/k8s
namespace/edgevision created (dry run)
configmap/evg-config created (dry run)
secret/evg-secrets created (dry run)
service/evg-gateway created (dry run)
service/evg-postgres created (dry run)
service/evg-redis created (dry run)
deployment.apps/evg-gateway created (dry run)
deployment.apps/evg-redis created (dry run)
statefulset.apps/evg-postgres created (dry run)
horizontalpodautoscaler.autoscaling/evg-gateway created (dry run)
```

This dry run requires a reachable API server for resource discovery, so a
throwaway `kind` cluster (`kind create cluster --name evg-validate`) was used and
then deleted. `kubectl kustomize deploy/k8s` also renders cleanly without a
cluster (342 lines, 10 resources).

## 3. Terraform

`terraform/` provisions the namespace, config map, secret, Deployment, Service
and HPA with the Kubernetes provider; the Helm provider is configured and can
optionally install a standalone Redis chart (`deploy_redis_with_helm=true`).
Postgres is expected to exist already (from the manifests above, or a managed
service) because the Terraform module focuses on the gateway application
resources.

```sh
cd terraform
terraform fmt -check
terraform init -backend=false
terraform validate
# terraform apply -var image_tag=<tag> -var postgres_dsn=... -var device_secret=...
```

Real output from this build:

```console
$ terraform fmt -check
$ terraform init -backend=false
...
Terraform has been successfully initialized!
$ terraform validate
Success! The configuration is valid.
```

Inputs of interest: `replicas`, `min_replicas`, `max_replicas`, `image_tag`,
`queue_depth_target`, `cpu_request`/`memory_request`/`cpu_limit`/`memory_limit`,
`postgres_dsn`, `redis_addr`, `device_secret`, `deploy_redis_with_helm`.
Outputs: namespace, service name, HPA name, image reference.

## Autoscaling story

**Metric.** `evg_queue_depth`, exported by the gateway on
`/metrics/prometheus` and scraped from each pod via the annotations
`prometheus.io/scrape=true`, `prometheus.io/port=8080`,
`prometheus.io/path=/metrics/prometheus`. A Prometheus Adapter rule exposes it
as a pod custom metric:

```yaml
rules:
  - seriesQuery: 'evg_queue_depth{namespace!="",pod!=""}'
    resources:
      overrides:
        namespace: {resource: namespace}
        pod: {resource: pod}
    name:
      matches: "^evg_queue_depth$"
      as: "evg_queue_depth"
    metricsQuery: 'avg(<<.Series>>{<<.LabelMatchers>>}) by (pod)'
```

**Policy.** `hpa.yaml` targets an average of **400 queued frames per pod**,
min 2 / max 20 replicas. Scale-up: at most 4 pods per 60 s after a 30 s
stabilization window. Scale-down: 1 pod per 60 s after 300 s.

**Why queue depth rather than CPU.** The worker pool is CPU-bound, but CPU
saturation is a lagging signal: by the time CPU is pegged, accepted frames are
already ageing in the queue and p99 has collapsed (the benchmark shows exactly
this in scenario B: process throughput holds near capacity while server p95
reaches ~1.9 s). Queue depth measures offered load minus service rate directly,
so it rises while there is still headroom to add replicas, and it falls
immediately after a burst even if CPU stays warm. A CPU HPA also cannot see
that shedding is already protecting latency.

**Measured capacity used to choose the threshold.** The worker sweep measured
1,116 processed fps at 8 workers, with 8 workers as the knee and 16 workers
buying no extra throughput. At a 2,048-frame queue, an average depth of 400
leaves >1,600 frames of burst absorption before the 85% shed threshold, which
is roughly 1.5 s of headroom at capacity. A pod at capacity is therefore still
far from shedding when the HPA starts adding replicas.

**CPU fallback.** For clusters without a custom-metrics adapter, apply
`deploy/k8s/hpa-cpu.yaml` instead of `hpa.yaml`:

```sh
kubectl -n edgevision delete hpa evg-gateway
kubectl apply -f deploy/k8s/hpa-cpu.yaml
```

It targets 70% CPU utilization with the same replica bounds. It is a fallback,
not a replacement: it reacts later and cannot distinguish a busy-but-draining
queue from a saturated one.

## Operational details

- `terminationGracePeriodSeconds: 40` is longer than `EVG_DRAIN_DEADLINE=25s`,
  so SIGTERM always allows the worker pool to drain before SIGKILL.
- The container runs as non-root with a read-only root filesystem, no privilege
  escalation, and all capabilities dropped.
- Liveness is `/healthz`; readiness is `/readyz` (checks Postgres and Redis).
  A pod that loses its dependencies leaves the Service without being killed.
- Postgres uses `synchronous_commit=off` in both compose and the StatefulSet.
  That is a deliberate throughput choice for high-rate detection inserts; it can
  lose the last few hundred milliseconds of writes on an unclean shutdown.
  Change it for data you cannot replay.
- Devices authenticate with HMAC tokens; rotate `EVG_DEVICE_SECRET` by rolling
  pods and re-issuing tokens. There is no built-in rotation endpoint yet.
