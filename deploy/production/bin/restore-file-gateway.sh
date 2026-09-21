#!/usr/bin/env bash
set -Eeuo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
deploy_dir="$(cd -- "$script_dir/.." && pwd)"
runtime_file="$deploy_dir/.env"
release_file="$deploy_dir/.release.env"

usage() {
  echo "用法：$0 --backup <备份批次目录> --confirm" >&2
  exit 64
}

backup_dir=""
confirmed=false
while (($#)); do
  case "$1" in
    --backup) (($# >= 2)) || usage; backup_dir="$2"; shift 2 ;;
    --confirm) confirmed=true; shift ;;
    *) usage ;;
  esac
done
[[ "$confirmed" == true && -n "$backup_dir" ]] || usage
backup_dir="$(cd -- "$backup_dir" 2>/dev/null && pwd)" || { echo "备份目录不存在" >&2; exit 1; }

for command_name in docker gzip tar sha256sum flock install mktemp awk; do
  command -v "$command_name" >/dev/null || { echo "缺少命令：$command_name" >&2; exit 1; }
done
for required in database.sql.gz files.tar.gz SHA256SUMS MANIFEST; do
  [[ -f "$backup_dir/$required" && ! -L "$backup_dir/$required" ]] || { echo "备份文件缺失或不安全：$required" >&2; exit 1; }
done
(cd -- "$backup_dir" && sha256sum --check SHA256SUMS)
gzip -t "$backup_dir/database.sql.gz"
if tar --list --file "$backup_dir/files.tar.gz" | awk '/(^\/|(^|\/)\.\.($|\/))/{bad=1} END{exit bad?0:1}'; then
  echo "文件备份包含绝对路径或穿越路径" >&2
  exit 1
fi

env_value() {
  awk -F= -v key="$1" '$0 !~ /^[[:space:]]*#/ && $1 == key { sub(/^[^=]*=/, ""); print; exit }' "$runtime_file"
}
[[ -f "$runtime_file" && -f "$release_file" ]] || { echo "生产环境文件不完整" >&2; exit 1; }
storage_root="$(env_value FILE_GATEWAY_HOST_ROOT)"
storage_root="${storage_root:-/opt/basic-platform/data/file-gateway}"
[[ "$storage_root" == /* && "$storage_root" != "/" && ! -L "$storage_root" ]] || { echo "存储根目录不安全：$storage_root" >&2; exit 1; }

parent_dir="$(dirname -- "$storage_root")"
install -d -m 750 "$parent_dir"
exec 9>"$parent_dir/.file-gateway-restore.lock"
flock -n 9 || { echo "另一个文件网关恢复正在运行" >&2; exit 1; }
stage="$(mktemp -d "$parent_dir/.file-gateway-restore.XXXXXX")"
rollback="$parent_dir/.file-gateway-rollback-$(date -u +%Y%m%dT%H%M%SZ)"
cleanup() { [[ -d "$stage" ]] && rm -rf -- "$stage"; }
trap cleanup EXIT
tar --extract --gzip --file "$backup_dir/files.tar.gz" --directory "$stage" --no-same-owner --no-same-permissions
install -d -m 750 "$stage/temporary" "$stage/quarantine"

compose=(docker compose --project-directory "$deploy_dir" --file "$deploy_dir/compose.yaml" --env-file "$runtime_file" --env-file "$release_file")
db_name="$(env_value FILE_GATEWAY_DB_NAME)"; db_name="${db_name:-file_gateway}"
db_password="$(env_value FILE_GATEWAY_DB_ROOT_PASSWORD)"
[[ -n "$db_password" ]] || { echo "缺少 FILE_GATEWAY_DB_ROOT_PASSWORD" >&2; exit 1; }

"${compose[@]}" stop file-gateway
if [[ -e "$storage_root" ]]; then
  mv -- "$storage_root" "$rollback"
fi
mv -- "$stage" "$storage_root"
stage=""
if ! gzip --decompress --stdout "$backup_dir/database.sql.gz" | "${compose[@]}" exec -T -e MYSQL_PWD="$db_password" file-gateway-mysql mysql -uroot "$db_name"; then
  failed="$parent_dir/.file-gateway-failed-restore-$(date -u +%Y%m%dT%H%M%SZ)"
  mv -- "$storage_root" "$failed"
  [[ -d "$rollback" ]] && mv -- "$rollback" "$storage_root"
  echo "数据库恢复失败；文件目录已回退，失败目录保留在 $failed" >&2
  exit 1
fi
"${compose[@]}" up -d --wait --wait-timeout 180 file-gateway
echo "恢复完成：数据库与文件来自同一批次 $backup_dir"
echo "旧文件目录保留用于人工确认后清理：$rollback"
