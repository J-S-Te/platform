#!/usr/bin/env bash
set -euo pipefail

# 临时局域网访问开关。
# 仅适用于 compose.local.yaml；不修改 compose.yaml 和常规 .env.local 文件。
# enable 会重建 api、contract-api、frontend，使公开地址和 OIDC 回调全部切换为 LAN 地址。
# disable 会删除临时覆盖文件并将 frontend 重新绑定到 127.0.0.1。

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
project_root="$(cd "${script_dir}/.." && pwd)"
workspace_root="$(cd "${project_root}/.." && pwd)"
contract_root="${workspace_root}/contract_management"
compose_file="${project_root}/compose.local.yaml"
platform_env_file="${project_root}/docker/.env.local"
contract_env_file="${contract_root}/.env.local"
override_file="${project_root}/docker/.env.lan"
placeholder_file="${project_root}/docker/.env.lan.disabled"
compose_project="basic-platform-local"

log() { printf '[lan-access] %s\n' "$*"; }
fail() { printf '[lan-access] ERROR: %s\n' "$*" >&2; exit 1; }

usage() {
    cat <<'USAGE'
用法：
  bash scripts/lan-access.sh enable [--address IPv4] [--port PORT]
  bash scripts/lan-access.sh disable
  bash scripts/lan-access.sh status

说明：
  enable  使用检测到的局域网 IPv4（或 --address 指定地址）发布统一前端，
          并临时同步基础平台/合同管理的公开地址、OIDC issuer 和回调地址。
  disable 关闭局域网监听，删除临时覆盖文件，并恢复为仅本机 127.0.0.1 访问。

示例：
  bash scripts/lan-access.sh enable
  bash scripts/lan-access.sh enable --address 192.168.1.25 --port 8081
  bash scripts/lan-access.sh disable
USAGE
}

command_name="${1:-}"
[[ -n "$command_name" ]] || { usage; exit 1; }
shift || true

