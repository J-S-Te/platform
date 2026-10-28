#!/usr/bin/env bash
set -Eeuo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
restore_mysql="$script_dir/restore-mysql.sh"
backup_gateway="$script_dir/backup-file-gateway.sh"
restore_gateway="$script_dir/restore-file-gateway.sh"
backup_all="$script_dir/backup-all.sh"
backup_keycloak="$script_dir/backup-keycloak-mysql.sh"

fail() { echo "FAIL: $*" >&2; exit 1; }
contains() { grep -Fq -- "$2" "$1" || fail "$3"; }

for script in "$restore_mysql" "$backup_gateway" "$restore_gateway" "$backup_all" "$backup_keycloak"; do
  bash -n "$script"
done

# backup-all 必须能复用调用方已持有的部署锁，否则 destroy 会对同一把锁二次 flock 而死锁。
contains "$backup_all" '--deployment-lock-fd' 'backup-all cannot inherit an already-held deployment lock'
contains "$backup_all" 'inherited deployment lock is not held by this task' 'backup-all does not verify an inherited deployment lock'
contains "$backup_all" '--deployment-lock-fd "$deploy_lock_fd"' 'backup-all does not forward the held lock to the gateway snapshot'

# 静态安全契约：这些断言刻意枚举维护边界，确保移除任一写入者或密钥类别都会导致 CI 失败。
contains "$restore_mysql" 'runtime/.deploy.lock' 'MySQL restore does not use unified deployment lock'
contains "$restore_mysql" 'docker ps -q' 'MySQL restore does not enumerate actual running containers'
for writer in platform-worker subsystem-provisioner temporal contract-worker project-sla-notifier customer-presale-worker customer-contract-transfer-worker portal-invite-compensation-worker settlement-worker settlement-catalog-sync data-analysis-aggregation-worker data-analysis-alert-worker data-analysis-metabase; do
  contains "$restore_mysql" "$writer" "MySQL restore writer map is missing $writer"
done
contains "$restore_mysql" 'RESTORE_SHARED_TEMPORAL_DATABASE' 'contract/Temporal shared database lacks independent confirmation'
contains "$restore_mysql" 'file-gateway-mysql cannot be restored independently' 'gateway database can still be restored without files'

contains "$backup_gateway" 'CONSISTENCY_MODE=writers-stopped' 'gateway backup does not declare a stopped-writer recovery point'
contains "$backup_gateway" "label=com.docker.compose.service=file-gateway" 'gateway backup does not discover pruned gateway containers'
stop_line="$(grep -n 'docker stop --timeout 60' "$backup_gateway" | head -1 | cut -d: -f1)"
dump_line="$(grep -n 'mysqldump --single-transaction' "$backup_gateway" | head -1 | cut -d: -f1)"
[[ -n "$stop_line" && -n "$dump_line" && "$stop_line" -lt "$dump_line" ]] || fail 'gateway dump is not inside the stopped-writer window'

contains "$restore_gateway" 'chown -R 10001:10001' 'gateway restore does not normalize UID/GID 10001'
contains "$restore_gateway" 'find "$stage" -xdev -type d -exec chmod 0750' 'gateway restore does not normalize directory permissions'
contains "$restore_gateway" 'find "$stage" -xdev -type f -exec chmod 0640' 'gateway restore does not normalize file permissions'
contains "$restore_gateway" '--verify-only' 'gateway restore lacks verify-only mode'
contains "$restore_gateway" 'RESTORE_FILE_GATEWAY' 'gateway restore lacks a fixed destructive confirmation'
restore_scan_line="$(grep -n 'running_container_ids=.*docker ps -q' "$restore_gateway" | head -1 | cut -d: -f1)"
restore_stop_line="$(grep -n 'docker stop --timeout 60' "$restore_gateway" | head -1 | cut -d: -f1)"
[[ -n "$restore_scan_line" && -n "$restore_stop_line" && "$restore_scan_line" -lt "$restore_stop_line" ]] ||
  fail 'gateway restore can stop managed containers before rejecting an unknown writer'
contains "$backup_gateway" 'docker gzip tar sha256sum flock install mktemp awk find grep' 'gateway backup prerequisite list omits grep'

