#!/usr/bin/env bash
# Build and export the basic-platform backend image (including file-gateway)
# using the production offline-package format consumed by deploy.sh import.
set -Eeuo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
platform_root="$(cd -- "$script_dir/.." && pwd)"
builder="$platform_root/deploy/production/bin/build-offline-packages.sh"
version="${OFFLINE_RELEASE_VERSION:-$(date -u +%Y%m%dT%H%M%SZ)}"
output="${OFFLINE_OUTPUT_DIR:-$platform_root/../.artifacts/offline/$version}"

usage() {
  cat <<'EOF'
用法：package-project-image.sh [--version 版本] [--output 输出目录]

从当前项目源码构建基础平台后端镜像（包含 /app/file-gateway），并生成
可由生产 deploy.sh import 导入的 linux/amd64 离线镜像包及 SHA256 校验文件。

选项：
  --version 版本号   默认使用 UTC 时间戳，也可通过 OFFLINE_RELEASE_VERSION 设置
  --output 目录      输出目录；默认写入 .artifacts/offline/<版本号>
  -h, --help         显示帮助

示例：
  ./platform/scripts/package-project-image.sh --version 1.2.3
  ./platform/scripts/package-project-image.sh --version 1.2.3 --output /tmp/uip-release
EOF
}

while (($#)); do
  case "$1" in
    --version) (($# >= 2)) || { echo '缺少 --version 的参数' >&2; exit 2; }; version="$2"; shift 2 ;;
    --output) (($# >= 2)) || { echo '缺少 --output 的参数' >&2; exit 2; }; output="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) usage >&2; exit 2 ;;
  esac
done

[[ "$version" =~ ^[A-Za-z0-9._-]+$ ]] || { echo '版本号仅允许字母、数字、点、下划线和连字符' >&2; exit 2; }
[[ -f "$builder" ]] || { echo "找不到离线镜像构建器：$builder" >&2; exit 1; }

exec "$builder" --component platform --version "$version" --output "$output"
