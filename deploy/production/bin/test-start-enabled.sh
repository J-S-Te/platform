#!/usr/bin/env bash
set -Eeuo pipefail
source_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
source "$source_dir/start-enabled.sh"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/uip-start.XXXXXX")"
trap 'rm -r -- "$test_root"' EXIT
deploy_dir="$test_root"; script_dir="$test_root/bin"; release_file="$test_root/.release.env"
mkdir -p "$script_dir" "$test_root/runtime"
cat > "$script_dir/backup-all.sh" <<'EOF'
#!/usr/bin/env bash
printf 'backup\n' >> "$START_LOG"
EOF
chmod +x "$script_dir/backup-all.sh"
export START_LOG="$test_root/calls"
ready=true; fail_migration=false; enabled=true
die() { echo "$*" >&2; exit 1; }
scope_require() { [[ "$enabled" == true ]]; }
scope_project() { printf 'test-project\n'; }
scope_services() { printf '%s\n' contract-api contract-migrate contract-mysql; }
scope_owned_services() { printf '%s\n' contract-api contract-migrate; }
subsystem_metadata() { subsystem_runtime="$test_root/runtime/contract.env"; subsystem_key=CONTRACT_IMAGE; subsystem_service=contract-api; subsystem_candidate=uip-contract-candidate; }
runtime_ready() { [[ "$ready" == true ]]; }
env_get() { printf 'registry.invalid/app@sha256:%064d\n' 1; }
flock() { :; }
docker() {
  case "$*" in
    'image inspect '*) return 0 ;;
    'container inspect '*) return 1 ;;
    'ps -aq '*) printf 'existing-contract\n' ;;
    *) return 1 ;;
  esac
}
compose() {
  printf '%s\n' "$*" >> "$START_LOG"
  [[ "$*" != 'run --no-deps contract-migrate' || "$fail_migration" != true ]]
}
start_registered contract > /dev/null
grep -q '^run --no-deps contract-api ./authz-catalog publish$' "$START_LOG"
[[ "$(tail -1 "$START_LOG")" == 'up -d --no-deps --wait --wait-timeout 180 contract-api' ]]
: > "$START_LOG"
fail_migration=true
if start_registered contract > /dev/null; then echo 'migration failure accepted' >&2; exit 1; fi
[[ "$(tail -1 "$START_LOG")" == 'run --no-deps contract-migrate' ]]
: > "$START_LOG"
ready=false
if start_registered contract > /dev/null; then echo 'unadopted module started' >&2; exit 1; fi
[[ ! -s "$START_LOG" ]]
ready=true; enabled=false
if start_registered contract > /dev/null; then echo 'disabled module started' >&2; exit 1; fi
[[ ! -s "$START_LOG" ]]
echo 'Staged subsystem restart, adoption gate and migration failure tests passed'
