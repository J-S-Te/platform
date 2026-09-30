#!/usr/bin/env bash
set -Eeuo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../../../.." && pwd)"
wrapper="$repo_root/一键制作离线镜像包.sh"
fixture="$(mktemp -d "${TMPDIR:-/tmp}/oneclick-build-extension-test.XXXXXX")"
trap 'rm -rf -- "$fixture"' EXIT

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

for command_name in jq tar gzip sha256sum; do
  command -v "$command_name" >/dev/null 2>&1 || fail "missing test command: $command_name"
done

baseline="$fixture/base-v1"
tools="$fixture/tools"
docker_root="$fixture/docker-root"
output="$fixture/extension-v1"
mkdir -p "$baseline" "$tools" "$docker_root"

make_baseline_image_package() {
  local component="$1" version="$2" name="$3" stage
  stage="$(mktemp -d "$fixture/base-package.XXXXXX")"
  printf 'PACKAGE_FORMAT=2\nCOMPONENT=%s\nVERSION=%s\nPLATFORM=linux/amd64\n' "$component" "$version" \
    > "$stage/package.env"
  printf 'baseline image payload for %s\n' "$component" > "$stage/images.tar"
  (cd "$stage" && sha256sum images.tar > SHA256SUMS)
  COPYFILE_DISABLE=1 tar -czf "$baseline/$name" -C "$stage" package.env SHA256SUMS images.tar
  (cd "$baseline" && sha256sum "$name" > "$name.sha256")
  rm -rf -- "$stage"
}

make_baseline_image_package common base-v1 common-infrastructure-linux-amd64.tar.gz
make_baseline_image_package platform base-v1 platform-backend-base-v1-linux-amd64.tar.gz
make_baseline_image_package frontend base-v1 frontend-base-v1-linux-amd64.tar.gz
make_baseline_image_package contract base-v1 contract-backend-base-v1-linux-amd64.tar.gz

asset_stage="$(mktemp -d "$fixture/base-assets.XXXXXX")"
mkdir -p "$asset_stage/subsystems.d"
cat > "$asset_stage/subsystems.d/data-analysis-prod.yaml" <<'YAML'
version: 1
application:
  allowed_service_bindings: [contract_dashboard_read, project_dashboard_read, file_gateway_write]
runtime:
  files:
    - bindings:
        FILE_GATEWAY_APPLICATION_ID: application_id
        FILE_GATEWAY_CLIENT_ID: service.file_gateway_write.client_id
        FILE_GATEWAY_CLIENT_SECRET: service.file_gateway_write.client_secret
YAML
COPYFILE_DISABLE=1 tar -czf "$baseline/deployment-assets-base-v1.tar.gz" -C "$asset_stage" subsystems.d/data-analysis-prod.yaml
rm -rf -- "$asset_stage"
(cd "$baseline" && sha256sum deployment-assets-base-v1.tar.gz > deployment-assets-base-v1.tar.gz.sha256)

platform_assets_input_fingerprint() (
  cd "$repo_root"
  {
    find platform/cmd platform/internal platform/migrations -type f -print0
    printf '%s\0' \
      platform/Dockerfile platform/.dockerignore platform/go.mod platform/go.sum \
      platform/docker-entrypoint.sh platform/scripts/sync-contract-catalog.sh \
      platform/scripts/sync-settlement-catalog.sh platform/scripts/sync-project-catalog.sh
    find frontend/src frontend/public frontend/nginx -type f -print0
    printf '%s\0' \
      frontend/Dockerfile frontend/.dockerignore frontend/package.json frontend/package-lock.json \
      frontend/index.html frontend/login.html frontend/vite.config.js
    find platform/deploy/production/bin platform/deploy/production/monitoring \
      platform/deploy/production/mysql-init platform/deploy/production/nginx \
      platform/deploy/production/subsystem-templates platform/deploy/production/subsystems.d \
      -type f -print0
    printf '%s\0' \
      platform/deploy/production/.env.example \
      platform/deploy/production/.release.env.example \
      platform/deploy/production/docker-compose.yml
  } | LC_ALL=C sort -zu | while IFS= read -r -d '' shared_file; do
    [[ -f "$shared_file" && ! -L "$shared_file" ]] || continue
    printf '%s  %s\0' "$(sha256sum "$shared_file" | awk '{print tolower($1)}')" "$shared_file"
  done | sha256sum | awk '{print tolower($1)}'
)

