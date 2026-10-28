#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
deploy_dir="$(cd -- "$script_dir/.." && pwd)"
runtime_file="$deploy_dir/.env"
release_file="$deploy_dir/.release.env"
backup_root="$deploy_dir/backups/system"
stamp="$(date -u +%Y%m%dT%H%M%SZ)"
batch="$backup_root/${stamp}-$$"
mode=create
source_backup=""
drill_root=""
encryption_key_file="${DISASTER_BACKUP_ENCRYPTION_KEY_FILE:-}"
inherited_deploy_lock_fd=""
if_existing=false

usage() {
  cat >&2 <<EOF
usage: $0 [--encryption-key-file FILE] [--deployment-lock-fd N] [--if-existing]
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
    --if-existing) if_existing=true; shift ;;
    *) usage ;;
  esac
done
if [[ "$mode" == create ]]; then
  [[ -z "$source_backup" && -z "$drill_root" ]] || usage
else
  [[ "$if_existing" == false ]] || usage
  [[ -n "$source_backup" ]] || usage
  [[ "$mode" != drill || -n "$drill_root" ]] || usage
fi

for command_name in gzip sha256sum flock install mktemp tar awk find grep head cp chmod stat realpath wc python3; do
  command -v "$command_name" >/dev/null || { echo "missing command: $command_name" >&2; exit 127; }
done

# 刻意保持独立：升级前备份可能发生在新发布器安装之前，绝不能因缺少此可选工具而省略批准文件。
runtime_approval_backup() {
  python3 - "$@" <<'PY'
import hashlib, json, os, pathlib, stat, sys
def checked(path, directory=False):
    if not path.is_absolute() or path == pathlib.Path('/') or path != path.resolve():
        raise ValueError('unsafe approval backup path')
    info=path.lstat()
    if (not stat.S_ISDIR(info.st_mode) if directory else not stat.S_ISREG(info.st_mode)) or info.st_mode & 0o022:
        raise ValueError('unsafe approval backup type/permissions')
    if not directory and (info.st_nlink != 1 or info.st_size > 65536):
        raise ValueError('unbounded/linked approval backup file')
    return path
def read(path):
    checked(path)
    with path.open('rb') as stream:
        raw=stream.read(65537)
    if len(raw)>65536: raise ValueError('approval backup too large')
    return raw
names={'license-evidence.json','license-migration-evidence.json'} | {
    'runtime-license-'+app+'-prod.json' for app in ('contract_management','customer_and_opportunity',
    'customer_portal','project_management','settlement','data_analysis')}
def snapshot(root):
    checked(root, True)
    authority=root/'runtime-approvals'
    present=authority.exists() or authority.is_symlink()
    selected=[]
    if present:
        checked(authority, True)
        if {p.name for p in authority.iterdir()}-names: raise ValueError('unknown approval backup file')
        selected.extend(authority.iterdir())
    selected.extend(root/n for n in names if (root/n).exists() or (root/n).is_symlink())
    raw={str(p.relative_to(root)):read(p) for p in selected}
    return present,raw
def metadata(present, raw):
    return {'version':1,'authority_present':present,
            'files':{name:hashlib.sha256(value).hexdigest() for name,value in raw.items()}}
def unique(items):
    value={}
    for key,item in items:
        if key in value: raise ValueError('duplicate approval backup metadata')
        value[key]=item
    return value
try:
    action,root=sys.argv[1],pathlib.Path(sys.argv[2])
    if action=='capture':
        dest=checked(pathlib.Path(sys.argv[3]),True)
        present,raw=snapshot(root)
        if (dest/'runtime-approvals').exists() or (dest/'runtime-approvals').is_symlink():
            raise ValueError('approval backup refuses overwrite')
        if present: (dest/'runtime-approvals').mkdir(mode=0o700)
        for name,value in {**raw,'runtime-approval-backup.json':(json.dumps(metadata(present,raw),sort_keys=True)+'\n').encode()}.items():
            with (dest/name).open('xb') as stream: stream.write(value)
            (dest/name).chmod(0o600)
    elif action=='verify':
        present,raw=snapshot(root)
        value=json.loads(read(root/'runtime-approval-backup.json'),object_pairs_hook=unique)
        if value!=metadata(present,raw): raise ValueError('approval backup proof mismatch')
    else: raise ValueError('invalid approval backup action')
except (OSError,ValueError,TypeError,KeyError) as error:
    print('runtime approval backup failed: '+str(error),file=sys.stderr)
    sys.exit(1)
PY
}

