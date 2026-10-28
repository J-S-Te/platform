#!/usr/bin/env bash
set -Eeuo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../../../.." && pwd)"
wrapper="$repo_root/一键制作离线镜像包.sh"
fixture="$(mktemp -d "${TMPDIR:-/tmp}/oneclick-build-extension-test.XXXXXX")"
cleanup_fixture() {
  local status="$?" test_log
  if ((status != 0)); then
    for test_log in "$fixture"/*.log; do
      [[ -f "$test_log" ]] || continue
      printf '\nFixture log: %s\n' "$(basename -- "$test_log")" >&2
      tail -n 8 "$test_log" >&2
    done
  fi
  rm -rf -- "$fixture"
  return "$status"
}
trap cleanup_fixture EXIT

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

# 用途：生成包含真实 OCI 字节及独立注册镜像名的最小基线包。
# 参数：组件编码、基线版本及输出归档文件名。
# 返回值：成功返回零；夹具生成失败时终止测试。
make_baseline_image_package() {
  python3 -B - "$repo_root" "$baseline/$3" "$1" "$2" <<'PY'
import hashlib, importlib.util, pathlib, sys
root, output, component, version = sys.argv[1:]
path = pathlib.Path(root) / 'platform/deploy/production/tests/test-delivery-inspect.py'
spec = importlib.util.spec_from_file_location('extension_image_fixture', path)
fixture = importlib.util.module_from_spec(spec)
spec.loader.exec_module(fixture)
fixture.VERSION = version
raw, _ = fixture.image_package(component, image_version=version)
target = pathlib.Path(output)
target.write_bytes(raw)
target.with_name(target.name + '.sha256').write_text(
    hashlib.sha256(raw).hexdigest() + '  ' + target.name + '\n')
PY
}

make_baseline_image_package common base-v1 common-infrastructure-linux-amd64.tar.gz
make_baseline_image_package platform base-v1 platform-backend-base-v1-linux-amd64.tar.gz
make_baseline_image_package frontend base-v1 frontend-base-v1-linux-amd64.tar.gz
make_baseline_image_package contract base-v1 contract-backend-base-v1-linux-amd64.tar.gz

asset_stage="$(mktemp -d "$fixture/base-assets.XXXXXX")"
mkdir -p "$asset_stage/subsystems.d" "$asset_stage/bin"
cp "$repo_root/platform/deploy/production/subsystems.d/"*.yaml "$asset_stage/subsystems.d/"
for helper in deployment-workflow.sh deployment-session.py delivery-runtime.sh delivery-runtime.py \
  delivery-inspect.py delivery-inspect-images.py delivery-inspect-v2.py delivery-files.py \
  subsystem-runtime.py project-background.sh; do
  cp "$repo_root/platform/deploy/production/bin/$helper" "$asset_stage/bin/"
done
printf 'services: {}\n' > "$asset_stage/docker-compose.yml"
printf '#!/bin/sh\nexit 99\n' > "$asset_stage/bin/deploy.sh"
COPYFILE_DISABLE=1 tar -czf "$baseline/deployment-assets-base-v1.tar.gz" -C "$asset_stage" \
  subsystems.d bin docker-compose.yml
rm -rf -- "$asset_stage"
(cd "$baseline" && sha256sum deployment-assets-base-v1.tar.gz > deployment-assets-base-v1.tar.gz.sha256)

source "$repo_root/platform/deploy/production/bin/offline-build-inputs.sh"
platform_assets_input_fingerprint() {
  offline_build_input_fingerprint "$repo_root" shared
}

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
  'image inspect')
    case "$*" in
      *'{{.Id}}'*) printf 'sha256:%064d\n' 1 ;;
      *'{{.Os}}/{{.Architecture}}'*) printf 'linux/amd64\n' ;;
      *) printf 'amd64/linux\n' ;;
    esac
    ;;
  'save --output')
    output_path="${3:?}"
    root="$(mktemp -d "${STUB_WORK_ROOT:?}/oci.XXXXXX")"
    mkdir -p "$root/blobs/sha256"
    printf '{"schemaVersion":2,"manifests":[]}\n' > "$root/index.json"
    shift 3
    for tag in "$@"; do
    printf '{"architecture":"amd64","os":"linux","config":{"Cmd":["%s"],"Labels":{"org.opencontainers.image.version":"%s","com.basic-platform.license.protocol":"1"}}}\n' \
      "$tag" "${tag##*:}" > "$root/config.json"
    config_digest="$(sha256sum "$root/config.json" | awk '{print $1}')"
    config_size="$(wc -c < "$root/config.json" | tr -d ' ')"
    mv "$root/config.json" "$root/blobs/sha256/$config_digest"
    jq -cn --arg digest "sha256:$config_digest" --argjson size "$config_size" \
      '{schemaVersion:2,mediaType:"application/vnd.oci.image.manifest.v1+json",config:{mediaType:"application/vnd.oci.image.config.v1+json",digest:$digest,size:$size},layers:[]}' \
      > "$root/manifest.json"
    manifest_digest="$(sha256sum "$root/manifest.json" | awk '{print $1}')"
    manifest_size="$(wc -c < "$root/manifest.json" | tr -d ' ')"
    mv "$root/manifest.json" "$root/blobs/sha256/$manifest_digest"
    jq --arg digest "sha256:$manifest_digest" --arg tag "$tag" --argjson size "$manifest_size" \
      '.manifests += [{mediaType:"application/vnd.oci.image.manifest.v1+json",digest:$digest,size:$size,annotations:{"org.opencontainers.image.ref.name":$tag}}]' \
      "$root/index.json" > "$root/index-next.json"
    mv "$root/index-next.json" "$root/index.json"
    done
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
  bash "$wrapper" --candidate \
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
for sidecar in "$output"/*.sha256; do
  [[ "$(awk -v name="$(basename -- "$sidecar")" '$2 == name {n++} END {print n+0}' "$output/SHA256SUMS")" == 1 ]] ||
    fail "output sidecar not uniquely bound by total checksum: $(basename -- "$sidecar")"
done
[[ -f "$fixture/uip-offline-delivery-extension-v1-linux-amd64.tar" ]] || fail 'extension outer archive missing'
(cd "$fixture" && sha256sum --check uip-offline-delivery-extension-v1-linux-amd64.tar.sha256 >/dev/null) || fail 'extension outer archive checksum failed'

if PATH="$tools:$PATH" STUB_WORK_ROOT="$fixture" STUB_DOCKER_ROOT="$docker_root" \
  bash "$wrapper" --candidate --version missing-components --output "$fixture/missing-components" \
    --extend-from "$baseline" --skip-host-deps --no-pause > "$fixture/missing-components.log" 2>&1; then
  fail '--extend-from accepted an implicit component selection'
fi
grep -q '必须同时显式指定 --components' "$fixture/missing-components.log" || fail 'missing component selection guidance absent'

PATH="$tools:$PATH" STUB_WORK_ROOT="$fixture" STUB_DOCKER_ROOT="$docker_root" \
  bash "$wrapper" --candidate --version compatible-data-analysis --output "$fixture/compatible-data-analysis" \
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
  bash "$wrapper" --candidate --version legacy-data-analysis --output "$fixture/legacy-data-analysis-output" \
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
  bash "$wrapper" --candidate --version legacy-no-lock --output "$fixture/legacy-no-lock-output" \
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
  bash "$wrapper" --candidate --version shared-input-drift --output "$fixture/shared-input-drift-output" \
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
  bash "$wrapper" --candidate --version tampered-base --output "$fixture/tampered-output" \
    --extend-from "$fixture/tampered-base" --components customer-opportunity \
    --skip-host-deps --no-pause > "$fixture/tampered.log" 2>&1; then
  fail 'extension accepted a tampered baseline delivery'
fi
grep -Eq 'FAILED|SHA256' "$fixture/tampered.log" || fail 'tampered baseline checksum failure missing'
[[ ! -e "$fixture/tampered-output" ]] || fail 'tampered baseline created an output directory'

: > "$fixture/docker-events"
full_output="$fixture/full-v1"
PATH="$tools:$PATH" STUB_WORK_ROOT="$fixture" STUB_DOCKER_ROOT="$docker_root" \
  bash "$wrapper" --candidate --version full-v1 --output "$full_output" \
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

# Shell 夹具仅替换加密验证；生产仍调用已编译的可信验证器。此处验证真实包装器和构建器对各镜像组件及获许可扩展资产的绑定。
cat > "$tools/go" <<'STUB'
#!/usr/bin/env bash
set -Eeuo pipefail
[[ "$PWD" == */platform && "$*" == 'run ./cmd/license-package verify --file '* ]] || exit 1
[[ "${STUB_LICENSE_REJECT:-false}" != true ]] || exit 1
jq -cn --arg digest "$(sha256sum "${5:?}" | awk '{print $1}')" \
  '{customer_id:"fixture-customer",instance_id:"fixture-instance",environment:"production",applications:["contract_management"],digest:$digest}'
