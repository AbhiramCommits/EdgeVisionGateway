# Container image

Built with the repository `Dockerfile` (multi-stage, Go 1.23 Alpine builder,
distroless static runtime, non-root):

```console
$ docker build -t edgevisiongateway:dev .
...
$ docker image inspect edgevisiongateway:dev --format '{{.Size}}'
30156467

$ docker images edgevisiongateway:dev --format '{{.Repository}}:{{.Tag}} {{.Size}} {{.ID}}'
edgevisiongateway:dev 30.2MB 6507bfd179ed
```

| Property | Value |
| --- | --- |
| Compressed/local image size | **30,156,467 bytes (30.2 MB)** |
| Builder | `golang:1.23-alpine` |
| Runtime | `gcr.io/distroless/static-debian12:nonroot` |
| User | `nonroot:nonroot` (UID/GID 65532) |
| Binary | static, `CGO_ENABLED=0`, `-trimpath -ldflags="-s -w"` |
| Exposed ports | 8080 (REST), 9090 (gRPC) |

Runtime smoke test of the image itself (in-memory store, published ports):

```console
$ docker run -d --rm --name evg-image-test \
    -e EVG_STORE=mem -e EVG_AUTO_REGISTER=true \
    -p 18082:8080 -p 19092:9090 edgevisiongateway:dev
$ curl -s http://localhost:18082/healthz
{"status":"ok"}
$ curl -s -o /dev/null -w "%{http_code}\n" http://localhost:18082/readyz
200
$ curl -s http://localhost:18082/metrics/prometheus | head -4
# HELP evg_queue_depth Frames currently waiting in the bounded queue.
# TYPE evg_queue_depth gauge
evg_queue_depth 0
# HELP evg_queue_capacity Configured queue capacity.
```

## Health checks

Distroless has no shell, `curl` or package manager, so a Dockerfile
`HEALTHCHECK` command would have nothing to execute. The equivalent is an HTTP
probe:

- liveness: `GET /healthz` (process is up)
- readiness: `GET /readyz` (Postgres and Redis reachable; always ready for
  `EVG_STORE=mem`)

Both are wired into `deploy/k8s/gateway-deployment.yaml`. With plain Docker,
probe from the host with the commands above or use an orchestrator/healthcheck
sidecar.
