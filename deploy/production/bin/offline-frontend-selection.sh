#!/usr/bin/env bash
# 发行选择只控制构建输入，不改变运行许可、接入登记或业务授权。

# offline_portal_directory_mode 校验本次发行的显式门户来源选择，缺省保留CRM。
# 参数：raw为CLI优先后确定的构建输入，不读取或更改运行配置。
# 返回值：crm或local；未知选择失败且不开始构建。
offline_portal_directory_mode() {
  case "${1:-crm}" in
    crm|local) printf '%s\n' "${1:-crm}" ;;
    *) echo 'Portal发行来源必须是 crm 或 local' >&2; return 2 ;;
  esac
}

# offline_frontend_enabled_modules 将已选择交付组件转换为规范前端编码。
# 参数：provided表示显式选择，raw为组件，mode为本次发行来源且默认crm。
# 返回值：all、none 或有序业务编码；未知组件返回非零。
offline_frontend_enabled_modules() {
  local provided="$1" raw="${2-}" mode token selected=' ' output='' code
  mode="$(offline_portal_directory_mode "${3:-crm}")" || return 2
  [[ "$provided" == true ]] || { printf 'all\n'; return 0; }
  raw="${raw//,/ }"
  for token in $raw; do
    case "$token" in
      contract|contract_management|contract-management) code=contract_management ;;
      project|project_management|project-management) code=project_management ;;
      settlement|settlement_and_invoicing|settlement-invoicing) code=settlement ;;
      customer-opportunity|customer_and_opportunity) code=customer_and_opportunity ;;
      customer-portal|customer_portal) code=customer_portal ;;
      data-analysis|data_analysis) code=data_analysis ;;
      *) echo "未知前端交付组件：$token" >&2; return 2 ;;
    esac
    selected="$selected$code "
  done
  # 只有显式本地发行免除CRM依赖；运行来源仍须首次安装时受控选择。
  [[ "$mode" == local || "$selected" != *' customer_portal '* ]] || selected="${selected}customer_and_opportunity "
  for code in project_management settlement contract_management customer_and_opportunity customer_portal data_analysis; do
    [[ "$selected" != *" $code "* ]] || output="${output}${output:+,}$code"
  done
  printf '%s\n' "${output:-none}"
}

# offline_bind_build_arguments 将实际构建参数绑定到源码指纹。
# 参数：第一个参数为源码摘要，其余参数为已验证的构建参数记录。
# 返回值：包含全部参数的 SHA256；哈希命令失败时返回非零。
offline_bind_build_arguments() {
  printf '%s\n' "$@" | sha256sum | awk '{print tolower($1)}'
}

# offline_frontend_manifest_codes 生成发行选择对应的精确模块清单。
# 参数：selection 为规范前端选择。
# 返回值：包含恒定平台模块的 JSON 数组。
offline_frontend_manifest_codes() {
  local selection="$1"
  if [[ "$selection" == all ]]; then
    selection='project_management,settlement,contract_management,customer_and_opportunity,customer_portal,data_analysis'
  elif [[ "$selection" == none ]]; then
    selection=''
  fi
  jq -cn --arg selection "$selection" '["basic-platform"] + ($selection | split(",") | map(select(length > 0)))'
}

# offline_validate_frontend_manifest 校验当前或复用镜像的发行清单。
# 参数：manifest 为清单路径，selection 为规范前端选择。
# 返回值：精确匹配返回零；缺失、符号链接或错配返回非零。
offline_validate_frontend_manifest() {
  local manifest="$1" selection="$2" expected
  [[ -f "$manifest" && ! -L "$manifest" ]] || {
    echo '缺少安全的 frontend-modules.json，请完整重建前端基线' >&2; return 1;
  }
  expected="$(offline_frontend_manifest_codes "$selection")" || return 1
  jq -e --argjson expected "$expected" '
    .schema_version == 1 and .enabled_modules == $expected and .compiled_modules == $expected
  ' "$manifest" >/dev/null || {
    echo '前端镜像模块清单与发行选择不一致，拒绝打包或复用' >&2; return 1;
  }
}

# offline_copy_frontend_manifest 从新镜像的未运行临时容器核验构建清单。
# 参数：镜像标签、目标平台、规范选择、输出目录。
# 返回值：清单校验及原子发布成功返回零；错配时停止交付。
offline_copy_frontend_manifest() (
  local image="$1" platform="$2" selection="$3" output="$4" container='' temporary
  temporary="$(mktemp "$output/.frontend-modules.XXXXXX")" || exit 1
  # cleanup_frontend_manifest 只清理本函数创建的临时文件及未运行容器。
  # 参数：无。
  # 返回值：保持原错误码；容器清理失败时返回非零。
  cleanup_frontend_manifest() {
    local status=$?
    rm -f -- "$temporary"
    if [[ -n "$container" ]]; then docker rm "$container" >/dev/null || status=1; fi
    exit "$status"
  }
  trap cleanup_frontend_manifest EXIT
  container="$(docker create --platform "$platform" "$image")" || exit 1
  [[ "$container" =~ ^[A-Za-z0-9][A-Za-z0-9_.-]+$ ]] || exit 1
  docker cp "$container:/usr/share/nginx/html/frontend-modules.json" "$temporary" || exit 1
  offline_validate_frontend_manifest "$temporary" "$selection" || exit 1
  [[ ! -L "$output/frontend-modules.json" ]] || exit 1
  if [[ -e "$output/frontend-modules.json" ]]; then
    cmp -s "$temporary" "$output/frontend-modules.json" || {
      echo '已有前端构建清单与镜像不一致，拒绝混合交付' >&2; exit 1;
    }
  else
    chmod 644 "$temporary"
    mv -- "$temporary" "$output/frontend-modules.json"
  fi
)
