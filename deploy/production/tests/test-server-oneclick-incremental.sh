#!/usr/bin/env bash
set -Eeuo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../../../.." && pwd)"
installer="$repo_root/服务器一键离线部署.sh"
fixture="$(mktemp -d "${TMPDIR:-/tmp}/server-oneclick-incremental-test.XXXXXX")"
trap 'rm -rf -- "$fixture"' EXIT

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

[[ "$(uname -s)" == Linux ]] || fail 'this regression must run in a Linux amd64 test container'
[[ "$(id -u)" == 0 ]] || fail 'this regression must run as root'

media="$fixture/media"
target="$fixture/target"
tools="$fixture/tools"
docker_root="$fixture/docker-root"
events="$fixture/events"
mkdir -p "$media" "$target/bin" "$target/packages" "$target/manifests" \
  "$target/runtime" "$target/subsystems.d" "$tools" "$docker_root"

make_image_package() {
  local component="$1" name="$2" package_root
  package_root="$(mktemp -d "$fixture/package.XXXXXX")"
  printf 'PACKAGE_FORMAT=2\nCOMPONENT=%s\nVERSION=test-v1\nPLATFORM=linux/amd64\n' "$component" \
    > "$package_root/package.env"
  printf 'payload for %s\n' "$component" > "$package_root/images.tar"
  (cd "$package_root" && sha256sum images.tar > SHA256SUMS)
  tar -czf "$media/$name" -C "$package_root" package.env SHA256SUMS images.tar
  (cd "$media" && sha256sum "$name" > "$name.sha256")
  rm -rf -- "$package_root"
}

make_image_package common common-infrastructure-linux-amd64.tar.gz
make_image_package platform platform-backend-test-v1-linux-amd64.tar.gz
make_image_package frontend frontend-test-v1-linux-amd64.tar.gz
make_image_package customer-opportunity customer-opportunity-backend-test-v1-linux-amd64.tar.gz
make_image_package customer-portal customer-portal-backend-test-v1-linux-amd64.tar.gz
make_image_package contract contract-backend-test-v1-linux-amd64.tar.gz

assets_root="$fixture/assets-root"
mkdir -p "$assets_root/subsystems.d" "$assets_root/subsystem-templates"
printf 'application_code: customer_and_opportunity\nenvironment_code: prod\n' \
  > "$assets_root/subsystems.d/customer_and_opportunity-prod.yaml"
printf 'CUSTOMER_CLIENT_ID=PENDING_ONBOARDING\n' \
  > "$assets_root/subsystem-templates/customer.env.example"
