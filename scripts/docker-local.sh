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
  bash scripts/docker-local.sh refresh-customer-api
  bash scripts/docker-local.sh refresh-portal-api

up/restart 选项：
  --build                         重新构建统一前端和三个常驻后端镜像；已接入门户另行构建 portal-api
  --pull                          兼容选项；启动前默认会串行拉取缺失的基础镜像
  --admin-display-name NAME       首次初始化超级管理员显示名称
  --admin-account-name NAME       首次初始化超级管理员账号
  --admin-password PASSWORD       首次初始化密码；仅为兼容旧调用保留，不建议使用
  --admin-password-stdin          从标准输入读取首次初始化密码；适合 CI Secret
  --env-file PATH                 基础平台环境文件（默认 platform/docker/.env.local）
  --contract-env-file PATH        合同后端环境文件（默认 contract_management/.env.local）
  --customer-env-file PATH        客户与商机后端环境文件（默认 platform/docker/.env.customer.local）
  --portal-env-file PATH          客户自助门户环境文件（默认 platform/docker/.env.portal.local）
  -h, --help                      显示帮助

定向更新：
  refresh-api           只重建基础平台后端镜像，执行基础平台迁移，并重启 api/受控 provisioner
  refresh-frontend      只重建并重启统一 frontend；四个前端模块同时更新
  refresh-contract-api  只重建合同管理后端镜像，执行合同迁移，并重启 contract-api
  refresh-customer-api  重建客户与商机管理后端、执行 CRM 迁移，并刷新统一前端网关
  refresh-portal-api    重建客户自助门户后端、执行 Portal 迁移；仅在已完成应用接入后启动

  四种定向更新都不会删除或重建 Application、Environment、LoginTarget、OAuth Client，
  因此不会影响已经完成的子系统统一登录接入。

应用容器：
  frontend      基础平台 + 合同管理 + 客户与商机管理 + 客户自助门户前端（宿主机仅发布 8081）
  api           基础平台 API + Worker
  contract-api  合同管理 API + Temporal Worker
  customer-api  客户与商机管理 API
  portal-api    客户自助门户 API（完成 customer_portal/dev 接入后启用）

首次启动若数据库中尚未存在超级管理员，必须提供三个管理员参数，或设置：
  BASIC_PLATFORM_ADMIN_DISPLAY_NAME
  BASIC_PLATFORM_ADMIN_ACCOUNT_NAME
  BASIC_PLATFORM_ADMIN_PASSWORD
USAGE
}

remove_volumes=false
while (($# > 0)); do
    case "$1" in
		up|down|stop|restart|ps|logs|config|verify|refresh-api|refresh-frontend|refresh-contract-api|refresh-customer-api|refresh-portal-api)
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
[[ -d "$customer_root" ]] || fail "客户与商机管理后端目录不存在：$customer_root"

export BASIC_PLATFORM_RUNTIME_ENV_FILE="$env_file"
export CONTRACT_RUNTIME_ENV_FILE="$contract_env_file"
export CUSTOMER_RUNTIME_ENV_FILE="$customer_env_file"
export PORTAL_RUNTIME_ENV_FILE="$portal_env_file"
export BASIC_PLATFORM_HOST_PROJECT_ROOT="$project_root"
export SUBSYSTEM_HOST_PROJECTS_ROOT="$workspace_root"

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
		--env-file "$customer_env_file" \
		--env-file "$portal_env_file" \
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
}

ensure_customer_env_file() {
    command -v openssl >/dev/null 2>&1 || fail "未找到 openssl，无法生成客户与商机管理数据库密码和敏感字段密钥"
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

portal_configured() {
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
        log "重新构建统一前端、基础平台、合同管理和客户与商机管理后端镜像"
    else
        log "构建缺失或有变更的四个业务镜像"
    fi
    # migrate/bootstrap-admin/subsystem-provisioner 都复用 api 构建出的
    # basic-platform/backend:local；这里只构建四个业务镜像各一次。
    COMPOSE_PARALLEL_LIMIT=1 compose --profile bootstrap --ansi never build \
        api contract-api customer-api frontend
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
	log "已清理内置业务模块的旧式整站反向代理；四个前端均由统一 frontend 容器直接承载"
}

run_migrations() {
	log "启动基础平台、合同、CRM 三个 MySQL 与 Temporal，并等待健康检查"
	compose_run up -d --wait mysql contract-mysql customer-mysql temporal
    log "执行基础平台数据库迁移"
    compose_run run --rm --no-deps migrate ./migrate
    log "执行合同管理版本化数据库迁移"
    compose_run run --rm --no-deps contract-migrate
	log "执行客户与商机管理 CRM 清单迁移"
	compose_run run --rm --no-deps customer-migrate
	if portal_configured; then
		log "启动客户自助门户 MySQL 并执行 Portal 清单迁移"
		compose_run --profile portal up -d --wait portal-mysql
		compose_run --profile portal run --rm --no-deps portal-migrate
	fi
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
	local customer_health_status customer_session_status portal_health_status portal_session_status
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

    customer_health_status="$(frontend_http_status /customer-opportunity/healthz)" || \
        fail "无法访问客户与商机管理健康检查路径"
    [[ "$customer_health_status" == "200" ]] || \
        fail "客户与商机管理健康检查路径返回 ${customer_health_status}，预期为 200"

	customer_session_status="$(frontend_http_status /customer-opportunity/api/v1/auth/me)" || \
        fail "无法访问客户与商机管理登录状态接口"
    case "$customer_session_status" in
        401|503) ;;
        *) fail "客户与商机管理登录状态接口返回 ${customer_session_status}，预期开发认证未携带身份时为 401（或认证未配置时为 503）" ;;
	esac

	if portal_configured; then
		portal_health_status="$(frontend_http_status /customer-portal/healthz)" || \
			fail "无法访问客户自助门户健康检查路径"
		[[ "$portal_health_status" == "200" ]] || \
			fail "客户自助门户健康检查路径返回 ${portal_health_status}，预期为 200"
		portal_session_status="$(frontend_http_status /customer-portal/api/v1/auth/me)" || \
			fail "无法访问客户自助门户登录状态接口"
		[[ "$portal_session_status" == "401" ]] || \
			fail "客户自助门户登录状态接口返回 ${portal_session_status}，预期未登录状态为 401"
	fi

    log "统一网关校验通过：platform API=401，contract healthz=200，customer healthz=200"
}

