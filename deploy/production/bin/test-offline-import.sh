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
# 网络修复和就绪检查分别覆盖，本套件专门隔离验证归档。
ensure_registry_bridge() { return 0; }
curl() { return 0; }
docker() {
  case "$*" in
    "load --input "*) echo loaded >> "$test_root/docker.log";;
    "image inspect registry:2.8.3") return 0;;
    "container inspect uip-offline-registry") return 0;;
    "inspect -f {{.State.Running}} uip-offline-registry") echo true;;
    *"{{index .RepoDigests 0}}"*)
      if [[ "$*" == *'/uip/docker-socket-proxy:'* ]]; then
        printf '127.0.0.1:5000/uip/docker-socket-proxy@sha256:%064d\n' 8
      else
        printf '127.0.0.1:5000/uip/file-gateway@sha256:%064d\n' 9
      fi;;
    *"{{.Server.Version}}"*) printf '%s\n' "${MOCK_DOCKER_VERSION:-29.0.1}";;
    *"{{.Architecture}}/{{.Os}}"*) echo amd64/linux;;
    *"{{.Id}}"*) if [[ -n "${MOCK_IMAGE_DIGEST:-}" ]]; then printf '%s\n' "$MOCK_IMAGE_DIGEST"; else printf 'sha256:%064d\n' "${MOCK_IMAGE_ID:-1}"; fi;;
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

# 导入临时数据必须遵守专用覆盖配置，不能假设 /tmp 容量足够；生产默认使用 runtime/import-tmp。
[[ -d "$UIP_IMPORT_TMPDIR" && "$(find "$UIP_IMPORT_TMPDIR" -prune -type d -perm 0700 -print)" == "$UIP_IMPORT_TMPDIR" ]] || {
  echo 'dedicated import temp root was not created with mode 700' >&2; exit 1;
}

# 即使两个文件位于同一目录，其他文件名的有效摘要也不能授权当前导入归档。
cp "$test_root/common.tar.gz" "$test_root/wrong-name.tar.gz"
cp "$test_root/common.tar.gz.sha256" "$test_root/wrong-name.tar.gz.sha256"
if (import_package "$test_root/wrong-name.tar.gz") >"$test_root/wrong-name.log" 2>&1; then
  echo 'sidecar for another archive name was accepted' >&2; exit 1
fi
grep -q '只包含当前包' "$test_root/wrong-name.log" || { echo 'strict sidecar binding error missing' >&2; exit 1; }
[[ "$(load_count)" -eq 1 ]] || { echo 'Docker load ran before sidecar binding rejection' >&2; exit 1; }

# 按原子提交的最后导入指针选择，不按版本字典序排列，避免 v2 被排在 v10 之后。
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

# 使用 containerd 的引擎可能让 .Id 返回索引摘要。只有严格重新导出能解析出包中记录的配置摘要时才接受，仍须拒绝不同的已加载内容。
# 归档解析器的真实经典及 OCI 夹具见 test-offline-package-metadata.sh；此覆盖只隔离导入编排。
mkdir -p "$test_root/classic-fallback"
printf '[]\n' > "$test_root/classic-fallback/manifest.json"
rm -f -- "$test_root/package/images.tar"
tar -cf "$test_root/package/images.tar" -C "$test_root/classic-fallback" manifest.json
(cd "$test_root/package" && sha256sum images.tar > SHA256SUMS)
tar -czf "$test_root/common-classic.tar.gz" -C "$test_root/package" package.env SHA256SUMS images.tar
(cd "$test_root" && sha256sum common-classic.tar.gz > common-classic.tar.gz.sha256)
verify_loaded_image_config_digest() {
  printf '%s|%s\n' "$1" "$2" >> "$test_root/reexport.log"
  [[ "${MOCK_REEXPORTED_CONFIG_VALID:-false}" == true ]]
}
MOCK_IMAGE_ID=2
MOCK_REEXPORTED_CONFIG_VALID=true
export MOCK_IMAGE_ID MOCK_REEXPORTED_CONFIG_VALID
import_package "$test_root/common-classic.tar.gz" >"$test_root/index-id.log"
grep -q '镜像配置校验通过（重新导出）：mysql:8.4' "$test_root/index-id.log" || {
  echo 'containerd index-ID fallback was not reported' >&2; cat "$test_root/index-id.log" >&2; exit 1;
}
[[ "$(wc -l < "$test_root/reexport.log" | tr -d ' ')" == 9 ]] || {
  echo 'not every mismatched loaded tag used strict config verification' >&2; exit 1;
}