tar -czf "$media/deployment-assets-test-v1.tar.gz" -C "$assets_root" .
(cd "$media" && sha256sum deployment-assets-test-v1.tar.gz > deployment-assets-test-v1.tar.gz.sha256)
cat > "$media/PLATFORM_ASSETS_COMPATIBILITY.env" <<EOF
FORMAT=1
PLATFORM_ASSETS_INPUT_SHA256=$(printf 'a%.0s' {1..64})
COMMON_PACKAGE=common-infrastructure-linux-amd64.tar.gz
COMMON_PACKAGE_SHA256=$(sha256sum "$media/common-infrastructure-linux-amd64.tar.gz" | awk '{print $1}')
PLATFORM_PACKAGE=platform-backend-test-v1-linux-amd64.tar.gz
PLATFORM_PACKAGE_SHA256=$(sha256sum "$media/platform-backend-test-v1-linux-amd64.tar.gz" | awk '{print $1}')
FRONTEND_PACKAGE=frontend-test-v1-linux-amd64.tar.gz
FRONTEND_PACKAGE_SHA256=$(sha256sum "$media/frontend-test-v1-linux-amd64.tar.gz" | awk '{print $1}')
ASSETS_PACKAGE=deployment-assets-test-v1.tar.gz
ASSETS_PACKAGE_SHA256=$(sha256sum "$media/deployment-assets-test-v1.tar.gz" | awk '{print $1}')
EOF
cat > "$media/install-assets.sh" <<'STUB'
#!/usr/bin/env bash
printf 'install-assets-called\n' >> "${STUB_EVENTS:?}"
exit 99
STUB
chmod 0755 "$media/install-assets.sh"
cp "$repo_root/系统运维工具.sh" "$media/系统运维工具.sh"
chmod 0755 "$media/系统运维工具.sh"
printf 'BASE_DELIVERY=base-v1\nBASE_SHA256SUMS_SHA256=%064d\n' 0 > "$media/BASE_DELIVERY_INFO.txt"
(
  cd "$media"
  sha256sum -- \
    common-infrastructure-linux-amd64.tar.gz \
    common-infrastructure-linux-amd64.tar.gz.sha256 \
    platform-backend-test-v1-linux-amd64.tar.gz \
    platform-backend-test-v1-linux-amd64.tar.gz.sha256 \
    frontend-test-v1-linux-amd64.tar.gz \
    frontend-test-v1-linux-amd64.tar.gz.sha256 \
    customer-opportunity-backend-test-v1-linux-amd64.tar.gz \
    customer-opportunity-backend-test-v1-linux-amd64.tar.gz.sha256 \
    customer-portal-backend-test-v1-linux-amd64.tar.gz \
    customer-portal-backend-test-v1-linux-amd64.tar.gz.sha256 \
    contract-backend-test-v1-linux-amd64.tar.gz \
    contract-backend-test-v1-linux-amd64.tar.gz.sha256 \
    deployment-assets-test-v1.tar.gz \
    deployment-assets-test-v1.tar.gz.sha256 \
    PLATFORM_ASSETS_COMPATIBILITY.env \
    install-assets.sh \
    系统运维工具.sh \
    BASE_DELIVERY_INFO.txt > SHA256SUMS
)

printf 'SERVER_IP=192.0.2.10\n' > "$target/.env"
printf 'PLATFORM_IMAGE=registry.example/platform@sha256:existing\n' > "$target/.release.env"
printf 'services: {}\n' > "$target/docker-compose.yml"
chmod 0600 "$target/.env" "$target/.release.env"

cat > "$target/bin/deploy.sh" <<'STUB'
#!/usr/bin/env bash
set -Eeuo pipefail
events="${STUB_EVENTS:?}"
case "${1:-}" in
  verify) printf 'verify\n' >> "$events" ;;
  reload-control-plane) printf 'reload-control-plane\n' >> "$events" ;;
  import)
    [[ -f "${2:?}" ]] || exit 1
    printf 'import:%s\n' "$(basename -- "$2")" >> "$events"
    ;;
  prepare) printf 'prepare:%s\n' "${2:?}" >> "$events" ;;
  status) printf 'status:%s\n' "${2:?}" >> "$events" ;;
  *) printf 'unexpected deploy command: %s\n' "$*" >&2; exit 1 ;;
esac
STUB
chmod 0755 "$target/bin/deploy.sh"

# 这些文件模拟首次已安装的基础平台和合同系统；增量流程必须保持它们。
# --extend-from 交付必须逐字节复用这三个基础包，服务器入口也要独立复核。
cp "$media/common-infrastructure-linux-amd64.tar.gz" "$target/packages/common-infrastructure-linux-amd64.tar.gz"
cp "$media/platform-backend-test-v1-linux-amd64.tar.gz" "$target/packages/platform-backend-test-v1-linux-amd64.tar.gz"
cp "$media/frontend-test-v1-linux-amd64.tar.gz" "$target/packages/frontend-test-v1-linux-amd64.tar.gz"
cp "$media/PLATFORM_ASSETS_COMPATIBILITY.env" \
  "$target/packages/PLATFORM_ASSETS_COMPATIBILITY.delivery.env"
