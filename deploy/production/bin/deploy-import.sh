#!/usr/bin/env bash
# Offline package import and image verification helpers. Sourced by deploy.sh.

verify_loaded_image_config_digest() (
  local tag="${1:?image tag required}" expected="${2:?expected config digest required}"
  local work_dir="${3:?work directory required}" sequence="${4:?sequence required}"
  local alias image_tar manifest_file manifest_member actual
  alias="uip-import-verify/${sequence}:local"
  image_tar="$work_dir/loaded-${sequence}.tar"
  manifest_file="$work_dir/loaded-${sequence}-manifest.json"

  cleanup_loaded_image_verification() {
    local status=$?
    trap - EXIT
    docker image rm "$alias" >/dev/null 2>&1 || true
    rm -f -- "$image_tar" "$manifest_file"
    exit "$status"
  }
  trap cleanup_loaded_image_verification EXIT

  docker tag "$tag" "$alias" >/dev/null || return 1
  docker image save --output "$image_tar" "$alias" >/dev/null || return 1
  if manifest_member="$(offline_archive_member "$image_tar" manifest.json 2>/dev/null)"; then
    tar -xOf "$image_tar" "$manifest_member" > "$manifest_file" || return 1
  elif tar -tf "$image_tar" | awk '$0 == "manifest.json" || $0 == "./manifest.json" {found++} END {exit found ? 0 : 1}'; then
    return 1
  else
    : > "$manifest_file"
  fi
  actual="$(offline_saved_image_config_id "$image_tar" "$manifest_file" "$alias")" || return 1
  [[ "$actual" == "$expected" ]]
)