cat > "$baseline/PLATFORM_ASSETS_COMPATIBILITY.env" <<EOF
FORMAT=1
PLATFORM_ASSETS_INPUT_SHA256=$(platform_assets_input_fingerprint)
COMMON_PACKAGE=common-infrastructure-linux-amd64.tar.gz
COMMON_PACKAGE_SHA256=$(sha256sum "$baseline/common-infrastructure-linux-amd64.tar.gz" | awk '{print $1}')
PLATFORM_PACKAGE=platform-backend-base-v1-linux-amd64.tar.gz
PLATFORM_PACKAGE_SHA256=$(sha256sum "$baseline/platform-backend-base-v1-linux-amd64.tar.gz" | awk '{print $1}')
FRONTEND_PACKAGE=frontend-base-v1-linux-amd64.tar.gz
FRONTEND_PACKAGE_SHA256=$(sha256sum "$baseline/frontend-base-v1-linux-amd64.tar.gz" | awk '{print $1}')
ASSETS_PACKAGE=deployment-assets-base-v1.tar.gz
ASSETS_PACKAGE_SHA256=$(sha256sum "$baseline/deployment-assets-base-v1.tar.gz" | awk '{print $1}')
EOF
cat > "$baseline/install-assets.sh" <<'STUB'
#!/usr/bin/env bash
exit 0
STUB
chmod 0755 "$baseline/install-assets.sh"
(cd "$baseline" && sha256sum install-assets.sh > install-assets.sh.sha256)
(
  cd "$baseline"
  sha256sum -- \
    common-infrastructure-linux-amd64.tar.gz \
    platform-backend-base-v1-linux-amd64.tar.gz \
    frontend-base-v1-linux-amd64.tar.gz \
    contract-backend-base-v1-linux-amd64.tar.gz \
    deployment-assets-base-v1.tar.gz \
    PLATFORM_ASSETS_COMPATIBILITY.env \
    install-assets.sh > SHA256SUMS
)

cat > "$tools/docker" <<'STUB'
#!/usr/bin/env bash
set -Eeuo pipefail
case "${1:-} ${2:-}" in
  'context inspect'|'buildx inspect') exit 0 ;;
  'buildx version') printf 'github.com/docker/buildx v0.test\n' ;;
  'buildx build')
    printf 'build:%s\n' "$*" >> "${STUB_WORK_ROOT:?}/docker-events"
    exit 0
    ;;
  tag\ *) exit 0 ;;
  'image inspect') printf 'amd64/linux\n' ;;
  'save --output')
    output_path="${3:?}"
    tag="${4:?}"
    root="$(mktemp -d "${STUB_WORK_ROOT:?}/oci.XXXXXX")"
    mkdir -p "$root/blobs/sha256"
    printf '{"architecture":"amd64","os":"linux","config":{"Cmd":["%s"]}}\n' "$tag" > "$root/config.json"
    config_digest="$(sha256sum "$root/config.json" | awk '{print $1}')"
    mv "$root/config.json" "$root/blobs/sha256/$config_digest"
    jq -cn --arg digest "sha256:$config_digest" \
      '{schemaVersion:2,mediaType:"application/vnd.oci.image.manifest.v1+json",config:{mediaType:"application/vnd.oci.image.config.v1+json",digest:$digest,size:1},layers:[]}' \
      > "$root/manifest.json"
    manifest_digest="$(sha256sum "$root/manifest.json" | awk '{print $1}')"
    mv "$root/manifest.json" "$root/blobs/sha256/$manifest_digest"
    jq -cn --arg digest "sha256:$manifest_digest" --arg tag "$tag" \
      '{schemaVersion:2,manifests:[{mediaType:"application/vnd.oci.image.manifest.v1+json",digest:$digest,size:1,annotations:{"org.opencontainers.image.ref.name":$tag}}]}' \
      > "$root/index.json"
    printf '{"imageLayoutVersion":"1.0.0"}\n' > "$root/oci-layout"
    tar -cf "$output_path" -C "$root" index.json oci-layout blobs
    rm -rf -- "$root"
    ;;
  'info --format') printf '%s\n' "${STUB_DOCKER_ROOT:?}" ;;
  'info ') exit 0 ;;
  *) printf 'unexpected docker invocation: %s\n' "$*" >&2; exit 1 ;;
esac
STUB
chmod 0755 "$tools/docker"

