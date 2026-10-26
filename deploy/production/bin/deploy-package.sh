#!/usr/bin/env bash
# Offline package discovery/index helpers. Sourced by deploy.sh.

discover_package() {
  local component="$1" pattern
  case "$component" in
    common) pattern='common-infrastructure-linux-amd64.tar.gz' ;;
    platform) pattern='platform-backend-*-linux-amd64.tar.gz' ;;
    frontend) pattern='frontend-*-linux-amd64.tar.gz' ;;
    customer-opportunity) pattern='customer-opportunity-backend-*-linux-amd64.tar.gz' ;;
    customer-portal) pattern='customer-portal-backend-*-linux-amd64.tar.gz' ;;
    contract) pattern='contract-backend-*-linux-amd64.tar.gz' ;;
    project) pattern='project-backend-*-linux-amd64.tar.gz' ;;
    settlement) pattern='settlement-backend-*-linux-amd64.tar.gz' ;;
    data-analysis) pattern='data-analysis-backend-*-linux-amd64.tar.gz' ;;
    *) die "不支持自动发现的组件：$component" ;;
  esac
  local -a matches=()
  shopt -s nullglob
  matches=("$package_dir"/$pattern)
  shopt -u nullglob
  if ((${#matches[@]} > 1)); then
    printf '错误：packages/ 中发现多个 %s 镜像包，无法安全选择：%s\n' "$component" "${matches[*]}" >&2
    return 2
  fi
  ((${#matches[@]} == 1)) || return 1
  [[ -e "${matches[0]}" ]] || return 1
  [[ ! -L "${matches[0]}" ]] || {
    printf '错误：%s 镜像包不能是符号链接：%s\n' "$component" "${matches[0]}" >&2
    return 2
  }
  [[ -f "${matches[0]}" ]] || {
    printf '错误：%s 镜像包必须是普通文件：%s\n' "$component" "${matches[0]}" >&2
    return 2
  }
  printf '%s\n' "${matches[0]}"
}

package_index() {
  case "$1" in
    common) printf '0' ;;
    platform) printf '1' ;;
    frontend) printf '2' ;;
    customer-opportunity) printf '3' ;;
    customer-portal) printf '4' ;;
    contract) printf '5' ;;
    project) printf '6' ;;
    settlement) printf '7' ;;
    data-analysis) printf '8' ;;
    *) return 1 ;;
  esac
}