# 包解压和镜像重新导出不要使用 /tmp。加固宿主机的 /tmp 通常是小型 tmpfs（验收主机为 1.7 GiB），
# 而公共 images.tar 与临时 docker-save 归档可能同时占用数 GiB。导入临时数据应存放在部署文件系统，安装器空间预算已覆盖；
# 操作人也可显式选择其他专用文件系统。
import_package() (
  local archive="${1:?package path required}" temporary import_temp_root manifest component version platform images image_ids expected_images tag local_name pushed digest record index actual_id expected_digest
  local record_temporary='' pointer_temporary=''
  local -a data_analysis_keys=(DATA_ANALYSIS_DASHBOARD_API_IMAGE DATA_ANALYSIS_AGGREGATION_WORKER_IMAGE DATA_ANALYSIS_ALERT_WORKER_IMAGE DATA_ANALYSIS_MIGRATE_IMAGE)
  local -a data_analysis_image_refs=()
  need docker; need tar; need gzip; need sha256sum; need awk; need curl; need jq
  # 分开判断三种情况：交付目录 iso/ 下的包不会自动进入部署目录的 packages/，
  # 这是现场最常见的漏步；旧写法用 -f 把“文件不存在”误报成“是符号链接”。
  [[ ! -L "$archive" ]] || die "镜像包不能是符号链接：$archive"
  if [[ ! -e "$archive" ]]; then
    die "镜像包不存在：$archive
交付目录 iso/ 下的镜像包需要先复制到部署目录的 packages/，再执行 import：
  sudo cp <交付目录>/iso/$(basename -- "$archive") $package_dir/
如包已放在其他位置，也可以直接 import 该实际路径，但需同时提供同名的 .sha256 伴随文件。"
  fi
  [[ -f "$archive" ]] || die "镜像包必须是普通文件：$archive"
  require_initialized
  need flock
  exec 8>"$deploy_dir/runtime/.deploy.lock"
  flock -w 60 8 || die '其他导入或发布任务正在运行'
  [[ ! -L "$archive.sha256" ]] || die "镜像包校验文件不能是符号链接：$archive.sha256"
  if [[ ! -f "$archive.sha256" ]]; then
    # 前置安装脚本已依据交付清单生成 packages/SHA256SUMS。后补上传的包只要登记在其中，
    # 就用清单里的摘要补出伴随文件：省掉手工步骤，且摘要仍来自审核清单，不降低信任级别。
    expected_digest="$(awk -v name="$(basename -- "$archive")" '$2 == name {print tolower($1)}' "$package_dir/SHA256SUMS" 2>/dev/null)"
    if [[ "$expected_digest" =~ ^[0-9a-f]{64}$ ]]; then
      printf '%s  %s\n' "$expected_digest" "$(basename -- "$archive")" > "$archive.sha256"
      chmod 600 "$archive.sha256"
      printf '已依据 packages/SHA256SUMS 生成伴随校验文件：%s\n' "$(basename -- "$archive").sha256"
    else
      die "镜像包缺少 SHA256 校验文件：$archive.sha256
该文件未登记在 packages/SHA256SUMS 中，无法自动补出。请在包所在目录执行：
  sha256sum $(basename -- "$archive") > $(basename -- "$archive").sha256"
    fi
  fi
  verify_archive_sidecar "$archive" "$archive.sha256" >/dev/null
  import_temp_root="$(prepare_import_temp_root)"
  temporary="$(mktemp -d "$import_temp_root/uip-import.XXXXXX")" || die "无法创建镜像导入临时工作目录：$import_temp_root"
  trap 'rm -r -- "$temporary"; [[ -z "$record_temporary" ]] || rm -f -- "$record_temporary"; [[ -z "$pointer_temporary" ]] || rm -f -- "$pointer_temporary"' EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM
  local listing details
  listing="$(tar -tzf "$archive")" || die '镜像包无法读取'
  details="$(tar -tvzf "$archive")" || die '镜像包目录无法校验'
  # 兼容旧版离线包中由 macOS COPYFILE 写入的三个 AppleDouble 元数据文件。
  # 仅放行已知 ._ 条目；任何其他额外路径仍按非法包拒绝。
  printf '%s\n' "$listing" | awk '
    $0 == "package.env" || $0 == "SHA256SUMS" || $0 == "images.tar" {seen[$0]++; next}
    $0 == "._package.env" || $0 == "._SHA256SUMS" || $0 == "._images.tar" {metadata[$0]++; next}
    {bad=1}
    END {
      exit bad || seen["package.env"] != 1 || seen["SHA256SUMS"] != 1 || seen["images.tar"] != 1 ||
        metadata["._package.env"] > 1 || metadata["._SHA256SUMS"] > 1 || metadata["._images.tar"] > 1
    }' || die '镜像包条目不符合格式'
  printf '%s\n' "$details" | awk 'substr($1,1,1) != "-" {bad=1} END {exit bad ? 1 : 0}' || die '镜像包包含链接或特殊文件'
  tar -xzf "$archive" --no-same-owner --no-same-permissions -C "$temporary"
  rm -f -- "$temporary/._package.env" "$temporary/._SHA256SUMS" "$temporary/._images.tar"
  awk 'NF != 2 || $2 != "images.tar" || length($1) != 64 || $1 ~ /[^0-9a-fA-F]/ {bad=1} END {exit bad || NR != 1}' "$temporary/SHA256SUMS" || die '镜像内容校验清单无效'
  (cd "$temporary" && sha256sum -c SHA256SUMS)
  manifest="$temporary/package.env"
  component="$(manifest_value "$manifest" COMPONENT)"; version="$(manifest_value "$manifest" VERSION)"
  platform="$(manifest_value "$manifest" PLATFORM)"; images="$(manifest_value "$manifest" IMAGES)"
  # 格式 2 记录 Docker save 配置摘要；仍接受短期存在的格式 1 构建器输出包中的 IMAGE_IDS。
  image_ids="$(manifest_value "$manifest" IMAGE_CONFIG_DIGESTS)"
  image_ids="${image_ids:-$(manifest_value "$manifest" IMAGE_IDS)}"
  [[ -n "$component" && -n "$version" && "$platform" == linux/amd64 && -n "$images" && -n "$image_ids" ]] || die "镜像包元数据不正确"
  [[ "$version" =~ ^[A-Za-z0-9._-]+$ ]] || die "镜像包版本格式不安全：$version"
  case "$component" in
    common) expected_images="mysql:8.4,quay.io/keycloak/keycloak:26.2,temporalio/auto-setup:1.29.7,metabase/metabase:v0.53.7,prom/prometheus:v3.5.0,prom/node-exporter:v1.9.1,registry:2.8.3,tecnativa/docker-socket-proxy:v0.5.0,uip-package/file-gateway:$version" ;;
    platform) expected_images="uip-package/platform-backend:$version" ;;
    frontend) expected_images="uip-package/frontend:$version" ;;
    customer-opportunity) expected_images="uip-package/customer-opportunity-backend:$version" ;;
    customer-portal) expected_images="uip-package/customer-portal-backend:$version" ;;
    contract) expected_images="uip-package/contract-backend:$version" ;;
    project) expected_images="uip-package/project-backend:$version" ;;
    settlement) expected_images="uip-package/settlement-backend:$version" ;;
    data-analysis) expected_images="uip-package/data-analysis-dashboard-api:$version,uip-package/data-analysis-aggregation-worker:$version,uip-package/data-analysis-alert-worker:$version,uip-package/data-analysis-production-migrate:$version" ;;
    *) die "镜像包组件未登记：$component" ;;
  esac
  [[ "$images" == "$expected_images" ]] || die "镜像名称与登记组件 $component 不匹配"
  IFS=, read -r -a image_tags <<< "$images"
  IFS=, read -r -a expected_ids <<< "$image_ids"
  [[ "${#image_tags[@]}" -eq "${#expected_ids[@]}" ]] || die '镜像名称数量与镜像 config digest 数量不一致'
  local proxy_upstream='' proxy_config='' pushed_proxy=''
  if [[ "$component" == common ]]; then
    proxy_upstream="$(manifest_optional_unique_value "$manifest" DOCKER_SOCKET_PROXY_UPSTREAM_REF)" || die '代理上游元数据字段重复或为空'
    proxy_config="$(manifest_optional_unique_value "$manifest" DOCKER_SOCKET_PROXY_CONFIG_DIGEST)" || die '代理配置摘要元数据字段重复或为空'
    if [[ -n "$proxy_upstream$proxy_config" ]]; then
      [[ "$proxy_upstream" == 'ghcr.io/tecnativa/docker-socket-proxy:v0.5.0@sha256:1f5038b54f06c3e18422902cf00ba21803d1c97805aae032e5e6673d532d3459' &&
         "$proxy_config" =~ ^sha256:[a-f0-9]{64}$ && "$proxy_config" == "${expected_ids[7]}" ]] ||
        die '公共包代理源说明与固定上游或归档配置摘要不一致'
    fi
  fi
  [[ -z "${2:-}" || "$component" == "$2" ]] || die "恢复要求 $2 镜像包，实际组件为 $component"
  # 待处理资产可能需要新版解析器二进制，此处只允许完整验证的控制面候选；门禁生效时业务导入仍关闭。
  if [[ -e "$control_plane_reload_marker" || -L "$control_plane_reload_marker" ]]; then
    [[ -f "$control_plane_reload_marker" && ! -L "$control_plane_reload_marker" ]] || die '控制面重载标记不安全'
    case "$component" in
      common|platform) ;;
      *) die '控制面升级未完成，禁止导入业务/前端镜像' ;;
    esac
  fi
  if ! docker load --input "$temporary/images.tar" >/dev/null; then
    local engine_version
    engine_version="$(docker version --format '{{.Server.Version}}' 2>/dev/null || true)"
    die "docker load 无法读取镜像包（目标 Docker Engine 版本：${engine_version:-未知}）。离线镜像包使用 OCI 镜像布局（blobs/sha256 + index.json + oci-layout），需要目标 Docker Engine 支持该布局，版本过低会在此处失败。请升级 Docker Engine，或先运行交付目录中的“服务器前置检查与安装脚本.sh”，用其中的镜像加载能力探测提前确认。"
  fi
  local identities_file="$temporary/image-identities.json" identity_probe_done=false identity_kind identity_status
  for index in "${!image_tags[@]}"; do
    tag="${image_tags[$index]}"
    [[ "$(docker image inspect "$tag" --format '{{.Architecture}}/{{.Os}}')" == amd64/linux ]] || die "镜像平台不是 linux/amd64：$tag"
    actual_id="$(docker image inspect "$tag" --format '{{.Id}}')"
    if [[ "$actual_id" != "${expected_ids[$index]}" ]]; then
      if [[ "$identity_probe_done" == false ]]; then
        identity_probe_done=true
        # 一次只读元数据遍历即可导出全部标签的已审核身份图；未安装可选辅助工具的旧安装保留 save 回退路径。
        if command -v python3 >/dev/null && [[ -f "$script_dir/offline-image-identities.py" && ! -L "$script_dir/offline-image-identities.py" ]]; then
          if python3 "$script_dir/offline-image-identities.py" --archive "$temporary/images.tar" --images "$images" --config-digests "$image_ids" > "$identities_file"; then
            :
          else
            identity_status=$?
            rm -f -- "$identities_file"
            [[ "$identity_status" == 2 ]] || die '镜像归档 OCI 元数据校验失败，拒绝使用重新导出掩盖不一致'
          fi
        fi
      fi
      if [[ -s "$identities_file" ]] && identity_kind="$(jq -er --arg tag "$tag" --arg actual "$actual_id" '
        [.images[] | select(.tag == $tag) | .identities[] |
          select(.digest == $actual and (.kind == "index" or .kind == "manifest")) | .kind]
        | if length == 1 then .[0] else error("loaded identity is not uniquely proved") end
      ' "$identities_file" 2>/dev/null)"; then
        case "$identity_kind" in index) identity_kind=索引 ;; manifest) identity_kind=清单 ;; esac
        printf '镜像校验通过（OCI %s与配置摘要对应）：%s\n' "$identity_kind" "$tag"
        continue
      fi
      printf '正在重新导出镜像以核验配置摘要：%s\n' "$tag"
      verify_loaded_image_config_digest "$tag" "${expected_ids[$index]}" "$temporary" "${component}-${index}" || {
        die "导入后的镜像配置摘要与清单不一致：${tag}（Docker 报告 ID：${actual_id}，清单 config digest：${expected_ids[$index]}）"
      }
      printf '镜像配置校验通过（重新导出）：%s\n' "$tag"
    else
      printf '镜像校验通过（配置摘要）：%s\n' "$tag"
    fi
  done
  if [[ "$component" != common ]]; then
    ensure_registry
    if [[ "$component" == data-analysis ]]; then
      [[ "${#image_tags[@]}" -eq 4 ]] || die "数据看板镜像包必须包含 API、聚合 Worker、告警 Worker 和迁移器四个镜像"
      for index in "${!image_tags[@]}"; do
        tag="${image_tags[$index]}"
        local_name="$registry_address/uip/${tag##*/}"
        docker tag "$tag" "$local_name"
        docker push "$local_name" >/dev/null
        pushed="$(docker image inspect "$local_name" --format '{{index .RepoDigests 0}}')"
        [[ "$pushed" == "$registry_address/"*"@sha256:"* ]] || die "本地镜像仓库没有返回不可变摘要：$local_name"
        data_analysis_image_refs+=("$pushed")
      done
      pushed="${data_analysis_image_refs[0]}"
      digest="${pushed#*@sha256:}"
    else
      [[ "${#image_tags[@]}" -eq 1 ]] || die "每个普通应用镜像包必须且只能包含一个镜像"
      tag="${image_tags[0]}"; local_name="$registry_address/uip/${component}:${version}"
      docker tag "$tag" "$local_name"
      docker push "$local_name" >/dev/null
      pushed="$(docker image inspect "$local_name" --format '{{index .RepoDigests 0}}')"
      [[ "$pushed" == "$registry_address/"*"@sha256:"* ]] || die "本地镜像仓库没有返回不可变摘要"
      digest="${pushed#*@sha256:}"
    fi
  else
    ensure_registry
    local gateway_tag="uip-package/file-gateway:$version" gateway_local pushed_gateway
    gateway_local="$registry_address/uip/file-gateway:$version"
    docker tag "$gateway_tag" "$gateway_local"
    docker push "$gateway_local" >/dev/null
    pushed_gateway="$(docker image inspect "$gateway_local" --format '{{index .RepoDigests 0}}')"
    [[ "$pushed_gateway" == "$registry_address/"*"@sha256:"* ]] || die '本地镜像仓库没有返回 File Gateway 不可变摘要'
    if [[ -n "$proxy_upstream" ]]; then
      local proxy_local="$registry_address/uip/docker-socket-proxy:$version"
      docker tag "${image_tags[7]}" "$proxy_local"
      docker push "$proxy_local" >/dev/null
      pushed_proxy="$(docker image inspect "$proxy_local" --format '{{index .RepoDigests 0}}')"
      [[ "$pushed_proxy" =~ ^[A-Za-z0-9.:-]+/uip/docker-socket-proxy@sha256:[a-f0-9]{64}$ && "$pushed_proxy" == "$registry_address/"* ]] || die '本地仓库没有返回代理镜像的唯一不可变引用'
    fi
    pushed="$images"; digest="-"
  fi
  if [[ "$component" == platform && -f "$control_plane_reload_marker" ]]; then
    local bound_image
    bound_image="$(awk -F= '$1=="PLATFORM_UPGRADE_IMAGE" {n++; v=substr($0,index($0,"=")+1)} END {if(n>1 || (n==1 && v==""))exit 1; print v}' "$control_plane_reload_marker")" || die '控制面候选绑定重复或为空'
    [[ -z "$bound_image" || "$bound_image" == "$pushed" ]] || die '待完成资产绑定其他平台镜像；拒绝覆盖恢复候选'
  fi
  local pointer
  record="$manifest_dir/${component}-${version}.imported"
  record_temporary="$(mktemp "$manifest_dir/.${component}-${version}.imported.XXXXXX")"
  {
    printf 'COMPONENT=%s\nVERSION=%s\nIMAGE_REF=%s\nSOURCE_IMAGE_IDS=%s\nIMAGE_DIGEST=%s\nPACKAGE_SHA256=%s\n' "$component" "$version" "$pushed" "$image_ids" "$digest" "$(sha256sum "$archive" | awk '{print $1}')"
    if [[ "$component" == common ]]; then
      printf 'FILE_GATEWAY_IMAGE_REF=%s\n' "$pushed_gateway"
      [[ -z "$pushed_proxy" ]] || printf 'DOCKER_SOCKET_PROXY_IMAGE_REF=%s\n' "$pushed_proxy"
    fi
    if [[ "$component" == data-analysis ]]; then
      for index in "${!data_analysis_keys[@]}"; do
        printf '%s=%s\n' "${data_analysis_keys[$index]}" "${data_analysis_image_refs[$index]}"
      done
    fi
  } > "$record_temporary"
  chmod 600 "$record_temporary"
  mv -f -- "$record_temporary" "$record"
  record_temporary=''
  pointer="$manifest_dir/${component}.latest"
  pointer_temporary="$(mktemp "$manifest_dir/.${component}.latest.XXXXXX")"
  printf '%s\n' "$(basename -- "$record")" > "$pointer_temporary"
  chmod 600 "$pointer_temporary"
  mv -f -- "$pointer_temporary" "$pointer"
  pointer_temporary=''
  printf '已导入组件：%s，版本：%s\n' "$component" "$version"
  # 把“下一步”写在成功输出里，避免 import 之后直接跳到平台页面而漏掉 prepare。
  case "$component" in
    platform|frontend)
      printf '下一步：./bin/deploy.sh deploy %s\n' "$component" ;;
    common)
      printf '下一步：继续导入 platform-backend 与 frontend 镜像包，再 deploy platform / deploy frontend\n' ;;
    *)
      printf '下一步（子系统必须执行，不可跳过）：./bin/deploy.sh prepare <子系统>\n'
      printf '  为什么：import 只把摘要登记到 manifests/；把不可变 digest 写入 .release.env 的是 prepare。\n'
      printf '  跳过 prepare 直接到平台页面采用/重试，会报 “production subsystem image must use an immutable digest”。\n'
      printf '  子系统参数：contract | project | settlement | data-analysis | customer-opportunity | customer-portal\n' ;;
  esac
)


