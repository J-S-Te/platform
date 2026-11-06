#!/usr/bin/env bash
# 发布由项目管理应用拥有的授权目录。
set -euo pipefail

prefix="[project-catalog-sync]"
manifest=/catalog/permission-manifest.json

# 预检失败分三类，文案互不相同（退出码统一为 2），与 sync-settlement-catalog.sh 保持一致：
#   1) 必需环境变量缺失 -> 逐个指名缺失变量（应用上下文/凭据没配）；
#   2) manifest 路径不可用 -> 打印具体路径（/catalog 挂载或路径没到位）；
#   3) manifest 内容解析失败 -> 说明清单自身损坏，而不是配置或挂载问题。
# 缺 env 必须在任何 curl/mktemp 之前失败：缺失时零请求、零临时文件。
missing_env=""
if [[ -z "${PLATFORM_APPLICATION_ID:-}" ]]; then
  missing_env="PLATFORM_APPLICATION_ID"
fi
if [[ -z "${PLATFORM_BASE_URL:-}" ]]; then
  missing_env="${missing_env:+$missing_env, }PLATFORM_BASE_URL"
fi
if [[ -z "${PLATFORM_AUTHORIZATION_CATALOG_CLIENT_ID:-}" ]]; then
  missing_env="${missing_env:+$missing_env, }PLATFORM_AUTHORIZATION_CATALOG_CLIENT_ID"
fi
if [[ -z "${PLATFORM_AUTHORIZATION_CATALOG_CLIENT_SECRET:-}" ]]; then
  missing_env="${missing_env:+$missing_env, }PLATFORM_AUTHORIZATION_CATALOG_CLIENT_SECRET"
fi
if [[ -n "$missing_env" ]]; then
  echo "$prefix ERROR: required environment variables not set: $missing_env" >&2
  exit 2
fi

# manifest 不可用与内容损坏分开报错（均退出 2）：前者是挂载/路径问题，后者是清单本身的问题。
if [[ ! -f "$manifest" || ! -r "$manifest" ]]; then
  echo "$prefix ERROR: manifest unavailable: $manifest (missing, unreadable, or not a regular file)" >&2
  exit 2
fi
jq empty "$manifest" >/dev/null 2>&1 || {
  echo "$prefix ERROR: manifest is not valid JSON: $manifest" >&2
  exit 2
}

config="$(mktemp)"
put_body_file=""
cleanup() {
  rm -f -- "$config"
  if [[ -n "$put_body_file" ]]; then
    rm -f -- "$put_body_file"
  fi
  return 0
}
trap cleanup EXIT
chmod 0600 "$config"
escape() { printf '%s' "$1" | sed 's/\\/\\\\/g; s/"/\\"/g'; }
printf 'silent\nshow-error\nfail-with-body\nuser = "%s:%s"\n' "$(escape "$PLATFORM_AUTHORIZATION_CATALOG_CLIENT_ID")" "$(escape "$PLATFORM_AUTHORIZATION_CATALOG_CLIENT_SECRET")" >"$config"

token_json="$(curl --config "$config" --data-urlencode grant_type=client_credentials --data-urlencode scope=authorization.catalog.sync -H 'Content-Type: application/x-www-form-urlencoded' --max-time 15 "${PLATFORM_BASE_URL%/}/oauth2/token")" || {
  echo "$prefix ERROR: catalog publisher token request failed" >&2
  exit 3
}
token="$(printf '%s' "$token_json" | jq -er '.access_token // empty')" || {
  echo "$prefix ERROR: token response did not include access_token" >&2
  exit 3
}
printf 'silent\nshow-error\nfail-with-body\nheader = "Authorization: Bearer %s"\n' "$(escape "$token")" >"$config"

# PUT 响应先落到 0600 临时文件（不写仓库目录、不直接回显凭据），
# 失败时只截取前 500 字符到 stderr，便于看到服务端错误码与响应体。
put_body_file="$(mktemp)"
chmod 0600 "$put_body_file"
put_exit=0
put_status="$(curl --config "$config" -H 'Content-Type: application/json' --data-binary "@$manifest" --max-time 20 -X PUT --output "$put_body_file" --write-out '%{http_code}' "${PLATFORM_BASE_URL%/}/api/v1/applications/${PLATFORM_APPLICATION_ID}/authorization-catalog")" || put_exit=$?
put_detail="$(head -c 500 -- "$put_body_file" 2>/dev/null || true)"
put_detail="${put_detail//$'\r'/ }"
put_detail="${put_detail//$'\n'/ }"
put_ok=1
if [[ "$put_exit" -ne 0 ]]; then
  put_ok=0
fi
case "$put_status" in
  2[0-9][0-9]) ;;
  *) put_ok=0 ;;
esac
if [[ "$put_ok" -ne 1 ]]; then
  echo "$prefix ERROR: PUT failed (curl_exit=$put_exit, http_status=${put_status:-000})${put_detail:+: $put_detail}" >&2
  exit 4
fi

if ! jq -e '(.data.sync_status // .sync_status) == "SYNCED"' "$put_body_file" >/dev/null 2>&1; then
  echo "$prefix ERROR: platform did not confirm catalog synchronization${put_detail:+: $put_detail}" >&2
  exit 1
fi
echo "$prefix OK: catalog published"
