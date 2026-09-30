#!/usr/bin/env bash
set -Eeuo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
source "$script_dir/offline-package-metadata.sh"

test_root="$(mktemp -d "${TMPDIR:-/tmp}/offline-package-metadata-test.XXXXXX")"
trap 'rm -r -- "$test_root"' EXIT
mkdir -p "$test_root/blobs/sha256"
config_file="$test_root/blobs/sha256/config.json"
image_tar="$test_root/images.tar"
manifest_file="$test_root/manifest.json"

printf '{"architecture":"amd64","os":"linux","config":{"Cmd":["run"]}}\n' > "$config_file"
config_digest="$(sha256sum "$config_file" | awk '{print $1}')"
config_path="blobs/sha256/$config_digest"
mv "$config_file" "$test_root/$config_path"
jq -n --arg config "$config_path" --arg tag 'uip-package/test:offline' \
  '[{"Config":$config,"RepoTags":[$tag],"Layers":[]}]' > "$manifest_file"
tar -cf "$image_tar" -C "$test_root" manifest.json "$config_path"

actual="$(offline_saved_image_config_id "$image_tar" "$manifest_file" 'uip-package/test:offline')"
[[ "$actual" == "sha256:$config_digest" ]] || {
  echo "archive config digest mismatch: $actual" >&2
  exit 1
}
if offline_saved_image_config_id "$image_tar" "$manifest_file" 'uip-package/other:offline' >/dev/null 2>&1; then
  echo 'metadata helper accepted a tag not present in the image archive' >&2
  exit 1
fi

# classic graphdriver 布局：docker save 写 <config-sha>.json，而不是 OCI blobs/sha256/<sha>
classic_root="$test_root/classic"
mkdir -p "$classic_root"
classic_manifest="$classic_root/manifest.json"
classic_tar="$classic_root/images.tar"
classic_config="$classic_root/config.json"
printf '{"architecture":"amd64","os":"linux","config":{"Cmd":["run"]}}\n' > "$classic_config"
classic_digest="$(sha256sum "$classic_config" | awk '{print $1}')"
mv "$classic_config" "$classic_root/$classic_digest.json"
jq -n --arg config "$classic_digest.json" --arg tag 'uip-package/test:classic' \
  '[{"Config":$config,"RepoTags":[$tag],"Layers":[]}]' > "$classic_manifest"
tar -cf "$classic_tar" -C "$classic_root" manifest.json "$classic_digest.json"
classic_actual="$(offline_saved_image_config_id "$classic_tar" "$classic_manifest" 'uip-package/test:classic')"
[[ "$classic_actual" == "sha256:$classic_digest" ]] || {
  echo "classic archive config digest mismatch: $classic_actual" >&2
  exit 1
}

# A correctly hashed but wrong-platform config must not be labelled as an
# amd64 package merely because its archive/tag shape is valid.
wrong_platform_root="$test_root/wrong-platform"
mkdir -p "$wrong_platform_root"
printf '{"architecture":"arm64","os":"linux","config":{"Cmd":["run"]}}\n' > "$wrong_platform_root/config.json"
wrong_platform_digest="$(sha256sum "$wrong_platform_root/config.json" | awk '{print $1}')"
mv "$wrong_platform_root/config.json" "$wrong_platform_root/$wrong_platform_digest.json"
jq -n --arg config "$wrong_platform_digest.json" --arg tag 'uip-package/test:wrong-platform' \
  '[{"Config":$config,"RepoTags":[$tag],"Layers":[]}]' > "$wrong_platform_root/manifest.json"
tar -cf "$wrong_platform_root/images.tar" -C "$wrong_platform_root" manifest.json "$wrong_platform_digest.json"
if offline_saved_image_config_id "$wrong_platform_root/images.tar" "$wrong_platform_root/manifest.json" 'uip-package/test:wrong-platform' >/dev/null 2>&1; then
  echo 'metadata helper accepted a linux/arm64 config for a linux/amd64 package' >&2
  exit 1
fi

# 既不是 OCI 也不是 classic 布局时必须拒绝
unknown_root="$test_root/unknown"
mkdir -p "$unknown_root"
unknown_manifest="$unknown_root/manifest.json"
unknown_tar="$unknown_root/images.tar"
printf '{}' > "$unknown_root/config.json"
jq -n --arg config 'config.json' --arg tag 'uip-package/test:unknown' \
  '[{"Config":$config,"RepoTags":[$tag],"Layers":[]}]' > "$unknown_manifest"