latest_record() {
  local component="$1" record pointer selected='' selected_mtime='' mtime name
  local -a records=()
  pointer="$manifest_dir/${component}.latest"
  if [[ -e "$pointer" || -L "$pointer" ]]; then
    [[ -f "$pointer" && ! -L "$pointer" ]] || die "最新导入指针不是普通文件：$pointer"
    name="$(awk 'NF == 1 && NR == 1 {value=$1; next} {bad=1} END {if (bad || NR != 1) exit 1; print value}' "$pointer")" ||
      die "最新导入指针格式无效：$pointer"
    [[ "$name" == "$component"-*.imported && "$name" != */* ]] || die "最新导入指针包含非法记录名：$pointer"
    record="$manifest_dir/$name"
    [[ -f "$record" && ! -L "$record" ]] || die "最新导入指针指向的记录不存在或不安全：${record}；请重新 import 目标版本"
    [[ "$(env_get "$record" COMPONENT)" == "$component" ]] || die "最新导入指针与记录组件不一致：$record"
    printf '%s\n' "$record"
    return 0
  fi

  # 兼容 *.latest 引入前的安装：按导入记录修改时间选择，绝不按版本字符串排序。时间戳相同则存在歧义，必须拒绝，直到显式重新导入写入指针。
  shopt -s nullglob
  records=("$manifest_dir"/"$component"-*.imported)
  shopt -u nullglob
  for record in "${records[@]}"; do
    [[ -f "$record" && ! -L "$record" ]] || continue
    if mtime="$(stat -c '%Y' "$record" 2>/dev/null)"; then :; else mtime="$(stat -f '%m' "$record")"; fi
    if [[ -z "$selected" || "$mtime" -gt "$selected_mtime" ]]; then
      selected="$record"
      selected_mtime="$mtime"
    elif [[ "$mtime" -eq "$selected_mtime" ]]; then
      die "$component 存在多个同时间的旧导入记录，无法判断最后导入版本；请重新 import 需要使用的包"
    fi
  done
  [[ -n "$selected" ]] || die "没有找到已导入的 $component 镜像包；请先执行：$0 import packages/${component}-*-linux-amd64.tar.gz"
  printf '%s\n' "$selected"
}

latest_image() {
  env_get "$(latest_record "$1")" IMAGE_REF
}

imported_file_gateway_image() {
  local record ref
  record="$(latest_record common)"
  ref="$(env_get "$record" FILE_GATEWAY_IMAGE_REF)"
  [[ "$ref" =~ ^127\.0\.0\.1:[0-9]+/uip/file-gateway@sha256:[a-f0-9]{64}$ ]] || die '公共基础设施包缺少 File Gateway 不可变镜像摘要；请重新导入新版公共基础设施包'
  printf '%s\n' "$ref"
}

imported_docker_socket_proxy_image() {
  local record ref
  record="$(latest_record common)"
  ref="$(manifest_optional_unique_value "$record" DOCKER_SOCKET_PROXY_IMAGE_REF)" || die '公共包代理导入引用不唯一'
  [[ -n "$ref" ]] || return 0
  [[ "$ref" =~ ^127\.0\.0\.1:[0-9]+/uip/docker-socket-proxy@sha256:[a-f0-9]{64}$ ]] || die '公共包代理引用不是受控本地仓库不可变摘要'
  printf '%s\n' "$ref"
}

stage_data_analysis_images() {
  local record="${1:?import record required}" key image
  local -a keys=(DATA_ANALYSIS_DASHBOARD_API_IMAGE DATA_ANALYSIS_AGGREGATION_WORKER_IMAGE DATA_ANALYSIS_ALERT_WORKER_IMAGE DATA_ANALYSIS_MIGRATE_IMAGE)
  for key in "${keys[@]}"; do
    image="$(env_get "$record" "$key")"
    [[ "$image" =~ ^127\.0\.0\.1:[0-9]+/uip/[a-z0-9._/-]+@sha256:[a-f0-9]{64}$ ]] || die "离线数据看板导入记录缺少不可变镜像：${key}；请重新导入完整的数据看板镜像包"
    env_set "$release_file" "$key" "$image" || return 1
  done
  env_set "$release_file" DATA_ANALYSIS_IMAGE "$(env_get "$record" IMAGE_REF)"
}

stage_subsystem_release() (
  # EXIT 可能在 die 释放函数局部变量后运行；事务状态保留在隔离子 Shell 中，确保显式退出后也能回滚。
  component="$1" record="$2" original_release="$release_file"
  committed=false complete=false preserve_previous=false
  shift 2
  previous="$(mktemp "$deploy_dir/runtime/.release-previous.XXXXXX")" || return 1
  next="$(mktemp "$deploy_dir/runtime/.release-next.XXXXXX")" || { rm -f -- "$previous"; return 1; }
  rollback_staged_release() {
    local result=$?
    if [[ "$committed" == true && "$complete" != true ]]; then
      if mv -f -- "$previous" "$original_release"; then
        release_file="$original_release"
        (refresh_subsystem_provisioner) || { printf '错误：旧发行指针已恢复，控制面重新绑定仍需重试。\n' >&2; result=1; }
      else
        preserve_previous=true
        printf '错误：发行指针恢复失败，旧指针备份保留于 %s，请保留现场人工核对。\n' "$previous" >&2
        result=1
      fi
      [[ "$result" != 0 ]] || result=1
    fi
    [[ "$preserve_previous" == true ]] || rm -f -- "$previous"
    rm -f -- "$next"
    return "$result"
  }
  trap rollback_staged_release EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM
  cp -p -- "$original_release" "$previous" || return 1
  cp -p -- "$original_release" "$next" || return 1
  image="$(env_get "$record" IMAGE_REF)"
  [[ "$image" =~ ^[^[:space:]]+@sha256:[a-f0-9]{64}$ ]] || return 1
  subsystem_metadata "$component"
  release_file="$next"
  env_set "$release_file" "$subsystem_key" "$image" || return 1
  if [[ "$component" == data-analysis ]]; then
    stage_data_analysis_images "$record" || return 1
  elif [[ "$component" == customer-opportunity ]]; then
    key="$(customer_presale_build_id "$image")" || return 1
    env_set "$release_file" CUSTOMER_PRESALE_TEMPORAL_WORKER_BUILD_ID "$key" || return 1
  elif [[ "$component" == project ]]; then
    key="$(project_background_build_id "$image")" || return 1
    env_set "$release_file" PROJECT_BACKGROUND_BUILD_ID "$key" || return 1
  fi
  if [[ "$(id -u)" == 0 ]]; then chown --reference="$original_release" "$next" || return 1; fi
  chmod 600 "$next" || return 1
  mv -f -- "$next" "$original_release" || return 1
  release_file="$original_release"
  committed=true
  refresh_subsystem_provisioner || return 1
  if (($#)); then "$@" || return 1; fi
  complete=true
)


deploy_component() {
  local component="${1:?component required}" image file_gateway_image proxy_image bootstrap_password bootstrap_display bootstrap_account
  require_initialized
  scope_require "$component" || return
  image="$(latest_image "$component")"
  case "$component" in
    platform)
      file_gateway_image="$(imported_file_gateway_image)"
      proxy_image="$(imported_docker_socket_proxy_image)"
      # deploy-service 持有部署锁，并在同一个 .release.env 事务中提交两个镜像指针。不要在这里修改网关指针。
      if [[ -e "$control_plane_reload_marker" || -L "$control_plane_reload_marker" ]]; then
        [[ "${DEPLOY_PLATFORM_CONTROL_PLANE_UPGRADE:-false}" == true ]] || die '平台候选需显式协调升级；不能普通发布越过控制面门禁'
      fi
      if [[ -n "$proxy_image" ]]; then
        "$script_dir/deploy-service.sh" platform "$image" "$file_gateway_image" "$proxy_image"
      else
        "$script_dir/deploy-service.sh" platform "$image" "$file_gateway_image"
      fi
      bootstrap_password="$(env_get "$runtime_file" IAM_BOOTSTRAP_ADMIN_PASSWORD)"
      bootstrap_display="$(env_get "$runtime_file" IAM_BOOTSTRAP_ADMIN_DISPLAY_NAME)"
      bootstrap_account="$(env_get "$runtime_file" IAM_BOOTSTRAP_ADMIN_ACCOUNT_NAME)"
      [[ -n "$bootstrap_password" && -n "$bootstrap_display" && -n "$bootstrap_account" ]] || die "首个平台管理员配置不完整"
      printf '%s\n' "$bootstrap_password" | compose run -T --rm platform-migrate ./bootstrap-admin \
        --display-name "$bootstrap_display" --account-name "$bootstrap_account" --password-stdin
      echo '基础平台管理员已初始化或已经存在；受保护的初始凭据仍保存在权限为 0600 的运行时配置文件中，请首次登录后及时修改。'
      if scope_services | grep -Fxq prometheus; then
        compose_monitoring up -d prometheus keycloak-backup-metrics
      fi
      ;;
    frontend) "$script_dir/deploy-service.sh" frontend "$image" ;;
    *) die "业务子系统必须依次执行 prepare、基础平台探测接入、continue：$component" ;;
  esac
}

# .release.env 以单文件 bind mount 挂进 subsystem-provisioner。env_set 用 mv 做原子替换后，
# 宿主机 inode 变化，而容器仍绑定旧 inode，会一直读到替换前的占位镜像值 —— 表现为平台页面
# 反复报 “production subsystem image must use an immutable digest”，即使宿主机文件已经正确。
# 写完 .release.env 后必须让 Agent 重新绑定，否则 prepare/upgrade 的成果对 Agent 不可见。
# 如果 platform-api 已经运行，还必须成对重载：API 与 Agent 都在启动时缓存生产清单，
# 只重建一侧会触发 manifest drift 保护，导致受控采用/更新被正确拒绝。
