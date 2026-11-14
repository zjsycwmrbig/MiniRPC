#!/usr/bin/env sh
set -eu

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
compose_file="$repo_root/deploy/docker-compose.yml"
otel_compose_file="$repo_root/deploy/docker-compose.otel-tls.yml"
project_name=${MINIRPC_OTEL_MTLS_ROTATION_DOCKER_PROJECT:-"mini-rpc-otel-mtls-rotation-$$"}
tls_dir=$(mktemp -d)
next_tls_dir=$(mktemp -d)
export MINIRPC_OTLP_TLS_DIR="$tls_dir"
export MINIRPC_ETCD_PORT=0
export MINIRPC_ECHO_A_METRICS_PORT=0
export MINIRPC_ECHO_B_METRICS_PORT=0
export MINIRPC_PROMETHEUS_PORT=0
export MINIRPC_OTLP_HTTP_PORT=0

compose() {
  docker compose --project-name "$project_name" \
    --file "$compose_file" --file "$otel_compose_file" "$@"
}

cleanup() {
  compose down --volumes --remove-orphans --rmi local >/dev/null 2>&1 || true
  rm -rf "$tls_dir" "$next_tls_dir"
}
trap cleanup EXIT INT TERM

paired_trace_ids() {
  compose logs --no-color otel-collector 2>&1 | awk '
    /service.name: Str\(mini-rpc-echo-client\)/ { service = "client" }
    /service.name: Str\(mini-rpc-echo-server\)/ { service = "server" }
    /Trace ID[[:space:]]*:/ {
      line = $0
      sub(/^.*Trace ID[[:space:]]*:[[:space:]]*/, "", line)
      id = substr(line, 1, 32)
      if (service != "" && length(id) == 32 && id !~ /[^0-9a-fA-F]/) {
        seen[id] = seen[id] service
      }
    }
    END {
      for (id in seen) {
        if (seen[id] ~ /client/ && seen[id] ~ /server/) print id
      }
    }
  ' | sort -u
}

wait_for_correlated_trace() {
  minimum=$1
  attempt=0
  while [ "$attempt" -lt 60 ]; do
    paired=$(paired_trace_ids)
    count=$(printf '%s\n' "$paired" | sed '/^$/d' | wc -l | tr -d ' ')
    if [ "$count" -ge "$minimum" ]; then
      return 0
    fi
    attempt=$((attempt + 1))
    sleep 0.25
  done
  return 1
}

wait_for_otlp_reloads() {
  attempt=0
  while [ "$attempt" -lt 60 ]; do
    echo_a_logs=$(compose logs --no-color echo-a 2>&1 || true)
    echo_b_logs=$(compose logs --no-color echo-b 2>&1 || true)
    case "$echo_a_logs" in
      *'OTLP TLS material reloaded'*)
        case "$echo_b_logs" in
          *'OTLP TLS material reloaded'*) return 0 ;;
        esac
        ;;
    esac
    attempt=$((attempt + 1))
    sleep 0.25
  done
  return 1
}

wait_for_collector_restart() {
  previous_ready=$1
  attempt=0
  while [ "$attempt" -lt 60 ]; do
    collector_logs=$(compose logs --no-color otel-collector 2>&1 || true)
    ready_count=$(printf '%s\n' "$collector_logs" |
      grep -c 'Everything is ready' || true)
    if [ "$ready_count" -gt "$previous_ready" ]; then
      return 0
    fi
    attempt=$((attempt + 1))
    sleep 0.25
  done
  return 1
}

"$repo_root/scripts/generate-test-certs.sh" "$tls_dir"
chmod 755 "$tls_dir"
chmod 644 "$tls_dir/server.key"
compose up --detach --build

response=$(compose run --rm echo-client)
case "$response" in
  *"docker-otlp-mtls-test"*) ;;
  *)
    printf 'unexpected initial OTLP mTLS Echo response: %s\n' "$response" >&2
    exit 1
    ;;
