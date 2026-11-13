#!/usr/bin/env sh
set -eu

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
compose_file="$repo_root/deploy/docker-compose.yml"
tls_compose_file="$repo_root/deploy/docker-compose.tls.yml"
project_name=${MINIRPC_TLS_ROTATION_DOCKER_PROJECT:-"mini-rpc-tls-rotation-$$"}
tls_dir=$(mktemp -d)
initial_tls_dir=$(mktemp -d)
rotated_tls_dir=$(mktemp -d)
retired_identity_dir=$(mktemp -d)
export MINIRPC_TLS_DIR="$tls_dir"
export MINIRPC_ETCD_PORT=0
export MINIRPC_ECHO_A_METRICS_PORT=0
export MINIRPC_ECHO_B_METRICS_PORT=0
export MINIRPC_PROMETHEUS_PORT=0

cleanup() {
  docker compose --project-name "$project_name" \
    --file "$compose_file" --file "$tls_compose_file" \
    down --volumes --remove-orphans --rmi local >/dev/null 2>&1 || true
  rm -rf "$tls_dir" "$initial_tls_dir" "$rotated_tls_dir" "$retired_identity_dir"
}
trap cleanup EXIT INT TERM

"$repo_root/scripts/generate-test-certs.sh" "$initial_tls_dir" >/dev/null
"$repo_root/scripts/generate-test-certs.sh" "$rotated_tls_dir" >/dev/null
cp "$initial_tls_dir"/ca.crt "$initial_tls_dir"/server.crt "$initial_tls_dir"/server.key \
  "$initial_tls_dir"/client.crt "$initial_tls_dir"/client.key "$tls_dir"/

docker compose --project-name "$project_name" \
  --file "$compose_file" --file "$tls_compose_file" \
  up --detach --build

compose_run() {
  docker compose --project-name "$project_name" \
    --file "$compose_file" --file "$tls_compose_file" \
    run --rm echo-client "$@"
}

active_client_args() {
  compose_run \
    -etcd-endpoints=etcd:2379 \
    -message="$1" \
    -observability=false \
    -retry-attempts=1 \
    -timeout=2s \
    -tls-ca=/tls/ca.crt \
    -tls-cert=/tls/client.crt \
    -tls-key=/tls/client.key
}

response=$(active_client_args "rotation smoke before")
case "$response" in
  *"rotation smoke before"*) ;;
  *)
    printf 'unexpected initial mutual TLS response: %s\n' "$response" >&2
    exit 1
    ;;
esac

cp "$rotated_tls_dir"/ca.crt "$rotated_tls_dir"/server.crt "$rotated_tls_dir"/server.key \
  "$rotated_tls_dir"/client.crt "$rotated_tls_dir"/client.key "$tls_dir"/

rotated=false
attempt=0
while [ "$attempt" -lt 80 ]; do
  if response=$(active_client_args "rotation smoke after" 2>/dev/null); then
    rotated=true
    break
  fi
  attempt=$((attempt + 1))
  sleep 0.25
done
if [ "$rotated" != true ]; then
  printf 'server and client did not accept the rotated TLS material\n' >&2
  exit 1
fi
case "$response" in
  *"rotation smoke after"*) ;;
  *)
    printf 'unexpected rotated mutual TLS response: %s\n' "$response" >&2
    exit 1
    ;;
esac

cp "$rotated_tls_dir"/ca.crt "$retired_identity_dir"/ca.crt
cp "$initial_tls_dir"/client.crt "$initial_tls_dir"/client.key "$retired_identity_dir"/
if docker compose --project-name "$project_name" \
  --file "$compose_file" --file "$tls_compose_file" \
  run --rm --volume "$retired_identity_dir:/retired:ro" echo-client \
  -etcd-endpoints=etcd:2379 \
  -message="retired identity must fail" \
  -observability=false \
  -retry-attempts=1 \
  -timeout=2s \
  -tls-ca=/retired/ca.crt \
  -tls-cert=/retired/client.crt \
  -tls-key=/retired/client.key >/dev/null 2>&1; then
  printf 'rotated server accepted a client certificate from the retired CA\n' >&2
  exit 1
fi

printf 'invalid certificate material' >"$tls_dir/server.crt"
sleep 1
if ! response=$(active_client_args "last known good remains active"); then
  printf 'invalid certificate update displaced the last known-good server configuration\n' >&2
  exit 1
fi
case "$response" in
  *"last known good remains active"*) ;;
  *)
    printf 'unexpected last-known-good response: %s\n' "$response" >&2
    exit 1
    ;;
esac

metrics=$(docker compose --project-name "$project_name" \
  --file "$compose_file" --file "$tls_compose_file" \
  exec --no-TTY echo-a wget -q -O - http://127.0.0.1:9090/metrics)
case "$metrics" in
  *'rpc_tls_reload_total{role="server",outcome="success"}'*) ;;
  *)
    printf 'server did not expose successful TLS reload metrics:\n%s\n' "$metrics" >&2
    exit 1
    ;;
esac
case "$metrics" in
  *'rpc_tls_reload_failures_total{role="server"}'*) ;;
  *)
    printf 'server did not expose failed TLS reload metrics:\n%s\n' "$metrics" >&2
    exit 1
    ;;
esac
case "$metrics" in
  *'rpc_tls_config_version{role="server"}'*) ;;
  *)
    printf 'server did not expose the active TLS configuration version:\n%s\n' "$metrics" >&2
    exit 1
    ;;
esac

printf 'Docker TLS rotation smoke test passed: new handshakes rotated, retired trust was rejected, and failed updates preserved the last known-good configuration.\n'
