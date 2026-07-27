#!/usr/bin/env bash
# 本地 Docker 编排入口：准备配置、构建镜像、执行迁移和幂等初始化超级管理员。
set -Eeuo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
project_root="$(cd "${script_dir}/.." && pwd)"
compose_file="${project_root}/compose.local.yaml"
template_file="${project_root}/docker/.env.local.example"
env_file="${project_root}/docker/.env.local"
compose_project="basic-platform-local"
command_name="up"
force_build=false
admin_display_name="${BASIC_PLATFORM_ADMIN_DISPLAY_NAME:-}"
admin_account_name="${BASIC_PLATFORM_ADMIN_ACCOUNT_NAME:-}"
admin_password="${BASIC_PLATFORM_ADMIN_PASSWORD:-}"

log() { printf '[docker-local] %s\n' "$*"; }
fail() { printf '[docker-local] ERROR: %s\n' "$*" >&2; exit 1; }

usage() {
    cat <<USAGE
用法：
  bash scripts/docker-local.sh up [选项]
  bash scripts/docker-local.sh down
  bash scripts/docker-local.sh stop
  bash scripts/docker-local.sh restart [选项]
  bash scripts/docker-local.sh ps
  bash scripts/docker-local.sh logs [服务名...]

up/restart 选项：
  --build                         重新构建后端和前端镜像
  --admin-display-name NAME       首次初始化超级管理员显示名称
  --admin-account-name NAME       首次初始化超级管理员账号
  --admin-password PASSWORD       首次初始化密码；仅建议本地临时使用，避免出现在 shell 历史
  --env-file PATH                 使用指定运行环境文件（默认 docker/.env.local）
  -h, --help                      显示帮助

首次启动若数据库中尚未存在超级管理员，必须提供上述三个管理员参数，或设置：
  BASIC_PLATFORM_ADMIN_DISPLAY_NAME
  BASIC_PLATFORM_ADMIN_ACCOUNT_NAME
  BASIC_PLATFORM_ADMIN_PASSWORD
USAGE
}

while (($# > 0)); do
    case "$1" in
        up|down|stop|restart|ps|logs)
            command_name="$1"
            shift
            ;;
        --build)
            force_build=true
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

# Compose 中的 env_file 路径需要由脚本显式传入，方便 --env-file 使用绝对路径。
export BASIC_PLATFORM_RUNTIME_ENV_FILE="$env_file"

compose() {
    docker compose --project-name "$compose_project" --file "$compose_file" --env-file "$env_file" "$@"
}

replace_line() {
    local key="$1" value="$2" tmp
    tmp="$(mktemp "${TMPDIR:-/tmp}/basic-platform-env.XXXXXX")"
    awk -v key="$key" -v value="$value" '
        index($0, key "=") == 1 { print key "=" value; found=1; next }
        { print }
        END { if (!found) print key "=" value }
    ' "$env_file" > "$tmp"
    chmod 600 "$tmp"
    mv "$tmp" "$env_file"
}

random_hex() { openssl rand -hex "$1" | tr -d '\n'; }
random_key() { openssl rand -base64 32 | tr -d '\n'; }

ensure_env_file() {
    command -v openssl >/dev/null 2>&1 || fail "未找到 openssl，无法生成本地密码和密钥"
    mkdir -p "$(dirname "$env_file")" "${project_root}/data/keys" "${project_root}/data/logs" "${project_root}/data/uploads"
    chmod 700 "${project_root}/data/keys" "${project_root}/data/logs" "${project_root}/data/uploads"

    if [[ ! -f "$env_file" ]]; then
        [[ -f "$template_file" ]] || fail "本地环境模板不存在：$template_file"
        cp "$template_file" "$env_file"
        chmod 600 "$env_file"
        replace_line MYSQL_PASSWORD "$(random_hex 24)"
        replace_line MYSQL_ROOT_PASSWORD "$(random_hex 32)"
        replace_line IAM_MOBILE_ENCRYPTION_KEY "$(random_key)"
        replace_line IAM_FEDERATED_PROVIDER_SECRET_ENCRYPTION_KEY "$(random_key)"
        replace_line IAM_EXTERNAL_LOGIN_STATE_ENCRYPTION_KEY "$(random_key)"
        replace_line IAM_BOOTSTRAP_TOKEN "$(random_hex 32)"
        log "已根据模板生成本地环境文件：$env_file"
    fi

    local unresolved
    unresolved="$(grep -E 'REPLACE_WITH_' "$env_file" | grep -v '^#' || true)"
    if [[ -n "$unresolved" ]]; then
        printf '%s\n' "$unresolved" >&2
        fail "本地环境文件仍包含未填写占位符或必需空值：$env_file"
    fi
}

