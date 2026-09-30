#!/usr/bin/env bash
set -Eeuo pipefail
source "$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)/deploy.sh"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/offline-import-test.XXXXXX")"
trap 'rm -r -- "$test_root"' EXIT
deploy_dir="$test_root/deploy"; runtime_file="$deploy_dir/.env"; release_file="$deploy_dir/.release.env"
manifest_dir="$deploy_dir/manifests"
mkdir -p "$manifest_dir" "$deploy_dir/runtime" "$test_root/tmp" "$test_root/package"
touch "$runtime_file" "$release_file"; chmod 600 "$runtime_file" "$release_file"
export TMPDIR="$test_root/tmp"
export UIP_IMPORT_TMPDIR="$test_root/tmp"
stat() {
  if [[ "${1:-}" == -c && "${2:-}" == %a ]]; then printf '600\n'; else command stat "$@"; fi
}
flock() { return 0; }
# Network repair/readiness are covered independently; this suite isolates archives.
ensure_registry_bridge() { return 0; }
curl() { return 0; }
docker() {
  case "$*" in
    "load --input "*) echo loaded >> "$test_root/docker.log";;
    "image inspect registry:2.8.3") return 0;;
    "container inspect uip-offline-registry") return 0;;
    "inspect -f {{.State.Running}} uip-offline-registry") echo true;;
    *"{{index .RepoDigests 0}}"*) printf '127.0.0.1:5000/uip/file-gateway@sha256:%064d\n' 9;;
    *"{{.Server.Version}}"*) printf '%s\n' "${MOCK_DOCKER_VERSION:-29.0.1}";;
    *"{{.Architecture}}/{{.Os}}"*) echo amd64/linux;;
    *"{{.Id}}"*) printf 'sha256:%064d\n' "${MOCK_IMAGE_ID:-1}";;
    "tag "*|"push "*) echo "$*" >> "$test_root/docker.log";;
    "ps -q --filter label=com.docker.compose.service=subsystem-provisioner"|"ps -q --filter name=uip-subsystem-provisioner") return 0;;
    *) echo "unexpected Docker operation $*" >&2; return 97;;
  esac
}
load_count() { grep -c '^loaded$' "$test_root/docker.log" || true; }
printf 'test fixture, never sent to Docker\n' > "$test_root/package/images.tar"
(cd "$test_root/package" && sha256sum images.tar > SHA256SUMS)
tags='mysql:8.4,quay.io/keycloak/keycloak:26.2,temporalio/auto-setup:1.29.7,metabase/metabase:v0.53.7,prom/prometheus:v3.5.0,prom/node-exporter:v1.9.1,registry:2.8.3,tecnativa/docker-socket-proxy:v0.5.0,uip-package/file-gateway:test'
ids=""
for i in 1 2 3 4 5 6 7 8 9; do ids+="$(printf 'sha256:%064d' 1),"; done
printf 'PACKAGE_FORMAT=2\nCOMPONENT=common\nVERSION=test\nPLATFORM=linux/amd64\nIMAGES=%s\nIMAGE_CONFIG_DIGESTS=%s\n' "$tags" "${ids%,}" > "$test_root/package/package.env"
tar -czf "$test_root/common.tar.gz" -C "$test_root/package" package.env SHA256SUMS images.tar
(cd "$test_root" && sha256sum common.tar.gz > common.tar.gz.sha256)
import_package "$test_root/common.tar.gz"
[[ -f "$manifest_dir/common-test.imported" && "$(load_count)" -eq 1 ]]
[[ "$(cat "$manifest_dir/common.latest")" == common-test.imported ]] || { echo 'latest-import pointer was not committed' >&2; exit 1; }
grep -q '^SOURCE_IMAGE_IDS=sha256:' "$manifest_dir/common-test.imported" || { echo 'source image config IDs were not recorded' >&2; exit 1; }
grep -q '^FILE_GATEWAY_IMAGE_REF=127.0.0.1:5000/uip/file-gateway@sha256:' "$manifest_dir/common-test.imported" || { echo 'common package did not record file-gateway immutable digest' >&2; exit 1; }
if compgen -G "$TMPDIR/uip-import.*" >/dev/null; then echo 'temporary import directory leaked' >&2; exit 1; fi

# Import scratch data must honor the dedicated override instead of assuming
# that /tmp has enough capacity.  Production defaults to runtime/import-tmp.
[[ -d "$UIP_IMPORT_TMPDIR" && "$(find "$UIP_IMPORT_TMPDIR" -prune -type d -perm 0700 -print)" == "$UIP_IMPORT_TMPDIR" ]] || {
  echo 'dedicated import temp root was not created with mode 700' >&2; exit 1;
}

# A valid digest for another filename must not authorize the archive being
# imported, even when both files are present in the same directory.
cp "$test_root/common.tar.gz" "$test_root/wrong-name.tar.gz"
cp "$test_root/common.tar.gz.sha256" "$test_root/wrong-name.tar.gz.sha256"
if (import_package "$test_root/wrong-name.tar.gz") >"$test_root/wrong-name.log" 2>&1; then
  echo 'sidecar for another archive name was accepted' >&2; exit 1