for required_scope in '.env .release.env docker-compose.yml' 'runtime subsystems.d subsystem-templates mysql-init bin' 'platform-keys' 'runtime/public-tls' 'VERSION_MANIFEST' 'RECOVERY_ARCHIVE_ENCRYPTED' 'restricted-mode-0600' 'encrypted-aes-256-cbc-pbkdf2'; do
  contains "$backup_all" "$required_scope" "disaster archive scope/policy missing: $required_scope"
done
contains "$backup_all" 'database volume $database_volume exists without a restorable' 'system backup silently skips retained DB volumes without containers'
contains "$backup_all" 'database container is stopped:' 'system backup silently skips stopped databases'
contains "$backup_all" '--verify-only' 'system disaster backup lacks verify-only mode'
contains "$backup_all" '--rebuild-drill' 'system disaster backup lacks rebuild drill mode'

# binlog 位点：两个备份入口都必须请求快照一致的位点，并落盘 sidecar。
for backup_script in "$backup_all" "$backup_keycloak"; do
  contains "$backup_script" '--source-data=2' "$(basename -- "$backup_script") does not request binlog coordinates"
  contains "$backup_script" '--single-transaction' "$(basename -- "$backup_script") lost single-transaction"
  contains "$backup_script" 'SELECT @@log_bin' "$(basename -- "$backup_script") does not check binlog state"
  contains "$backup_script" 'binlog-position' "$(basename -- "$backup_script") does not persist binlog coordinates"
done
contains "$backup_all" 'BINLOG_POSITIONS=required' 'system backup does not require binlog position sidecars'
contains "$backup_keycloak" 'keycloak-*.sql.gz.binlog-position' 'Keycloak retention does not include position sidecars'

# 全新首装回归：compose 预建的空命名卷不得触发 fail-closed；有数据、非 local 或无法核验的卷仍必须拒绝。
(
  set -Eeuo pipefail
  fixture="$(mktemp -d "${TMPDIR:-/tmp}/fresh-volume-test.XXXXXX")"
  trap 'rm -rf -- "$fixture"' EXIT
  install -d "$fixture/deploy/bin" "$fixture/stub" "$fixture/empty-volume" "$fixture/data-volume"
  printf 'stale database page\n' >"$fixture/data-volume/stale.dat"
  # 宿主机 Bash 3 不支持动态文件描述符；仅在临时夹具中替换锁描述符分配，生产脚本保持不变。
  awk '
    /exec \{backup_lock_fd\}>/ {
      print "backup_lock_fd=11"
      sub(/exec \{backup_lock_fd\}>/, "exec 11>")
    }
    { print }
  ' "$backup_all" >"$fixture/deploy/bin/backup-all.sh"
  printf '%s\n' 'COMPOSE_PROJECT_NAME=fresh-volume-fixture' >"$fixture/deploy/.env"
  printf '%s\n' 'PLATFORM_IMAGE=fixture' >"$fixture/deploy/.release.env"
  printf '%s\n' 'services: {}' >"$fixture/deploy/docker-compose.yml"
  cat >"$fixture/stub/flock" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF
  cat >"$fixture/stub/docker" <<'EOF'
#!/usr/bin/env bash
set -eu
if [[ "$1" == compose ]]; then
  case "$*" in
    *' config --services')
      printf '%s\n' platform-mysql file-gateway-mysql
      ;;
    *' ps -a -q '*)
      service=''
      for argument in "$@"; do service="$argument"; done
      if [[ " ${EXISTING_CONTAINERS:-} " == *" $service "* ]]; then
        printf '%s\n' fixture-container
      fi
      ;;
    *) exit 90 ;;
  esac
elif [[ "$1" == volume && "${2:-}" == ls ]]; then
  [[ "${VOLUMES_FAILED:-false}" != true ]] || exit 9
  for argument in "$@"; do
    case "$argument" in
      label=com.docker.compose.volume=*)
        name="${argument#label=com.docker.compose.volume=}"
        if [[ " ${EXISTING_VOLUMES:-} " == *" $name "* ]]; then
          printf 'fixture_%s\n' "$name"
        fi
        ;;
    esac
  done
