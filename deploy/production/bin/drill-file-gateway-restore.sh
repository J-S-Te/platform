#!/usr/bin/env bash
set -Eeuo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
deploy_dir="$(cd -- "$script_dir/.." && pwd)"
runtime_file="$deploy_dir/.env"
release_file="$deploy_dir/.release.env"

usage() {
  echo "用法：$0 --backup <备份批次目录>" >&2
  exit 64
}

backup_dir=""
while (($#)); do
  case "$1" in
    --backup) (($# >= 2)) || usage; backup_dir="$2"; shift 2 ;;
    *) usage ;;
  esac
done
[[ -n "$backup_dir" ]] || usage
backup_dir="$(cd -- "$backup_dir" 2>/dev/null && pwd)" || { echo "备份目录不存在" >&2; exit 1; }

for command_name in docker gzip tar sha256sum mktemp awk; do
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

platform_image="$(awk -F= '$1 == "PLATFORM_IMAGE" {sub(/^[^=]*=/, ""); print; exit}' "$release_file")"
[[ "$platform_image" == *@sha256:* ]] || { echo "PLATFORM_IMAGE 必须是不可变 digest" >&2; exit 1; }

install -d -m 700 "$deploy_dir/runtime"
drill_root="$(mktemp -d "$deploy_dir/runtime/file-gateway-restore-drill.XXXXXX")"
db_container="file-gateway-restore-drill-$(date -u +%Y%m%dT%H%M%SZ)-$$"
cleanup() {
  docker rm -f "$db_container" >/dev/null 2>&1 || true
  if [[ -n "${drill_root:-}" && "$drill_root" == "$deploy_dir/runtime/file-gateway-restore-drill."* ]]; then
    rm -rf -- "$drill_root"
  fi
}
trap cleanup EXIT

install -d -m 750 "$drill_root/files"
tar --extract --gzip --file "$backup_dir/files.tar.gz" --directory "$drill_root/files" --no-same-owner --no-same-permissions

docker run -d --name "$db_container" --tmpfs /var/lib/mysql:rw,noexec,nosuid,size=1g \
  -e MYSQL_ROOT_PASSWORD=restore-drill-only -e MYSQL_DATABASE=file_gateway mysql:8.4 >/dev/null
for _ in $(seq 1 60); do
  if docker exec "$db_container" mysqladmin ping -h 127.0.0.1 -uroot -prestore-drill-only --silent >/dev/null 2>&1; then
    break
  fi
  sleep 2
done
docker exec "$db_container" mysqladmin ping -h 127.0.0.1 -uroot -prestore-drill-only --silent >/dev/null
gzip --decompress --stdout "$backup_dir/database.sql.gz" | docker exec -i -e MYSQL_PWD=restore-drill-only "$db_container" mysql -uroot file_gateway

database_ready="$(docker exec -e MYSQL_PWD=restore-drill-only "$db_container" mysql -uroot -N -B file_gateway -e "SELECT COUNT(*) FROM file_object WHERE status='READY';")"
database_bytes="$(docker exec -e MYSQL_PWD=restore-drill-only "$db_container" mysql -uroot -N -B file_gateway -e "SELECT COALESCE(SUM(size_bytes),0) FROM file_version WHERE status='READY';")"
inventory_output="$(docker run --rm --network "container:$db_container" \
  --volume "$drill_root/files:/app/data/file-gateway:ro" \
  -e FILE_GATEWAY_DATABASE_DSN='root:restore-drill-only@tcp(127.0.0.1:3306)/file_gateway?charset=utf8mb4&parseTime=true&loc=UTC' \
  -e FILE_GATEWAY_STORAGE_ROOT=/app/data/file-gateway \
  "$platform_image" ./file-inventory --limit 100000 --interval 0s)"

echo "文件网关隔离恢复演练通过：backup=$(basename -- "$backup_dir") ready_files=$database_ready ready_bytes=$database_bytes"
echo "$inventory_output"
