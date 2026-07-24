#!/usr/bin/env bash
# 为 Docker Compose 本地部署生成专用运行配置和持久化目录，不读取或覆盖项目根目录 .env。
set -Eeuo pipefail

project_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
env_file="${project_root}/docker/.env"
force=false

if [[ "${1:-}" == "--force" ]]; then
    force=true
elif [[ $# -gt 0 ]]; then
    echo "用法: $0 [--force]" >&2
    exit 64
fi

if ! command -v openssl >/dev/null 2>&1; then
    echo "未找到 openssl，无法安全生成 Docker 运行配置。" >&2
    exit 1
fi

if [[ -e "$env_file" && "$force" != true ]]; then
    echo "配置文件已存在：$env_file" >&2
    echo "如确认需要重新生成（会替换数据库密码和初始化令牌），请使用：$0 --force" >&2
    exit 1
fi

base64_key() {
    openssl rand -base64 32 | tr -d '\n'
}

random_hex() {
    openssl rand -hex "$1"
}

mkdir -p "${project_root}/docker" \
    "${project_root}/data/keys" \
    "${project_root}/data/logs" \
    "${project_root}/data/uploads"
chmod 700 "${project_root}/data/keys" "${project_root}/data/logs" "${project_root}/data/uploads"

umask 077
cat > "$env_file" <<ENV
# 由 scripts/prepare-docker-env.sh 于 $(date '+%Y-%m-%d %H:%M:%S %z') 生成。
# 包含密码、加密密钥和首次初始化令牌，禁止提交或发送到不可信环境。
APP_ENV=development
APP_NAME=basic-platform
APP_TIMEZONE=Asia/Shanghai
APP_HTTP_ADDR=:8080
APP_PUBLIC_BASE_URL=http://localhost:7897
APP_CORS_ALLOWED_ORIGINS=http://localhost:7897

MYSQL_HOST=mysql
MYSQL_PORT=3306
MYSQL_DATABASE=basic_platform
MYSQL_USERNAME=basic_platform
MYSQL_USER=basic_platform
MYSQL_PASSWORD=$(random_hex 24)
MYSQL_ROOT_PASSWORD=$(random_hex 32)
MYSQL_PARAMS=charset=utf8mb4&parseTime=true&loc=UTC

AUTH_JWT_ISSUER=basic-platform
AUTH_JWT_AUDIENCE=basic-platform-api
AUTH_APPLICATION_JWT_AUDIENCE=basic-platform-application
AUTH_JWT_PRIVATE_KEY_PATH=/app/data/keys/jwt-ed25519-private.pem
AUTH_JWT_PUBLIC_KEY_PATH=/app/data/keys/jwt-ed25519-public.pem
AUTH_SESSION_COOKIE_NAME=bp_session
AUTH_SESSION_COOKIE_SECURE=false
AUTH_SESSION_COOKIE_SAME_SITE=Lax
AUTH_SESSION_TTL=8h

OIDC_ISSUER=http://localhost:7897

IAM_MOBILE_ENCRYPTION_KEY=$(base64_key)
IAM_MFA_ENCRYPTION_KEY=$(base64_key)
IAM_FEDERATED_PROVIDER_SECRET_ENCRYPTION_KEY=$(base64_key)
IAM_EXTERNAL_LOGIN_STATE_ENCRYPTION_KEY=$(base64_key)
IAM_EXTERNAL_OIDC_HTTP_TIMEOUT=10s
IAM_DINGTALK_HTTP_TIMEOUT=10s
IAM_EXTERNAL_OIDC_ALLOW_INSECURE_HTTP=false
IAM_EXTERNAL_OIDC_ALLOWED_HOSTS=
IAM_BOOTSTRAP_TOKEN=$(random_hex 32)

AUDIT_APPLICATION_CODE=platform
AUDIT_ENVIRONMENT_CODE=dev
FILE_STORAGE_ROOT=/app/data/uploads
ASYNC_WORKER_ID=basic-platform-worker-docker
ASYNC_WORKER_POLL_INTERVAL=2s
ASYNC_WORKER_STALE_LOCK_TIMEOUT=5m
LOG_LEVEL=info
LOG_FORMAT=json
LOG_DIRECTORY=/app/data/logs
ENV

chmod 600 "$env_file"
echo "已生成 Docker 运行配置：$env_file"
echo "已创建持久化目录：${project_root}/data/{keys,logs,uploads}"
echo "下一步执行：docker compose up --build -d"
