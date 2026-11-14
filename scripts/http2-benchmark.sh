#!/usr/bin/env bash
set -euo pipefail

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
work_dir=$(mktemp -d)
cert_dir="$work_dir/tls"
server_pid=""

duration=${MINIRPC_HTTP2_BENCH_DURATION:-3s}
warmup=${MINIRPC_HTTP2_BENCH_WARMUP:-500ms}
concurrency=${MINIRPC_HTTP2_BENCH_CONCURRENCY:-64}
handshakes=${MINIRPC_HTTP2_BENCH_HANDSHAKES:-16}
transports=${MINIRPC_HTTP2_BENCH_TRANSPORTS:-h2c,h2-tls,h2-mtls}

stop_server() {
  if [[ -n "$server_pid" ]] && kill -0 "$server_pid" 2>/dev/null; then
    kill -TERM "$server_pid" 2>/dev/null || true
    wait "$server_pid" 2>/dev/null || true
  fi
  server_pid=""
}

cleanup() {
  stop_server
  rm -rf "$work_dir"
}
trap cleanup EXIT INT TERM

cd "$repo_root"
go build -trimpath -o "$work_dir/echo-server" ./cmd/echo-server
go build -trimpath -o "$work_dir/benchmark" ./benchmark

needs_tls=false
IFS=',' read -r -a transport_list <<<"$transports"
for transport in "${transport_list[@]}"; do
  case "$transport" in
    h2c) ;;
    h2-tls|h2-mtls) needs_tls=true ;;
    *)
      printf 'unsupported HTTP/2 benchmark transport: %s\n' "$transport" >&2
      exit 2
      ;;
  esac
done
if [[ "$needs_tls" == true ]]; then
  ./scripts/generate-test-certs.sh "$cert_dir" >/dev/null 2>&1
fi

clock_ticks=$(getconf CLK_TCK)
page_size=$(getconf PAGESIZE)
read_cpu_ticks() {
  awk '{print $14 + $15}' "/proc/$server_pid/stat"
}
read_rss_bytes() {
  awk -v page_size="$page_size" '{print $2 * page_size}' "/proc/$server_pid/statm"
}

start_server() {
  local transport=$1
  local server_log="$work_dir/echo-server-$transport.log"
  local -a server_options=(
    -addr=127.0.0.1:0
    -http2
    -metrics-addr=
    -observability=false
  )
  if [[ "$transport" == h2-tls || "$transport" == h2-mtls ]]; then
    server_options+=(
      -tls-cert="$cert_dir/server.crt"
      -tls-key="$cert_dir/server.key"
    )
  fi
  if [[ "$transport" == h2-mtls ]]; then
    server_options+=(-tls-client-ca="$cert_dir/ca.crt")
  fi
  "$work_dir/echo-server" "${server_options[@]}" >"$server_log" 2>&1 &
  server_pid=$!
  for _ in $(seq 1 100); do
    if ! kill -0 "$server_pid" 2>/dev/null; then
      printf 'HTTP/2 benchmark server for %s exited before listening:\n' "$transport" >&2
      cat "$server_log" >&2
      exit 1
    fi
    server_address=$(sed -n \
      's/^Mini-RPC echo server listening on \([^ ]*\) via HTTP\/2$/\1/p' \
      "$server_log" | tail -n 1)
    if [[ -n "$server_address" ]]; then
      return
    fi
    sleep 0.1
  done
  printf 'HTTP/2 benchmark server for %s did not report a listen address:\n' "$transport" >&2
  cat "$server_log" >&2
  exit 1
}

run_transport() {
  local transport=$1
  local benchmark_output="$work_dir/benchmark-$transport.txt"
  local -a client_options=(
    -target="$server_address"
    -transports="$transport"
    -duration="$duration"
    -warmup="$warmup"
    -concurrency="$concurrency"
    -handshakes="$handshakes"
  )
  if [[ "$transport" == h2-tls || "$transport" == h2-mtls ]]; then
    client_options+=(
      -target-ca="$cert_dir/ca.crt"
      -target-server-name=localhost
    )
  fi
  if [[ "$transport" == h2-mtls ]]; then
    client_options+=(
      -target-cert="$cert_dir/client.crt"
      -target-key="$cert_dir/client.key"
    )
  fi

  local cpu_before cpu_after wall_started wall_finished server_rss
  cpu_before=$(read_cpu_ticks)
  wall_started=$(date +%s%N)
  printf '\n=== %s ===\n' "$transport"
  "$work_dir/benchmark" "${client_options[@]}" | tee "$benchmark_output"
  wall_finished=$(date +%s%N)
  cpu_after=$(read_cpu_ticks)
  server_rss=$(read_rss_bytes)

  if [[ $(grep -c '0\.000%' "$benchmark_output") -lt 2 ]]; then
    printf '%s benchmark reported workload failures.\n' "$transport" >&2
    exit 1
  fi
  if ! grep -q 'Topology: external-http2' "$benchmark_output"; then
    printf '%s benchmark did not record the isolated topology.\n' "$transport" >&2
    exit 1
  fi

  awk \
    -v transport="$transport" \
    -v ticks="$((cpu_after - cpu_before))" \
    -v hz="$clock_ticks" \
    -v elapsed_ns="$((wall_finished - wall_started))" \
    -v rss_bytes="$server_rss" \
    'BEGIN {
      seconds = ticks / hz;
      elapsed = elapsed_ns / 1000000000;
      printf "Isolated server process [%s]: cpu_seconds=%.3f cpu_cores=%.3f rss_bytes=%d\n",
        transport, seconds, seconds / elapsed, rss_bytes;
    }'
  stop_server
}

for transport in "${transport_list[@]}"; do
  start_server "$transport"
  run_transport "$transport"
done
