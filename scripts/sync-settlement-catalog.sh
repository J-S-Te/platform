#!/usr/bin/env bash
# 发布可信、由应用管理的结算授权清单。仅在部署 Agent 的一次性容器内运行，catalog-publisher 凭据通过进程环境提供。
set -euo pipefail

prefix="[settlement-catalog-sync]"
app_id="${PLATFORM_APPLICATION_ID:-}"
issuer="${PLATFORM_BASE_URL:-}"
client_id="${PLATFORM_AUTHORIZATION_CATALOG_CLIENT_ID:-}"
client_secret="${PLATFORM_AUTHORIZATION_CATALOG_CLIENT_SECRET:-}"
manifest="/catalog/permission-manifest.json"

# 预检失败分三类，文案互不相同（退出码统一为 2），便于现场区分：
#   1) 必需环境变量缺失 -> 逐个指名缺失变量（应用上下文/凭据没配）；
#   2) manifest 路径不可用 -> 打印具体路径（/catalog 挂载或路径没到位）；
#   3) manifest 内容解析失败 -> 说明清单自身损坏，而不是配置或挂载问题。
missing_env=""
if [[ -z "$app_id" ]]; then
  missing_env="PLATFORM_APPLICATION_ID"
fi
if [[ -z "$issuer" ]]; then
  missing_env="${missing_env:+$missing_env, }PLATFORM_BASE_URL"
fi
if [[ -z "$client_id" ]]; then
  missing_env="${missing_env:+$missing_env, }PLATFORM_AUTHORIZATION_CATALOG_CLIENT_ID"
fi
if [[ -z "$client_secret" ]]; then
  missing_env="${missing_env:+$missing_env, }PLATFORM_AUTHORIZATION_CATALOG_CLIENT_SECRET"
fi
if [[ -n "$missing_env" ]]; then
  echo "$prefix ERROR: required environment variables not set: $missing_env" >&2
  exit 2
fi

if [[ ! -f "$manifest" || ! -r "$manifest" ]]; then
  echo "$prefix ERROR: manifest unavailable: $manifest (missing, unreadable, or not a regular file)" >&2
  exit 2
fi

jq empty "$manifest" >/dev/null 2>&1 || {
  echo "$prefix ERROR: manifest is not valid JSON: $manifest" >&2
  exit 2
}

config_file="$(mktemp)"
trap 'rm -f -- "$config_file"' EXIT
chmod 0600 "$config_file"
escape() { printf '%s' "$1" | sed 's/\\/\\\\/g; s/"/\\"/g'; }
printf 'silent\nshow-error\nfail-with-body\nuser = "%s:%s"\n' "$(escape "$client_id")" "$(escape "$client_secret")" >"$config_file"

token_json="$(curl --config "$config_file" --data-urlencode grant_type=client_credentials --data-urlencode scope=authorization.catalog.sync --header 'Content-Type: application/x-www-form-urlencoded' --max-time 15 "${issuer%/}/oauth2/token")" || {
  echo "$prefix ERROR: catalog publisher token request failed" >&2
  exit 3
}
token="$(printf '%s' "$token_json" | jq -er '.access_token // empty')" || {
  echo "$prefix ERROR: token response did not include access_token" >&2
  exit 3
}
printf 'silent\nshow-error\nfail-with-body\nheader = "Authorization: Bearer %s"\n' "$(escape "$token")" >"$config_file"
response="$(curl --config "$config_file" --header 'Content-Type: application/json' --data-binary "@$manifest" --max-time 20 --request PUT "${issuer%/}/api/v1/applications/${app_id}/authorization-catalog")" || {
  echo "$prefix ERROR: authorization catalog publication failed" >&2
  exit 4
}
status="$(printf '%s' "$response" | jq -r '.data.sync_status // .sync_status // empty')"
if [[ "$status" != "SYNCED" ]]; then
  echo "$prefix ERROR: platform did not confirm catalog synchronization" >&2
  exit 5
fi
echo "$prefix OK: catalog published"
