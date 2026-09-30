#!/usr/bin/env bash
set -Eeuo pipefail
entrypoint="$(cd "$(dirname "$0")/../../.." && pwd)/docker-entrypoint.sh"
fixture="$(mktemp -d)"
trap 'rm -rf -- "$fixture"' EXIT
export AUTH_JWT_PRIVATE_KEY_PATH="$fixture/keys/private.pem"
export AUTH_JWT_PUBLIC_KEY_PATH="$fixture/keys/public.pem"
sh "$entrypoint" ./init-keys
before="$(openssl pkey -in "$AUTH_JWT_PRIVATE_KEY_PATH" -pubout | openssl dgst -sha256)"
sh "$entrypoint" ./init-keys
after="$(openssl pkey -in "$AUTH_JWT_PRIVATE_KEY_PATH" -pubout | openssl dgst -sha256)"
[[ "$before" == "$after" ]]
rm "$AUTH_JWT_PUBLIC_KEY_PATH"
sh "$entrypoint" ./init-keys
[[ "$before" == "$(openssl pkey -pubin -in "$AUTH_JWT_PUBLIC_KEY_PATH" -pubout | openssl dgst -sha256)" ]]
rm "$AUTH_JWT_PRIVATE_KEY_PATH"
if sh "$entrypoint" ./init-keys 2>/dev/null; then echo 'missing private key must fail' >&2; exit 1; fi
[[ ! -e "$AUTH_JWT_PRIVATE_KEY_PATH" ]]
echo 'PASS: first initialization, idempotence, public-key recovery and private-key loss rejection'
