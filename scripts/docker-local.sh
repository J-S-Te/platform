#!/usr/bin/env bash
# 统一 Docker 编排入口：一个统一前端，以及彼此隔离的平台、合同、CRM 和客户门户后端。
set -Eeuo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
project_root="$(cd "${script_dir}/.." && pwd)"
workspace_root="$(cd "${project_root}/.." && pwd)"
contract_root="${workspace_root}/contract_management"
customer_root="${workspace_root}/customer_and_opportunity"
compose_file="${project_root}/compose.local.yaml"
platform_template_file="${project_root}/docker/.env.local.example"
env_file="${project_root}/docker/.env.local"
contract_template_file="${contract_root}/.env.example"
contract_env_file="${contract_root}/.env.local"
customer_template_file="${project_root}/docker/.env.customer.local.example"
customer_env_file="${project_root}/docker/.env.customer.local"
portal_template_file="${project_root}/docker/.env.portal.local.example"
portal_env_file="${project_root}/docker/.env.portal.local"
project_management_root="${workspace_root}/project_management"
project_template_file="${project_management_root}/.env.example"
project_env_file="${project_management_root}/.env.local"
data_analysis_root="${workspace_root}/data_analysis"
data_analysis_template_file="${data_analysis_root}/.env.example"
data_analysis_env_file="${data_analysis_root}/.env.local"
presale_worker_env_file="${customer_root}/.env.presale-worker"
lan_override_file="${project_root}/docker/.env.lan"
customer_lan_override_file="${project_root}/docker/.env.customer.lan"
lan_placeholder_file="${project_root}/docker/.env.lan.disabled"
compose_project="basic-platform-local"
command_name="up"
force_build=false
admin_display_name="${BASIC_PLATFORM_ADMIN_DISPLAY_NAME:-}"
admin_account_name="${BASIC_PLATFORM_ADMIN_ACCOUNT_NAME:-}"
admin_password="${BASIC_PLATFORM_ADMIN_PASSWORD:-}"
admin_password_stdin=false
frontend_public_origin=""
# 用于 Compose 首次解析的本地 CRM 目录基线。customer-api 真正启动前，
# sync_crm_authorization_catalog 会从当前镜像读取并导出实际哈希，因此这里
# 不替代运行时目录校验；它只避免 .env 模板占位符阻断初始化流程。
local_crm_role_config_hash="sha256:807e4520577f82966cdc8eb73ed974fa21994210e80e28517672a1d6ba049d2f"

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
  bash scripts/docker-local.sh logs [选项] [服务名...]
  bash scripts/docker-local.sh config
  bash scripts/docker-local.sh verify
  bash scripts/docker-local.sh refresh-api
  bash scripts/docker-local.sh refresh-frontend
  bash scripts/docker-local.sh refresh-contract-api
  bash scripts/docker-local.sh refresh-customer-api
  bash scripts/docker-local.sh refresh-portal-api
  bash scripts/docker-local.sh refresh-project-api
  bash scripts/docker-local.sh start-presale-worker [--presale-worker-env-file PATH]
  bash scripts/docker-local.sh start-presale-alert-worker

up/restart 选项：
  --build                         重新构建镜像并强制重建使用新镜像的业务容器
  --pull                          兼容选项；启动前默认会串行拉取缺失的基础镜像
  --admin-display-name NAME       首次初始化超级管理员显示名称
  --admin-account-name NAME       首次初始化超级管理员账号
  --admin-password PASSWORD       首次初始化密码；仅为兼容旧调用保留，不建议使用
  --admin-password-stdin          从标准输入读取首次初始化密码；适合 CI Secret
  --env-file PATH                 基础平台环境文件（默认 platform/docker/.env.local）
  --contract-env-file PATH        合同后端环境文件（默认 contract_management/.env.local）
  --customer-env-file PATH        客户与商机后端环境文件（默认 platform/docker/.env.customer.local）
  --portal-env-file PATH          客户自助门户环境文件（默认 platform/docker/.env.portal.local）
  --project-env-file PATH         项目管理系统环境文件（默认 project_management/.env.local）
  --data-analysis-env-file PATH  数据看板与统计分析环境文件（默认 data_analysis/.env.local）
  --presale-worker-env-file PATH  售前投递 Worker 环境文件（默认 customer_and_opportunity/.env.presale-worker）
  -h, --help                      显示帮助

日志取证选项：
  --since DURATION                仅显示指定时间范围内的日志，例如 10m、1h
  --tail COUNT                    每个服务显示的末尾行数（或 all），例如 200
  --no-follow                     一次性输出日志后退出（默认持续跟踪）

示例：
  bash scripts/docker-local.sh logs api
  bash scripts/docker-local.sh logs --since 10m --tail 200 --no-follow api
  bash scripts/docker-local.sh logs api subsystem-provisioner --since 30m --no-follow

定向更新：
  refresh-api           只重建基础平台后端镜像，执行基础平台迁移，并重启 api/受控 provisioner
  refresh-frontend      只重建并重启统一 frontend；六个前端模块同时更新
  refresh-contract-api  只重建合同管理后端镜像，执行合同迁移，并重启 contract-api
  refresh-customer-api  重建客户与商机管理后端、执行 CRM 迁移，并刷新统一前端网关
  refresh-portal-api    重建客户自助门户后端、执行 Portal 迁移；仅在已完成应用接入后启动
  refresh-project-api   重建项目管理系统后端、执行项目迁移；仅在已完成应用接入后启动
  refresh-data-analysis-api  重建数据看板后端（dashboard-api + 聚合 Worker）、执行聚合库迁移；仅在已完成应用接入后启动
  start-presale-worker  构建并启动售前投递 Worker，等待数据库出现真实新鲜心跳（up 已自动执行）
  start-presale-alert-worker  构建并启动售前预警扫描 Worker（up 已自动执行）

  各定向更新都不会删除或重建 Application、Environment、LoginTarget、OAuth Client，
  因此不会影响已经完成的子系统统一登录接入。

应用容器：
  frontend      基础平台 + 合同管理 + 客户与商机管理 + 客户自助门户前端（宿主机仅发布 8081）
  api           基础平台 API + Worker
  contract-api  合同管理 API + Temporal Worker
  customer-api  客户与商机管理 API
  portal-api    客户自助门户 API（完成 customer_portal/dev 接入后启用）
  project-api   项目管理系统 API + Temporal Worker（完成 project_management/dev 接入后启用）
  dashboard-api 数据看板与统计分析 API（嵌入桥）+ aggregation-worker + alert-worker + Metabase（完成 data_analysis/dev 接入后启用）
  presale-worker 售前申请审批/PMS 投递 Worker（up 默认启动）

首次启动若数据库中尚未存在超级管理员，必须提供三个管理员参数，或设置：
  BASIC_PLATFORM_ADMIN_DISPLAY_NAME
  BASIC_PLATFORM_ADMIN_ACCOUNT_NAME
  BASIC_PLATFORM_ADMIN_PASSWORD
USAGE
}

