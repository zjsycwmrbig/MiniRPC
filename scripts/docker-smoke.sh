#!/usr/bin/env sh
set -eu

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
compose_file="$repo_root/deploy/docker-compose.yml"
project_name=${MINIRPC_DOCKER_PROJECT:-"mini-rpc-smoke-$$"}
export MINIRPC_ETCD_PORT=0
export MINIRPC_ECHO_A_METRICS_PORT=0
export MINIRPC_ECHO_B_METRICS_PORT=0
export MINIRPC_PROMETHEUS_PORT=0

cleanup() {
  docker compose --project-name "$project_name" --file "$compose_file" \
    down --volumes --remove-orphans --rmi local >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

docker compose --project-name "$project_name" --file "$compose_file" up --detach --build
response=$(docker compose --project-name "$project_name" --file "$compose_file" \
  run --rm echo-client \
  -etcd-endpoints=etcd:2379 \
  -message="docker smoke test" \
  -observability=false)

case "$response" in
  *"docker smoke test"*) ;;
  *)
    printf 'unexpected Echo response: %s\n' "$response" >&2
    exit 1
    ;;
esac

metrics_found=false
for service in echo-a echo-b; do
  if docker compose --project-name "$project_name" --file "$compose_file" \
    exec --no-TTY "$service" wget -q -O - http://127.0.0.1:9090/metrics |
    grep -q 'rpc_requests_total'; then
    metrics_found=true
    break
  fi
done
if [ "$metrics_found" != true ]; then
  printf 'Echo metrics endpoint did not expose rpc_requests_total\n' >&2
  exit 1
fi

printf 'Docker smoke test passed: discovery RPC and Prometheus metrics are healthy.\n'
