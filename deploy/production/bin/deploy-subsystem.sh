#!/usr/bin/env bash
# Subsystem upgrade/staging helpers. Sourced by deploy.sh.

stage_upgrade() {
  local component="${1:?component required}" image key record
  require_initialized
  scope_require "$component" || return
  record="$(latest_record "$component")"
  image="$(env_get "$record" IMAGE_REF)"
  case "$component" in
    platform|frontend) deploy_component "$component"; return ;;
    customer-opportunity) key=CUSTOMER_CRM_IMAGE ;;
    customer-portal) key=CUSTOMER_PORTAL_IMAGE ;;
    contract) key=CONTRACT_IMAGE ;;
    project) key=PROJECT_IMAGE ;;
    settlement) key=SETTLEMENT_IMAGE ;;
    data-analysis) key=DATA_ANALYSIS_IMAGE ;;
    *) die "不支持的组件：$component" ;;
  esac
  need flock
  [[ ! -L "$deploy_dir/runtime/.deploy.lock" ]] || die '部署锁不能是符号链接'
  exec 7>"$deploy_dir/runtime/.deploy.lock"
  flock -w 60 7 || die '平台 Agent 或其他发布任务正在运行；请等待其结束后重试升级'
  if ! stage_subsystem_release "$component" "$record"; then
    flock -u 7
    exec 7>&-
    return 1
  fi
  flock -u 7
  exec 7>&-
  printf '组件 %s 的升级镜像已暂存：%s\n' "$component" "$image"
  echo '请打开基础平台的应用页面执行受控更新或重试；平台会依次完成升级前备份、数据库迁移和健康检查。'
}

subsystem_metadata() {
  local component="$1"
  subsystem_candidate="uip-${component}-candidate"
  case "$component" in
    customer-opportunity) subsystem_key=CUSTOMER_CRM_IMAGE; subsystem_service=customer-api; subsystem_app_code=customer_and_opportunity; subsystem_app_name='客户与商机管理系统'; subsystem_internal_host=customer-api; subsystem_internal_port=8090; subsystem_health=/customer-opportunity/healthz; subsystem_path=/customer-opportunity; subsystem_callback=/customer-opportunity/auth/callback; subsystem_runtime="$deploy_dir/runtime/customer.env" ;;
    customer-portal) subsystem_key=CUSTOMER_PORTAL_IMAGE; subsystem_service=portal-api; subsystem_app_code=customer_portal; subsystem_app_name='客户自助门户'; subsystem_internal_host=portal-api; subsystem_internal_port=8091; subsystem_health=/customer-portal/healthz; subsystem_path=/customer-portal; subsystem_callback=/customer-portal/auth/callback; subsystem_runtime="$deploy_dir/runtime/portal.env" ;;
    contract) subsystem_key=CONTRACT_IMAGE; subsystem_service=contract-api; subsystem_app_code=contract_management; subsystem_app_name='合同管理系统'; subsystem_internal_host=contract-api; subsystem_internal_port=8081; subsystem_health=/healthz; subsystem_path=/contract_management; subsystem_callback=/contract_management/auth/callback; subsystem_runtime="$deploy_dir/runtime/contract.env" ;;
    project) subsystem_key=PROJECT_IMAGE; subsystem_service=project-api; subsystem_app_code=project_management; subsystem_app_name='项目服务内容管理系统'; subsystem_internal_host=project-api; subsystem_internal_port=8082; subsystem_health=/healthz; subsystem_path=/project_management; subsystem_callback=/project_management/auth/callback; subsystem_runtime="$deploy_dir/runtime/project.env" ;;
    settlement) subsystem_key=SETTLEMENT_IMAGE; subsystem_service=settlement-api; subsystem_app_code=settlement; subsystem_app_name='结算与开票管理系统'; subsystem_internal_host=settlement-api; subsystem_internal_port=8085; subsystem_health=/healthz; subsystem_path=/settlement; subsystem_callback=/settlement/auth/callback; subsystem_runtime="$deploy_dir/runtime/settlement.env" ;;
    data-analysis) subsystem_key=DATA_ANALYSIS_IMAGE; subsystem_service=data-analysis-api; subsystem_app_code=data_analysis; subsystem_app_name='数据看板与统计分析系统'; subsystem_internal_host=data-analysis-api; subsystem_internal_port=8080; subsystem_health=/data_analysis/healthz; subsystem_path=/data_analysis; subsystem_callback=/data_analysis/auth/callback; subsystem_runtime="$deploy_dir/runtime/data-analysis.env" ;;
    *) die "不支持的子系统：$component" ;;
  esac
}

