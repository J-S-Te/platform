#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
deploy_dir="$(cd -- "$script_dir/.." && pwd)"
runtime_file="$deploy_dir/.env"
release_file="$deploy_dir/.release.env"
service=""
backup=""
confirm=""
temporal_maintenance_confirm=""
verify_only=false

usage() {
  cat >&2 <<EOF
usage: $0 --service SERVICE --backup BACKUP.sql.gz --verify-only
       $0 --service SERVICE --backup BACKUP.sql.gz --confirm RESTORE_MYSQL_SERVICE
       $0 --service contract-mysql --backup BACKUP.sql.gz --confirm RESTORE_MYSQL_SERVICE \\
          --confirm-temporal-maintenance RESTORE_SHARED_TEMPORAL_DATABASE
EOF
  exit 2
}

while (($#)); do
  case "$1" in
    --service) service="${2:-}"; shift 2 ;;
    --backup) backup="${2:-}"; shift 2 ;;
    --verify-only) verify_only=true; shift ;;
    --confirm) confirm="${2:-}"; shift 2 ;;
    --confirm-temporal-maintenance) temporal_maintenance_confirm="${2:-}"; shift 2 ;;
    *) usage ;;
  esac
done

case "$service" in
  platform-mysql|keycloak-db|file-gateway-mysql|customer-mysql|portal-mysql|contract-mysql|project-mysql|settlement-mysql|data-analysis-mysql) ;;
  *) usage ;;