base_common_digest="$(sha256sum "$baseline/common-infrastructure-linux-amd64.tar.gz" | awk '{print $1}')"
base_platform_digest="$(sha256sum "$baseline/platform-backend-base-v1-linux-amd64.tar.gz" | awk '{print $1}')"
base_frontend_digest="$(sha256sum "$baseline/frontend-base-v1-linux-amd64.tar.gz" | awk '{print $1}')"
base_assets_digest="$(sha256sum "$baseline/deployment-assets-base-v1.tar.gz" | awk '{print $1}')"
base_compatibility_digest="$(sha256sum "$baseline/PLATFORM_ASSETS_COMPATIBILITY.env" | awk '{print $1}')"
base_manifest_digest="$(sha256sum "$baseline/SHA256SUMS" | awk '{print $1}')"

PATH="$tools:$PATH" STUB_WORK_ROOT="$fixture" STUB_DOCKER_ROOT="$docker_root" \
  bash "$wrapper" \
    --version extension-v1 \
    --output "$output" \
    --extend-from "$baseline" \
    --components customer-opportunity \
    --skip-host-deps \
    --no-pause > "$fixture/build.log"

[[ "$(wc -l < "$fixture/docker-events" | tr -d ' ')" == 1 ]] || fail 'extension rebuilt more than the selected CRM image'
grep -q 'customer-opportunity-backend:extension-v1' "$fixture/docker-events" || fail 'selected CRM image was not built'
if grep -Eq 'platform-backend|frontend|file-gateway|offline-base' "$fixture/docker-events"; then
  fail 'extension unexpectedly rebuilt a mandatory base image'
fi

[[ "$(sha256sum "$output/common-infrastructure-linux-amd64.tar.gz" | awk '{print $1}')" == "$base_common_digest" ]] || fail 'common package was not reused byte-for-byte'
[[ "$(sha256sum "$output/platform-backend-base-v1-linux-amd64.tar.gz" | awk '{print $1}')" == "$base_platform_digest" ]] || fail 'platform package was not reused byte-for-byte'
[[ "$(sha256sum "$output/frontend-base-v1-linux-amd64.tar.gz" | awk '{print $1}')" == "$base_frontend_digest" ]] || fail 'frontend package was not reused byte-for-byte'
[[ "$(sha256sum "$output/deployment-assets-base-v1.tar.gz" | awk '{print $1}')" == "$base_assets_digest" ]] || fail 'asset package was not reused byte-for-byte'
[[ "$(sha256sum "$output/PLATFORM_ASSETS_COMPATIBILITY.env" | awk '{print $1}')" == "$base_compatibility_digest" ]] || fail 'compatibility lock was not reused byte-for-byte'
[[ -f "$output/customer-opportunity-backend-extension-v1-linux-amd64.tar.gz" ]] || fail 'CRM extension package missing'
[[ ! -e "$output/contract-backend-base-v1-linux-amd64.tar.gz" ]] || fail 'unselected baseline business package leaked into extension delivery'
[[ ! -e "$output/deployment-assets-extension-v1.tar.gz" ]] || fail 'extension rebuilt deployment assets'
grep -Fxq "BASE_SHA256SUMS_SHA256=$base_manifest_digest" "$output/BASE_DELIVERY_INFO.txt" || fail 'baseline provenance digest missing'
grep -Fxq "PLATFORM_ASSETS_COMPATIBILITY_SHA256=$base_compatibility_digest" "$output/BASE_DELIVERY_INFO.txt" || fail 'compatibility lock provenance missing'
cmp -s "$repo_root/服务器一键离线部署.sh" "$output/服务器一键离线部署.sh" || fail 'latest server installer was not included'
cmp -s "$repo_root/系统运维工具.sh" "$output/系统运维工具.sh" || fail 'latest operations tool was not included'
(cd "$output" && sha256sum --check SHA256SUMS >/dev/null) || fail 'extension total checksum failed'
[[ -f "$fixture/uip-offline-delivery-extension-v1-linux-amd64.tar" ]] || fail 'extension outer archive missing'
(cd "$fixture" && sha256sum --check uip-offline-delivery-extension-v1-linux-amd64.tar.sha256 >/dev/null) || fail 'extension outer archive checksum failed'

if PATH="$tools:$PATH" STUB_WORK_ROOT="$fixture" STUB_DOCKER_ROOT="$docker_root" \
  bash "$wrapper" --version missing-components --output "$fixture/missing-components" \
    --extend-from "$baseline" --skip-host-deps --no-pause > "$fixture/missing-components.log" 2>&1; then
  fail '--extend-from accepted an implicit component selection'
fi
grep -q '必须同时显式指定 --components' "$fixture/missing-components.log" || fail 'missing component selection guidance absent'

