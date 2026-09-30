#!/usr/bin/env bash
set -Eeuo pipefail

deploy_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
builder="$deploy_dir/bin/build-offline-packages.sh"
fixture="$(mktemp -d "${TMPDIR:-/tmp}/offline-package-builder-test.XXXXXX")"
trap 'rm -rf -- "$fixture"' EXIT
mkdir -p "$fixture/tools" "$fixture/output"

# macOS still ships Bash 3.2. Under `set -u`, an unbraced variable followed
# immediately by multibyte Chinese punctuation can be parsed as a longer,
# unset identifier. Keep this message braced because the local-image reuse
# branch is part of the supported macOS cross-build path.
grep -Fq '复用本地镜像（${image_platform}）：${image}' "$builder" || {
  echo 'local image reuse message is not safe under macOS Bash 3.2 nounset parsing' >&2
  exit 1
}

cat > "$fixture/tools/docker" <<'STUB'
#!/usr/bin/env bash
set -Eeuo pipefail
case "${1:-} ${2:-}" in
  'buildx version') exit 0 ;;
  'buildx build')
    printf '%s\n' "$@" > "$STUB_WORK_ROOT/build-args"
    exit 0
    ;;
  'image inspect')
    [[ "${STUB_PULL_FAILURE:-false}" != true ]] || exit 1
    printf 'amd64/linux\n'
    exit 0
    ;;
  'pull --platform')
    printf 'pull\n' >> "$STUB_WORK_ROOT/pull-calls"
    exit 1
    ;;
  'save --output')
    output="$3"
    tag="$4"
    root="$(mktemp -d "$STUB_WORK_ROOT/oci.XXXXXX")"
    mkdir -p "$root/blobs/sha256"
    printf '{"architecture":"amd64","os":"linux","config":{"Cmd":["%s"]}}\n' "${STUB_IMAGE_VARIANT:-run}" > "$root/config.json"
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
    tar -cf "$output" -C "$root" index.json oci-layout blobs
    rm -rf -- "$root"
    exit 0
    ;;
esac
printf 'unexpected docker stub invocation: %q\n' "$*" >&2
exit 1
STUB
chmod +x "$fixture/tools/docker"

if PATH="$fixture/tools:$PATH" STUB_WORK_ROOT="$fixture" OFFLINE_GOSUMDB=' OFF ' \
  bash "$builder" --component assets --version disabled-sumdb --output "$fixture/disabled-sumdb" \
  >"$fixture/disabled-sumdb.log" 2>&1; then
  echo 'builder allowed OFFLINE_GOSUMDB=off' >&2
  exit 1
fi
grep -q '不得关闭 Go 模块校验' "$fixture/disabled-sumdb.log"

PATH="$fixture/tools:$PATH" STUB_WORK_ROOT="$fixture" \
  OFFLINE_GOPROXY='https://go.example.invalid|direct' \
  OFFLINE_GOSUMDB='sum.example.invalid' \
  bash "$builder" --component platform --version pure-oci-test --output "$fixture/output"

platform_package="$fixture/output/platform-backend-pure-oci-test-linux-amd64.tar.gz"
[[ -f "$platform_package" ]]
cp "$fixture/output/BUILD_INFO.txt" "$fixture/build-info.expected"
printf 'SOURCE_INPUT_SHA256=%064d\n' 0 >> "$fixture/output/BUILD_INFO.txt"
if PATH="$fixture/tools:$PATH" STUB_WORK_ROOT="$fixture" \
  OFFLINE_GOPROXY='https://go.example.invalid|direct' OFFLINE_GOSUMDB='sum.example.invalid' \
  bash "$builder" --component platform --version pure-oci-test --output "$fixture/output" \
  >"$fixture/mixed-release.log" 2>&1; then
  echo 'builder accepted changed release-wide BUILD_INFO for an existing version' >&2
  exit 1
fi
grep -q '拒绝混合构建' "$fixture/mixed-release.log"
mv "$fixture/build-info.expected" "$fixture/output/BUILD_INFO.txt"
platform_package_digest="$(sha256sum "$platform_package" | awk '{print $1}')"
PATH="$fixture/tools:$PATH" STUB_WORK_ROOT="$fixture" \
  OFFLINE_GOPROXY='https://go.example.invalid|direct' OFFLINE_GOSUMDB='sum.example.invalid' \
  bash "$builder" --component platform --version pure-oci-test --output "$fixture/output"
[[ "$(sha256sum "$platform_package" | awk '{print $1}')" == "$platform_package_digest" ]] || {
  echo 'same-version equivalent rebuild replaced the already published package' >&2
  exit 1
}
if PATH="$fixture/tools:$PATH" STUB_WORK_ROOT="$fixture" STUB_IMAGE_VARIANT=changed \
  OFFLINE_GOPROXY='https://go.example.invalid|direct' OFFLINE_GOSUMDB='sum.example.invalid' \
  bash "$builder" --component platform --version pure-oci-test --output "$fixture/output" \
  >"$fixture/different-image.log" 2>&1; then
  echo 'same-version rebuild accepted a genuinely different image config digest' >&2
  exit 1
fi
grep -q '同名产物已存在且内容不同' "$fixture/different-image.log"
[[ "$(sha256sum "$platform_package" | awk '{print $1}')" == "$platform_package_digest" ]] || {
  echo 'rejected different image build damaged the published package' >&2
  exit 1
}
tar -xOf "$platform_package" package.env | grep -Eq '^IMAGE_CONFIG_DIGESTS=sha256:[a-f0-9]{64}$'
tar -xOf "$platform_package" images.tar > "$fixture/platform-images.tar"
if tar -tf "$fixture/platform-images.tar" | grep -Eq '(^|/)manifest\.json$'; then
  echo 'pure OCI builder regression unexpectedly contains Docker manifest.json' >&2
  exit 1
fi
grep -Fxq -- '--build-arg' "$fixture/build-args"
grep -Fxq 'GOPROXY=https://go.example.invalid|direct' "$fixture/build-args"
grep -Fxq 'GOSUMDB=sum.example.invalid' "$fixture/build-args"

PATH="$fixture/tools:$PATH" STUB_WORK_ROOT="$fixture" \
  OFFLINE_NPM_CONFIG_REGISTRY='https://npm.example.invalid' \
  bash "$builder" --component frontend --version pure-oci-test --output "$fixture/output"
grep -Fxq 'NPM_CONFIG_REGISTRY=https://npm.example.invalid' "$fixture/build-args"

rm -f "$fixture/pull-calls"
if PATH="$fixture/tools:$PATH" STUB_WORK_ROOT="$fixture" STUB_PULL_FAILURE=true \
  OFFLINE_PULL_RETRIES=2 OFFLINE_PULL_RETRY_DELAY_SECONDS=1 \
  bash "$builder" --component common --version pull-retry-test --output "$fixture/retry-output" \
  >"$fixture/retry.log" 2>&1; then
  echo 'builder accepted a public image after every bounded pull attempt failed' >&2
  exit 1
fi
[[ "$(wc -l < "$fixture/pull-calls" | tr -d ' ')" == 2 ]] || {
  cat "$fixture/retry.log" >&2
  echo 'public image pull did not honor OFFLINE_PULL_RETRIES=2' >&2
  exit 1
}
grep -q '已尝试 2 次' "$fixture/retry.log"
if find "$fixture" -type f \( -name '.build-info.*' -o -name '.build-info-check.*' \) -print -quit | grep -q .; then
  echo 'builder leaked BUILD_INFO temporary files after failure' >&2
  exit 1
fi

echo 'offline package builder pure OCI, dependency mirror and bounded pull retry tests passed'
