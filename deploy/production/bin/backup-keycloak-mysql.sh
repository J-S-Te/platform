#!/usr/bin/env bash
# Online logical backup for the production Keycloak MySQL database.
# It deliberately never stops containers, deletes a backup by default, or reads
# secrets into the host shell. Run from cron/systemd with the deploy user's access
# to Docker.
set -Eeuo pipefail
umask 077

script_dir="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
deploy_dir="$(CDPATH= cd -- "$script_dir/.." && pwd)"
cd "$deploy_dir"

backup_dir="${KEYCLOAK_BACKUP_DIR:-$deploy_dir/backups/keycloak}"
metrics_file="${KEYCLOAK_BACKUP_PROM_FILE:-$deploy_dir/monitoring/textfile/keycloak_backup.prom}"
retention_days="${KEYCLOAK_BACKUP_RETENTION_DAYS:-0}"
lock_file="${KEYCLOAK_BACKUP_LOCK_FILE:-$backup_dir/.backup.lock}"

if ! [[ "$retention_days" =~ ^[0-9]+$ ]]; then
  printf '%s\n' 'KEYCLOAK_BACKUP_RETENTION_DAYS must be a non-negative integer' >&2
  exit 2
fi
command -v docker >/dev/null || { printf '%s\n' 'docker is required' >&2; exit 127; }
command -v flock >/dev/null || { printf '%s\n' 'flock is required' >&2; exit 127; }
command -v gzip >/dev/null || { printf '%s\n' 'gzip is required' >&2; exit 127; }
mkdir -p "$backup_dir" "$(dirname -- "$metrics_file")"

exec 9>"$lock_file"
if ! flock -n 9; then
  printf '%s\n' 'another Keycloak backup is already running; leaving existing backups untouched' >&2
  exit 0
fi

compose_args=(docker compose)
[[ -f .env ]] && compose_args+=(--env-file .env)
[[ -f .release.env ]] && compose_args+=(--env-file .release.env)
compose_args+=(-f compose.yaml)

timestamp="$(date -u +%Y%m%dT%H%M%SZ)"
final_file="$backup_dir/keycloak-$timestamp.sql.gz"
temporary_file="$final_file.partial"
cleanup() { rm -f -- "$temporary_file"; }
trap cleanup EXIT

# MYSQL_ROOT_PASSWORD and MYSQL_DATABASE are resolved inside the database
# container. This avoids exposing a database password in cron, process lists,
# or a host-side environment variable.
"${compose_args[@]}" exec -T keycloak-db sh -ec '
  test -n "${MYSQL_DATABASE:-}" && test -n "${MYSQL_ROOT_PASSWORD:-}"
  exec env MYSQL_PWD="$MYSQL_ROOT_PASSWORD" mysqldump \
    --single-transaction --routines --events --triggers --add-drop-table \
    -uroot "$MYSQL_DATABASE"
' | gzip -c > "$temporary_file"

gzip -t "$temporary_file"
[[ -s "$temporary_file" ]] || { printf '%s\n' 'backup output is empty' >&2; exit 1; }
mv -- "$temporary_file" "$final_file"

if command -v sha256sum >/dev/null; then
  sha256sum "$final_file" > "$final_file.sha256"
else
  shasum -a 256 "$final_file" > "$final_file.sha256"
fi

# Retention is opt-in. A positive value only removes completed backup pairs
# older than that many whole days, never the current file or partial files.
if (( retention_days > 0 )); then
  find "$backup_dir" -maxdepth 1 -type f \( -name 'keycloak-*.sql.gz' -o -name 'keycloak-*.sql.gz.sha256' \) \
    -mtime "+$retention_days" -print -delete
fi

metric_tmp="$metrics_file.tmp.$$"
printf '# HELP keycloak_backup_last_success_unixtime Unix time of last verified Keycloak MySQL backup.\n# TYPE keycloak_backup_last_success_unixtime gauge\nkeycloak_backup_last_success_unixtime %s\n' "$(date -u +%s)" > "$metric_tmp"
mv -- "$metric_tmp" "$metrics_file"
trap - EXIT
printf 'Keycloak backup verified: %s\n' "$final_file"
