#!/usr/bin/env bash
# Linux regression for the current blank-server bootstrap path. The retired
# 20260922 all-in-one installer is deliberately not a dependency: a delivery
# must be installable with the standalone install-assets.sh emitted beside the
# deployment-assets archive.
set -Eeuo pipefail

test_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
deploy_dir="$(cd -- "$test_dir/.." && pwd -P)"
installer="$deploy_dir/bin/install-assets.sh"
evidence_collector="$deploy_dir/bin/acceptance-evidence.sh"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/uip-current-preflight.XXXXXX")"
trap 'rm -rf -- "$test_root"' EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }
pass() { echo "PASS: $*"; }
reject() {
  local description="$1"; shift
  if "$@" >"$test_root/rejected.log" 2>&1; then
    cat "$test_root/rejected.log" >&2
    fail "$description was accepted"
  fi
  pass "$description rejected"
}

[[ -f "$installer" && ! -L "$installer" ]] || fail 'current standalone installer missing'
[[ -f "$evidence_collector" && ! -L "$evidence_collector" ]] || fail 'acceptance evidence collector missing'
bash -n "$installer" "$evidence_collector"
pass 'current bootstrap entrypoints have valid shell syntax'

fixture="$test_root/fixture"
mkdir -p "$fixture/bin" "$fixture/subsystems.d" "$fixture/subsystem-templates" \
  "$fixture/mysql-init" "$fixture/tests"
printf 'services: {}\n' >"$fixture/docker-compose.yml"
printf 'example=true\n' >"$fixture/.env.example"
printf 'PLATFORM_IMAGE=pending\n' >"$fixture/.release.env.example"
printf '# offline runbook\n' >"$fixture/OFFLINE_RUNBOOK.md"
printf '# acceptance checklist\n' >"$fixture/ACCEPTANCE_CHECKLIST.md"
printf '#!/usr/bin/env bash\nset -Eeuo pipefail\nprintf "deploy fixture\\n"\n' >"$fixture/bin/deploy.sh"
printf '#!/usr/bin/env bash\nset -Eeuo pipefail\n' >"$fixture/bin/acceptance-evidence.sh"
printf 'SELECT 1;\n' >"$fixture/mysql-init/bootstrap.sql"
chmod 750 "$fixture/bin/"*.sh

archive="$test_root/deployment-assets-current-preflight.tar.gz"
tar -czf "$archive" -C "$fixture" .
archive_digest="$(sha256sum "$archive" | awk '{print tolower($1)}')"
printf '%s  %s\n' "$archive_digest" "$(basename -- "$archive")" >"$archive.sha256"

reject 'relative deployment target' bash "$installer" "$archive" relative/path
reject 'root deployment target' bash "$installer" "$archive" /

mv "$archive.sha256" "$archive.sha256.saved"
reject 'missing archive sidecar' bash "$installer" "$archive" "$test_root/missing-sidecar-target"
mv "$archive.sha256.saved" "$archive.sha256"

printf '%s  %s\n' "$archive_digest" wrong-name.tar.gz >"$archive.sha256"
reject 'sidecar bound to another filename' bash "$installer" "$archive" "$test_root/misbound-target"
printf '%s  %s\n' "$archive_digest" "$(basename -- "$archive")" >"$archive.sha256"

unsafe_fixture="$test_root/unsafe-fixture"
mkdir -p "$unsafe_fixture/bin"
cp "$fixture/bin/deploy.sh" "$unsafe_fixture/bin/deploy.sh"
cp "$fixture/docker-compose.yml" "$unsafe_fixture/docker-compose.yml"
printf 'not allowlisted\n' >"$unsafe_fixture/secret.txt"
unsafe_archive="$test_root/deployment-assets-unsafe.tar.gz"
tar -czf "$unsafe_archive" -C "$unsafe_fixture" .
printf '%s  %s\n' "$(sha256sum "$unsafe_archive" | awk '{print tolower($1)}')" \
  "$(basename -- "$unsafe_archive")" >"$unsafe_archive.sha256"
reject 'unknown deployment asset' bash "$installer" "$unsafe_archive" "$test_root/unsafe-target"

link_fixture="$test_root/link-fixture"
mkdir -p "$link_fixture/bin"
cp "$fixture/bin/deploy.sh" "$link_fixture/bin/deploy.sh"
cp "$fixture/docker-compose.yml" "$link_fixture/docker-compose.yml"
ln -s /tmp "$link_fixture/subsystems.d"
link_archive="$test_root/deployment-assets-link.tar.gz"
tar -czf "$link_archive" -C "$link_fixture" .
printf '%s  %s\n' "$(sha256sum "$link_archive" | awk '{print tolower($1)}')" \
  "$(basename -- "$link_archive")" >"$link_archive.sha256"
reject 'symbolic link in deployment assets' bash "$installer" "$link_archive" "$test_root/link-target"

target="$test_root/target"
bash "$installer" "$archive" "$target" >"$test_root/install.log"
[[ -f "$target/docker-compose.yml" && -f "$target/bin/deploy.sh" ]] || fail 'installed entrypoints missing'
[[ -f "$target/OFFLINE_RUNBOOK.md" && -f "$target/ACCEPTANCE_CHECKLIST.md" ]] || fail 'installed operator documents missing'
[[ ! -e "$target/.env" && ! -e "$target/.release.env" ]] || fail 'installer created runtime configuration'
[[ ! -e "$target/runtime/.assets-install-transaction" ]] || fail 'committed install left a transaction marker'
[[ "$(stat -c '%a' "$target/bin/deploy.sh")" == 750 ]] || fail 'script mode is not 0750'
[[ "$(stat -c '%a' "$target/mysql-init/bootstrap.sql")" == 644 ]] || fail 'container-readable asset mode is not 0644'
pass 'blank target installs current assets without creating runtime configuration'

printf 'services:\n  changed: {}\n' >"$fixture/docker-compose.yml"
tar -czf "$archive" -C "$fixture" .
archive_digest="$(sha256sum "$archive" | awk '{print tolower($1)}')"
printf '%s  %s\n' "$archive_digest" "$(basename -- "$archive")" >"$archive.sha256"
bash "$installer" "$archive" "$target" >"$test_root/update.log"
grep -q '^services: {}$' "$target/docker-compose.yml" || fail 'local Compose selection was overwritten'
grep -q '^  changed: {}$' "$target/docker-compose.yml.dist" || fail 'updated Compose template was not staged as .dist'
pass 'repeat install preserves active Compose and stages the new template'

if [[ -e "$deploy_dir/../../../20260922-offline-v1/服务器前置检查与安装脚本.sh" ]]; then
  fail 'retired all-in-one installer unexpectedly reintroduced'
fi
pass 'regression has no dependency on retired delivery scripts'

echo 'Current blank-server bootstrap regression passed.'
