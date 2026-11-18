# EdgeVisionGateway

EdgeVisionGateway is a Go backend that ingests image frames and heartbeats from
a simulated fleet of thousands of edge cameras, runs pure-Go computer-vision
object detection on those frames in a bounded worker pool, persists detections
to a time-partitioned PostgreSQL store with a Redis hot cache, and serves
low-latency query APIs over REST and gRPC. It is built to stay up under
overload: admission control, explicit load shedding with `429`/`Retry-After`,
per-device HMAC auth, graceful drain on shutdown, and a measured autoscaling
story based on queue depth.

**Measured headline numbers from this build** (details below):
**1,008 frames/sec processed** at 640x480 on 8 cores,
**p50 2.0 s / p95 2.5 s / p99 2.5 s** server-side latency at saturation,
**3.3% shed** at ~1,000 offered fps, **78.7% shed** under a 5x burst with the
queue held at its 2,048-frame bound, **1.00 precision / 1.00 recall / 1.00
mAP@0.5** on the synthetic accuracy harness (with an honest noise sweep down to
0.0041 precision at extreme noise), and a **30.2 MB** distroless image.

## Architecture

```text
                    simulated fleet (cmd/loadgen)
                      │  REST :8080              │  gRPC :9090
                      ▼                          ▼
              ┌────────────────────────────────────────┐
              │ per-device auth: HMAC token, registry  │
              │ lookup, negative-result LRU            │
              └───────────────────┬────────────────────┘
                                  │
                                  ▼
              ┌────────────────────────────────────────┐
              │ bounded queue (capacity 2048)          │
              │ shed threshold 85%; low-priority first │──► 429 / ResourceExhausted
              └───────────────────┬────────────────────┘    + Retry-After
                                  │
                                  ▼
              ┌────────────────────────────────────────┐
              │ worker pool, one detector per worker   │
              │ JPEG/PNG → gray → background → diff →  │
              │ morphology → connected components →NMS │
              └───────┬───────────────────────┬────────┘
                      │ batched COPY          │ latest frame
                      ▼                       ▼
        ┌──────────────────────────┐   ┌──────────────────────┐
        │ PostgreSQL 16            │   │ Redis 7 hot cache    │
        │ detections partitioned   │   │ det:{device}:latest  │
        │ by UTC day, per-partition│   │ + device registry    │
        │ (device,time)/(label,time)│  │ + liveness sorted set│
        └────────────┬─────────────┘   └──────────┬───────────┘
                     └────────────┬────────────────┘
                                  ▼
                    query API: GET /v1/devices/{id}/detections
                               GET /v1/detections
                    response includes source: "cache" | "db"
                                  │
                    /metrics (JSON) + /metrics/prometheus
                                  │
                     HPA on evg_queue_depth (CPU fallback)
```

### Design decisions

**Bounded queue + explicit shedding.** Latency is the product here, so the
gateway refuses work it cannot finish instead of accepting an unbounded backlog.
`TryEnqueue` is non-blocking; at 85% occupancy frames from devices that produced
a detection in the last 500 ms are rejected first (they are the most likely to
be near-duplicates and already represented in the cache), and once the channel
is full every producer gets `429` with `Retry-After`. The queue's high-water
mark, shed counts and enqueue→persisted quantiles are exported for operators.

**Time-partitioned PostgreSQL.** `detections` is `PARTITION BY RANGE
(captured_at)` with one partition per UTC day. Queries include a time window, so
the planner prunes to the relevant days; each partition carries
`(device_id, captured_at DESC)` and `(label, captured_at DESC)` indexes. Daily
partitions also make retention a metadata operation (`DROP PARTITION`) instead
of a mass delete. Writes use `pgx.CopyFrom` in batches of up to 500 rows.

**Redis on the read hot path.** The newest detections for a device are written
to `det:{device}:latest` with a TTL after the durable insert. A query whose
window contains that cached frame is served from Redis with
`"source":"cache"`; older or non-overlapping windows go to Postgres and return
`"source":"db"`. The device registry and heartbeat liveness live in Redis too
(hash + sorted set), so auth does not hit Postgres on every frame. Unknown
tokens are negatively cached to keep the auth path cheap.