fi
grep -q '只包含当前包' "$test_root/wrong-name.log" || { echo 'strict sidecar binding error missing' >&2; exit 1; }
[[ "$(load_count)" -eq 1 ]] || { echo 'Docker load ran before sidecar binding rejection' >&2; exit 1; }

# Selection follows the atomically committed last-import pointer, not lexical
# version ordering (where v2 can otherwise beat v10).
printf 'COMPONENT=platform\nVERSION=v2\nIMAGE_REF=test-v2\n' > "$manifest_dir/platform-v2.imported"
printf 'COMPONENT=platform\nVERSION=v10\nIMAGE_REF=test-v10\n' > "$manifest_dir/platform-v10.imported"
printf 'platform-v10.imported\n' > "$manifest_dir/platform.latest"
[[ "$(latest_record platform)" == "$manifest_dir/platform-v10.imported" ]] || { echo 'explicit latest pointer ignored' >&2; exit 1; }
printf 'platform-v2.imported\n' > "$manifest_dir/platform.latest"
[[ "$(latest_record platform)" == "$manifest_dir/platform-v2.imported" ]] || { echo 'last imported version was replaced by lexical ordering' >&2; exit 1; }

# 格式 1 为短暂发布过的兼容格式；导入器继续兼容。
sed 's/^PACKAGE_FORMAT=2$/PACKAGE_FORMAT=1/; s/^IMAGE_CONFIG_DIGESTS=/IMAGE_IDS=/' \
  "$test_root/package/package.env" > "$test_root/package/legacy.env"
mv "$test_root/package/legacy.env" "$test_root/package/package.env"
tar -czf "$test_root/legacy.tar.gz" -C "$test_root/package" package.env SHA256SUMS images.tar
(cd "$test_root" && sha256sum legacy.tar.gz > legacy.tar.gz.sha256)
import_package "$test_root/legacy.tar.gz" >"$test_root/legacy.log"
[[ "$(load_count)" -eq 2 ]] || { echo 'legacy package compatibility import did not load once' >&2; exit 1; }

# 恢复 v2 fixture，后续 AppleDouble/额外条目测试使用它。
sed 's/^PACKAGE_FORMAT=1$/PACKAGE_FORMAT=2/; s/^IMAGE_IDS=/IMAGE_CONFIG_DIGESTS=/' \
  "$test_root/package/package.env" > "$test_root/package/modern.env"
mv "$test_root/package/modern.env" "$test_root/package/package.env"
# 旧 macOS 交付包会包含这三个 AppleDouble 条目。它们可以丢弃，但不能把
# 对任意额外路径的校验一并放宽。
for metadata in package.env SHA256SUMS images.tar; do printf 'legacy metadata\n' > "$test_root/package/._$metadata"; done
tar -czf "$test_root/appledouble.tar.gz" -C "$test_root/package" \
  ._package.env package.env ._SHA256SUMS SHA256SUMS ._images.tar images.tar
(cd "$test_root" && sha256sum appledouble.tar.gz > appledouble.tar.gz.sha256)
import_package "$test_root/appledouble.tar.gz"
[[ "$(load_count)" -eq 3 ]]
printf 'unexpected\n' > "$test_root/package/unexpected"
tar -czf "$test_root/extra.tar.gz" -C "$test_root/package" package.env SHA256SUMS images.tar unexpected
(cd "$test_root" && sha256sum extra.tar.gz > extra.tar.gz.sha256)
if (import_package "$test_root/extra.tar.gz") >/dev/null 2>&1; then echo 'unexpected tar entry accepted' >&2; exit 1; fi
[[ "$(load_count)" -eq 3 ]]
printf 'broken archive\n' > "$test_root/broken.tar.gz"
(cd "$test_root" && sha256sum broken.tar.gz > broken.tar.gz.sha256)
if (import_package "$test_root/broken.tar.gz") >/dev/null 2>&1; then echo 'corrupt tar accepted' >&2; exit 1; fi
[[ "$(load_count)" -eq 3 ]]
if compgen -G "$TMPDIR/uip-import.*" >/dev/null; then echo 'failure cleanup leaked' >&2; exit 1; fi
mv "$test_root/package/images.tar" "$test_root/package/real-image"
ln -s real-image "$test_root/package/images.tar"
tar -czf "$test_root/link.tar.gz" -C "$test_root/package" package.env SHA256SUMS images.tar
(cd "$test_root" && sha256sum link.tar.gz > link.tar.gz.sha256)
if (import_package "$test_root/link.tar.gz") >/dev/null 2>&1; then echo 'link tar accepted' >&2; exit 1; fi
[[ "$(load_count)" -eq 3 ]]