STUB
chmod +x "$tools/go"
mkdir -p "$fixture/approvals"
# 用途：为临时镜像生成结构完整的测试许可、运行批准和安装边界。
# 参数：本轮夹具镜像版本，用于绑定该镜像的真实配置摘要。
# 返回值：成功返回零；临时测试材料生成失败时终止测试。
prepare_licensed_fixture() {
  python3 -B - "$fixture" "$1" <<'PY'
import base64, hashlib, json, pathlib, subprocess, sys
root, version = pathlib.Path(sys.argv[1]), sys.argv[2]
tag = 'uip-package/contract-backend:' + version
config = {'architecture': 'amd64', 'os': 'linux', 'config': {'Cmd': [tag], 'Labels': {
    'org.opencontainers.image.version': version, 'com.basic-platform.license.protocol': '1'}}}
raw = (json.dumps(config, separators=(',', ':')) + '\n').encode()
digest = 'sha256:' + hashlib.sha256(raw).hexdigest()
services = ['contract-api', 'contract-worker']
approval = {'version': 1, 'application': 'contract_management', 'environment': 'prod',
            'components': [{'service_id': service, 'protocol': 1,
                            'coverage_digest': 'sha256:' + 'c' * 64, 'image_digest': digest}
                           for service in services]}
evidence = {'version': 1, 'project': 'fixture-project', 'installation_boundary': True,
            'services': [{'application': 'contract_management', 'environment': 'prod',
                          'name': service, 'image_digest': digest, 'version': version,
                          'protocol': '1', 'replicas': 1} for service in services],
            'excluded_services': []}
baseline = dict(evidence, services=[])
for name, value in [('runtime-license-contract_management-prod.json', approval),
                    ('license-evidence.json', evidence),
                    ('license-migration-evidence.json', baseline)]:
    (root / 'approvals' / name).write_text(json.dumps(value) + '\n')
header = {'alg': 'EdDSA', 'typ': 'commercial-license+jws', 'kid': 'fixture-only'}
claims = {'protocol_version': 1, 'issuer': 'unified-business-suite-vendor',
          'product_id': 'unified-business-suite', 'license_id': 'fixture-license',
          'customer_id': 'fixture-customer', 'instance_id': 'fixture-instance',
          'environment': 'production', 'version': 1, 'issued_at': 1700000000,
          'not_before': 1700000000, 'applications': [{'code': 'contract_management',
          'kind': 'FULL', 'not_before': 1700000000, 'expires_at': 2000000000}]}
segments = [base64.urlsafe_b64encode(json.dumps(value).encode()).rstrip(b'=')
            for value in (header, claims)]
signing_input = root / 'fixture-signing-input'
signing_input.write_bytes(b'.'.join(segments))
key = root / 'fixture-signing-key.pem'
if not key.exists():
    subprocess.run(['openssl', 'genpkey', '-algorithm', 'ED25519', '-out', str(key)], check=True)
signature = subprocess.run(['openssl', 'pkeyutl', '-sign', '-rawin', '-inkey', str(key),
                            '-in', str(signing_input)], check=True, capture_output=True).stdout
segments.append(base64.urlsafe_b64encode(signature).rstrip(b'='))
(root / 'license.jws').write_bytes(b'.'.join(segments))
PY
}
prepare_licensed_fixture licensed-full
license_args=(--license-file "$fixture/license.jws" --runtime-approval-dir "$fixture/approvals" \
  --runtime-approval-approved --installation-scenario fresh --customer-id fixture-customer)
