#!/usr/bin/env bash
set -Eeuo pipefail
source_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/auto-install-packages-test.XXXXXX")"
trap 'rm -r -- "$test_root"' EXIT
mkdir -p "$test_root/deploy/bin" "$test_root/deploy/packages" "$test_root/deploy/manifests"
cp "$source_dir/bin/deploy.sh" "$source_dir/bin/compose-scope.sh" "$source_dir/bin/start-enabled.sh" "$source_dir/bin/project-background.sh" "$source_dir/bin/provisioner-config-refresh.sh" "$source_dir/bin/offline-configure.sh" "$source_dir/bin/offline-package-metadata.sh" "$source_dir/bin/public-transport.sh" "$test_root/deploy/bin/"
cp "$source_dir"/bin/deploy-{common,import,subsystem,license,doctor,package,status,verify,menu,registry}.sh "$test_root/deploy/bin/"
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
# sha256_of 计算测试文件的内容摘要。
# 参数：第一个参数为夹具文件路径。
# 返回值：输出SHA256摘要，读取失败时返回非零。
sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum -- "$1" | awk '{print $1}'
  else
    shasum -a 256 -- "$1" | awk '{print $1}'
  fi
}
# file_mode 兼容Linux及macOS读取测试恢复件权限。
# 参数：第一个参数为待核验文件路径。
# 返回值：输出八进制权限；读取失败返回非零。
file_mode() {
  local mode
  if mode="$(stat -c '%a' -- "$1" 2>/dev/null)" && [[ "$mode" =~ ^[0-7]{3,4}$ ]]; then
    printf '%s' "$mode"
    return 0
  fi
  stat -f '%Lp' -- "$1"
}

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

# 签名交付安装先导入身份再准备业务候选，然后等待实际强制执行；失败绝不能报告成功。
mkdir -p "$deploy_dir/license"
printf 'fixture-license\n' > "$deploy_dir/license/commercial-license.jws"
# 一次性维护命令需要 API 的生产配置、profile 挂载和 provisioner 套接字，但不能重启其依赖。
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
# 这里只检查续期传输；真实 Go 验证器和导入器分别由签名和 SQL 测试覆盖，不由此 Shell 夹具模拟。
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
# 落盘失败保护：平台已导入续期，但替换交付许可证失败时必须保留新证、旧证不变、非零退出且不激活。
# 用 PATH 注入假 mv（以及假 install 不让 runtime 恢复件落盘）复现宿主机写入失败。
fake_bin="$test_root/fake-bin"
mkdir -p "$fake_bin/mv" "$fake_bin/mv-install"
real_install="$(command -v install)"
printf '#!/usr/bin/env bash\nexit 1\n' > "$fake_bin/mv/mv"
printf '#!/usr/bin/env bash\nexit 1\n' > "$fake_bin/mv-install/mv"
cat > "$fake_bin/mv-install/install" <<EOF
#!/usr/bin/env bash
for argument in "\$@"; do
  case "\$argument" in
    */runtime/*) exit 1 ;;
  esac
done
exec "$real_install" "\$@"
EOF
chmod +x "$fake_bin/mv/mv" "$fake_bin/mv-install/mv" "$fake_bin/mv-install/install"

installed_sha="$(sha256_of "$deploy_dir/license/commercial-license.jws")"
printf 'mv-failure-renewal\n' > "$test_root/mv-failure.jws"
: > "$events"
if (
  PATH="$fake_bin/mv:$PATH"
  renew_delivery_license "$test_root/mv-failure.jws"
) > "$test_root/mv-failure.log" 2>&1; then fail 'license replacement failure returned success'; fi
[[ "$(sha256_of "$deploy_dir/license/commercial-license.jws")" == "$installed_sha" ]] || fail 'failed replacement changed the installed license'
if grep -q '^license:activate$' "$events"; then fail 'activation ran after a failed license replacement'; fi
recovery_path="$(sed -n 's/^恢复件（0600 普通文件）：//p' "$test_root/mv-failure.log")"
[[ -n "$recovery_path" && "$recovery_path" == /* ]] || fail 'recovery artifact path is not printed as an absolute path'
[[ "$recovery_path" == "$deploy_dir/runtime/"* ]] || fail 'recovery artifact was not written under runtime/'
[[ -f "$recovery_path" && ! -L "$recovery_path" ]] || fail 'recovery artifact is not a regular file'
[[ "$(file_mode "$recovery_path")" == 600 ]] || fail 'recovery artifact permissions are not 0600'
cmp -s "$test_root/mv-failure.jws" "$recovery_path" || fail 'recovery artifact does not contain the new renewal license'
staged_path="$(sed -n 's/^暂存副本（0600）：//p' "$test_root/mv-failure.log")"
[[ -f "$staged_path" && ! -L "$staged_path" ]] || fail 'staged renewal was not preserved'
[[ "$(file_mode "$staged_path")" == 600 ]] || fail 'preserved staged renewal permissions are not 0600'
cmp -s "$test_root/mv-failure.jws" "$staged_path" || fail 'preserved staged renewal content changed'
grep -q '^恢复命令：install -m 600 -- .* license-activate$' "$test_root/mv-failure.log" || fail 'executable recovery command absent'
if grep -q 'mv-failure-renewal' "$test_root/mv-failure.log"; then fail 'license document content leaked into the failure log'; fi
rm -f -- "$recovery_path" "$staged_path"

printf 'double-failure-renewal\n' > "$test_root/double-failure.jws"
: > "$events"
if (
  PATH="$fake_bin/mv-install:$PATH"
  renew_delivery_license "$test_root/double-failure.jws"
) > "$test_root/double-failure.log" 2>&1; then fail 'double failure returned success'; fi
[[ "$(sha256_of "$deploy_dir/license/commercial-license.jws")" == "$installed_sha" ]] || fail 'double failure changed the installed license'
if grep -q '^license:activate$' "$events"; then fail 'activation ran after a double failure'; fi
staged_fallback="$(sed -n 's/^恢复件（0600 普通文件）：//p' "$test_root/double-failure.log")"
[[ "$staged_fallback" == "$deploy_dir/license/.renewal."* ]] || fail 'double failure did not fall back to the staged renewal'
[[ -f "$staged_fallback" && ! -L "$staged_fallback" ]] || fail 'staged renewal was deleted after a double failure'
[[ "$(file_mode "$staged_fallback")" == 600 ]] || fail 'fallback staged renewal permissions are not 0600'
cmp -s "$test_root/double-failure.jws" "$staged_fallback" || fail 'fallback staged renewal content changed'
if compgen -G "$deploy_dir/runtime/.commercial-license.recovery.*" >/dev/null; then fail 'failed recovery copy was left in runtime/'; fi
grep -q '^恢复命令：install -m 600 -- .* license-activate$' "$test_root/double-failure.log" || fail 'fallback recovery command absent'
if grep -q 'double-failure-renewal' "$test_root/double-failure.log"; then fail 'license document content leaked into the double-failure log'; fi
rm -f -- "$staged_fallback"
rm "$deploy_dir/license/commercial-license.jws"
rmdir "$deploy_dir/license"
# 仅覆盖控制流程夹具；真实注册和 ACK 检查由 Go 测试执行。
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
