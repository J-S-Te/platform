#!/usr/bin/env bash
# Controlled Keycloak MySQL logical restore for an isolated drill or an
# explicitly approved recovery. It never stops/starts containers itself: the
# operator must first freeze writes and stop every Keycloak node.
set -Eeuo pipefail
umask 077

script_dir="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
deploy_dir="$(CDPATH= cd -- "$script_dir/.." && pwd)"
cd "$deploy_dir"

backup_dir="${KEYCLOAK_BACKUP_DIR:-$deploy_dir/backups/keycloak}"
backup_file=""
verify_only=false
confirmation=""

usage() {
  cat <<'EOF'
Usage:
  restore-keycloak-mysql.sh --backup <keycloak-*.sql.gz> --verify-only
  restore-keycloak-mysql.sh --backup <keycloak-*.sql.gz> --confirm RESTORE_KEYCLOAK_DATABASE

The verify-only mode checks archive integrity, optional SHA-256 sidecar,
Compose configuration, and database readiness without changing data.

The restore mode DROPS and recreates the Keycloak database. Before using it:
  1. Freeze authentication changes and stop every Keycloak node.
  2. Select a backup and its matching encrypted .env/runtime/ingress material.
  3. Validate the procedure in an isolated recovery drill first.

The script intentionally does not start Keycloak after import. Start the same
approved image digest only after the import succeeds, then perform the
post-restore checks printed by this script.
EOF
}

while (($# > 0)); do
  case "$1" in
    --backup)
      (($# >= 2)) || { usage >&2; exit 2; }
      backup_file="$2"
      shift 2
      ;;
    --verify-only)
      verify_only=true
      shift
      ;;
    --confirm)
      (($# >= 2)) || { usage >&2; exit 2; }
      confirmation="$2"
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      printf 'unknown argument: %s\n' "$1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

[[ -n "$backup_file" ]] || { usage >&2; exit 2; }
if ! $verify_only && [[ "$confirmation" != "RESTORE_KEYCLOAK_DATABASE" ]]; then
  printf '%s\n' 'restore requires: --confirm RESTORE_KEYCLOAK_DATABASE' >&2
  exit 2
fi
if $verify_only && [[ -n "$confirmation" ]]; then
  printf '%s\n' '--verify-only cannot be combined with --confirm' >&2
  exit 2
fi

command -v docker >/dev/null || { printf '%s\n' 'docker is required' >&2; exit 127; }
command -v gzip >/dev/null || { printf '%s\n' 'gzip is required' >&2; exit 127; }
command -v realpath >/dev/null || { printf '%s\n' 'realpath is required' >&2; exit 127; }

[[ -d "$backup_dir" && ! -L "$backup_dir" ]] || { printf '%s\n' 'backup directory must exist and not be a symbolic link' >&2; exit 2; }
[[ ! -L "$backup_file" ]] || { printf '%s\n' 'backup file must not be a symbolic link' >&2; exit 2; }
backup_root="$(realpath "$backup_dir")"
candidate="$(realpath -e "$backup_file")" || { printf '%s\n' 'backup file does not exist' >&2; exit 2; }
case "$candidate" in
  "$backup_root"/keycloak-*.sql.gz) ;;
  *)
    printf '%s\n' 'backup must be a keycloak-*.sql.gz file directly inside KEYCLOAK_BACKUP_DIR' >&2
    exit 2
    ;;
esac

compose_args=(docker compose)
[[ -f .env ]] && compose_args+=(--env-file .env)
[[ -f .release.env ]] && compose_args+=(--env-file .release.env)
compose_args+=(-f compose.yaml)

"${compose_args[@]}" config -q
gzip -t "$candidate"

checksum_file="$candidate.sha256"
if [[ -f "$checksum_file" ]]; then
  if command -v sha256sum >/dev/null; then
    (cd "$(dirname -- "$candidate")" && sha256sum -c "$(basename -- "$checksum_file")")
  else
    expected="$(awk '{print $1}' "$checksum_file")"
    actual="$(shasum -a 256 "$candidate" | awk '{print $1}')"
    [[ "$expected" == "$actual" ]] || { printf '%s\n' 'SHA-256 verification failed' >&2; exit 1; }
  fi
else
  printf '%s\n' 'warning: no .sha256 sidecar; gzip integrity was verified but source authenticity was not' >&2
fi

"${compose_args[@]}" exec -T keycloak-db sh -ec '
  test -n "${MYSQL_DATABASE:-}" && test -n "${MYSQL_ROOT_PASSWORD:-}"
  case "$MYSQL_DATABASE:$MYSQL_ROOT_PASSWORD" in
    *REPLACE_WITH_*|*PENDING_*)
      printf "%s\\n" "Keycloak database credentials still contain a deployment placeholder" >&2
      exit 2
      ;;
  esac
  mysqladmin ping -h 127.0.0.1 -uroot -p"$MYSQL_ROOT_PASSWORD" --silent
'

if $verify_only; then
  printf 'Backup verified without data changes: %s\n' "$candidate"
  exit 0
fi

keycloak_state="$("${compose_args[@]}" ps --status running --services keycloak 2>/dev/null || true)"
if [[ -n "$keycloak_state" ]]; then
  printf '%s\n' 'refusing restore while keycloak is running; freeze writes and stop all Keycloak nodes first' >&2
  exit 2
fi

# The database and root password are read only inside the container. The
# destructive SQL is fixed; neither the backup filename nor host input is
# interpolated into the SQL command.
gzip -dc -- "$candidate" | "${compose_args[@]}" exec -T keycloak-db sh -ec '
  set -eu
  case "$MYSQL_DATABASE" in
    ""|*[!A-Za-z0-9_]*)
      printf "%s\\n" "Keycloak database name contains unsupported characters" >&2
      exit 2
      ;;
  esac
  export MYSQL_PWD="$MYSQL_ROOT_PASSWORD"
  mysql -uroot -e "DROP DATABASE IF EXISTS \`$MYSQL_DATABASE\`; CREATE DATABASE \`$MYSQL_DATABASE\`;"
  exec mysql -uroot "$MYSQL_DATABASE"
'

"${compose_args[@]}" exec -T keycloak-db sh -ec '
  export MYSQL_PWD="$MYSQL_ROOT_PASSWORD"
  mysql -N -uroot "$MYSQL_DATABASE" -e "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE();" \
    | awk "{ if (\$1 < 1) exit 1 }"
'

printf '%s\n' "Keycloak database restore completed: $candidate"
printf '%s\n' 'Next: restore the matching protected configuration/Secrets, start the approved Keycloak image, then verify health, issuer discovery, Realm, Client, JWKS and a test login. Do not treat import success as a completed recovery.'