licensed_full="$fixture/licensed-full"
PATH="$tools:$PATH" STUB_WORK_ROOT="$fixture" STUB_DOCKER_ROOT="$docker_root" \
  bash "$wrapper" --candidate --version licensed-full --output "$licensed_full" --components contract \
    --skip-host-deps --no-pause "${license_args[@]}" > "$fixture/licensed-full.log"
grep -Fxq "LICENSE_SHA256=$(sha256sum "$fixture/license.jws" | awk '{print $1}')" \
  "$licensed_full/BUILD_INFO.txt" || fail 'licensed components lost BUILD_INFO binding'
cmp -s "$fixture/license.jws" <(tar -xOf "$licensed_full/deployment-assets-licensed-full.tar.gz" ./license/commercial-license.jws) \
  || fail 'licensed full delivery missing exact signed bytes'
grep -Eq '^[a-f0-9]{64}  license-activate.sh$' "$licensed_full/SHA256SUMS" \
  || fail 'activation helper not bound by delivery manifest'
for helper in license-upgrade.sh forward-recover.sh; do
  [[ -x "$licensed_full/$helper" ]] || fail "delivery missing helper: $helper"
  grep -Fq "  $helper" "$licensed_full/SHA256SUMS" || fail "unbound helper: $helper"
done
(cd "$licensed_full" && sha256sum --check SHA256SUMS >/dev/null) || fail 'licensed full checksums failed'

