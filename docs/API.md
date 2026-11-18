# API

Two transports share one service layer (`internal/ingest.Service`), so
admission control, shedding and query routing behave identically:

- REST on `EVG_HTTP_ADDR` (default `:8080`)
- gRPC on `EVG_GRPC_ADDR` (default `:9090`), service `edgevision.v1.Gateway`

Every device-facing call requires a bearer token:

```
Authorization: Bearer <base64url(deviceID)>.<hex HMAC-SHA256(deviceID, EVG_DEVICE_SECRET)>
```

Generate one with:

```sh
go run ./cmd/tokengen -device cam-1
# Y2FtLTE.850b83ef6a23453208189b49285b03e7248d0309ee32ff02f3c6ed895aa7d3b3
```

Tokens are deterministic for a given secret, so the load generator and the
curl examples can mint them locally. For these examples the gateway ran with
`EVG_STORE=mem EVG_AUTO_REGISTER=true` (auto-registration creates a signed but
unknown device on first use; keep it off in production). The default
Postgres + Redis stack behaves the same through `storage.HybridStore`.

## Endpoints

| Method | Path | Auth | Purpose |
| --- | --- | --- | --- |
| POST | `/v1/frames` | Bearer | Ingest a raw JPEG/PNG body or a `multipart/form-data` `frame` part. 202 accepted, 429 shed, 401 bad token |
| POST | `/v1/heartbeat` | Bearer | Refresh liveness (Redis TTL) and durable `last_seen_at` |
| GET | `/v1/devices/{id}/detections` | Bearer | `?from&to&limit&source=db`; serves Redis when the window is recent, Postgres otherwise, response includes `source` |
| GET | `/v1/detections` | Bearer | `?label&from&to&limit`; Postgres label scan |
| GET | `/healthz` | none | process liveness |
| GET | `/readyz` | none | Postgres/Redis connectivity (memory store is always ready) |
| GET | `/metrics` | none | JSON snapshot: counters, queue depth, shed rate, latency quantiles, alive devices |
| GET | `/metrics/prometheus` | none | Prometheus text exposition of the same metrics; source of the HPA custom metric `evg_queue_depth` |

Frame metadata travels in headers: `X-Device-ID`, `X-Frame-ID`,
`X-Captured-At` (RFC3339). The token is authoritative; a conflicting
`X-Device-ID` is rejected with 401.

## Real transcript

The two test frames in `testdata/` come from `cmd/mkframe`:
`demo-empty.jpg` is frame 0 of the generator (empty warm-up frame) and
`demo-objects.jpg` is frame 20 with four moving rectangles. The first frame
bootstraps the background model, the second produces detections.

```console
$ go run ./cmd/tokengen -device cam-1
Y2FtLTE.850b83ef6a23453208189b49285b03e7248d0309ee32ff02f3c6ed895aa7d3b3

$ TOKEN=$(go run ./cmd/tokengen -device cam-1)
$ NOW=$(date -u +%Y-%m-%dT%H:%M:%SZ)

$ curl -s -i -X POST http://localhost:18080/v1/frames \
    -H "Authorization: Bearer $TOKEN" -H "Content-Type: image/jpeg" \
    -H "X-Device-ID: cam-1" -H "X-Frame-ID: demo-frame-0" \
    -H "X-Captured-At: $NOW" --data-binary @testdata/demo-empty.jpg
HTTP/1.1 202 Accepted
Content-Type: application/json
X-Request-Id: 9ccff4e087db63f8
Content-Length: 62

{"frame_id":"demo-frame-0","queue_depth":0,"status":"queued"}

$ curl -s -i -X POST http://localhost:18080/v1/frames \
    -H "Authorization: Bearer $TOKEN" -H "Content-Type: image/jpeg" \
    -H "X-Device-ID: cam-1" -H "X-Frame-ID: demo-frame-1" \
    -H "X-Captured-At: $NOW" --data-binary @testdata/demo-objects.jpg
HTTP/1.1 202 Accepted
X-Request-Id: 09c9e3a4d2e2c5da
Content-Length: 62

{"frame_id":"demo-frame-1","queue_depth":0,"status":"queued"}

$ FROM=$(date -u -v-2M +%Y-%m-%dT%H:%M:%SZ)
$ TO=$(date -u -v+1M +%Y-%m-%dT%H:%M:%SZ)
$ curl -s "http://localhost:18080/v1/devices/cam-1/detections?from=$FROM&to=$TO" \
    -H "Authorization: Bearer $TOKEN"
{"detections":[{"device_id":"cam-1","frame_id":"demo-frame-1","captured_at":"2026-11-18T01:46:27Z","label":"object","confidence":0.708984375,"x":81,"y":157,"w":66,"h":44},{"device_id":"cam-1","frame_id":"demo-frame-1","captured_at":"2026-11-18T01:46:27Z","label":"object","confidence":0.708984375,"x":146,"y":395,"w":66,"h":44},{"device_id":"cam-1","frame_id":"demo-frame-1","captured_at":"2026-11-18T01:46:27Z","label":"person","confidence":0.6328125,"x":35,"y":33,"w":36,"h":72},{"device_id":"cam-1","frame_id":"demo-frame-1","captured_at":"2026-11-18T01:46:27Z","label":"person","confidence":0.6328125,"x":127,"y":260,"w":36,"h":72}],"source":"cache"}
```

