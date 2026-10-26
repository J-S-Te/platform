#!/usr/bin/env bash
# Deploy doctor/status checks. Sourced by deploy.sh.

doctor_ok() { printf '[OK] %s\n' "$*"; }
doctor_warn() { doctor_warnings=$((doctor_warnings + 1)); printf '[警告] %s\n' "$*"; }
doctor_fail() { doctor_failures=$((doctor_failures + 1)); printf '[失败] %s\n' "$*"; }

doctor_is_placeholder_image() {
  local value="$1"
  [[ -z "$value" || "$value" == *:pending || "$value" == *:latest || "$value" == uip-package/* ]]
}

doctor_release_keys() {
  case "$1" in
    platform) printf 'PLATFORM_IMAGE\n' ;;
    frontend) printf 'FRONTEND_IMAGE\n' ;;
    customer-opportunity) printf 'CUSTOMER_CRM_IMAGE\n' ;;
    customer-portal) printf 'CUSTOMER_PORTAL_IMAGE\n' ;;
    contract) printf 'CONTRACT_IMAGE\n' ;;
    project) printf 'PROJECT_IMAGE\n' ;;
    settlement) printf 'SETTLEMENT_IMAGE\n' ;;
    data-analysis)
      printf '%s\n' DATA_ANALYSIS_IMAGE DATA_ANALYSIS_DASHBOARD_API_IMAGE \
        DATA_ANALYSIS_AGGREGATION_WORKER_IMAGE DATA_ANALYSIS_ALERT_WORKER_IMAGE DATA_ANALYSIS_MIGRATE_IMAGE
      ;;
    *) return 1 ;;
  esac
}

doctor_check_config_files() {
  printf '\n[1/6] 运行配置文件存在性与权限\n'
  local file label mode
  for file in "$runtime_file" "$release_file"; do
    label="${file##*/}"
    if [[ -L "$file" ]]; then
      doctor_fail "$label 是符号链接：$file"
      continue
    fi
    if [[ ! -e "$file" ]]; then
      doctor_fail "$label 不存在：${file}；请先执行：$0 configure"
      continue
    fi
    if [[ ! -f "$file" ]]; then
      doctor_fail "$label 不是普通文件：$file"
      continue
    fi
    mode="$(stat -c '%a' "$file" 2>/dev/null || true)"
    if [[ "$mode" == 600 ]]; then
      doctor_ok "$label 存在且权限为 0600"
    else
      doctor_fail "$label 权限为 ${mode:-未知}，必须为 0600：chmod 600 $file"
    fi
  done
}

doctor_check_packages() {
  printf '\n[2/6] packages/ 镜像包伴随校验文件\n'
  shopt -s nullglob
  local archives=("$package_dir"/*-linux-amd64.tar.gz)
  shopt -u nullglob
  if ((${#archives[@]} == 0)); then
    doctor_warn "未在 $package_dir 找到 *-linux-amd64.tar.gz；确认交付目录 iso/ 里的镜像包已复制到部署目录 packages/（见 OFFLINE_DEPLOYMENT.md 0.4）"
    return 0
  fi
  local archive sidecar base
  for archive in "${archives[@]}"; do
    base="$(basename -- "$archive")"
    sidecar="$archive.sha256"
    if [[ -L "$sidecar" ]]; then
      doctor_fail "校验伴随文件是符号链接：$sidecar"
      continue
    fi
    if [[ ! -f "$sidecar" ]]; then
      doctor_fail "缺少校验伴随文件：${sidecar}；请执行：(cd $(dirname -- "$archive") && sha256sum $base > $base.sha256)"
      continue
    fi
    if (verify_archive_sidecar "$archive" "$sidecar" >/dev/null 2>&1); then
      doctor_ok "$base 摘要校验通过"
    else
      doctor_fail "$base 摘要或伴随文件绑定失败；请重新复制当前包及与其同名的 .sha256 后重试"
    fi
  done
}

doctor_check_manifests() {
  printf '\n[3/6] manifests/ 导入记录与 .release.env 不可变引用\n'
  shopt -s nullglob
  local records=("$manifest_dir"/*.imported)
  shopt -u nullglob
  if ((${#records[@]} == 0)); then
    doctor_warn "未在 $manifest_dir 找到 *.imported；尚未导入任何镜像包"
  fi
  local record component image_ref key release_value expected_ref expected_field
  # 当前 .release.env 的 digest 只要能在"任意一条"导入记录中找到即可：manifests/ 会按发布
  # 历史累积 v1、v2…，旧记录是回滚依据，不能因为当前指针已指向更新的导入版本就报失败。
  # 用空格包裹的字符串集合记录键名，避免依赖关联数组（脚本其余部分也不需要 bash 4+ 的声明）。
  local doctor_matched_keys=" " doctor_keys_seen=" "
  for record in "${records[@]}"; do
    component="$(env_get "$record" COMPONENT)"
    image_ref="$(env_get "$record" IMAGE_REF)"
    if [[ -z "$component" || -z "$image_ref" ]]; then
      doctor_fail "导入记录缺少 COMPONENT 或 IMAGE_REF：$(basename -- "$record")"
      continue
    fi
    if [[ "$component" == common ]]; then
      doctor_ok "$(basename -- "$record")：公共基础设施包按 IMAGES 清单发布，不产生单一不可变 digest"
      continue
    fi
    if [[ ! "$image_ref" =~ @sha256:[0-9a-f]{64}$ ]]; then
      doctor_fail "$(basename -- "$record") 的 IMAGE_REF 不是不可变 @sha256: 引用：${image_ref}；请重新 import 对应镜像包"
      continue
    fi
    if [[ ! -f "$release_file" ]]; then
      doctor_fail ".release.env 不存在，无法核对 $(basename -- "$record") 的镜像键；请先执行：$0 configure"
      continue
    fi
    local keys=()
    mapfile -t keys < <(doctor_release_keys "$component")
    if ((${#keys[@]} == 0)); then
      doctor_warn "未登记组件 $component 的 .release.env 键名，跳过一致性比较"
      continue
    fi
    local key_checked=false key_placeholder=false
    for key in "${keys[@]}"; do
      expected_field=IMAGE_REF
      expected_ref="$image_ref"
      # data-analysis 是一个包内包含四个生产镜像的复合组件。兼容键
      # DATA_ANALYSIS_IMAGE 仍指向 dashboard API，其余键必须逐项核对
      # 导入记录中的同名字段，不能拿通用 IMAGE_REF 误判 Worker/Migrate。
      if [[ "$component" == data-analysis && "$key" != DATA_ANALYSIS_IMAGE ]]; then
        expected_field="$key"
        expected_ref="$(env_get "$record" "$expected_field")"
        if [[ -z "$expected_ref" ]]; then
          doctor_fail "$(basename -- "$record") 缺少 ${expected_field}；请重新 import 对应镜像包"
          continue
        fi
        if [[ ! "$expected_ref" =~ @sha256:[0-9a-f]{64}$ ]]; then
          doctor_fail "$(basename -- "$record") 的 $expected_field 不是不可变 @sha256: 引用：${expected_ref}；请重新 import 对应镜像包"
          continue
        fi
      fi
      release_value="$(env_get "$release_file" "$key")"
      if [[ -z "$release_value" ]]; then
        doctor_fail "$key 在 .release.env 中未设置；请重新执行 prepare/deploy 使其写入不可变 digest"
        continue
      fi
      if doctor_is_placeholder_image "$release_value"; then
        key_placeholder=true
        continue
      fi
      if [[ "$release_value" != "$expected_ref" ]]; then
        # 历史导入记录：当前指针指向另一个已导入版本（回滚仍需保留旧记录）。
        # 真正的门禁是"当前 digest 必须出现在某条导入记录里"，见记录循环之后的核对。
        doctor_ok "$(basename -- "$record")：历史导入记录（当前 $key 指向其他已导入版本，保留用于回滚）"
      else
        key_checked=true
        if [[ "$doctor_matched_keys" != *" $key "* ]]; then
          doctor_matched_keys+="$key "
        fi
      fi
      if [[ "$doctor_keys_seen" != *" $key "* ]]; then
        doctor_keys_seen+="$key "
      fi
    done
    if [[ "$key_placeholder" == true ]]; then
      if [[ "$component" == platform || "$component" == frontend ]]; then
        doctor_warn "$component 已 import，但 ${keys[*]} 仍是占位值；下一步：$0 deploy $component"
      else
        doctor_warn "$component 已 import，但 ${keys[*]} 仍是占位值；下一步：$0 prepare ${component}（import 只登记 manifests/，写入 .release.env 的是 prepare）"
      fi
    elif [[ "$key_checked" == true ]]; then
      doctor_ok "${component}：${keys[*]} 已登记不可变 digest，与导入记录一致"
    fi
  done

  # 当前 .release.env 中的每个镜像键都必须能在导入记录里找到来源；否则才是真正的不一致
  # （例如手工改写 digest 或漏 import 当前包）。旧版本记录已在上面按历史记录处理。
  local seen_key
  for seen_key in $doctor_keys_seen; do
    [[ -n "$seen_key" ]] || continue
    [[ "$doctor_matched_keys" == *" $seen_key "* ]] && continue
    release_value="$(env_get "$release_file" "$seen_key")"
    [[ -n "$release_value" ]] || continue
    doctor_is_placeholder_image "$release_value" && continue
    doctor_fail "$seen_key 的当前 digest（${release_value}）不在任何 manifests/*.imported 记录中；请重新 import 对应镜像包后再 prepare/deploy"
  done

  if [[ -f "$release_file" && ! -L "$release_file" ]]; then
    local placeholders=() line key value
    while IFS= read -r line; do
      [[ -n "${line//[[:space:]]/}" ]] || continue
      if [[ "$line" =~ ^[[:space:]]*# ]]; then continue; fi
      [[ "$line" == *=* ]] || continue
      key="${line%%=*}"; value="${line#*=}"
      if doctor_is_placeholder_image "$value"; then placeholders+=("$key=$value"); fi
    done < "$release_file"
    if ((${#placeholders[@]} > 0)); then
      doctor_warn ".release.env 仍有 ${#placeholders[@]} 个镜像键是占位值："
      printf '        %s\n' "${placeholders[@]}"
      printf '        提示：import 只把摘要登记到 manifests/；写入 .release.env 的是 prepare（子系统）或 deploy（platform/frontend）。\n'
    else
      doctor_ok ".release.env 中所有镜像键都已是不可变引用"
    fi
  fi
}

doctor_check_provisioner_binding() {
  printf '\n[4/6] subsystem-provisioner 容器内 .release.env 绑定一致性\n'
  if [[ "$doctor_docker_available" != true ]]; then
    doctor_warn '未找到 docker 命令，跳过容器内 .release.env 比对'
    return 0
  fi
  local container
  container="$(docker ps -q --filter 'label=com.docker.compose.service=subsystem-provisioner' 2>/dev/null || true)"
  [[ -n "$container" ]] || container="$(docker ps -q --filter 'name=uip-subsystem-provisioner' 2>/dev/null || true)"
  if [[ -z "$container" ]]; then
    doctor_ok 'subsystem-provisioner 未运行，无需比对容器内绑定'
    return 0
  fi
  local name path host_digest container_digest
  name="$(docker inspect -f '{{.Name}}' "$container" 2>/dev/null | sed 's#^/##' || true)"
  name="${name:-$container}"
  # 容器内路径优先取自 Agent 自己的环境变量，其次从绑定挂载的目的地推导。
  path="$(docker inspect -f '{{range .Config.Env}}{{println .}}{{end}}' "$container" 2>/dev/null |
    awk -F= '$1 == "SUBSYSTEM_PRODUCTION_RELEASE_ENV_PATH" {print $2; exit}' || true)"
  if [[ -z "$path" ]]; then
    path="$(docker inspect -f '{{range .Mounts}}{{println .Source "|" .Destination}}{{end}}' "$container" 2>/dev/null |
      awk -F'|' -v host="$release_file" '$1 == host {print $2; exit}' || true)"
  fi
  if [[ -z "$path" ]]; then
    doctor_warn "无法确定容器 $name 内 .release.env 的路径，跳过比对；如页面仍报 immutable digest，请执行：docker restart $name"
    return 0
  fi
  if [[ ! -f "$release_file" ]]; then
    doctor_fail "宿主机 .release.env 不存在，无法比对：$release_file"
    return 0
  fi
  host_digest="$(sha256sum "$release_file" | awk '{print $1}')"
  # 内容摘要优先：.release.env 是单文件 bind mount，原子替换后 inode 变化而内容比对能直接
  # 暴露“容器仍绑旧 inode”。镜像缺少 cat 时退回容器内 sha256sum。
  if container_digest="$(docker exec "$container" cat "$path" 2>/dev/null | sha256sum | awk '{print $1}')" &&
    docker exec "$container" test -f "$path" >/dev/null 2>&1; then
    :
  else
    container_digest="$(docker exec "$container" sha256sum "$path" 2>/dev/null | awk '{print $1}' || true)"
  fi
  if [[ -z "$container_digest" ]]; then
    local started_at host_mtime
    started_at="$(docker inspect -f '{{.State.StartedAt}}' "$container" 2>/dev/null || true)"
    host_mtime="$(stat -c '%y' "$release_file" 2>/dev/null || true)"
    doctor_warn "无法读取容器 $name 内的 ${path}（镜像可能缺少 cat/sha256sum）；容器启动时间=${started_at:-未知}，宿主机 .release.env 修改时间=${host_mtime:-未知}。若宿主机文件在容器启动后被替换过，请执行：docker restart $name"
    return 0
  fi
  if [[ "$container_digest" == "$host_digest" ]]; then
    doctor_ok "容器 $name 内读取到的 .release.env 与宿主机内容一致"
  else
    doctor_fail "容器 $name 仍绑定旧的 .release.env（内容摘要不一致）：宿主机=$host_digest 容器内=${container_digest}；这是 .release.env 被原子替换后 inode 变化导致的，请执行：docker restart $name"
  fi
}

doctor_check_runtime_placeholders() {
  printf '\n[5/6] runtime/*.env 待平台下发的凭据\n'
  shopt -s nullglob
  local files=("$deploy_dir"/runtime/*.env)
  shopt -u nullglob
  if ((${#files[@]} == 0)); then
    doctor_ok 'runtime/ 下暂无子系统配置文件'
    return 0
  fi
  local file pending=() key
  for file in "${files[@]}"; do
    [[ -f "$file" && ! -L "$file" ]] || continue
    pending=()
    mapfile -t pending < <(awk -F'=' '
      /^[[:space:]]*#/ {next}
      /^[[:space:]]*$/ {next}
      {
        key=$1
        value=substr($0, index($0,"=") + 1)
        gsub(/^[[:space:]]+|[[:space:]]+$/, "", key)
        gsub(/^[[:space:]]+|[[:space:]]+$/, "", value)
        if (value == "" || value == "PENDING_ONBOARDING" || value ~ /^REPLACE_WITH_/) print key "=" value
      }' "$file")
    if ((${#pending[@]} > 0)); then
      doctor_warn "$(basename -- "$file") 有 ${#pending[@]} 个键仍是占位值或空值："
      printf '        %s\n' "${pending[@]}"
    else
      doctor_ok "$(basename -- "$file") 未发现占位值或空值"
    fi
  done
  printf '        说明：这些值由基础平台在受控采用/重试时下发。出现 PENDING_ONBOARDING 不代表文件权限问题，\n'
  printf '        也不要手工填入猜测值；按 status <子系统> 的下一步操作即可。\n'
}

doctor_check_subsystem() {
  local component="$1"
  printf '\n[6/6] 子系统 %s\n' "$component"
  case "$component" in
    customer-opportunity|customer-portal|contract|project|settlement|data-analysis) ;;
    *)
      doctor_fail "不支持的子系统：${component}（可用：contract | project | settlement | data-analysis | customer-opportunity | customer-portal）"
      return 0
      ;;
  esac
  subsystem_metadata "$component"
  if [[ "$doctor_docker_available" != true ]]; then
    doctor_warn "未找到 docker 命令，无法读取 $component 的部署阶段"
    return 0
  fi
  local phase
  phase="$(subsystem_phase "$component" 2>/dev/null || true)"
  case "$phase" in
    NOT_IMPORTED)
      doctor_fail "$component 尚未 import；请执行：$0 import packages/$component-*-linux-amd64.tar.gz"
      ;;
    '')
      doctor_fail "无法读取 $component 的部署阶段；请确认 Docker 可用且已执行 $0 configure"
      ;;
    *)
      doctor_ok "$component 当前阶段：${phase}（生产目标：$subsystem_app_code/prod）"
      print_subsystem_next_action "$component" "$phase"
      ;;
  esac
}

doctor() {
  local component="${1:-}"
  doctor_failures=0
  doctor_warnings=0
  command -v docker >/dev/null 2>&1 && doctor_docker_available=true || doctor_docker_available=false
  printf '环境自检（只读）：%s\n' "$deploy_dir"
  printf '本命令不修改任何文件，也不会重启或创建任何容器。\n'
  doctor_check_config_files
  doctor_check_packages
  doctor_check_manifests
  doctor_check_provisioner_binding
  doctor_check_runtime_placeholders
  if [[ -n "$component" ]]; then
    doctor_check_subsystem "$component"
  fi
  printf '\n自检结果：%d 项失败，%d 项警告\n' "$doctor_failures" "$doctor_warnings"
  if ((doctor_failures > 0)); then
    printf '存在阻断问题：请按上面的建议处理后重新执行 doctor。\n' >&2
    return 1
  fi
  if ((doctor_warnings > 0)); then
    printf '未发现阻断问题，但有 %d 项警告。\n' "$doctor_warnings"
    return 0
  fi
  printf '未发现阻断问题。\n'
  return 0
}