tar -cf "$unknown_tar" -C "$unknown_root" manifest.json config.json
if offline_saved_image_config_id "$unknown_tar" "$unknown_manifest" 'uip-package/test:unknown' >/dev/null 2>&1; then
  echo 'metadata helper accepted an unknown image archive layout' >&2
  exit 1
fi

# Pure OCI archive: there is deliberately no Docker manifest.json. The top
# index identifies the requested tag and points to a multi-platform image
# index. The helper must select linux/amd64 and return the config digest, never
# either image-manifest or image-index digest.
oci_root="$test_root/oci"
mkdir -p "$oci_root/blobs/sha256"
oci_tar="$oci_root/images.tar"
oci_missing_docker_manifest="$oci_root/missing-manifest.json"
oci_tag='uip-package/test:oci'

printf '{"architecture":"amd64","os":"linux","config":{"Cmd":["amd64"]}}\n' > "$oci_root/amd64-config.json"
oci_amd64_config_digest="$(sha256sum "$oci_root/amd64-config.json" | awk '{print $1}')"
mv "$oci_root/amd64-config.json" "$oci_root/blobs/sha256/$oci_amd64_config_digest"
printf '{"architecture":"arm64","os":"linux","config":{"Cmd":["arm64"]}}\n' > "$oci_root/arm64-config.json"
oci_arm64_config_digest="$(sha256sum "$oci_root/arm64-config.json" | awk '{print $1}')"
mv "$oci_root/arm64-config.json" "$oci_root/blobs/sha256/$oci_arm64_config_digest"

jq -cn --arg digest "sha256:$oci_amd64_config_digest" '{schemaVersion:2,mediaType:"application/vnd.oci.image.manifest.v1+json",config:{mediaType:"application/vnd.oci.image.config.v1+json",digest:$digest,size:1},layers:[]}' > "$oci_root/amd64-manifest.json"
oci_amd64_manifest_digest="$(sha256sum "$oci_root/amd64-manifest.json" | awk '{print $1}')"
mv "$oci_root/amd64-manifest.json" "$oci_root/blobs/sha256/$oci_amd64_manifest_digest"
jq -cn --arg digest "sha256:$oci_arm64_config_digest" '{schemaVersion:2,mediaType:"application/vnd.oci.image.manifest.v1+json",config:{mediaType:"application/vnd.oci.image.config.v1+json",digest:$digest,size:1},layers:[]}' > "$oci_root/arm64-manifest.json"
oci_arm64_manifest_digest="$(sha256sum "$oci_root/arm64-manifest.json" | awk '{print $1}')"
mv "$oci_root/arm64-manifest.json" "$oci_root/blobs/sha256/$oci_arm64_manifest_digest"

jq -cn \
  --arg amd64 "sha256:$oci_amd64_manifest_digest" \
  --arg arm64 "sha256:$oci_arm64_manifest_digest" \
  '{schemaVersion:2,mediaType:"application/vnd.oci.image.index.v1+json",manifests:[
    {mediaType:"application/vnd.oci.image.manifest.v1+json",digest:$arm64,size:1,platform:{os:"linux",architecture:"arm64"}},
    {mediaType:"application/vnd.oci.image.manifest.v1+json",digest:$amd64,size:1,platform:{os:"linux",architecture:"amd64"}}
  ]}' > "$oci_root/platform-index.json"
oci_platform_index_digest="$(sha256sum "$oci_root/platform-index.json" | awk '{print $1}')"
mv "$oci_root/platform-index.json" "$oci_root/blobs/sha256/$oci_platform_index_digest"

jq -cn --arg digest "sha256:$oci_platform_index_digest" --arg tag "$oci_tag" \
  '{schemaVersion:2,manifests:[{mediaType:"application/vnd.oci.image.index.v1+json",digest:$digest,size:1,annotations:{"org.opencontainers.image.ref.name":$tag}}]}' > "$oci_root/index.json"
printf '{"imageLayoutVersion":"1.0.0"}\n' > "$oci_root/oci-layout"
tar -cf "$oci_tar" -C "$oci_root" index.json oci-layout blobs

oci_actual="$(offline_saved_image_config_id "$oci_tar" "$oci_missing_docker_manifest" "$oci_tag")"
[[ "$oci_actual" == "sha256:$oci_amd64_config_digest" ]] || {
  echo "OCI archive config digest mismatch: $oci_actual" >&2
  exit 1
}
[[ "$oci_actual" != "sha256:$oci_amd64_manifest_digest" && "$oci_actual" != "sha256:$oci_platform_index_digest" ]] || {
  echo 'OCI image config digest was confused with a manifest or index digest' >&2
  exit 1
}
if offline_saved_image_config_id "$oci_tar" "$oci_missing_docker_manifest" 'uip-package/test:wrong-tag' >/dev/null 2>&1; then
  echo 'OCI metadata helper accepted an unbound image reference' >&2
  exit 1
