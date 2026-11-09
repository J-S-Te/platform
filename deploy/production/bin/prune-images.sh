#!/usr/bin/env bash
# Docker 镜像保留清理工具（默认 dry-run，只有显式 --apply 才删除镜像）。
#
# 背景（2026-11-07 测试服务器真实事故）：每次发布都按不可变 digest 拉取新镜像，旧镜像
# 一直留在宿主机上；镜像与历史备份一起累积，磁盘两次被打到 100%，后续发布被 fail-closed
# 拒绝。镜像可重复拉取，不需要像备份那样长期保留，因此单独按小时保留。
#
# 用法：
#   prune-images.sh                     # 只打印 docker system df 与将执行的命令（dry-run）
#   prune-images.sh --keep-hours 24     # 只清理 24 小时前的未使用镜像（仍是 dry-run）
#   prune-images.sh --keep-hours 24 --apply   # 真正删除
#
# 参数：
#   --keep-hours N  只清理时间戳早于 N 小时、且未被任何容器使用的镜像；默认 24；
#                   0 表示清理所有未被容器使用的镜像（慎用：回滚窗口随之消失）。
#   --apply         真正执行 docker image prune；缺省只打印计划，不删除任何内容。
#
# 发布侧环境变量（deploy-service.sh 读取，非法值 WARN 后回落默认值）：
#   PLATFORM_IMAGE_RETENTION_HOURS  默认 24，等价于这里的 --keep-hours。
#
# 安全边界：
#   - 只执行 `docker image prune -a -f --filter "until=<N>h"`；绝不使用 `docker system prune`
#     （它还会删除未使用的网络、卷与容器），也不删除数据卷和容器。
#   - 运行中容器引用的镜像天然受保护：docker 不会删除仍被容器使用的镜像。
#   - docker CLI 不存在或 daemon 不可达时明确报错退出 1，不做任何删除（fail-closed）。
#   - 幂等：重复执行安全，已清理的镜像不会再次出现。
# 退出码：0 成功；1 docker 环境不满足或 prune 执行失败；2 参数错误。
set -Eeuo pipefail
umask 077

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"

usage() {
  cat >&2 <<'EOF'
usage: prune-images.sh [--keep-hours N] [--apply]

  --keep-hours N  只清理早于 N 小时、且未被任何容器使用的镜像（默认 24；0 = 清理所有未使用镜像）
  --apply         真正执行 docker image prune；缺省只打印计划与 docker system df（dry-run）

只执行 `docker image prune -a -f --filter "until=<N>h"`：不使用 docker system prune，
不删除数据卷，不删除容器；运行中容器使用的镜像由 docker 自身保护。
EOF
  exit "${1:-2}"
}

keep_hours=24
apply=false

while (($#)); do
  case "$1" in
    --keep-hours) (($# >= 2)) || usage; keep_hours="$2"; shift 2 ;;
    --apply) apply=true; shift ;;
    -h|--help) usage 0 ;;
    *) usage ;;
  esac
done

# 只接受非负十进制整数，避免 "72;rm"、空串或浮点数进入算术/过滤字符串。
[[ "$keep_hours" =~ ^[0-9]+$ ]] || {
  echo "prune-images: --keep-hours must be a non-negative integer: $keep_hours" >&2
  exit 2
}

# docker 环境检查必须在任何删除动作之前，且失败即退出 1（不静默跳过）。
command -v docker >/dev/null 2>&1 || {
  echo "prune-images: docker CLI not found in PATH; refusing to prune anything" >&2
  exit 1
}
if ! docker info >/dev/null 2>&1; then
  echo "prune-images: docker daemon is unreachable (docker info failed); refusing to prune anything" >&2
  exit 1
fi

prune_command=(docker image prune -a -f --filter "until=${keep_hours}h")

# docker system df 前后对比：prune 未报告回收量时，调用方仍可据前后差异交叉核对。
show_system_df() {
  printf 'image prune: docker system df (%s):\n' "$1"
  docker system df || echo 'WARNING: docker system df failed; continue without the disk snapshot' >&2
}

if [[ "$apply" == true ]]; then
  mode=apply
else
  mode=dry-run
fi
printf 'image prune: mode=%s keep-hours=%s\n' "$mode" "$keep_hours"
printf 'image prune: only unused images are touched; images used by running containers are protected by docker\n'
show_system_df before

if [[ "$apply" != true ]]; then
  printf 'image prune: would run: %s\n' "${prune_command[*]}"
  printf 'image prune: dry-run: nothing was deleted; re-run with --apply to remove unused images older than %sh\n' "$keep_hours"
  exit 0
fi

printf 'image prune: running: %s\n' "${prune_command[*]}"
prune_output=""
if ! prune_output="$("${prune_command[@]}" 2>&1)"; then
  printf '%s\n' "$prune_output" >&2
  echo "prune-images: docker image prune failed; inspect the output above (no rollback needed)" >&2
  exit 1
fi
printf '%s\n' "$prune_output"

# 优先解析 docker 自己报告的回收量；解析不到时退化为上面的前后 df 对比。
reclaimed="$(printf '%s\n' "$prune_output" | awk '/Total reclaimed space/ { sub(/^[^:]*:[[:space:]]*/, ""); print; exit }')"
show_system_df after
if [[ -n "$reclaimed" ]]; then
  printf 'image prune: reclaimed %s (reported as "Total reclaimed space" by docker)\n' "$reclaimed"
else
  echo 'image prune: docker did not report "Total reclaimed space"; compare the docker system df before/after output above' >&2
fi
echo "image prune: done (keep-hours=$keep_hours); re-running this tool is safe"
exit 0
