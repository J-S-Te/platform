#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
deploy_dir="$(cd -- "$script_dir/.." && pwd)"
runtime_file="$deploy_dir/.env"
release_file="$deploy_dir/.release.env"
backup_root="${FILE_GATEWAY_BACKUP_ROOT:-$deploy_dir/backups/file-gateway}"
stamp="$(date -u +%Y%m%dT%H%M%SZ)"
inherited_deploy_lock_fd=""

usage() {
  echo "用法：$0 [--stamp YYYYMMDDTHHMMSSZ] [--deployment-lock-fd FD]" >&2
  exit 64
}

while (($#)); do
  case "$1" in
    --stamp) (($# >= 2)) || usage; stamp="$2"; shift 2 ;;
    --deployment-lock-fd) (($# >= 2)) || usage; inherited_deploy_lock_fd="$2"; shift 2 ;;
    *) usage ;;
  esac
done
[[ "$stamp" =~ ^[0-9]{8}T[0-9]{6}Z$ ]] || usage

for command_name in docker gzip tar sha256sum flock install mktemp awk find grep wc chown chmod realpath; do
  command -v "$command_name" >/dev/null || { echo "缺少命令：$command_name" >&2; exit 1; }
done
[[ -f "$runtime_file" && ! -L "$runtime_file" && -f "$release_file" && ! -L "$release_file" ]] || {
  echo "生产环境文件不完整或不安全" >&2
  exit 1
}

env_value() {
  awk -F= -v key="$1" '$0 !~ /^[[:space:]]*#/ && $1 == key { sub(/^[^=]*=/, ""); print; exit }' "$runtime_file"
}

storage_root="$(env_value FILE_GATEWAY_HOST_ROOT)"
storage_root="${storage_root:-$deploy_dir/data/file-gateway}"
[[ "$storage_root" == /* && "$storage_root" != / && -d "$storage_root" && ! -L "$storage_root" ]] || {
  echo "文件网关存储目录不存在或不安全：$storage_root" >&2
  exit 1
}
if find "$storage_root" -xdev \( -type l -o \( ! -type d ! -type f \) \) -print -quit | grep -q .; then
  echo "文件网关存储目录包含符号链接或特殊文件，拒绝备份" >&2
  exit 1
fi

# A file backup is a deployment-wide maintenance operation.  backup-all may
# pass its already-held descriptor; direct invocations acquire the same lock.
install -d -m 700 "$deploy_dir/runtime"
deploy_lock="$deploy_dir/runtime/.deploy.lock"
[[ ! -L "$deploy_lock" ]] || { echo "部署锁不能是符号链接" >&2; exit 1; }
if [[ -n "$inherited_deploy_lock_fd" ]]; then
  [[ "$inherited_deploy_lock_fd" =~ ^[0-9]+$ && -e "/proc/$$/fd/$inherited_deploy_lock_fd" ]] || {
    echo "继承的部署锁描述符无效" >&2
    exit 1
  }
  lock_target="$(realpath "/proc/$$/fd/$inherited_deploy_lock_fd")"
  [[ "$lock_target" == "$(realpath "$deploy_lock")" ]] || { echo "继承的部署锁指向错误文件" >&2; exit 1; }
  flock -n "$inherited_deploy_lock_fd" || { echo "继承的部署锁未被当前任务持有" >&2; exit 1; }
else
  exec 8>"$deploy_lock"
  flock -w 900 8 || { echo "等待统一部署锁超时" >&2; exit 1; }
fi

install -d -m 700 "$backup_root"
[[ ! -L "$backup_root/.backup.lock" ]] || { echo "文件网关备份锁不能是符号链接" >&2; exit 1; }
exec 9>"$backup_root/.backup.lock"
flock -n 9 || { echo "另一个文件网关备份正在运行" >&2; exit 1; }
[[ ! -e "$backup_root/$stamp" ]] || { echo "备份批次已存在：$backup_root/$stamp" >&2; exit 1; }
work="$(mktemp -d "$backup_root/.${stamp}.XXXXXX")"
stopped_ids=()

resume_gateway_containers() {
  local failed=0 container_id
  for container_id in "${stopped_ids[@]}"; do
    docker start "$container_id" >/dev/null || failed=1
  done
  stopped_ids=()
  return "$failed"
}

cleanup() {
  local status=$?
  trap - EXIT
  if ((${#stopped_ids[@]} > 0)); then
    resume_gateway_containers || status=1
  fi
  if [[ -n "${work:-}" && -d "$work" && "$work" == "$backup_root/.${stamp}."* ]]; then
    rm -rf -- "$work"
  fi
  exit "$status"
}
trap cleanup EXIT

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

# Stop every running instance labelled as the gateway in this project,
# including orphaned containers whose service was pruned from current YAML.
while IFS= read -r candidate_id; do
  [[ -n "$candidate_id" ]] || continue
  stopped_ids+=("$candidate_id")
done < <(docker ps -q --filter "label=com.docker.compose.project=$project" --filter 'label=com.docker.compose.service=file-gateway')
if ((${#stopped_ids[@]} > 0)); then
  docker stop --timeout 60 "${stopped_ids[@]}" >/dev/null
fi

# Refuse a hidden/legacy writer rather than taking a split database/file
# snapshot. Environment values are inspected in-memory and are never logged.
running_container_ids="$(docker ps -q)" || { echo "无法枚举运行中的 Docker 容器" >&2; exit 1; }
unknown_writers=()
while IFS= read -r candidate_id; do
  [[ -n "$candidate_id" && "$candidate_id" != "$db_container" ]] || continue
  if docker inspect -f '{{range .Config.Env}}{{println .}}{{end}}' "$candidate_id" 2>/dev/null |
      awk 'index($0, "file-gateway-mysql") { found=1 } END { exit(found ? 0 : 1) }'; then
    candidate_service="$(docker inspect -f '{{index .Config.Labels "com.docker.compose.service"}}' "$candidate_id" 2>/dev/null || true)"
    candidate_name="$(docker inspect -f '{{.Name}}' "$candidate_id" 2>/dev/null || printf '%s' "$candidate_id")"
    unknown_writers+=("${candidate_name#/} [service=${candidate_service:-unknown}]")
  fi
done <<<"$running_container_ids"
if ((${#unknown_writers[@]} > 0)); then
  printf '检测到未冻结的文件网关数据库写入者，拒绝生成不一致备份：\n' >&2
  printf '  - %s\n' "${unknown_writers[@]}" >&2
  exit 1
fi

# Password is forwarded by environment name; its value never appears in the
# docker/compose argv or backup logs.
MYSQL_PWD="$db_password" "${compose[@]}" exec -T -e MYSQL_PWD file-gateway-mysql \
  mysqldump --single-transaction --routines --triggers --events -uroot "$db_name" | gzip -9 >"$work/database.sql.gz"
gzip -t "$work/database.sql.gz"
[[ -s "$work/database.sql.gz" ]] || { echo "文件网关数据库备份为空" >&2; exit 1; }

# The gateway remains stopped from immediately before mysqldump until the
# archive and inventory are complete. This makes DB rows and object files a
# single write-free recovery point. Temporary uploads are intentionally omitted.
tar --create --gzip --file "$work/files.tar.gz" --directory "$storage_root" --exclude='./temporary' .
file_count=0
file_bytes=0
while IFS= read -r -d '' stored_file; do
  file_count=$((file_count + 1))
  stored_bytes="$(wc -c <"$stored_file")"
  file_bytes=$((file_bytes + stored_bytes))
done < <(find "$storage_root" -xdev -path "$storage_root/temporary" -prune -o -type f -print0)
(cd -- "$work" && sha256sum database.sql.gz files.tar.gz >SHA256SUMS)
cat >"$work/MANIFEST" <<EOF
FORMAT=2
CREATED_AT=$stamp
CONSISTENCY_MODE=writers-stopped
DATABASE=$db_name
FILE_UID=10001
FILE_GID=10001
FILE_COUNT=$file_count
FILE_BYTES=$file_bytes
EOF
chmod 600 "$work/database.sql.gz" "$work/files.tar.gz" "$work/SHA256SUMS" "$work/MANIFEST"
mv "$work" "$backup_root/$stamp"
work=""
chmod 700 "$backup_root/$stamp"

if ! resume_gateway_containers; then
  echo "备份已经安全写入，但原文件网关容器恢复启动失败；请立即检查容器状态：$backup_root/$stamp" >&2
  exit 1
fi
trap - EXIT
echo "文件网关一致性备份已完成：$backup_root/$stamp"
