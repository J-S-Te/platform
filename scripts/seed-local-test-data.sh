#!/usr/bin/env bash
# 向本地 Docker MySQL 写入符合当前身份、组织和授权模型的测试数据。
# 该脚本不创建测试登录账号，不写入伪造审计事件，并且可以重复执行。

set -Eeuo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd -- "${SCRIPT_DIR}/.." && pwd)"
COMPOSE_FILE="${PROJECT_ROOT}/compose.yaml"
ENV_FILE="${PROJECT_ROOT}/docker/.env"
PROJECT_NAME="basic-platform"
SQL_FILE="${PROJECT_ROOT}/docker/seed-local-test-data.sql"
DRY_RUN=false

log() {
  printf '[seed-test-data] %s\n' "$*"
}

fail() {
  printf '[seed-test-data] ERROR: %s\n' "$*" >&2
  exit 1
}

usage() {
  cat <<'EOF'
用法：
  bash scripts/seed-local-test-data.sh [选项]

选项：
  --compose-file FILE  Compose 文件，默认 compose.yaml
  --env-file FILE      Compose 环境文件，默认 docker/.env
  --project-name NAME  Compose 项目名，默认 basic-platform
  --dry-run            仅检查前置条件，不写数据库
  -h, --help           显示帮助

说明：
  - 复用 default 租户、platform 应用、ROOT 组织和 platform-user 内置角色。
  - 创建 5 个普通用户、6 个组织单元、5 个岗位、5 条主任职和 5 条普通用户角色绑定。
  - 不创建登录账号：当前系统中“用户”和“登录账号”是两个独立生命周期。
  - 不写入审计事件：审计数据应由真实接口操作产生。
  - 脚本幂等，可重复执行，不会重复插入同一批测试数据。
EOF
}

while (($# > 0)); do
  case "$1" in
    --compose-file)
      (($# >= 2)) || fail '--compose-file 缺少参数'
      COMPOSE_FILE="$2"
      shift 2
      ;;
    --env-file)
      (($# >= 2)) || fail '--env-file 缺少参数'
      ENV_FILE="$2"
      shift 2
      ;;
    --project-name)
      (($# >= 2)) || fail '--project-name 缺少参数'
      PROJECT_NAME="$2"
      shift 2
      ;;
    --dry-run)
      DRY_RUN=true
      shift
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

[[ -f "$COMPOSE_FILE" ]] || fail "Compose 文件不存在：${COMPOSE_FILE}"
[[ -f "$ENV_FILE" ]] || fail "环境文件不存在：${ENV_FILE}"
[[ -f "$SQL_FILE" ]] || fail "测试数据 SQL 不存在：${SQL_FILE}"
command -v docker >/dev/null 2>&1 || fail '未找到 docker 命令'
docker compose version >/dev/null 2>&1 || fail '当前 Docker 未提供 compose 子命令'
docker info >/dev/null 2>&1 || fail 'Docker daemon 未运行或当前用户无权访问'

compose=(
  docker compose
  --project-name "$PROJECT_NAME"
  --env-file "$ENV_FILE"
  --file "$COMPOSE_FILE"
)

running_services="$(${compose[@]} ps --status running --services)"
grep -qx 'mysql' <<<"$running_services" || fail 'mysql 服务未运行，请先启动本地 Docker 环境'

mysql_exec() {
  "${compose[@]}" exec -T mysql sh -c \
    'exec mysql --default-character-set=utf8mb4 --batch --raw -uroot -p"$MYSQL_ROOT_PASSWORD" "$MYSQL_DATABASE"'
}

preflight_sql=$(cat <<'SQL'
SELECT CONCAT_WS('|',
  (SELECT COUNT(*) FROM iam_tenant WHERE code = 'default' AND status = 'ACTIVE'),
  (SELECT COUNT(*)
   FROM platform_application a
   JOIN iam_tenant t ON t.id = a.tenant_id
   WHERE t.code = 'default' AND a.code = 'platform' AND a.status = 'ACTIVE'),
  (SELECT COUNT(*)
   FROM authz_role r
   JOIN iam_tenant t ON t.id = r.tenant_id
   WHERE t.code = 'default' AND r.code = 'platform-user' AND r.status = 'ACTIVE'),
  (SELECT COUNT(*)
   FROM iam_org_unit o
   JOIN iam_tenant t ON t.id = o.tenant_id
   WHERE t.code = 'default' AND o.code = 'ROOT' AND o.status = 'ACTIVE'),
  (SELECT COUNT(*)
   FROM iam_bootstrap_state s
   JOIN iam_tenant t ON t.id = s.tenant_id
   WHERE t.code = 'default')
);
SQL
)

preflight_result="$(printf '%s\n' "$preflight_sql" | mysql_exec | tail -n 1)"
[[ "$preflight_result" == '1|1|1|1|1' ]] || fail \
  "数据库基础数据不完整（tenant|application|role|root_org|bootstrap=${preflight_result}），请先完成迁移和超级管理员初始化"

log "Compose 项目：${PROJECT_NAME}"
log "Compose 文件：${COMPOSE_FILE}"
log "环境文件：${ENV_FILE}"
log '基础数据检查通过：default 租户、platform 应用、ROOT 组织、platform-user 角色、超级管理员均存在。'

if [[ "$DRY_RUN" == true ]]; then
  log 'dry-run 完成，未写入数据库。'
  exit 0
fi

log '开始写入组织、岗位、普通用户、主任职和普通用户角色绑定。'
mysql_exec < "$SQL_FILE"

log '测试数据写入完成。页面刷新后预期统计至少为：用户 6、账号 1、组织 7、任职 5。'
log '测试用户未创建登录账号；如需登录，请在“登录账号”页按独立业务流程为指定用户创建账号。'
