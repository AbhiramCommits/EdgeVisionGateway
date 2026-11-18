#!/usr/bin/env bash
# Runs the benchmark scenario matrix against a real gateway + Postgres + Redis
# (or EVG_BENCH_STORE=mem), then writes docs/results/benchmark.md from the raw
# JSON artefacts in docs/results/raw/.
set -euo pipefail
cd "$(dirname "$0")/.."
ROOT=$PWD
RAW=$ROOT/docs/results/raw
mkdir -p "$RAW"

STORE=${EVG_BENCH_STORE:-postgres}
QUEUE=${EVG_BENCH_QUEUE:-2048}
HTTP_PORT=${EVG_BENCH_HTTP_PORT:-18080}
GRPC_PORT=${EVG_BENCH_GRPC_PORT:-19090}
GATEWAY_PID=""

cleanup() {
  if [[ -n "${GATEWAY_PID}" ]] && kill -0 "$GATEWAY_PID" 2>/dev/null; then
    kill -TERM "$GATEWAY_PID" 2>/dev/null || true
    for _ in $(seq 1 80); do
      kill -0 "$GATEWAY_PID" 2>/dev/null || break
      sleep 0.5
    done
    kill -KILL "$GATEWAY_PID" 2>/dev/null || true
  fi
  GATEWAY_PID=""
}
trap cleanup EXIT

echo "== building =="
make build >/dev/null

if [[ "$STORE" == "postgres" ]]; then
  echo "== starting postgres + redis =="
  docker compose up -d >/dev/null
  for _ in $(seq 1 60); do
    p=$(docker inspect -f '{{.State.Health.Status}}' evg-postgres 2>/dev/null || echo starting)
    r=$(docker inspect -f '{{.State.Health.Status}}' evg-redis 2>/dev/null || echo starting)
    [[ "$p" == "healthy" && "$r" == "healthy" ]] && break
    sleep 1
  done
  if [[ "$p" != "healthy" || "$r" != "healthy" ]]; then
    echo "postgres/redis did not become healthy" >&2
    exit 1
  fi
  # Start each benchmark run from an empty detection store. Partitions and
  # indexes are re-created by the gateway's Migrate on startup.
  docker exec evg-postgres psql -U evg -d evg -c 'TRUNCATE TABLE detections' >/dev/null
fi

start_gateway() {
  local workers=$1
  cleanup
  : > "$RAW/gateway.log"
  EVG_STORE="$STORE" \
  EVG_AUTO_REGISTER=true \
  EVG_HTTP_ADDR=":$HTTP_PORT" \
  EVG_GRPC_ADDR=":$GRPC_PORT" \
  EVG_WORKER_COUNT="$workers" \
  EVG_QUEUE_CAPACITY="$QUEUE" \
  EVG_SHED_THRESHOLD=0.85 \
  EVG_BATCH_MAX=500 \
  EVG_FLUSH_INTERVAL=20ms \
  EVG_CACHE_TTL=5m \
  EVG_DRAIN_DEADLINE=30s \
  ./bin/gateway >> "$RAW/gateway.log" 2>&1 &
  GATEWAY_PID=$!
  for _ in $(seq 1 120); do
    if curl -sf "http://localhost:$HTTP_PORT/readyz" >/dev/null; then
      return 0
    fi
    sleep 0.5
  done
  echo "gateway failed to start" >&2
  tail -40 "$RAW/gateway.log" >&2
  exit 1
}

wait_drain() {
  local deadline=$((SECONDS + 120))
  while (( SECONDS < deadline )); do
    depth=$(curl -s "http://localhost:$HTTP_PORT/metrics" \
      | python3 -c 'import json,sys; print(json.load(sys.stdin)["queue_depth"])' 2>/dev/null || echo 1)
    [[ "$depth" == "0" ]] && return 0
    sleep 1
  done
  echo "warning: queue did not drain before deadline" >&2
  return 0
}

run_scenario() {
  local name=$1 workers=$2 devices=$3 fps=$4 duration=$5 burst_at=$6 burst_mult=$7 burst_dur=$8
  echo "== scenario $name: workers=$workers devices=$devices fps=$fps duration=${duration}s burst=${burst_at}s x${burst_mult} =="
  start_gateway "$workers"
  ./bin/loadgen \
    -scenario "$name" \
    -gateway-workers "$workers" \
    -devices "$devices" \
    -fps-per-device "$fps" \
    -duration "${duration}s" \
    -image-size 640x480 \
    -target http \
    -http-addr "http://localhost:$HTTP_PORT" \
    -grpc-addr "localhost:$GRPC_PORT" \
    -sample-every 5 \
    -sample-device-stride 50 \
    -poll-interval 100ms \
    -max-inflight-queries 16 \
    -query-timeout 30s \
    -burst-at "${burst_at}s" \
    -burst-multiplier "$burst_mult" \
    -burst-duration "${burst_dur}s" \
    -out "$RAW/loadgen_$name.json"
  wait_drain
  curl -s "http://localhost:$HTTP_PORT/metrics" > "$RAW/metrics_$name.json"
  cp "$RAW/gateway.log" "$RAW/gateway_$name.log"
  cleanup
}

run_scenario A_steady 8 500 2 60 0 1 0
run_scenario B_fleet2000 8 2000 1 60 0 1 0
run_scenario C_burst5x 8 2000 1 60 20 5 15
for w in 2 4 8 16; do
  run_scenario "D_workers_$w" "$w" 2000 1 30 0 1 0
done

echo "== detector microbenchmark =="
go test -run '^$' -bench 'BenchmarkDetect' -benchmem -count=1 ./internal/vision \
  | tee "$RAW/bench_detect.txt"

echo "== writing summary =="
BENCH_STORE="$STORE" \
BENCH_QUEUE="$QUEUE" \
BENCH_CPU="$(sysctl -n hw.ncpu)" \
BENCH_MEM="$(sysctl -n hw.memsize)" \
BENCH_OS="$(sw_vers -productName) $(sw_vers -productVersion) ($(uname -m))" \
BENCH_GO="$(go version)" \
python3 scripts/summarize_bench.py > docs/results/benchmark.md

echo "== done: docs/results/benchmark.md =="