auto_install_packages() {
  require_initialized
  scope_services >/dev/null || die '统一 Compose 无法解析'
  scope_report
  local component archive index
  local -a packages=()
  local -a ordered=(common platform frontend customer-opportunity customer-portal contract project settlement data-analysis)

  printf '扫描镜像包目录：%s\n' "$package_dir"
  # 先解析全部包名并检查歧义，避免平台启动后才发现包版本冲突。
  for component in "${ordered[@]}"; do
    scope_enabled "$component" || { printf '  %s 未启用，跳过镜像包检查。\n' "$component"; continue; }
    if archive="$(discover_package "$component")"; then
      index="$(package_index "$component")"
      packages[$index]="$archive"
      printf '  已发现 %-20s %s\n' "$component" "$(basename -- "$archive")"
    else
      case "$?" in
        2) die "镜像包发现存在歧义或文件类型不安全：$component" ;;
        *)
          if [[ "$component" == common || "$component" == platform || "$component" == frontend ]]; then
            die "缺少基础组件镜像包：${component}；请先将对应 tar.gz 放入 ${package_dir}"
          fi
          printf '  未提供 %-20s（跳过）\n' "$component"
          ;;
      esac
    fi
  done

  printf '\n阶段 A：导入公共基础设施和基础平台镜像\n'
  if workflow_enabled; then
    workflow_import common-import delivery_import_matches delivery_import_component common "${packages[0]}" || return $?
    workflow_import platform-import delivery_import_matches delivery_import_component platform "${packages[1]}" || return $?
  else
    import_package "${packages[0]}"
    import_package "${packages[1]}"
  fi
  printf '\n阶段 B：启动并检查基础平台\n'
  if workflow_enabled; then
    workflow_publish platform delivery_release_matches delivery_deploy_component platform "${packages[1]}" "${packages[0]}" || return $?
  else
    deploy_component platform
  fi
  if ! wait_for_required_service_health platform-api platform-worker file-gateway keycloak temporal; then
    printf '自动安装暂停于基础平台验收；已完成的迁移和服务会保留。修复问题后执行：sudo ./bin/deploy.sh verify\n' >&2
    return 1
  fi

  printf '\n阶段 C：导入并启动统一前端\n'
  if workflow_enabled; then
    workflow_import frontend-import delivery_import_matches delivery_import_component frontend "${packages[2]}" || return $?
    workflow_publish frontend delivery_release_matches delivery_deploy_component frontend "${packages[2]}" || return $?
  else
    import_package "${packages[2]}"
    deploy_component frontend
  fi
  if ! verify; then
    printf '自动安装暂停于前端验收；基础平台保持运行。修复问题后执行：sudo ./bin/deploy.sh verify\n' >&2
    return 1
  fi
  if workflow_enabled; then workflow_start license-prepare || return $?; fi

  # 只有平台维护命令才能建立已签名的安装身份；普通环境变量值绝不能选择或重置客户实例。
  if workflow_enabled && workflow_was_done license-prepare; then
    printf '保留已完成的安装身份/授权准备；不重复导入原交付许可证，当前授权由最终验收重新核对。\n'
  elif [[ -e "$deploy_dir/license-installation.json" || -L "$deploy_dir/license-installation.json" ]]; then
    prepare_license_installation || return 1
  elif [[ -e "$deploy_dir/license" || -L "$deploy_dir/license" ]]; then
    install_delivery_license import || return 1
  fi
  if workflow_enabled; then workflow_done license-prepare || return $?; fi

  printf '\n阶段 D：导入并准备已提供的业务子系统镜像\n'
  for component in customer-opportunity customer-portal contract project settlement data-analysis; do
    index="$(package_index "$component")"
    if [[ -z "${packages[$index]:-}" ]]; then
      if workflow_enabled && discover_package "$component" >/dev/null 2>&1; then
        workflow_start "${component}-import" || return $?
        workflow_update "${component}-import" SKIPPED COMPONENT_DISABLED || return $?
        workflow_start "${component}-prepare" || return $?
        workflow_update "${component}-prepare" SKIPPED COMPONENT_DISABLED || return $?
      fi
      continue
    fi
    if workflow_enabled; then
      workflow_import "${component}-import" delivery_import_matches delivery_import_component "$component" "${packages[$index]}" || return $?
      workflow_run "${component}-prepare" delivery_prepare_resume "$component" "${packages[$index]}" || return $?
    else
      import_package "${packages[$index]}"
      prepare_subsystem "$component" || return 1
    fi
  done
  if workflow_enabled; then
    workflow_start license || return $?
    if workflow_was_done license; then
      printf '保留已完成的授权处理；最终验收将重新核验当前有效许可证和自然 ACK，允许在平台独立续期后继续。\n'
      workflow_done license || return $?
      return 0
    fi
  fi
  if [[ -f "$deploy_dir/license-installation.json" ]]; then
    # 只有不可变的升级前基线可以决定迁移目标；新包和新购系统不能伪造历史兼容资格。
    compose run -T --rm --no-deps platform-api ./license-install migrate || return 1
    printf '\n存量迁移确认完成；过渡资格不等于正式授权，新增未授权系统未开放业务。\n'
  fi
  if [[ -e "$deploy_dir/license" || -L "$deploy_dir/license" ]]; then
    install_delivery_license activate || return 1
    if workflow_enabled; then workflow_done license || return $?; fi
    printf '\n交付授权已生效：全部必需业务执行组件已确认。\n'
    return 0
  fi
  if [[ -f "$deploy_dir/license-installation.json" ]]; then
    if workflow_enabled; then workflow_done license || return $?; fi
    printf '\n四场景安装准备完成：基础平台和前端可用，存量系统已按冻结基线完成迁移确认。\n'
    printf '新增未授权系统不会开放业务；请在授权管理中导入有效许可证并完成组件激活确认。\n'
    return 0
  fi
  printf '\n自动安装阶段完成：基础平台和前端已部署；子系统包已登记并准备候选。\n'
  printf '子系统服务不会自动启动；请在平台页面逐个受控采用 prod，再按 status -> continue -> verify 完成验收。\n'
  if workflow_enabled; then workflow_done license || return $?; fi
}

delivery_import_component() { bash "$script_dir/deploy.sh" import "$2"; }
delivery_deploy_component() { bash "$script_dir/deploy.sh" deploy "$1"; }