compose_run() {
    compose --ansi never "$@"
}

pull_image_with_retry() {
    local image="$1"
    local max_attempts="${2:-5}"
    local attempt delay

    if docker image inspect "$image" >/dev/null 2>&1; then
        log "基础镜像已存在，跳过拉取：$image"
        return 0
    fi

    for ((attempt = 1; attempt <= max_attempts; attempt++)); do
        log "串行拉取基础镜像（${attempt}/${max_attempts}）：$image"
        if docker pull "$image"; then
            return 0
        fi

        if ((attempt < max_attempts)); then
            delay=$((attempt * 5))
            log "基础镜像拉取失败，${delay} 秒后重试：$image"
            sleep "$delay"
        fi
    done

    fail "基础镜像拉取失败：$image。请确认 Docker Desktop 的 HTTP/HTTPS 代理均指向 Clash 地址 http://127.0.0.1:7897，然后重试。"
}

prepare_base_images() {
    local image
    local images=(
        "golang:1.26.4-alpine"
        "alpine:3.21"
        "node:22-alpine"
        "nginx:1.27-alpine"
        "mysql:8.4"
    )

    log "检查并串行准备 Docker 构建及运行所需基础镜像"
    for image in "${images[@]}"; do
        pull_image_with_retry "$image"
    done
}

build_images() {
    if [[ "$force_build" == true ]]; then
        log "重新构建后端和前端镜像"
    else
        log "构建缺失或变更后的镜像"
    fi

    # 限制 Compose 并发，避免 Docker Desktop 经代理同时访问 Docker Hub 时出现 EOF 或令牌请求超时。
    COMPOSE_PARALLEL_LIMIT=1 compose --profile bootstrap --ansi never build migrate bootstrap-admin api worker frontend
}

run_migrations() {
    log "启动 MySQL 并等待健康检查"
    compose_run up -d --wait mysql
    log "执行现有数据库迁移"
    compose_run run --rm --no-deps migrate ./migrate
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

    if [[ -z "$admin_display_name" && -t 0 ]]; then
        read -r -p '首次超级管理员显示名称：' admin_display_name
    fi
    if [[ -z "$admin_account_name" && -t 0 ]]; then
        read -r -p '首次超级管理员账号：' admin_account_name
    fi
    if [[ -z "$admin_password" && -t 0 ]]; then
        read -r -s -p '首次超级管理员密码：' admin_password
        printf '\n'
    fi
    [[ -n "$admin_display_name" ]] || fail "首次初始化需要 --admin-display-name 或 BASIC_PLATFORM_ADMIN_DISPLAY_NAME"
    [[ -n "$admin_account_name" ]] || fail "首次初始化需要 --admin-account-name 或 BASIC_PLATFORM_ADMIN_ACCOUNT_NAME"
    [[ -n "$admin_password" ]] || fail "首次初始化需要 --admin-password 或 BASIC_PLATFORM_ADMIN_PASSWORD"

    log "初始化第一个超级管理员（后端命令具备数据库状态检查和并发幂等保护）"
    printf '%s\n' "$admin_password" | compose_run --profile bootstrap run --rm --no-deps -T bootstrap-admin ./bootstrap-admin \
        --display-name "$admin_display_name" \
        --account-name "$admin_account_name" \
        --password-stdin
    unset admin_password BASIC_PLATFORM_ADMIN_PASSWORD
}

start_stack() {
    ensure_env_file
    build_images
    run_migrations
    bootstrap_admin_if_needed
    log "启动 API、Worker、Frontend；宿主机仅发布 8081"
    compose_run up -d api worker frontend
    compose_run ps
    log "本地地址：http://localhost:8081"
}

case "$command_name" in
    up)
        start_stack
        ;;
    restart)
        start_stack
        ;;
    down)
        ensure_env_file
        compose_run down
        ;;
    stop)
        ensure_env_file
        compose_run stop
        ;;
    ps)
        ensure_env_file
        compose_run ps
        ;;
    logs)
        ensure_env_file
        if ((${#log_services[@]} > 0)); then
            compose_run logs -f "${log_services[@]}"
        else
            compose_run logs -f
        fi
        ;;
esac
