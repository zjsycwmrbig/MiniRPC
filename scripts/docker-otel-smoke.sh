#!/usr/bin/env sh
set -eu

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
compose_file="$repo_root/deploy/docker-compose.yml"
otel_compose_file="$repo_root/deploy/docker-compose.otel.yml"
project_name=${MINIRPC_OTEL_DOCKER_PROJECT:-"mini-rpc-otel-smoke-$$"}
export MINIRPC_ETCD_PORT=0
export MINIRPC_ECHO_A_METRICS_PORT=0
export MINIRPC_ECHO_B_METRICS_PORT=0
export MINIRPC_PROMETHEUS_PORT=0
export MINIRPC_OTLP_HTTP_PORT=0

cleanup() {
  docker compose --project-name "$project_name" \
    --file "$compose_file" --file "$otel_compose_file" \
    down --volumes --remove-orphans --rmi local >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

docker compose --project-name "$project_name" \
  --file "$compose_file" --file "$otel_compose_file" \
  up --detach --build

response=$(docker compose --project-name "$project_name" \
  --file "$compose_file" --file "$otel_compose_file" \
  run --rm echo-client)
case "$response" in
  *"docker-otlp-trace-test"*) ;;
  *)
    printf 'unexpected OTLP Echo response: %s\n' "$response" >&2
    exit 1
    ;;
esac

traces_exported=false
attempt=0
while [ "$attempt" -lt 40 ]; do
  collector_logs=$(docker compose --project-name "$project_name" \
    --file "$compose_file" --file "$otel_compose_file" \
    logs --no-color otel-collector 2>&1 || true)
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
  printf 'OTLP collector did not receive correlated client and server spans:\n%s\n' "$collector_logs" >&2
  exit 1
fi

case "$collector_logs" in
  *'service.name: Str(mini-rpc-echo-client)'*) ;;
  *)
    printf 'OTLP logs are missing the client service resource\n' >&2
    exit 1
    ;;
  esac
case "$collector_logs" in
  *'service.name: Str(mini-rpc-echo-server)'*) ;;
  *)
    printf 'OTLP logs are missing the server service resource\n' >&2
    exit 1
    ;;
esac

trace_id_count=$(printf '%s\n' "$collector_logs" |
  sed -n 's/.*Trace ID[[:space:]]*:[[:space:]]*\([0-9a-fA-F]\{32\}\).*/\1/p' |
  sort -u |
  wc -l |
  tr -d ' ')
if [ "$trace_id_count" -ne 1 ]; then
  printf 'OTLP client and server spans do not share one trace ID (found %s unique IDs)\n' \
    "$trace_id_count" >&2
  exit 1
fi

printf 'Docker OTLP trace smoke test passed: client and server spans reached the collector with shared trace correlation.\n'
