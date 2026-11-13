#!/usr/bin/env sh
set -eu

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
compose_file="$repo_root/deploy/docker-compose.yml"
tls_compose_file="$repo_root/deploy/docker-compose.tls.yml"
project_name=${MINIRPC_TLS_DOCKER_PROJECT:-"mini-rpc-tls-smoke-$$"}
tls_dir=$(mktemp -d)
export MINIRPC_TLS_DIR="$tls_dir"
export MINIRPC_ETCD_PORT=0
export MINIRPC_ECHO_A_METRICS_PORT=0
export MINIRPC_ECHO_B_METRICS_PORT=0
export MINIRPC_PROMETHEUS_PORT=0

cleanup() {
  docker compose --project-name "$project_name" \
    --file "$compose_file" --file "$tls_compose_file" \
    down --volumes --remove-orphans --rmi local >/dev/null 2>&1 || true
  rm -rf "$tls_dir"
}
trap cleanup EXIT INT TERM

"$repo_root/scripts/generate-test-certs.sh" "$tls_dir"
docker compose --project-name "$project_name" \
  --file "$compose_file" --file "$tls_compose_file" \
  up --detach --build

compose_run() {
  docker compose --project-name "$project_name" \
    --file "$compose_file" --file "$tls_compose_file" \
    run --rm echo-client "$@"
}

response=$(compose_run \
  -etcd-endpoints=etcd:2379 \
  -message="docker mutual TLS smoke test" \
  -observability=false \
  -retry-attempts=1 \
  -tls-ca=/tls/ca.crt \
  -tls-cert=/tls/client.crt \
  -tls-key=/tls/client.key)
case "$response" in
  *"docker mutual TLS smoke test"*) ;;
  *)
    printf 'unexpected mutual TLS Echo response: %s\n' "$response" >&2
    exit 1
    ;;
esac

if compose_run \
  -addr=echo-a:8080 \
  -message="plaintext must fail" \
  -observability=false \
  -retry-attempts=1 \
  -timeout=2s >/dev/null 2>&1; then
  printf 'plaintext client unexpectedly succeeded against an mTLS server\n' >&2
  exit 1
fi

if ! docker compose --project-name "$project_name" \
  --file "$compose_file" --file "$tls_compose_file" \
  exec --no-TTY echo-a wget -q -O - http://127.0.0.1:9090/metrics |
  grep -q 'rpc_tls_handshake_failures_total{role="server"} 1'; then
  printf 'mTLS server did not record the rejected plaintext handshake\n' >&2
  exit 1
fi

metrics_found=false
for service in echo-a echo-b; do
  if docker compose --project-name "$project_name" \
    --file "$compose_file" --file "$tls_compose_file" \
    exec --no-TTY "$service" wget -q -O - http://127.0.0.1:9090/metrics |
    grep -q 'rpc_requests_total'; then
    metrics_found=true
    break
  fi
done
if [ "$metrics_found" != true ]; then
  printf 'TLS Echo metrics endpoint did not expose rpc_requests_total\n' >&2
  exit 1
fi

printf 'Docker mTLS smoke test passed: discovered RPC required client certificates.\n'
