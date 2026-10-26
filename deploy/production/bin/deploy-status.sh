#!/usr/bin/env bash
# Subsystem runtime/status helpers. Sourced by deploy.sh.

runtime_ready() {
  local file="$1" report="${2:-true}" required prefix=OIDC
  [[ -f "$file" ]] || return 1
  required='PLATFORM_APPLICATION_CODE PLATFORM_ENVIRONMENT_CODE PLATFORM_AUDIT_CLIENT_ID PLATFORM_AUDIT_CLIENT_SECRET FILE_GATEWAY_BASE_URL FILE_GATEWAY_APPLICATION_ID FILE_GATEWAY_CLIENT_ID FILE_GATEWAY_CLIENT_SECRET'
  case "${file##*/}" in
    portal.env)
      prefix=PORTAL_OIDC
      required+=' PORTAL_ENCRYPTION_KEY_BASE64 PORTAL_REPORT_INGEST_DESCRIPTOR_KEY_BASE64 PORTAL_REPORT_CONTENT_KEY_BASE64 PORTAL_REPORT_STORE_ROOT PORTAL_PDF_CONFIG_ROOT PORTAL_CJK_FONT_PATH PORTAL_REPORT_IDENTITY_URL PORTAL_REPORT_IDENTITY_CLIENT_ID PORTAL_REPORT_IDENTITY_CLIENT_SECRET PORTAL_HMAC_KEY_BASE64 PORTAL_ROLE_CONFIG_HASH PORTAL_AUTHORIZATION_CONTEXT_URL PORTAL_AUTHORIZATION_CATALOG_APPLICATION_ID PORTAL_AUTHORIZATION_CATALOG_CLIENT_ID PORTAL_AUTHORIZATION_CATALOG_CLIENT_SECRET'
      ;;
    customer.env) required+=' SENSITIVE_ENCRYPTION_KEY_BASE64 SENSITIVE_HMAC_KEY_BASE64 PORTAL_INVITE_PEPPER_BASE64 OIDC_ROLE_CONFIG_HASH' ;;
    contract.env|project.env) required+=' OIDC_SESSION_ENCRYPTION_KEY_BASE64' ;;
    settlement.env)
      required='PLATFORM_ENVIRONMENT_CODE OIDC_SESSION_ENCRYPTION_KEY_BASE64'
      if [[ "$(env_get "$file" SETTLEMENT_FILE_GATEWAY_MODE)" == required ]]; then
        required+=' FILE_GATEWAY_URL SETTLEMENT_FILE_GATEWAY_APPLICATION_ID FILE_GATEWAY_CLIENT_ID FILE_GATEWAY_CLIENT_SECRET FILE_GATEWAY_SCOPE'
      fi
      ;;
    data-analysis.env) required+=' OIDC_CODEC_KEY OIDC_ROLE_CONFIG_HASH METABASE_EMBEDDING_SECRET' ;;
    *) echo '未知子系统配置文件' >&2; return 1 ;;
  esac
  [[ "$prefix" == PORTAL_OIDC ]] || required+=' PLATFORM_AUTHORIZATION_CONTEXT_URL PLATFORM_AUTHORIZATION_CATALOG_APPLICATION_ID PLATFORM_AUTHORIZATION_CATALOG_CLIENT_ID PLATFORM_AUTHORIZATION_CATALOG_CLIENT_SECRET'
  required+=" ${prefix}_ISSUER ${prefix}_CLIENT_ID ${prefix}_CLIENT_SECRET ${prefix}_REDIRECT_URI ${prefix}_TENANT_ID"
  # 只校验明确必需的核心接入字段；可选功能未启用时允许其配置为空。
  awk -v required="$required" -v report="$report" '
    /^[[:space:]]*(#|$)/ {next}
    {
      delimiter=index($0,"=")
      if (!delimiter) {if (report == "true") print "配置格式错误，行 " NR > "/dev/stderr"; bad=1; next}
      key=substr($0,1,delimiter-1); value=substr($0,delimiter+1)
      if (key !~ /^[A-Za-z_][A-Za-z0-9_]*$/ || seen[key]++) {
        if (report == "true") print "无效或重复配置键，行 " NR > "/dev/stderr"; bad=1
      }
      gsub(/^[[:space:]]+|[[:space:]]+$/, "", value)
      gsub(/^\047|\047$|^"|"$/, "", value)
      values[key]=value
    }
    END {
      n=split(required, keys, " ")
      for (i=1;i<=n;i++) if (values[keys[i]] == "" || values[keys[i]] ~ /^(PENDING_|REPLACE_WITH_)/) {
        if (report == "true") print "接入配置缺失：" keys[i] > "/dev/stderr"; bad=1
      }
      exit bad ? 1 : 0
    }' "$file"
}

