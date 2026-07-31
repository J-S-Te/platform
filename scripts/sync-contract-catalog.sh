#!/usr/bin/env bash
# sync-contract-catalog.sh
# ----------------------------------------------------------------------------
# 触发 contract_management 子系统授权目录的 catalog 同步。
# 流程：
#   1. 用 catalog-publisher client 拉 /oauth2/token (client_credentials)。
#   2. 从 platform MySQL 读出该子系统的 resources / permissions / roles / role-permission 映射。
#   3. 调 PUT /api/v1/applications/{id}/authorization-catalog 把目录推给平台。
# 同步是幂等的：catalog_version 不变 + hash 一致时平台不会重写数据。
#
# 由 platform 部署助手（subsystem-provisioner 容器）在 contract-api 启动后调用，
# 也会被 docker-local.sh 的 subsystem onboarding 流程复用。
# 也可以手动执行：bash sync-contract-catalog.sh
#   <app_id> <issuer> <client_id> <client_secret>
#   [catalog_version] [mysql_container] [mysql_user] [mysql_password]
# ----------------------------------------------------------------------------

set -euo pipefail

LOG_PREFIX="[contract-catalog-sync]"

APP_ID="${1:-${PLATFORM_APPLICATION_ID:-}}"
ISSUER="${2:-${PLATFORM_BASE_URL:-}}"
CLIENT_ID="${3:-${PLATFORM_AUTHORIZATION_CATALOG_CLIENT_ID:-}}"
CLIENT_SECRET="${4:-${PLATFORM_AUTHORIZATION_CATALOG_CLIENT_SECRET:-}}"
CATALOG_VERSION="${5:-auto}"
MYSQL_CONTAINER="${6:-${PLATFORM_MYSQL_CONTAINER:-basic-platform-local-mysql-1}}"
MYSQL_USER="${7:-${PLATFORM_MYSQL_USER:-basic_platform}}"
MYSQL_PASSWORD="${8:-${PLATFORM_MYSQL_PASSWORD:-}}"
DB_NAME="${9:-${PLATFORM_MYSQL_DATABASE:-basic_platform}}"
CLAIMS_ROLE_CONFIG_HASH="${CLAIMS_ROLE_CONFIG_HASH:-}"

if [[ -z "$APP_ID" || -z "$ISSUER" || -z "$CLIENT_ID" || -z "$CLIENT_SECRET" ]]; then
  echo "$LOG_PREFIX ERROR: missing arguments. expected: <app_id> <issuer> <client_id> <client_secret> [catalog_version] [mysql_container] [mysql_user] [mysql_password] [db_name]" >&2
  exit 2
fi

if [[ -z "$MYSQL_PASSWORD" ]]; then
  echo "$LOG_PREFIX ERROR: MYSQL_PASSWORD not set (env PLATFORM_MYSQL_PASSWORD or arg 8)" >&2
  exit 2
fi

if [[ "$CATALOG_VERSION" == "auto" ]]; then
  CATALOG_VERSION="v1-$(date -u +%Y%m%d%H%M%S)"
fi

# 1) 拉 access_token
echo "$LOG_PREFIX step 1: requesting access_token from ${ISSUER%/}/oauth2/token" >&2
TOKEN_JSON=$(wget -q -O- \
  --user "$CLIENT_ID" \
  --password "$CLIENT_SECRET" \
  --post-data "grant_type=client_credentials&scope=authorization.catalog.sync" \
  --header "Content-Type: application/x-www-form-urlencoded" \
  --timeout 15 \
  "${ISSUER%/}/oauth2/token" 2>/dev/null) || {
  echo "$LOG_PREFIX ERROR: token endpoint unreachable" >&2
  exit 3
}
ACCESS_TOKEN=$(printf '%s' "$TOKEN_JSON" | sed -nE 's/.*"access_token":"([^"]+)".*/\1/p')
if [[ -z "$ACCESS_TOKEN" ]]; then
  echo "$LOG_PREFIX ERROR: token response missing access_token: $TOKEN_JSON" >&2
  exit 3
fi
echo "$LOG_PREFIX step 1: got access_token (length=${#ACCESS_TOKEN})" >&2

# 2) 从 platform DB 读出该应用的 resources / permissions / roles
#    用 docker exec（容器内 mysql 客户端），密码从 env 传入，ps 不可见。
run_mysql() {
  docker exec -i -e "MYSQL_PWD=$MYSQL_PASSWORD" "$MYSQL_CONTAINER" \
    mysql -u"$MYSQL_USER" --default-character-set=utf8mb4 -N -B \
    "$DB_NAME" -e "$1" 2>/dev/null
}

# JSON-escape 工具：把 stdin 的字符串 escape 成合法 JSON 字符串（不含外侧引号）
json_escape() {
  sed -e 's/\\/\\\\/g' -e 's/"/\\"/g' -e "s/	/\\t/g"
}

