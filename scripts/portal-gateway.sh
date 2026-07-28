#!/usr/bin/env bash
# 把平台管理后台登记的子系 environment（path_prefix + upstream_url）渲染成 nginx
# 反向代理 location 配置，并 reload nginx。配合 applicationregistry 的
# Environment.UpstreamURL / Environment.PathPrefix 字段使用。
#
# 注意：这是底层网关维护工具。完整子系统接入请使用 scripts/subsystem-onboarding.sh，
# 不要仅执行 add/reload，否则不会创建 Application、Environment、LoginTarget 和 OAuth Client。
#
# 设计：所有用户访问都从门户统一入口进入，门户的 frontend nginx 把
# 配置的 path_prefix（例如 /contract/）反代到子系真实内网地址。这样子系不需要对外暴露端口，
# 也不需要在 OAuth redirect_uri、LoginTarget.TargetURI 里写任何子系端口。
#
# 用法：
#   portal-gateway.sh add <code> <path_prefix> <upstream_url>
#   portal-gateway.sh remove <code>
#   portal-gateway.sh list
#   portal-gateway.sh sync   # 仅在部署管理 API 认证适配层后拉取并全量重写
#   portal-gateway.sh reload
#   portal-gateway.sh apply  # 仅在 sync 可认证时执行 sync + reload
#
# 维护的 nginx include 文件默认写入项目的 docker/portal-apps-locations.conf，由 compose 挂载到 frontend Nginx。
# 传统宿主机 Nginx 可通过 PORTAL_GATEWAY_NGINX_INCLUDE 覆盖该路径。

set -Eeuo pipefail
IFS=$'\n\t'
umask 077

PROJECT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
NGINX_INCLUDE="${PORTAL_GATEWAY_NGINX_INCLUDE:-${PROJECT_ROOT}/docker/portal-apps-locations.conf}"
NGINX_RELOAD_CMD="${PORTAL_GATEWAY_NGINX_RELOAD_CMD:-}"
COMPOSE_FILE="${PORTAL_GATEWAY_COMPOSE_FILE:-}"
API_BASE_URL="${PORTAL_GATEWAY_API_BASE_URL:-http://127.0.0.1:8080}"
API_TOKEN="${PORTAL_GATEWAY_API_TOKEN:-}"
PAGE_LIMIT="${PORTAL_GATEWAY_PAGE_LIMIT:-100}"
# 这些模块已经编译进统一 Vue 前端，只允许代理其后端 API 子路径，不能生成整站反代。
INTEGRATED_FRONTEND_CODES="${PORTAL_GATEWAY_INTEGRATED_FRONTEND_CODES:-contract_management,contract-management}"

log() {
  local level="$1"
  shift
  printf '[%s] [%s] %s\n' "$(date '+%Y-%m-%d %H:%M:%S')" "$level" "$*" >&2
}

usage() {
  cat <<'USAGE'
用法：
  portal-gateway.sh <command> [args]

完整接入：
  bash scripts/subsystem-onboarding.sh --help
  本脚本仅维护网关，不创建应用、环境、登录目标或 OAuth 客户端。

命令：
  add <code> <path_prefix> <upstream_url>
      追加或替换一个子系的反代 location。
  remove <code>
      移除一个子系的反代 location。
  list
      列出当前已注册的子系（按 code 字母序）。
  sync
      从平台管理后台拉所有 ACTIVE application/environment，全量重写 include 文件。
      当前管理接口默认只接受平台会话 Cookie，而本命令发送 Bearer Token；默认部署不可直接使用。
      只有部署了受控认证适配层后，才可通过 PORTAL_GATEWAY_API_BASE_URL 和
      PORTAL_GATEWAY_API_TOKEN 提供 application/environment 读权限。
  reload
      触发 nginx 重载配置。
  apply
      在 sync 已具备受控认证适配层时同步配置，并在成功后立即重载 nginx。

环境变量：
  PORTAL_GATEWAY_NGINX_INCLUDE     include 文件绝对路径
  PORTAL_GATEWAY_NGINX_RELOAD_CMD  自定义 reload 命令（设置后优先使用）
  PORTAL_GATEWAY_COMPOSE_FILE       frontend 所在 Compose 文件；未设置时自动探测
  PORTAL_GATEWAY_API_BASE_URL      sync 认证适配层的平台 API 入口
  PORTAL_GATEWAY_API_TOKEN         认证适配层接受的 Bearer token
  PORTAL_GATEWAY_PAGE_LIMIT        列表接口单页大小
USAGE
}

