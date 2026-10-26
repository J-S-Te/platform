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
exit 0
EOF

cat > "$test_root/bin/id" <<'EOF'
#!/bin/sh
if [ "${1:-}" = "-u" ]; then
    printf '%s\n' "${ENTRYPOINT_TEST_UID:-10001}"
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

# Credential transport keys are created only by the explicit init job, never by
# config-check or an ordinary API startup. Repeated init preserves the active ring.
cat > "$test_root/work/credential-keygen" <<'KEYGEN'
#!/bin/sh
set -eu
printf 'credential-keygen\n' >> "$ENTRYPOINT_CREDENTIAL_LOG"
[ "$1" = '--dir' ]; dir="$2"; shift 2
[ "$1" = '--name' ]; name="$2"
mkdir -p "$dir"
umask 077
printf '{"fixture":true}\n' > "$dir/$name.json"
KEYGEN
chmod +x "$test_root/work/credential-keygen"
ring="$test_root/data/credential-keyring.json"
credential_log="$test_root/credential.log"
(cd "$test_root/work" && PATH="$test_root/bin:$PATH" ENTRYPOINT_OPENSSL_LOG="$openssl_log" \
    ENTRYPOINT_CREDENTIAL_LOG="$credential_log" CREDENTIAL_ENCRYPTION_MODE=required \
    CREDENTIAL_ENCRYPTION_KEYRING_FILE="$ring" \
    AUTH_JWT_PRIVATE_KEY_PATH="$private_key" AUTH_JWT_PUBLIC_KEY_PATH="$public_key" \
    "$entrypoint" ./init-keys)
[ -f "$ring" ]
cp "$ring" "$ring.previous"
(cd "$test_root/work" && PATH="$test_root/bin:$PATH" ENTRYPOINT_OPENSSL_LOG="$openssl_log" \
    ENTRYPOINT_CREDENTIAL_LOG="$credential_log" CREDENTIAL_ENCRYPTION_MODE=required \
    CREDENTIAL_ENCRYPTION_KEYRING_FILE="$ring" \
    AUTH_JWT_PRIVATE_KEY_PATH="$private_key" AUTH_JWT_PUBLIC_KEY_PATH="$public_key" \
    "$entrypoint" ./init-keys)
cmp "$ring" "$ring.previous"
[ "$(wc -l < "$credential_log" | tr -d ' ')" = 1 ]
rm "$ring"
(cd "$test_root/work" && PATH="$test_root/bin:$PATH" ENTRYPOINT_OPENSSL_LOG="$openssl_log" \
    ENTRYPOINT_CREDENTIAL_LOG="$credential_log" CREDENTIAL_ENCRYPTION_MODE=required \
    CREDENTIAL_ENCRYPTION_KEYRING_FILE="$ring" \
    AUTH_JWT_PRIVATE_KEY_PATH="$private_key" AUTH_JWT_PUBLIC_KEY_PATH="$public_key" \
    "$entrypoint" ./api config-check) >/dev/null
[ ! -e "$ring" ]
printf 'credential key initialization is explicit, idempotent and config-check remains read-only: PASS\n'

cat > "$test_root/bin/stat" <<'STAT'
#!/bin/sh
[ "$1" = '-c' ] && [ "$2" = '%u:%g' ] || exit 1
printf '1000:1000\n'
STAT
cat > "$test_root/bin/su-exec" <<'SU'
#!/bin/sh
printf '%s\n' "$1" >> "$ENTRYPOINT_CREDENTIAL_OWNER_LOG"
shift
exec "$@"
SU
chmod +x "$test_root/bin/stat" "$test_root/bin/su-exec"
(cd "$test_root/work" && PATH="$test_root/bin:$PATH" ENTRYPOINT_OPENSSL_LOG="$openssl_log" \
    ENTRYPOINT_CREDENTIAL_LOG="$credential_log" ENTRYPOINT_CREDENTIAL_OWNER_LOG="$test_root/owner.log" \
    ENTRYPOINT_TEST_UID=0 CREDENTIAL_ENCRYPTION_MODE=required CREDENTIAL_ENCRYPTION_KEYRING_FILE="$ring" \
    AUTH_JWT_PRIVATE_KEY_PATH="$private_key" AUTH_JWT_PUBLIC_KEY_PATH="$public_key" \
    "$entrypoint" ./init-keys)
grep -qx '1000:1000' "$test_root/owner.log"
rm "$ring"
touch "$test_root/data/credential-keyring-existing.private.pem"
if (cd "$test_root/work" && PATH="$test_root/bin:$PATH" ENTRYPOINT_OPENSSL_LOG="$openssl_log" \
    ENTRYPOINT_CREDENTIAL_LOG="$credential_log" CREDENTIAL_ENCRYPTION_MODE=required \
    CREDENTIAL_ENCRYPTION_KEYRING_FILE="$ring" AUTH_JWT_PRIVATE_KEY_PATH="$private_key" \
    AUTH_JWT_PUBLIC_KEY_PATH="$public_key" "$entrypoint" ./init-keys) 2>"$test_root/error.log"; then
    echo 'orphan key should block automatic key regeneration' >&2; exit 1
fi
[ ! -e "$ring" ]
grep -q 'restore the keyring' "$test_root/error.log"
printf 'credential key owner is preserved and missing metadata never rotates orphan keys: PASS\n'