PATH="$tools:$PATH" STUB_WORK_ROOT="$fixture" STUB_DOCKER_ROOT="$docker_root" \
  bash "$wrapper" --version compatible-data-analysis --output "$fixture/compatible-data-analysis" \
    --extend-from "$baseline" --components data-analysis --skip-host-deps --preflight-only --no-pause \
    > "$fixture/compatible-data-analysis.log"
grep -q '构建前检查通过' "$fixture/compatible-data-analysis.log" || fail 'compatible data-analysis baseline did not pass preflight'

cp -R "$baseline" "$fixture/legacy-data-analysis-base"
legacy_asset_stage="$(mktemp -d "$fixture/legacy-assets.XXXXXX")"
mkdir -p "$legacy_asset_stage/subsystems.d"
cat > "$legacy_asset_stage/subsystems.d/data-analysis-prod.yaml" <<'YAML'
version: 1
application:
  allowed_service_bindings: [contract_dashboard_read, project_dashboard_read]
runtime:
  files:
    - bindings: {}
YAML
COPYFILE_DISABLE=1 tar -czf "$fixture/legacy-data-analysis-base/deployment-assets-base-v1.tar.gz" \
  -C "$legacy_asset_stage" subsystems.d/data-analysis-prod.yaml
rm -rf -- "$legacy_asset_stage"
sed -i.bak \
  "s/^ASSETS_PACKAGE_SHA256=.*/ASSETS_PACKAGE_SHA256=$(sha256sum "$fixture/legacy-data-analysis-base/deployment-assets-base-v1.tar.gz" | awk '{print $1}')/" \
  "$fixture/legacy-data-analysis-base/PLATFORM_ASSETS_COMPATIBILITY.env"
rm -f -- "$fixture/legacy-data-analysis-base/PLATFORM_ASSETS_COMPATIBILITY.env.bak"
(
  cd "$fixture/legacy-data-analysis-base"
  sha256sum deployment-assets-base-v1.tar.gz > deployment-assets-base-v1.tar.gz.sha256
  sha256sum -- \
    common-infrastructure-linux-amd64.tar.gz \
    platform-backend-base-v1-linux-amd64.tar.gz \
    frontend-base-v1-linux-amd64.tar.gz \
    contract-backend-base-v1-linux-amd64.tar.gz \
    deployment-assets-base-v1.tar.gz \
    PLATFORM_ASSETS_COMPATIBILITY.env \
    install-assets.sh > SHA256SUMS
)
if PATH="$tools:$PATH" STUB_WORK_ROOT="$fixture" STUB_DOCKER_ROOT="$docker_root" \
  bash "$wrapper" --version legacy-data-analysis --output "$fixture/legacy-data-analysis-output" \
    --extend-from "$fixture/legacy-data-analysis-base" --components data-analysis \
    --skip-host-deps --preflight-only --no-pause > "$fixture/legacy-data-analysis.log" 2>&1; then
  fail 'extension accepted legacy data-analysis assets without File Gateway bindings'
fi
grep -Eq 'file_gateway_write|File Gateway 运行时绑定' "$fixture/legacy-data-analysis.log" ||
  fail 'legacy data-analysis asset guidance missing'
[[ ! -e "$fixture/legacy-data-analysis-output" ]] || fail 'incompatible data-analysis baseline created an output directory'

cp -R "$baseline" "$fixture/legacy-no-lock-base"
rm -f -- "$fixture/legacy-no-lock-base/PLATFORM_ASSETS_COMPATIBILITY.env"
(
  cd "$fixture/legacy-no-lock-base"
  sha256sum -- \
    common-infrastructure-linux-amd64.tar.gz \
    platform-backend-base-v1-linux-amd64.tar.gz \
    frontend-base-v1-linux-amd64.tar.gz \
    contract-backend-base-v1-linux-amd64.tar.gz \
    deployment-assets-base-v1.tar.gz \
    install-assets.sh > SHA256SUMS
)
if PATH="$tools:$PATH" STUB_WORK_ROOT="$fixture" STUB_DOCKER_ROOT="$docker_root" \
  bash "$wrapper" --version legacy-no-lock --output "$fixture/legacy-no-lock-output" \
    --extend-from "$fixture/legacy-no-lock-base" --components customer-opportunity \
    --skip-host-deps --preflight-only --no-pause > "$fixture/legacy-no-lock.log" 2>&1; then
  fail 'extension accepted a legacy baseline without a compatibility lock'
fi
grep -q 'PLATFORM_ASSETS_COMPATIBILITY.env' "$fixture/legacy-no-lock.log" ||
  fail 'missing compatibility lock guidance absent'
