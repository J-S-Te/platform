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
# 手动执行时通过下列 PLATFORM_* 环境变量传入；敏感值不接受命令行参数，
# 避免进入 shell history、CI 日志和进程 argv。
# ----------------------------------------------------------------------------

set -euo pipefail

LOG_PREFIX="[contract-catalog-sync]"

if (( $# > 0 )); then
  echo "$LOG_PREFIX ERROR: command-line arguments are disabled; use protected PLATFORM_* environment variables" >&2
  exit 2
fi

APP_ID="${PLATFORM_APPLICATION_ID:-}"
ISSUER="${PLATFORM_BASE_URL:-}"
CLIENT_ID="${PLATFORM_AUTHORIZATION_CATALOG_CLIENT_ID:-}"
CLIENT_SECRET="${PLATFORM_AUTHORIZATION_CATALOG_CLIENT_SECRET:-}"
CATALOG_VERSION="${PLATFORM_AUTHORIZATION_CATALOG_VERSION:-auto}"
MYSQL_CONTAINER="${PLATFORM_MYSQL_CONTAINER:-basic-platform-local-mysql-1}"
MYSQL_USER="${PLATFORM_MYSQL_USER:-basic_platform}"
MYSQL_PASSWORD="${PLATFORM_MYSQL_PASSWORD:-}"
DB_NAME="${PLATFORM_MYSQL_DATABASE:-basic_platform}"
CLAIMS_ROLE_CONFIG_HASH="${CLAIMS_ROLE_CONFIG_HASH:-}"

if [[ -z "$APP_ID" || -z "$ISSUER" || -z "$CLIENT_ID" || -z "$CLIENT_SECRET" ]]; then
  echo "$LOG_PREFIX ERROR: PLATFORM_APPLICATION_ID, PLATFORM_BASE_URL and catalog publisher credentials are required" >&2
  exit 2
fi

if [[ -z "$MYSQL_PASSWORD" ]]; then
  echo "$LOG_PREFIX ERROR: PLATFORM_MYSQL_PASSWORD not set" >&2
  exit 2
fi

[[ "$APP_ID" =~ ^[0-9A-HJKMNP-TV-Z]{26}$ ]] || {
  echo "$LOG_PREFIX ERROR: PLATFORM_APPLICATION_ID must be an uppercase ULID" >&2
  exit 2
}

command -v jq >/dev/null 2>&1 || {
  echo "$LOG_PREFIX ERROR: jq is required" >&2
  exit 2
}
command -v curl >/dev/null 2>&1 || {
  echo "$LOG_PREFIX ERROR: curl is required" >&2
  exit 2
}

if [[ "$CATALOG_VERSION" == "auto" ]]; then
  CATALOG_VERSION="v1-$(date -u +%Y%m%d%H%M%S)"
fi

# 1) 拉 access_token
echo "$LOG_PREFIX step 1: requesting access_token from ${ISSUER%/}/oauth2/token" >&2
CURL_CONFIG_FILE="$(mktemp)"
PAYLOAD_FILE=""
cleanup() {
  rm -f -- "$CURL_CONFIG_FILE" ${PAYLOAD_FILE:+"$PAYLOAD_FILE"}
}
trap cleanup EXIT
chmod 0600 "$CURL_CONFIG_FILE"
[[ "$ISSUER" =~ ^https?://[^/?#]+$ ]] || {
  echo "$LOG_PREFIX ERROR: PLATFORM_BASE_URL must be an absolute HTTP(S) URL" >&2
  exit 2
}
curl_config_escape() {
  printf '%s' "$1" | sed 's/\\/\\\\/g; s/"/\\"/g'
}
printf 'silent\nshow-error\nfail-with-body\nuser = "%s:%s"\n' \
  "$(curl_config_escape "$CLIENT_ID")" "$(curl_config_escape "$CLIENT_SECRET")" >"$CURL_CONFIG_FILE"

TOKEN_JSON=$(curl --config "$CURL_CONFIG_FILE" \
  --data-urlencode "grant_type=client_credentials" \
  --data-urlencode "scope=authorization.catalog.sync" \
  --header "Content-Type: application/x-www-form-urlencoded" \
  --max-time 15 \
  "${ISSUER%/}/oauth2/token" 2>/dev/null) || {
  echo "$LOG_PREFIX ERROR: token endpoint unreachable" >&2
  exit 3
}
ACCESS_TOKEN=$(printf '%s' "$TOKEN_JSON" | jq -er '.access_token // empty' 2>/dev/null || true)
if [[ -z "$ACCESS_TOKEN" ]]; then
  TOKEN_ERROR=$(printf '%s' "$TOKEN_JSON" | jq -r '[.error, .error_description] | map(select(type == "string" and length > 0)) | join(": ")' 2>/dev/null || true)
  echo "$LOG_PREFIX ERROR: token response missing access_token${TOKEN_ERROR:+: $TOKEN_ERROR}" >&2
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
  obj=$(jq -cn --arg code "$code" --arg name "$name" --arg action "$action" \
    --arg rcode "$rcode" --arg rname "${RESOURCE_NAME[$rcode]:-$rcode}" --arg risk "$risk" \
    '{code:$code,name:$name,action:$action,resource_code:$rcode,resource_name:$rname,risk_level:$risk}')
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
    p_json=$(jq -cn --arg value "$p" '$value')
    PERMS_LIST+=",$p_json"
  done
  PERMS_LIST="${PERMS_LIST#,}"
  obj=$(jq -cn --arg code "$code" --arg name "$name" --arg description "$desc" \
    --argjson permissions "[$PERMS_LIST]" '{code:$code,name:$name,description:$description,permissions:$permissions}')
  if [[ $first -eq 1 ]]; then ROLES_JSON+="$obj"; first=0; else ROLES_JSON+=",$obj"; fi
done <<< "$ROLES_TSV"
ROLES_JSON+="]"

# 拼最终 payload
PAYLOAD_FILE="$(mktemp)"
chmod 0600 "$PAYLOAD_FILE"
jq -cn --arg catalog_version "$CATALOG_VERSION" \
  --argjson permissions "$PERMS_JSON" --argjson roles "$ROLES_JSON" \
  --arg claims_role_config_hash "$CLAIMS_ROLE_CONFIG_HASH" \
  '{catalog_version:$catalog_version,permissions:$permissions,roles:$roles,policy:{max_effective_roles:8},claims_role_config_hash:$claims_role_config_hash}' \
  >"$PAYLOAD_FILE"

# 3) PUT
echo "$LOG_PREFIX step 3: PUT ${ISSUER%/}/api/v1/applications/$APP_ID/authorization-catalog" >&2
printf 'silent\nshow-error\nfail-with-body\nheader = "Authorization: Bearer %s"\n' \
  "$(curl_config_escape "$ACCESS_TOKEN")" >"$CURL_CONFIG_FILE"
PUT_RESPONSE=$(curl --config "$CURL_CONFIG_FILE" \
  --header "Content-Type: application/json" \
  --header "Origin: $ISSUER" \
  --data-binary "@$PAYLOAD_FILE" \
  --max-time 20 \
  --request PUT \
  "${ISSUER%/}/api/v1/applications/$APP_ID/authorization-catalog" 2>&1) || {
  echo "$LOG_PREFIX ERROR: PUT failed: $PUT_RESPONSE" >&2
  exit 4
}

SYNC_STATUS=$(printf '%s' "$PUT_RESPONSE" | jq -r '.data.sync_status // .sync_status // empty' 2>/dev/null || true)
ROLE_COUNT=$(printf '%s' "$PUT_RESPONSE" | jq -r '(.data.roles // .roles // []) | length' 2>/dev/null || printf '0')

if [[ "$SYNC_STATUS" == "SYNCED" ]]; then
  echo "$LOG_PREFIX OK: app=$APP_ID roles=$ROLE_COUNT version=$CATALOG_VERSION"
  exit 0
fi

echo "$LOG_PREFIX WARN: sync_status=$SYNC_STATUS, body=$PUT_RESPONSE" >&2
exit 5
