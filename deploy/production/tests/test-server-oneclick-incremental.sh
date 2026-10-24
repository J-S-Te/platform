#!/usr/bin/env bash
set -Eeuo pipefail

# 生成真实 OCI 包和完整配置；仅宿主机操作使用替身。
tests_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
exec python3 "$tests_dir/test-server-oneclick-incremental.py" "$@"
