#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
deploy_dir="$(cd -- "$script_dir/.." && pwd)"
runtime_file="$deploy_dir/.env"
release_file="$deploy_dir/.release.env"

usage() {
  cat >&2 <<EOF
用法：$0 --backup <备份批次目录> --verify-only
      $0 --backup <备份批次目录> --confirm RESTORE_FILE_GATEWAY
EOF
  exit 64
}

backup_dir=""
confirmation=""
verify_only=false
while (($#)); do
  case "$1" in
    --backup) (($# >= 2)) || usage; backup_dir="$2"; shift 2 ;;
    --verify-only) verify_only=true; shift ;;
    --confirm) (($# >= 2)) || usage; confirmation="$2"; shift 2 ;;
    *) usage ;;
  esac
done
[[ -n "$backup_dir" ]] || usage
if [[ "$verify_only" != true && "$confirmation" != RESTORE_FILE_GATEWAY ]]; then
  echo "恢复需要 --confirm RESTORE_FILE_GATEWAY" >&2
  exit 64
fi
backup_dir="$(cd -- "$backup_dir" 2>/dev/null && pwd)" || { echo "备份目录不存在" >&2; exit 1; }

for command_name in gzip tar sha256sum flock install mktemp awk find chown chmod realpath grep wc; do
  command -v "$command_name" >/dev/null || { echo "缺少命令：$command_name" >&2; exit 1; }
done
for required in database.sql.gz files.tar.gz SHA256SUMS MANIFEST; do
  [[ -f "$backup_dir/$required" && ! -L "$backup_dir/$required" ]] || { echo "备份文件缺失或不安全：$required" >&2; exit 1; }
done

manifest_value() {
  awk -F= -v key="$1" '$1 == key { sub(/^[^=]*=/, ""); print; exit }' "$backup_dir/MANIFEST"
}

validate_checksum_manifest() {
  awk '
    NF < 2 { exit 1 }
    {
      name=substr($0, 67)
      sub(/^\*/, "", name)
      if (name == "" || name ~ /^\// || name ~ /(^|\/)\.\.($|\/)/ || name ~ /\\/) exit 1
    }
  ' "$backup_dir/SHA256SUMS" || { echo "SHA256SUMS 包含不安全路径" >&2; exit 1; }
  (cd -- "$backup_dir" && sha256sum --check SHA256SUMS)
}

archive_stage=""
stage=""
cleanup_stage() {
  if [[ -n "${archive_stage:-}" && -d "$archive_stage" ]]; then
    rm -rf -- "$archive_stage"
  fi
  if [[ -n "${stage:-}" && -d "$stage" ]]; then
    case "$stage" in
      */.file-gateway-restore.*) rm -rf -- "$stage" ;;
    esac
  fi
}
trap cleanup_stage EXIT

validate_and_extract() {
  local destination="$1" format expected_count expected_bytes actual_count actual_bytes
  validate_checksum_manifest
  gzip -t "$backup_dir/database.sql.gz"
  [[ -s "$backup_dir/database.sql.gz" ]] || { echo "数据库备份为空" >&2; return 1; }
  if tar --list --gzip --file "$backup_dir/files.tar.gz" |
      awk '/(^\/|(^|\/)\.\.($|\/))/{bad=1} END{exit bad?0:1}'; then
    echo "文件备份包含绝对路径或穿越路径" >&2
    return 1
  fi
  tar --extract --gzip --file "$backup_dir/files.tar.gz" --directory "$destination" --no-same-owner --no-same-permissions
  if find "$destination" -xdev \( -type l -o \( ! -type d ! -type f \) \) -print -quit | grep -q .; then
    echo "文件备份解压后包含符号链接或特殊文件" >&2
    return 1
  fi
  format="$(manifest_value FORMAT)"
  if [[ "$format" == 2 ]]; then
    [[ "$(manifest_value CONSISTENCY_MODE)" == writers-stopped ]] || { echo "备份不是写入冻结的一致性批次" >&2; return 1; }
    [[ "$(manifest_value FILE_UID)" == 10001 && "$(manifest_value FILE_GID)" == 10001 ]] || {
      echo "备份声明的文件网关 UID/GID 不受支持" >&2
      return 1
    }
    expected_count="$(manifest_value FILE_COUNT)"
    expected_bytes="$(manifest_value FILE_BYTES)"
    [[ "$expected_count" =~ ^[0-9]+$ && "$expected_bytes" =~ ^[0-9]+$ ]] || { echo "备份文件清单计数无效" >&2; return 1; }
    actual_count=0
    actual_bytes=0
    while IFS= read -r -d '' restored_file; do
      actual_count=$((actual_count + 1))
      restored_bytes="$(wc -c <"$restored_file")"
      actual_bytes=$((actual_bytes + restored_bytes))
    done < <(find "$destination" -xdev -type f -print0)
    [[ "$actual_count" == "$expected_count" && "$actual_bytes" == "$expected_bytes" ]] || {
      echo "文件归档清单不一致：count=$actual_count/$expected_count bytes=$actual_bytes/$expected_bytes" >&2
      return 1
    }
  elif [[ -n "$format" ]]; then
    echo "不支持的文件网关备份格式：$format" >&2
    return 1
  fi
}

archive_stage="$(mktemp -d "${TMPDIR:-/tmp}/file-gateway-verify.XXXXXX")"
validate_and_extract "$archive_stage"
if [[ "$verify_only" == true ]]; then
  echo "文件网关备份校验通过：$backup_dir"
  exit 0
fi

command -v docker >/dev/null || { echo "缺少命令：docker" >&2; exit 1; }

[[ -f "$runtime_file" && ! -L "$runtime_file" && -f "$release_file" && ! -L "$release_file" ]] || {
  echo "生产环境文件不完整或不安全" >&2
  exit 1
}
env_value() {
  awk -F= -v key="$1" '$0 !~ /^[[:space:]]*#/ && $1 == key { sub(/^[^=]*=/, ""); print; exit }' "$runtime_file"
}
storage_root="$(env_value FILE_GATEWAY_HOST_ROOT)"
storage_root="${storage_root:-/opt/unified-identity-platform/data/file-gateway}"
[[ "$storage_root" == /* && "$storage_root" != / && ! -L "$storage_root" ]] || { echo "存储根目录不安全：$storage_root" >&2; exit 1; }

# Keep the unified deployment lock from final backup revalidation through DB
# import, directory swap, ownership repair and gateway health verification.
install -d -m 700 "$deploy_dir/runtime"
deploy_lock="$deploy_dir/runtime/.deploy.lock"
[[ ! -L "$deploy_lock" ]] || { echo "部署锁不能是符号链接" >&2; exit 1; }
exec 8>"$deploy_lock"
flock -w 900 8 || { echo "等待统一部署锁超时" >&2; exit 1; }

parent_dir="$(dirname -- "$storage_root")"
install -d -m 750 "$parent_dir"
[[ ! -L "$parent_dir/.file-gateway-restore.lock" ]] || { echo "恢复锁不能是符号链接" >&2; exit 1; }
exec 9>"$parent_dir/.file-gateway-restore.lock"
flock -n 9 || { echo "另一个文件网关恢复正在运行" >&2; exit 1; }

# Revalidate immediately before changing live data. Move the already validated
# tree to the target filesystem so the final swap remains atomic.
validate_checksum_manifest
stage="$(mktemp -d "$parent_dir/.file-gateway-restore.XXXXXX")"
tar --extract --gzip --file "$backup_dir/files.tar.gz" --directory "$stage" --no-same-owner --no-same-permissions
if find "$stage" -xdev \( -type l -o \( ! -type d ! -type f \) \) -print -quit | grep -q .; then
  echo "文件备份包含符号链接或特殊文件" >&2
  exit 1
fi
install -d -m 750 "$stage/temporary" "$stage/quarantine"
chown -R 10001:10001 "$stage"
find "$stage" -xdev -type d -exec chmod 0750 {} +
find "$stage" -xdev -type f -exec chmod 0640 {} +

rollback="$parent_dir/.file-gateway-rollback-$(date -u +%Y%m%dT%H%M%SZ)"
compose=(docker compose --project-directory "$deploy_dir" --file "$deploy_dir/docker-compose.yml" --env-file "$runtime_file" --env-file "$release_file")
db_name="$(env_value FILE_GATEWAY_DB_NAME)"; db_name="${db_name:-file_gateway}"
db_password="$(env_value FILE_GATEWAY_DB_ROOT_PASSWORD)"
[[ -n "$db_password" ]] || { echo "缺少 FILE_GATEWAY_DB_ROOT_PASSWORD" >&2; exit 1; }
db_container="$("${compose[@]}" ps -q file-gateway-mysql 2>/dev/null || true)"
[[ -n "$db_container" && "$(docker inspect -f '{{.State.Running}}' "$db_container")" == true ]] || {
  echo "file-gateway-mysql 必须处于运行状态" >&2
  exit 1
}
project="$(docker inspect -f '{{index .Config.Labels "com.docker.compose.project"}}' "$db_container" 2>/dev/null || true)"
[[ -n "$project" && "$project" != '<no value>' ]] || { echo "无法识别文件网关 Compose project" >&2; exit 1; }

gateway_ids=()
while IFS= read -r candidate_id; do
  [[ -n "$candidate_id" ]] && gateway_ids+=("$candidate_id")
done < <(docker ps -q --filter "label=com.docker.compose.project=$project" --filter 'label=com.docker.compose.service=file-gateway')
running_container_ids="$(docker ps -q)" || { echo "无法枚举运行中的 Docker 容器" >&2; exit 1; }
unknown_writers=()
while IFS= read -r candidate_id; do
  [[ -n "$candidate_id" && "$candidate_id" != "$db_container" ]] || continue
  is_managed_gateway=false
  for gateway_id in "${gateway_ids[@]}"; do
    [[ "$candidate_id" == "$gateway_id" ]] && is_managed_gateway=true
  done
  [[ "$is_managed_gateway" == false ]] || continue
  if docker inspect -f '{{range .Config.Env}}{{println .}}{{end}}' "$candidate_id" 2>/dev/null |
      awk 'index($0, "file-gateway-mysql") { found=1 } END { exit(found ? 0 : 1) }'; then
    candidate_service="$(docker inspect -f '{{index .Config.Labels "com.docker.compose.service"}}' "$candidate_id" 2>/dev/null || true)"
    candidate_name="$(docker inspect -f '{{.Name}}' "$candidate_id" 2>/dev/null || printf '%s' "$candidate_id")"
    unknown_writers+=("${candidate_name#/} [service=${candidate_service:-unknown}]")
  fi
done <<<"$running_container_ids"
if ((${#unknown_writers[@]} > 0)); then
  printf '检测到未停止的文件网关数据库写入者，拒绝恢复：\n' >&2
  printf '  - %s\n' "${unknown_writers[@]}" >&2
  exit 1
fi
if ((${#gateway_ids[@]} > 0)); then
  docker stop --timeout 60 "${gateway_ids[@]}" >/dev/null
fi

if [[ -e "$storage_root" ]]; then
  mv -- "$storage_root" "$rollback"
fi
mv -- "$stage" "$storage_root"
stage=""

# MYSQL_PWD is forwarded by name so the secret never appears in argv.
if ! gzip --decompress --stdout "$backup_dir/database.sql.gz" |
    MYSQL_PWD="$db_password" "${compose[@]}" exec -T -e MYSQL_PWD file-gateway-mysql mysql -uroot "$db_name"; then
  failed="$parent_dir/.file-gateway-failed-restore-$(date -u +%Y%m%dT%H%M%SZ)"
  mv -- "$storage_root" "$failed"
  [[ -d "$rollback" ]] && mv -- "$rollback" "$storage_root"
  echo "数据库恢复失败；文件目录已回退，失败目录保留在 ${failed}；网关保持停止" >&2
  exit 1
fi

[[ "$(find "$storage_root" -xdev ! -user 10001 -print -quit)" == "" ]] || { echo "恢复目录存在非 UID 10001 条目" >&2; exit 1; }
[[ "$(find "$storage_root" -xdev ! -group 10001 -print -quit)" == "" ]] || { echo "恢复目录存在非 GID 10001 条目" >&2; exit 1; }
"${compose[@]}" up -d --wait --wait-timeout 180 file-gateway
trap - EXIT
cleanup_stage
echo "恢复完成：数据库与文件来自同一一致性批次 $backup_dir"
if [[ -d "$rollback" ]]; then
  echo "旧文件目录已保留，完成业务验收后再人工清理：$rollback"
fi
if ((${#gateway_ids[@]} > 1)); then
  echo "已检测并停止多个旧/裁剪文件网关容器；只启动当前 Compose 定义的 file-gateway，请人工核对旧容器。"
fi
