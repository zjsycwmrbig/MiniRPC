#!/usr/bin/env sh
set -eu

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
compose_file="$repo_root/deploy/docker-compose.yml"
otel_compose_file="$repo_root/deploy/docker-compose.otel-tls.yml"
project_name=${MINIRPC_OTEL_MTLS_DOCKER_PROJECT:-"mini-rpc-otel-mtls-smoke-$$"}
tls_dir=$(mktemp -d)
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
  rm -rf "$tls_dir"
}
trap cleanup EXIT INT TERM

"$repo_root/scripts/generate-test-certs.sh" "$tls_dir"
# The official Collector image runs as UID 10001. These are disposable test
# credentials in a private temporary directory, so make only the server key
# readable by that non-root process while client material remains host-private.
chmod 755 "$tls_dir"
chmod 644 "$tls_dir/server.key"
compose up --detach --build

response=$(compose run --rm echo-client)
case "$response" in
  *"docker-otlp-mtls-test"*) ;;
  *)
    printf 'unexpected OTLP mTLS Echo response: %s\n' "$response" >&2
    exit 1
    ;;
esac

traces_exported=false
attempt=0
while [ "$attempt" -lt 40 ]; do
  collector_logs=$(compose logs --no-color otel-collector 2>&1 || true)
  case "$collector_logs" in
    *'rpc.client/rpc.echo.v1.Echo/Echo'*)
      case "$collector_logs" in
        *'rpc.server/rpc.echo.v1.Echo/Echo'*)
          traces_exported=true
          break
          ;;
      esac
      ;;
  esac
  attempt=$((attempt + 1))
  sleep 0.5
done

if [ "$traces_exported" != true ]; then
  printf 'mTLS OTLP collector did not receive client and server spans:\n%s\n' "$collector_logs" >&2
  exit 1
fi
case "$collector_logs" in
  *'service.name: Str(mini-rpc-echo-client)'*) ;;
  *)
    printf 'mTLS OTLP logs are missing the client service resource\n' >&2
    exit 1
    ;;
esac
case "$collector_logs" in
  *'service.name: Str(mini-rpc-echo-server)'*) ;;
  *)
    printf 'mTLS OTLP logs are missing the server service resource\n' >&2
    exit 1
    ;;
esac

trace_id_count=$(printf '%s\n' "$collector_logs" |
  sed -n 's/.*Trace ID[[:space:]]*:[[:space:]]*\([0-9a-fA-F]\{32\}\).*/\1/p' |
  sort -u |
  wc -l |
  tr -d ' ')
if [ "$trace_id_count" -ne 1 ]; then
  printf 'mTLS OTLP client and server spans do not share one trace ID (found %s unique IDs)\n' \
    "$trace_id_count" >&2
  exit 1
fi

negative_output=$(compose run --rm echo-client \
  -etcd-endpoints=etcd:2379 \
  -message=docker-otlp-mtls-negative-test \
  -retry-attempts=1 \
  -otlp-endpoint=https://otel-collector:4318/v1/traces \
  -otlp-ca=/tls/ca.crt \
  -otlp-tls-server-name=otel-collector \
  -otlp-export-timeout=500ms 2>&1 || true)

# The exporter may surface either the remote TLS alert or a transport retry
# failure after the Collector closes the handshake. Require the Collector-side
# rejection log so the negative check does not depend on which client error wins.
negative_rejected=false
attempt=0
while [ "$attempt" -lt 40 ]; do
  collector_logs=$(compose logs --no-color otel-collector 2>&1 || true)
  case "$collector_logs" in
    *"tls: client didn't provide a certificate"* | *'tls: certificate required'*)
      negative_rejected=true
      break
      ;;
  esac
  attempt=$((attempt + 1))
  sleep 0.25
done
if [ "$negative_rejected" != true ]; then
  printf 'OTLP collector accepted trace export without a client certificate.\nClient output:\n%s\nCollector logs:\n%s\n' \
    "$negative_output" "$collector_logs" >&2
  exit 1
fi

printf 'Docker OTLP mTLS smoke test passed: custom CA, client identity, collector verification, and trace correlation are enforced.\n'
