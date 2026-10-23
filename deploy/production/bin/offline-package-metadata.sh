#!/usr/bin/env bash

# 从 tar 归档返回一个确切成员名。OCI 生成器对是否添加 ./ 前缀处理不同；仅接受这两种写法，使元数据检查无需解压且能拒绝歧义。
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

# 为确切镜像引用解析 OCI 描述符。即使归档只有一个镜像，也必须有 reference-name 注解；静默选择唯一描述符无法证明内容属于哪个标签。
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

# 将 OCI 镜像索引描述符解析为 linux/amd64 镜像清单。此摘要标识序列化清单，并非 Docker 镜像 ID；调用方最终返回清单引用的配置摘要。
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

# Docker Desktop 和 containerd 的部分命令可能返回索引级摘要，
# docker image inspect .Id 也未必对应所选平台配置。因此包元数据记录 images.tar 中实际保存且已验证的配置摘要，
# 同时支持经典 Docker 归档
# （manifest.json + <config>.json）和 OCI 归档（index.json + blobs/sha256）
# 这两种格式；此处刻意不返回清单或索引摘要。
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
    # 命令替换会移除末尾换行，因此直接对归档原始字节计算哈希，不使用规范化后的 Shell 字符串。
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