licensed_extension="$fixture/licensed-extension"
prepare_licensed_fixture licensed-extension
PATH="$tools:$PATH" STUB_WORK_ROOT="$fixture" STUB_DOCKER_ROOT="$docker_root" \
  bash "$wrapper" --candidate --version licensed-extension --output "$licensed_extension" --extend-from "$baseline" \
    --components contract --skip-host-deps --no-pause "${license_args[@]}" > "$fixture/licensed-extension.log"
[[ ! -e "$licensed_extension/deployment-assets-base-v1.tar.gz" ]] || fail 'licensed extension inherited old customer assets'
[[ -f "$licensed_extension/deployment-assets-licensed-extension.tar.gz.sha256" ]] || fail 'new licensed asset sidecar missing'
grep -Fxq 'ASSETS_PACKAGE=deployment-assets-licensed-extension.tar.gz' "$licensed_extension/PLATFORM_ASSETS_COMPATIBILITY.env" \
  || fail 'licensed extension compatibility lock points to baseline assets'
grep -Fxq "ASSETS_PACKAGE_SHA256=$(sha256sum "$licensed_extension/deployment-assets-licensed-extension.tar.gz" | awk '{print $1}')" \
  "$licensed_extension/PLATFORM_ASSETS_COMPATIBILITY.env" || fail 'new compatibility digest incorrect'
grep -Fxq "BASE_SHA256SUMS_SHA256=$base_manifest_digest" "$licensed_extension/BASE_DELIVERY_INFO.txt" \
  || fail 'licensed extension lost source provenance'
cmp -s "$fixture/license.jws" <(tar -xOf "$licensed_extension/deployment-assets-licensed-extension.tar.gz" ./license/commercial-license.jws) \
  || fail 'licensed extension license missing'
(cd "$licensed_extension" && sha256sum --check SHA256SUMS >/dev/null) || fail 'licensed extension checksum failed'
if PATH="$tools:$PATH" STUB_WORK_ROOT="$fixture" STUB_DOCKER_ROOT="$docker_root" STUB_LICENSE_REJECT=true \
  bash "$wrapper" --candidate --version rejected-license --output "$fixture/rejected-license" --components contract \
    --skip-host-deps --no-pause "${license_args[@]}" > "$fixture/rejected-license.log" 2>&1; then
  fail 'wrapper accepted rejected license'
fi
[[ ! -e "$fixture/rejected-license" ]] || fail 'rejected license mutated release output'

