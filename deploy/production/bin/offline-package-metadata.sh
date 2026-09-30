#!/usr/bin/env bash

# Return one exact member name from a tar archive. OCI producers differ on
# whether names are prefixed with "./"; accepting only these two spellings
# keeps metadata inspection independent of extraction and rejects ambiguity.
offline_archive_member() {
  local image_tar="$1" expected="$2"
  tar -tf "$image_tar" | awk -v expected="$expected" '
    $0 == expected || $0 == "./" expected {member=$0; count++}
    END {if (count != 1) exit 1; print member}
  '
}

offline_archive_json() {
  local image_tar="$1" expected="$2" member
  member="$(offline_archive_member "$image_tar" "$expected")" || return 1
  tar -xOf "$image_tar" "$member"
}

offline_verify_sha256_blob() {
  local image_tar="$1" path="$2" expected_digest="$3" actual_digest
  [[ "$expected_digest" =~ ^sha256:([a-f0-9]{64})$ ]] || return 1
  actual_digest="$(offline_archive_json "$image_tar" "$path" | sha256sum | awk '{print tolower($1)}')" || return 1
  [[ "$actual_digest" == "${BASH_REMATCH[1]}" ]]
}

# Resolve an OCI descriptor for an exact image reference. The reference-name
# annotation is required even for a single-image archive: silently choosing the
# only descriptor would bind metadata to content without proving which tag it
# belongs to.
offline_oci_descriptor_for_ref() {
  local image_tar="$1" image_ref="$2" index_json
  index_json="$(offline_archive_json "$image_tar" index.json)" || return 1
  jq -cer --arg image "$image_ref" '
    [.manifests[]? | select(
      .annotations["org.opencontainers.image.ref.name"] == $image or
      .annotations["io.containerd.image.name"] == $image
    )]
    | if length == 1 then .[0] else error("image reference must occur exactly once") end
  ' <<<"$index_json"
}

# Resolve an OCI image-index descriptor to the linux/amd64 image manifest.
# This digest identifies the serialized manifest, not the Docker image ID. The
# caller ultimately returns the config digest referenced by that manifest.
offline_oci_image_manifest() {
  local image_tar="$1" descriptor_json="$2" descriptor_digest descriptor_path descriptor_body nested_count
  descriptor_digest="$(jq -er '.digest' <<<"$descriptor_json")" || return 1
  [[ "$descriptor_digest" =~ ^sha256:([a-f0-9]{64})$ ]] || return 1
  descriptor_path="blobs/sha256/${BASH_REMATCH[1]}"
  offline_verify_sha256_blob "$image_tar" "$descriptor_path" "$descriptor_digest" || return 1
  descriptor_body="$(offline_archive_json "$image_tar" "$descriptor_path")" || return 1

  if jq -e '.manifests | type == "array"' >/dev/null 2>&1 <<<"$descriptor_body"; then
    nested_count="$(jq -r '[.manifests[]? | select(.platform.os == "linux" and .platform.architecture == "amd64" and ((.platform.variant // "") == ""))] | length' <<<"$descriptor_body")" || return 1
    [[ "$nested_count" == 1 ]] || return 1
    descriptor_json="$(jq -cer '[.manifests[]? | select(.platform.os == "linux" and .platform.architecture == "amd64" and ((.platform.variant // "") == ""))][0]' <<<"$descriptor_body")" || return 1
    descriptor_digest="$(jq -er '.digest' <<<"$descriptor_json")" || return 1
    [[ "$descriptor_digest" =~ ^sha256:([a-f0-9]{64})$ ]] || return 1
    descriptor_path="blobs/sha256/${BASH_REMATCH[1]}"
    offline_verify_sha256_blob "$image_tar" "$descriptor_path" "$descriptor_digest" || return 1
    descriptor_body="$(offline_archive_json "$image_tar" "$descriptor_path")" || return 1
  fi

  jq -e '.config.digest | type == "string"' >/dev/null 2>&1 <<<"$descriptor_body" || return 1
  printf '%s\n' "$descriptor_body"
}

# Docker Desktop/containerd can report an index-level digest in some commands,
# while `docker image inspect .Id` after `docker load` identifies the selected
# platform config blob. Package metadata therefore records the verified config
# digest actually stored in images.tar. Both Docker classic archives
# (manifest.json + <config>.json) and OCI archives (index.json + blobs/sha256)
# are supported; manifest/index digests are deliberately never returned here.
offline_saved_image_config_id() {
  local image_tar="$1" manifest_file="$2" image_ref="$3"
  local config_path config_digest config_json actual_digest descriptor_json image_manifest

  if [[ -s "$manifest_file" ]] && jq -e 'type == "array"' "$manifest_file" >/dev/null 2>&1; then
    config_path="$(jq -er --arg image "$image_ref" '
      [.[] | select((.RepoTags // []) | index($image)) | .Config]
      | if length == 1 then .[0] else error("image tag must occur exactly once") end
    ' "$manifest_file")" || return 1
    if [[ "$config_path" =~ ^blobs/sha256/([a-f0-9]{64})$ ]]; then
      config_digest="${BASH_REMATCH[1]}"
    elif [[ "$config_path" =~ ^([a-f0-9]{64})\.json$ ]]; then
      config_digest="${BASH_REMATCH[1]}"
    else
      return 1
    fi
    config_json="$(offline_archive_json "$image_tar" "$config_path")" || return 1
    # Command substitution strips trailing newlines, so hash the archive bytes
    # directly rather than the normalized shell string.
    actual_digest="$(offline_archive_json "$image_tar" "$config_path" | sha256sum | awk '{print tolower($1)}')" || return 1
    [[ "$actual_digest" == "$config_digest" ]] || return 1
    jq -e '.os == "linux" and .architecture == "amd64"' >/dev/null 2>&1 <<<"$config_json" || return 1
    printf 'sha256:%s\n' "$config_digest"
    return 0
  fi

  descriptor_json="$(offline_oci_descriptor_for_ref "$image_tar" "$image_ref")" || return 1
  image_manifest="$(offline_oci_image_manifest "$image_tar" "$descriptor_json")" || return 1
  config_digest="$(jq -er '.config.digest' <<<"$image_manifest")" || return 1
  [[ "$config_digest" =~ ^sha256:([a-f0-9]{64})$ ]] || return 1
  config_path="blobs/sha256/${BASH_REMATCH[1]}"
  offline_verify_sha256_blob "$image_tar" "$config_path" "$config_digest" || return 1
  config_json="$(offline_archive_json "$image_tar" "$config_path")" || return 1
  jq -e '.os == "linux" and .architecture == "amd64"' >/dev/null 2>&1 <<<"$config_json" || return 1
  printf '%s\n' "$config_digest"
}
