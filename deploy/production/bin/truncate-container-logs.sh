#!/usr/bin/env bash
# 截断 Docker 容器 JSON 日志，释放宿主机磁盘。默认 dry-run。
#
# 背景（2026-11-07 事故）：备份撑满磁盘后清理只能回收数 GB，宿主机 55G 占用里有大量
# 容器 stdout 日志（/var/lib/docker/containers/*/*-json.log）。容器日志可以截断，
# 正在运行的容器会继续从当前偏移写，不会影响服务。
#
# 用法：
#   truncate-container-logs.sh            # 只报告大小（dry-run）
#   truncate-container-logs.sh --apply    # 真正截断为 0 字节
# 环境变量：DOCKER_ROOT（默认 /var/lib/docker，便于测试/非标准安装）
# 退出码：0 成功；1 环境不满足；2 参数错误。
set -Eeuo pipefail

# Docker 数据根目录可能被运维改到独立磁盘（测试服务器就不是 /var/lib/docker）。
# 探测顺序：显式 DOCKER_ROOT → docker CLI（普通/sudo）→ 常见安装路径 → 默认值。
detect_docker_root() {
  local candidate
  for candidate in \
    "$(docker info --format '{{.DockerRootDir}}' 2>/dev/null || true)" \
    "$(sudo -n docker info --format '{{.DockerRootDir}}' 2>/dev/null || true)"; do
    candidate="${candidate//$'\n'/}"
    if [[ -n "$candidate" && -d "$candidate/containers" ]]; then
      printf '%s\n' "$candidate"
      return 0
    fi
  done
  for candidate in /var/lib/docker /data/docker /opt/docker /var/lib/containers /mnt/docker; do
    [[ -d "$candidate/containers" ]] && { printf '%s\n' "$candidate"; return 0; }
  done
  printf '/var/lib/docker\n'
}
if [[ -z "${DOCKER_ROOT:-}" ]]; then
  DOCKER_ROOT="$(detect_docker_root)"
fi
APPLY=false
for arg in "$@"; do
  case "$arg" in
    --apply) APPLY=true ;;
    --dry-run) APPLY=false ;;
    -h|--help) sed -n '2,12p' "$0"; exit 0 ;;
    *) echo "unknown argument: $arg" >&2; exit 2 ;;
  esac
done

[[ -d "$DOCKER_ROOT/containers" ]] || { echo "container log directory not found: $DOCKER_ROOT/containers" >&2; exit 1; }

truncate_file() {
  local file="$1"
  if [[ -w "$file" ]]; then
    : >"$file"
  elif command -v sudo >/dev/null 2>&1 && sudo -n true 2>/dev/null; then
    sudo -n truncate -s 0 "$file"
  else
    echo "no permission to truncate: $file" >&2
    return 1
  fi
}

before_avail="$(df -P -k "$DOCKER_ROOT" | awk 'NR==2 {print $4}')"
echo "container log cleanup mode: $( $APPLY && echo apply || echo dry-run ) root=$DOCKER_ROOT"
echo "df before: $(df -P -k "$DOCKER_ROOT" | awk 'NR==2 {print $3" KiB used, "$4" KiB available"}')"

total_kib=0
while IFS= read -r file; do
  size_bytes="$(stat -c%s "$file" 2>/dev/null || stat -f%z "$file")"
  [[ "$size_bytes" =~ ^[0-9]+$ ]] || continue
  (( size_bytes > 0 )) || continue
  total_kib=$(( total_kib + (size_bytes + 1023) / 1024 ))
  echo "$(printf '%-10s' "$(numfmt --to=iec --suffix=B "$size_bytes" 2>/dev/null || echo "${size_bytes}B")") $file"
  if [[ "$APPLY" == true ]]; then
    truncate_file "$file" || exit 1
  fi
done < <(find "$DOCKER_ROOT/containers" -maxdepth 2 -type f -name '*-json.log*' -size +0c 2>/dev/null | LC_ALL=C sort)

after_avail="$(df -P -k "$DOCKER_ROOT" | awk 'NR==2 {print $4}')"
echo "container logs: would free/reclaimed ${total_kib} KiB; available: ${before_avail} -> ${after_avail} KiB"
if [[ "$APPLY" == true ]]; then
  echo "applied: truncated container logs to 0 bytes"
else
  echo "dry-run: nothing was truncated; re-run with --apply to truncate"
fi
