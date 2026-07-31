#!/usr/bin/env bash
# 兼容旧版命令：转发到 subsystem.sh offboard。
# 新脚本是统一入口（onboard/update/offboard），推荐直接调用 subsystem.sh。
# 旧版仅删 DB 记录；新版默认深清理（停容器 + 删 .env.local + 删门户网关 + 删 DB）。
# 使用 --shallow 即可保持旧版语义。
# 保留本脚本仅用于自动化 / 文档中的旧调用路径。

set -Eeuo pipefail
IFS=$'\n\t'
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
exec bash "${SCRIPT_DIR}/subsystem.sh" offboard "$@"