chmod 0600 "$target/packages/PLATFORM_ASSETS_COMPATIBILITY.delivery.env"
printf 'installed contract\n' > "$target/packages/contract-backend-existing-linux-amd64.tar.gz"
cp -a "$assets_root/subsystems.d/." "$target/subsystems.d/"
mkdir -p "$target/subsystem-templates"
cp -a "$assets_root/subsystem-templates/." "$target/subsystem-templates/"
base_digest_before="$(sha256sum "$target/packages/platform-backend-test-v1-linux-amd64.tar.gz")"
env_digest_before="$(sha256sum "$target/.env")"

cat > "$tools/docker" <<'STUB'
#!/usr/bin/env bash
set -Eeuo pipefail
case "${1:-} ${2:-}" in
  'compose version') printf 'v2.30.0\n' ;;
  'info --format') printf '%s\n' "${STUB_DOCKER_ROOT:?}" ;;
  'info ') exit 0 ;;
  'version --format') printf '29.8.1\n' ;;
  *) printf 'unexpected docker invocation: %s\n' "$*" >&2; exit 1 ;;
esac
STUB
chmod 0755 "$tools/docker"
for command_name in jq curl update-ca-certificates openssl flock ss; do
  cat > "$tools/$command_name" <<'STUB'
#!/usr/bin/env bash
exit 0
STUB
  chmod 0755 "$tools/$command_name"
done

common_env=(
  PATH="$tools:$PATH"
  STUB_DOCKER_ROOT="$docker_root"
  STUB_EVENTS="$events"
)

env "${common_env[@]}" bash "$installer" \
  --media "$media" \
  --target "$target" \
  --add-components customer-opportunity \
  --skip-host-deps \
  --yes \
  --check-only > "$fixture/check-only.log"
grep -q '增量检查完成' "$fixture/check-only.log" || fail 'incremental check-only success marker missing'
[[ ! -e "$events" ]] || fail 'check-only invoked deploy operations'
[[ ! -e "$target/系统运维工具.sh" ]] || fail 'check-only installed the operations tool'

env "${common_env[@]}" bash "$installer" \
  --media "$media" \
  --target "$target" \
  --add-subsystems customer-opportunity \
  --skip-host-deps \
  --yes > "$fixture/add-crm.log"

cmp -s "$repo_root/系统运维工具.sh" "$target/系统运维工具.sh" ||
  fail 'incremental install did not install the operations tool'

cat > "$fixture/expected-crm-events" <<'EOF'
reload-control-plane
verify
import:customer-opportunity-backend-test-v1-linux-amd64.tar.gz
prepare:customer-opportunity
status:customer-opportunity
EOF
diff -u "$fixture/expected-crm-events" "$events" || fail 'CRM incremental operation order is wrong'
grep -q '基础平台、基础设施、统一前端、已有子系统及数据未重新部署' "$fixture/add-crm.log" ||
  fail 'incremental safety summary missing'
[[ "$(sha256sum "$target/packages/platform-backend-test-v1-linux-amd64.tar.gz")" == "$base_digest_before" ]] ||
  fail 'incremental add changed the installed platform package'
[[ "$(sha256sum "$target/.env")" == "$env_digest_before" ]] || fail 'incremental add changed .env'
[[ -f "$target/packages/customer-opportunity-backend-test-v1-linux-amd64.tar.gz" ]] || fail 'CRM package was not retained'
if grep -q 'install-assets-called' "$events"; then fail 'incremental add invoked the asset installer'; fi

# 幂等续跑允许相同包，不会因上一次已复制而失败。
: > "$events"
env "${common_env[@]}" bash "$installer" \
  --media "$media" \
  --target "$target" \
  --add-subsystems customer-opportunity \
  --skip-host-deps \
  --yes > "$fixture/resume-crm.log"
grep -q '镜像包已存在且内容一致' "$fixture/resume-crm.log" || fail 'idempotent resume marker missing'
diff -u "$fixture/expected-crm-events" "$events" || fail 'idempotent CRM resume order is wrong'

