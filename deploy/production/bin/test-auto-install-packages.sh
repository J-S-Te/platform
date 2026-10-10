#!/usr/bin/env bash
set -Eeuo pipefail
source_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/auto-install-packages-test.XXXXXX")"
trap 'rm -r -- "$test_root"' EXIT
mkdir -p "$test_root/deploy/bin" "$test_root/deploy/packages" "$test_root/deploy/manifests"
cp "$source_dir/bin/deploy.sh" "$source_dir/bin/compose-scope.sh" "$source_dir/bin/start-enabled.sh" "$source_dir/bin/provisioner-config-refresh.sh" "$source_dir/bin/offline-configure.sh" "$source_dir/bin/offline-package-metadata.sh" "$source_dir/bin/public-transport.sh" "$test_root/deploy/bin/"
cp "$source_dir/docker-compose.yml" "$test_root/deploy/"
touch "$test_root/deploy/.env" "$test_root/deploy/.release.env"
chmod 600 "$test_root/deploy/.env" "$test_root/deploy/.release.env"
source "$test_root/deploy/bin/deploy.sh"
scope_services() { printf '%s\n' platform-api frontend customer-api portal-api contract-api project-api settlement-api data-analysis-api; }
scope_report() { :; }
deploy_dir="$test_root/deploy"
package_dir="$deploy_dir/packages"
events="$test_root/events"
# 这个用例聚焦包发现和安装顺序；目标部署入口在 Linux 上使用 GNU stat -c，
# 测试环境可能是 macOS，因此仅替换配置文件权限的预检，不触碰部署动作。
require_initialized() {
  [[ -f "$runtime_file" && -f "$release_file" ]]
}
prompt_ip="$(printf ' 192.168.3.38 \r\n' | configuration_prompt '服务器 IP' '')"
[[ "$prompt_ip" == 192.168.3.38 ]] || { echo 'IP prompt did not trim pasted whitespace/CRLF' >&2; exit 1; }
import_package() { printf 'import:%s\n' "$(basename -- "$1")" >> "$events"; }
prepare_subsystem() { printf 'prepare:%s\n' "$1" >> "$events"; }
deploy_component() { printf 'deploy:%s\n' "$1" >> "$events"; }
verify() { printf 'verify\n' >> "$events"; }
wait_for_required_service_health() { printf 'verify\n' >> "$events"; }
fail() { echo "FAIL: $*" >&2; exit 1; }

if (auto_install_packages) >"$test_root/missing.log" 2>&1; then fail 'missing core packages accepted'; fi
grep -q '缺少基础组件镜像包：common' "$test_root/missing.log" || { cat "$test_root/missing.log" >&2; fail 'missing core package message absent'; }
[[ ! -s "$events" ]] || fail 'performed operations before validating required package set'

for name in common-infrastructure-linux-amd64.tar.gz platform-backend-v1-linux-amd64.tar.gz frontend-v1-linux-amd64.tar.gz customer-opportunity-backend-v1-linux-amd64.tar.gz contract-backend-v1-linux-amd64.tar.gz; do
  : > "$package_dir/$name"
done
auto_install_packages >"$test_root/install.log"
printf '%s\n' \
  'import:common-infrastructure-linux-amd64.tar.gz' \
  'import:platform-backend-v1-linux-amd64.tar.gz' \
  'deploy:platform' \
  'verify' \
  'import:frontend-v1-linux-amd64.tar.gz' \
  'deploy:frontend' \
  'verify' \
  'import:customer-opportunity-backend-v1-linux-amd64.tar.gz' \
  'prepare:customer-opportunity' \
  'import:contract-backend-v1-linux-amd64.tar.gz' \
  'prepare:contract' > "$test_root/expected"
diff -u "$test_root/expected" "$events" || fail 'package operations were not performed in the required order'
grep -q '子系统服务不会自动启动' "$test_root/install.log" || fail 'subsystem safety notice absent'

: > "$events"
if (
  prepare_subsystem() {
    printf 'prepare:%s\n' "$1" >> "$events"
    [[ "$1" != customer-opportunity ]]
  }
  auto_install_packages
) >"$test_root/prepare-failure.log" 2>&1; then
  fail 'failed subsystem prepare did not stop automatic installation'
fi
grep -q '^prepare:customer-opportunity$' "$events" || fail 'failed prepare stage was not recorded'
if grep -q '^import:contract-backend-v1-linux-amd64.tar.gz$' "$events"; then
  fail 'automatic installation continued after a failed prepare'
fi

: > "$events"
: > "$package_dir/platform-backend-v2-linux-amd64.tar.gz"
if (auto_install_packages) >"$test_root/duplicate.log" 2>&1; then fail 'ambiguous platform packages accepted'; fi
grep -q '发现多个 platform 镜像包' "$test_root/duplicate.log" || fail 'ambiguous package error absent'
[[ ! -s "$events" ]] || fail 'performed operations before rejecting ambiguous packages'
rm "$package_dir/platform-backend-v2-linux-amd64.tar.gz"