MOCK_REEXPORTED_CONFIG_VALID=false
export MOCK_REEXPORTED_CONFIG_VALID
if import_package "$test_root/common-classic.tar.gz" >"$test_root/config-mismatch.log" 2>&1; then
  echo 'genuinely different loaded image config was accepted' >&2; exit 1
fi
grep -q '导入后的镜像配置摘要与清单不一致' "$test_root/config-mismatch.log" || {
  echo 'strict config-digest mismatch error missing' >&2; cat "$test_root/config-mismatch.log" >&2; exit 1;
}
unset MOCK_IMAGE_ID
unset MOCK_REEXPORTED_CONFIG_VALID

# 使用真实 OCI 图，在不执行 docker save 的情况下证明索引和所选清单身份；为九个已审核标签一次性导出元数据。
mkdir -p "$test_root/fast-package"
command python3 - "$test_root/fast-package" "$tags" <<'PYFIXTURE'
import hashlib,json,pathlib,sys,tarfile
root=pathlib.Path(sys.argv[1]);tags=sys.argv[2].split(',');blob=root/'blobs/sha256';blob.mkdir(parents=True)
def write(value,media):
 raw=json.dumps(value,separators=(',',':')).encode();digest=hashlib.sha256(raw).hexdigest();(blob/digest).write_bytes(raw)
 return {'digest':'sha256:'+digest,'size':len(raw),'mediaType':media}
config=write({'os':'linux','architecture':'amd64','config':{'Cmd':['isolated-OCI-fixture']}},'application/vnd.oci.image.config.v1+json')
manifest=write({'schemaVersion':2,'mediaType':'application/vnd.oci.image.manifest.v1+json','config':config,'layers':[]},'application/vnd.oci.image.manifest.v1+json')
child=dict(manifest,platform={'os':'linux','architecture':'amd64'})
index=write({'schemaVersion':2,'mediaType':'application/vnd.oci.image.index.v1+json','manifests':[child]},'application/vnd.oci.image.index.v1+json')
(root/'index.json').write_text(json.dumps({'schemaVersion':2,'manifests':[dict(index,annotations={'org.opencontainers.image.ref.name':tag}) for tag in tags]}))
(root/'oci-layout').write_text('{"imageLayoutVersion":"1.0.0"}')
(root/'expected-config').write_text(config['digest']);(root/'index-id').write_text(index['digest']);(root/'manifest-id').write_text(manifest['digest'])
(root/'package.env').write_text('PACKAGE_FORMAT=2\nCOMPONENT=common\nVERSION=test\nPLATFORM=linux/amd64\nIMAGES='+','.join(tags)+'\nIMAGE_CONFIG_DIGESTS='+','.join([config['digest']]*len(tags))+'\n')
with tarfile.open(root/'images.tar','w') as tar:
 for name in ['index.json','oci-layout','blobs']:tar.add(root/name,arcname=name)
