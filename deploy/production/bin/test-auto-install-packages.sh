#!/usr/bin/env bash
set -Eeuo pipefail
source_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/auto-install-packages-test.XXXXXX")"
trap 'rm -r -- "$test_root"' EXIT
mkdir -p "$test_root/deploy/bin" "$test_root/deploy/packages" "$test_root/deploy/manifests"
cp "$source_dir/bin/deploy.sh" "$source_dir/bin/offline-configure.sh" "$source_dir/bin/public-transport.sh" "$test_root/deploy/bin/"
cp "$source_dir/compose.yaml" "$test_root/deploy/"
touch "$test_root/deploy/.env" "$test_root/deploy/.release.env"
chmod 600 "$test_root/deploy/.env" "$test_root/deploy/.release.env"
source "$test_root/deploy/bin/deploy.sh"
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
echo 'automatic package discovery and staged install tests passed'
