# Benchmark results

Produced by `make bench` → `scripts/bench.sh`. Every figure below is read from
the raw artefacts in `docs/results/raw/` (`loadgen_*.json`, `metrics_*.json`,
`bench_detect.txt`); nothing is hand-entered.

## Environment

- Host: macOS 27.2.1 (arm64), 8 logical CPUs, 16.0 GiB RAM
- Go: go version go1.27.1 darwin/arm64
- Store: **real PostgreSQL 16 + Redis 7 (docker compose)**
- Queue capacity: 2048, shed threshold 0.85, 640x480 JPEG frames, 20 ms batch flush
- Load generator and gateway share the same host; the load generator, gateway workers,
  Postgres and Redis all compete for the same cores. Numbers are single-host end-to-end
  numbers, not isolated service benchmarks.

## Commands

```sh
docker compose up -d
make build
make bench
```

The `docs/results/raw/` artefacts are generated locally and intentionally not
committed (see `.gitignore`); re-run `make bench` to reproduce them.

`scripts/bench.sh` restarts the gateway before every scenario, runs the scenario,
waits for the queue to drain, scrapes `/metrics`, then moves on. Sustained processed
fps is `frames_processed / measured_elapsed_seconds`; accepted/shed/error fractions are
client-observed. E2E latency is the load generator's sampled read-back via
`GET /v1/devices/{id}/detections?source=db`; server p50/p95/p99 is the gateway's
enqueue → persisted histogram. Both are reported.

## Scenario matrix

| Scenario | Target fps | Attempted fps | Accepted fps | Sustained processed fps | Shed | Shed % | Server p50 / p95 / p99 (ms) | E2E p50 / p95 / p99 (ms) | E2E samples | Max queue | Errors |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| A: steady state | 1,000 | 969 | 938 | 938 | 1,916 | 3.3% | 1,132.5 / 2,239.2 / 2,577.1 | 397.7 / 1,760.4 / 2,460.1 | 130 | 1,971 | 0 |
| B: fleet scale | 2,000 | 1,918 | 924 | 924 | 60,731 | 51.8% | 2,152.3 / 2,761.9 / 2,873.9 | 2,181.5 / 2,803.7 / 2,882.2 | 73 | 2,048 | 0 |
| C: 5x burst | 2,000 | 3,503 | 747 | 747 | 168,270 | 78.7% | 2,308.5 / 5,084.0 / 5,427.5 | 2,473.1 / 5,208.1 / 5,500.3 | 121 | 2,048 | 7 |

Scenario definitions: A = 500 devices x 2 fps for 60 s; B = 2000 x 1 fps for 60 s;
C = B plus a 5x rate burst from t=20 s to t=35 s. The burst offers 10,000 fps and is
deliberately far past capacity, to exercise shedding rather than to pass.

## Worker-count sweep

Fixed offered load: 2,000 devices x 1 fps = 2,000 fps, 30 s per run; only `EVG_WORKER_COUNT` changes.

| Workers | Accepted fps | Processed fps | Shed % | Server p50 / p95 / p99 (ms) | E2E p95 (ms) | Max queue |
| ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 2 | 465 | 465 | 75.0% | 4,790.0 / 5,441.3 / 5,498.6 | 5,461.9 | 2,048 |
| 4 | 822 | 822 | 55.8% | 2,639.8 / 2,875.6 / 2,896.6 | 2,882.6 | 2,048 |
| 8 | 1,008 | 1,008 | 45.6% | 2,038.2 / 2,450.8 / 2,532.0 | 2,494.7 | 2,048 |
| 16 | 958 | 958 | 47.7% | 1,970.2 / 2,761.4 / 2,879.2 | 2,769.5 | 2,048 |

## Interpretation

- The worker sweep peaks at **1,008 processed fps with 8 workers**.
  8 workers is the smallest count reaching at least 95% of that peak.
  Throughput moves from 822 fps at 4 workers to 1,008 fps at 8,
  so the knee sits between 4 and 8 workers on this 8-core host.
  16 workers measured 958 fps; more workers did not raise throughput.
- Devices supported per gateway instance at 1 fps/device ≈ **1,008** (the sustained
  processed rate divided by 1 fps). At the more realistic 2 fps/device: **504** devices; derated by 30% for headroom: **353** devices per instance.
  Arithmetic shown: `processed_fps / 2` and `0.7 × processed_fps / 2`, where
  processed_fps = 1,008 measured above.
- A (1,000 offered fps) accepted 56,774 frames and shed
  1,916 (3.3%); sustained processing was 938 fps.
- B (2,000 offered fps) shed 51.8% and still sustained
  924 processed fps, i.e. the queue absorbed and then rejected the excess instead
  of collapsing latency for accepted frames.
- C targets a 5x burst (10,000 fps during the
  burst window); the load generator actually attempted 3,503 fps
  on average because it is itself CPU-bound at that rate. It shed 78.7% of
  requests while the queue high-water mark reached 2,048 of 2,048
  slots, which is the backpressure behaviour the bounded queue exists to provide.
  Load-generator errors in this run: transport=7; these are client-side transport errors
  under the burst, not gateway responses, and are counted separately from 429s.

## Measurement notes

- Server-side latency is measured from enqueue to a successful durable batch insert; it
  therefore includes time spent waiting behind other frames in the bounded queue.
- E2E read-backs sample every 50th device and 1 frame in 5 from those devices, with at
  most 16 concurrent pollers at a 100 ms interval. That caps read-back load at ~160
  queries/s; the small sample counts (40-130) are the honest
  consequence, so treat the p99 columns as indicative rather than exact.
- The load generator is not infinitely fast: at the 10,000 fps burst
  target it actually attempted 3,503 fps on average (CPU-bound client),
  which is still 3.5x the measured per-instance capacity.
- Scenario A sustained 938 fps (93% of the sweep's peak)
  and shed 3.3%; its peak queue (1,971) and p95 reflect operation near
  capacity rather than an idle system.
- These are single-run numbers on a shared 8-core host; repeated runs of the matrix
  moved the 8-worker result by roughly 10%, so treat absolute throughput as ±10%.

## Detector microbenchmark

`go test -run '^$' -bench BenchmarkDetect -benchmem -count=1 ./internal/vision`, alternating
two frames that contain moving objects so every iteration runs the full pipeline.

| Frame size | ns/op | ms/frame | B/op | allocs/op |
| --- | ---: | ---: | ---: | ---: |
| 640x480 | 1,609,888 | 1.61 | 3,984 | 14 |
| 1280x720 | 4,646,728 | 4.65 | 6,544 | 14 |

Raw output:

```
goos: darwin
goarch: arm64
pkg: github.com/edgevision/edgevisiongateway/internal/vision
cpu: Apple M3
BenchmarkDetect/640x480-8         	     730	   1609888 ns/op	    3984 B/op	      14 allocs/op
BenchmarkDetect/1280x720-8        	     266	   4646728 ns/op	    6544 B/op	      14 allocs/op
PASS
ok  	github.com/edgevision/edgevisiongateway/internal/vision	3.935s
```

