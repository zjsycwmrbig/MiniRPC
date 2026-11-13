#!/usr/bin/env sh
set -eu

if [ "$#" -ne 1 ]; then
  printf 'usage: %s <output-directory>\n' "$0" >&2
  exit 2
fi

output_dir=$1
umask 077
mkdir -p "$output_dir"

openssl ecparam -name prime256v1 -genkey -noout -out "$output_dir/ca.key"
openssl req -x509 -new -key "$output_dir/ca.key" -sha256 -days 1 \
  -subj "/CN=Mini-RPC Local Test CA" -out "$output_dir/ca.crt"

openssl ecparam -name prime256v1 -genkey -noout -out "$output_dir/server.key"
openssl req -new -key "$output_dir/server.key" \
  -subj "/CN=mini-rpc-server" -out "$output_dir/server.csr"
cat >"$output_dir/server.ext" <<'EOF'
basicConstraints=CA:FALSE
keyUsage=digitalSignature
extendedKeyUsage=serverAuth
subjectAltName=DNS:localhost,DNS:echo-a,DNS:echo-b,IP:127.0.0.1
EOF
openssl x509 -req -in "$output_dir/server.csr" -CA "$output_dir/ca.crt" \
  -CAkey "$output_dir/ca.key" -CAcreateserial -days 1 -sha256 \
  -extfile "$output_dir/server.ext" -out "$output_dir/server.crt"

openssl ecparam -name prime256v1 -genkey -noout -out "$output_dir/client.key"
openssl req -new -key "$output_dir/client.key" \
  -subj "/CN=mini-rpc-smoke-client" -out "$output_dir/client.csr"
cat >"$output_dir/client.ext" <<'EOF'
basicConstraints=CA:FALSE
keyUsage=digitalSignature
extendedKeyUsage=clientAuth
EOF
openssl x509 -req -in "$output_dir/client.csr" -CA "$output_dir/ca.crt" \
  -CAkey "$output_dir/ca.key" -CAcreateserial -days 1 -sha256 \
  -extfile "$output_dir/client.ext" -out "$output_dir/client.crt"

openssl verify -CAfile "$output_dir/ca.crt" "$output_dir/server.crt" "$output_dir/client.crt"
chmod 600 "$output_dir"/*.key
chmod 644 "$output_dir"/*.crt

printf 'Generated TLS test material in %s\n' "$output_dir"