fi

# Every descriptor/config blob is content-addressed. Corruption must be
# rejected even if index.json still names the original digest.
corrupt_root="$test_root/oci-corrupt"
mkdir -p "$corrupt_root"
tar -xf "$oci_tar" -C "$corrupt_root"
printf '\ncorrupt\n' >> "$corrupt_root/blobs/sha256/$oci_amd64_config_digest"
corrupt_tar="$corrupt_root/images.tar"
tar -cf "$corrupt_tar" -C "$corrupt_root" index.json oci-layout blobs
if offline_saved_image_config_id "$corrupt_tar" "$oci_missing_docker_manifest" "$oci_tag" >/dev/null 2>&1; then
  echo 'OCI metadata helper accepted a corrupted config blob' >&2
  exit 1
fi

# Duplicate tag annotations are ambiguous and must not be resolved by array
# order.
ambiguous_root="$test_root/oci-ambiguous"
mkdir -p "$ambiguous_root"
tar -xf "$oci_tar" -C "$ambiguous_root"
jq '.manifests += [.manifests[0]]' "$ambiguous_root/index.json" > "$ambiguous_root/index.json.new"
mv "$ambiguous_root/index.json.new" "$ambiguous_root/index.json"
ambiguous_tar="$ambiguous_root/images.tar"
tar -cf "$ambiguous_tar" -C "$ambiguous_root" index.json oci-layout blobs
if offline_saved_image_config_id "$ambiguous_tar" "$oci_missing_docker_manifest" "$oci_tag" >/dev/null 2>&1; then
  echo 'OCI metadata helper accepted duplicate descriptors for one image reference' >&2
  exit 1
fi

# Builder integration: exercise the real package_images path with a Docker CLI
# fixture. This catches regressions where the builder requires manifest.json
# before the helper can inspect a pure OCI archive.
builder="$script_dir/build-offline-packages.sh"
fake_bin="$test_root/fake-bin"
mkdir -p "$fake_bin"
cat > "$fake_bin/docker" <<'EOF'
#!/usr/bin/env bash
set -eu
case "${1:-} ${2:-}" in
  'buildx version') printf 'github.com/docker/buildx v0.test\n' ;;
  'buildx build') exit 0 ;;
  'image inspect') printf 'amd64/linux\n' ;;
  'version --format') printf 'test-version\n' ;;
  'save --output')
    output="${3:?missing docker save output}"
    if [[ "${FAIL_SAVE:-0}" == 1 ]]; then
      printf 'partial docker archive\n' > "$output"
      exit 44
    fi
    cp "${FIXTURE_TAR:?missing archive fixture}" "$output"
    ;;
  *) printf 'unexpected fake Docker operation: %s\n' "$*" >&2; exit 97 ;;
esac
EOF
chmod +x "$fake_bin/docker"

make_classic_builder_fixture() {
  local root="$1" tag="$2" config digest
  mkdir -p "$root"
  config="$root/config.json"
  printf '{"architecture":"amd64","os":"linux","config":{"Cmd":["builder-classic"]}}\n' > "$config"
  digest="$(sha256sum "$config" | awk '{print $1}')"
  mv "$config" "$root/$digest.json"
  jq -n --arg config "$digest.json" --arg tag "$tag" '[{"Config":$config,"RepoTags":[$tag],"Layers":[]}]' > "$root/manifest.json"
  tar -cf "$root/images.tar" -C "$root" manifest.json "$digest.json"
  printf '%s\n' "$digest"
}

make_oci_builder_fixture() {
  local root="$1" tag="$2"
  mkdir -p "$root"
  tar -xf "$oci_tar" -C "$root"
  jq --arg tag "$tag" '.manifests[0].annotations["org.opencontainers.image.ref.name"]=$tag' "$root/index.json" > "$root/index.json.new"
  mv "$root/index.json.new" "$root/index.json"
  tar -cf "$root/images.tar" -C "$root" index.json oci-layout blobs
}

classic_builder_root="$test_root/builder-classic"
classic_builder_version='builder-classic'
classic_builder_tag="uip-package/platform-backend:$classic_builder_version"
classic_builder_digest="$(make_classic_builder_fixture "$classic_builder_root" "$classic_builder_tag")"
classic_output="$test_root/output-classic"
env PATH="$fake_bin:$PATH" FIXTURE_TAR="$classic_builder_root/images.tar" \
  bash "$builder" --component platform --version "$classic_builder_version" --output "$classic_output" >/dev/null