The four boxes match the generator's ground truth exactly for this frame; the
response came from the Redis-shaped hot cache (`"source":"cache"`). Adding
`&source=db` forces the Postgres/memstore path and returns `"source":"db"`.

### Auth failure

```console
$ curl -s -i -X POST http://localhost:18081/v1/frames \
    -H "Content-Type: image/jpeg" --data-binary @testdata/demo-objects.jpg
HTTP/1.1 401 Unauthorized
Content-Type: application/json
X-Request-Id: 3fd034aa5d778925
Content-Length: 33

{"error":"invalid device token"}
```

### Backpressure: 429 with Retry-After

Gateway started with `EVG_QUEUE_CAPACITY=1 EVG_WORKER_COUNT=1`; eight concurrent
uploads:

```console
$ seq 1 8 | xargs -P8 -I{} curl -s -o /tmp/evg_shed_{}.json -w "%{http_code}\n" \
    -X POST http://localhost:18081/v1/frames \
    -H "Authorization: Bearer $TOKEN2" -H "X-Frame-ID: burst-{}" \
    -H "Content-Type: image/jpeg" --data-binary @testdata/demo-objects.jpg | sort | uniq -c
   3 202
   5 429

$ grep -l "queue saturated" /tmp/evg_shed_*.json | head -1 | xargs cat
{"error":"queue saturated: frame shed","retry_after_ms":1000}
```

The 429 responses carry `Retry-After: 1`. Server metrics after the burst:

```json
{"frames_received":3,"frames_shed":5,"frames_processed":3,"detections_written":0,
 "auth_failures":1,"detect_errors":0,"cache_hits":0,"cache_misses":0,
 "cache_hit_rate":0,"shed_rate":0.625,"queue_depth":0,"queue_capacity":1,
 "alive_devices":1,
 "latency":{"count":3,"sum_ms":78.737083,"mean_ms":26.245694333333333,
            "max_ms":33.144083,"p50_ms":24.375724,"p90_ms":35.602216,
            "p95_ms":36.269599,"p99_ms":36.803505,"p999_ms":36.923634}}
```

`detections_written` is 0 here because all eight uploads were the same frame:
after the first frame establishes the background, an identical second frame
contains no motion. The load generator varies every frame, so production load
produced non-zero detections.

## gRPC

The proto is `proto/edgevision/v1/gateway.proto`; generated code is committed in
`internal/pb`. Auth uses the `authorization` metadata key with the same Bearer
token. Status mapping:

| Condition | gRPC code |
| --- | --- |
| missing/bad token | `Unauthenticated` |
| token/device mismatch | `PermissionDenied` |
| queue shed | `ResourceExhausted` (with `retry-after-ms` header) |
| shutting down | `Unavailable` |
| bad window/empty frame | `InvalidArgument` |

Covered by `internal/api/grpc_test.go` over an in-process `bufconn` connection.
