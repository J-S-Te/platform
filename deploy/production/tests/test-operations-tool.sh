#!/usr/bin/env bash
set -Eeuo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../../../.." && pwd)"
tool="$repo_root/系统运维工具.sh"
stage="$(mktemp -d)"
trap 'rm -rf -- "$stage"' EXIT

target="$stage/deploy"
fake_bin="$stage/bin"
action_log="$stage/actions.log"
mkdir -p "$target/bin" "$fake_bin"
touch "$target/.env" "$target/.release.env" "$target/docker-compose.yml" "$action_log"
chmod 600 "$target/.env" "$target/.release.env"

cat >"$target/bin/deploy.sh" <<'EOF'
#!/usr/bin/env bash
set -u
printf 'deploy:%s\n' "$*" >>"$ACTION_LOG"
case "${1:-}" in
  doctor) printf '[OK] doctor\n' ;;
  status) printf 'status healthy\n' ;;
  verify) printf 'verify healthy\n' ;;
  logs) printf 'logs for %s\n' "${2:-}" ;;
  backup) printf 'backup created\n' ;;
esac
EOF

cat >"$target/bin/lifecycle.sh" <<'EOF'
#!/usr/bin/env bash
printf 'lifecycle:%s\n' "$*" >>"$ACTION_LOG"
EOF

cat >"$target/bin/backup-all.sh" <<'EOF'
#!/usr/bin/env bash
printf 'backup-tool:%s\n' "$*" >>"$ACTION_LOG"
EOF

cat >"$fake_bin/docker" <<'EOF'
#!/usr/bin/env bash
printf 'docker:%s\n' "$*" >>"$ACTION_LOG"
case "${1:-}" in
  info)
    if [[ "${2:-}" == --format ]]; then printf '%s\n' /tmp; else printf 'Docker info\n'; fi
    ;;
  version) printf 'Docker version test\n' ;;
  compose) printf 'compose output\n' ;;
  ps) printf 'container output\n' ;;
  stats) printf 'stats output\n' ;;
esac
EOF

cat >"$fake_bin/free" <<'EOF'
#!/usr/bin/env bash
printf 'Mem: test\n'
EOF

chmod 755 "$target/bin/deploy.sh" "$target/bin/lifecycle.sh" "$target/bin/backup-all.sh" "$fake_bin/docker" "$fake_bin/free"
export ACTION_LOG="$action_log"
export PATH="$fake_bin:$PATH"

fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
assert_log() { grep -Fxq "$1" "$action_log" || fail "missing action: $1"; }

bash -n "$tool"
bash "$tool" --help >/dev/null

bash "$tool" --target "$target" doctor contract >/dev/null
assert_log 'deploy:doctor contract'

bash "$tool" --target "$target" logs customer-opportunity >/dev/null
assert_log 'deploy:logs customer_and_opportunity'

bash "$tool" --target "$target" logs platform >/dev/null
assert_log 'docker:compose --project-directory '"$target"' --file '"$target"'/docker-compose.yml --env-file '"$target"'/.env --env-file '"$target"'/.release.env logs --tail 200 platform-api platform-worker subsystem-provisioner file-gateway keycloak temporal'

before="$(wc -l <"$action_log")"
if bash "$tool" --target "$target" disable contract WRONG_CONFIRMATION >/dev/null 2>&1; then
  fail 'disable accepted an invalid confirmation token'
fi
after="$(wc -l <"$action_log")"
[[ "$before" == "$after" ]] || fail 'invalid confirmation performed an action'

bash "$tool" --target "$target" disable contract DISABLE_KEEP_DATA >/dev/null
assert_log 'deploy:disable contract'
assert_log 'deploy:status contract'

bash "$tool" --target "$target" repair frontend REPAIR_CURRENT_VERSION >/dev/null
assert_log 'lifecycle:repair frontend'

bash "$tool" --target "$target" backup CREATE_BACKUP >/dev/null
assert_log 'deploy:backup'

backup="$stage/backup"
mkdir -p "$backup"
bash "$tool" --target "$target" verify-backup "$backup" >/dev/null
assert_log "backup-tool:--verify-only --backup $backup"

package="$stage/customer-opportunity-backend-test-linux-amd64.tar.gz"
printf 'test package\n' >"$package"
(cd "$stage" && sha256sum "$(basename -- "$package")" >"$(basename -- "$package").sha256")
(
  cd "$stage"
  bash "$tool" --target "$target" upgrade customer-opportunity "$(basename -- "$package")" STAGE_UPGRADE >/dev/null
)
assert_log "deploy:import $package"
assert_log 'deploy:upgrade customer-opportunity'
assert_log 'deploy:status customer-opportunity'

diagnostics="$stage/diagnostics"
bash "$tool" --target "$target" collect "$diagnostics" >/dev/null 2>&1
for required in README.txt host.txt docker-version.txt compose-version.txt docker-info.txt containers.txt doctor.txt status.txt verify.txt stats.txt; do
  [[ -f "$diagnostics/$required" ]] || fail "diagnostic file missing: $required"
done
if find "$diagnostics" -type f \( -name '.env' -o -name '*.env' \) -print -quit | grep -q .; then
  fail 'diagnostics copied a secret environment file'
fi

printf 'PASS: operations tool dispatch, confirmations, log mapping and diagnostics\n'