remove_volumes=false
while (($# > 0)); do
    case "$1" in
		up|down|stop|restart|ps|logs|config|verify|refresh-api|refresh-frontend|refresh-contract-api|refresh-customer-api|refresh-portal-api|refresh-project-api|refresh-data-analysis-api|start-presale-worker|start-presale-alert-worker)
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
            log "WARN: --admin-password 会暴露在进程参数中，请改用 --admin-password-stdin"
            shift 2
            ;;
        --admin-password-stdin)
            admin_password_stdin=true
            shift
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
		--customer-env-file)
            (($# >= 2)) || fail "$1 缺少参数"
            customer_env_file="$2"
			shift 2
			;;
		--portal-env-file)
			(($# >= 2)) || fail "$1 缺少参数"
			portal_env_file="$2"
			shift 2
			;;
		--project-env-file)
			(($# >= 2)) || fail "$1 缺少参数"
			project_env_file="$2"
			shift 2
			;;
		--data-analysis-env-file)
			(($# >= 2)) || fail "$1 缺少参数"
			data_analysis_env_file="$2"
			shift 2
			;;
		--presale-worker-env-file)
			(($# >= 2)) || fail "$1 缺少参数"
			presale_worker_env_file="$2"
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

log_services=()
log_since=""
log_tail=""
log_follow=true

if [[ "$command_name" == "logs" ]]; then
    while (($# > 0)); do
        case "$1" in
            --since)
                (($# >= 2)) || fail "$1 缺少参数"
                log_since="$2"
                shift 2
                ;;
            --tail)
                (($# >= 2)) || fail "$1 缺少参数"
                log_tail="$2"
                shift 2
                ;;
            --no-follow)
                log_follow=false
                shift
                ;;
            --)
                shift
                log_services+=("$@")
                break
                ;;
            -*)
                fail "logs 不支持的选项：$1"
                ;;
            *)
                log_services+=("$1")
                shift
                ;;
        esac
    done
fi

command -v docker >/dev/null 2>&1 || fail "未找到 docker 命令"
docker compose version >/dev/null 2>&1 || fail "当前 Docker 不支持 docker compose 子命令"
[[ -f "$compose_file" ]] || fail "Compose 文件不存在：$compose_file"
[[ -d "$contract_root" ]] || fail "合同管理后端目录不存在：$contract_root"
[[ -d "$customer_root" ]] || fail "客户与商机管理后端目录不存在：$customer_root"
[[ -d "$project_management_root" ]] || fail "项目管理系统后端目录不存在：$project_management_root"

export BASIC_PLATFORM_RUNTIME_ENV_FILE="$env_file"
export CONTRACT_RUNTIME_ENV_FILE="$contract_env_file"
export CUSTOMER_RUNTIME_ENV_FILE="$customer_env_file"
export PORTAL_RUNTIME_ENV_FILE="$portal_env_file"
export PROJECT_RUNTIME_ENV_FILE="$project_env_file"
export DATA_ANALYSIS_RUNTIME_ENV_FILE="$data_analysis_env_file"
export PRESALE_WORKER_ENV_FILE="$presale_worker_env_file"
export BASIC_PLATFORM_HOST_PROJECT_ROOT="$project_root"
export SUBSYSTEM_HOST_PROJECTS_ROOT="$workspace_root"

validate_local_automation_paths() {
    [[ -d "$BASIC_PLATFORM_HOST_PROJECT_ROOT" ]] || \
        fail "基础平台宿主机项目目录不存在：$BASIC_PLATFORM_HOST_PROJECT_ROOT"
    [[ -d "$SUBSYSTEM_HOST_PROJECTS_ROOT" ]] || \
        fail "子系统宿主机根目录不存在：$SUBSYSTEM_HOST_PROJECTS_ROOT"
    [[ -f "$BASIC_PLATFORM_HOST_PROJECT_ROOT/scripts/portal-gateway.sh" ]] || \
        fail "基础平台网关脚本不存在：$BASIC_PLATFORM_HOST_PROJECT_ROOT/scripts/portal-gateway.sh"
    [[ -f "$BASIC_PLATFORM_HOST_PROJECT_ROOT/docker/portal-apps-locations.conf" ]] || \
        fail "基础平台网关配置不存在：$BASIC_PLATFORM_HOST_PROJECT_ROOT/docker/portal-apps-locations.conf"
}

validate_local_automation_paths

# scripts/lan-access.sh 通过 docker/.env.lan 标记临时局域网模式。docker-local
# 必须继续为基础平台、合同后端和客户商机后端加载同源覆盖配置，否则平台
# 发布的 OIDC issuer 与子系统期望的 issuer 会分别落到 LAN 地址和 localhost，
# 业务后端将因 discovery 校验失败而持续重启。
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
    if [[ -f "$customer_lan_override_file" ]]; then
        sed -i.bak -E "s|http://${file_ip}:${file_port}|http://${new_ip}:${file_port}|g" "$customer_lan_override_file"
        rm -f "$customer_lan_override_file.bak"
    fi
}

write_customer_lan_override() {
    local public_origin="$1"
    umask 077
    cat > "$customer_lan_override_file" <<EOF
# 由 scripts/docker-local.sh/lan-access.sh 自动维护；仅用于临时局域网访问。
APP_PUBLIC_ORIGIN=${public_origin}
OIDC_ISSUER=${public_origin}
OIDC_REDIRECT_URI=${public_origin}/customer-opportunity/auth/callback
OIDC_POST_LOGOUT_REDIRECT_URI=${public_origin}/customer-opportunity/
EOF
    chmod 600 "$customer_lan_override_file"
}

configure_access_mode() {
    local public_origin lan_port detected_ip file_ip

    [[ -f "$lan_placeholder_file" ]] || fail "局域网占位环境文件不存在：$lan_placeholder_file"

    if [[ ! -f "$lan_override_file" ]]; then
        export BASIC_PLATFORM_LAN_OVERRIDE_ENV_FILE="$lan_placeholder_file"
        export CONTRACT_LAN_OVERRIDE_ENV_FILE="$lan_placeholder_file"
		export CUSTOMER_LAN_OVERRIDE_ENV_FILE="$lan_placeholder_file"
		export PORTAL_LAN_OVERRIDE_ENV_FILE="$lan_placeholder_file"
		export PROJECT_LAN_OVERRIDE_ENV_FILE="$lan_placeholder_file"
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

    write_customer_lan_override "$public_origin"
    export BASIC_PLATFORM_LAN_OVERRIDE_ENV_FILE="$lan_override_file"
	export CONTRACT_LAN_OVERRIDE_ENV_FILE="$lan_override_file"
	export CUSTOMER_LAN_OVERRIDE_ENV_FILE="$customer_lan_override_file"
	export PORTAL_LAN_OVERRIDE_ENV_FILE="$lan_placeholder_file"
	export PROJECT_LAN_OVERRIDE_ENV_FILE="$lan_override_file"
    export FRONTEND_BIND_ADDRESS="0.0.0.0"
    export FRONTEND_HTTP_PORT="$lan_port"
    frontend_public_origin="$public_origin"
    log "统一访问地址：$public_origin"
}

configure_access_mode

compose() {
    # macOS 自带 Bash 3.2 在 `set -u` 下展开空数组会报 unbound variable，
    # 因此不使用可选数组，而是按是否接入 Portal 显式分支。
    # customer_portal/dev 接入成功后，普通 up/down/stop/ps/logs/config 自动纳入
    # portal-api。首次接入前不能强行启动，因为此时浏览器 OIDC Client、租户、
    # 角色目录和六组机器凭据尚不存在；平台与 CRM 需先运行以完成受控接入。
    data_analysis_profile_arg=""
    if data_analysis_configured; then
        data_analysis_profile_arg="--profile data-analysis"
    fi
    if portal_configured && project_configured; then
        docker compose \
            --project-name "$compose_project" \
            --file "$compose_file" \
            --env-file "$env_file" \
            --env-file "$contract_env_file" \
            --env-file "$customer_env_file" \
            --env-file "$portal_env_file" \
            --env-file "$project_env_file" \
            --env-file "$data_analysis_env_file" \
            --profile portal \
            --profile project \
            --profile presale-worker \
            --profile keycloak \
            $data_analysis_profile_arg \
            "$@"
        return
    fi
    if portal_configured; then
        docker compose \
            --project-name "$compose_project" \
            --file "$compose_file" \
            --env-file "$env_file" \
            --env-file "$contract_env_file" \
            --env-file "$customer_env_file" \
            --env-file "$portal_env_file" \
            --env-file "$project_env_file" \
            --env-file "$data_analysis_env_file" \
            --profile portal \
            --profile presale-worker \
            --profile keycloak \
            $data_analysis_profile_arg \
            "$@"
        return
    fi
    if project_configured; then
        docker compose \
            --project-name "$compose_project" \
            --file "$compose_file" \
            --env-file "$env_file" \
            --env-file "$contract_env_file" \
            --env-file "$customer_env_file" \
            --env-file "$portal_env_file" \
            --env-file "$project_env_file" \
            --env-file "$data_analysis_env_file" \
            --profile project \
            --profile presale-worker \
            --profile keycloak \
            $data_analysis_profile_arg \
            "$@"
        return
    fi
    docker compose \
        --project-name "$compose_project" \
        --file "$compose_file" \
        --env-file "$env_file" \
        --env-file "$contract_env_file" \
        --env-file "$customer_env_file" \
        --env-file "$portal_env_file" \
        --env-file "$project_env_file" \
        --profile presale-worker \
        --profile keycloak \
        $data_analysis_profile_arg \
        "$@"
}

replace_line_in_file() {
    local file="$1" key="$2" value="$3" tmp
    tmp="$(mktemp "${TMPDIR:-/tmp}/docker-local-env.XXXXXX")"
    # 先写入临时文件并设为 0600，再原子替换，避免中断时留下半行配置或短暂放宽密钥权限。
    # 临时文件位于系统临时目录，mv 跨文件系统时未必原子；调用者应避免并发执行多个编排命令。
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

ensure_platform_jwt_key_pair() {
    local key_dir="${project_root}/data/keys"
    local private_key="${key_dir}/jwt-ed25519-private.pem"
    local public_key="${key_dir}/jwt-ed25519-public.pem"
    if [[ -f "$private_key" && -f "$public_key" ]]; then
        chmod 600 "$private_key" "$public_key"
        return
    fi
    if [[ -e "$private_key" || -e "$public_key" ]]; then
        fail "JWT 密钥对不完整；请恢复配套公私钥，或同时移走 ${private_key} 和 ${public_key} 后重新生成"
    fi

    local temporary_dir
    temporary_dir="$(mktemp -d "${key_dir}/.jwt-keys.XXXXXX")"
    openssl genpkey -algorithm ED25519 -out "${temporary_dir}/private.pem" >/dev/null 2>&1 || {
        rm -rf -- "$temporary_dir"
        fail "生成 Ed25519 JWT 私钥失败"
    }
    openssl pkey -in "${temporary_dir}/private.pem" -pubout -out "${temporary_dir}/public.pem" >/dev/null 2>&1 || {
        rm -rf -- "$temporary_dir"
        fail "生成 Ed25519 JWT 公钥失败"
    }
    chmod 600 "${temporary_dir}/private.pem" "${temporary_dir}/public.pem"
    mv "${temporary_dir}/private.pem" "$private_key"
    mv "${temporary_dir}/public.pem" "$public_key"
    rmdir "$temporary_dir"
    log "已生成本地 Ed25519 JWT 密钥对：${key_dir}"
}

ensure_platform_env_file() {
    command -v openssl >/dev/null 2>&1 || fail "未找到 openssl，无法生成本地密码和密钥"
    mkdir -p "$(dirname "$env_file")" "${project_root}/data/keys" "${project_root}/data/logs" "${project_root}/data/uploads"
    chmod 700 "${project_root}/data/keys" "${project_root}/data/logs" "${project_root}/data/uploads"
    ensure_platform_jwt_key_pair

    # 仅在文件不存在时生成密钥；重复 up 绝不轮换既有数据库密码和加密密钥。
    if [[ ! -f "$env_file" ]]; then
        # Docker 数据卷会保留 MySQL 初始化时的账号密码。若环境文件丢失但卷还在，
        # 重新随机生成 MYSQL_PASSWORD 会让迁移和 API 永久无法登录旧库。此处明确
        # 停止并要求恢复环境文件或走受控密码恢复，绝不在用户不知情时删除数据卷。
        # compose.local.yaml 为本地卷指定了固定 name（使用连字符，而不是
        # Compose 默认的下划线名称）。平台库和 Keycloak 库任意一个存在，都
        # 说明不能安全地依据模板重新随机化 .env.local 中的数据库密码。
        local retained_volume
        for retained_volume in \
            "${compose_project}-mysql-data" \
            "${compose_project}-keycloak-mysql-data"
        do
            if command -v docker >/dev/null 2>&1 && docker volume inspect "$retained_volume" >/dev/null 2>&1; then
                fail "检测到已有本地数据卷 ${retained_volume}，但环境文件缺失：${env_file}。请先恢复旧 .env.local，或按数据库密码恢复流程重置账号；不要删除数据卷。"
            fi
        done
        [[ -f "$platform_template_file" ]] || fail "基础平台环境模板不存在：$platform_template_file"
        cp "$platform_template_file" "$env_file"
        chmod 600 "$env_file"
        replace_line_in_file "$env_file" MYSQL_PASSWORD "$(random_hex 24)"
        replace_line_in_file "$env_file" MYSQL_ROOT_PASSWORD "$(random_hex 32)"
        replace_line_in_file "$env_file" IAM_MOBILE_ENCRYPTION_KEY "$(random_key)"
        replace_line_in_file "$env_file" IAM_FEDERATED_PROVIDER_SECRET_ENCRYPTION_KEY "$(random_key)"
        replace_line_in_file "$env_file" IAM_EXTERNAL_LOGIN_STATE_ENCRYPTION_KEY "$(random_key)"
        replace_line_in_file "$env_file" IAM_BOOTSTRAP_TOKEN "$(random_hex 32)"
        replace_line_in_file "$env_file" KEYCLOAK_DB_PASSWORD "$(random_hex 24)"
        replace_line_in_file "$env_file" KEYCLOAK_DB_ROOT_PASSWORD "$(random_hex 32)"
        replace_line_in_file "$env_file" KEYCLOAK_ADMIN_PASSWORD "$(random_hex 32)"
        log "已根据模板生成基础平台环境文件：$env_file"
    fi

    # Keycloak 使用独立 MySQL 卷；升级到该编排版本时，仅为新增字段
    # 生成凭据，不轮换已有真实凭据。
    local keycloak_db_password keycloak_db_root_password keycloak_admin_username keycloak_admin_password
    keycloak_db_password="$(env_value "$env_file" KEYCLOAK_DB_PASSWORD)"
    keycloak_db_root_password="$(env_value "$env_file" KEYCLOAK_DB_ROOT_PASSWORD)"
    keycloak_admin_username="$(env_value "$env_file" KEYCLOAK_ADMIN_USERNAME)"
    keycloak_admin_password="$(env_value "$env_file" KEYCLOAK_ADMIN_PASSWORD)"
    if [[ -z "$keycloak_db_password" || "$keycloak_db_password" == REPLACE_WITH_* ]]; then
        replace_line_in_file "$env_file" KEYCLOAK_DB_PASSWORD "$(random_hex 24)"
    fi
    if [[ -z "$keycloak_db_root_password" || "$keycloak_db_root_password" == REPLACE_WITH_* ]]; then
        replace_line_in_file "$env_file" KEYCLOAK_DB_ROOT_PASSWORD "$(random_hex 32)"
    fi
    if [[ -z "$keycloak_admin_password" || "$keycloak_admin_password" == REPLACE_WITH_* ]]; then
        replace_line_in_file "$env_file" KEYCLOAK_ADMIN_PASSWORD "$(random_hex 32)"
    fi
    if [[ -z "$keycloak_admin_username" || "$keycloak_admin_username" == REPLACE_WITH_* ]]; then
        replace_line_in_file "$env_file" KEYCLOAK_ADMIN_USERNAME admin
    fi
    # The management page performs Realm/Client initialization through the
    # platform API.  These are non-secret local routing defaults.
    replace_line_in_file "$env_file" KEYCLOAK_MANAGEMENT_ENABLED true
    replace_line_in_file "$env_file" KEYCLOAK_ADMIN_URL http://keycloak:8080
    replace_line_in_file "$env_file" KEYCLOAK_PUBLIC_URL http://localhost:18090
    replace_line_in_file "$env_file" KEYCLOAK_REALM basic-platform
    # 新接入环境默认走 Keycloak；已有环境的 issuer_alias 存在于平台数据库，
    # 不会因这里变更而被隐式切换，仍需通过应用接入页完成受控迁移。
    replace_line_in_file "$env_file" SUBSYSTEM_DEFAULT_ISSUER_ALIAS keycloak

    # OIDC_ROLE_CONFIG_HASH 是 CRM Token Claims 的目录一致性校验值。旧模板
    # 使用占位符时在 Compose 解析前填入可信本地基线；启动 CRM 前仍会用镜像
    # 实际目录哈希复核，目录发生变化不会被这个基线静默掩盖。
    local role_config_hash
    role_config_hash="$(env_value "$env_file" OIDC_ROLE_CONFIG_HASH)"
    if [[ -z "$role_config_hash" || "$role_config_hash" == REPLACE_WITH_* ]]; then
        replace_line_in_file "$env_file" OIDC_ROLE_CONFIG_HASH "$local_crm_role_config_hash"
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
        replace_line_in_file "$contract_env_file" OIDC_ISSUER "http://localhost:18090/realms/basic-platform"
        replace_line_in_file "$contract_env_file" OIDC_BACKCHANNEL_BASE_URL "http://keycloak:8080"
        replace_line_in_file "$contract_env_file" OIDC_REDIRECT_URI "http://localhost:8081/contract_management/auth/callback"
        replace_line_in_file "$contract_env_file" OIDC_SESSION_ENCRYPTION_KEY_BASE64 "$(random_key)"
        replace_line_in_file "$contract_env_file" APP_PUBLIC_URL "http://localhost:8081/contract_management/dashboard"
        replace_line_in_file "$contract_env_file" APP_PATH_PREFIX "/contract_management"
        log "已生成合同管理环境文件：$contract_env_file"
        log "请先在基础平台完成合同管理系统接入，并填写 OIDC_CLIENT_ID、OIDC_CLIENT_SECRET、OIDC_TENANT_ID。"
    fi

    if [[ -z "$(env_value "$contract_env_file" OIDC_SESSION_ENCRYPTION_KEY_BASE64)" || "$(env_value "$contract_env_file" OIDC_SESSION_ENCRYPTION_KEY_BASE64)" == REPLACE_WITH_* ]]; then
        replace_line_in_file "$contract_env_file" OIDC_SESSION_ENCRYPTION_KEY_BASE64 "$(random_key)"
    fi

    if [[ "$strict" == true ]]; then
        local unresolved
        unresolved="$(grep -E 'REPLACE_WITH_' "$contract_env_file" | grep -v '^#' || true)"
        [[ -z "$unresolved" ]] || fail "合同管理环境文件仍包含接入占位符：$contract_env_file"
    fi

    # 旧版本曾在基础平台环境文件中保存合同数据库凭据。统一部署继续使用已有
    # 数据卷时，优先沿用这组凭据，避免新建合同环境文件后静默生成第二套密码。
    # MySQL 的 MYSQL_PASSWORD 只在空数据目录首次初始化时生效，单纯修改 env
    # 文件不会更新已有数据库账号。
    local legacy_contract_password legacy_contract_root_password current_contract_password current_contract_root_password
    legacy_contract_password="$(env_value "$env_file" CONTRACT_MYSQL_PASSWORD)"
    legacy_contract_root_password="$(env_value "$env_file" CONTRACT_MYSQL_ROOT_PASSWORD)"
    current_contract_password="$(env_value "$contract_env_file" CONTRACT_MYSQL_PASSWORD)"
    current_contract_root_password="$(env_value "$contract_env_file" CONTRACT_MYSQL_ROOT_PASSWORD)"
    if [[ -n "$legacy_contract_password" && -n "$legacy_contract_root_password" ]] && \
       [[ "$legacy_contract_password" != "$current_contract_password" || "$legacy_contract_root_password" != "$current_contract_root_password" ]]; then
        replace_line_in_file "$contract_env_file" CONTRACT_MYSQL_PASSWORD "$legacy_contract_password"
        replace_line_in_file "$contract_env_file" CONTRACT_MYSQL_ROOT_PASSWORD "$legacy_contract_root_password"
        log "已沿用基础平台环境文件中的既有合同数据库凭据，避免已有数据卷认证失败"
    fi

    # OIDC_ISSUER 是浏览器可见地址；容器自身不能通过 localhost 访问宿主机
    # 的 8081。运行时文件没有后通道时，按 Issuer 补齐对应 Compose 内网地址。
    # 已由应用接入流程写入的非空值绝不覆盖，避免干扰已切换的环境。
    local oidc_issuer oidc_backchannel keycloak_public_url
    oidc_issuer="$(env_value "$contract_env_file" OIDC_ISSUER)"
    oidc_backchannel="$(env_value "$contract_env_file" OIDC_BACKCHANNEL_BASE_URL)"
    keycloak_public_url="$(env_value "$env_file" KEYCLOAK_PUBLIC_URL)"
    if [[ -z "$oidc_backchannel" ]]; then
        if [[ "$oidc_issuer" == "http://localhost:8081" || "$oidc_issuer" == "http://127.0.0.1:8081" ]]; then
            replace_line_in_file "$contract_env_file" OIDC_BACKCHANNEL_BASE_URL "http://platform-api:8080"
            log "已为合同管理补齐基础平台 OIDC 容器内后通道"
        elif [[ -n "$keycloak_public_url" && "$oidc_issuer" == "${keycloak_public_url%/}/realms/"* ]]; then
            replace_line_in_file "$contract_env_file" OIDC_BACKCHANNEL_BASE_URL "http://keycloak:8080"
            log "已为合同管理补齐 Keycloak OIDC 容器内后通道"
        fi
    elif [[ -n "$keycloak_public_url" && "$oidc_issuer" == "${keycloak_public_url%/}/realms/"* && "$oidc_backchannel" == */realms/* ]]; then
        # OIDC_BACKCHANNEL_BASE_URL is an origin, not the public issuer.  Older
        # local configurations accidentally stored /realms/<realm> here, which
        # makes the contract service fail before it can start.  Keep the value
        # private to the Compose network and repair only this known-invalid form.
        replace_line_in_file "$contract_env_file" OIDC_BACKCHANNEL_BASE_URL "http://keycloak:8080"
        log "已修复合同管理 Keycloak 后通道：移除了错误的 /realms 路径"
    fi
}

ensure_customer_env_file() {
    command -v openssl >/dev/null 2>&1 || fail "未找到 openssl，无法生成客户与商机管理数据库密码和敏感字段密钥"
    # CRM 的加密键与检索 HMAC 键职责不同，首次生成后都必须稳定保留，否则历史敏感字段不可读/不可查。
    if [[ ! -f "$customer_env_file" ]]; then
        [[ -f "$customer_template_file" ]] || fail "客户与商机管理环境模板不存在：$customer_template_file"
        cp "$customer_template_file" "$customer_env_file"
        chmod 600 "$customer_env_file"

        local password root_password encryption_key hmac_key
        password="$(random_hex 24)"
        root_password="$(random_hex 32)"
        encryption_key="$(random_key)"
        hmac_key="$(random_key)"
        replace_line_in_file "$customer_env_file" CUSTOMER_MYSQL_PASSWORD "$password"
        replace_line_in_file "$customer_env_file" CUSTOMER_MYSQL_ROOT_PASSWORD "$root_password"
        replace_line_in_file "$customer_env_file" MYSQL_DSN "customer:${password}@tcp(customer-mysql:3306)/customer_opportunity?charset=utf8mb4&parseTime=True&loc=UTC&multiStatements=true"
        replace_line_in_file "$customer_env_file" SENSITIVE_ENCRYPTION_KEY_BASE64 "$encryption_key"
        replace_line_in_file "$customer_env_file" SENSITIVE_HMAC_KEY_BASE64 "$hmac_key"
        log "已生成客户与商机管理环境文件：$customer_env_file"
    fi

    local unresolved
    unresolved="$(grep -E 'REPLACE_WITH_' "$customer_env_file" | grep -v '^#' || true)"
    [[ -z "$unresolved" ]] || fail "客户与商机管理环境文件仍包含未填写占位符：$customer_env_file"

    # OIDC_BACKCHANNEL_BASE_URL 只接受容器内可达的 HTTP(S) origin，不能携带
    # /realms/<realm> 路径。修复历史本地配置，避免 CRM 在 Compose 健康检查前循环重启。
    local oidc_backchannel
    oidc_backchannel="$(env_value "$customer_env_file" OIDC_BACKCHANNEL_BASE_URL)"
    if [[ "$oidc_backchannel" == */realms/* ]]; then
        replace_line_in_file "$customer_env_file" OIDC_BACKCHANNEL_BASE_URL "http://keycloak:8080"
        log "已修复客户与商机管理 Keycloak 后通道：移除了错误的 /realms 路径"
    fi
}

ensure_portal_env_file() {
	command -v openssl >/dev/null 2>&1 || fail "未找到 openssl，无法生成客户门户数据库密码和安全密钥"
	if [[ ! -f "$portal_env_file" ]]; then
		[[ -f "$portal_template_file" ]] || fail "客户自助门户环境模板不存在：$portal_template_file"
		cp "$portal_template_file" "$portal_env_file"
		chmod 600 "$portal_env_file"

		local password root_password encryption_key descriptor_key hmac_key
		password="$(random_hex 24)"
		root_password="$(random_hex 32)"
		encryption_key="$(random_key)"
		descriptor_key="$(random_key)"
		hmac_key="$(random_key)"
		replace_line_in_file "$portal_env_file" PORTAL_MYSQL_PASSWORD "$password"
		replace_line_in_file "$portal_env_file" PORTAL_MYSQL_ROOT_PASSWORD "$root_password"
		replace_line_in_file "$portal_env_file" PORTAL_MYSQL_DSN "portal:${password}@tcp(portal-mysql:3306)/customer_portal?charset=utf8mb4&parseTime=true&loc=UTC&multiStatements=true"
		replace_line_in_file "$portal_env_file" PORTAL_ENCRYPTION_KEY_BASE64 "$encryption_key"
		replace_line_in_file "$portal_env_file" PORTAL_REPORT_INGEST_DESCRIPTOR_KEY_BASE64 "$descriptor_key"
		replace_line_in_file "$portal_env_file" PORTAL_HMAC_KEY_BASE64 "$hmac_key"
		log "已生成客户自助门户环境文件：$portal_env_file"
	fi

	local unresolved
	unresolved="$(grep -E 'REPLACE_WITH_' "$portal_env_file" | grep -v '^#' || true)"
	[[ -z "$unresolved" ]] || fail "客户自助门户环境文件仍包含未填写占位符：$portal_env_file"
}

ensure_project_env_file() {
	command -v openssl >/dev/null 2>&1 || fail "未找到 openssl，无法生成项目管理系统数据库密码"
	if [[ ! -f "$project_env_file" ]]; then
		[[ -f "$project_template_file" ]] || fail "项目管理系统环境模板不存在：$project_template_file"
		cp "$project_template_file" "$project_env_file"
		chmod 600 "$project_env_file"

		local password root_password
		password="$(random_hex 24)"
		root_password="$(random_hex 32)"
		replace_line_in_file "$project_env_file" PROJECT_MYSQL_PASSWORD "$password"
		replace_line_in_file "$project_env_file" PROJECT_MYSQL_ROOT_PASSWORD "$root_password"
		replace_line_in_file "$project_env_file" MYSQL_DSN "project:${password}@tcp(project-mysql:3306)/project_management?parseTime=true&charset=utf8mb4&collation=utf8mb4_unicode_ci"
		replace_line_in_file "$project_env_file" PLATFORM_BASE_URL "http://localhost:8081"
		replace_line_in_file "$project_env_file" PROJECT_PLATFORM_BACKCHANNEL_BASE_URL "http://platform-api:8080"
        replace_line_in_file "$project_env_file" OIDC_ISSUER "http://localhost:18090/realms/basic-platform"
        replace_line_in_file "$project_env_file" OIDC_BACKCHANNEL_BASE_URL "http://keycloak:8080"
        replace_line_in_file "$project_env_file" OIDC_REDIRECT_URI "http://localhost:8081/project_management/auth/callback"
        replace_line_in_file "$project_env_file" OIDC_SESSION_ENCRYPTION_KEY_BASE64 "$(random_key)"
        replace_line_in_file "$project_env_file" OIDC_POST_LOGOUT_REDIRECT_URI "http://localhost:8081/project_management/logged-out"
        replace_line_in_file "$project_env_file" APP_PATH_PREFIX "/project_management"
        replace_line_in_file "$project_env_file" OIDC_SESSION_COOKIE_SECURE "false"
        replace_line_in_file "$project_env_file" PLATFORM_ENVIRONMENT_CODE "dev"
        log "已生成项目管理系统环境文件：$project_env_file"
        log "请先在基础平台完成项目管理系统接入，并填写 OIDC_CLIENT_ID、OIDC_CLIENT_SECRET、OIDC_TENANT_ID。"
    fi

    if [[ -z "$(env_value "$project_env_file" OIDC_SESSION_ENCRYPTION_KEY_BASE64)" || "$(env_value "$project_env_file" OIDC_SESSION_ENCRYPTION_KEY_BASE64)" == REPLACE_WITH_* ]]; then
        replace_line_in_file "$project_env_file" OIDC_SESSION_ENCRYPTION_KEY_BASE64 "$(random_key)"
    fi
}

project_configured() {
	# 项目后端启动强校验 OIDC 四项接入值；缺少任何一项都视为尚未完成 project_management/dev 接入。
	[[ -n "$(env_value "$project_env_file" OIDC_CLIENT_ID)" ]] &&
		[[ -n "$(env_value "$project_env_file" OIDC_CLIENT_SECRET)" ]] &&
		[[ -n "$(env_value "$project_env_file" OIDC_REDIRECT_URI)" ]] &&
		[[ -n "$(env_value "$project_env_file" OIDC_TENANT_ID)" ]]
}

ensure_data_analysis_env_file() {
	command -v openssl >/dev/null 2>&1 || fail "未找到 openssl，无法生成数据看板数据库密码"
	if [[ ! -f "$data_analysis_env_file" ]]; then
		[[ -f "$data_analysis_template_file" ]] || fail "数据看板环境模板不存在：$data_analysis_template_file"
		cp "$data_analysis_template_file" "$data_analysis_env_file"
		chmod 600 "$data_analysis_env_file"

		local password root_password
		password="$(random_hex 24)"
		root_password="$(random_hex 32)"
		replace_line_in_file "$data_analysis_env_file" DASHBOARD_MYSQL_PASSWORD "$password"
		replace_line_in_file "$data_analysis_env_file" DASHBOARD_MYSQL_ROOT_PASSWORD "$root_password"
		replace_line_in_file "$data_analysis_env_file" OIDC_CODEC_KEY "$(random_hex 32)"
		replace_line_in_file "$data_analysis_env_file" METABASE_EMBEDDING_SECRET "$(random_hex 32)"
		replace_line_in_file "$data_analysis_env_file" PLATFORM_BASE_URL "http://localhost:8081"
		replace_line_in_file "$data_analysis_env_file" OIDC_ISSUER "http://localhost:18090/realms/basic-platform"
		replace_line_in_file "$data_analysis_env_file" OIDC_BACKCHANNEL_BASE_URL "http://keycloak:8080"
		replace_line_in_file "$data_analysis_env_file" OIDC_REDIRECT_URI "http://localhost:8081/data_analysis/auth/callback"
		replace_line_in_file "$data_analysis_env_file" APP_PATH_PREFIX "/data_analysis"
		replace_line_in_file "$data_analysis_env_file" APP_PUBLIC_ORIGIN "http://localhost:8081"
		replace_line_in_file "$data_analysis_env_file" COOKIE_SECURE "false"
		log "已生成数据看板环境文件：$data_analysis_env_file"
		log "请先在基础平台完成数据看板接入，并填写 OIDC_CLIENT_ID、OIDC_CLIENT_SECRET、OIDC_TENANT_ID。"
	fi

	if [[ -z "$(env_value "$data_analysis_env_file" OIDC_CODEC_KEY)" || "$(env_value "$data_analysis_env_file" OIDC_CODEC_KEY)" == REPLACE_WITH_* ]]; then
		replace_line_in_file "$data_analysis_env_file" OIDC_CODEC_KEY "$(random_hex 32)"
	fi
}

data_analysis_configured() {
	# 数据看板后端强校验 OIDC 四项接入值；缺少任何一项都视为尚未完成 data_analysis/dev 接入。
	[[ -n "$(env_value "$data_analysis_env_file" OIDC_CLIENT_ID)" ]] &&
		[[ -n "$(env_value "$data_analysis_env_file" OIDC_CLIENT_SECRET)" ]] &&
		[[ -n "$(env_value "$data_analysis_env_file" OIDC_REDIRECT_URI)" ]] &&
		[[ -n "$(env_value "$data_analysis_env_file" OIDC_TENANT_ID)" ]]
}

portal_configured() {
	# 不能只看镜像或数据库是否存在；四项接入产物齐全才允许把 Portal 纳入默认 profile 启动。
	[[ -n "$(env_value "$portal_env_file" PORTAL_OIDC_CLIENT_ID)" ]] &&
		[[ -n "$(env_value "$portal_env_file" PORTAL_OIDC_CLIENT_SECRET)" ]] &&
		[[ -n "$(env_value "$portal_env_file" PORTAL_OIDC_TENANT_ID)" ]] &&
		[[ -n "$(env_value "$portal_env_file" PORTAL_ROLE_CONFIG_HASH)" ]]
}

ensure_catalog_publisher_credentials_consistent() {
    local runtime_id runtime_secret platform_id platform_secret
    runtime_id="$(env_value "$contract_env_file" PLATFORM_AUTHORIZATION_CATALOG_CLIENT_ID)"
    runtime_secret="$(env_value "$contract_env_file" PLATFORM_AUTHORIZATION_CATALOG_CLIENT_SECRET)"
    platform_id="$(env_value "$env_file" CONTRACT_CATALOG_PUBLISHER_CLIENT_ID)"
    platform_secret="$(env_value "$env_file" CONTRACT_CATALOG_PUBLISHER_CLIENT_SECRET)"

    if [[ -n "$platform_id" && -n "$platform_secret" ]]; then
        if [[ "$platform_id" != "$runtime_id" || "$platform_secret" != "$runtime_secret" ]]; then
            # 平台环境中的长期凭据是既有 OAuth 客户端的运维记录。子系统配置可能
            # 来自更早一次接入，必须由平台向子系统同步，不能用已失效值反向覆盖。
            replace_line_in_file "$contract_env_file" PLATFORM_AUTHORIZATION_CATALOG_CLIENT_ID "$platform_id"
            replace_line_in_file "$contract_env_file" PLATFORM_AUTHORIZATION_CATALOG_CLIENT_SECRET "$platform_secret"
            log "已将平台保存的有效授权目录发布凭据同步到合同后端运行配置"
        fi
        return 0
    fi
    if [[ -n "$runtime_id" && -n "$runtime_secret" ]]; then
        # 兼容首次接入：旧平台环境尚未记录 publisher 凭据，但 provisioner 已将
        # 一次性明文安全写入子系统环境文件。此时补齐平台侧的长期运维记录。
        replace_line_in_file "$env_file" CONTRACT_CATALOG_PUBLISHER_CLIENT_ID "$runtime_id"
        replace_line_in_file "$env_file" CONTRACT_CATALOG_PUBLISHER_CLIENT_SECRET "$runtime_secret"
        log "已从首次接入配置补齐平台授权目录发布凭据"
    fi
}

env_value() {
    local file="$1" key="$2" value
    [[ -f "$file" ]] || return 0
    value="$(awk -v key="$key" '
        index($0, key "=") == 1 {
            value=substr($0, length(key) + 2)
            print value
            exit
        }
    ' "$file")"
    if [[ ${#value} -ge 2 ]]; then
        case "$value" in
            \"*\") value="${value:1:${#value}-2}" ;;
            \'*\') value="${value:1:${#value}-2}" ;;
        esac
    fi
    printf '%s' "$value"
}

runtime_value_configured() {
	local value="${1:-}"
	[[ -n "$value" && "$value" != PENDING_ONBOARDING && "$value" != REPLACE_WITH_* ]]
}

# 新环境的浏览器 OIDC 必须由 Keycloak 提供；基础平台只提供在线
# authorization-context，不再充当子系统的 token issuer。以下校验只对已经
# 写入浏览器 Client 的运行时文件生效，因此首次接入前仍可以先启动控制面。
trim_trailing_slash() {
    local value="${1:-}"
    while [[ "$value" == */ ]]; do value="${value%/}"; done
    printf '%s' "$value"
}

valid_http_origin() {
    local value
    value="$(trim_trailing_slash "${1:-}")"
    [[ "$value" =~ ^https?://[^/@?#]+(:[0-9]{1,5})?$ ]]
}

keycloak_realm_issuer() {
    local public_url realm
    public_url="$(trim_trailing_slash "$(env_value "$env_file" KEYCLOAK_PUBLIC_URL)")"
    realm="$(env_value "$env_file" KEYCLOAK_REALM)"
    runtime_value_configured "$public_url" || fail "KEYCLOAK_PUBLIC_URL 未配置，无法校验子系统 OIDC 运行时"
    runtime_value_configured "$realm" || fail "KEYCLOAK_REALM 未配置，无法校验子系统 OIDC 运行时"
    valid_http_origin "$public_url" || fail "KEYCLOAK_PUBLIC_URL 必须是 HTTP(S) origin：${public_url}"
    printf '%s/realms/%s' "$public_url" "$realm"
}

keycloak_internal_origin() {
    local value
    value="$(trim_trailing_slash "$(env_value "$env_file" KEYCLOAK_INTERNAL_URL)")"
    [[ -n "$value" ]] || value="http://keycloak:8080"
    valid_http_origin "$value" || fail "KEYCLOAK_INTERNAL_URL 必须是容器可达的 HTTP(S) origin：${value}"
    printf '%s' "$value"
}

validate_runtime_value() {
    local description="$1" key="$2" value="$3"
    runtime_value_configured "$value" || fail "${description}运行时 ${key} 未配置或仍是占位符；请先在应用接入页同步 Keycloak Client"
}

# 对一份已接入的子系统运行时文件执行同一套 V2 门禁：
# - 浏览器 issuer 必须是公开 Keycloak Realm；
# - 后通道只能是 Compose 私网 Keycloak origin，不能携带 realm/path；
# - redirect、client、租户、应用/环境自然键和角色目录哈希必须成组一致；
# - online authorization-context 由 Compose 固定注入 platform-api；运行时平台
#   地址只允许是统一入口或 Compose 私网平台 API，不能误配为 Keycloak。
validate_keycloak_runtime() {
    local description="$1" runtime_file="$2" application_code="$3" environment_code="$4"
    local issuer_key="$5" backchannel_key="$6" client_key="$7" secret_key="$8" redirect_key="$9" tenant_key="${10}"
    local platform_base_key="${11}" expected_redirect="${12}" hash_key="${13:-}"
    local issuer backchannel client_id client_secret redirect_uri tenant_id platform_base hash_value
    local expected_issuer expected_backchannel expected_client_id

    client_id="$(env_value "$runtime_file" "$client_key")"
    # 尚未由控制面创建 Client 的可选子系统不会启动；这里不把初始接入过程
    # 误判为错误。只要已有 Client，就必须通过完整门禁。
    runtime_value_configured "$client_id" || return 0

    issuer="$(env_value "$runtime_file" "$issuer_key")"
    backchannel="$(trim_trailing_slash "$(env_value "$runtime_file" "$backchannel_key")")"
    client_secret="$(env_value "$runtime_file" "$secret_key")"
    redirect_uri="$(env_value "$runtime_file" "$redirect_key")"
    tenant_id="$(env_value "$runtime_file" "$tenant_key")"
    platform_base="$(trim_trailing_slash "$(env_value "$runtime_file" "$platform_base_key")")"
    expected_issuer="$(keycloak_realm_issuer)"
    expected_backchannel="$(keycloak_internal_origin)"
    expected_client_id="${application_code}-${environment_code}-web"

    validate_runtime_value "$description" "$issuer_key" "$issuer"
    validate_runtime_value "$description" "$backchannel_key" "$backchannel"
    validate_runtime_value "$description" "$secret_key" "$client_secret"
    validate_runtime_value "$description" "$redirect_key" "$redirect_uri"
    validate_runtime_value "$description" "$tenant_key" "$tenant_id"
    validate_runtime_value "$description" "$platform_base_key" "$platform_base"
    [[ "$(trim_trailing_slash "$issuer")" == "$expected_issuer" ]] || \
        fail "${description}运行时 ${issuer_key}=${issuer}；本地 Keycloak Realm 应为 ${expected_issuer}"
    [[ "$backchannel" == "$expected_backchannel" ]] || \
        fail "${description}运行时 ${backchannel_key}=${backchannel}；应为 Compose 私网地址 ${expected_backchannel}（不得包含 /realms 路径）"
    [[ "$client_id" == "$expected_client_id" ]] || \
        fail "${description}运行时 ${client_key}=${client_id}；本地 dev 环境要求 ${expected_client_id}"
    [[ "$redirect_uri" == "$expected_redirect" ]] || \
        fail "${description}运行时 ${redirect_key}=${redirect_uri}；应与统一前端入口一致：${expected_redirect}"
    case "$platform_base" in
        "$(trim_trailing_slash "$frontend_public_origin")"|http://platform-api:8080) ;;
        *) fail "${description}运行时 ${platform_base_key}=${platform_base}；只能使用统一前端入口 ${frontend_public_origin} 或 Compose 私网平台地址 http://platform-api:8080，不能指向 Keycloak" ;;
    esac

    if [[ -n "$hash_key" ]]; then
        hash_value="$(env_value "$runtime_file" "$hash_key")"
        validate_runtime_value "$description" "$hash_key" "$hash_value"
        [[ "$hash_value" =~ ^sha256:[a-f0-9]{64}$ ]] || \
            fail "${description}运行时 ${hash_key} 必须为 sha256:<64位小写十六进制目录哈希>"
    fi
}

validate_authorization_context_wiring() {
    local expected='http://platform-api:8080/oauth2/authorization-context'
    local standard_count portal_count
    standard_count="$(grep -F "PLATFORM_AUTHORIZATION_CONTEXT_URL: ${expected}" "$compose_file" | wc -l | tr -d '[:space:]')"
    portal_count="$(grep -F "PORTAL_AUTHORIZATION_CONTEXT_URL: ${expected}" "$compose_file" | wc -l | tr -d '[:space:]')"
    [[ "$standard_count" -ge 3 && "$portal_count" -ge 1 ]] || \
        fail "Compose 未为合同、项目、CRM、Portal 完整注入平台在线 authorization-context：${expected}"
}

validate_all_keycloak_runtimes() {
	validate_authorization_context_wiring
    validate_keycloak_runtime "合同管理" "$contract_env_file" contract_management dev \
        OIDC_ISSUER OIDC_BACKCHANNEL_BASE_URL OIDC_CLIENT_ID OIDC_CLIENT_SECRET OIDC_REDIRECT_URI OIDC_TENANT_ID \
        PLATFORM_BASE_URL "${frontend_public_origin}/contract_management/auth/callback"
    validate_keycloak_runtime "客户与商机管理" "$customer_env_file" customer_and_opportunity dev \
        OIDC_ISSUER OIDC_BACKCHANNEL_BASE_URL OIDC_CLIENT_ID OIDC_CLIENT_SECRET OIDC_REDIRECT_URI OIDC_TENANT_ID \
        PLATFORM_BASE_URL "${frontend_public_origin}/customer-opportunity/auth/callback" OIDC_ROLE_CONFIG_HASH
    validate_keycloak_runtime "客户自助门户" "$portal_env_file" customer_portal dev \
        PORTAL_OIDC_ISSUER PORTAL_OIDC_BACKCHANNEL_BASE_URL PORTAL_OIDC_CLIENT_ID PORTAL_OIDC_CLIENT_SECRET PORTAL_OIDC_REDIRECT_URI PORTAL_OIDC_TENANT_ID \
        PORTAL_PLATFORM_BASE_URL "${frontend_public_origin}/customer-portal/auth/callback" PORTAL_ROLE_CONFIG_HASH
    validate_keycloak_runtime "项目管理" "$project_env_file" project_management dev \
        OIDC_ISSUER OIDC_BACKCHANNEL_BASE_URL OIDC_CLIENT_ID OIDC_CLIENT_SECRET OIDC_REDIRECT_URI OIDC_TENANT_ID \
        PLATFORM_BASE_URL "${frontend_public_origin}/project_management/auth/callback"
}

# 本地 Compose 的标准发现环境固定为 dev。运行时一旦完成 OIDC 接入，Client、
# application code 和 environment code 必须指向同一自然键；缺省元数据兼容旧 Portal
# 文件，但任何显式冲突都在启动/重建前失败，避免把 prod Client 装入 dev 容器。
validate_local_runtime_target() {
	local description="$1" runtime_file="$2" expected_application="$3" expected_environment="$4" client_key="$5"
	local application_code environment_code client_id expected_client_id
	application_code="$(env_value "$runtime_file" PLATFORM_APPLICATION_CODE)"
	environment_code="$(env_value "$runtime_file" PLATFORM_ENVIRONMENT_CODE)"
	client_id="$(env_value "$runtime_file" "$client_key")"
	expected_client_id="${expected_application}-${expected_environment}-web"

	runtime_value_configured "$client_id" || return 0
	if [[ -n "$application_code" && "$application_code" != "$expected_application" ]]; then
		fail "${description}本地运行配置不匹配：文件=${runtime_file}；当前应用=${application_code}；Compose 发现应用=${expected_application}；请重新执行 ${expected_application}/${expected_environment} 应用接入"
	fi
	if [[ -n "$environment_code" && "$environment_code" != "$expected_environment" ]]; then
		fail "${description}本地运行配置不匹配：文件=${runtime_file}；当前环境=${environment_code}；Compose 发现环境=${expected_environment}；当前 Client=${client_id:-未配置}；期望 Client=${expected_client_id}；请重新同步 ${expected_application}/${expected_environment}，不要将生产运行配置用于 docker-local.sh"
	fi
	if [[ "$client_id" != "$expected_client_id" ]]; then
		fail "${description}本地运行配置不匹配：文件=${runtime_file}；当前环境=${environment_code:-未配置}；当前 Client=${client_id:-未配置}；期望 Client=${expected_client_id}；请重新同步 ${expected_application}/${expected_environment}，不要将生产 Client 用于 docker-local.sh"
	fi
}

validate_all_local_runtime_targets() {
	validate_local_runtime_target "合同管理" "$contract_env_file" contract_management dev OIDC_CLIENT_ID
	validate_local_runtime_target "客户与商机管理" "$customer_env_file" customer_and_opportunity dev OIDC_CLIENT_ID
	validate_local_runtime_target "客户自助门户" "$portal_env_file" customer_portal dev PORTAL_OIDC_CLIENT_ID
	validate_local_runtime_target "项目管理" "$project_env_file" project_management dev OIDC_CLIENT_ID
}

portal_compensation_configured() {
	local key
	for key in PORTAL_PROVISION_CLIENT_ID PORTAL_PROVISION_CLIENT_SECRET PLATFORM_ROLE_ASSIGN_CLIENT_ID PLATFORM_ROLE_ASSIGN_CLIENT_SECRET; do
		runtime_value_configured "$(env_value "$customer_env_file" "$key")" || return 1
	done
}

disable_contract_startup_catalog_sync() {
    # 本地授权目录由隔离 provisioner 在接入/更新流程中发布。旧环境若仍启用
    # contract-api 启动同步，失效的 publisher 凭据会令 API 和 Worker 一起重启。
    if [[ "$(env_value "$contract_env_file" PLATFORM_AUTHORIZATION_CATALOG_SYNC_ENABLED)" == "true" ]]; then
        replace_line_in_file "$contract_env_file" PLATFORM_AUTHORIZATION_CATALOG_SYNC_ENABLED false
        log "已关闭合同后端启动期授权目录同步；目录改由受控 provisioner 发布"
    fi
}

compose_run() {
    compose --ansi never "$@"
}

# 旧版本的 split-runtime profile 可能留下名为 worker 的平台 Worker。
# 当前默认编排不启用该 profile，Compose --remove-orphans 不会处理未激活
# profile 中的服务；down 时只按 Compose 项目和服务标签清理这一类遗留容器，
# 避免它继续占用 basic-platform-local_default 网络。
remove_legacy_split_runtime_containers() {
    local container_id container_ids
    container_ids="$(docker ps -aq \
        --filter "label=com.docker.compose.project=${compose_project}" \
        --filter "label=com.docker.compose.service=worker")"
    while IFS= read -r container_id; do
        [[ -n "$container_id" ]] || continue
        log "移除遗留 split-runtime Worker 容器：$container_id"
        docker rm -f "$container_id" >/dev/null
    done <<< "$container_ids"
}

# Compose --wait 可能在冷启动尚可恢复时立即因 unhealthy 返回。这里做一次有界重试；
# 仍失败则输出服务状态与日志，避免只留下无法定位的 dependency failed to start。
compose_up_wait() {
    local description="$1"
    shift
    local attempt recreate_flag=""

    if [[ "$force_build" == true ]]; then
        recreate_flag="--force-recreate"
    fi

    for attempt in 1 2; do
        if compose_run up -d --wait $recreate_flag "$@"; then
            return 0
        fi
        if [[ "$attempt" -eq 1 ]]; then
            log "${description}仍在冷启动，首次健康等待未完成；输出当前状态并在 5 秒后重试"
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
        "quay.io/keycloak/keycloak:26.2"
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
    # portal-api 即使尚未完成 OIDC 接入也可以安全构建；只是不应在凭据、租户和
    # 角色目录准备好之前启动。始终构建它可确保本地镜像拓扑稳定，且 Worker 与 CRM 使用同一版本。
    local build_services=(api contract-api customer-api customer-presale-alert-worker portal-api project-api dashboard-migrate dashboard-api aggregation-worker alert-worker frontend presale-worker presale-integration-mock)
    if [[ "$force_build" == true ]]; then
        log "重新构建统一前端、平台/合同/CRM/门户/项目后端及售前投递 Worker 镜像"
    else
        log "构建缺失或有变更的统一前端及独立后端镜像"
    fi
    # migrate/bootstrap-admin/subsystem-provisioner 都复用 api 构建出的
    # basic-platform/backend:local；CRM、Portal 和售前 Worker 使用不同 target/镜像，不会
    # 把 crm-server、portal-server 和 Worker 运行在同一个业务容器中。
    # 限制并发既降低匿名镜像令牌抖动，也避免本地机器同时编译四个后端造成资源争抢。
    COMPOSE_PARALLEL_LIMIT=1 compose --profile bootstrap --profile customer --profile portal --profile project --profile presale-worker --ansi never build "${build_services[@]}"
}

prepare_gateway_config() {
    local gateway_script="${project_root}/scripts/portal-gateway.sh"
    [[ -x "$gateway_script" ]] || chmod +x "$gateway_script"
    PORTAL_GATEWAY_NGINX_INCLUDE="${project_root}/docker/portal-apps-locations.conf" \
        "$gateway_script" remove contract_management >/dev/null
    PORTAL_GATEWAY_NGINX_INCLUDE="${project_root}/docker/portal-apps-locations.conf" \
        "$gateway_script" remove customer_and_opportunity >/dev/null
	PORTAL_GATEWAY_NGINX_INCLUDE="${project_root}/docker/portal-apps-locations.conf" \
		"$gateway_script" remove customer-opportunity >/dev/null
	PORTAL_GATEWAY_NGINX_INCLUDE="${project_root}/docker/portal-apps-locations.conf" \
		"$gateway_script" remove customer_portal >/dev/null
	PORTAL_GATEWAY_NGINX_INCLUDE="${project_root}/docker/portal-apps-locations.conf" \
		"$gateway_script" remove project_management >/dev/null
	log "已清理内置业务模块的旧式整站反向代理；五个前端均由统一 frontend 容器直接承载"
}

run_migrations() {
	# 数据库先就绪、迁移再串行执行，业务 API 只能在全部 schema 成功后启动；禁止由 API 自动建表。
	# 各数据库互相独立，不存在跨库事务；某个后续迁移失败时，已成功数据库按自身幂等迁移规则重试。
	log "启动基础平台、合同、CRM 常驻 MySQL 与 Temporal，并等待健康检查"
	compose_run up -d --wait mysql contract-mysql customer-mysql temporal
    log "执行基础平台数据库迁移"
    compose_run run --rm --no-deps migrate ./migrate
    log "执行合同管理版本化数据库迁移"
    compose_run run --rm --no-deps contract-migrate
	log "执行客户与商机管理 CRM 清单迁移"
	compose_run run --rm --no-deps customer-migrate
	if portal_configured; then
		log "启动客户自助门户 MySQL 并执行 Portal 清单迁移"
		compose_run up -d --wait portal-mysql
		compose_run run --rm --no-deps portal-migrate
	fi
	if project_configured; then
		log "启动项目管理系统 MySQL 并执行项目清单迁移"
		compose_run up -d --wait project-mysql
		compose_run run --rm --no-deps project-migrate
	fi
}

bootstrap_admin_if_needed() {
    local status_rc
    log "检查超级管理员是否已初始化"
    # status 的退出码 3 是“尚未初始化”的协议结果，不是脚本错误；局部关闭 errexit 后必须立即恢复。
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
    if [[ -z "$admin_password" && "$admin_password_stdin" == true ]]; then
        IFS= read -r admin_password || true
    elif [[ -z "$admin_password" && -t 0 ]]; then
        read -r -s -p '首次超级管理员密码：' admin_password
        printf '\n'
    fi
    [[ -n "$admin_display_name" ]] || fail "首次初始化需要 --admin-display-name 或 BASIC_PLATFORM_ADMIN_DISPLAY_NAME"
    [[ -n "$admin_account_name" ]] || fail "首次初始化需要 --admin-account-name 或 BASIC_PLATFORM_ADMIN_ACCOUNT_NAME"
    [[ -n "$admin_password" ]] || fail "首次初始化需要 --admin-password 或 BASIC_PLATFORM_ADMIN_PASSWORD"

    log "初始化第一个超级管理员"
    # 密码经 stdin 进入一次性容器，不拼进容器命令行；完成后清除当前 shell 中的明文变量。
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

# 后端容器通过 Compose healthcheck 变为 healthy 后，统一 Nginx 仍可能在数秒内
# 复用旧上游连接。启动校验对业务健康地址短暂重试，避免已成功启动的服务被一次 502
# 误判为失败；认证/授权等语义校验仍保持单次、严格检查。
wait_for_frontend_status() {
    local route="$1" expected_status="$2" label="$3" attempt status
    for attempt in $(seq 1 20); do
        status="$(frontend_http_status "$route" 2>/dev/null || true)"
        if [[ "$status" == "$expected_status" ]]; then
            return 0
        fi
        sleep 1
    done
    fail "${label} 返回 ${status:-无响应}，预期为 ${expected_status}"
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
	local customer_health_status customer_session_status portal_health_status portal_session_status
	local project_health_status project_session_status
    local contract_authorize_url contract_authorize_route platform_authorize_status platform_login_url contract_issuer

    log "校验统一前端 Nginx 配置"
    compose_run exec -T frontend nginx -t >/dev/null

    # 任职关系接口属于基础平台 API。未携带会话时应被认证层拒绝为 401；
    # 404 代表前端网关或 api 容器仍在使用未包含该路由的旧版本。
    platform_membership_status="$(frontend_http_status /api/v1/memberships)" || \
        fail "无法访问基础平台任职关系接口"
    [[ "$platform_membership_status" == "401" ]] || \
        fail "基础平台任职关系接口返回 ${platform_membership_status}，预期未登录状态为 401；请重新构建并启动 api 与 frontend 容器"

    wait_for_frontend_status /contract_management/healthz 200 "合同管理健康检查路径"

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
    contract_issuer="$(env_value "$contract_env_file" OIDC_ISSUER)"
    case "$contract_authorize_url" in
        */realms/*/protocol/openid-connect/auth\?*)
            # Keycloak RP：合同 API 启动时已经通过同一环境的私网后通道完成
            # discovery；这里仅验证浏览器被正确导向公开 Realm，而不让前端
            # 容器错误地以 localhost 访问浏览器专用的公开地址。
            [[ "$contract_issuer" == */realms/* ]] || \
                fail "合同管理登录入口跳转到 Keycloak，但 OIDC_ISSUER 不是 Realm 地址"
            ;;
        *)
            # 兼容尚未迁移的基础平台 OIDC 环境。
            contract_authorize_route="$(public_url_to_route "$contract_authorize_url")" || \
                fail "合同管理登录入口返回了无法识别的 OIDC 授权地址"
            [[ "$contract_authorize_route" == /authorize\?* ]] || \
                fail "合同管理登录入口未跳转到基础平台 /authorize 或 Keycloak Realm"

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
            ;;
    esac

    wait_for_frontend_status /customer-opportunity/healthz 200 "客户与商机管理健康检查路径"

	customer_session_status="$(frontend_http_status /customer-opportunity/api/v1/auth/me)" || \
        fail "无法访问客户与商机管理登录状态接口"
    case "$customer_session_status" in
        401|503) ;;
        *) fail "客户与商机管理登录状态接口返回 ${customer_session_status}，预期开发认证未携带身份时为 401（或认证未配置时为 503）" ;;
	esac

	if portal_configured; then
		wait_for_frontend_status /customer-portal/healthz 200 "客户自助门户健康检查路径"
		portal_session_status="$(frontend_http_status /customer-portal/api/v1/auth/me)" || \
			fail "无法访问客户自助门户登录状态接口"
		[[ "$portal_session_status" == "401" ]] || \
			fail "客户自助门户登录状态接口返回 ${portal_session_status}，预期未登录状态为 401"
	fi

	if project_configured; then
		wait_for_frontend_status /project_management/healthz 200 "项目管理系统健康检查路径"
		project_session_status="$(frontend_http_status /project_management/api/v1/auth/me)" || \
			fail "无法访问项目管理系统登录状态接口"
		[[ "$project_session_status" == "401" ]] || \
			fail "项目管理系统登录状态接口返回 ${project_session_status}，预期未登录状态为 401"
	fi

	if data_analysis_configured; then
		wait_for_frontend_status /data_analysis/healthz 200 "数据看板健康检查路径"
		data_analysis_session_status="$(frontend_http_status /data_analysis/api/v1/auth/me)" || \
			fail "无法访问数据看板登录状态接口"
		[[ "$data_analysis_session_status" == "401" ]] || \
			fail "数据看板登录状态接口返回 ${data_analysis_session_status}，预期未登录状态为 401"
	fi

    log "统一网关校验通过：platform API=401，contract healthz=200，customer healthz=200"
}

start_stack() {
    ensure_platform_env_file
    ensure_contract_env_file true
	ensure_customer_env_file
	ensure_portal_env_file
	ensure_project_env_file
	ensure_data_analysis_env_file
	validate_all_local_runtime_targets
	validate_all_keycloak_runtimes
	if portal_configured && ! portal_compensation_configured; then
		fail "客户自助门户已接入，但 Portal 补偿 Worker 的映射/角色分配机器凭据不完整；请在应用接入页重试 customer_portal/dev"
	fi
    ensure_catalog_publisher_credentials_consistent
    disable_contract_startup_catalog_sync
    prepare_gateway_config
    build_images
    run_migrations
    bootstrap_admin_if_needed
    log "启动独立 Keycloak 认证容器与 MySQL"
    compose_up_wait "Keycloak" keycloak-db keycloak
    # 分阶段启动，避免四个 API 和 frontend 在同一次 Compose wait 中
    # 把下游的短暂冷启动误报为整套部署失败，同时让错误日志能够准确指向服务。
    log "启动基础平台 API 与受控子系统 provisioner"
    compose_up_wait "基础平台 API" subsystem-provisioner api
    sync_crm_authorization_catalog
    log "启动合同管理后端"
    compose_up_wait "合同管理后端" contract-api
	log "启动客户与商机管理后端"
	compose_up_wait "客户与商机管理后端" customer-api
	log "启动售前预警扫描 Worker"
	compose_run up -d --wait --no-deps customer-presale-alert-worker
	# 售前申请提交依赖独立 Worker 的新鲜心跳。up 现在默认一并启动本地
	# 集成 Mock 和 Worker，避免只启动 customer-api 后页面始终提示 Worker 未就绪。
	log "启动售前投递 Worker 并确认新鲜心跳"
	start_presale_worker --skip-build --skip-migrate
	if portal_configured; then
		log "启动已接入的客户自助门户后端"
		# portal-migrate 已在 run_migrations 中串行成功执行；再次让
		# `up --wait` 解析其 service_completed_successfully 依赖会重建一次性
		# 容器，并可能在等待阶段引用已被 Compose 清理的旧容器 ID。
		compose_run up -d --wait --no-deps portal-api
		compose_run up -d --no-deps portal-invite-compensation-worker
	else
		log "客户自助门户尚未接入，跳过 portal-api；可在应用接入中创建 customer_portal/dev"
	fi
	if project_configured; then
		log "启动已接入的项目管理系统后端"
		# project-migrate 同样已在 run_migrations 中完成，避免 Compose 在
		# `--wait` 时重复重建一次性迁移容器。
		compose_run up -d --wait --no-deps project-api
	else
		log "项目管理系统尚未接入，跳过 project-api；可在应用接入中创建 project_management/dev"
	fi
	if data_analysis_configured; then
		log "启动已接入的数据看板后端（dashboard-api + 聚合 Worker + Metabase）"
		# 业务 Schema 由独立迁移二进制管理；MySQL 初始化只创建 Metabase 元数据库。
		compose_run up -d --wait dashboard-mysql
		compose_run run --rm --no-deps dashboard-migrate
		compose_run up -d --wait --no-deps dashboard-api aggregation-worker alert-worker metabase
	else
		log "数据看板尚未接入，跳过 dashboard-api；可在应用接入中创建 data_analysis/dev"
	fi
    log "启动统一前端"
    compose_up_wait "统一前端" frontend
    verify_gateway_routes
    compose_run ps
    log "统一访问地址：${frontend_public_origin}"
    log "合同管理前端：${frontend_public_origin}/contract_management/"
	log "客户与商机管理前端：${frontend_public_origin}/customer-opportunity/"
	if portal_configured; then log "客户自助门户：${frontend_public_origin}/customer-portal/"; fi
	if project_configured; then log "项目管理系统：${frontend_public_origin}/project_management/"; fi
	if data_analysis_configured; then log "数据看板与统计分析：${frontend_public_origin}/data_analysis/"; fi
}

refresh_platform_api() {
    # 仅刷新基础平台后端：当前 API 不会包含新增路由时，使用该命令即可。
    # compose() 固定携带合同环境文件，所以这里只保证它存在，不校验其 OIDC 占位符。
    prepare_operational_env
    ensure_catalog_publisher_credentials_consistent
    prepare_go_backend_base_images "基础平台后端"

    log "重新构建基础平台后端镜像（不构建 frontend、contract-api 或 customer-api）"
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
    log "基础平台后端已刷新；统一前端、合同/CRM 后端、可选门户后端和现有统一登录接入配置保持不变"
}

refresh_unified_frontend() {
    prepare_operational_env
    prepare_frontend_base_images
    prepare_gateway_config

    log "重新构建统一前端镜像（基础平台 + 合同管理 + 客户与商机管理 + 客户自助门户 + 项目管理系统前端）"
    COMPOSE_PARALLEL_LIMIT=1 compose --ansi never build frontend
    log "重建统一 frontend 容器；平台/合同/CRM 后端、可选门户/项目后端和统一登录接入配置保持不变"
    compose_run up -d --wait --no-deps frontend
    verify_gateway_routes
    compose_run ps frontend
}

refresh_contract_backend() {
    ensure_platform_env_file
    ensure_contract_env_file true
    ensure_customer_env_file
	ensure_project_env_file
	ensure_portal_env_file
	validate_local_runtime_target "合同管理" "$contract_env_file" contract_management dev OIDC_CLIENT_ID
    ensure_catalog_publisher_credentials_consistent
    disable_contract_startup_catalog_sync
    prepare_go_backend_base_images "合同管理后端"

    # 平台 API 读取同一组 publisher 凭据为受控 update 流程服务。若脚本刚完成
    # 凭据归一化，必须重建 API 与 provisioner，不能让旧容器继续持有旧环境。
    log "重建基础平台 API 与 provisioner，使授权目录发布凭据保持一致"
    compose_run up -d --wait --force-recreate --no-deps subsystem-provisioner api

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

refresh_customer_backend() {
    ensure_platform_env_file
    ensure_contract_env_file false
	ensure_customer_env_file
	ensure_portal_env_file
	validate_local_runtime_target "客户与商机管理" "$customer_env_file" customer_and_opportunity dev OIDC_CLIENT_ID
    prepare_go_backend_base_images "客户与商机管理后端"

    log "重新构建客户与商机管理后端镜像（不构建 frontend、基础平台 api 或 contract-api）"
    COMPOSE_PARALLEL_LIMIT=1 compose --ansi never build customer-api customer-presale-alert-worker
    sync_crm_authorization_catalog
    prepare_gateway_config
    log "重新构建统一前端网关，使客户与商机管理路径转发到 customer-api"
    COMPOSE_PARALLEL_LIMIT=1 compose --ansi never build frontend
    log "启动客户与商机数据库并执行 CRM 清单迁移"
    compose_run up -d --wait customer-mysql
    compose_run run --rm --no-deps customer-migrate
	log "重建 customer-api 与统一前端网关；另外两个后端和统一登录接入配置保持不变"
	compose_run up -d --wait --no-deps customer-api customer-presale-alert-worker
    compose_run up -d --wait --no-deps frontend
    verify_gateway_routes
	compose_run ps customer-api customer-mysql
}

# CRM 启动时会严格校验基础平台已发布目录的 claims_role_config_hash。
# 所有启动入口都必须在启动 customer-api 前完成同一镜像目录的发布，
# 否则服务会因旧目录进入重启循环，而发布命令也无法稳定执行。
sync_crm_authorization_catalog() {
    local local_crm_hash
    local_crm_hash="$(compose_run run --rm --no-deps customer-api ./authz-catalog print crm \
        | awk -F= '$1 == "claims_role_config_hash" { print $2; exit }')"
    [[ "$local_crm_hash" =~ ^sha256:[a-f0-9]{64}$ ]] || fail "无法从本地 CRM 镜像读取有效授权目录哈希"
    export OIDC_ROLE_CONFIG_HASH="$local_crm_hash"
    log "使用本地 CRM 镜像内嵌授权目录哈希：$OIDC_ROLE_CONFIG_HASH"
    log "启动 CRM 前发布客户与商机管理授权目录"
    compose_run run --rm --no-deps customer-api ./authz-catalog publish crm
}

refresh_portal_backend() {
	ensure_platform_env_file
	ensure_contract_env_file false
	ensure_customer_env_file
	ensure_portal_env_file
	ensure_project_env_file
	portal_configured || fail "客户自助门户尚未完成 customer_portal/dev 应用接入，不能启动 portal-api"
	validate_local_runtime_target "客户自助门户" "$portal_env_file" customer_portal dev PORTAL_OIDC_CLIENT_ID
	portal_compensation_configured || fail "Portal 补偿 Worker 的映射/角色分配机器凭据不完整；请在应用接入页重试 customer_portal/dev"
	prepare_go_backend_base_images "客户自助门户后端"

	log "重新构建客户自助门户独立后端镜像"
	COMPOSE_PARALLEL_LIMIT=1 compose --ansi never build portal-api
	prepare_gateway_config
	log "刷新统一前端网关中的客户门户路由"
	COMPOSE_PARALLEL_LIMIT=1 compose --ansi never build frontend
	log "启动门户数据库并执行 Portal 清单迁移"
	compose_run up -d --wait portal-mysql
	compose_run run --rm --no-deps portal-migrate
	log "重建 portal-api、Portal 补偿 Worker 与统一前端"
	compose_run up -d --wait --no-deps portal-api
	compose_run up -d --no-deps portal-invite-compensation-worker
	compose_run up -d --wait --no-deps frontend
	verify_gateway_routes
	compose_run ps portal-api portal-invite-compensation-worker portal-mysql frontend
}

refresh_project_backend() {
	ensure_platform_env_file
	ensure_contract_env_file false
	ensure_customer_env_file
	ensure_portal_env_file
	ensure_project_env_file
	project_configured || fail "项目管理系统尚未完成 project_management/dev 应用接入，不能启动 project-api"
	validate_local_runtime_target "项目管理" "$project_env_file" project_management dev OIDC_CLIENT_ID
	prepare_go_backend_base_images "项目管理系统后端"

	log "重新构建项目管理系统独立后端镜像"
	COMPOSE_PARALLEL_LIMIT=1 compose --profile project --ansi never build project-api
	log "启动项目管理系统 MySQL 并执行项目清单迁移"
	compose_run up -d --wait project-mysql
	compose_run run --rm --no-deps project-migrate
	log "重建 project-api 容器；统一前端、基础平台后端和统一登录接入配置保持不变"
	compose_run up -d --wait --no-deps project-api
	verify_gateway_routes
	compose_run ps project-api project-mysql
}

refresh_data_analysis_backend() {
	ensure_platform_env_file
	ensure_contract_env_file false
	ensure_customer_env_file
	ensure_portal_env_file
	ensure_project_env_file
	ensure_data_analysis_env_file
	data_analysis_configured || fail "数据看板尚未完成 data_analysis/dev 应用接入，不能启动 dashboard-api"
	prepare_go_backend_base_images "数据看板后端"

	log "重新构建数据看板独立后端镜像"
	COMPOSE_PARALLEL_LIMIT=1 compose --profile data-analysis --ansi never build dashboard-migrate dashboard-api aggregation-worker alert-worker
	log "启动数据看板 MySQL并执行版本化迁移"
	compose_run up -d --wait dashboard-mysql
	compose_run run --rm --no-deps dashboard-migrate
	log "重建 dashboard-api 与 Worker；统一前端、基础平台后端和统一登录接入配置保持不变"
	compose_run up -d --wait --no-deps dashboard-api aggregation-worker alert-worker metabase
	verify_gateway_routes
	compose_run ps dashboard-api aggregation-worker alert-worker dashboard-mysql
}

start_presale_worker() {
	local skip_build=false skip_migrate=false option
	for option in "$@"; do
		case "$option" in
			--skip-build) skip_build=true ;;
			--skip-migrate) skip_migrate=true ;;
			*) fail "start-presale-worker 不支持参数：$option" ;;
		esac
	done
	ensure_platform_env_file
	ensure_contract_env_file false
	ensure_customer_env_file
	ensure_portal_env_file
	ensure_project_env_file
	validate_local_runtime_target "客户与商机管理" "$customer_env_file" customer_and_opportunity dev OIDC_CLIENT_ID
	[[ -f "$presale_worker_env_file" ]] || \
		fail "售前投递 Worker 环境文件不存在：${presale_worker_env_file}；请从 customer_and_opportunity/.env.presale-worker.example 复制并填写实际环境值"
	[[ -s "$presale_worker_env_file" ]] || \
		fail "售前投递 Worker 环境文件为空：${presale_worker_env_file}"
	if [[ "$skip_build" != true ]]; then
		prepare_go_backend_base_images "售前投递 Worker"
		log "构建售前投递 Worker 与本地集成 Mock；统一认证、审批和 PMS 地址仅从指定环境文件注入"
		COMPOSE_PARALLEL_LIMIT=1 compose --profile presale-worker --ansi never build presale-worker presale-integration-mock
	fi
	if [[ "$skip_migrate" != true ]]; then
		log "启动客户与商机数据库并执行 CRM 清单迁移"
		compose_run --profile presale-worker up -d --wait customer-mysql
		compose_run --profile presale-worker run --rm --no-deps customer-migrate
	fi
	# presale-worker 使用 --no-deps 启动，必须在此前显式拉起 Temporal；否则
	# 单独执行 start-presale-worker（尤其是 --skip-migrate）时会连接失败。
	compose_run --profile presale-worker up -d --wait temporal
	log "启动本地售前集成 Mock（仅开发联调使用）"
	compose_run --profile presale-worker up -d --wait presale-integration-mock
	log "启动售前投递 Worker"
	compose_run --profile presale-worker up -d --no-deps presale-worker

	local attempt heartbeat
	for attempt in $(seq 1 20); do
		heartbeat="$(
			compose_run exec -T customer-mysql sh -c \
				'MYSQL_PWD="$MYSQL_PASSWORD" mysql --protocol=TCP -h 127.0.0.1 -u"$MYSQL_USER" -D "$MYSQL_DATABASE" -Nse "SELECT EXISTS(SELECT 1 FROM crm_worker_heartbeats WHERE worker_type=0x70726573616c655f64656c6976657279 AND heartbeat_at >= UTC_TIMESTAMP(3) - INTERVAL 15 SECOND)"' \
				2>/dev/null || true
		)"
		if [[ "$heartbeat" == "1" ]]; then
			compose_run --profile presale-worker ps presale-worker
			log "售前投递 Worker 已启动，数据库新鲜心跳已确认"
			return 0
		fi
		sleep 1
	done

	compose_run --profile presale-worker ps -a presale-worker >&2 || true
	compose_run --profile presale-worker logs --tail 80 presale-worker >&2 || true
	fail "售前投递 Worker 未在 20 秒内产生新鲜心跳；申请入口保持安全关闭"
}

start_presale_alert_worker() {
	ensure_platform_env_file
	ensure_customer_env_file
	validate_local_runtime_target "客户与商机管理" "$customer_env_file" customer_and_opportunity dev OIDC_CLIENT_ID
	prepare_go_backend_base_images "售前预警扫描 Worker"
	log "构建售前预警扫描 Worker"
	COMPOSE_PARALLEL_LIMIT=1 compose --profile customer --ansi never build customer-presale-alert-worker
	log "启动客户与商机数据库并执行 CRM 清单迁移"
	compose_run up -d --wait customer-mysql
	compose_run run --rm --no-deps customer-migrate
	log "启动售前预警扫描 Worker"
	compose_run up -d --wait --no-deps customer-presale-alert-worker
	compose_run ps customer-presale-alert-worker customer-mysql
}

prepare_operational_env() {
    ensure_platform_env_file
    ensure_contract_env_file false
	ensure_customer_env_file
	ensure_portal_env_file
	ensure_project_env_file
}

case "$command_name" in
    up|restart)
        start_stack
        ;;
    down)
        prepare_operational_env
        # 清理旧版本或已从当前 Compose 文件移除的服务容器。否则残留的
        # profile/worker 容器会继续占用项目网络，导致 `docker compose down`
        # 报 Resource is still in use，脚本无法完成收尾。
        remove_legacy_split_runtime_containers
        if [[ "$remove_volumes" == true ]]; then
            compose_run down --volumes --remove-orphans
        else
            compose_run down --remove-orphans
        fi
        # Compose 在发现残留 endpoint 时可能已经输出过一次占用提示；容器
        # 清理后再次尝试移除网络，让 down 最终状态与实际资源一致。
        docker network rm "${compose_project}_default" >/dev/null 2>&1 || true
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
        log_args=(logs)
        [[ -n "$log_since" ]] && log_args+=(--since "$log_since")
        [[ -n "$log_tail" ]] && log_args+=(--tail "$log_tail")
        [[ "$log_follow" == true ]] && log_args+=(-f)
        if ((${#log_services[@]} > 0)); then
            compose_run "${log_args[@]}" "${log_services[@]}"
        else
            compose_run "${log_args[@]}"
        fi
        ;;
	config)
        ensure_platform_env_file
        ensure_contract_env_file true
		ensure_customer_env_file
		ensure_portal_env_file
		ensure_project_env_file
		validate_all_local_runtime_targets
		validate_all_keycloak_runtimes
		if portal_configured && ! portal_compensation_configured; then
			fail "Portal 补偿 Worker 的映射/角色分配机器凭据不完整；请在应用接入页重试 customer_portal/dev"
		fi
        ensure_catalog_publisher_credentials_consistent
        disable_contract_startup_catalog_sync
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
	refresh-customer-api)
		refresh_customer_backend
		;;
	refresh-portal-api)
		refresh_portal_backend
		;;
	refresh-project-api)
		refresh_project_backend
		;;
	refresh-data-analysis-api)
		refresh_data_analysis_backend
		;;
	start-presale-worker)
		start_presale_worker "$@"
		;;
	start-presale-alert-worker)
		start_presale_alert_worker "$@"
		;;
esac