# 旧版快捷路径执行真实包装器、构建器和归档逻辑，Docker 仍使用替身，因此这些发布只是夹具，绝不代表安装批准。
legacy_bridge="$fixture/legacy-bridge"
: > "$fixture/docker-events"
PATH="$tools:$PATH" STUB_WORK_ROOT="$fixture" STUB_DOCKER_ROOT="$docker_root" \
  bash "$wrapper" --candidate --legacy-license-bridge gays-sq --version legacy-bridge \
    --output "$legacy_bridge" --skip-host-deps --no-pause > "$fixture/legacy-bridge.log"
[[ ! -e "$legacy_bridge/customer-opportunity-backend-legacy-bridge-linux-amd64.tar.gz" ]] || fail 'bridge packaged CRM'
if grep -Eq 'customer-opportunity|contract-backend' "$fixture/docker-events"; then fail 'bridge built business images'; fi
if tar -tf "$legacy_bridge/deployment-assets-legacy-bridge.tar.gz" | grep -Eq 'license-installation.json|commercial-license.jws|license-migration-evidence.json'; then
  fail 'bridge forged migration or license assets'
fi
(cd "$legacy_bridge" && sha256sum --check SHA256SUMS >/dev/null) || fail 'bridge manifest failed'
grep -q '客户 gays-sq' "$fixture/legacy-bridge.log" || fail 'bridge lost initialization hint'

legacy_migrate="$fixture/legacy-migrate"
prepare_licensed_fixture legacy-migrate
PATH="$tools:$PATH" STUB_WORK_ROOT="$fixture" STUB_DOCKER_ROOT="$docker_root" \
  bash "$wrapper" --candidate --legacy-license-upgrade gays-sq --components contract --version legacy-migrate \
    --output "$legacy_migrate" --runtime-approval-dir "$fixture/approvals" \
    --runtime-approval-approved --skip-host-deps --no-pause > "$fixture/legacy-migrate.log"
tar -xOf "$legacy_migrate/deployment-assets-legacy-migrate.tar.gz" ./license-installation.json \
  | jq -e '.version == 1 and .scenario == "migrate" and .customer_id == "gays-sq"' >/dev/null || fail 'legacy shortcut lost migration binding'
cmp -s "$fixture/approvals/license-migration-evidence.json" \
  <(tar -xOf "$legacy_migrate/deployment-assets-legacy-migrate.tar.gz" ./license-migration-evidence.json) || fail 'legacy evidence changed'
if tar -tf "$legacy_migrate/deployment-assets-legacy-migrate.tar.gz" | grep -q commercial-license.jws; then fail 'migration fabricated JWS'; fi
grep -Eq '^INSTALLATION_PLAN_SHA256=[a-f0-9]{64}$' "$legacy_migrate/BUILD_INFO.txt" || fail 'migration inputs not bound'
(cd "$legacy_migrate" && sha256sum --check SHA256SUMS >/dev/null) || fail 'migration manifest failed'
(cd "$fixture" && sha256sum --check uip-offline-delivery-legacy-bridge-linux-amd64.tar.sha256 \
  uip-offline-delivery-legacy-migrate-linux-amd64.tar.sha256 >/dev/null) || fail 'legacy outer archive checksum failed'

