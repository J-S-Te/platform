#!/usr/bin/env bash
set -Eeuo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
deploy_dir="$(cd -- "$script_dir/.." && pwd)"
runtime_file="$deploy_dir/.env"
release_file="$deploy_dir/.release.env"
backup_root="${FILE_GATEWAY_BACKUP_ROOT:-$deploy_dir/backups/file-gateway}"
stamp="$(date -u +%Y%m%dT%H%M%SZ)"

for command_name in docker gzip tar sha256sum flock install mktemp awk; do
  command -v "$command_name" >/dev/null || { echo "缺少命令：$command_name" >&2; exit 1; }
done
[[ -f "$runtime_file" && -f "$release_file" ]] || { echo "生产环境文件不完整" >&2; exit 1; }

env_value() {
  awk -F= -v key="$1" '$0 !~ /^[[:space:]]*#/ && $1 == key { sub(/^[^=]*=/, ""); print; exit }' "$runtime_file"
}

storage_root="$(env_value FILE_GATEWAY_HOST_ROOT)"
storage_root="${storage_root:-/opt/basic-platform/data/file-gateway}"
[[ -d "$storage_root" && ! -L "$storage_root" ]] || { echo "文件网关存储目录不存在或为符号链接：$storage_root" >&2; exit 1; }

install -d -m 700 "$backup_root"
exec 9>"$backup_root/.backup.lock"
flock -n 9 || { echo "另一个文件网关备份正在运行" >&2; exit 1; }
work="$(mktemp -d "$backup_root/.${stamp}.XXXXXX")"
trap 'rm -rf -- "$work"' EXIT

compose=(docker compose --project-directory "$deploy_dir" --file "$deploy_dir/compose.yaml" --env-file "$runtime_file" --env-file "$release_file")
db_name="$(env_value FILE_GATEWAY_DB_NAME)"; db_name="${db_name:-file_gateway}"
db_password="$(env_value FILE_GATEWAY_DB_ROOT_PASSWORD)"
[[ -n "$db_password" ]] || { echo "缺少 FILE_GATEWAY_DB_ROOT_PASSWORD" >&2; exit 1; }

# 安全（SEC-F6）：密码只在宿主进程环境里按名转发给容器（-e MYSQL_PWD 不带值），
# 不再以 -e "MYSQL_PWD=<口令>" 形式进入 docker/compose 的 argv（/proc/*/cmdline）。
MYSQL_PWD="$db_password" "${compose[@]}" exec -T -e MYSQL_PWD file-gateway-mysql \
  mysqldump --single-transaction --routines --triggers -uroot "$db_name" | gzip -9 >"$work/database.sql.gz"

# 临时上传可在恢复后由会话对账清理，不进入长期备份。
tar --create --gzip --file "$work/files.tar.gz" --directory "$storage_root" --exclude='./temporary' .
# Store stable relative names. The staging directory is atomically renamed at
# the end of the backup, so absolute staging paths would become unverifiable.
(cd -- "$work" && sha256sum database.sql.gz files.tar.gz >SHA256SUMS)
printf 'created_at=%s\nstorage_root=%s\ndatabase=%s\n' "$stamp" "$storage_root" "$db_name" >"$work/MANIFEST"
mv "$work" "$backup_root/$stamp"
trap - EXIT
echo "文件网关备份已完成：$backup_root/$stamp"
