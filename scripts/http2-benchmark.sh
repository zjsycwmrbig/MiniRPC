#!/usr/bin/env bash
set -euo pipefail

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
work_dir=$(mktemp -d)
server_log="$work_dir/echo-server.log"
benchmark_output="$work_dir/benchmark.txt"
server_pid=""

duration=${MINIRPC_HTTP2_BENCH_DURATION:-3s}
warmup=${MINIRPC_HTTP2_BENCH_WARMUP:-500ms}
concurrency=${MINIRPC_HTTP2_BENCH_CONCURRENCY:-64}
handshakes=${MINIRPC_HTTP2_BENCH_HANDSHAKES:-16}

cleanup() {
  if [[ -n "$server_pid" ]] && kill -0 "$server_pid" 2>/dev/null; then
    kill -TERM "$server_pid" 2>/dev/null || true
    wait "$server_pid" 2>/dev/null || true
  fi
  rm -rf "$work_dir"
}
trap cleanup EXIT INT TERM

cd "$repo_root"
go build -trimpath -o "$work_dir/echo-server" ./cmd/echo-server
go build -trimpath -o "$work_dir/benchmark" ./benchmark

"$work_dir/echo-server" \
  -addr=127.0.0.1:0 \
  -http2 \
  -metrics-addr= \
  -observability=false >"$server_log" 2>&1 &
server_pid=$!

server_address=""
for _ in $(seq 1 100); do
  if ! kill -0 "$server_pid" 2>/dev/null; then
    printf 'HTTP/2 benchmark server exited before listening:\n' >&2
    cat "$server_log" >&2
    exit 1
  fi
  server_address=$(sed -n \
    's/^Mini-RPC echo server listening on \([^ ]*\) via HTTP\/2$/\1/p' \
    "$server_log" | tail -n 1)
  if [[ -n "$server_address" ]]; then
    break
  fi
  sleep 0.1
done
if [[ -z "$server_address" ]]; then
  printf 'HTTP/2 benchmark server did not report a listen address:\n' >&2
  cat "$server_log" >&2
  exit 1
fi

clock_ticks=$(getconf CLK_TCK)
page_size=$(getconf PAGESIZE)
read_cpu_ticks() {
  awk '{print $14 + $15}' "/proc/$server_pid/stat"
}
read_rss_bytes() {
  awk -v page_size="$page_size" '{print $2 * page_size}' "/proc/$server_pid/statm"
}

cpu_before=$(read_cpu_ticks)
wall_started=$(date +%s%N)
"$work_dir/benchmark" \
  -target="$server_address" \
  -transports=h2c \
  -duration="$duration" \
  -warmup="$warmup" \
  -concurrency="$concurrency" \
  -handshakes="$handshakes" | tee "$benchmark_output"
wall_finished=$(date +%s%N)
cpu_after=$(read_cpu_ticks)
server_rss=$(read_rss_bytes)

if [[ $(grep -c '0\.000%' "$benchmark_output") -lt 2 ]]; then
  printf 'HTTP/2 benchmark reported workload failures.\n' >&2
  exit 1
fi
if ! grep -q 'Topology: external-http2' "$benchmark_output"; then
  printf 'HTTP/2 benchmark did not record the isolated topology.\n' >&2
  exit 1
fi

awk \
  -v ticks="$((cpu_after - cpu_before))" \
  -v hz="$clock_ticks" \
  -v elapsed_ns="$((wall_finished - wall_started))" \
  -v rss_bytes="$server_rss" \
  'BEGIN {
    seconds = ticks / hz;
    elapsed = elapsed_ns / 1000000000;
    printf "\nIsolated server process: cpu_seconds=%.3f cpu_cores=%.3f rss_bytes=%d\n",
      seconds, seconds / elapsed, rss_bytes;
  }'