**HPA on queue depth, not CPU.** A CPU threshold reacts after workers are
already saturated, when accepted frames are ageing and p99 has collapsed. Queue
depth is offered load minus service rate, so it rises while there is still time
to add capacity and falls as soon as a burst ends. The gateway exports
`evg_queue_depth` on `/metrics/prometheus`; the HPA targets an average of 400
queued frames per pod (min 2, max 20). A CPU-based HPA is committed as an
explicit fallback for clusters without a custom-metrics adapter.

**One detector per worker.** The default `MotionDetector` keeps a running
background model, so state cannot be shared across concurrent frames. Each
worker constructs its own detector; all intermediate images are reused, giving
14 allocations and 3,984 B per 640x480 frame.

## How to run

Prerequisites: Go 1.23+ (this build was compiled and tested with Go 1.27.1 and
also verified inside `golang:1.23-alpine`), Docker with Compose, `make`.

```sh
docker compose up -d            # Postgres 16 + Redis 7, both healthchecked
make build                      # bin/gateway, bin/loadgen, bin/evaldet
EVG_AUTO_REGISTER=true ./bin/gateway
```

`EVG_AUTO_REGISTER=true` is for local demos and the simulated fleet: a validly
signed token for an unknown device registers it on first use. Leave it off in
production. Defaults: REST `:8080`, gRPC `:9090`, Postgres
`postgres://evg:evg@localhost:5432/evg`, Redis `localhost:6379`, 8 workers, queue
capacity 4096, 5-minute cache TTL. With no Docker at all,
`EVG_STORE=mem ./bin/gateway` runs the in-memory store (non-durable; used by the
unit tests).

Upload one frame and read the detections back (the first `demo-empty.jpg` frame
bootstraps the background model; the second contains four moving objects):

```sh
TOKEN=$(go run ./cmd/tokengen -device cam-1)
NOW=$(date -u +%Y-%m-%dT%H:%M:%SZ)

curl -s -X POST http://localhost:8080/v1/frames \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: image/jpeg" \
  -H "X-Device-ID: cam-1" -H "X-Frame-ID: demo-frame-0" \
  -H "X-Captured-At: $NOW" --data-binary @testdata/demo-empty.jpg
# {"frame_id":"demo-frame-0","queue_depth":0,"status":"queued"}

curl -s -X POST http://localhost:8080/v1/frames \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: image/jpeg" \
  -H "X-Device-ID: cam-1" -H "X-Frame-ID: demo-frame-1" \
  -H "X-Captured-At: $NOW" --data-binary @testdata/demo-objects.jpg
# {"frame_id":"demo-frame-1","queue_depth":0,"status":"queued"}

FROM=$(date -u -v-2M +%Y-%m-%dT%H:%M:%SZ)   # GNU date: date -u -d '2 minutes ago' +%Y-%m-%dT%H:%M:%SZ
TO=$(date -u -v+1M +%Y-%m-%dT%H:%M:%SZ)
sleep 0.5   # let the worker persist + populate the cache
curl -s "http://localhost:8080/v1/devices/cam-1/detections?from=$FROM&to=$TO" \
  -H "Authorization: Bearer $TOKEN"
```

Real response captured from this build:

```json
{"detections":[{"device_id":"cam-1","frame_id":"demo-frame-1","captured_at":"2026-11-18T01:46:27Z","label":"object","confidence":0.708984375,"x":81,"y":157,"w":66,"h":44},{"device_id":"cam-1","frame_id":"demo-frame-1","captured_at":"2026-11-18T01:46:27Z","label":"object","confidence":0.708984375,"x":146,"y":395,"w":66,"h":44},{"device_id":"cam-1","frame_id":"demo-frame-1","captured_at":"2026-11-18T01:46:27Z","label":"person","confidence":0.6328125,"x":35,"y":33,"w":36,"h":72},{"device_id":"cam-1","frame_id":"demo-frame-1","captured_at":"2026-11-18T01:46:27Z","label":"person","confidence":0.6328125,"x":127,"y":260,"w":36,"h":72}],"source":"cache"}
```