# Signed-delivery installation imports identity before preparing business
# candidates, then waits for actual enforcement; failure must not report success.
mkdir -p "$deploy_dir/license"
printf 'fixture-license\n' > "$deploy_dir/license/commercial-license.jws"
# The one-shot maintenance command needs the API's production configuration,
# profile mounts and provisioner socket, without restarting its dependencies.
compose() { printf '%s\n' "$*" > "$test_root/license-compose"; }
install_delivery_license import
grep -q 'run -T --rm --no-deps .* platform-api ./license-install import' "$test_root/license-compose" || fail 'license maintenance did not inherit the API topology'
for scenario in fresh migrate platform-only expand; do
  jq -n --arg scenario "$scenario" '{version:1,scenario:$scenario,customer_id:"fixture-customer"}' > "$deploy_dir/license-installation.json"
  prepare_license_installation
  grep -q "platform-api ./license-install prepare --scenario $scenario --customer fixture-customer --file" "$test_root/license-compose" || fail 'scenario or customer not passed to controlled preparation'
done
: > "$events"
if (
  compose() { return 1; }
  auto_install_packages
) > "$test_root/baseline-failure.log" 2>&1; then fail 'failed baseline accepted'; fi
if grep -q '^prepare:' "$events"; then fail 'business preparation started after failed baseline'; fi
rm "$deploy_dir/license-installation.json"
install_delivery_license() { printf 'license:%s\n' "$1" >> "$events"; }
: > "$events"
auto_install_packages > "$test_root/license.log"
grep -q '^license:import$' "$events" || fail 'delivery import missing'
grep -q '^license:activate$' "$events" || fail 'enforcement confirmation missing'
[[ "$(tail -1 "$events")" == license:activate ]] || fail 'activation ran before preparation completed'
grep -q '交付授权已生效' "$test_root/license.log" || fail 'confirmed enforcement result absent'
if (
  install_delivery_license() { [[ "$1" != activate ]]; }
  auto_install_packages
) > "$test_root/license-failure.log" 2>&1; then
  fail 'failed activation reported successful install'
fi
if grep -q '交付授权已生效' "$test_root/license-failure.log"; then fail 'failed activation printed enforcement success'; fi
# Transport-only renewal checks: the real Go verifier/importer is independently
# covered by signature and SQL tests, not simulated by this shell fixture.
mkdir -p "$deploy_dir/runtime"
flock() { return 0; }
printf 'renewal-fixture\n' > "$test_root/renewal.jws"
renew_delivery_license "$test_root/renewal.jws"
cmp "$test_root/renewal.jws" "$deploy_dir/license/commercial-license.jws" || fail 'verified renewal was not atomically installed'
grep -q 'run -T --rm --no-deps .* platform-api ./license-install import' "$test_root/license-compose" || fail 'renewal did not inherit API topology'
printf 'rejected-fixture\n' > "$test_root/rejected.jws"
if (
  compose() { return 1; }
  renew_delivery_license "$test_root/rejected.jws"
) > "$test_root/renewal-failure.log" 2>&1; then fail 'rejected renewal returned success'; fi
cmp "$test_root/renewal.jws" "$deploy_dir/license/commercial-license.jws" || fail 'rejected renewal replaced the installed license'
rm "$deploy_dir/license/commercial-license.jws"
rmdir "$deploy_dir/license"
# Control-flow fixtures only: real registration/ACK checks run in Go tests.
for scenario in fresh migrate platform-only expand; do
  jq -n --arg scenario "$scenario" '{version:1,scenario:$scenario,customer_id:"fixture-customer"}' > "$deploy_dir/license-installation.json"
  : > "$events"
  (
    compose() { printf 'maintenance:%s\n' "$*" >> "$events"; }
    auto_install_packages
  ) > "$test_root/scenario-$scenario.log"
  [[ "$(tail -1 "$events")" == 'maintenance:run -T --rm --no-deps platform-api ./license-install migrate' ]] || fail 'migration confirmation did not follow business preparation'
  baseline_line="$(grep -n 'license-install prepare' "$events" | cut -d: -f1)"
  business_line="$(grep -n '^prepare:customer-opportunity$' "$events" | cut -d: -f1)"
  [[ "$baseline_line" -lt "$business_line" ]] || fail 'baseline captured after business preparation'
  grep -q '新增未授权系统不会开放业务' "$test_root/scenario-$scenario.log" || fail 'scenario safety notice absent'
  if grep -q '交付授权已生效' "$test_root/scenario-$scenario.log"; then fail 'unlicensed migration claimed formal enforcement'; fi
done
rm "$deploy_dir/license-installation.json"
scope_services() { printf '%s\n' platform-api frontend; }
: > "$events"
auto_install_packages > "$test_root/core-only.log"
if grep -Eq 'contract|customer-opportunity' "$events"; then
  fail 'commented subsystem package was imported or prepared'
fi
echo 'automatic package discovery and staged install tests passed'