PYFIXTURE
(cd "$test_root/fast-package" && sha256sum images.tar > SHA256SUMS)
tar -czf "$test_root/fast.tar.gz" -C "$test_root/fast-package" package.env SHA256SUMS images.tar
(cd "$test_root" && sha256sum fast.tar.gz > fast.tar.gz.sha256)
python3() { printf 'probe\n' >> "$test_root/probes.log"; command python3 "$@"; }
MOCK_IMAGE_DIGEST="$(cat "$test_root/fast-package/index-id")"
MOCK_REEXPORTED_CONFIG_VALID=false
: > "$test_root/reexport.log"; : > "$test_root/probes.log"
import_package "$test_root/fast.tar.gz" > "$test_root/fast-index.log"
[[ ! -s "$test_root/reexport.log" ]] || { echo 'proved index unnecessarily re-exported layers' >&2; exit 1; }
[[ "$(wc -l < "$test_root/probes.log" | tr -d ' ')" == 1 ]] || { echo 'OCI metadata probed more than once per package' >&2; exit 1; }
[[ "$(grep -c '镜像校验通过（OCI 索引与配置摘要对应）' "$test_root/fast-index.log")" == 9 ]]
MOCK_IMAGE_DIGEST="$(cat "$test_root/fast-package/manifest-id")"
: > "$test_root/probes.log"
import_package "$test_root/fast.tar.gz" > "$test_root/fast-manifest.log"
[[ ! -s "$test_root/reexport.log" ]]
[[ "$(grep -c '镜像校验通过（OCI 清单与配置摘要对应）' "$test_root/fast-manifest.log")" == 9 ]]

# 归档配置有效不代表可接受未知的已加载 ID；此时仍以现有严格重新导出结果为准。
MOCK_IMAGE_DIGEST="$(printf 'sha256:%064d' 7)"
if import_package "$test_root/fast.tar.gz" > "$test_root/unproved.log" 2>&1; then
  echo 'unproved loaded identity accepted without a valid re-export' >&2; exit 1
fi
grep -q '导入后的镜像配置摘要与清单不一致' "$test_root/unproved.log"
[[ -s "$test_root/reexport.log" ]]

# 已知无效的 OCI 元数据必须失败，不能通过重新导出被规范化掩盖，即使模拟旧回退会报告成功。
command python3 - "$test_root/fast-package/images.tar" "$test_root/fast-package/expected-config" <<'PYFIXTURE'
import io,pathlib,sys,tarfile
path=pathlib.Path(sys.argv[1]);digest=pathlib.Path(sys.argv[2]).read_text()[7:];members=[]
with tarfile.open(path,'r:') as tar:
 for item in tar.getmembers():
  raw=tar.extractfile(item).read() if item.isfile() else None
  if item.name=='blobs/sha256/'+digest:raw=b'{}';item.size=len(raw)
  members.append((item,raw))
with tarfile.open(path,'w') as tar:
 for item,raw in members:tar.addfile(item,io.BytesIO(raw) if raw is not None else None)
PYFIXTURE
(cd "$test_root/fast-package" && sha256sum images.tar > SHA256SUMS)
tar -czf "$test_root/corrupt-oci.tar.gz" -C "$test_root/fast-package" package.env SHA256SUMS images.tar
(cd "$test_root" && sha256sum corrupt-oci.tar.gz > corrupt-oci.tar.gz.sha256)
MOCK_IMAGE_DIGEST="$(cat "$test_root/fast-package/index-id")"; MOCK_REEXPORTED_CONFIG_VALID=true
: > "$test_root/reexport.log"
if import_package "$test_root/corrupt-oci.tar.gz" > "$test_root/invalid-oci.log" 2>&1; then
  echo 'invalid OCI metadata normalized by fallback' >&2; exit 1
fi
grep -q 'OCI 元数据校验失败' "$test_root/invalid-oci.log"
[[ ! -s "$test_root/reexport.log" ]] || { echo 'invalid metadata reached re-export fallback' >&2; exit 1; }
unset -f python3
unset MOCK_IMAGE_DIGEST MOCK_REEXPORTED_CONFIG_VALID
if compgen -G "$TMPDIR/uip-import.*" >/dev/null; then echo 'fast import cleanup leaked' >&2; exit 1; fi