require_arg() {
  if [[ -z "${1:-}" ]]; then
    log "ERROR" "$2"
    exit 2
  fi
}

is_integrated_frontend_code() {
  local target="$1" item
  local items=()
  IFS=',' read -r -a items <<<"$INTEGRATED_FRONTEND_CODES"
  for item in "${items[@]}"; do
    item="${item#${item%%[![:space:]]*}}"
    item="${item%${item##*[![:space:]]}}"
    [[ "$target" != "$item" ]] || return 0
  done
  return 1
}

validate_code() {
  local code="$1"
  # sync 会用 <application-code>-<environment> 生成稳定且唯一的条目标识。
  if [[ ! "$code" =~ ^[a-z0-9][a-z0-9_.-]{0,79}$ ]]; then
    log "ERROR" "code 必须是 ^[a-z0-9][a-z0-9_.-]{0,79}\$：$code"
    exit 2
  fi
}

validate_path_prefix() {
  local prefix="$1"
  # 门户根路径保留给门户自身；不接受重复斜杠、点段、编码字符或 nginx 特殊字符。
  local pattern='^/[A-Za-z0-9._~!+/\-]*$'
  if [[ ${#prefix} -gt 128 || "$prefix" == "/" || ! "$prefix" =~ $pattern ]]; then
    log "ERROR" "path_prefix 必须是非根绝对路径（最长 128，字母数字 . _ ~ ! - + /）：$prefix"
    exit 2
  fi
  if [[ "$prefix" == *"//"* ]]; then
    log "ERROR" "path_prefix 不能包含 //：$prefix"
    exit 2
  fi
  local segment
  local segments=()
  IFS='/' read -r -a segments <<<"$prefix"
  for segment in "${segments[@]}"; do
    if [[ "$segment" == "." || "$segment" == ".." ]]; then
      log "ERROR" "path_prefix 不能包含 . 或 .. 路径段：$prefix"
      exit 2
    fi
  done
}

validate_upstream_url() {
  local url="$1"
  # 支持域名、IPv4 和方括号 IPv6；禁止 query/fragment/空白及 nginx 配置注入字符。
  local pattern='^https?://(\[[0-9A-Fa-f:.]+\]|[A-Za-z0-9._~-]+)(:([0-9]{1,5}))?(/[A-Za-z0-9._~!%+/@-]*)?$'
  if [[ ${#url} -gt 512 || ! "$url" =~ $pattern ]]; then
    log "ERROR" "upstream_url 必须是安全的 http(s) 绝对 URL（支持 [IPv6]:port）：$url"
    exit 2
  fi
  local port="${BASH_REMATCH[3]:-}"
  if [[ -n "$port" && (10#$port -lt 1 || 10#$port -gt 65535) ]]; then
    log "ERROR" "upstream_url 端口必须在 1..65535：$url"
    exit 2
  fi
}

validate_page_limit() {
  if [[ ! "$PAGE_LIMIT" =~ ^[0-9]+$ ]] || (( PAGE_LIMIT < 1 || PAGE_LIMIT > 100 )); then
    log "ERROR" "PORTAL_GATEWAY_PAGE_LIMIT 必须在 1..100（后端最大页大小为 100）：$PAGE_LIMIT"
    exit 2
  fi
}

trim_trailing_slash() {
  local value="$1"
  value="${value%/}"
  printf '%s' "$value"
}

ensure_include_file() {
  local file="$1"
  mkdir -p "$(dirname -- "$file")"
  if [[ ! -f "$file" ]]; then
    install -m 0644 /dev/null "$file"
    cat > "$file" <<'HEADER'
# DO NOT EDIT: managed by scripts/portal-gateway.sh.
# 任何手动修改会在下一次 add/remove/sync 时被覆盖。
HEADER
  fi
}

render_location() {
  local code="$1"
  local path_prefix="$2"
  local upstream_url="$3"
  local trimmed_prefix
  trimmed_prefix="$(trim_trailing_slash "$path_prefix")"
  local trimmed_upstream
  trimmed_upstream="$(trim_trailing_slash "$upstream_url")"
  cat <<EOF

# redirect code=${code}
location = ${trimmed_prefix} {
    return 308 ${trimmed_prefix}/;
}

# code=${code}
location ${trimmed_prefix}/ {
    proxy_pass ${trimmed_upstream}/;
    proxy_http_version 1.1;
    proxy_set_header Host \$host;
    proxy_set_header X-Real-IP \$remote_addr;
    proxy_set_header X-Forwarded-For \$proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto \$scheme;
    proxy_set_header X-Forwarded-Host \$host;
    proxy_set_header X-Forwarded-Prefix ${trimmed_prefix};
}
EOF
}

parse_entry() {
  # 解析 include 文件中由本脚本写入的 entry（用注释行 # code=xxx 标识）。
  # 输出 "code<TAB>prefix<TAB>upstream"。只依赖 grep / sed / awk 基础功能。
  awk '
    function trim(s) { sub(/^[[:space:]]+|[[:space:]]+$/, "", s); return s }
    /^# code=/ {
      current_code = substr($0, length("# code=") + 1)
      in_block = 1
      prefix = ""
      upstream = ""
      next
    }
    in_block {
      if ($0 ~ /^[[:space:]]*location[[:space:]]+/) {
        # location 后面第一个非空白 token 就是前缀；忽略尾部 { 和空白。
        line = $0
        sub(/^[[:space:]]*location[[:space:]]+/, "", line)
        sub(/[[:space:]]*\{.*$/, "", line)
        prefix = trim(line)
      } else if ($0 ~ /proxy_pass[[:space:]]+/) {
        line = $0
        sub(/^[[:space:]]*proxy_pass[[:space:]]+/, "", line)
        sub(/;[[:space:]]*$/, "", line)
        upstream = trim(line)
      } else if ($0 ~ /^[[:space:]]*\}[[:space:]]*$/) {
        if (current_code != "" && prefix != "" && upstream != "") {
          printf "%s\t%s\t%s\n", current_code, prefix, upstream
        }
        current_code = ""
        in_block = 0
      }
    }
  ' "$1"
}

write_combined() {
  local file="$1"
  local mode="$2"  # "rewrite" | "append"
  shift 2
  local tmp
  tmp="$(mktemp)"
  if [[ "$mode" == "rewrite" ]]; then
    cat > "$tmp" <<'HEADER'
# DO NOT EDIT: managed by scripts/portal-gateway.sh.
# 任何手动修改会在下一次 add/remove/sync 时被覆盖。
HEADER
  else
    cp -- "$file" "$tmp"
  fi
  local entry entry_code entry_prefix entry_upstream
  for entry in "$@"; do
    IFS=$'\t' read -r entry_code entry_prefix entry_upstream <<<"$entry"
    render_location "$entry_code" "$entry_prefix" "$entry_upstream" >> "$tmp"
  done
  # 该文件通常以单文件方式 bind mount 到 Nginx 容器。不能用 mv 替换 inode，
  # 否则运行中的容器仍会读取旧文件；保留 inode 并覆盖内容。
  cat -- "$tmp" > "$file"
  rm -f -- "$tmp"
  chmod 0644 "$file"
}

do_add() {
  local code="$1" path_prefix="$2" upstream_url="$3"
  validate_code "$code"
  validate_path_prefix "$path_prefix"
  validate_upstream_url "$upstream_url"

  if is_integrated_frontend_code "$code"; then
    do_remove "$code"
    log "INFO" "${code} 已内置于统一前端，跳过整站反向代理登记"
    return 0
  fi

  ensure_include_file "$NGINX_INCLUDE"

  local entries=()
  if [[ -s "$NGINX_INCLUDE" ]]; then
    while IFS=$'\t' read -r existing_code existing_prefix existing_upstream; do
      if [[ -n "$existing_code" && "$existing_code" != "$code" ]]; then
        if [[ "$(trim_trailing_slash "$existing_prefix")" == "$(trim_trailing_slash "$path_prefix")" ]]; then
          log "ERROR" "path_prefix 已被子系 ${existing_code} 占用：$path_prefix"
          exit 2
        fi
        entries+=("${existing_code}"$'\t'"${existing_prefix}"$'\t'"${existing_upstream}")
      fi
    done < <(parse_entry "$NGINX_INCLUDE")
  fi

  entries+=("${code}"$'\t'"${path_prefix}"$'\t'"${upstream_url}")
  write_combined "$NGINX_INCLUDE" rewrite "${entries[@]}"
  log "INFO" "已注册子系 ${code} -> ${path_prefix} -> ${upstream_url}"
}

do_remove() {
  local code="$1"
  validate_code "$code"

  if [[ ! -f "$NGINX_INCLUDE" ]]; then
    log "INFO" "include 文件不存在，无需清理"
    return
  fi

  local entries=()
  local found=false
  while IFS=$'\t' read -r existing_code existing_prefix existing_upstream; do
    if [[ -n "$existing_code" && "$existing_code" != "$code" ]]; then
      entries+=("${existing_code}"$'\t'"${existing_prefix}"$'\t'"${existing_upstream}")
    elif [[ "$existing_code" == "$code" ]]; then
      found=true
    fi
  done < <(parse_entry "$NGINX_INCLUDE")

  if [[ "$found" != "true" ]]; then
    log "INFO" "子系 ${code} 不存在，跳过"
    return
  fi
  if (( ${#entries[@]} > 0 )); then
    write_combined "$NGINX_INCLUDE" rewrite "${entries[@]}"
  else
    # macOS 自带 Bash 3.2 在 set -u 下展开空数组会报 unbound variable。
    write_combined "$NGINX_INCLUDE" rewrite
  fi
  log "INFO" "已移除子系 ${code}"
}

do_list() {
  if [[ ! -f "$NGINX_INCLUDE" ]]; then
    log "INFO" "include 文件不存在"
    return
  fi
  printf '%-32s %-40s %s\n' "code" "path_prefix" "upstream_url"
  parse_entry "$NGINX_INCLUDE" | while IFS=$'\t' read -r code prefix upstream; do
    printf '%-32s %-40s %s\n' "$code" "$prefix" "$upstream"
  done
}

resolve_compose_file() {
  if [[ -n "$COMPOSE_FILE" ]]; then
    if [[ ! -f "$COMPOSE_FILE" ]]; then
      log "ERROR" "PORTAL_GATEWAY_COMPOSE_FILE 不存在：$COMPOSE_FILE"
      return 1
    fi
    printf '%s' "$COMPOSE_FILE"
    return
  fi

  local candidate
  for candidate in "${PROJECT_ROOT}/compose.local.yaml" "${PROJECT_ROOT}/compose.yaml"; do
    [[ -f "$candidate" ]] || continue
    if docker compose -f "$candidate" ps --status running --services 2>/dev/null | grep -Fxq 'frontend'; then
      printf '%s' "$candidate"
      return
    fi
  done

  log "ERROR" "未找到正在运行的 frontend 服务；请先启动平台，或设置 PORTAL_GATEWAY_COMPOSE_FILE"
  return 1
}

do_reload() {
  if [[ -n "$NGINX_RELOAD_CMD" ]]; then
    log "INFO" "触发自定义 nginx reload: ${NGINX_RELOAD_CMD}"
    bash -c "$NGINX_RELOAD_CMD"
    return
  fi

  local compose_file
  compose_file="$(resolve_compose_file)"
  log "INFO" "校验 nginx 配置: docker compose -f ${compose_file} exec -T frontend nginx -t"
  docker compose -f "$compose_file" exec -T frontend nginx -t
  log "INFO" "触发 nginx reload: docker compose -f ${compose_file} exec -T frontend nginx -s reload"
  docker compose -f "$compose_file" exec -T frontend nginx -s reload
}

# sync 子命令：从平台管理后台拉取所有 ACTIVE 的 application/environment，
# 全量重写 include 文件。当前管理接口默认使用会话 Cookie，而此实现发送 Bearer Token；
# 只有部署受控认证适配层并授予只读权限后才能使用。默认部署请使用 add + reload。
do_sync() {
  require_arg "$API_TOKEN" "sync 需要 PORTAL_GATEWAY_API_TOKEN"
  validate_page_limit
  ensure_include_file "$NGINX_INCLUDE"

  local sync_input
  sync_input="$(mktemp "${NGINX_INCLUDE}.sync-input.XXXXXX")"
  local page=1
  while :; do
    local payload
    payload=$(curl -fsS -G \
      --data-urlencode "page=${page}" \
      --data-urlencode "page_size=${PAGE_LIMIT}" \
      --data-urlencode "status=ACTIVE" \
      -H "Authorization: Bearer ${API_TOKEN}" \
      "${API_BASE_URL}/api/v1/applications") || {
      rm -f "$sync_input"
      log "ERROR" "拉取 application 列表失败（page=${page}）"
      return 1
    }
    if ! printf '%s' "$payload" | jq -e '.data.items | arrays' >/dev/null; then
      rm -f "$sync_input"
      log "ERROR" "application 列表响应格式无效（page=${page}）"
      return 1
    fi

    local count
    count=$(printf '%s' "$payload" | jq -r '.data.items | length')
    if [[ "$count" -eq 0 ]]; then
      break
    fi

    while IFS=$'\t' read -r application_id application_code; do
      [[ -z "$application_id" || -z "$application_code" ]] && continue
      if is_integrated_frontend_code "$application_code"; then
        log "INFO" "跳过统一前端内置模块的整站反代：${application_code}"
        continue
      fi
      local environment_page=1
      while :; do
        local env_payload
        env_payload=$(curl -fsS -G \
          --data-urlencode "page=${environment_page}" \
          --data-urlencode "page_size=${PAGE_LIMIT}" \
          --data-urlencode "status=ACTIVE" \
          -H "Authorization: Bearer ${API_TOKEN}" \
          "${API_BASE_URL}/api/v1/applications/${application_id}/environments") || {
          rm -f "$sync_input"
          log "ERROR" "拉取 environment 列表失败（application=${application_id}, page=${environment_page}）"
          return 1
        }
        if ! printf '%s' "$env_payload" | jq -e '.data.items | arrays' >/dev/null; then
          rm -f "$sync_input"
          log "ERROR" "environment 列表响应格式无效（application=${application_id}, page=${environment_page}）"
          return 1
        fi

        while IFS=$'\t' read -r environment_code prefix upstream; do
          [[ -z "$environment_code" || -z "$prefix" || -z "$upstream" ]] && continue
          local entry_code="${application_code}-${environment_code}"
          validate_code "$entry_code"
          validate_path_prefix "$prefix"
          validate_upstream_url "$upstream"
          printf '%s\t%s\t%s\n' "$entry_code" "$prefix" "$upstream" >> "$sync_input"
        done < <(printf '%s' "$env_payload" | jq -r '
          .data.items[]
          | select(.upstream_url != null and .upstream_url != "" and .path_prefix != null and .path_prefix != "")
          | [.environment, .path_prefix, .upstream_url]
          | @tsv
        ')

        local environment_total
        environment_total=$(printf '%s' "$env_payload" | jq -r '.data.total // 0')
        if (( environment_page * PAGE_LIMIT >= environment_total )); then
          break
        fi
        environment_page=$((environment_page + 1))
      done
    done < <(printf '%s' "$payload" | jq -r '.data.items[] | [.application_id, .code] | @tsv')

    local total
    total=$(printf '%s' "$payload" | jq -r '.data.total // 0')
    if (( page * PAGE_LIMIT >= total )); then
      break
    fi
    page=$((page + 1))
  done

  local duplicate_prefix
  duplicate_prefix=$(cut -f2 "$sync_input" | sed 's:/*$::' | sort | uniq -d)
  if [[ -n "$duplicate_prefix" ]]; then
    rm -f "$sync_input"
    log "ERROR" "多个 ACTIVE environment 使用了同一 path_prefix：$duplicate_prefix"
    return 1
  fi

  local rendered
  rendered="$(mktemp "${NGINX_INCLUDE}.rendered.XXXXXX")"
  cat > "$rendered" <<'HEADER'
# DO NOT EDIT: managed by scripts/portal-gateway.sh sync.
# 任何手动修改会在下一次 add/remove/sync 时被覆盖。
HEADER
  while IFS=$'\t' read -r code prefix upstream; do
    [[ -z "$code" || -z "$prefix" || -z "$upstream" ]] && continue
    render_location "$code" "$prefix" "$upstream" >> "$rendered"
  done < <(sort -t $'\t' -k1,1 "$sync_input")

  # 保留单文件 bind mount 的 inode，确保运行中的 Nginx 容器看到新内容。
  cat -- "$rendered" > "$NGINX_INCLUDE"
  rm -f -- "$rendered" "$sync_input"
  chmod 0644 "$NGINX_INCLUDE"
  log "INFO" "sync 完成，请执行 reload 让 nginx 生效"
}

main() {
  local command="${1:-}"
  if [[ -z "$command" ]]; then
    usage
    exit 1
  fi
  case "$command" in
    add) shift; require_arg "${1:-}" "缺少 <code>"; require_arg "${2:-}" "缺少 <path_prefix>"; require_arg "${3:-}" "缺少 <upstream_url>"; do_add "$1" "$2" "$3" ;;
    remove) shift; require_arg "${1:-}" "缺少 <code>"; do_remove "$1" ;;
    list) do_list ;;
    sync) do_sync ;;
    reload) do_reload ;;
    apply) do_sync && do_reload ;;
    -h|--help|help) usage ;;
    *) log "ERROR" "未知命令: $command"; usage; exit 1 ;;
  esac
}

main "$@"