start_stack() {
    ensure_platform_env_file
    ensure_contract_env_file true
	ensure_customer_env_file
	ensure_portal_env_file
    ensure_catalog_publisher_credentials_consistent
    disable_contract_startup_catalog_sync
    prepare_gateway_config
    build_images
    run_migrations
    bootstrap_admin_if_needed
    # 分阶段启动，避免三个 API 和 frontend 在同一次 Compose wait 中
    # 把下游的短暂冷启动误报为整套部署失败，同时让错误日志能够准确指向服务。
    log "启动基础平台 API 与受控子系统 provisioner"
    compose_up_wait "基础平台 API" subsystem-provisioner api
    log "启动合同管理后端"
    compose_up_wait "合同管理后端" contract-api
	log "启动客户与商机管理后端"
	compose_up_wait "客户与商机管理后端" customer-api
	if portal_configured; then
		log "启动已接入的客户自助门户后端"
		compose_run --profile portal up -d --wait portal-api
	else
		log "客户自助门户尚未接入，跳过 portal-api；可在应用接入中创建 customer_portal/dev"
	fi
    log "启动统一前端"
    compose_up_wait "统一前端" frontend
    verify_gateway_routes
    compose_run ps
    log "统一访问地址：${frontend_public_origin}"
    log "合同管理前端：${frontend_public_origin}/contract_management/"
	log "客户与商机管理前端：${frontend_public_origin}/customer-opportunity/"
	if portal_configured; then log "客户自助门户：${frontend_public_origin}/customer-portal/"; fi
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

    log "重新构建统一前端镜像（基础平台 + 合同管理 + 客户与商机管理前端）"
    COMPOSE_PARALLEL_LIMIT=1 compose --ansi never build frontend
    log "重建统一 frontend 容器；平台/合同/CRM 后端、可选门户后端和统一登录接入配置保持不变"
    compose_run up -d --wait --no-deps frontend
    verify_gateway_routes
    compose_run ps frontend
}

refresh_contract_backend() {
    ensure_platform_env_file
    ensure_contract_env_file true
    ensure_customer_env_file
	ensure_portal_env_file
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
    prepare_go_backend_base_images "客户与商机管理后端"

    log "重新构建客户与商机管理后端镜像（不构建 frontend、基础平台 api 或 contract-api）"
    COMPOSE_PARALLEL_LIMIT=1 compose --ansi never build customer-api
    prepare_gateway_config
    log "重新构建统一前端网关，使客户与商机管理路径转发到 customer-api"
    COMPOSE_PARALLEL_LIMIT=1 compose --ansi never build frontend
    log "启动客户与商机数据库并执行 CRM 清单迁移"
    compose_run up -d --wait customer-mysql
    compose_run run --rm --no-deps customer-migrate
    log "重建 customer-api 与统一前端网关；另外两个后端和统一登录接入配置保持不变"
    compose_run up -d --wait --no-deps customer-api
    compose_run up -d --wait --no-deps frontend
    verify_gateway_routes
	compose_run ps customer-api customer-mysql
}

refresh_portal_backend() {
	ensure_platform_env_file
	ensure_contract_env_file false
	ensure_customer_env_file
	ensure_portal_env_file
	portal_configured || fail "客户自助门户尚未完成 customer_portal/dev 应用接入，不能启动 portal-api"
	prepare_go_backend_base_images "客户自助门户后端"

	log "重新构建客户/门户共享后端镜像"
	COMPOSE_PARALLEL_LIMIT=1 compose --profile portal --ansi never build portal-api
	prepare_gateway_config
	log "刷新统一前端网关中的客户门户路由"
	COMPOSE_PARALLEL_LIMIT=1 compose --ansi never build frontend
	log "启动门户数据库并执行 Portal 清单迁移"
	compose_run --profile portal up -d --wait portal-mysql
	compose_run --profile portal run --rm --no-deps portal-migrate
	log "重建 portal-api 与统一前端"
	compose_run --profile portal up -d --wait --no-deps portal-api
	compose_run up -d --wait --no-deps frontend
	verify_gateway_routes
	compose_run --profile portal ps portal-api portal-mysql frontend
}

prepare_operational_env() {
    ensure_platform_env_file
    ensure_contract_env_file false
	ensure_customer_env_file
	ensure_portal_env_file
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
		ensure_customer_env_file
		ensure_portal_env_file
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
esac
