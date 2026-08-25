#!/usr/bin/env bash
# Publishes the trusted, application-owned Settlement authorization manifest.
# It runs only inside the deployment Agent's one-shot container with a
# catalog-publisher credential supplied through the process environment.
set -euo pipefail

prefix="[settlement-catalog-sync]"
app_id="${PLATFORM_APPLICATION_ID:-}"
issuer="${PLATFORM_BASE_URL:-}"
client_id="${PLATFORM_AUTHORIZATION_CATALOG_CLIENT_ID:-}"
client_secret="${PLATFORM_AUTHORIZATION_CATALOG_CLIENT_SECRET:-}"
manifest="/catalog/permission-manifest.json"

if [[ -z "$app_id" || -z "$issuer" || -z "$client_id" || -z "$client_secret" || ! -r "$manifest" ]]; then
  echo "$prefix ERROR: application context, publisher credentials, or manifest is unavailable" >&2
  exit 2
fi

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