esac
[[ -n "$backup" && -f "$backup" && ! -L "$backup" ]] || usage
backup="$(realpath -e "$backup")"
case "$backup" in
  "$deploy_dir"/backups/system/*/"$service".sql.gz) ;;
  *) echo "backup path does not match service" >&2; exit 2 ;;
esac

batch_dir="$(dirname -- "$backup")"
[[ -f "$batch_dir/SHA256SUMS" && ! -L "$batch_dir/SHA256SUMS" ]] || {
  echo "backup checksum manifest is missing or unsafe" >&2
  exit 1
}
(cd -- "$batch_dir" && sha256sum --check SHA256SUMS)
gzip -t "$backup"
[[ -s "$backup" ]] || { echo "backup is empty" >&2; exit 1; }
if [[ "$verify_only" == true ]]; then
  printf 'backup verified: %s\n' "$backup"
  exit 0
fi

if [[ "$service" == file-gateway-mysql ]]; then
  echo "file-gateway-mysql cannot be restored independently; use restore-file-gateway.sh with the matching database and object archive" >&2
  exit 2
fi

[[ "$confirm" == RESTORE_MYSQL_SERVICE ]] || {
  echo "restore requires --confirm RESTORE_MYSQL_SERVICE" >&2
  exit 2
}
if [[ "$service" == contract-mysql && "$temporal_maintenance_confirm" != RESTORE_SHARED_TEMPORAL_DATABASE ]]; then
  echo "contract-mysql also stores Temporal databases; restore requires --confirm-temporal-maintenance RESTORE_SHARED_TEMPORAL_DATABASE" >&2
  exit 2
fi
[[ -f "$runtime_file" && ! -L "$runtime_file" && -f "$release_file" && ! -L "$release_file" ]] || {
  echo "deployment configuration is missing or unsafe" >&2
  exit 1
}

for command_name in docker flock install realpath gzip sha256sum awk; do
  command -v "$command_name" >/dev/null || { echo "missing command: $command_name" >&2; exit 127; }
done

# Hold the same lock used by deploy/prepare/continue for the complete writer
# discovery and import window. This closes the check-then-restart race where a
# concurrent release could bring a writer back after the preflight check.
install -d -m 700 "$deploy_dir/runtime"
[[ ! -L "$deploy_dir/runtime/.deploy.lock" ]] || { echo "deployment lock must not be a symbolic link" >&2; exit 1; }
exec 8>"$deploy_dir/runtime/.deploy.lock"
flock -w 900 8 || { echo "timed out waiting for the unified deployment lock" >&2; exit 1; }

compose=(docker compose --project-directory "$deploy_dir" --file "$deploy_dir/docker-compose.yml" --env-file "$runtime_file" --env-file "$release_file")
container="$("${compose[@]}" ps -q "$service" 2>/dev/null || true)"
[[ -n "$container" && "$(docker inspect -f '{{.State.Running}}' "$container")" == true ]] || {
  echo "$service must be running" >&2
  exit 1
}
project="$(docker inspect -f '{{index .Config.Labels "com.docker.compose.project"}}' "$container" 2>/dev/null || true)"
[[ -n "$project" && "$project" != '<no value>' ]] || { echo "cannot determine Compose project for $service" >&2; exit 1; }

known_writer_for_database() {
  local database="$1" candidate="$2"
  case "$database:$candidate" in
    platform-mysql:platform-api|platform-mysql:platform-worker|platform-mysql:subsystem-provisioner|platform-mysql:platform-migrate|platform-mysql:bootstrap-admin) return 0 ;;
    keycloak-db:keycloak) return 0 ;;
    file-gateway-mysql:file-gateway) return 0 ;;
    customer-mysql:customer-api|customer-mysql:customer-migrate|customer-mysql:customer-opportunity-alert-worker|customer-mysql:customer-owner-notification-worker|customer-mysql:customer-presale-alert-worker|customer-mysql:customer-presale-assignment-notification-worker|customer-mysql:customer-presale-progress-notification-worker|customer-mysql:customer-notification-delivery-worker|customer-mysql:customer-presale-worker) return 0 ;;
    portal-mysql:portal-api|portal-mysql:portal-migrate|portal-mysql:portal-invite-compensation-worker) return 0 ;;
    contract-mysql:contract-api|contract-mysql:contract-migrate|contract-mysql:temporal) return 0 ;;
    project-mysql:project-api|project-mysql:project-migrate|project-mysql:project-sla-notifier) return 0 ;;
    settlement-mysql:settlement-api|settlement-mysql:settlement-migrate|settlement-mysql:settlement-worker|settlement-mysql:settlement-catalog-sync) return 0 ;;
    data-analysis-mysql:data-analysis-api|data-analysis-mysql:data-analysis-migrate|data-analysis-mysql:data-analysis-aggregation-worker|data-analysis-mysql:data-analysis-alert-worker|data-analysis-mysql:data-analysis-metabase|data-analysis-mysql:data-analysis-metabase-init) return 0 ;;
  esac
  return 1
}

container_references_database() {
  local candidate_id="$1" database="$2"
  # Inspect only inside this process and never print environment values: they
  # can contain DSNs and client secrets. This also finds orphaned/pruned
  # Compose services that no longer exist in the current YAML.
  docker inspect -f '{{range .Config.Env}}{{println .}}{{end}}' "$candidate_id" 2>/dev/null |
    awk -v database="$database" 'index($0, database) { found=1 } END { exit(found ? 0 : 1) }'
}

running_container_ids="$(docker ps -q)" || { echo "cannot enumerate running Docker containers" >&2; exit 1; }
writers=()
while IFS= read -r candidate_id; do
  [[ -n "$candidate_id" && "$candidate_id" != "$container" ]] || continue
  candidate_project="$(docker inspect -f '{{index .Config.Labels "com.docker.compose.project"}}' "$candidate_id" 2>/dev/null || true)"
  candidate_service="$(docker inspect -f '{{index .Config.Labels "com.docker.compose.service"}}' "$candidate_id" 2>/dev/null || true)"
  is_writer=false
  if [[ "$candidate_project" == "$project" ]] && known_writer_for_database "$service" "$candidate_service"; then
    is_writer=true
  elif container_references_database "$candidate_id" "$service"; then
    is_writer=true
  fi
  if [[ "$is_writer" == true ]]; then
    candidate_name="$(docker inspect -f '{{.Name}}' "$candidate_id" 2>/dev/null || printf '%s' "$candidate_id")"
    writers+=("${candidate_name#/} [service=${candidate_service:-unknown}]")
  fi
done <<<"$running_container_ids"

if ((${#writers[@]} > 0)); then
  printf 'refusing restore while database writers are running for %s:\n' "$service" >&2
  printf '  - %s\n' "${writers[@]}" >&2
  echo "Stop every listed writer, then rerun the restore. Containers removed from the current YAML are intentionally included." >&2
  exit 1
fi

# Security (SEC-F5b): the root password reaches the mysql client only via the
# in-container MYSQL_PWD environment variable, never as a -p argument, so the
# password does not appear in argv (/proc/*/cmdline, visible to ps).
gzip -dc "$backup" | "${compose[@]}" exec -T "$service" sh -ec 'exec env MYSQL_PWD="$MYSQL_ROOT_PASSWORD" mysql -uroot'
printf 'database restore completed: service=%s backup=%s\n' "$service" "$backup"
echo 'Keep the maintenance window active, start approved dependents, and run deploy.sh verify before declaring recovery complete.'
