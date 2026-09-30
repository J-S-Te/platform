#!/usr/bin/env bash
# 部署范围只取决于统一 Compose 的实际服务；不维护第二份启用名单。
scope_services() {
  docker compose --project-directory "$deploy_dir" --file "$deploy_dir/docker-compose.yml" \
    --env-file "$runtime_file" --env-file "$release_file" config --services
}

scope_service_for() {
  case "$1" in
    common|platform) printf '%s\n' platform-api ;;
    frontend) printf '%s\n' frontend ;;
    customer-opportunity) printf '%s\n' customer-api ;;
    customer-portal) printf '%s\n' portal-api ;;
    contract|project|settlement|data-analysis) printf '%s-api\n' "$1" ;;
    *) printf '未知系统：%s\n' "$1" >&2; return 1 ;;
  esac
}

scope_enabled() {
  local service services
  service="$(scope_service_for "$1")" || return
  services="$(scope_services)" || return
  grep -Fxq "$service" <<< "$services"
}

scope_require() {
  scope_enabled "$1" && return 0
  printf '此系统未启用或统一 Compose 无法解析：%s；请检查 docker-compose.yml 中对应服务块。\n' "$1" >&2
  return 1
}

scope_owned_services() {
  case "$1" in
    contract) printf '%s\n' contract-api contract-migrate ;;
    project) printf '%s\n' project-api project-migrate project-mysql project-sla-notifier ;;
    customer-opportunity) printf '%s\n' customer-api customer-migrate customer-mysql customer-opportunity-alert-worker customer-owner-notification-worker customer-presale-alert-worker customer-presale-assignment-notification-worker customer-presale-progress-notification-worker customer-notification-delivery-worker customer-presale-worker ;;
    customer-portal) printf '%s\n' portal-api portal-migrate portal-mysql portal-invite-compensation-worker ;;
    settlement) printf '%s\n' settlement-api settlement-worker settlement-migrate settlement-catalog-sync settlement-mysql ;;
    data-analysis) printf '%s\n' data-analysis-api data-analysis-migrate data-analysis-mysql data-analysis-metabase-init data-analysis-aggregation-worker data-analysis-alert-worker data-analysis-metabase ;;
    *) printf '只能停用业务子系统：%s\n' "$1" >&2; return 1 ;;
  esac
}

scope_project() {
  local project
  project="$(awk -F= '$1=="COMPOSE_PROJECT_NAME" {sub(/^[^=]*=/, ""); print; exit}' "$runtime_file")"
  printf '%s\n' "${project:-basic-platform-production}"
}

scope_report() {
  local services project id service module
  services="$(scope_services)" || return
  project="$(scope_project)"
  while read -r id service; do
    [[ -n "$id" && -n "$service" ]] || continue
    if ! grep -Fxq "$service" <<< "$services"; then
      printf '部署范围差异：现有容器 %s / %s 已不在 YAML 中；未自动停止。\n' "$id" "$service" >&2
      for module in contract project customer-opportunity customer-portal settlement data-analysis; do
        if scope_owned_services "$module" | grep -Fxq "$service"; then
          printf '显式停用并保留数据：./bin/deploy.sh disable %s\n' "$module" >&2
          break
        fi
      done
    fi
  done < <(docker ps -a --filter "label=com.docker.compose.project=$project" --format '{{.ID}} {{.Label "com.docker.compose.service"}}')
  if ! grep -Fxq contract-api <<< "$services"; then
    printf '能力影响：合同未启用，CRM 到合同交接不可用；共享 Temporal 数据库仍保留。\n' >&2
  fi
  if ! grep -Fxq customer-api <<< "$services"; then
    printf '能力影响：CRM 未启用，CRM 权威数据查询、相关合同创建和门户邀请补偿不可用。\n' >&2
  fi
  for module in project settlement data-analysis customer-portal; do
    service="$(scope_service_for "$module")"
    grep -Fxq "$service" <<< "$services" && continue
    case "$module" in
      project) printf '能力影响：项目未启用，合同项目交接与门户项目查询不可用。\n' >&2 ;;
      settlement) printf '能力影响：结算未启用，结算与开票操作不可用。\n' >&2 ;;
      data-analysis) printf '能力影响：数据看板未启用，统计分析与嵌入式报表不可用。\n' >&2 ;;
      customer-portal) printf '能力影响：客户门户未启用，客户自助入口与门户邀请不可用。\n' >&2 ;;
    esac
  done
}

scope_disable() {
  local module="$1" project service id owned
  owned="$(scope_owned_services "$module")" || return
  project="$(scope_project)"
  while read -r service; do
    while read -r id; do
      [[ -n "$id" ]] || continue
      docker stop --timeout 60 "$id" || return
      printf '已停止 %s（%s），保留容器、数据库卷和密钥。\n' "$service" "$id"
    done < <(docker ps -q --filter "label=com.docker.compose.project=$project" --filter "label=com.docker.compose.service=$service")
  done <<< "$owned"
}