[[ ! -e "$fixture/legacy-no-lock-output" ]] || fail 'legacy baseline without lock created an output directory'

cp -R "$baseline" "$fixture/shared-input-drift-base"
sed -i.bak \
  's/^PLATFORM_ASSETS_INPUT_SHA256=.*/PLATFORM_ASSETS_INPUT_SHA256=0000000000000000000000000000000000000000000000000000000000000000/' \
  "$fixture/shared-input-drift-base/PLATFORM_ASSETS_COMPATIBILITY.env"
rm -f -- "$fixture/shared-input-drift-base/PLATFORM_ASSETS_COMPATIBILITY.env.bak"
(
  cd "$fixture/shared-input-drift-base"
  sha256sum -- \
    common-infrastructure-linux-amd64.tar.gz \
    platform-backend-base-v1-linux-amd64.tar.gz \
    frontend-base-v1-linux-amd64.tar.gz \
    contract-backend-base-v1-linux-amd64.tar.gz \
    deployment-assets-base-v1.tar.gz \
    PLATFORM_ASSETS_COMPATIBILITY.env \
    install-assets.sh > SHA256SUMS
)
if PATH="$tools:$PATH" STUB_WORK_ROOT="$fixture" STUB_DOCKER_ROOT="$docker_root" \
  bash "$wrapper" --version shared-input-drift --output "$fixture/shared-input-drift-output" \
    --extend-from "$fixture/shared-input-drift-base" --components customer-opportunity \
    --skip-host-deps --preflight-only --no-pause > "$fixture/shared-input-drift.log" 2>&1; then
  fail 'extension accepted a baseline from different shared platform/assets inputs'
fi
grep -q '共享部署输入已与基线不同' "$fixture/shared-input-drift.log" ||
  fail 'shared input drift guidance absent'
[[ ! -e "$fixture/shared-input-drift-output" ]] || fail 'shared-input drift created an output directory'

cp -R "$baseline" "$fixture/tampered-base"
printf 'tampered\n' >> "$fixture/tampered-base/platform-backend-base-v1-linux-amd64.tar.gz"
if PATH="$tools:$PATH" STUB_WORK_ROOT="$fixture" STUB_DOCKER_ROOT="$docker_root" \
  bash "$wrapper" --version tampered-base --output "$fixture/tampered-output" \
    --extend-from "$fixture/tampered-base" --components customer-opportunity \
    --skip-host-deps --no-pause > "$fixture/tampered.log" 2>&1; then
  fail 'extension accepted a tampered baseline delivery'
fi
grep -Eq 'FAILED|SHA256' "$fixture/tampered.log" || fail 'tampered baseline checksum failure missing'
[[ ! -e "$fixture/tampered-output" ]] || fail 'tampered baseline created an output directory'

: > "$fixture/docker-events"
full_output="$fixture/full-v1"
PATH="$tools:$PATH" STUB_WORK_ROOT="$fixture" STUB_DOCKER_ROOT="$docker_root" \
  bash "$wrapper" --version full-v1 --output "$full_output" \
    --components none --skip-host-deps --no-pause > "$fixture/full-v1.log"
[[ -f "$full_output/PLATFORM_ASSETS_COMPATIBILITY.env" ]] || fail 'full baseline compatibility lock missing'
grep -Fxq "PLATFORM_ASSETS_INPUT_SHA256=$(platform_assets_input_fingerprint)" \
  "$full_output/PLATFORM_ASSETS_COMPATIBILITY.env" || fail 'full baseline shared-input fingerprint incorrect'
grep -Eq '^PLATFORM_PACKAGE_SHA256=[a-f0-9]{64}$' "$full_output/PLATFORM_ASSETS_COMPATIBILITY.env" ||
  fail 'full baseline platform package digest missing'
grep -Eq '^ASSETS_PACKAGE_SHA256=[a-f0-9]{64}$' "$full_output/PLATFORM_ASSETS_COMPATIBILITY.env" ||
  fail 'full baseline assets package digest missing'
grep -Eq '^[a-f0-9]{64}  PLATFORM_ASSETS_COMPATIBILITY\.env$' "$full_output/SHA256SUMS" ||
  fail 'full baseline total manifest does not bind compatibility lock'
(cd "$full_output" && sha256sum --check SHA256SUMS >/dev/null) || fail 'full baseline total checksum failed'
[[ -f "$fixture/uip-offline-delivery-full-v1-linux-amd64.tar" ]] || fail 'full baseline outer archive missing'

printf 'one-click build extension tests passed\n'