# Portal 必须先处理 CRM，不能反转依赖顺序。
: > "$events"
env "${common_env[@]}" bash "$installer" \
  --media "$media" \
  --target "$target" \
  --add-subsystems customer-portal \
  --skip-host-deps \
  --yes > "$fixture/add-portal.log"
cat > "$fixture/expected-portal-events" <<'EOF'
reload-control-plane
verify
import:customer-opportunity-backend-test-v1-linux-amd64.tar.gz
prepare:customer-opportunity
import:customer-portal-backend-test-v1-linux-amd64.tar.gz
prepare:customer-portal
status:customer-opportunity
status:customer-portal
EOF
diff -u "$fixture/expected-portal-events" "$events" || fail 'Portal dependency order is wrong'

# 同系统的其他版本必须转升级流程，不能被增量首次追加覆盖。
rm -f -- "$target/packages/customer-opportunity-backend-test-v1-linux-amd64.tar.gz" \
  "$target/packages/customer-opportunity-backend-test-v1-linux-amd64.tar.gz.sha256"
printf 'older customer package\n' > "$target/packages/customer-opportunity-backend-old-linux-amd64.tar.gz"
: > "$events"
if env "${common_env[@]}" bash "$installer" \
  --media "$media" \
  --target "$target" \
  --add-subsystems customer-opportunity \
  --skip-host-deps \
  --yes > "$fixture/version-conflict.log" 2>&1; then
  fail 'incremental add accepted another installed version of the same subsystem'
fi
grep -q '必须使用受控 upgrade 流程' "$fixture/version-conflict.log" || fail 'upgrade guidance missing'
if grep -Eq '^(import|prepare):' "$events"; then fail 'version conflict imported or prepared a package'; fi

# 资产清单已被热修复或来自另一基线时，必须在重载控制面、导入镜像之前拒绝。
rm -f -- "$target/packages/customer-opportunity-backend-old-linux-amd64.tar.gz"
printf '\nallowed_service_bindings: [file_gateway_write]\n' \
  >> "$target/subsystems.d/customer_and_opportunity-prod.yaml"
: > "$events"
if env "${common_env[@]}" bash "$installer" \
  --media "$media" \
  --target "$target" \
  --add-subsystems customer-opportunity \
  --skip-host-deps \
  --yes > "$fixture/assets-drift.log" 2>&1; then
  fail 'incremental add accepted drifted shared deployment assets'
fi
grep -q '共享资产与目标不一致' "$fixture/assets-drift.log" || fail 'asset drift guidance missing'
[[ ! -s "$events" ]] || fail 'asset drift invoked control-plane or deploy operations'

# 旧目标没有持久化兼容性锁时 fail-closed，不能仅凭文件名猜测同基线。
cp "$assets_root/subsystems.d/customer_and_opportunity-prod.yaml" \
  "$target/subsystems.d/customer_and_opportunity-prod.yaml"
mv "$target/packages/PLATFORM_ASSETS_COMPATIBILITY.delivery.env" \
  "$target/packages/PLATFORM_ASSETS_COMPATIBILITY.delivery.env.saved"
: > "$events"
if env "${common_env[@]}" bash "$installer" \
  --media "$media" \
  --target "$target" \
  --add-subsystems customer-opportunity \
  --skip-host-deps \
  --yes > "$fixture/missing-lock.log" 2>&1; then
  fail 'incremental add accepted a legacy target without compatibility lock'
fi
grep -q '缺少已保存的 Platform/资产兼容性锁' "$fixture/missing-lock.log" ||
  fail 'missing compatibility lock guidance absent'
[[ ! -s "$events" ]] || fail 'missing compatibility lock invoked deploy operations'
mv "$target/packages/PLATFORM_ASSETS_COMPATIBILITY.delivery.env.saved" \
  "$target/packages/PLATFORM_ASSETS_COMPATIBILITY.delivery.env"