elif [[ "$1" == volume && "${2:-}" == inspect ]]; then
  [[ "${INSPECT_FAILED:-false}" != true ]] || exit 9
  printf '%s|%s\n' "${VOLUME_DRIVER:-local}" "${VOLUME_MOUNTPOINT:?}"
else
  exit 91
fi
EOF
  chmod 750 "$fixture/stub/flock" "$fixture/stub/docker"
  run_fresh_backup() {
    local output="$1"
    shift
    env PATH="$fixture/stub:$PATH" "$@" bash "$fixture/deploy/bin/backup-all.sh" --if-existing >"$output" 2>&1
  }

  # 1) 数据库卷与网关卷均为空且无容器：全新首装必须成功跳过。
  if ! run_fresh_backup "$fixture/empty.out" \
      EXISTING_VOLUMES='platform-mysql-data file-gateway-mysql-data' \
      VOLUME_MOUNTPOINT="$fixture/empty-volume"; then
    sed -n '1,120p' "$fixture/empty.out" >&2
    fail 'fresh install with only empty pre-created volumes was rejected'
  fi
  contains "$fixture/empty.out" 'has never stored data and has no platform-mysql container' \
    'fresh install skip did not explain the empty database volume'
  contains "$fixture/empty.out" 'has no file-gateway-mysql container' \
    'fresh install skip did not explain the empty gateway volume'
  if grep -Fq 'exists without a restorable' "$fixture/empty.out"; then
    fail 'fresh install with empty volumes produced the fail-closed database message'
  fi

  # 2) 有数据的数据库卷仍必须 fail-closed，并保留原文案。
  if run_fresh_backup "$fixture/data.out" \
      EXISTING_VOLUMES='platform-mysql-data' \
      VOLUME_MOUNTPOINT="$fixture/data-volume"; then
    fail 'backup accepted a data-bearing database volume without its container'
  fi
  contains "$fixture/data.out" 'database volume platform-mysql-data exists without a restorable platform-mysql container' \
    'data-bearing database volume rejection lost its fail-closed message'

  # 3) 非 local driver 即使目录为空也按已有数据处理。
  if run_fresh_backup "$fixture/foreign.out" \
      EXISTING_VOLUMES='platform-mysql-data' \
      VOLUME_MOUNTPOINT="$fixture/empty-volume" \
      VOLUME_DRIVER=nfs; then
    fail 'backup skipped a non-local database volume'
  fi
  contains "$fixture/foreign.out" 'database volume platform-mysql-data exists without a restorable' \
    'non-local volume did not fail closed with the database volume message'

  # 4) 无法核验空卷（docker volume inspect 失败）时必须按已有数据处理。
  if run_fresh_backup "$fixture/inspect-failed.out" \
      EXISTING_VOLUMES='platform-mysql-data' \
      VOLUME_MOUNTPOINT="$fixture/empty-volume" \
      INSPECT_FAILED=true; then
    fail 'backup skipped a database volume that could not be inspected'
  fi
  contains "$fixture/inspect-failed.out" 'database volume platform-mysql-data exists without a restorable' \
    'unverifiable volume did not fail closed'

  # 5) 只有 file-gateway 空卷也必须被识别为全新首装。
  if ! run_fresh_backup "$fixture/gateway-empty.out" \
      EXISTING_VOLUMES='file-gateway-mysql-data' \
      VOLUME_MOUNTPOINT="$fixture/empty-volume"; then
    sed -n '1,120p' "$fixture/gateway-empty.out" >&2
    fail 'fresh install with an empty gateway volume was rejected'
  fi
  contains "$fixture/gateway-empty.out" 'file-gateway-mysql-data has never stored data' \
    'fresh install skip did not explain the empty gateway volume'

  # 6) 网关卷有数据或网关容器存在时仍拒绝跳过备份。
  if run_fresh_backup "$fixture/gateway-data.out" \
      EXISTING_VOLUMES='file-gateway-mysql-data' \
      VOLUME_MOUNTPOINT="$fixture/data-volume"; then
    fail 'backup skipped a data-bearing gateway volume'
  fi
  contains "$fixture/gateway-data.out" 'gateway data exists without the core databases; cannot skip backup' \
    'gateway data rejection lost its fail-closed message'
  if run_fresh_backup "$fixture/gateway-container.out" \
      EXISTING_CONTAINERS='file-gateway-mysql'; then
    fail 'backup skipped backup even though the gateway container exists'
  fi
  contains "$fixture/gateway-container.out" 'gateway data exists without the core databases; cannot skip backup' \
    'existing gateway container rejection lost its fail-closed message'

  # 7) 没有任何卷和容器时保持原有全新首装早退语义。
  if ! run_fresh_backup "$fixture/fresh.out"; then
    sed -n '1,120p' "$fixture/fresh.out" >&2
    fail 'plain fresh install early exit regressed'
  fi
  contains "$fixture/fresh.out" 'Fresh installation: no database containers or database data' \
    'plain fresh install early exit message regressed'

  # 8) 直接验证空卷判定边界：local driver、绝对路径、非符号链接空目录缺一不可。
  volume_function="$(sed -n '/^volume_has_no_data() {$/,/^}$/p' "$backup_all")"
  [[ -n "$volume_function" ]] || fail 'backup-all is missing the volume_has_no_data helper'
  (
    eval "$volume_function"
    volume_driver=local
    volume_mountpoint="$fixture/empty-volume"
    inspect_failed=false
    docker() {
      if [[ "$1" == volume && "${2:-}" == inspect ]]; then
        [[ "$inspect_failed" != true ]] || return 9
        printf '%s|%s\n' "$volume_driver" "$volume_mountpoint"
      else
        return 91
      fi
    }
    volume_has_no_data fixture-volume || fail 'empty local volume was not detected as unused'
    volume_driver=nfs
    if volume_has_no_data fixture-volume; then fail 'non-local volume was treated as unused'; fi
    volume_driver=local
    volume_mountpoint="$fixture/data-volume"
    if volume_has_no_data fixture-volume; then fail 'data-bearing volume was treated as unused'; fi
    volume_mountpoint=relative/volume/path
    if volume_has_no_data fixture-volume; then fail 'relative volume mountpoint was treated as unused'; fi
    volume_mountpoint="$fixture/volume-link"
    ln -s "$fixture/empty-volume" "$fixture/volume-link"
    if volume_has_no_data fixture-volume; then fail 'symbolic-link volume mountpoint was treated as unused'; fi
    volume_mountpoint="$fixture/empty-volume"
    inspect_failed=true
    if volume_has_no_data fixture-volume; then fail 'unverifiable volume was treated as unused'; fi
    inspect_failed=false
    if volume_has_no_data $'one\ntwo'; then fail 'ambiguous multiple volume match was treated as unused'; fi
  )
)

