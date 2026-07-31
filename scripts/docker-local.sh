#!/usr/bin/env bash
# 统一 Docker 编排入口：一个前端、一个基础平台后端、一个合同管理后端。
set -Eeuo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
project_root="$(cd "${script_dir}/.." && pwd)"
workspace_root="$(cd "${project_root}/.." && pwd)"
contract_root="${workspace_root}/contract_management"
compose_file="${project_root}/compose.local.yaml"
platform_template_file="${project_root}/docker/.env.local.example"
env_file="${project_root}/docker/.env.local"
contract_template_file="${contract_root}/.env.example"
contract_env_file="${contract_root}/.env.local"
lan_override_file="${project_root}/docker/.env.lan"
lan_placeholder_file="${project_root}/docker/.env.lan.disabled"
compose_project="basic-platform-local"
command_name="up"
force_build=false
admin_display_name="${BASIC_PLATFORM_ADMIN_DISPLAY_NAME:-}"
admin_account_name="${BASIC_PLATFORM_ADMIN_ACCOUNT_NAME:-}"
admin_password="${BASIC_PLATFORM_ADMIN_PASSWORD:-}"
frontend_public_origin=""

log() { printf '[docker-local] %s\n' "$*"; }
fail() { printf '[docker-local] ERROR: %s\n' "$*" >&2; exit 1; }

usage() {
    cat <<USAGE
用法：
  bash scripts/docker-local.sh up [选项]
  bash scripts/docker-local.sh down [--volumes]
  bash scripts/docker-local.sh stop
  bash scripts/docker-local.sh restart [选项]
  bash scripts/docker-local.sh ps
  bash scripts/docker-local.sh logs [服务名...]
  bash scripts/docker-local.sh config
  bash scripts/docker-local.sh verify
  bash scripts/docker-local.sh refresh-api
  bash scripts/docker-local.sh refresh-frontend
  bash scripts/docker-local.sh refresh-contract-api

up/restart 选项：
  --build                         重新构建三个业务镜像
  --pull                          兼容选项；启动前默认会串行拉取缺失的基础镜像
  --admin-display-name NAME       首次初始化超级管理员显示名称
  --admin-account-name NAME       首次初始化超级管理员账号
  --admin-password PASSWORD       首次初始化密码；仅建议本地临时使用
  --env-file PATH                 基础平台环境文件（默认 platform/docker/.env.local）
  --contract-env-file PATH        合同后端环境文件（默认 contract_management/.env.local）
  -h, --help                      显示帮助

定向更新：
  refresh-api           只重建基础平台后端镜像，执行基础平台迁移，并重启 api/受控 provisioner
  refresh-frontend      只重建并重启统一 frontend；基础平台前端与合同管理前端同时更新
  refresh-contract-api  只重建合同管理后端镜像，执行合同迁移，并重启 contract-api

  三种定向更新都不会删除或重建 Application、Environment、LoginTarget、OAuth Client，
  因此不会影响已经完成的子系统统一登录接入。

应用容器：
  frontend      基础平台前端 + 合同管理前端（宿主机仅发布 8081）
  api           基础平台 API + Worker
  contract-api  合同管理 API + Temporal Worker

首次启动若数据库中尚未存在超级管理员，必须提供三个管理员参数，或设置：
  BASIC_PLATFORM_ADMIN_DISPLAY_NAME
  BASIC_PLATFORM_ADMIN_ACCOUNT_NAME
  BASIC_PLATFORM_ADMIN_PASSWORD
USAGE
}