prepare_subsystem() {
  local component="${1:?component required}" image version phase profile
  require_initialized
  scope_require "$component" || return
  subsystem_metadata "$component"
  need flock
  [[ ! -L "$deploy_dir/runtime/.deploy.lock" ]] || die '部署锁不能是符号链接'
  exec 7>"$deploy_dir/runtime/.deploy.lock"
  flock -w 5 7 || die '平台 Agent 或其他发布任务正在运行；请等待其结束后执行 status'
  profile="$deploy_dir/subsystems.d/${subsystem_app_code}-prod.yaml"
  [[ "$component" != data-analysis ]] || profile="$deploy_dir/subsystems.d/data-analysis-prod.yaml"
  [[ -f "$profile" && ! -L "$profile" ]] || die "生产环境发布配置缺失或不是普通文件：$profile"
  phase="$(subsystem_phase "$component")"
  case "$phase" in
    VERIFIED)
      printf '%s/prod 已完成部署，无需重新创建候选容器。\n' "$subsystem_app_code"
      flock -u 7
      exec 7>&-
      return 0
      ;;
    READY_TO_CONTINUE)
      die "$subsystem_app_code/prod 已由平台部署完成；下一步仅执行：$0 continue $component"
      ;;
    WAITING_FOR_PLATFORM_ADOPTION|PROVISIONING_OR_FAILED)
      printf '候选容器已存在，未重复创建。当前阶段：%s。\n' "$phase"
      print_subsystem_next_action "$component" "$phase"
      flock -u 7
      exec 7>&-
      return 0
      ;;
    INVALID_CANDIDATE)
      die "发现标签不匹配的候选容器 ${subsystem_candidate}；请人工核对，脚本不会覆盖"
      ;;
  esac
  local record
  record="$(latest_record "$component")"
  image="$(env_get "$record" IMAGE_REF)"
  version="$(env_get "$record" VERSION)"
  if ! stage_subsystem_release "$component" "$record" create_subsystem_candidate "$component" "$image" "$version"; then
    flock -u 7
    exec 7>&-
    return 1
  fi
  printf '候选容器已创建：%s\n当前阶段：WAITING_FOR_PLATFORM_ADOPTION\n' "$subsystem_candidate"
  print_subsystem_next_action "$component" WAITING_FOR_PLATFORM_ADOPTION
  flock -u 7
  exec 7>&-
}


create_subsystem_candidate() {
  local component="$1" image="$2" version="$3"
  subsystem_metadata "$component"
  docker create --name "$subsystem_candidate" --network none --entrypoint /bin/sh \
    --label com.basic-platform.discovery=v1 \
    --label "com.basic-platform.application_code=$subsystem_app_code" \
    --label "com.basic-platform.application_name=$subsystem_app_name" \
    --label com.basic-platform.environment=prod \
    --label "com.basic-platform.service_name=$subsystem_service" \
    --label com.basic-platform.service_role=business \
    --label "com.basic-platform.internal_host=$subsystem_internal_host" \
    --label "com.basic-platform.internal_port=$subsystem_internal_port" \
    --label com.basic-platform.protocol=http \
    --label "com.basic-platform.health_endpoint=$subsystem_health" \
    --label "com.basic-platform.path_prefix=$subsystem_path" \
    --label "com.basic-platform.oidc_callback_path=$subsystem_callback" \
    --label com.basic-platform.oidc_callback_supported=true \
    --label "com.basic-platform.version=$version" \
    "$image" -c 'exit 64' >/dev/null
}
