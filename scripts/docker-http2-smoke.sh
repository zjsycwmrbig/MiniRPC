#!/usr/bin/env sh
set -eu

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
compose_file="$repo_root/deploy/docker-compose.yml"
h2c_compose_file="$repo_root/deploy/docker-compose.h2c.yml"
project_name=${MINIRPC_HTTP2_DOCKER_PROJECT:-"mini-rpc-http2-smoke-$$"}
export MINIRPC_ETCD_PORT=0
export MINIRPC_ECHO_A_METRICS_PORT=0
export MINIRPC_ECHO_B_METRICS_PORT=0
export MINIRPC_PROMETHEUS_PORT=0

compose() {
  docker compose --project-name "$project_name" \
    --file "$compose_file" --file "$h2c_compose_file" "$@"
}

cleanup() {
  compose down --volumes --remove-orphans --rmi local >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

compose up --detach --build
response=$(compose run --rm echo-client \
  -http2 \
  -etcd-endpoints=etcd:2379 \
  -message="docker HTTP/2 smoke test" \
  -observability=false)
case "$response" in
  *"docker HTTP/2 smoke test"*) ;;
  *)
    printf 'unexpected HTTP/2 Echo response: %s\n' "$response" >&2
    exit 1
    ;;
esac

server_logs=$(compose logs --no-color echo-a echo-b 2>&1 || true)
case "$server_logs" in
  *"via HTTP/2"*) ;;
  *)
    printf 'Echo servers did not report HTTP/2 mode:\n%s\n' "$server_logs" >&2
    exit 1
    ;;
esac

metrics_found=false
for service in echo-a echo-b; do
  metrics=$(compose exec --no-TTY "$service" \
    wget -q -O - http://127.0.0.1:9090/metrics)
  case "$metrics" in
    *'rpc_requests_total{'*)
      metrics_found=true
      break
      ;;
  esac
done
if [ "$metrics_found" != true ]; then
  printf 'HTTP/2 Echo metrics did not expose completed RPC requests\n' >&2
  exit 1
fi

printf 'Docker HTTP/2 smoke test passed: h2c discovery, generated RPC, and metrics are healthy.\n'