subsystem_phase() {
  local component="$1" record candidate_code candidate_environment container running health
  subsystem_metadata "$component"
  record="$(find "$manifest_dir" -maxdepth 1 -type f -name "${component}-*.imported" -print -quit 2>/dev/null || true)"
  [[ -n "$record" ]] || { printf 'NOT_IMPORTED\n'; return; }

  if docker container inspect "$subsystem_candidate" >/dev/null 2>&1; then
    candidate_code="$(docker container inspect -f '{{index .Config.Labels "com.basic-platform.application_code"}}' "$subsystem_candidate" 2>/dev/null || true)"
    candidate_environment="$(docker container inspect -f '{{index .Config.Labels "com.basic-platform.environment"}}' "$subsystem_candidate" 2>/dev/null || true)"
    if [[ "$candidate_code" != "$subsystem_app_code" || "$candidate_environment" != prod ]]; then
      printf 'INVALID_CANDIDATE\n'
      return
    fi
  else
    container="$(compose ps -q "$subsystem_service" 2>/dev/null || true)"
    if [[ -n "$container" ]]; then
      running="$(docker inspect -f '{{.State.Running}}' "$container" 2>/dev/null || true)"
      health="$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' "$container" 2>/dev/null || true)"
      if [[ "$running" == true && ( "$health" == healthy || "$health" == none ) ]] && runtime_ready "$subsystem_runtime" false &&
          verify_selected_runtime "$component" >/dev/null 2>&1; then
        printf 'VERIFIED\n'
        return
      fi
    fi
    printf 'IMPORTED\n'
    return
  fi

  if ! runtime_ready "$subsystem_runtime" false; then
    printf 'WAITING_FOR_PLATFORM_ADOPTION\n'
    return
  fi
  container="$(compose ps -q "$subsystem_service" 2>/dev/null || true)"
  [[ -n "$container" ]] || { printf 'PROVISIONING_OR_FAILED\n'; return; }
  running="$(docker inspect -f '{{.State.Running}}' "$container" 2>/dev/null || true)"
  health="$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' "$container" 2>/dev/null || true)"
  if [[ "$running" == true && ( "$health" == healthy || "$health" == none ) ]] &&
      verify_selected_runtime "$component" >/dev/null 2>&1; then
    printf 'READY_TO_CONTINUE\n'
  else
    printf 'PROVISIONING_OR_FAILED\n'
  fi
}

print_subsystem_next_action() {
  local component="$1" phase="$2"
  subsystem_metadata "$component"
  case "$phase" in
    NOT_IMPORTED) printf '下一步：%s import packages/%s-*-linux-amd64.tar.gz\n' "$0" "$component" ;;
    IMPORTED) printf '下一步：%s prepare %s\n' "$0" "$component" ;;
    WAITING_FOR_PLATFORM_ADOPTION)
      printf '下一步：在基础平台“子系统探测与接入”页面采用 %s/prod。\n' "$subsystem_app_code"
      printf '此阶段不要执行 continue，也不要更新旧的 %s/dev 环境。\n' "$subsystem_app_code"
      ;;
    PROVISIONING_OR_FAILED) printf '下一步：在基础平台查看 %s/prod 部署状态；失败时对该 prod 环境点击“重试”，并查看 subsystem-provisioner 日志。\n' "$subsystem_app_code" ;;
    READY_TO_CONTINUE) printf '下一步：%s continue %s\n' "$0" "$component" ;;
    VERIFIED) printf '%s/prod 已完成部署和验收，无需继续操作。\n' "$subsystem_app_code" ;;
    INVALID_CANDIDATE) printf '下一步：人工核对候选容器 %s 的 application_code/environment 标签；脚本不会自动删除未知候选。\n' "$subsystem_candidate" ;;
    *) die "未知部署阶段：$phase" ;;
  esac
}

subsystem_status() {
  local component="$1" phase
  require_initialized
  subsystem_metadata "$component"
  phase="$(subsystem_phase "$component")"
  printf '组件：%s\n生产目标：%s/prod\n当前阶段：%s\n' "$component" "$subsystem_app_code" "$phase"
  print_subsystem_next_action "$component" "$phase"
}

continue_subsystem() {
  local component="${1:?component required}" phase container health
  require_initialized
  scope_require "$component" || return
  subsystem_metadata "$component"
  need flock
  [[ ! -L "$deploy_dir/runtime/.deploy.lock" ]] || die '部署锁不能是符号链接'
  exec 7>"$deploy_dir/runtime/.deploy.lock"
  flock -w 5 7 || die '平台 Agent 正在部署该子系统；请等待平台任务结束后再执行 continue'
  phase="$(subsystem_phase "$component")"
  if [[ "$phase" == VERIFIED ]]; then
    printf '%s/prod 已完成部署和验收。\n' "$subsystem_app_code"
    flock -u 7
    exec 7>&-
    return 0
  fi
  if [[ "$phase" != READY_TO_CONTINUE ]]; then
    printf '错误：当前阶段 %s 不允许执行 continue。\n' "$phase" >&2
    print_subsystem_next_action "$component" "$phase" >&2
    flock -u 7
    exec 7>&-
    return 1
  fi
  verify_selected_runtime "$component" || {
    printf '错误：完整运行集合或平台当前部署核验失败；保留候选。\n' >&2
    flock -u 7
    exec 7>&-
    return 1
  }
  container="$(compose ps -q "$subsystem_service" 2>/dev/null || true)"
  health="$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' "$container")"
  docker rm "$subsystem_candidate" >/dev/null || die '服务已就绪，但候选容器删除失败，请检查后重试'
  printf '%s 正在运行（健康状态：%s）；阶段已进入 VERIFIED，候选容器已删除。\n' "$subsystem_service" "$health"
  flock -u 7
  exec 7>&-
}

# -----------------------------------------------------------------------------
# doctor：只读环境自检
#
# 现场反复出现的四类“环境不一致”都不是脚本缺陷，而是漏步或宿主机/容器视图分叉：
#   1. 镜像包只上传到交付目录的 iso/，没有进入部署目录的 packages/；
#   2. 只 import 没有 prepare，.release.env 里仍是占位镜像；
#   3. .release.env 被 mktemp+mv 原子替换后 inode 变化，以单文件 bind mount 挂载它的
#      subsystem-provisioner 仍绑旧 inode，宿主机文件已正确而 Agent 读到占位值；
#   4. subsystem runtime 中待平台下发的 Client 凭据仍是 PENDING_ONBOARDING，错误信息
#      却容易被误读成文件权限问题。
# doctor 一次性把这些查出来并给出可执行建议。它绝不修改任何文件、不重启或创建容器。
# -----------------------------------------------------------------------------
doctor_failures=0
doctor_warnings=0
doctor_docker_available=false