esac
if ! wait_for_correlated_trace 1; then
  printf 'initial OTLP mTLS environment did not export a correlated trace\n' >&2
  exit 1
fi
initial_pairs=$(printf '%s\n' "$(paired_trace_ids)" | sed '/^$/d' | wc -l | tr -d ' ')

"$repo_root/scripts/generate-test-certs.sh" "$next_tls_dir"
mkdir -p "$tls_dir/retired-a"
cp "$tls_dir/ca.crt" "$tls_dir/client.crt" "$tls_dir/client.key" "$tls_dir/retired-a/"
cp "$next_tls_dir/ca.crt" "$tls_dir/ca.crt"
cp "$next_tls_dir/server.crt" "$tls_dir/server.crt"
cp "$next_tls_dir/server.key" "$tls_dir/server.key"
cp "$next_tls_dir/client.crt" "$tls_dir/client.crt"
cp "$next_tls_dir/client.key" "$tls_dir/client.key"
chmod 644 "$tls_dir/ca.crt" "$tls_dir/server.crt" "$tls_dir/server.key" "$tls_dir/client.crt"
chmod 600 "$tls_dir/client.key" "$tls_dir/retired-a/client.key"

if ! wait_for_otlp_reloads; then
  printf 'live Echo servers did not publish rotated OTLP TLS material\n' >&2
  exit 1
fi
# Allow a second poll to publish a complete multi-file snapshot if a copy was
# observed mid-update, then start the Collector with the same final material.
sleep 0.75

collector_logs=$(compose logs --no-color otel-collector 2>&1 || true)
previous_ready=$(printf '%s\n' "$collector_logs" | grep -c 'Everything is ready' || true)
compose restart otel-collector >/dev/null
if ! wait_for_collector_restart "$previous_ready"; then
  printf 'OTLP Collector did not restart with rotated TLS material\n' >&2
  exit 1
fi

rotation_response=$(compose run --rm echo-client \
  -etcd-endpoints=etcd:2379 \
  -message=docker-otlp-mtls-rotation-test \
  -retry-attempts=1 \
  -otlp-endpoint=https://otel-collector:4318/v1/traces \
  -otlp-ca=/tls/ca.crt \
  -otlp-cert=/tls/client.crt \
  -otlp-key=/tls/client.key \
  -otlp-tls-server-name=otel-collector \
  -otlp-tls-min-version=1.2)
case "$rotation_response" in
  *"docker-otlp-mtls-rotation-test"*) ;;
  *)
    printf 'unexpected post-rotation OTLP mTLS Echo response: %s\n' "$rotation_response" >&2
    exit 1
    ;;
esac
if ! wait_for_correlated_trace "$((initial_pairs + 1))"; then
  printf 'post-rotation client and server spans were not exported under a new shared Trace ID\n' >&2
  exit 1
fi

retired_output=$(compose run --rm echo-client \
  -etcd-endpoints=etcd:2379 \
  -message=docker-otlp-mtls-retired-client-test \
  -retry-attempts=1 \
  -otlp-endpoint=https://otel-collector:4318/v1/traces \
  -otlp-ca=/tls/ca.crt \
  -otlp-cert=/tls/retired-a/client.crt \
  -otlp-key=/tls/retired-a/client.key \
  -otlp-tls-server-name=otel-collector \
  -otlp-export-timeout=500ms 2>&1 || true)
case "$retired_output" in
  *certificate*docker-otlp-mtls-retired-client-test*) ;;
  *docker-otlp-mtls-retired-client-test*certificate*) ;;
  *)
    printf 'OTLP Collector accepted the retired client identity:\n%s\n' "$retired_output" >&2
    exit 1
    ;;
esac

printf 'Docker OTLP mTLS rotation smoke test passed: live exporters adopted new roots and identity, post-rotation traces correlated, and retired credentials were rejected.\n'