# 从脚本正文取出解析函数执行，防止测试复制一份实现。
(
  set -Eeuo pipefail
  position_fixture="$(mktemp -d "${TMPDIR:-/tmp}/binlog-position-test.XXXXXX")"
  trap 'rm -rf -- "$position_fixture"' EXIT
  for source_script in "$backup_all" "$backup_keycloak"; do
    function_text="$(sed -n '/^extract_binlog_position() {$/,/^}$/p' "$source_script")"
    [[ -n "$function_text" ]] || { echo "FAIL: missing extract_binlog_position in $source_script" >&2; exit 1; }
    (
      eval "$function_text"
      {
        printf '%s\n' '-- MySQL dump fixture'
        printf '%s\n' "-- CHANGE REPLICATION SOURCE TO SOURCE_LOG_FILE='binlog.000069', SOURCE_LOG_POS=436217060;"
        printf '%s\n' 'CREATE TABLE t (id INT);'
      } | gzip -9 >"$position_fixture/dump.sql.gz"
      printf '%s\n' '-- no coordinates here' | gzip -9 >"$position_fixture/missing.sql.gz"
      result="$(extract_binlog_position "$position_fixture/dump.sql.gz")"
      [[ "$result" == $'binlog.000069\t436217060' ]] || { echo 'FAIL: parsed binlog position mismatch' >&2; exit 1; }
      if extract_binlog_position "$position_fixture/missing.sql.gz" >/dev/null; then
        echo 'FAIL: backup without coordinates was accepted' >&2
        exit 1
      fi
      write_function="$(sed -n '/^write_binlog_position() {$/,/^}$/p' "$source_script")"
      [[ -n "$write_function" ]] || { echo "FAIL: missing write_binlog_position in $source_script" >&2; exit 1; }
      eval "$write_function"
      write_binlog_position 0 "$position_fixture/missing.sql.gz" "$position_fixture/off.position" || {
        echo 'FAIL: binlog-off sidecar was not written' >&2
        exit 1
      }
      grep -Fq 'LOG_BIN=0' "$position_fixture/off.position" || { echo 'FAIL: binlog-off sidecar state mismatch' >&2; exit 1; }
      if grep -Fq 'SOURCE_LOG_FILE=' "$position_fixture/off.position"; then
        echo 'FAIL: binlog-off sidecar contains coordinates' >&2
        exit 1
      fi
      if write_binlog_position 1 "$position_fixture/missing.sql.gz" "$position_fixture/fail.position"; then
        echo 'FAIL: missing binlog coordinates were accepted' >&2
        exit 1
      fi
      [[ ! -e "$position_fixture/fail.position" ]] || { echo 'FAIL: failed sidecar was left behind' >&2; exit 1; }
    ) || { echo "FAIL: binlog position parser failed for $source_script" >&2; exit 1; }
  done
)