remove_volumes=false
while (($# > 0)); do
    case "$1" in
        up|down|stop|restart|ps|logs|config|verify|refresh-api|refresh-frontend|refresh-contract-api)
            command_name="$1"
            shift
            ;;
        --build)
            force_build=true
            shift
            ;;
        --pull)
            # 兼容旧命令；缺失基础镜像现在默认会预拉取。
            shift
            ;;
        --volumes)
            remove_volumes=true
            shift
            ;;
        --admin-display-name)
            (($# >= 2)) || fail "$1 缺少参数"
            admin_display_name="$2"
            shift 2
            ;;
        --admin-account-name)
            (($# >= 2)) || fail "$1 缺少参数"
            admin_account_name="$2"
            shift 2
            ;;
        --admin-password)
            (($# >= 2)) || fail "$1 缺少参数"
            admin_password="$2"
            shift 2
            ;;
        --env-file)
            (($# >= 2)) || fail "$1 缺少参数"
            env_file="$2"
            shift 2
            ;;
        --contract-env-file)
            (($# >= 2)) || fail "$1 缺少参数"
            contract_env_file="$2"
            shift 2
            ;;
        -h|--help)
            usage
            exit 0
            ;;
        *)
            break
            ;;
    esac
done

if [[ "$command_name" == "logs" && $# -gt 0 ]]; then
    log_services=("$@")
else
    log_services=()
fi

command -v docker >/dev/null 2>&1 || fail "未找到 docker 命令"
docker compose version >/dev/null 2>&1 || fail "当前 Docker 不支持 docker compose 子命令"
[[ -f "$compose_file" ]] || fail "Compose 文件不存在：$compose_file"
[[ -d "$contract_root" ]] || fail "合同管理后端目录不存在：$contract_root"

export BASIC_PLATFORM_RUNTIME_ENV_FILE="$env_file"
export CONTRACT_RUNTIME_ENV_FILE="$contract_env_file"
export BASIC_PLATFORM_HOST_PROJECT_ROOT="$project_root"
export SUBSYSTEM_HOST_PROJECTS_ROOT="$workspace_root"

# scripts/lan-access.sh 通过 docker/.env.lan 标记临时局域网模式。docker-local
# 必须继续为基础平台和合同后端加载同一份覆盖配置，否则基础平台发布的
# OIDC issuer 与合同后端期望的 issuer 会分别落到 LAN 地址和 localhost，
# contract-api 将因 discovery 校验失败而持续重启。
#
# 同时：每次 up 启动时实时检测当前网络的 IPv4 地址（不再信任 docker/.env.lan
# 里写死的值）。当网段切换、远程 ssh 或换 WiFi 后，up 仍然能用正确的 IP 配置
# 平台后端的 OIDC issuer / CORS allowed origin / 回调地址，避免 403。
is_ipv4() {
    local candidate="$1" octet
    [[ "$candidate" =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ ]] || return 1
    IFS='.' read -r -a octets <<< "$candidate"
    for octet in "${octets[@]}"; do
        ((10#$octet <= 255)) || return 1
    done
}

# 实时检测宿主机可用的第一个非 loopback IPv4。优先用 default route 对应接口
# 的地址，否则回退到 ifconfig 列表里第一个非 127.x 的 IPv4。
detect_current_lan_ipv4() {
    local iface candidate
    iface="$(route -n get default 2>/dev/null | awk '/interface:/{print $2; exit}')"
    if [[ -n "$iface" ]]; then
        candidate="$(ipconfig getifaddr "$iface" 2>/dev/null || true)"
        if is_ipv4 "$candidate" && [[ "$candidate" != 127.* ]]; then
            printf '%s' "$candidate"
            return 0
        fi
    fi

    while IFS= read -r candidate; do
        [[ -z "$candidate" ]] && continue
        is_ipv4 "$candidate" || continue
        [[ "$candidate" == 127.* ]] && continue
        printf '%s' "$candidate"
        return 0
    done < <(ifconfig 2>/dev/null | awk '/inet / {print $2}')
    return 1
}

# 用新 IP 覆盖 docker/.env.lan 中所有 http://<旧IP>:<端口> 的值，
# 保持端口与 path 前缀不变，让 docker compose 重新加载后使用新地址。
rewrite_lan_override_file() {
    local new_ip="$1" file_ip="$2"
    local file_port
    file_port="$(awk -F= '$1 == "APP_PUBLIC_BASE_URL" {print substr($0, index($0, "=") + 1); exit}' "$lan_override_file" | sed -E 's|^http://[^:]+:||; s|/.*$||')"
    [[ -z "$file_port" ]] && file_port="${FRONTEND_HTTP_PORT:-8081}"
    sed -i.bak -E "s|http://${file_ip}:${file_port}|http://${new_ip}:${file_port}|g" "$lan_override_file"
    rm -f "$lan_override_file.bak"
}

configure_access_mode() {
    local public_origin lan_port detected_ip file_ip

    [[ -f "$lan_placeholder_file" ]] || fail "局域网占位环境文件不存在：$lan_placeholder_file"

    if [[ ! -f "$lan_override_file" ]]; then
        export BASIC_PLATFORM_LAN_OVERRIDE_ENV_FILE="$lan_placeholder_file"
        export CONTRACT_LAN_OVERRIDE_ENV_FILE="$lan_placeholder_file"
        export FRONTEND_BIND_ADDRESS="${FRONTEND_BIND_ADDRESS:-127.0.0.1}"
        frontend_public_origin="http://localhost:${FRONTEND_HTTP_PORT:-8081}"
        return 0
    fi

    file_ip="$(awk -F= '$1 == "APP_PUBLIC_BASE_URL" {print substr($0, index($0, "=") + 1); exit}' "$lan_override_file" | sed -E 's|^http://||; s|:.*$||')"
    detected_ip="$(detect_current_lan_ipv4 || true)"

    if [[ -z "$detected_ip" ]]; then
        log "WARN: 未检测到可用 IPv4，沿用 docker/.env.lan 中写死的 $file_ip"
    elif [[ "$detected_ip" != "$file_ip" ]]; then
        log "WARN: 当前 IPv4 ($detected_ip) 与 docker/.env.lan 中写死的 $file_ip 不一致，自动用检测值；如需落盘可执行 bash scripts/lan-access.sh enable"
        rewrite_lan_override_file "$detected_ip" "$file_ip"
    fi

    public_origin="$(awk -F= '$1 == "APP_PUBLIC_BASE_URL" {print substr($0, index($0, "=") + 1); exit}' "$lan_override_file")"
    [[ "$public_origin" =~ ^http://([0-9]{1,3}\.){3}[0-9]{1,3}:[1-9][0-9]{0,4}$ ]] || \
        fail "局域网覆盖文件中的 APP_PUBLIC_BASE_URL 无效：${public_origin:-'(空)'}；请重新执行 bash scripts/lan-access.sh enable"

    lan_port="${public_origin##*:}"
    ((lan_port <= 65535)) || fail "局域网覆盖文件中的端口无效：$lan_port"

    export BASIC_PLATFORM_LAN_OVERRIDE_ENV_FILE="$lan_override_file"
    export CONTRACT_LAN_OVERRIDE_ENV_FILE="$lan_override_file"
    export FRONTEND_BIND_ADDRESS="0.0.0.0"
    export FRONTEND_HTTP_PORT="$lan_port"
    frontend_public_origin="$public_origin"
    log "统一访问地址：$public_origin"
}

configure_access_mode

compose() {
    docker compose \
        --project-name "$compose_project" \
        --file "$compose_file" \
        --env-file "$env_file" \
        --env-file "$contract_env_file" \
        "$@"
}

replace_line_in_file() {
    local file="$1" key="$2" value="$3" tmp
    tmp="$(mktemp "${TMPDIR:-/tmp}/docker-local-env.XXXXXX")"
    awk -v key="$key" -v value="$value" '
        index($0, key "=") == 1 { print key "=" value; found=1; next }
        { print }
        END { if (!found) print key "=" value }
    ' "$file" > "$tmp"
    chmod 600 "$tmp"
    mv "$tmp" "$file"
}

random_hex() { openssl rand -hex "$1" | tr -d '\n'; }
random_key() { openssl rand -base64 32 | tr -d '\n'; }

ensure_platform_env_file() {
    command -v openssl >/dev/null 2>&1 || fail "未找到 openssl，无法生成本地密码和密钥"
    mkdir -p "$(dirname "$env_file")" "${project_root}/data/keys" "${project_root}/data/logs" "${project_root}/data/uploads"
    chmod 700 "${project_root}/data/keys" "${project_root}/data/logs" "${project_root}/data/uploads"

    if [[ ! -f "$env_file" ]]; then
        [[ -f "$platform_template_file" ]] || fail "基础平台环境模板不存在：$platform_template_file"
        cp "$platform_template_file" "$env_file"
        chmod 600 "$env_file"
        replace_line_in_file "$env_file" MYSQL_PASSWORD "$(random_hex 24)"
        replace_line_in_file "$env_file" MYSQL_ROOT_PASSWORD "$(random_hex 32)"
        replace_line_in_file "$env_file" IAM_MOBILE_ENCRYPTION_KEY "$(random_key)"
        replace_line_in_file "$env_file" IAM_BOOTSTRAP_TOKEN "$(random_hex 32)"
        log "已根据模板生成基础平台环境文件：$env_file"
    fi

    local unresolved
    unresolved="$(grep -E 'REPLACE_WITH_' "$env_file" | grep -v '^#' || true)"
    [[ -z "$unresolved" ]] || fail "基础平台环境文件仍包含未填写占位符：$env_file"
}

ensure_contract_env_file() {
    local strict="${1:-false}"
    command -v openssl >/dev/null 2>&1 || fail "未找到 openssl，无法生成合同数据库密码"

    if [[ ! -f "$contract_env_file" ]]; then
        [[ -f "$contract_template_file" ]] || fail "合同管理环境模板不存在：$contract_template_file"
        cp "$contract_template_file" "$contract_env_file"
        chmod 600 "$contract_env_file"
        replace_line_in_file "$contract_env_file" CONTRACT_MYSQL_PASSWORD "$(random_hex 24)"
        replace_line_in_file "$contract_env_file" CONTRACT_MYSQL_ROOT_PASSWORD "$(random_hex 32)"
        replace_line_in_file "$contract_env_file" PLATFORM_BASE_URL "http://localhost:8081"
        replace_line_in_file "$contract_env_file" OIDC_ISSUER "http://localhost:8081"
        replace_line_in_file "$contract_env_file" OIDC_REDIRECT_URI "http://localhost:8081/contract_management/auth/callback"
        replace_line_in_file "$contract_env_file" APP_PUBLIC_URL "http://localhost:8081/contract_management/dashboard"
        replace_line_in_file "$contract_env_file" APP_PATH_PREFIX "/contract_management"
        log "已生成合同管理环境文件：$contract_env_file"
        log "请先在基础平台完成合同管理系统接入，并填写 OIDC_CLIENT_ID、OIDC_CLIENT_SECRET、OIDC_TENANT_ID。"
    fi

    if [[ "$strict" == true ]]; then
        local unresolved
        unresolved="$(grep -E 'REPLACE_WITH_' "$contract_env_file" | grep -v '^#' || true)"
        [[ -z "$unresolved" ]] || fail "合同管理环境文件仍包含接入占位符：$contract_env_file"
    fi
}

compose_run() {
    compose --ansi never "$@"
}

# Compose --wait may return as soon as a newly-created service is reported unhealthy,
# even when the process is still completing a recoverable cold start. Start related
# services in bounded stages and retry the wait once; if the service still cannot become
# healthy, print the relevant status and logs instead of leaving only the generic
# "dependency failed to start" message.
compose_up_wait() {
    local description="$1"
    shift
    local attempt

    for attempt in 1 2; do
        if compose_run up -d --wait "$@"; then
            return 0
        fi
        if [[ "$attempt" -eq 1 ]]; then
            log "WARN: ${description}首次健康等待未通过，输出状态并在 5 秒后继续等待"
            compose_run ps "$@" >&2 || true
            sleep 5
            continue
        fi
        log "ERROR: ${description}启动失败，以下为容器状态和最近日志"
        compose_run ps -a "$@" >&2 || true
        compose_run logs --no-color --tail=200 "$@" >&2 || true
        return 1
    done
}

pull_image_with_retry() {
    local image="$1" max_attempts="${2:-5}" attempt delay
    if docker image inspect "$image" >/dev/null 2>&1; then
        log "基础镜像已存在，跳过拉取：$image"
        return 0
    fi
    for ((attempt = 1; attempt <= max_attempts; attempt++)); do
        log "串行拉取基础镜像（${attempt}/${max_attempts}）：$image"
        if docker pull "$image"; then return 0; fi
        if ((attempt < max_attempts)); then
            delay=$((attempt * 5))
            log "基础镜像拉取失败，${delay} 秒后重试：$image"
            sleep "$delay"
        fi
    done
    fail "基础镜像拉取失败：$image。Docker Hub 认证端点不可达；请检查 Docker Desktop 的 Proxies/镜像加速器配置后重试。"
}

prepare_base_images() {
    local image
    local images=(
        "golang:1.26.4-alpine"
        "alpine:3.21"
        "node:22-alpine"
        "nginx:1.27-alpine"
        "mysql:8.4"
        "temporalio/auto-setup:1.29.7"
    )
    log "检查并串行准备基础镜像"
    for image in "${images[@]}"; do pull_image_with_retry "$image"; done
}

prepare_go_backend_base_images() {
    local target_name="${1:-Go 后端}"
    local image
    local images=(
        "golang:1.26.4-alpine"
        "alpine:3.21"
    )
    log "检查并串行准备${target_name}所需基础镜像"
    for image in "${images[@]}"; do pull_image_with_retry "$image"; done
}

prepare_frontend_base_images() {
    local image
    local images=(
        "node:22-alpine"
        "nginx:1.27-alpine"
    )
    log "检查并串行准备统一前端所需基础镜像"
    for image in "${images[@]}"; do pull_image_with_retry "$image"; done
}

build_images() {
    # Compose/BuildKit 会并发解析多个 Docker Hub 基础镜像。网络或代理不稳定时，
    # 任意一次匿名令牌请求超时都会让整个构建立即失败。先串行拉取缺失镜像并
    # 对每个镜像重试，使普通的 `up --build` 在全新环境中也具备容错能力。
    prepare_base_images
    if [[ "$force_build" == true ]]; then
        log "重新构建统一前端、基础平台后端和合同管理后端镜像"
    else
        log "构建缺失或有变更的三个业务镜像"
    fi
    # migrate/bootstrap-admin/subsystem-provisioner 都复用 api 构建出的
    # basic-platform/backend:local；这里只构建三个业务镜像各一次。
    COMPOSE_PARALLEL_LIMIT=1 compose --profile bootstrap --ansi never build \
        api contract-api frontend
}

prepare_gateway_config() {
    local gateway_script="${project_root}/scripts/portal-gateway.sh"
    [[ -x "$gateway_script" ]] || chmod +x "$gateway_script"
    PORTAL_GATEWAY_NGINX_INCLUDE="${project_root}/docker/portal-apps-locations.conf" \
        "$gateway_script" remove contract_management >/dev/null
    log "已清理合同管理旧式整站反向代理；合同前端由统一 frontend 容器直接承载"
}

run_migrations() {
    log "启动两个 MySQL 与 Temporal，并等待健康检查"
    compose_run up -d --wait mysql contract-mysql temporal
    log "执行基础平台数据库迁移"
    compose_run run --rm --no-deps migrate ./migrate
    log "执行合同管理幂等数据库迁移"
    compose_run run --rm --no-deps contract-migrate
}

bootstrap_admin_if_needed() {
    local status_rc
    log "检查超级管理员是否已初始化"
    set +e
    compose_run --profile bootstrap run --rm --no-deps bootstrap-admin ./bootstrap-admin --status
    status_rc=$?
    set -e
    if [[ "$status_rc" -eq 0 ]]; then
        log "超级管理员已存在，跳过初始化"
        return 0
    fi
    [[ "$status_rc" -eq 3 ]] || fail "检查超级管理员状态失败，退出码：$status_rc"

    if [[ -z "$admin_display_name" && -t 0 ]]; then read -r -p '首次超级管理员显示名称：' admin_display_name; fi
    if [[ -z "$admin_account_name" && -t 0 ]]; then read -r -p '首次超级管理员账号：' admin_account_name; fi
    if [[ -z "$admin_password" && -t 0 ]]; then
        read -r -s -p '首次超级管理员密码：' admin_password
        printf '\n'
    fi
    [[ -n "$admin_display_name" ]] || fail "首次初始化需要 --admin-display-name 或 BASIC_PLATFORM_ADMIN_DISPLAY_NAME"
    [[ -n "$admin_account_name" ]] || fail "首次初始化需要 --admin-account-name 或 BASIC_PLATFORM_ADMIN_ACCOUNT_NAME"
    [[ -n "$admin_password" ]] || fail "首次初始化需要 --admin-password 或 BASIC_PLATFORM_ADMIN_PASSWORD"

    log "初始化第一个超级管理员"
    printf '%s\n' "$admin_password" | compose_run --profile bootstrap run --rm --no-deps -T bootstrap-admin ./bootstrap-admin \
        --display-name "$admin_display_name" \
        --account-name "$admin_account_name" \
        --password-stdin
    unset admin_password BASIC_PLATFORM_ADMIN_PASSWORD
}

# 从统一前端容器内部访问公开路径，返回第一次 HTTP 响应的状态码。
# 使用容器自带的 wget，避免部署主机必须额外安装 curl。
frontend_http_status() {
    local route="$1" output status

    output="$(
        compose_run exec -T frontend \
            wget -S -O /dev/null "http://127.0.0.1${route}" 2>&1 || true
    )"
    status="$(printf '%s\n' "$output" | awk '$1 ~ /^HTTP\// { print $2; exit }')"
    if [[ -z "$status" ]]; then
        printf '%s\n' "$output" >&2
        return 1
    fi
    printf '%s' "$status"
}

# 返回第一次重定向响应中的 Location。该函数用于验证合同系统实际生成的
# OIDC 授权地址，避免仅检查 /auth/login=302 而遗漏后续 /authorize 转发错误。
frontend_http_location() {
    local route="$1" output location

    output="$(
        compose_run exec -T frontend \
            wget -S -O /dev/null "http://127.0.0.1${route}" 2>&1 || true
    )"
    location="$(printf '%s\n' "$output" | awk 'tolower($1) == "location:" { sub(/\r$/, "", $2); print $2; exit }')"
    if [[ -z "$location" ]]; then
        printf '%s\n' "$output" >&2
        return 1
    fi
    printf '%s' "$location"
}

# 将公开绝对地址转换为前端容器内可访问的路径，并完整保留查询参数。
public_url_to_route() {
    local public_url="$1" route

    case "$public_url" in
        http://*|https://*)
            route="$(printf '%s' "$public_url" | sed -E 's#^https?://[^/]+##')"
            ;;
        /*)
            route="$public_url"
            ;;
        *)
            return 1
            ;;
    esac
    [[ "$route" == /* ]] || return 1
    printf '%s' "$route"
}

verify_gateway_routes() {
    local platform_membership_status contract_health_status contract_session_status contract_login_status
    local contract_authorize_url contract_authorize_route platform_authorize_status platform_login_url

    log "校验统一前端 Nginx 配置"
    compose_run exec -T frontend nginx -t >/dev/null

    # 任职关系接口属于基础平台 API。未携带会话时应被认证层拒绝为 401；
    # 404 代表前端网关或 api 容器仍在使用未包含该路由的旧版本。
    platform_membership_status="$(frontend_http_status /api/v1/memberships)" || \
        fail "无法访问基础平台任职关系接口"
    [[ "$platform_membership_status" == "401" ]] || \
        fail "基础平台任职关系接口返回 ${platform_membership_status}，预期未登录状态为 401；请重新构建并启动 api 与 frontend 容器"

    contract_health_status="$(frontend_http_status /contract_management/healthz)" || \
        fail "无法访问合同管理健康检查路径"
    [[ "$contract_health_status" == "200" ]] || \
        fail "合同管理健康检查路径返回 ${contract_health_status}，预期为 200"

    # 自检请求不携带登录 Cookie，因此 /auth/me 应返回 401。若返回 404，说明
    # /contract_management 前缀未被正确移除，部署必须立即失败。
    contract_session_status="$(frontend_http_status /contract_management/api/v1/auth/me)" || \
        fail "无法访问合同管理登录状态接口"
    [[ "$contract_session_status" == "401" ]] || \
        fail "合同管理登录状态接口返回 ${contract_session_status}，预期未登录状态为 401"

    contract_login_status="$(frontend_http_status /contract_management/auth/login)" || \
        fail "无法访问合同管理登录入口"
    [[ "$contract_login_status" == "302" ]] || \
        fail "合同管理登录入口返回 ${contract_login_status}，预期为 302 OIDC 跳转"

    # 继续验证合同后端生成的真实授权地址。若 Nginx 在转发 /authorize 时丢失
    # client_id、redirect_uri、state、nonce 或 PKCE 参数，此处会得到 400 并阻止部署。
    contract_authorize_url="$(frontend_http_location /contract_management/auth/login)" || \
        fail "合同管理登录入口未返回 OIDC 授权地址"
    contract_authorize_route="$(public_url_to_route "$contract_authorize_url")" || \
        fail "合同管理登录入口返回了无法识别的 OIDC 授权地址"
    [[ "$contract_authorize_route" == /authorize\?* ]] || \
        fail "合同管理登录入口未跳转到统一身份平台 /authorize"

    platform_authorize_status="$(frontend_http_status "$contract_authorize_route")" || \
        fail "无法通过统一前端访问 OIDC 授权端点"
    [[ "$platform_authorize_status" == "302" ]] || \
        fail "OIDC 授权端点返回 ${platform_authorize_status}，预期未登录状态为 302；请检查 /authorize 是否完整保留查询参数"

    platform_login_url="$(frontend_http_location "$contract_authorize_route")" || \
        fail "OIDC 授权端点未返回统一登录页地址"
    case "$platform_login_url" in
        /login.html\?return_to=*|http://*/login.html\?return_to=*|https://*/login.html\?return_to=*) ;;
        *) fail "OIDC 授权端点未跳转到统一登录页" ;;
    esac

    log "合同管理 OIDC 网关校验通过：healthz=200，auth/me=401，auth/login=302，authorize=302"
}

start_stack() {
    ensure_platform_env_file
    ensure_contract_env_file true
    prepare_gateway_config
    build_images
    run_migrations
    bootstrap_admin_if_needed
    # 分阶段启动，避免 api、contract-api 和 frontend 在同一次 Compose wait 中
    # 把下游的短暂冷启动误报为整套部署失败，同时让错误日志能够准确指向服务。
    log "启动基础平台 API 与受控子系统 provisioner"
    compose_up_wait "基础平台 API" subsystem-provisioner api
    log "启动合同管理后端"
    compose_up_wait "合同管理后端" contract-api
    log "启动统一前端"
    compose_up_wait "统一前端" frontend
    verify_gateway_routes
    compose_run ps
    log "统一访问地址：${frontend_public_origin}"
    log "合同管理前端：${frontend_public_origin}/contract_management/"
}

refresh_platform_api() {
    # 仅刷新基础平台后端：当前 API 不会包含新增路由时，使用该命令即可。
    # compose() 固定携带合同环境文件，所以这里只保证它存在，不校验其 OIDC 占位符。
    prepare_operational_env
    prepare_go_backend_base_images "基础平台后端"

    log "重新构建基础平台后端镜像（不构建 frontend 或 contract-api）"
    COMPOSE_PARALLEL_LIMIT=1 compose --profile bootstrap --ansi never build api

    log "启动基础平台 MySQL，并执行基础平台数据库迁移"
    compose_run up -d --wait mysql
    compose_run run --rm --no-deps migrate ./migrate

    # api 依赖 provisioner 的 Unix Socket。先以新镜像重建 provisioner，再重启 api，
    # 避免 API 启动后连接到过期 helper 的情况。
    log "重建受控子系统 provisioner"
    compose_run up -d --wait --no-deps subsystem-provisioner
    log "重建基础平台 API"
    compose_run up -d --wait --no-deps api
    compose_run ps api subsystem-provisioner mysql
    log "基础平台后端已刷新；统一前端、合同管理后端和现有统一登录接入配置保持不变"
}

refresh_unified_frontend() {
    prepare_operational_env
    prepare_frontend_base_images
    prepare_gateway_config

    log "重新构建统一前端镜像（基础平台前端 + 合同管理前端）"
    COMPOSE_PARALLEL_LIMIT=1 compose --ansi never build frontend
    log "重建统一 frontend 容器；两个后端容器和统一登录接入配置保持不变"
    compose_run up -d --wait --no-deps frontend
    verify_gateway_routes
    compose_run ps frontend
}

refresh_contract_backend() {
    ensure_platform_env_file
    ensure_contract_env_file true
    prepare_go_backend_base_images "合同管理后端"

    log "重新构建合同管理后端镜像（不构建 frontend 或基础平台 api）"
    COMPOSE_PARALLEL_LIMIT=1 compose --ansi never build contract-api
    log "启动合同数据库与 Temporal，并执行合同管理数据库迁移"
    compose_run up -d --wait contract-mysql temporal
    compose_run run --rm --no-deps contract-migrate
    log "重建 contract-api 容器；统一前端、基础平台后端和统一登录接入配置保持不变"
    compose_run up -d --wait --no-deps contract-api
    verify_gateway_routes
    compose_run ps contract-api contract-mysql temporal
}

prepare_operational_env() {
    ensure_platform_env_file
    ensure_contract_env_file false
}

case "$command_name" in
    up|restart)
        start_stack
        ;;
    down)
        prepare_operational_env
        if [[ "$remove_volumes" == true ]]; then compose_run down --volumes; else compose_run down; fi
        ;;
    stop)
        prepare_operational_env
        compose_run stop
        ;;
    ps)
        prepare_operational_env
        compose_run ps
        ;;
    logs)
        prepare_operational_env
        if ((${#log_services[@]} > 0)); then compose_run logs -f "${log_services[@]}"; else compose_run logs -f; fi
        ;;
    config)
        ensure_platform_env_file
        ensure_contract_env_file true
        compose_run config --quiet
        log "Compose 配置校验通过"
        ;;
    verify)
        prepare_operational_env
        verify_gateway_routes
        ;;
    refresh-api)
        refresh_platform_api
        ;;
    refresh-frontend)
        refresh_unified_frontend
        ;;
    refresh-contract-api)
        refresh_contract_backend
        ;;
esac