echo "$LOG_PREFIX step 2: reading resources / permissions / roles from $MYSQL_CONTAINER" >&2
RESOURCES_TSV=$(run_mysql "SELECT code, name FROM authz_resource WHERE application_id='$APP_ID' AND status='ACTIVE' ORDER BY code;")
PERMS_TSV=$(run_mysql "
  SELECT p.code, p.name, p.action, UPPER(IFNULL(p.risk_level,'LOW')), r.code
  FROM authz_permission p JOIN authz_resource r ON r.id=p.resource_id
  WHERE p.application_id='$APP_ID' AND p.status='ACTIVE'
  ORDER BY p.code;")
ROLE_PERMS_TSV=$(run_mysql "
  SELECT ro.code, p.code
  FROM authz_role_permission rp
  JOIN authz_role ro ON ro.id=rp.role_id
  JOIN authz_permission p ON p.id=rp.permission_id
  WHERE ro.application_id='$APP_ID'
  ORDER BY ro.code, p.code;")
ROLES_TSV=$(run_mysql "SELECT code, name, IFNULL(description,'') FROM authz_role WHERE application_id='$APP_ID' AND status='ACTIVE' AND role_type<>'COMPATIBILITY' ORDER BY code;")

# 拼 resources map (key -> name)，用资源代码作 fallback
declare -A RESOURCE_NAME
while IFS=$'\t' read -r code name; do
  [[ -z "$code" ]] && continue
  RESOURCE_NAME["$code"]="$name"
done <<< "$RESOURCES_TSV"

# 拼 permissions 数组
PERMS_JSON="["
first=1
while IFS=$'\t' read -r code name action risk rcode; do
  [[ -z "$code" ]] && continue
  code_e=$(printf '%s' "$code" | json_escape)
  name_e=$(printf '%s' "$name" | json_escape)
  action_e=$(printf '%s' "$action" | json_escape)
  rcode_e=$(printf '%s' "$rcode" | json_escape)
  rname_e=$(printf '%s' "${RESOURCE_NAME[$rcode]:-$rcode}" | json_escape)
  risk_e=$(printf '%s' "$risk" | json_escape)
  obj="{\"code\":\"$code_e\",\"name\":\"$name_e\",\"action\":\"$action_e\",\"resource_code\":\"$rcode_e\",\"resource_name\":\"$rname_e\",\"risk_level\":\"$risk_e\"}"
  if [[ $first -eq 1 ]]; then PERMS_JSON+="$obj"; first=0; else PERMS_JSON+=",$obj"; fi
done <<< "$PERMS_TSV"
PERMS_JSON+="]"

# 拼 role -> permissions map
declare -A ROLE_PERMS
while IFS=$'\t' read -r rcode pcode; do
  [[ -z "$rcode" || -z "$pcode" ]] && continue
  ROLE_PERMS["$rcode"]+="${pcode}|"
done <<< "$ROLE_PERMS_TSV"

# 拼 roles 数组
ROLES_JSON="["
first=1
while IFS=$'\t' read -r code name desc; do
  [[ -z "$code" ]] && continue
  IFS='|' read -ra perms_arr <<< "${ROLE_PERMS[$code]:-}"
  PERMS_LIST=""
  for p in "${perms_arr[@]}"; do
    [[ -z "$p" ]] && continue
    p_e=$(printf '%s' "$p" | json_escape)
    PERMS_LIST+=",\"$p_e\""
  done
  PERMS_LIST="${PERMS_LIST#,}"
  code_e=$(printf '%s' "$code" | json_escape)
  name_e=$(printf '%s' "$name" | json_escape)
  desc_e=$(printf '%s' "$desc" | json_escape)
  obj="{\"code\":\"$code_e\",\"name\":\"$name_e\",\"description\":\"$desc_e\",\"permissions\":[$PERMS_LIST]}"
  if [[ $first -eq 1 ]]; then ROLES_JSON+="$obj"; first=0; else ROLES_JSON+=",$obj"; fi
done <<< "$ROLES_TSV"
ROLES_JSON+="]"

# 拼最终 payload
PAYLOAD=$(cat <<EOF
{
  "catalog_version": "$CATALOG_VERSION",
  "permissions": $PERMS_JSON,
  "roles": $ROLES_JSON,
  "policy": {"max_effective_roles": 8},
  "claims_role_config_hash": "$CLAIMS_ROLE_CONFIG_HASH"
}
EOF
)

# 3) PUT
echo "$LOG_PREFIX step 3: PUT ${ISSUER%/}/api/v1/applications/$APP_ID/authorization-catalog" >&2
PUT_RESPONSE=$(wget -q -O- \
  --header "Authorization: Bearer $ACCESS_TOKEN" \
  --header "Content-Type: application/json" \
  --header "Origin: $ISSUER" \
  --body-data "$PAYLOAD" \
  --timeout 20 \
  --method PUT \
  "${ISSUER%/}/api/v1/applications/$APP_ID/authorization-catalog" 2>&1) || {
  echo "$LOG_PREFIX ERROR: PUT failed: $PUT_RESPONSE" >&2
  exit 4
}

SYNC_STATUS=$(printf '%s' "$PUT_RESPONSE" | sed -nE 's/.*"sync_status":"([^"]+)".*/\1/p')
ROLE_COUNT=$(printf '%s' "$PUT_RESPONSE" | grep -o '"code":"[a-z_]*"' | wc -l | tr -d ' ')

if [[ "$SYNC_STATUS" == "SYNCED" ]]; then
  echo "$LOG_PREFIX OK: app=$APP_ID roles=$ROLE_COUNT version=$CATALOG_VERSION"
  exit 0
fi

echo "$LOG_PREFIX WARN: sync_status=$SYNC_STATUS, body=$PUT_RESPONSE" >&2
exit 5