# Keycloak 端到端：假 docker 输出带位点注释的转储，验证 sidecar 与两份校验和。
(
  set -Eeuo pipefail
  keycloak_fixture="$(mktemp -d "${TMPDIR:-/tmp}/keycloak-backup-test.XXXXXX")"
  trap 'rm -rf -- "$keycloak_fixture"' EXIT
  install -d "$keycloak_fixture/deploy/bin" "$keycloak_fixture/stub"
  install -m 750 "$backup_keycloak" "$keycloak_fixture/deploy/bin/backup-keycloak-mysql.sh"
  : >"$keycloak_fixture/deploy/.env"
  : >"$keycloak_fixture/deploy/.release.env"
  printf '%s\n' 'services: {}' >"$keycloak_fixture/deploy/docker-compose.yml"

  cat >"$keycloak_fixture/stub/docker" <<'EOF'
#!/usr/bin/env bash
set -eu
[[ "$1" == compose ]] || exit 97
case "$*" in
  *' config -q'*) exit 0 ;;
  *'mysqladmin ping'*) exit 0 ;;
  *'SELECT @@log_bin'*) printf '1\n' ;;
  *mysqldump*)
    printf '%s\n' '-- MySQL dump fixture'
    printf '%s\n' "-- CHANGE REPLICATION SOURCE TO SOURCE_LOG_FILE='binlog.000069', SOURCE_LOG_POS=436217060;"
    printf '%s\n' 'CREATE TABLE t (id INT);'
    ;;
  *) exit 98 ;;
esac
EOF
  cat >"$keycloak_fixture/stub/flock" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF
  chmod 750 "$keycloak_fixture/stub/docker" "$keycloak_fixture/stub/flock"

  PATH="$keycloak_fixture/stub:$PATH" \
    KEYCLOAK_BACKUP_DIR="$keycloak_fixture/backups" \
    KEYCLOAK_BACKUP_PROM_FILE="$keycloak_fixture/keycloak_backup.prom" \
    "$keycloak_fixture/deploy/bin/backup-keycloak-mysql.sh" >"$keycloak_fixture/run.out" 2>&1 || {
      sed -n '1,80p' "$keycloak_fixture/run.out" >&2
      exit 1
    }

  sql_file="$(find "$keycloak_fixture/backups" -type f -name 'keycloak-*.sql.gz' -print -quit)"
  position_file="$sql_file.binlog-position"
  [[ -n "$sql_file" && -s "$position_file" ]] || { echo 'FAIL: Keycloak backup did not write a position sidecar' >&2; exit 1; }
  grep -Fq 'SOURCE_LOG_FILE=binlog.000069' "$position_file" || { echo 'FAIL: sidecar log file mismatch' >&2; exit 1; }
  grep -Fq 'SOURCE_LOG_POS=436217060' "$position_file" || { echo 'FAIL: sidecar position mismatch' >&2; exit 1; }
  [[ -s "$sql_file.sha256" && -s "$position_file.sha256" ]] || { echo 'FAIL: Keycloak checksums are incomplete' >&2; exit 1; }
)

