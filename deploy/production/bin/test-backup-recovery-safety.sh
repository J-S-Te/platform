#!/usr/bin/env bash
set -Eeuo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
restore_mysql="$script_dir/restore-mysql.sh"
backup_gateway="$script_dir/backup-file-gateway.sh"
restore_gateway="$script_dir/restore-file-gateway.sh"
backup_all="$script_dir/backup-all.sh"

fail() { echo "FAIL: $*" >&2; exit 1; }
contains() { grep -Fq -- "$2" "$1" || fail "$3"; }

for script in "$restore_mysql" "$backup_gateway" "$restore_gateway" "$backup_all"; do
  bash -n "$script"
done

# backup-all 必须能复用调用方已持有的部署锁，否则 destroy 会对同一把锁二次 flock 而死锁。
contains "$backup_all" '--deployment-lock-fd' 'backup-all cannot inherit an already-held deployment lock'
contains "$backup_all" 'inherited deployment lock is not held by this task' 'backup-all does not verify an inherited deployment lock'
contains "$backup_all" '--deployment-lock-fd "$deploy_lock_fd"' 'backup-all does not forward the held lock to the gateway snapshot'

# Static security contract: these assertions intentionally enumerate the
# maintenance boundaries so removing one writer or secret class breaks CI.
contains "$restore_mysql" 'runtime/.deploy.lock' 'MySQL restore does not use unified deployment lock'
contains "$restore_mysql" 'docker ps -q' 'MySQL restore does not enumerate actual running containers'
for writer in platform-worker subsystem-provisioner temporal project-sla-notifier customer-presale-worker portal-invite-compensation-worker settlement-worker settlement-catalog-sync data-analysis-aggregation-worker data-analysis-alert-worker data-analysis-metabase; do
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

# Dynamic regression: a Temporal container orphaned from current YAML must
# still stop a contract-mysql restore. The Docker stub never exposes secrets.
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
        [[ "$object" == orphan-temporal-container ]] && printf '%s\n' temporal || printf '%s\n' contract-mysql
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

if PATH="$fixture/stub:$PATH" "$fixture/deploy/bin/restore-mysql.sh" \
    --service contract-mysql \
    --backup "$fixture/deploy/backups/system/20260926T000000Z/contract-mysql.sql.gz" \
    --confirm RESTORE_MYSQL_SERVICE >"$fixture/temporal-confirm.out" 2>&1; then
  fail 'contract restore accepted missing Temporal maintenance confirmation'
fi
contains "$fixture/temporal-confirm.out" 'RESTORE_SHARED_TEMPORAL_DATABASE' 'missing Temporal confirmation did not fail closed'

# GNU/Linux runs the archive-level dynamic tests. macOS lacks the production
# GNU tar/find/flock semantics and is covered by syntax/static + writer tests.
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

  # Archive traversal through a symlink is rejected even with valid SHA files.
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