classic_package="$classic_output/platform-backend-$classic_builder_version-linux-amd64.tar.gz"
classic_package_id="$(tar -xOzf "$classic_package" package.env | awk -F= '$1 == "IMAGE_CONFIG_DIGESTS" {print $2}')"
[[ "$classic_package_id" == "sha256:$classic_builder_digest" ]] || {
  echo 'real builder recorded the wrong classic config digest' >&2
  exit 1
}
(cd "$classic_output" && sha256sum --check SHA256SUMS >/dev/null)

# Repeating byte-identical input is allowed, while the same release name with
# different image content must be rejected without changing the first package.
classic_package_before="$(sha256sum "$classic_package" | awk '{print $1}')"
env PATH="$fake_bin:$PATH" FIXTURE_TAR="$classic_builder_root/images.tar" \
  bash "$builder" --component platform --version "$classic_builder_version" --output "$classic_output" >/dev/null
[[ "$(sha256sum "$classic_package" | awk '{print $1}')" == "$classic_package_before" ]] || {
  echo 'byte-identical image package retry changed the published artifact' >&2
  exit 1
}
classic_conflict_root="$test_root/builder-classic-conflict"
mkdir -p "$classic_conflict_root"
printf '{"architecture":"amd64","os":"linux","config":{"Cmd":["different-content"]}}\n' > "$classic_conflict_root/config.json"
classic_conflict_digest="$(sha256sum "$classic_conflict_root/config.json" | awk '{print $1}')"
mv "$classic_conflict_root/config.json" "$classic_conflict_root/$classic_conflict_digest.json"
jq -n --arg config "$classic_conflict_digest.json" --arg tag "$classic_builder_tag" \
  '[{"Config":$config,"RepoTags":[$tag],"Layers":[]}]' > "$classic_conflict_root/manifest.json"
tar -cf "$classic_conflict_root/images.tar" -C "$classic_conflict_root" manifest.json "$classic_conflict_digest.json"
if env PATH="$fake_bin:$PATH" FIXTURE_TAR="$classic_conflict_root/images.tar" \
  bash "$builder" --component platform --version "$classic_builder_version" --output "$classic_output" >/dev/null 2>&1; then
  echo 'builder overwrote a different image package under the same release version' >&2
  exit 1
fi
[[ "$(sha256sum "$classic_package" | awk '{print $1}')" == "$classic_package_before" ]] || {
  echo 'same-version image conflict changed the published artifact' >&2
  exit 1
}

oci_builder_root="$test_root/builder-oci"
oci_builder_version='builder-oci'
oci_builder_tag="uip-package/platform-backend:$oci_builder_version"
make_oci_builder_fixture "$oci_builder_root" "$oci_builder_tag"
oci_output="$test_root/output-oci"
env PATH="$fake_bin:$PATH" FIXTURE_TAR="$oci_builder_root/images.tar" \
  bash "$builder" --component platform --version "$oci_builder_version" --output "$oci_output" >/dev/null
oci_package="$oci_output/platform-backend-$oci_builder_version-linux-amd64.tar.gz"
oci_package_id="$(tar -xOzf "$oci_package" package.env | awk -F= '$1 == "IMAGE_CONFIG_DIGESTS" {print $2}')"
[[ "$oci_package_id" == "sha256:$oci_amd64_config_digest" ]] || {
  echo 'real builder confused OCI config, manifest and index digests' >&2
  exit 1
}
(cd "$oci_output" && sha256sum --check SHA256SUMS >/dev/null)

# A partial docker save must not replace the last verified package or leak a
# publish-stage archive. Existing release bytes remain immutable.
oci_package_before="$(sha256sum "$oci_package" | awk '{print $1}')"
if env PATH="$fake_bin:$PATH" FIXTURE_TAR="$oci_builder_root/images.tar" FAIL_SAVE=1 \
  bash "$builder" --component platform --version "$oci_builder_version" --output "$oci_output" >/dev/null 2>&1; then
  echo 'builder accepted an interrupted docker save' >&2
  exit 1
fi
[[ "$(sha256sum "$oci_package" | awk '{print $1}')" == "$oci_package_before" ]] || {
  echo 'interrupted docker save changed an existing published package' >&2
  exit 1
}
if find "$oci_output" -maxdepth 1 \( -name '.*.archive.*' -o -name '.*.sidecar.*' \) -print -quit | grep -q .; then
  echo 'interrupted image package build leaked publish-stage files' >&2
  exit 1
fi

echo 'offline image package metadata tests passed'