# 动态回归：当前 YAML 中已不存在的 Temporal 容器仍须阻止 contract-mysql 恢复，Docker 替身绝不暴露密钥。
fixture="$(/bin/realpath "$(mktemp -d "${TMPDIR:-/tmp}/restore-writer-test.XXXXXX")")"
cleanup() { rm -rf -- "$fixture"; }
trap cleanup EXIT
install -d "$fixture/deploy/bin" "$fixture/deploy/backups/system/20260926T000000Z" "$fixture/stub"
install -m 750 "$restore_mysql" "$fixture/deploy/bin/restore-mysql.sh"
printf '%s\n' 'COMPOSE_PROJECT_NAME=basic-platform-production' >"$fixture/deploy/.env"
printf '%s\n' 'PLATFORM_IMAGE=local/platform@sha256:test' >"$fixture/deploy/.release.env"
printf '%s\n' 'services: {}' >"$fixture/deploy/docker-compose.yml"
printf '%s\n' 'contract backup' | gzip -c >"$fixture/deploy/backups/system/20260926T000000Z/contract-mysql.sql.gz"
(cd "$fixture/deploy/backups/system/20260926T000000Z" && sha256sum contract-mysql.sql.gz >SHA256SUMS)
cat >"$fixture/stub/flock" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF
cat >"$fixture/stub/realpath" <<'EOF'
#!/usr/bin/env bash
[[ "${1:-}" == -e ]] && shift
exec /bin/realpath "$@"
EOF
cat >"$fixture/stub/docker" <<'EOF'
#!/usr/bin/env bash
case "$1" in
  compose)
    for argument in "$@"; do
      if [[ "$argument" == ps ]]; then printf '%s\n' database-container; exit 0; fi
      if [[ "$argument" == exec ]]; then exit 99; fi
    done
    ;;
  ps) printf '%s\n' orphan-temporal-container ;;
  inspect)
    format="$3"
    object="$4"
    case "$format" in
      *State.Running*) printf '%s\n' true ;;
      *com.docker.compose.project*) printf '%s\n' basic-platform-production ;;
      *com.docker.compose.service*)
        [[ "$object" == orphan-temporal-container ]] && printf '%s\n' "${QA_RESTORE_WRITER:-temporal}" || printf '%s\n' contract-mysql
        ;;
      *Config.Env*) printf '%s\n' 'TEMPORAL_ADDRESS=temporal:7233' ;;
      *'.Name'*) printf '%s\n' /orphan-temporal ;;
    esac
    ;;
esac
EOF
chmod 750 "$fixture/stub/flock" "$fixture/stub/realpath" "$fixture/stub/docker"

if PATH="$fixture/stub:$PATH" "$fixture/deploy/bin/restore-mysql.sh" \
    --service contract-mysql \
    --backup "$fixture/deploy/backups/system/20260926T000000Z/contract-mysql.sql.gz" \
    --confirm RESTORE_MYSQL_SERVICE \
    --confirm-temporal-maintenance RESTORE_SHARED_TEMPORAL_DATABASE \
    >"$fixture/writer.out" 2>&1; then
  fail 'restore accepted an orphaned running Temporal writer'
fi
if ! grep -Fq 'orphan-temporal [service=temporal]' "$fixture/writer.out"; then
  sed -n '1,120p' "$fixture/writer.out" >&2
  fail 'restore did not identify orphaned Temporal writer'
fi
contains "$fixture/writer.out" 'Containers removed from the current YAML are intentionally included.' 'restore did not explain pruned-container gate'