# 基础镜像包内容不同也属于混合基线，不得因文件同名而放行。
cp "$assets_root/subsystems.d/customer_and_opportunity-prod.yaml" \
  "$target/subsystems.d/customer_and_opportunity-prod.yaml"
printf 'tampered platform baseline\n' > "$target/packages/platform-backend-test-v1-linux-amd64.tar.gz"
: > "$events"
if env "${common_env[@]}" bash "$installer" \
  --media "$media" \
  --target "$target" \
  --add-subsystems customer-opportunity \
  --skip-host-deps \
  --yes > "$fixture/base-drift.log" 2>&1; then
  fail 'incremental add accepted a different platform baseline package'
fi
grep -q '内容不同' "$fixture/base-drift.log" || fail 'base package drift guidance missing'
[[ ! -s "$events" ]] || fail 'base package drift invoked control-plane or deploy operations'

# 首装成功路径也必须在最终验收前成对重载控制面。
grep -Fq 'bash ./bin/deploy.sh reload-control-plane' "$installer" ||
  fail 'server installer does not converge the control plane before final verification'

# 空目录首装要原子持久化兼容性锁，供以后增量交付 fail-closed 比对。
full_media="$fixture/full-media"
full_target="$fixture/full-target"
full_events="$fixture/full-events"
cp -a "$media" "$full_media"
cat > "$full_media/install-assets.sh" <<'STUB'
#!/usr/bin/env bash
set -Eeuo pipefail
target="${2:?target required}"
printf 'install-assets-called\n' >> "${STUB_EVENTS:?}"
mkdir -p "$target/bin" "$target/packages" "$target/manifests" "$target/runtime" \
  "$target/subsystems.d" "$target/subsystem-templates"
printf 'services: {}\n' > "$target/docker-compose.yml"
cat > "$target/bin/deploy.sh" <<'DEPLOY_STUB'
#!/usr/bin/env bash
set -Eeuo pipefail
events="${STUB_EVENTS:?}"
case "${1:-}" in
  configure)
    printf 'configure\n' >> "$events"
    printf 'APP_PUBLIC_BASE_URL=http://192.0.2.10\nKEYCLOAK_PUBLIC_URL=http://192.0.2.10:18090\n' > .env
    printf 'PLATFORM_IMAGE=uip/platform@sha256:test\n' > .release.env
    chmod 0600 .env .release.env
    ;;
  install|reload-control-plane|verify|status) printf '%s\n' "$1" >> "$events" ;;
  *) printf 'unexpected first-install command: %s\n' "$*" >&2; exit 1 ;;
esac
DEPLOY_STUB
chmod 0755 "$target/bin/deploy.sh"
STUB
chmod 0755 "$full_media/install-assets.sh"
(
  cd "$full_media"
  find . -maxdepth 1 -type f ! -name SHA256SUMS -printf '%f\n' |
    LC_ALL=C sort |
    xargs sha256sum -- > SHA256SUMS
)
: > "$full_events"
env PATH="$tools:$PATH" STUB_DOCKER_ROOT="$docker_root" STUB_EVENTS="$full_events" \
  bash "$installer" \
    --media "$full_media" \
    --target "$full_target" \
    --skip-host-deps \
    --yes > "$fixture/full-install.log"
cmp -s "$full_media/PLATFORM_ASSETS_COMPATIBILITY.env" \
  "$full_target/packages/PLATFORM_ASSETS_COMPATIBILITY.delivery.env" ||
  fail 'first install did not persist the exact compatibility lock'
[[ "$(stat -c '%a' "$full_target/packages/PLATFORM_ASSETS_COMPATIBILITY.delivery.env")" == 600 ]] ||
  fail 'persisted compatibility lock permissions are not 0600'
cat > "$fixture/expected-full-events" <<'EOF'
install-assets-called
configure
install
reload-control-plane
verify
status
EOF
diff -u "$fixture/expected-full-events" "$full_events" ||
  fail 'first-install control-plane convergence order is wrong'

printf 'server one-click incremental subsystem tests passed\n'
