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
grep -Fxq 'APP_VERSION=pure-oci-test' "$fixture/build-args"

PATH="$fixture/tools:$PATH" STUB_WORK_ROOT="$fixture" \
  OFFLINE_GOPROXY='https://go.example.invalid|direct' OFFLINE_GOSUMDB='sum.example.invalid' \
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

# Only the trusted verifier process is replaced in this shell integration
# fixture. Its cryptographic behavior is covered by license-package Go tests;
# this checks transport, preflight ordering and per-customer output isolation.
cat > "$fixture/tools/go" <<'STUB'
#!/usr/bin/env bash
set -Eeuo pipefail
[[ "$PWD" == */platform && "$*" == 'run ./cmd/license-package verify --file '* ]] || exit 1
[[ "${STUB_LICENSE_VERIFY_OK:-false}" == true ]] || {
  echo 'fixture trusted verifier rejected the license' >&2
  exit 1
}
file="${5:?missing file}"
[[ -f "$file" && "$file" == /* ]] || exit 1
jq -cn --arg digest "$(sha256sum "$file" | awk '{print $1}')" --arg environment "${STUB_LICENSE_ENVIRONMENT:-production}" \
  '{customer_id:"fixture-customer",instance_id:"fixture-instance",environment:$environment,applications:["contract_management"],digest:$digest}'
STUB
chmod +x "$fixture/tools/go"
printf 'fixture.customer-a.signature\n' > "$fixture/customer-a.jws"
printf 'fixture.customer-b.signature\n' > "$fixture/customer-b.jws"
mkdir "$fixture/approvals"
printf '{"version":1,"fixture_transport_only":true}\n' > "$fixture/approvals/license-evidence.json"
printf '{"version":1,"application":"contract_management","environment":"prod","fixture_transport_only":true}\n' > "$fixture/approvals/runtime-license-contract_management-prod.json"
printf '{"candidate":true,"operator":"fixture-only"}\n' > "$fixture/approvals/approval-review.json"
printf 'DO-NOT-SHIP-FIXTURE-PRIVATE-KEY\n' > "$fixture/approvals/vendor-private.pem"
printf '{"unknown":true}\n' > "$fixture/approvals/unknown.json"
if PATH="$fixture/tools:$PATH" STUB_WORK_ROOT="$fixture" STUB_LICENSE_VERIFY_OK=true \
  bash "$builder" --component assets --version unapproved --output "$fixture/unapproved-output" \
  --license-file "$fixture/customer-a.jws" --runtime-approval-dir "$fixture/approvals" \
  >"$fixture/unapproved.log" 2>&1; then
  echo 'builder implicitly approved a candidate runtime bundle' >&2
  exit 1
fi
[[ ! -e "$fixture/unapproved-output" ]]
if PATH="$fixture/tools:$PATH" STUB_WORK_ROOT="$fixture" STUB_LICENSE_VERIFY_OK=true \
  bash "$builder" --component assets --version missing-approval --output "$fixture/missing-approval-output" \
  --license-file "$fixture/customer-a.jws" >"$fixture/missing-approval.log" 2>&1; then
  echo 'builder accepted an automatic license delivery without runtime approvals' >&2
  exit 1
fi
[[ ! -e "$fixture/missing-approval-output" ]]
if PATH="$fixture/tools:$PATH" STUB_WORK_ROOT="$fixture" STUB_LICENSE_VERIFY_OK=true STUB_LICENSE_ENVIRONMENT=prod \
  bash "$builder" --component assets --version wrong-environment --output "$fixture/wrong-environment-output" \
  --license-file "$fixture/customer-a.jws" --runtime-approval-dir "$fixture/approvals" --runtime-approval-approved \
  >"$fixture/wrong-environment.log" 2>&1; then
  echo 'builder confused installation environment prod with production' >&2
  exit 1
fi
[[ ! -e "$fixture/wrong-environment-output" ]]
if PATH="$fixture/tools:$PATH" STUB_WORK_ROOT="$fixture" \
  bash "$builder" --component assets --version invalid-license --output "$fixture/invalid-license-output" \
  --license-file "$fixture/customer-a.jws" --runtime-approval-dir "$fixture/approvals" --runtime-approval-approved \
  >"$fixture/invalid-license.log" 2>&1; then
  echo 'builder accepted a rejected license' >&2
  exit 1
fi
[[ ! -e "$fixture/invalid-license-output" ]] || {
  echo 'license preflight modified the output directory before verification' >&2
  exit 1
}
grep -q '未修改输出目录' "$fixture/invalid-license.log"
ln -s "$fixture/customer-a.jws" "$fixture/license-link.jws"
if PATH="$fixture/tools:$PATH" STUB_WORK_ROOT="$fixture" STUB_LICENSE_VERIFY_OK=true \
  bash "$builder" --component assets --version symlink-license --output "$fixture/symlink-output" \
  --license-file "$fixture/license-link.jws" --runtime-approval-dir "$fixture/approvals" --runtime-approval-approved \
  >"$fixture/symlink-license.log" 2>&1; then
  echo 'builder accepted a symbolic license file' >&2
  exit 1
fi
[[ ! -e "$fixture/symlink-output" ]]

PATH="$fixture/tools:$PATH" STUB_WORK_ROOT="$fixture" STUB_LICENSE_VERIFY_OK=true \
  bash "$builder" --component assets --version licensed-test --output "$fixture/licensed-output" \
  --license-file "$fixture/customer-a.jws" --runtime-approval-dir "$fixture/approvals" --runtime-approval-approved
licensed_package="$fixture/licensed-output/deployment-assets-licensed-test.tar.gz"
tar -xOf "$licensed_package" ./license/commercial-license.jws > "$fixture/packaged-license.jws"
cmp "$fixture/customer-a.jws" "$fixture/packaged-license.jws"
grep -Fxq "LICENSE_SHA256=$(sha256sum "$fixture/customer-a.jws" | awk '{print $1}')" "$fixture/licensed-output/BUILD_INFO.txt"
grep -Eq '^RUNTIME_APPROVAL_SHA256=[a-f0-9]{64}$' "$fixture/licensed-output/BUILD_INFO.txt"
tar -xOf "$licensed_package" ./license-evidence.json > "$fixture/packaged-evidence.json"
cmp "$fixture/approvals/license-evidence.json" "$fixture/packaged-evidence.json"
tar -xOf "$licensed_package" ./runtime-license-contract_management-prod.json > "$fixture/packaged-runtime.json"
cmp "$fixture/approvals/runtime-license-contract_management-prod.json" "$fixture/packaged-runtime.json"
if tar -tzf "$licensed_package" | grep -Eq '(unknown\.json|approval-review\.json|vendor-private\.pem)$'; then
  echo 'builder shipped an unreviewed file, internal review record or private key' >&2
  exit 1
fi
licensed_digest="$(sha256sum "$licensed_package" | awk '{print $1}')"
PATH="$fixture/tools:$PATH" STUB_WORK_ROOT="$fixture" STUB_LICENSE_VERIFY_OK=true \
  bash "$builder" --component assets --version licensed-test --output "$fixture/licensed-output" \
  --license-file "$fixture/customer-a.jws" --runtime-approval-dir "$fixture/approvals" --runtime-approval-approved
[[ "$(sha256sum "$licensed_package" | awk '{print $1}')" == "$licensed_digest" ]]
for changed_license in customer-b omitted; do
  license_args=()
  [[ "$changed_license" == omitted ]] || license_args=(--license-file "$fixture/customer-b.jws")
  if PATH="$fixture/tools:$PATH" STUB_WORK_ROOT="$fixture" STUB_LICENSE_VERIFY_OK=true \
    bash "$builder" --component assets --version licensed-test --output "$fixture/licensed-output" \
    --runtime-approval-dir "$fixture/approvals" --runtime-approval-approved \
    ${license_args[@]+"${license_args[@]}"} >"$fixture/$changed_license-license.log" 2>&1; then
    echo 'builder reused a customer release with changed or omitted license' >&2
    exit 1
  fi
  grep -q '拒绝混合构建' "$fixture/$changed_license-license.log"
  [[ "$(sha256sum "$licensed_package" | awk '{print $1}')" == "$licensed_digest" ]]
done
cp "$fixture/approvals/license-evidence.json" "$fixture/original-evidence.json"
printf '{"version":1,"fixture_transport_only":"changed"}\n' > "$fixture/approvals/license-evidence.json"
if PATH="$fixture/tools:$PATH" STUB_WORK_ROOT="$fixture" STUB_LICENSE_VERIFY_OK=true \
  bash "$builder" --component assets --version licensed-test --output "$fixture/licensed-output" \
  --license-file "$fixture/customer-a.jws" --runtime-approval-dir "$fixture/approvals" --runtime-approval-approved \
  >"$fixture/changed-approval.log" 2>&1; then
  echo 'builder reused different runtime approval bytes in the same customer release' >&2
  exit 1
fi
grep -q '拒绝混合构建' "$fixture/changed-approval.log"
[[ "$(sha256sum "$licensed_package" | awk '{print $1}')" == "$licensed_digest" ]]
mv "$fixture/original-evidence.json" "$fixture/approvals/license-evidence.json"
mv "$fixture/approvals/runtime-license-contract_management-prod.json" "$fixture/runtime-approval.json"
if PATH="$fixture/tools:$PATH" STUB_WORK_ROOT="$fixture" STUB_LICENSE_VERIFY_OK=true \
  bash "$builder" --component assets --version incomplete-approval --output "$fixture/incomplete-approval-output" \
  --license-file "$fixture/customer-a.jws" --runtime-approval-dir "$fixture/approvals" --runtime-approval-approved \
  >"$fixture/incomplete-approval.log" 2>&1; then
  echo 'builder accepted a license without its fixed runtime approval document' >&2
  exit 1
fi
[[ ! -e "$fixture/incomplete-approval-output" ]]
mv "$fixture/runtime-approval.json" "$fixture/approvals/runtime-license-contract_management-prod.json"
printf '[]\n' > "$fixture/approvals/license-evidence.json"
if PATH="$fixture/tools:$PATH" STUB_WORK_ROOT="$fixture" STUB_LICENSE_VERIFY_OK=true \
  bash "$builder" --component assets --version malformed-approval --output "$fixture/malformed-approval-output" \
  --license-file "$fixture/customer-a.jws" --runtime-approval-dir "$fixture/approvals" --runtime-approval-approved \
  >"$fixture/malformed-approval.log" 2>&1; then
  echo 'builder accepted an approval that is not one JSON object' >&2
  exit 1
fi
[[ ! -e "$fixture/malformed-approval-output" ]]
if tar -tzf "$licensed_package" | grep -Eq '\.(pem|key)$'; then
  echo 'customer licensed asset package unexpectedly includes a key' >&2
  exit 1
fi
PATH="$fixture/tools:$PATH" STUB_WORK_ROOT="$fixture" \
  bash "$builder" --component assets --version unlicensed-test --output "$fixture/unlicensed-output"
if tar -tzf "$fixture/unlicensed-output/deployment-assets-unlicensed-test.tar.gz" | grep -q './license/'; then
  echo 'legacy packaging unexpectedly inherited a customer license' >&2
  exit 1
fi
printf '{"version":1,"fixture_transport_only":true}\n' > "$fixture/approvals/license-evidence.json"
printf '{"version":1,"project":"fixture-old-installation","installation_boundary":true,"services":[],"excluded_services":[]}\n' > "$fixture/approvals/license-migration-evidence.json"
for scenario in fresh migrate platform-only expand; do
  PATH="$fixture/tools:$PATH" STUB_WORK_ROOT="$fixture" \
    bash "$builder" --component assets --version "scenario-$scenario" --output "$fixture/scenario-$scenario" \
    --installation-scenario "$scenario" --customer-id fixture-customer \
    --runtime-approval-dir "$fixture/approvals" --runtime-approval-approved
  tar -xOf "$fixture/scenario-$scenario/deployment-assets-scenario-$scenario.tar.gz" ./license-installation.json | \
    jq -e --arg scenario "$scenario" '.version == 1 and .scenario == $scenario and .customer_id == "fixture-customer"' >/dev/null
  tar -xOf "$fixture/scenario-$scenario/deployment-assets-scenario-$scenario.tar.gz" ./license-migration-evidence.json > "$fixture/old-evidence.json"
  cmp "$fixture/approvals/license-migration-evidence.json" "$fixture/old-evidence.json"
  grep -Eq '^INSTALLATION_PLAN_SHA256=[a-f0-9]{64}$' "$fixture/scenario-$scenario/BUILD_INFO.txt"
done
if PATH="$fixture/tools:$PATH" STUB_WORK_ROOT="$fixture" STUB_LICENSE_VERIFY_OK=true \
  bash "$builder" --component assets --version bad-customer --output "$fixture/bad-customer" \
  --installation-scenario fresh --customer-id another-customer --license-file "$fixture/customer-a.jws" \
  --runtime-approval-dir "$fixture/approvals" --runtime-approval-approved > "$fixture/customer-mismatch.log" 2>&1; then
  echo 'scenario customer mismatch accepted' >&2; exit 1
fi
[[ ! -e "$fixture/bad-customer" ]]
echo 'offline package builder OCI, dependencies, retry and per-customer license transport tests passed'
