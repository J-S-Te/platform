#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
deploy_dir="$(cd -- "$script_dir/.." && pwd)"
runtime_file="$deploy_dir/.env"
release_file="$deploy_dir/.release.env"
backup_root="$deploy_dir/backups/system"
stamp="$(date -u +%Y%m%dT%H%M%SZ)"
batch="$backup_root/$stamp"
mode=create
source_backup=""
drill_root=""
encryption_key_file="${DISASTER_BACKUP_ENCRYPTION_KEY_FILE:-}"
inherited_deploy_lock_fd=""

usage() {
  cat >&2 <<EOF
usage: $0 [--encryption-key-file FILE] [--deployment-lock-fd N]
       $0 --verify-only --backup BATCH [--encryption-key-file FILE]
       $0 --rebuild-drill --backup BATCH --drill-root EMPTY_DIRECTORY [--encryption-key-file FILE]
EOF
  exit 64
}

while (($#)); do
  case "$1" in
    --verify-only) [[ "$mode" == create ]] || usage; mode=verify; shift ;;
    --rebuild-drill) [[ "$mode" == create ]] || usage; mode=drill; shift ;;
    --backup) (($# >= 2)) || usage; source_backup="$2"; shift 2 ;;
    --drill-root) (($# >= 2)) || usage; drill_root="$2"; shift 2 ;;
    --encryption-key-file) (($# >= 2)) || usage; encryption_key_file="$2"; shift 2 ;;
    --deployment-lock-fd) (($# >= 2)) || usage; inherited_deploy_lock_fd="$2"; shift 2 ;;
    *) usage ;;
  esac
done
if [[ "$mode" == create ]]; then
  [[ -z "$source_backup" && -z "$drill_root" ]] || usage
else
  [[ -n "$source_backup" ]] || usage
  [[ "$mode" != drill || -n "$drill_root" ]] || usage
fi

for command_name in gzip sha256sum flock install mktemp tar awk find grep head cp chmod stat realpath; do
  command -v "$command_name" >/dev/null || { echo "missing command: $command_name" >&2; exit 127; }
done

env_value_from() {
  local file="$1" key="$2"
  awk -F= -v key="$key" '$0 !~ /^[[:space:]]*#/ && $1 == key { sub(/^[^=]*=/, ""); print; exit }' "$file"
}

validate_encryption_key() {
  [[ -n "$encryption_key_file" ]] || return 0
  [[ -f "$encryption_key_file" && ! -L "$encryption_key_file" && -s "$encryption_key_file" ]] || {
    echo "encryption key file is missing, empty, or unsafe" >&2
    return 1
  }
  key_mode="$(stat -c '%a' "$encryption_key_file" 2>/dev/null || stat -f '%Lp' "$encryption_key_file")"
  case "$key_mode" in
    400|600) ;;
    *) echo "encryption key file mode must be 0400 or 0600" >&2; return 1 ;;
  esac
  command -v openssl >/dev/null || { echo "openssl is required for encrypted disaster backups" >&2; return 1; }
}

validate_sha_manifest() {
  local root="$1"
  [[ -f "$root/SHA256SUMS" && ! -L "$root/SHA256SUMS" ]] || { echo "SHA256SUMS is missing or unsafe" >&2; return 1; }
  awk '
    NF < 2 { exit 1 }
    {
      name=substr($0, 67)
      sub(/^\*/, "", name)
      if (name == "" || name ~ /^\// || name ~ /(^|\/)\.\.($|\/)/ || name ~ /\\/) exit 1
    }
  ' "$root/SHA256SUMS" || { echo "SHA256SUMS contains an unsafe path" >&2; return 1; }
  (cd -- "$root" && sha256sum --check SHA256SUMS)
}

safe_extract_tar() {
  local archive="$1" destination="$2"
  if tar --list --gzip --file "$archive" | awk '/(^\/|(^|\/)\.\.($|\/))/{bad=1} END{exit bad?0:1}'; then
    echo "recovery archive contains an absolute or traversal path" >&2
    return 1
  fi
  tar --extract --gzip --file "$archive" --directory "$destination" --no-same-owner --no-same-permissions
  if find "$destination" -xdev \( -type l -o \( ! -type d ! -type f \) \) -print -quit | grep -q .; then
    echo "recovery archive contains a symbolic link or special file" >&2
    return 1
  fi
}

prepare_recovery_archive() {
  local root="$1" destination="$2" encrypted archive temporary
  encrypted="$(env_value_from "$root/MANIFEST" RECOVERY_ARCHIVE_ENCRYPTED)"
  archive="$(env_value_from "$root/MANIFEST" RECOVERY_ARCHIVE)"
  [[ -n "$archive" && "$archive" != */* && -f "$root/$archive" && ! -L "$root/$archive" ]] || {
    echo "recovery archive declared by MANIFEST is missing or unsafe" >&2
    return 1
  }
  if [[ "$encrypted" == true ]]; then
    [[ -n "$encryption_key_file" ]] || { echo "encrypted backup requires --encryption-key-file" >&2; return 1; }
    validate_encryption_key
    temporary="$(mktemp "${TMPDIR:-/tmp}/recovery-state.XXXXXX.tar.gz")"
    if ! openssl enc -d -aes-256-cbc -pbkdf2 -in "$root/$archive" -out "$temporary" -pass "file:$encryption_key_file"; then
      rm -f -- "$temporary"
      return 1
    fi
    if ! safe_extract_tar "$temporary" "$destination"; then
      rm -f -- "$temporary"
      return 1
    fi
    rm -f -- "$temporary"
  elif [[ "$encrypted" == false ]]; then
    safe_extract_tar "$root/$archive" "$destination"
  else
    echo "MANIFEST has an invalid RECOVERY_ARCHIVE_ENCRYPTED value" >&2
    return 1
  fi
}

verify_disaster_batch() {
  local root="$1" verify_stage database_count actual_database_count https_enabled file_gateway_relative
  if find "$root" -type l -print -quit | grep -q .; then
    echo "disaster recovery batch contains a symbolic link" >&2
    return 1
  fi
  for required in MANIFEST VERSION_MANIFEST SHA256SUMS RECOVERY_INSTRUCTIONS.txt; do
    [[ -f "$root/$required" && ! -L "$root/$required" ]] || { echo "required disaster recovery file is missing or unsafe: $required" >&2; return 1; }
  done
  [[ "$(env_value_from "$root/MANIFEST" FORMAT)" == 2 ]] || { echo "unsupported disaster backup format" >&2; return 1; }
  validate_sha_manifest "$root"
  database_count="$(env_value_from "$root/MANIFEST" DATABASE_COUNT)"
  [[ "$database_count" =~ ^[0-9]+$ ]] || { echo "invalid database count in MANIFEST" >&2; return 1; }
  actual_database_count=0
  while IFS= read -r sql_backup; do
    [[ -f "$sql_backup" && ! -L "$sql_backup" && -s "$sql_backup" ]] || { echo "unsafe or empty SQL backup" >&2; return 1; }
    gzip -t "$sql_backup"
    actual_database_count=$((actual_database_count + 1))
  done < <(find "$root" -type f -name '*.sql.gz' -print | sort)
  [[ "$actual_database_count" == "$database_count" ]] || {
    echo "database backup count mismatch: actual=$actual_database_count expected=$database_count" >&2
    return 1
  }
  verify_stage="$(mktemp -d "${TMPDIR:-/tmp}/system-recovery-verify.XXXXXX")"
  if ! prepare_recovery_archive "$root" "$verify_stage"; then
    rm -rf -- "$verify_stage"
    return 1
  fi
  for required in deploy/.env deploy/.release.env deploy/docker-compose.yml deploy/runtime deploy/subsystems.d deploy/subsystem-templates deploy/mysql-init deploy/bin platform-keys; do
    [[ -e "$verify_stage/recovery-state/$required" && ! -L "$verify_stage/recovery-state/$required" ]] || {
      echo "recovery state is incomplete: $required" >&2
      rm -rf -- "$verify_stage"
      return 1
    }
  done
  [[ -s "$verify_stage/recovery-state/platform-keys/jwt-ed25519-private.pem" && -s "$verify_stage/recovery-state/platform-keys/jwt-ed25519-public.pem" ]] || {
    echo "platform signing key pair is missing from recovery state" >&2
    rm -rf -- "$verify_stage"
    return 1
  }
  https_enabled="$(env_value_from "$verify_stage/recovery-state/deploy/.env" PUBLIC_HTTPS_ENABLED)"
  if [[ "$https_enabled" == true ]]; then
    for tls_file in platform.crt platform.key sso.crt sso.key; do
      [[ -s "$verify_stage/recovery-state/deploy/runtime/public-tls/$tls_file" ]] || {
        echo "HTTPS recovery material is incomplete: runtime/public-tls/$tls_file" >&2
        rm -rf -- "$verify_stage"
        return 1
      }
    done
  fi
  file_gateway_relative="$(env_value_from "$root/MANIFEST" FILE_GATEWAY_BATCH)"
  [[ "$file_gateway_relative" =~ ^file-gateway/[0-9]{8}T[0-9]{6}Z$ ]] || {
    echo "MANIFEST has an unsafe File Gateway batch path" >&2
    rm -rf -- "$verify_stage"
    return 1
  }
  "$script_dir/restore-file-gateway.sh" --backup "$root/$file_gateway_relative" --verify-only
  rm -rf -- "$verify_stage"
}

if [[ "$mode" != create ]]; then
  validate_encryption_key
  [[ ! -L "$source_backup" ]] || { echo "backup batch must not be a symbolic link" >&2; exit 1; }
  source_backup="$(cd -- "$source_backup" 2>/dev/null && pwd)" || { echo "backup batch does not exist" >&2; exit 1; }
  verify_disaster_batch "$source_backup"
  if [[ "$mode" == verify ]]; then
    echo "disaster recovery batch verified: $source_backup"
    exit 0
  fi
  [[ "$drill_root" == /* && "$drill_root" != / ]] || { echo "--drill-root must be a safe absolute path" >&2; exit 1; }
  if [[ -e "$drill_root" ]]; then
    [[ -d "$drill_root" && ! -L "$drill_root" && -z "$(find "$drill_root" -mindepth 1 -print -quit)" ]] || {
      echo "drill root must be an empty, non-symbolic-link directory" >&2
      exit 1
    }
  else
    install -d -m 700 "$drill_root"
  fi
  prepare_recovery_archive "$source_backup" "$drill_root"
  install -d -m 700 "$drill_root/database-backups"
  while IFS= read -r sql_backup; do
    relative="${sql_backup#"$source_backup"/}"
    safe_name="${relative//\//__}"
    install -m 600 "$sql_backup" "$drill_root/database-backups/$safe_name"
  done < <(find "$source_backup" -type f -name '*.sql.gz' -print | sort)
  install -m 600 "$source_backup/VERSION_MANIFEST" "$drill_root/VERSION_MANIFEST"
  install -m 600 "$source_backup/RECOVERY_INSTRUCTIONS.txt" "$drill_root/RECOVERY_INSTRUCTIONS.txt"
  echo "isolated rebuild drill staged and verified: $drill_root"
  echo "No online container, database, volume, or deployment directory was modified."
  exit 0
fi

command -v docker >/dev/null || { echo "missing command: docker" >&2; exit 127; }
validate_encryption_key
[[ -f "$runtime_file" && ! -L "$runtime_file" && -f "$release_file" && ! -L "$release_file" ]] || {
  echo "run deploy.sh configure first; configuration files must be regular files" >&2
  exit 1
}
install -d -m 700 "$backup_root" "$deploy_dir/runtime"
deploy_lock_file="$deploy_dir/runtime/.deploy.lock"
[[ ! -L "$deploy_lock_file" ]] || { echo "deployment lock must not be a symbolic link" >&2; exit 1; }
deploy_lock_fd=8
if [[ -n "$inherited_deploy_lock_fd" ]]; then
  # destroy 等已持锁入口通过 --deployment-lock-fd 传入描述符；对同一文件再次
  # flock 会与父进程持有的排他锁冲突并卡到超时，因此这里必须复用而不是重取。
  [[ "$inherited_deploy_lock_fd" =~ ^[0-9]+$ && -e "/proc/$$/fd/$inherited_deploy_lock_fd" ]] || {
    echo "inherited deployment lock descriptor is invalid" >&2; exit 1; }
  [[ "$(realpath "/proc/$$/fd/$inherited_deploy_lock_fd")" == "$(realpath "$deploy_lock_file")" ]] || {
    echo "inherited deployment lock points at the wrong file" >&2; exit 1; }
  flock -n "$inherited_deploy_lock_fd" || { echo "inherited deployment lock is not held by this task" >&2; exit 1; }
  deploy_lock_fd="$inherited_deploy_lock_fd"
else
  exec 8>"$deploy_lock_file"
  flock -w 900 8 || { echo "timed out waiting for the unified deployment lock" >&2; exit 1; }
fi
[[ ! -L "$backup_root/.backup.lock" ]] || { echo "system backup lock must not be a symbolic link" >&2; exit 1; }
exec 9>"$backup_root/.backup.lock"
flock -n 9 || { echo "another system backup is running" >&2; exit 1; }
[[ ! -e "$batch" ]] || { echo "backup batch already exists: $batch" >&2; exit 1; }
work="$(mktemp -d "$backup_root/.${stamp}.XXXXXX")"
cleanup_work() {
  if [[ -n "${work:-}" && -d "$work" && "$work" == "$backup_root/.${stamp}."* ]]; then
    rm -rf -- "$work"
  fi
}
trap cleanup_work EXIT

compose=(docker compose --project-directory "$deploy_dir" --file "$deploy_dir/docker-compose.yml" --env-file "$runtime_file" --env-file "$release_file")
services=(platform-mysql keycloak-db customer-mysql portal-mysql contract-mysql project-mysql settlement-mysql data-analysis-mysql)
database_volume_for_service() {
  case "$1" in
    platform-mysql) printf platform-mysql-data ;;
    keycloak-db) printf keycloak-mysql-data ;;
    customer-mysql) printf customer-mysql-data ;;
    portal-mysql) printf portal-mysql-data ;;
    contract-mysql) printf contract-mysql-data ;;
    project-mysql) printf project-mysql-data ;;
    settlement-mysql) printf settlement-mysql-data ;;
    data-analysis-mysql) printf data-analysis-mysql-data ;;
  esac
}
compose_project="$(env_value_from "$runtime_file" COMPOSE_PROJECT_NAME)"
compose_project="${compose_project:-basic-platform-production}"
backed_up=0
for service in "${services[@]}"; do
  container="$("${compose[@]}" ps -a -q "$service" 2>/dev/null || true)"
  if [[ "$container" == *$'\n'* ]]; then
    echo "multiple database containers found for $service; refuse an ambiguous disaster backup" >&2
    exit 1
  fi
  if [[ -z "$container" ]]; then
    database_volume="$(database_volume_for_service "$service")"
    retained_volume="$(docker volume ls -q \
      --filter "label=com.docker.compose.project=$compose_project" \
      --filter "label=com.docker.compose.volume=$database_volume" | head -1)"
    [[ -z "$retained_volume" ]] || {
      echo "database volume $database_volume exists without a restorable $service container; re-enable/start the database before creating a complete disaster backup" >&2
      exit 1
    }
    continue
  fi
  [[ "$(docker inspect -f '{{.State.Running}}' "$container")" == true ]] || {
    echo "database container is stopped: $service; start it before creating a complete disaster backup" >&2
    exit 1
  }
  temporary="$work/.${service}.sql.gz"
  # Security (SEC-F5b): the root password reaches mysqldump only via the
  # in-container MYSQL_PWD environment variable, never as a -p argument, so the
  # password does not appear in argv (/proc/*/cmdline, visible to ps).
  if ! "${compose[@]}" exec -T "$service" sh -ec \
    'exec env MYSQL_PWD="$MYSQL_ROOT_PASSWORD" mysqldump --single-transaction --routines --triggers --events --all-databases -uroot' |
      gzip -9 >"$temporary"; then
    echo "database backup failed: $service" >&2
    exit 1
  fi
  gzip -t "$temporary"
  [[ -s "$temporary" ]] || { echo "empty database backup: $service" >&2; exit 1; }
  mv "$temporary" "$work/${service}.sql.gz"
  backed_up=$((backed_up + 1))
done
((backed_up > 0)) || { echo "no running MySQL service was found" >&2; exit 1; }

# File Gateway DB and objects are captured only by the dedicated write-free
# snapshot, never by an unrelated online database dump.
FILE_GATEWAY_BACKUP_ROOT="$work/file-gateway" "$script_dir/backup-file-gateway.sh" \
  --stamp "$stamp" --deployment-lock-fd "$deploy_lock_fd"
rm -f -- "$work/file-gateway/.backup.lock"
file_gateway_batch="file-gateway/$stamp"
[[ -d "$work/$file_gateway_batch" ]] || { echo "file gateway recovery batch was not created" >&2; exit 1; }

state="$work/.recovery-state"
install -d -m 700 "$state/recovery-state/deploy" "$state/recovery-state/platform-keys"
for source_file in .env .release.env docker-compose.yml; do
  [[ -f "$deploy_dir/$source_file" && ! -L "$deploy_dir/$source_file" ]] || { echo "recovery source is missing or unsafe: $source_file" >&2; exit 1; }
  install -m 600 "$deploy_dir/$source_file" "$state/recovery-state/deploy/$source_file"
done
for source_dir in runtime subsystems.d subsystem-templates mysql-init bin; do
  [[ -d "$deploy_dir/$source_dir" && ! -L "$deploy_dir/$source_dir" ]] || { echo "recovery source directory is missing or unsafe: $source_dir" >&2; exit 1; }
  if find "$deploy_dir/$source_dir" -xdev \( -type l -o \( ! -type d ! -type f \) \) -print -quit | grep -q .; then
    echo "recovery source contains a symbolic link or special file: $source_dir" >&2
    exit 1
  fi
  install -d -m 700 "$state/recovery-state/deploy/$source_dir"
  cp -a -- "$deploy_dir/$source_dir/." "$state/recovery-state/deploy/$source_dir/"
done
keys_dir="$(env_value_from "$runtime_file" PLATFORM_KEYS_DIR)"; keys_dir="${keys_dir:-./data/platform/keys}"
[[ "$keys_dir" == /* ]] || keys_dir="$deploy_dir/${keys_dir#./}"
keys_dir="$(realpath -e "$keys_dir")"
[[ "$keys_dir" != / && -d "$keys_dir" && ! -L "$keys_dir" ]] || { echo "platform key directory is missing or unsafe" >&2; exit 1; }
if find "$keys_dir" -xdev \( -type l -o \( ! -type d ! -type f \) \) -print -quit | grep -q .; then
  echo "platform key directory contains a symbolic link or special file" >&2
  exit 1
fi
[[ -s "$keys_dir/jwt-ed25519-private.pem" && -s "$keys_dir/jwt-ed25519-public.pem" ]] || {
  echo "platform signing key pair is incomplete" >&2
  exit 1
}
cp -a -- "$keys_dir/." "$state/recovery-state/platform-keys/"
find "$state" -type d -exec chmod 0700 {} +
find "$state" -type f -exec chmod 0600 {} +

recovery_archive=recovery-state.tar.gz
tar --create --gzip --file "$work/$recovery_archive" --directory "$state" recovery-state
rm -rf -- "$state"
encrypted=false
if [[ -n "$encryption_key_file" ]]; then
  openssl enc -aes-256-cbc -pbkdf2 -salt -in "$work/$recovery_archive" \
    -out "$work/$recovery_archive.enc" -pass "file:$encryption_key_file"
  rm -f -- "$work/$recovery_archive"
  recovery_archive="$recovery_archive.enc"
  encrypted=true
fi

awk -F= '
  $0 !~ /^[[:space:]]*#/ && $1 ~ /(^RELEASE_VERSION$|_IMAGE$|_IMAGE_DIGEST$|_VERSION$)/ {
    print
  }
' "$release_file" | LC_ALL=C sort >"$work/VERSION_MANIFEST"
docker_engine_version="$(docker version --format '{{.Server.Version}}' 2>/dev/null || printf unknown)"
cat >"$work/MANIFEST" <<EOF
FORMAT=2
CREATED_AT=$stamp
DATABASE_COUNT=$((backed_up + 1))
FILE_GATEWAY_BATCH=$file_gateway_batch
RECOVERY_ARCHIVE=$recovery_archive
RECOVERY_ARCHIVE_ENCRYPTED=$encrypted
RECOVERY_STATE_POLICY=$([[ "$encrypted" == true ]] && printf encrypted-aes-256-cbc-pbkdf2 || printf restricted-mode-0600)
DOCKER_ENGINE_VERSION=$docker_engine_version
EOF
cat >"$work/RECOVERY_INSTRUCTIONS.txt" <<'EOF'
This batch contains production secrets, signing keys, TLS private keys and database data.
Keep the batch directory mode 0700 and every regular file mode 0600 on encrypted storage.
If RECOVERY_ARCHIVE_ENCRYPTED=true, store the encryption key separately from this batch.
Run bin/backup-all.sh --verify-only before every restore.
Use --rebuild-drill with an empty isolated directory to materialize and inspect recovery state.
Database and File Gateway restores remain separate approved maintenance actions; never restore into a live production writer topology.
EOF
find "$work" -type d -exec chmod 0700 {} +
find "$work" -type f -exec chmod 0600 {} +
(cd -- "$work" && while IFS= read -r recovery_file; do sha256sum "$recovery_file"; done < <(find . -type f ! -name SHA256SUMS -print | LC_ALL=C sort) >SHA256SUMS)
chmod 600 "$work/SHA256SUMS"

verify_disaster_batch "$work"
mv "$work" "$batch"
work=""
chmod 700 "$batch"
trap - EXIT
printf 'verified disaster recovery backup: %s\n' "$batch"
if [[ "$encrypted" == false ]]; then
  echo 'WARNING: recovery state is protected by 0700/0600 permissions but is not encrypted; copy it only to access-controlled encrypted storage.' >&2
fi