legacy_expect_rejected() {
  local label="$1" expected="$2" before
  shift 2
  before="$(sha256sum "$fixture/docker-events" | awk '{print $1}')"
  if PATH="$tools:$PATH" STUB_WORK_ROOT="$fixture" STUB_DOCKER_ROOT="$docker_root" \
    bash "$wrapper" --candidate --version "$label" --output "$fixture/$label" --skip-host-deps \
      --preflight-only --no-pause "$@" > "$fixture/$label.log" 2>&1; then fail "legacy accepted $label"; fi
  grep -q "$expected" "$fixture/$label.log" || fail "legacy rejection guidance missing: $label"
  [[ ! -e "$fixture/$label" ]] || fail "legacy rejection created output: $label"
  [[ "$(sha256sum "$fixture/docker-events" | awk '{print $1}')" == "$before" ]] || fail "legacy rejection built images: $label"
}
legacy_expect_rejected bridge-business '只接受 --components none' --legacy-license-bridge gays-sq --components customer-opportunity
legacy_expect_rejected bridge-scenario 'installation-scenario 冲突' --legacy-license-bridge gays-sq --installation-scenario platform-only
legacy_expect_rejected bridge-license 'bridge 只制作兼容基础平台包' --legacy-license-bridge gays-sq --license-file "$fixture/license.jws"
legacy_expect_rejected bridge-approval 'bridge 只制作兼容基础平台包' --legacy-license-bridge gays-sq --runtime-approval-dir "$fixture/approvals"
legacy_expect_rejected legacy-extension '不支持 --extend-from' --legacy-license-upgrade gays-sq --extend-from "$baseline"
legacy_expect_rejected legacy-double '只能指定一次' --legacy-license-bridge gays-sq --legacy-license-upgrade gays-sq
legacy_expect_rejected legacy-customer '客户不一致' --legacy-license-upgrade gays-sq --customer-id different
legacy_expect_rejected legacy-invalid-id '有效客户标识' --legacy-license-upgrade 'invalid customer'
legacy_expect_rejected legacy-no-components '显式指定实际升级' --legacy-license-upgrade gays-sq
legacy_expect_rejected legacy-no-approval '暂无批准请先用' --legacy-license-upgrade gays-sq --components contract
legacy_expect_rejected legacy-no-confirm '暂无批准请先用' --legacy-license-upgrade gays-sq --components contract --runtime-approval-dir "$fixture/approvals"
legacy_test_args=(--legacy-license-upgrade gays-sq --components contract --runtime-approval-dir "$fixture/approvals" --runtime-approval-approved)
legacy_expect_rejected legacy-empty-business '至少需要一个业务子系统' --legacy-license-upgrade gays-sq --components none --runtime-approval-dir "$fixture/approvals" --runtime-approval-approved
legacy_expect_rejected legacy-missing-crm 'runtime-license-customer_and_opportunity-prod.json' --legacy-license-upgrade gays-sq --components customer-opportunity --runtime-approval-dir "$fixture/approvals" --runtime-approval-approved
mv "$fixture/approvals/license-migration-evidence.json" "$fixture/migration-evidence.saved"
legacy_expect_rejected legacy-no-evidence 'license-migration-evidence.json' "${legacy_test_args[@]}"
mv "$fixture/migration-evidence.saved" "$fixture/approvals/license-migration-evidence.json"
mv "$fixture/approvals/runtime-license-contract_management-prod.json" "$fixture/runtime-approval.saved"
ln -s "$fixture/runtime-approval.saved" "$fixture/approvals/runtime-license-contract_management-prod.json"
legacy_expect_rejected legacy-symlink '安全批准文件' "${legacy_test_args[@]}"
rm "$fixture/approvals/runtime-license-contract_management-prod.json"
printf '{"application":"wrong","environment":"prod"}\n' > "$fixture/approvals/runtime-license-contract_management-prod.json"
legacy_expect_rejected legacy-wrong-app '系统或环境' "${legacy_test_args[@]}"
printf '{bad json\n' > "$fixture/approvals/runtime-license-contract_management-prod.json"
legacy_expect_rejected legacy-bad-json '单个 JSON 对象' "${legacy_test_args[@]}"
mv "$fixture/runtime-approval.saved" "$fixture/approvals/runtime-license-contract_management-prod.json"
PATH="$tools:$PATH" STUB_WORK_ROOT="$fixture" STUB_DOCKER_ROOT="$docker_root" \
  bash "$wrapper" --candidate --version legacy-preflight --output "$fixture/legacy-preflight" \
    --skip-host-deps --preflight-only --no-pause "${legacy_test_args[@]}" > "$fixture/legacy-preflight.log"
[[ ! -e "$fixture/legacy-preflight" ]] || fail 'legacy preflight created output'
grep -q '构建前检查通过' "$fixture/legacy-preflight.log" || fail 'valid legacy preflight rejected'
printf 'one-click build extension and licensed binding tests passed\n'