Under saturation the same endpoint answers `429` with `Retry-After: 1`; a
missing/tampered token answers `401`. The complete transcript, including a real
8-concurrent-upload burst (3× `202`, 5× `429`) and the resulting metrics
snapshot, is in [`docs/API.md`](docs/API.md).

```sh
make test              # unit tests, zero external services (in-memory store)
make test-integration  # Postgres/Redis integration tests (needs compose up)
make eval              # detection accuracy harness → docs/results/vision_eval.json
make bench             # full scenario matrix → docs/results/benchmark.md (≈10 min)
```

`make test` passes from a clean clone with no containers running.
`make bench` starts the compose stack itself, restarts the gateway per scenario,
waits for drain, scrapes `/metrics`, and regenerates the results file.

The ONNX backend is optional and build-tagged:

```sh
brew install onnxruntime                       # any ONNX Runtime 1.x shared lib
go build -tags onnx -o bin/gateway ./cmd/gateway
EVG_DETECTOR=onnx EVG_ONNX_MODEL=/path/yolov8n.onnx \
EVG_ONNXRUNTIME_LIB=/opt/homebrew/lib/libonnxruntime.dylib ./bin/gateway
```

A binary built without the tag logs a warning and falls back to `motion-v1`
rather than failing ingest. See [`docs/VISION.md`](docs/VISION.md).

## Results

**Measurement setup.** macOS 27.2.1 on an Apple M3 (arm64), 8 logical cores,
16 GiB RAM; Docker Desktop 29.8.2 with an 8-vCPU / ~7.75 GiB VM running real
PostgreSQL 16 and Redis 7. The load generator, gateway workers, Postgres and
Redis all share those cores, so these are single-host end-to-end numbers. Raw
commands: `docker compose up -d && make build && make bench`; full artefacts and
method in [`docs/results/benchmark.md`](docs/results/benchmark.md).

### Throughput and latency (real Postgres + Redis)

| Scenario | Target fps | Attempted fps | Accepted fps | Sustained processed fps | Shed | Shed % | Server p50 / p95 / p99 | E2E p50 / p95 / p99 | Max queue |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| A: 500 devices @ 2 fps, 60 s | 1,000 | 969 | 938 | 938 | 1,916 | 3.3% | 1,132 / 2,239 / 2,577 ms | 398 / 1,760 / 2,460 ms | 1,971 |
| B: 2,000 devices @ 1 fps, 60 s | 2,000 | 1,918 | 924 | 924 | 60,731 | 51.8% | 2,152 / 2,762 / 2,874 ms | 2,182 / 2,804 / 2,882 ms | 2,048 |
| C: B + 5x burst, 20–35 s | 2,000 (10,000 in burst) | 3,503 | 747 | 747 | 168,270 | 78.7% | 2,309 / 5,084 / 5,428 ms | 2,473 / 5,208 / 5,500 ms | 2,048 |

Sustained processed fps is `frames_processed / measured elapsed window` from
the server's own counters after the queue drained. E2E latency is the load
generator's sampled read-back through the query API; server latency is the
gateway's enqueue→persisted histogram and therefore includes queue wait. Both
are reported because they answer different questions.

### Worker-count sweep (fixed 2,000 fps offered)

| Workers | Processed fps | Shed % | Server p50 / p95 / p99 | E2E p95 | Max queue |
| ---: | ---: | ---: | ---: | ---: | ---: |
| 2 | 465 | 75.0% | 4,790 / 5,441 / 5,499 ms | 5,462 ms | 2,048 |
| 4 | 822 | 55.8% | 2,640 / 2,876 / 2,897 ms | 2,883 ms | 2,048 |
| 8 | **1,008** | 45.6% | 2,038 / 2,451 / 2,532 ms | 2,495 ms | 2,048 |
| 16 | 958 | 47.7% | 1,970 / 2,761 / 2,879 ms | 2,770 ms | 2,048 |

The knee is between 4 and 8 workers: throughput rises 822 → 1,008 fps, then
falls to 958 fps at 16 workers while p95 rises, i.e. the extra goroutines
oversubscribe 8 cores. Eight workers is the best measured point.

### Devices supported per instance

At the measured peak of **1,008 processed fps** on one 8-core instance:

- at **1 fps/device**: `1,008 / 1 = 1,008` devices per instance.
- at **2 fps/device** (the assumption used for the derated figure):
  `1,008 / 2 = 504` devices.
- with 30% headroom for burst absorption and scheduler variance:
  `0.7 × 504 = 353` devices per instance at 2 fps.

These are 640x480 JPEG frames with full detection. Smaller frames or a lower
detection rate raise the device count; the ONNX backend would change it again.

### Detection accuracy (synthetic ground truth)

`go run ./cmd/evaldet -frames 300 -objects 4 -noise 6 -seed 7` — 300 frames,
640x480, four moving rectangles, exact ground-truth boxes:

| Metric | Value |
| --- | ---: |
| TP / FP / FN | 1,152 / 0 / 0 |
| Precision | 1.0000 |
| Recall | 1.0000 |
| F1 | 1.0000 |
| Mean IoU (matched) | 1.0000 |
| AP@0.5 (all-point interpolation) | 1.0000 |
| Mean detect latency | 2,133 µs/frame |

The perfect score is real **for this scene**: high-contrast rectangles on a
static background are exactly what background subtraction is designed for. It
is not a claim about real-world imagery. The noise sweep is the honest signal:

| Noise sigma | Precision | Recall | Mean IoU | AP@0.5 |
| ---: | ---: | ---: | ---: | ---: |
| 0 | 1.0000 | 1.0000 | 1.0000 | 1.0000 |
| 4 | 1.0000 | 1.0000 | 1.0000 | 1.0000 |
| 8 | 1.0000 | 1.0000 | 1.0000 | 1.0000 |
| 16 | 1.0000 | 1.0000 | 0.9955 | 1.0000 |
| 32 | 1.0000 | 1.0000 | 0.8807 | 1.0000 |
| 48 | 0.9156 | 0.9323 | 0.6446 | 0.9199 |
| 64 | 0.0041 | 0.0512 | 0.5813 | 0.0064 |

Raw per-run JSON and the full table are in
[`docs/results/vision_eval.json`](docs/results/vision_eval.json) and
[`docs/results/vision_eval_sweep.md`](docs/results/vision_eval_sweep.md).

### Detector microbenchmark

`go test -run '^$' -bench BenchmarkDetect -benchmem -count=1 ./internal/vision`:

| Frame size | ns/op | ms/frame | B/op | allocs/op |
| --- | ---: | ---: | ---: | ---: |
| 640x480 | 1,609,888 | 1.61 | 3,984 | 14 |
| 1280x720 | 4,646,728 | 4.65 | 6,544 | 14 |

### Build, tests and image

| Item | Value |
| --- | ---: |
| Distroless image | **30,156,467 bytes (30.2 MB)**, `nonroot`, static binary |
| Test functions | **87** |
| Unit coverage (`go test -race -coverprofile … ./...`) | **47.4%** of statements |
| Coverage including `-tags integration` (real PG + Redis) | **52.2%** |
| Storage integration tests | **15/15 pass** |
| CI-equivalent checks | gofmt, vet, race tests, integration, docker build, terraform validate, k8s dry-run — all pass |

Evidence: [`docs/results/ci.md`](docs/results/ci.md) and
[`docs/results/image.md`](docs/results/image.md).

### Measurement notes

- Hardware/OS: macOS 27.2.1 (Darwin 27.2.0, arm64), Apple M3, 8 logical cores,
  16 GiB RAM; Docker Desktop 29.8.2, 8 vCPU / 8,319,770,624 B VM. See
  [`docs/ENVIRONMENT.md`](docs/ENVIRONMENT.md).
- Store: **real PostgreSQL 16 + Redis 7** via `docker-compose.yml`, not the
  in-memory fallback. The `EVG_STORE=mem` path exists for unit tests and
  no-Docker runs.
- Exact commands: `docker compose up -d`, `make build`, `make bench` (which
  reruns `go test -bench BenchmarkDetect`), `go run ./cmd/evaldet -frames 300
  -objects 4 -noise 6 -seed 7`.
- E2E read-backs sample every 50th device and 1 in 5 frames from those devices,
  with at most 16 concurrent pollers at 100 ms (≤160 queries/s). Sample counts
  were 40–130 per run, so treat p99 as indicative.
