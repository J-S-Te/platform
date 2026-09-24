#!/bin/sh
set -eu

repo_root="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
entrypoint="$repo_root/docker-entrypoint.sh"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/docker-entrypoint-key-init.XXXXXX")"
trap 'rm -rf "$test_root"' EXIT HUP INT TERM

mkdir -p "$test_root/bin" "$test_root/work"

cat > "$test_root/bin/openssl" <<'EOF'
#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$ENTRYPOINT_OPENSSL_LOG"
output=""
while [ "$#" -gt 0 ]; do
    if [ "$1" = "-out" ]; then
        shift
        output="$1"
    fi
    shift
done
[ -n "$output" ] && : > "$output"
EOF

cat > "$test_root/bin/id" <<'EOF'
#!/bin/sh
if [ "${1:-}" = "-u" ]; then
    printf '10001\n'
    exit 0
fi
exit 1
EOF

cat > "$test_root/work/api" <<'EOF'
#!/bin/sh
printf 'api invoked\n'
EOF

cat > "$test_root/work/file-gateway" <<'EOF'
#!/bin/sh
printf 'file-gateway invoked\n'
EOF

chmod +x "$test_root/bin/openssl" "$test_root/bin/id" "$test_root/work/api" "$test_root/work/file-gateway"

openssl_log="$test_root/openssl.log"
private_key="$test_root/data/jwt-ed25519-private.pem"
public_key="$test_root/data/jwt-ed25519-public.pem"

gateway_output="$(cd "$test_root/work" && PATH="$test_root/bin:$PATH" ENTRYPOINT_OPENSSL_LOG="$openssl_log" \
    AUTH_JWT_PRIVATE_KEY_PATH="$private_key" AUTH_JWT_PUBLIC_KEY_PATH="$public_key" \
    "$entrypoint" ./file-gateway)"
[ "$gateway_output" = "file-gateway invoked" ]
[ ! -e "$openssl_log" ]
[ ! -e "$private_key" ]
[ ! -e "$public_key" ]

api_output="$(cd "$test_root/work" && PATH="$test_root/bin:$PATH" ENTRYPOINT_OPENSSL_LOG="$openssl_log" \
    AUTH_JWT_PRIVATE_KEY_PATH="$private_key" AUTH_JWT_PUBLIC_KEY_PATH="$public_key" \
    "$entrypoint" ./api)"
[ "$api_output" = "api invoked" ]
[ -f "$private_key" ]
[ -f "$public_key" ]
grep -q '^genpkey -algorithm ED25519 -out ' "$openssl_log"
grep -q '^pkey -in .* -pubout -out ' "$openssl_log"

printf 'docker entrypoint key initialization tests passed\n'
