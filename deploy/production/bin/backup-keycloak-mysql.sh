#!/usr/bin/env bash
# 生产 Keycloak MySQL 数据库的在线逻辑备份。刻意不停止容器、不默认删除备份，也不将密钥读取到宿主机 Shell。
# 由具备 Docker 访问权限的部署用户通过 cron 或 systemd 运行。
set -Eeuo pipefail
umask 077

script_dir="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
deploy_dir="$(CDPATH= cd -- "$script_dir/.." && pwd)"
cd "$deploy_dir"

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

write_sha256() {
  local file="$1"
  if command -v sha256sum >/dev/null; then
    sha256sum "$file" > "$file.sha256"
  else
    shasum -a 256 "$file" > "$file.sha256"
  fi
  chmod 600 "$file.sha256"
}

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
if [[ -L "$backup_dir" || -L "$(dirname -- "$metrics_file")" ]]; then
  printf '%s\n' 'backup and metrics directories must not be symbolic links' >&2
  exit 2
fi
mkdir -p "$backup_dir" "$(dirname -- "$metrics_file")"
chmod 700 "$backup_dir"

exec 9>"$lock_file"
if ! flock -n 9; then
  printf '%s\n' 'another Keycloak backup is already running; leaving existing backups untouched' >&2
  exit 0
fi

compose_args=(docker compose)
[[ -f .env ]] && compose_args+=(--env-file .env)
[[ -f .release.env ]] && compose_args+=(--env-file .release.env)
compose_args+=(-f docker-compose.yml)

"${compose_args[@]}" config -q
"${compose_args[@]}" exec -T keycloak-db sh -ec '
  test -n "${MYSQL_DATABASE:-}" && test -n "${MYSQL_ROOT_PASSWORD:-}"
  case "$MYSQL_DATABASE:$MYSQL_ROOT_PASSWORD" in
    *REPLACE_WITH_*|*PENDING_*)
      printf "%s\\n" "Keycloak database credentials still contain a deployment placeholder" >&2
      exit 2
      ;;
  esac
  # 安全（SEC-F5c）：密码经容器内 MYSQL_PWD 传入 mysqladmin，不再进入进程 argv。
  export MYSQL_PWD="$MYSQL_ROOT_PASSWORD"
  mysqladmin ping -h 127.0.0.1 -uroot --silent
'

log_bin="$("${compose_args[@]}" exec -T keycloak-db sh -ec '
  test -n "${MYSQL_ROOT_PASSWORD:-}"
  exec env MYSQL_PWD="$MYSQL_ROOT_PASSWORD" mysql -uroot -Nse "SELECT @@log_bin"
')" || { printf '%s\n' 'cannot determine Keycloak MySQL binlog state' >&2; exit 1; }
log_bin="${log_bin//[[:space:]]/}"
case "$log_bin" in
  0|1) ;;
  *) printf 'unexpected Keycloak MySQL binlog state: %s\n' "$log_bin" >&2; exit 1 ;;
esac

timestamp="$(date -u +%Y%m%dT%H%M%SZ)"
final_file="$backup_dir/keycloak-$timestamp.sql.gz"
temporary_file="$final_file.partial"
position_file="$final_file.binlog-position"
cleanup() { rm -f -- "$temporary_file" "$position_file" "$position_file.sha256"; }
trap cleanup EXIT

dump_args=(--single-transaction --routines --events --triggers --add-drop-table)
if [[ "$log_bin" == 1 ]]; then
  dump_args=(--source-data=2 "${dump_args[@]}")
fi

# MYSQL_ROOT_PASSWORD 和 MYSQL_DATABASE 在数据库容器内解析，避免密码暴露于 cron、进程列表或宿主机环境变量。
"${compose_args[@]}" exec -T keycloak-db sh -ec '
  test -n "${MYSQL_DATABASE:-}" && test -n "${MYSQL_ROOT_PASSWORD:-}"
  exec env MYSQL_PWD="$MYSQL_ROOT_PASSWORD" mysqldump "$@" -uroot "$MYSQL_DATABASE"
' _ "${dump_args[@]}" | gzip -c > "$temporary_file"

gzip -t "$temporary_file"
[[ -s "$temporary_file" ]] || { printf '%s\n' 'backup output is empty' >&2; exit 1; }
write_binlog_position "$log_bin" "$temporary_file" "$position_file" || {
  printf '%s\n' 'binlog position is missing from the Keycloak dump' >&2
  exit 1
}
mv -- "$temporary_file" "$final_file"

write_sha256 "$final_file"
write_sha256 "$position_file"

# 保留期清理需显式启用；正值仅删除早于指定完整天数的已完成备份对，绝不删除当前或部分文件。
if (( retention_days > 0 )); then
  find "$backup_dir" -maxdepth 1 -type f \( \
    -name 'keycloak-*.sql.gz' -o -name 'keycloak-*.sql.gz.sha256' \
    -o -name 'keycloak-*.sql.gz.binlog-position' \
    -o -name 'keycloak-*.sql.gz.binlog-position.sha256' \
  \) -mtime "+$retention_days" -print -delete
fi

metric_tmp="$metrics_file.tmp.$$"
printf '# HELP keycloak_backup_last_success_unixtime Unix time of last verified Keycloak MySQL backup.\n# TYPE keycloak_backup_last_success_unixtime gauge\nkeycloak_backup_last_success_unixtime %s\n' "$(date -u +%s)" > "$metric_tmp"
mv -- "$metric_tmp" "$metrics_file"
trap - EXIT
printf 'Keycloak backup verified: %s\n' "$final_file"
