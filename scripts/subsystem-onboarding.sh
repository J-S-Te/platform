#!/usr/bin/env bash
# 兼容旧版命令：转发到 subsystem.sh onboard。
# 新脚本是统一入口（onboard/update/offboard），推荐直接调用 subsystem.sh。
# 保留本脚本仅用于自动化 / 文档中的旧调用路径。

set -Eeuo pipefail
IFS=$'\n\t'
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
exec bash "${SCRIPT_DIR}/subsystem.sh" onboard "$@"
