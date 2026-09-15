#!/usr/bin/env bash
# Publish the application-owned project-management authorization catalog.
set -euo pipefail
: "${PLATFORM_APPLICATION_ID:?missing application id}"
: "${PLATFORM_BASE_URL:?missing platform url}"
: "${PLATFORM_AUTHORIZATION_CATALOG_CLIENT_ID:?missing publisher client id}"
: "${PLATFORM_AUTHORIZATION_CATALOG_CLIENT_SECRET:?missing publisher secret}"
manifest=/catalog/permission-manifest.json
[[ -r "$manifest" ]] || { echo "[project-catalog-sync] manifest unavailable" >&2; exit 2; }
config="$(mktemp)"; trap 'rm -f -- "$config"' EXIT; chmod 0600 "$config"
escape() { printf '%s' "$1" | sed 's/\\/\\\\/g; s/"/\\"/g'; }
printf 'silent\nshow-error\nfail-with-body\nuser = "%s:%s"\n' "$(escape "$PLATFORM_AUTHORIZATION_CATALOG_CLIENT_ID")" "$(escape "$PLATFORM_AUTHORIZATION_CATALOG_CLIENT_SECRET")" >"$config"
token="$(curl --config "$config" --data-urlencode grant_type=client_credentials --data-urlencode scope=authorization.catalog.sync -H 'Content-Type: application/x-www-form-urlencoded' --max-time 15 "${PLATFORM_BASE_URL%/}/oauth2/token" | jq -er '.access_token')"
printf 'silent\nshow-error\nfail-with-body\nheader = "Authorization: Bearer %s"\n' "$(escape "$token")" >"$config"
curl --config "$config" -H 'Content-Type: application/json' --data-binary "@$manifest" --max-time 20 -X PUT "${PLATFORM_BASE_URL%/}/api/v1/applications/${PLATFORM_APPLICATION_ID}/authorization-catalog" | jq -e '(.data.sync_status // .sync_status) == "SYNCED"' >/dev/null
echo '[project-catalog-sync] OK: catalog published'