# 即使 inspect 响应不含 DSN，固定的生产 worker 仍是已知写入者，必须在发送任何 SQL 前阻止恢复。
printf '%s\n' 'customer backup' | gzip -c >"$fixture/deploy/backups/system/20260926T000000Z/customer-mysql.sql.gz"
(cd "$fixture/deploy/backups/system/20260926T000000Z" && sha256sum contract-mysql.sql.gz customer-mysql.sql.gz >SHA256SUMS)
for pair in contract-mysql:contract-worker customer-mysql:customer-contract-transfer-worker; do
  database="${pair%%:*}"; writer="${pair#*:}"
  if QA_RESTORE_WRITER="$writer" PATH="$fixture/stub:$PATH" "$fixture/deploy/bin/restore-mysql.sh" \
      --service "$database" \
      --backup "$fixture/deploy/backups/system/20260926T000000Z/$database.sql.gz" \
      --confirm RESTORE_MYSQL_SERVICE \
      --confirm-temporal-maintenance RESTORE_SHARED_TEMPORAL_DATABASE \
      >"$fixture/$writer.out" 2>&1; then
    fail "restore accepted a running $writer"
  fi
  contains "$fixture/$writer.out" "[service=$writer]" "restore did not identify $writer"
done

if PATH="$fixture/stub:$PATH" "$fixture/deploy/bin/restore-mysql.sh" \
    --service contract-mysql \
    --backup "$fixture/deploy/backups/system/20260926T000000Z/contract-mysql.sql.gz" \
    --confirm RESTORE_MYSQL_SERVICE >"$fixture/temporal-confirm.out" 2>&1; then
  fail 'contract restore accepted missing Temporal maintenance confirmation'
fi
contains "$fixture/temporal-confirm.out" 'RESTORE_SHARED_TEMPORAL_DATABASE' 'missing Temporal confirmation did not fail closed'

# GNU/Linux 执行归档级动态测试；macOS 缺少生产 GNU tar、find、flock 语义，改由语法、静态检查及写入者测试覆盖。
if [[ "$(uname -s)" == Linux || "${UIP_FORCE_ARCHIVE_TESTS:-false}" == true ]]; then
  batch="$fixture/disaster-batch"
  state="$fixture/state/recovery-state"
  install -d "$batch/file-gateway/20260926T000000Z" \
    "$state/deploy/runtime" "$state/deploy/subsystems.d" "$state/deploy/subsystem-templates" \
    "$state/deploy/mysql-init" "$state/deploy/bin" "$state/platform-keys"
  printf '%s\n' 'PUBLIC_HTTPS_ENABLED=false' >"$state/deploy/.env"
  printf '%s\n' 'PLATFORM_IMAGE=local/platform@sha256:test' >"$state/deploy/.release.env"
  printf '%s\n' 'services: {}' >"$state/deploy/docker-compose.yml"
  printf '%s\n' private >"$state/platform-keys/jwt-ed25519-private.pem"
  printf '%s\n' public >"$state/platform-keys/jwt-ed25519-public.pem"
  printf '%s\n' tool >"$state/deploy/bin/backup-all.sh"
  tar -czf "$batch/recovery-state.tar.gz" -C "$fixture/state" recovery-state
  printf '%s\n' database | gzip -c >"$batch/platform-mysql.sql.gz"

  gateway="$batch/file-gateway/20260926T000000Z"
  printf '%s' abc >"$fixture/object"
  tar -czf "$gateway/files.tar.gz" -C "$fixture" object
  printf '%s\n' gateway-db | gzip -c >"$gateway/database.sql.gz"
  cat >"$gateway/MANIFEST" <<'EOF'
FORMAT=2
CREATED_AT=20260926T000000Z
CONSISTENCY_MODE=writers-stopped
DATABASE=file_gateway
FILE_UID=10001
FILE_GID=10001
FILE_COUNT=1
FILE_BYTES=3
EOF
  (cd "$gateway" && sha256sum database.sql.gz files.tar.gz >SHA256SUMS)
  printf '%s\n' 'PLATFORM_IMAGE=local/platform@sha256:test' >"$batch/VERSION_MANIFEST"
  printf '%s\n' recovery >"$batch/RECOVERY_INSTRUCTIONS.txt"
  cat >"$batch/MANIFEST" <<'EOF'