# 现场最常见的漏步：包只放在交付目录的 iso/，没有复制到部署目录的 packages/。
# 必须报“不存在”并给出复制命令，不能误报成“是符号链接”。
if (import_package "$test_root/does-not-exist.tar.gz") >"$test_root/missing.log" 2>&1; then
  echo 'missing package accepted' >&2; exit 1
fi
grep -q '镜像包不存在' "$test_root/missing.log" || { echo 'missing package error is not explicit' >&2; cat "$test_root/missing.log" >&2; exit 1; }
if grep -q '符号链接' "$test_root/missing.log"; then
  echo 'missing package misreported as symlink' >&2; exit 1
fi
grep -q 'packages/' "$test_root/missing.log" || { echo 'missing package guidance absent' >&2; exit 1; }

# 真正的符号链接仍然必须被拒绝。
ln -sfn "$test_root/common.tar.gz" "$test_root/linked-package.tar.gz"
if (import_package "$test_root/linked-package.tar.gz") >"$test_root/symlink.log" 2>&1; then
  echo 'symlink package accepted' >&2; exit 1
fi
grep -q '不能是符号链接' "$test_root/symlink.log" || { echo 'symlink package error missing' >&2; exit 1; }

# 后补上传的包只带归档、没有伴随文件时，只要登记在安装脚本生成的 packages/SHA256SUMS 中，
# 就必须用清单里的摘要自动补出伴随文件，而不是让人手工处理。
package_dir="$deploy_dir/packages"
mkdir -p "$package_dir"
cp "$test_root/common.tar.gz" "$package_dir/common.tar.gz"
(cd "$package_dir" && sha256sum common.tar.gz) > "$package_dir/SHA256SUMS"
import_package "$package_dir/common.tar.gz" >/dev/null 2>&1 || { echo 'registered package without sidecar rejected' >&2; exit 1; }
[[ -f "$package_dir/common.tar.gz.sha256" ]] || { echo 'sidecar not derived from packages/SHA256SUMS' >&2; exit 1; }
grep -q '^[0-9a-f]\{64\}  common\.tar\.gz$' "$package_dir/common.tar.gz.sha256" || { echo 'derived sidecar format invalid' >&2; exit 1; }

# 既没有伴随文件、也没有登记在清单中的包必须拒绝并给出生成命令。
cp "$test_root/common.tar.gz" "$package_dir/unregistered.tar.gz"
if (import_package "$package_dir/unregistered.tar.gz") >"$test_root/nosidecar.log" 2>&1; then
  echo 'unregistered package without sidecar accepted' >&2; exit 1
fi
grep -q 'sha256sum' "$test_root/nosidecar.log" || { echo 'missing sidecar guidance absent' >&2; exit 1; }

# import 成功后必须打印下一步，避免漏掉子系统的 prepare。
if (import_package "$package_dir/common.tar.gz") >"$test_root/next-step.log" 2>&1; then :; fi
grep -q 'deploy platform' "$test_root/next-step.log" || { echo 'component next step guidance missing' >&2; exit 1; }

# A containerd-backed Engine may report an index digest from `.Id`.  The
# importer must accept that only when a strict re-export resolves to the config
# digest recorded by the package, and must still reject genuinely different
# loaded content.  The archive parser itself has real classic/OCI fixtures in
# test-offline-package-metadata.sh; this override isolates import orchestration.
verify_loaded_image_config_digest() {
  printf '%s|%s\n' "$1" "$2" >> "$test_root/reexport.log"
  [[ "${MOCK_REEXPORTED_CONFIG_VALID:-false}" == true ]]
}
MOCK_IMAGE_ID=2
MOCK_REEXPORTED_CONFIG_VALID=true
export MOCK_IMAGE_ID MOCK_REEXPORTED_CONFIG_VALID
import_package "$test_root/common.tar.gz" >"$test_root/index-id.log"
grep -q '已通过重新导出的归档校验：mysql:8.4' "$test_root/index-id.log" || {
  echo 'containerd index-ID fallback was not reported' >&2; cat "$test_root/index-id.log" >&2; exit 1;
}
[[ "$(wc -l < "$test_root/reexport.log" | tr -d ' ')" == 9 ]] || {
  echo 'not every mismatched loaded tag used strict config verification' >&2; exit 1;
}

MOCK_REEXPORTED_CONFIG_VALID=false
export MOCK_REEXPORTED_CONFIG_VALID
if import_package "$test_root/common.tar.gz" >"$test_root/config-mismatch.log" 2>&1; then
  echo 'genuinely different loaded image config was accepted' >&2; exit 1
fi
grep -q '导入后的镜像配置摘要与清单不一致' "$test_root/config-mismatch.log" || {
  echo 'strict config-digest mismatch error missing' >&2; cat "$test_root/config-mismatch.log" >&2; exit 1;
}
unset MOCK_IMAGE_ID
unset MOCK_REEXPORTED_CONFIG_VALID

echo 'offline package import validation and cleanup tests passed'