# 新介质将固定上游绑定到保存的配置摘要；旧介质仍可导入，但不能静默声称符合新的固定代理契约。
[[ -z "$(imported_docker_socket_proxy_image)" ]] || { echo 'legacy common invented proxy identity' >&2; exit 1; }
proxy_upstream='ghcr.io/tecnativa/docker-socket-proxy:v0.5.0@sha256:1f5038b54f06c3e18422902cf00ba21803d1c97805aae032e5e6673d532d3459'
cp "$test_root/package/package.env" "$test_root/proxy-base.env"
printf 'DOCKER_SOCKET_PROXY_UPSTREAM_REF=%s\nDOCKER_SOCKET_PROXY_CONFIG_DIGEST=sha256:%064d\n' "$proxy_upstream" 1 >> "$test_root/package/package.env"
pack_proxy_fixture() {
  tar -czf "$test_root/proxy.tar.gz" -C "$test_root/package" package.env SHA256SUMS images.tar
  (cd "$test_root" && sha256sum proxy.tar.gz > proxy.tar.gz.sha256)
}
pack_proxy_fixture
import_package "$test_root/proxy.tar.gz" > "$test_root/proxy-success.log"
[[ "$(imported_docker_socket_proxy_image)" == "$(printf '127.0.0.1:5000/uip/docker-socket-proxy@sha256:%064d' 8)" ]]
grep -q '^push 127.0.0.1:5000/uip/docker-socket-proxy:test$' "$test_root/docker.log"
cp "$test_root/package/package.env" "$test_root/proxy-valid.env"
proxy_loads="$(load_count)"
for malformed in missing-config missing-source wrong-source wrong-config duplicate-source duplicate-config empty-source empty-config both-empty; do
  case "$malformed" in
    missing-config) sed '/^DOCKER_SOCKET_PROXY_CONFIG_DIGEST=/d' "$test_root/proxy-valid.env" > "$test_root/package/package.env" ;;
    missing-source) sed '/^DOCKER_SOCKET_PROXY_UPSTREAM_REF=/d' "$test_root/proxy-valid.env" > "$test_root/package/package.env" ;;
    wrong-source) sed 's#^DOCKER_SOCKET_PROXY_UPSTREAM_REF=.*#DOCKER_SOCKET_PROXY_UPSTREAM_REF=registry.invalid/proxy:latest#' "$test_root/proxy-valid.env" > "$test_root/package/package.env" ;;
    wrong-config) sed "s/^DOCKER_SOCKET_PROXY_CONFIG_DIGEST=.*/DOCKER_SOCKET_PROXY_CONFIG_DIGEST=$(printf 'sha256:%064d' 2)/" "$test_root/proxy-valid.env" > "$test_root/package/package.env" ;;
    duplicate-source) cp "$test_root/proxy-valid.env" "$test_root/package/package.env"; printf 'DOCKER_SOCKET_PROXY_UPSTREAM_REF=%s\n' "$proxy_upstream" >> "$test_root/package/package.env" ;;
    duplicate-config) cp "$test_root/proxy-valid.env" "$test_root/package/package.env"; printf 'DOCKER_SOCKET_PROXY_CONFIG_DIGEST=sha256:%064d\n' 1 >> "$test_root/package/package.env" ;;
    empty-source) sed 's/^DOCKER_SOCKET_PROXY_UPSTREAM_REF=.*/DOCKER_SOCKET_PROXY_UPSTREAM_REF=/' "$test_root/proxy-valid.env" > "$test_root/package/package.env" ;;
    empty-config) sed 's/^DOCKER_SOCKET_PROXY_CONFIG_DIGEST=.*/DOCKER_SOCKET_PROXY_CONFIG_DIGEST=/' "$test_root/proxy-valid.env" > "$test_root/package/package.env" ;;
    both-empty) sed 's/^DOCKER_SOCKET_PROXY_UPSTREAM_REF=.*/DOCKER_SOCKET_PROXY_UPSTREAM_REF=/; s/^DOCKER_SOCKET_PROXY_CONFIG_DIGEST=.*/DOCKER_SOCKET_PROXY_CONFIG_DIGEST=/' "$test_root/proxy-valid.env" > "$test_root/package/package.env" ;;
  esac
  pack_proxy_fixture
  if (import_package "$test_root/proxy.tar.gz") > "$test_root/proxy-${malformed}.log" 2>&1; then
    echo "malformed proxy metadata accepted: $malformed" >&2; exit 1
  fi
  [[ "$(load_count)" == "$proxy_loads" ]] || { echo 'invalid proxy metadata reached Docker load' >&2; exit 1; }
done

echo 'offline package import validation and cleanup tests passed'