FORMAT=2
CREATED_AT=20260926T000000Z
DATABASE_COUNT=2
FILE_GATEWAY_BATCH=file-gateway/20260926T000000Z
RECOVERY_ARCHIVE=recovery-state.tar.gz
RECOVERY_ARCHIVE_ENCRYPTED=false
RECOVERY_STATE_POLICY=restricted-mode-0600
DOCKER_ENGINE_VERSION=test
EOF
  (cd "$batch" && while IFS= read -r file; do sha256sum "$file"; done < <(find . -type f ! -name SHA256SUMS -print | sort) >SHA256SUMS)
  PATH="$fixture/stub:$PATH" "$backup_all" --verify-only --backup "$batch"
  drill="$fixture/drill"
  PATH="$fixture/stub:$PATH" "$backup_all" --rebuild-drill --backup "$batch" --drill-root "$drill"
  [[ -s "$drill/recovery-state/deploy/.env" && -s "$drill/recovery-state/platform-keys/jwt-ed25519-private.pem" ]] ||
    fail 'rebuild drill did not materialize configuration and signing keys'

  encrypted_batch="$fixture/encrypted-batch"
  cp -a "$batch" "$encrypted_batch"
  printf '%s\n' 'test-only-backup-key' >"$fixture/encryption.key"
  chmod 600 "$fixture/encryption.key"
  openssl enc -aes-256-cbc -pbkdf2 -salt \
    -in "$encrypted_batch/recovery-state.tar.gz" \
    -out "$encrypted_batch/recovery-state.tar.gz.enc" \
    -pass "file:$fixture/encryption.key"
  rm -f "$encrypted_batch/recovery-state.tar.gz"
  awk '
    /^RECOVERY_ARCHIVE=/ { print "RECOVERY_ARCHIVE=recovery-state.tar.gz.enc"; next }
    /^RECOVERY_ARCHIVE_ENCRYPTED=/ { print "RECOVERY_ARCHIVE_ENCRYPTED=true"; next }
    /^RECOVERY_STATE_POLICY=/ { print "RECOVERY_STATE_POLICY=encrypted-aes-256-cbc-pbkdf2"; next }
    { print }
  ' "$encrypted_batch/MANIFEST" >"$encrypted_batch/MANIFEST.next"
  mv "$encrypted_batch/MANIFEST.next" "$encrypted_batch/MANIFEST"
  (cd "$encrypted_batch" && while IFS= read -r file; do sha256sum "$file"; done < <(find . -type f ! -name SHA256SUMS -print | sort) >SHA256SUMS)
  PATH="$fixture/stub:$PATH" "$backup_all" --verify-only --backup "$encrypted_batch" \
    --encryption-key-file "$fixture/encryption.key"
  if PATH="$fixture/stub:$PATH" "$backup_all" --verify-only --backup "$encrypted_batch" \
      >"$fixture/missing-key.out" 2>&1; then
    fail 'encrypted recovery state was accepted without its separate key'
  fi
  contains "$fixture/missing-key.out" 'requires --encryption-key-file' 'missing encryption key failure was not explicit'

  # 即使 SHA 文件有效，也拒绝通过符号链接进行归档路径穿越。
  bad="$fixture/bad-gateway"
  install -d "$bad" "$fixture/bad-tree"
  ln -s /etc/passwd "$fixture/bad-tree/escape"
  tar -czf "$bad/files.tar.gz" -C "$fixture/bad-tree" .
  printf '%s\n' bad-db | gzip -c >"$bad/database.sql.gz"
  cp "$gateway/MANIFEST" "$bad/MANIFEST"
  (cd "$bad" && sha256sum database.sql.gz files.tar.gz >SHA256SUMS)
  if PATH="$fixture/stub:$PATH" "$restore_gateway" --backup "$bad" --verify-only >"$fixture/bad.out" 2>&1; then
    fail 'gateway verify-only accepted a symbolic link archive'
  fi
  contains "$fixture/bad.out" '符号链接或特殊文件' 'gateway unsafe archive rejection was not explicit'
fi

echo 'backup/restore safety regression tests passed'