address=""
port="${FRONTEND_HTTP_PORT:-8081}"
while (($# > 0)); do
    case "$1" in
        --address)
            (($# >= 2)) || fail '--address 缺少 IPv4 参数'
            address="$2"
            shift 2
            ;;
        --port)
            (($# >= 2)) || fail '--port 缺少端口参数'
            port="$2"
            shift 2
            ;;
        -h|--help)
            usage
            exit 0
            ;;
        *)
            fail "未知参数：$1"
            ;;
    esac
done

[[ "$port" =~ ^[1-9][0-9]{0,4}$ ]] && ((port <= 65535)) || fail '端口必须是 1-65535 的整数'
[[ -f "$compose_file" ]] || fail "Compose 文件不存在：$compose_file"
[[ -f "$platform_env_file" ]] || fail "基础平台环境文件不存在：$platform_env_file；请先执行 bash scripts/docker-local.sh up"
[[ -f "$contract_env_file" ]] || fail "合同管理环境文件不存在：$contract_env_file；请先执行 bash scripts/docker-local.sh up"
[[ -f "$placeholder_file" ]] || fail "局域网占位环境文件不存在：$placeholder_file"
command -v docker >/dev/null 2>&1 || fail '未找到 docker 命令'
docker compose version >/dev/null 2>&1 || fail '当前 Docker 不支持 docker compose 子命令'

is_ipv4() {
    local candidate="$1" octet
    [[ "$candidate" =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ ]] || return 1
    IFS='.' read -r -a octets <<< "$candidate"
    for octet in "${octets[@]}"; do
        ((10#$octet <= 255)) || return 1
    done
}

detect_lan_address() {
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
        if is_ipv4 "$candidate" && [[ "$candidate" != 127.* ]]; then
            printf '%s' "$candidate"
            return 0
        fi
    done < <(ifconfig 2>/dev/null | awk '/inet / {print $2}')
    return 1
}

compose_with_lan() {
    BASIC_PLATFORM_RUNTIME_ENV_FILE="$platform_env_file" \
    CONTRACT_RUNTIME_ENV_FILE="$contract_env_file" \
    BASIC_PLATFORM_LAN_OVERRIDE_ENV_FILE="$override_file" \
    CONTRACT_LAN_OVERRIDE_ENV_FILE="$override_file" \
    BASIC_PLATFORM_HOST_PROJECT_ROOT="$project_root" \
    SUBSYSTEM_HOST_PROJECTS_ROOT="$workspace_root" \
    FRONTEND_BIND_ADDRESS=0.0.0.0 \
    FRONTEND_HTTP_PORT="$port" \
    docker compose --project-name "$compose_project" \
        --file "$compose_file" \
        --env-file "$platform_env_file" \
        --env-file "$contract_env_file" \
        "$@"
}

compose_local_only() {
    BASIC_PLATFORM_RUNTIME_ENV_FILE="$platform_env_file" \
    CONTRACT_RUNTIME_ENV_FILE="$contract_env_file" \
    BASIC_PLATFORM_LAN_OVERRIDE_ENV_FILE="$placeholder_file" \
    CONTRACT_LAN_OVERRIDE_ENV_FILE="$placeholder_file" \
    BASIC_PLATFORM_HOST_PROJECT_ROOT="$project_root" \
    SUBSYSTEM_HOST_PROJECTS_ROOT="$workspace_root" \
    FRONTEND_BIND_ADDRESS=127.0.0.1 \
    FRONTEND_HTTP_PORT="$port" \
    docker compose --project-name "$compose_project" \
        --file "$compose_file" \
        --env-file "$platform_env_file" \
        --env-file "$contract_env_file" \
        "$@"
}

write_override() {
    local public_origin="$1"
    umask 077
    cat > "$override_file" <<EOF
# 由 scripts/lan-access.sh 自动生成；执行 disable 会删除本文件。
# 仅用于临时局域网访问，不要提交到版本库。
APP_PUBLIC_BASE_URL=${public_origin}
APP_CORS_ALLOWED_ORIGINS=${public_origin}
OIDC_ISSUER=${public_origin}
OIDC_ISSUER_BASE_URL=${public_origin}
OIDC_REDIRECT_URI=${public_origin}/contract_management/auth/callback
APP_PUBLIC_URL=${public_origin}/contract_management/
EOF
    chmod 600 "$override_file"
}

case "$command_name" in
    enable)
        if [[ -z "$address" ]]; then
            address="$(detect_lan_address || true)"
        fi
        is_ipv4 "$address" || fail '未检测到可用局域网 IPv4；请显式传入 --address，例如 --address 192.168.1.25'
        [[ "$address" != 127.* ]] || fail '--address 不能是 127.0.0.1'

        public_origin="http://${address}:${port}"
        write_override "$public_origin"
        log "正在发布临时局域网地址：${public_origin}"
        log '将重建 api、contract-api、frontend；数据库、角色、用户和生产 compose 配置不会被修改。'
        compose_with_lan up -d --wait --force-recreate api contract-api frontend
        log "已启用。局域网访问地址：${public_origin}"
        log "合同管理地址：${public_origin}/contract_management/"
        log '请仅在可信局域网使用；当前模式为 HTTP，不应暴露到互联网。'
        ;;
    disable)
        rm -f "$override_file"
        log '正在关闭临时局域网访问并恢复仅本机监听…'
        compose_local_only up -d --wait --force-recreate api contract-api frontend
        log "已关闭。仅可通过 http://127.0.0.1:${port} 访问。"
        ;;
    status)
        if [[ -f "$override_file" ]]; then
            origin="$(awk -F= '$1 == "APP_PUBLIC_BASE_URL" {print substr($0, index($0, "=") + 1); exit}' "$override_file")"
            log "临时局域网访问已启用：${origin:-'(地址未读取到)'}"
        else
            log "临时局域网访问未启用；预期仅本机地址：http://127.0.0.1:${port}"
        fi
        docker ps --filter "name=${compose_project}-frontend" --format 'table {{.Names}}\t{{.Ports}}' || true
        ;;
    *)
        usage
        exit 1
        ;;
esac