env_value_from() {
  local file="$1" key="$2"
  awk -F= -v key="$key" '$0 !~ /^[[:space:]]*#/ && $1 == key { sub(/^[^=]*=/, ""); print; exit }' "$file"
}

extract_binlog_position() {
  local dump="$1" line source_file source_pos
  line="$(gzip -dc -- "$dump" 2>/dev/null | head -n 40 | grep -m1 '^-- CHANGE REPLICATION SOURCE TO ' || true)"
  [[ -n "$line" ]] || return 1
  source_file="$(printf '%s\n' "$line" | grep -o "SOURCE_LOG_FILE='[^']*'" | cut -d"'" -f2)"
  source_pos="$(printf '%s\n' "$line" | grep -o 'SOURCE_LOG_POS=[0-9][0-9]*' | cut -d= -f2)"
  [[ -n "$source_file" && -n "$source_pos" ]] || return 1
  printf '%s\t%s\n' "$source_file" "$source_pos"
}

write_binlog_position() {
  local log_bin="$1" dump="$2" destination="$3" position source_file source_pos
  printf 'FORMAT=1\nCAPTURED_AT=%s\nLOG_BIN=%s\n' \
    "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$log_bin" >"$destination"
  if [[ "$log_bin" == 1 ]]; then
    position="$(extract_binlog_position "$dump")" || {
      rm -f -- "$destination"
      return 1
    }
    source_file="${position%%$'\t'*}"
    source_pos="${position#*$'\t'}"
    printf 'SOURCE_LOG_FILE=%s\nSOURCE_LOG_POS=%s\n' "$source_file" "$source_pos" >>"$destination"
  fi
  chmod 0600 "$destination"
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
  local root="$1" verify_stage database_count actual_database_count binlog_positions https_enabled file_gateway_relative
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

  binlog_positions="$(env_value_from "$root/MANIFEST" BINLOG_POSITIONS)"
  if [[ "$binlog_positions" == required ]]; then
    while IFS= read -r sql_backup; do
      case "$sql_backup" in
        "$root"/*/*) continue ;;
      esac
      [[ -s "$sql_backup.binlog-position" && ! -L "$sql_backup.binlog-position" ]] || {
        echo "missing binlog position sidecar for database backup: $sql_backup" >&2
        return 1
      }
    done < <(find "$root" -type f -name '*.sql.gz' -print | sort)
  fi
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
  # 旧格式 2 批次早于批准采集标志；新批次必须证明授权目录是否存在，并覆盖每个固定旧版影子文件。
  local approvals_captured
  approvals_captured="$(env_value_from "$root/MANIFEST" RUNTIME_APPROVALS_CAPTURED)"
  if [[ -n "$approvals_captured" ]]; then
    [[ "$approvals_captured" == true ]] || {
      echo 'runtime approval recovery flag invalid' >&2
      rm -rf -- "$verify_stage"
      return 1
    }
    if ! runtime_approval_backup verify "$verify_stage/recovery-state/deploy"; then
      rm -rf -- "$verify_stage"
      return 1
    fi
  fi
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
  local portal_archive
  portal_archive="$(env_value_from "$root/MANIFEST" PORTAL_REPORTS_ARCHIVE)"
  # 报告存储引入前创建的旧格式 2 批次仍允许此值为空。
  if [[ -n "$portal_archive" && "$portal_archive" != none ]]; then
    [[ "$portal_archive" == portal-reports.tar.gz && -s "$root/$portal_archive" && ! -L "$root/$portal_archive" ]] || {
      echo 'Portal report archive is missing or unsafe' >&2
      rm -rf -- "$verify_stage"
      return 1
    }
    install -d -m 700 "$verify_stage/portal-reports"
    if ! safe_extract_tar "$root/$portal_archive" "$verify_stage/portal-reports"; then
      rm -rf -- "$verify_stage"
      return 1
    fi
  fi
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
  if [[ "$(env_value_from "$source_backup/MANIFEST" PORTAL_REPORTS_ARCHIVE)" == portal-reports.tar.gz ]]; then
    install -d -m 700 "$drill_root/portal-reports"
    safe_extract_tar "$source_backup/portal-reports.tar.gz" "$drill_root/portal-reports"
    find "$drill_root/portal-reports" -type d -exec chmod 0700 {} +
    find "$drill_root/portal-reports" -type f -exec chmod 0600 {} +
  fi
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
# 分配独立文件描述符：发布调用方可能已持有 FD 9，复用会静默替换继承的部署锁描述符。
exec {backup_lock_fd}>"$backup_root/.backup.lock"
flock -n "$backup_lock_fd" || { echo "another system backup is running" >&2; exit 1; }
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
configured_services="$("${compose[@]}" config --services)" || { echo 'cannot validate Compose for database backup' >&2; exit 1; }
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
# 全新首装时 compose 可能一次性创建项目全部命名卷。只有确认卷从未写入任何数据
# （local driver、真实存在的非符号链接空目录）才允许跳过；其余情况一律按“已有数据”处理。
volume_has_no_data() {
  local volume="$1" metadata driver mountpoint entry
  [[ -n "$volume" && "$volume" != *$'\n'* ]] || return 1
  metadata="$(docker volume inspect --format '{{.Driver}}|{{.Mountpoint}}' "$volume" 2>/dev/null)" || return 1
  driver="${metadata%%|*}"
  mountpoint="${metadata#*|}"
  [[ "$driver" == local ]] || return 1
  [[ "$mountpoint" == /* && -d "$mountpoint" && ! -L "$mountpoint" ]] || return 1
  entry="$(find "$mountpoint" -mindepth 1 -maxdepth 1 -print -quit 2>/dev/null)" || return 1
  [[ -z "$entry" ]]
}
compose_project="$(env_value_from "$runtime_file" COMPOSE_PROJECT_NAME)"
compose_project="${compose_project:-basic-platform-production}"
backed_up=0
for service in "${services[@]}"; do
  container=""
  if grep -Fxq "$service" <<< "$configured_services"; then
    container="$("${compose[@]}" ps -a -q "$service")" || { echo "cannot inspect database service: $service" >&2; exit 1; }
  fi
  if [[ "$container" == *$'\n'* ]]; then
    echo "multiple database containers found for $service; refuse an ambiguous disaster backup" >&2
    exit 1
  fi
  if [[ -z "$container" ]]; then
    database_volume="$(database_volume_for_service "$service")"
    retained_volume="$(docker volume ls -q \
      --filter "label=com.docker.compose.project=$compose_project" \
      --filter "label=com.docker.compose.volume=$database_volume")" || { echo "cannot inspect retained database volumes: $service" >&2; exit 1; }
    if [[ -n "$retained_volume" ]] && volume_has_no_data "$retained_volume"; then
      echo "fresh installation: database volume $database_volume has never stored data and has no $service container; skipping $service backup"
      continue
    fi
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
  log_bin="$("${compose[@]}" exec -T "$service" sh -ec \
    'exec env MYSQL_PWD="$MYSQL_ROOT_PASSWORD" mysql -uroot -Nse "SELECT @@log_bin"')" || {
    echo "cannot determine binlog state: $service" >&2
    exit 1
  }
  log_bin="${log_bin//[[:space:]]/}"
  case "$log_bin" in
    0|1) ;;
    *) echo "unexpected binlog state '$log_bin': $service" >&2; exit 1 ;;
  esac

  dump_args=(--single-transaction --routines --triggers --events --all-databases)
  if [[ "$log_bin" == 1 ]]; then
    dump_args=(--source-data=2 "${dump_args[@]}")
  fi

  temporary="$work/.${service}.sql.gz"
  # 安全约束 SEC-F5b：root 密码仅通过容器内 MYSQL_PWD 环境变量传给 mysqldump，绝不用 -p 参数，
  # 避免出现在 argv、/proc/*/cmdline 或 ps 输出中。
  if ! "${compose[@]}" exec -T "$service" sh -ec \
    'exec env MYSQL_PWD="$MYSQL_ROOT_PASSWORD" mysqldump "$@" -uroot' _ "${dump_args[@]}" |
      gzip -9 >"$temporary"; then
    echo "database backup failed: $service" >&2
    exit 1
  fi
  gzip -t "$temporary"
  sql_bytes="$(gzip -dc "$temporary" | wc -c)"
  ((sql_bytes > 0)) || { echo "empty SQL database backup: $service" >&2; exit 1; }
  if ! write_binlog_position "$log_bin" "$temporary" "$work/${service}.sql.gz.binlog-position"; then
    echo "binlog position is missing from the database backup: $service" >&2
    exit 1
  fi
  mv "$temporary" "$work/${service}.sql.gz"
  backed_up=$((backed_up + 1))
done
if ((backed_up == 0)) && [[ "$if_existing" == true ]]; then
  gateway_container=""
  if grep -Fxq file-gateway-mysql <<< "$configured_services"; then
    gateway_container="$("${compose[@]}" ps -a -q file-gateway-mysql)" || { echo 'cannot inspect gateway database' >&2; exit 1; }
  fi
  gateway_volume="$(docker volume ls -q --filter "label=com.docker.compose.project=$compose_project" \
    --filter 'label=com.docker.compose.volume=file-gateway-mysql-data')" || { echo 'cannot inspect retained gateway volume' >&2; exit 1; }
  if [[ -n "$gateway_container" ]] || { [[ -n "$gateway_volume" ]] && ! volume_has_no_data "$gateway_volume"; }; then
    echo 'gateway data exists without the core databases; cannot skip backup' >&2
    exit 1
  fi
  if [[ -n "$gateway_volume" ]]; then
    echo 'fresh installation: File Gateway volume file-gateway-mysql-data has never stored data and has no file-gateway-mysql container; skipping gateway database backup'
  fi
  echo 'Fresh installation: no database containers or database data; no existing database to back up.'
  exit 0
fi
((backed_up > 0)) || { echo "no running MySQL service was found" >&2; exit 1; }

# File Gateway 数据库和对象仅由专用的无写入快照采集，绝不使用无关在线数据库转储。
FILE_GATEWAY_BACKUP_ROOT="$work/file-gateway" "$script_dir/backup-file-gateway.sh" \
  --stamp "$stamp" --deployment-lock-fd "$deploy_lock_fd"
rm -f -- "$work/file-gateway/.backup.lock"
file_gateway_batch="file-gateway/$stamp"
[[ -d "$work/$file_gateway_batch" ]] || { echo "file gateway recovery batch was not created" >&2; exit 1; }

# 若保留了加密门户报告库，即使当前 Compose 已禁用门户，也必须将其纳入备份，绝不能静默丢失报告。
source "$script_dir/portal-report-backup.sh"
backup_portal_reports "$compose_project" "$release_file" "$work"

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
runtime_approval_backup capture "$deploy_dir" "$state/recovery-state/deploy"
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
BINLOG_POSITIONS=required
FILE_GATEWAY_BATCH=$file_gateway_batch
PORTAL_REPORTS_ARCHIVE=$([[ -f "$work/portal-reports.tar.gz" ]] && printf portal-reports.tar.gz || printf none)
PORTAL_REPORTS_CONSISTENCY=immutable-objects-after-database-snapshot
RECOVERY_ARCHIVE=$recovery_archive
RECOVERY_ARCHIVE_ENCRYPTED=$encrypted
RECOVERY_STATE_POLICY=$([[ "$encrypted" == true ]] && printf encrypted-aes-256-cbc-pbkdf2 || printf restricted-mode-0600)
RUNTIME_APPROVALS_CAPTURED=true
DOCKER_ENGINE_VERSION=$docker_engine_version
EOF
cat >"$work/RECOVERY_INSTRUCTIONS.txt" <<'EOF'
This batch contains production secrets, signing keys, TLS private keys and database data.
Keep the batch directory mode 0700 and every regular file mode 0600 on encrypted storage.
If RECOVERY_ARCHIVE_ENCRYPTED=true, store the encryption key separately from this batch.
Run bin/backup-all.sh --verify-only before every restore.
Database backups may have .binlog-position sidecars; use them only with binlog files that were not purged.
Use --rebuild-drill with an empty isolated directory to materialize and inspect recovery state.
Database and File Gateway restores remain separate approved maintenance actions; never restore into a live production writer topology.
If PORTAL_REPORTS_ARCHIVE=portal-reports.tar.gz, restore that encrypted vault with the same batch's Portal database and runtime report keys. Rebuild drill stages it under portal-reports/. Stop Portal API and report worker before restoring; never merge a different batch or rotate its content/descriptor keys.
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