- The load generator is CPU-bound at the 10,000 fps burst target; it actually
  attempted 3,503 fps on average, still 3.5x measured per-instance capacity.
- Single-run numbers on a shared host: repeated full runs moved the 8-worker
  result by roughly 10%, so read absolute throughput as ±10%.
- Detector latency also varies with host load: the committed eval run measured
  2,133 µs/frame; an immediate repeat measured 1,649 µs/frame with identical
  precision/recall/IoU/AP. Accuracy reproduced exactly; latency did not.

## Project layout

```text
cmd/
  gateway/       REST + gRPC server, store/detector/queue/worker wiring
  loadgen/       simulated camera fleet, outcome and latency measurement
  evaldet/       synthetic ground-truth accuracy harness
  mkframe/       writes demo frames used by the curl examples
  tokengen/      prints a device's HMAC bearer token
internal/
  api/           net/http handler and gRPC server (shared service layer)
  config/        EVG_* env loading and validation
  ingest/        auth, admission, query routing, worker pool
  obs/           atomic counters and log-spaced latency histogram
  pb/            committed protobuf/gRPC generated code
  queue/         bounded queue, shed policy, high-water mark
  storage/       Postgres partitions, Redis registry/cache, in-memory store
  vision/        detector interface, motion pipeline, synth generator, eval, ONNX
proto/edgevision/v1/gateway.proto
deploy/k8s/      namespace, config, secret example, deployments, HPA, kustomize
terraform/       kubernetes + helm providers, namespace→HPA module
scripts/         bench.sh, summarize_bench.py
docs/            ENVIRONMENT, VISION, API, DEPLOY, results/
testdata/        demo-empty.jpg, demo-objects.jpg
```

## Limitations and what I'd do next

- **Synthetic imagery.** Accuracy is measured on generated rectangles, where a
  background model is near-ideal. Real footage (shadows, lighting changes,
  crowds, compression artifacts) will score much lower. The ONNX backend
  compiles but was not executed here (no model or runtime installed), so no
  ONNX accuracy or latency is claimed.
- **Heuristic detector.** `MotionDetector` is a presence/motion detector, not a
  semantic classifier. A production system should track objects across frames
  and run a real detector; the per-worker background model also needs to become
  a per-camera session to be correct for thousands of independent scenes.
- **Single-host benchmark.** Gateway, load generator, Postgres and Redis share
  8 cores, and the client is CPU-bound during the 5x burst. Multi-node numbers
  with a dedicated load generator and managed Postgres/Redis would differ.
- **Cache semantics.** Only the newest frame per device is cached; arbitrary
  historical windows always hit Postgres. Duplicate `frame_id`s are not
  deduplicated (at-least-once inserts).
- **Autoscaling not applied.** Manifests and Terraform validate cleanly (k8s
  dry-run against a throwaway kind cluster, `terraform validate`), but no
  production cluster was touched. Queue-depth HPA needs a Prometheus Adapter;
  the CPU fallback is committed and documented.
- **Security hardening.** TLS/mTLS, per-route authorization, token rotation and
  a real secret manager are out of scope for this build; the committed secret is
  an example. Device tokens are static HMACs.
- **Retention.** Daily partitions are created on startup, but an automated
  partition-drop job and `pg_partman`-style maintenance are not included.
- **Durability choice.** Postgres runs with `synchronous_commit=off` for batch
  throughput; an unclean host failure can lose the last few hundred
  milliseconds of detections.
- **Next steps:** real YOLO/ONNX evaluation, per-device sessions, tracking,
  Kafka/durable ingest buffer, Prometheus-native metrics, partition retention,
  multi-node load tests, TLS and token rotation.

## Documentation

- [Vision pipeline, thresholds, AP method, ONNX build](docs/VISION.md)
- [Deployment: compose, kind, Terraform, autoscaling](docs/DEPLOY.md)
- [API surface and real REST transcript](docs/API.md)
- [Benchmark results](docs/results/benchmark.md) ·
  [accuracy sweep](docs/results/vision_eval_sweep.md) ·
  [image](docs/results/image.md) · [CI](docs/results/ci.md) ·
  [environment](docs/ENVIRONMENT.md)
