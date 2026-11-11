#!/usr/bin/env bash
# 发布前备份目录可访问性自检（2026-11-07 属主漂移事故回归）。
# 从 deploy-service.sh 抽出真实函数执行，避免复制一份实现形成两套语义。
set -Eeuo pipefail
ROOT="$(cd -- "$(dirname -- "$0")/.." && pwd)"
SCRIPT="$ROOT/bin/deploy-service.sh"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/backup-access-test.XXXXXX")"
trap 'chmod -R u+rwx "$WORK" 2>/dev/null || true; rm -rf -- "$WORK"' EXIT
pass=0; fail=0
check() { if eval "$2"; then echo "PASS: $1"; pass=$((pass+1)); else echo "FAIL: $1"; fail=$((fail+1)); fi; }

# 抽出函数定义后在子 shell 里执行（函数依赖全局 deploy_dir）。
fn_file="$WORK/fn.sh"
awk '/^require_backup_tree_accessible\(\) \{/{flag=1} flag{print} flag && /^\}/{exit}' "$SCRIPT" >"$fn_file"
check "function extracted from deploy-service.sh" "[[ -s \"$fn_file\" ]]"

run_check() {
  ( set -Eeuo pipefail; deploy_dir="$1"; source "$fn_file"; require_backup_tree_accessible ) 2>"$WORK/err.log"
}

# 1) 全部可读写：通过
deploy_dir="$WORK/ok"; mkdir -p "$deploy_dir/backups/system" "$deploy_dir/backups/file-gateway"
check "accessible backups passes" "run_check '$deploy_dir'"

# 1b) 普通文件 600（无执行位）：不得误报为不可访问
deploy_dir="$WORK/files"; mkdir -p "$deploy_dir/backups"; : >"$deploy_dir/backups/portal-20260101T000000Z.sql.gz"; chmod 600 "$deploy_dir/backups/portal-20260101T000000Z.sql.gz"
check "readable 600 file passes" "run_check '$deploy_dir'"

# 1c) 600 的遗留备份文件在多层目录里也不得拦发布（历史 onboarding 产物）
deploy_dir="$WORK/legacy"; mkdir -p "$deploy_dir/backups/system/20260101T000000Z-1"
: >"$deploy_dir/backups/system/20260101T000000Z-1/lineage-onboarding.sql.backup"; chmod 600 "$deploy_dir/backups/system/20260101T000000Z-1/lineage-onboarding.sql.backup"
check "legacy 600 file in nested dir passes" "run_check '$deploy_dir'"

# 2) 子目录 000：失败且给出修复提示（等价于属主/权限漂移）
deploy_dir="$WORK/bad"; mkdir -p "$deploy_dir/backups/file-gateway"; chmod 000 "$deploy_dir/backups/file-gateway"
check "unreadable child fails" "! run_check '$deploy_dir'"
check "failure message names remediation" "grep -q 'chown -R' '$WORK/err.log'"
chmod 700 "$deploy_dir/backups/file-gateway"

# 3) 无 backups 目录：视为首次安装，通过
deploy_dir="$WORK/none"; mkdir -p "$deploy_dir"
check "missing backups directory passes" "run_check '$deploy_dir'"

echo "backup access preflight tests: pass=$pass fail=$fail"
[[ "$fail" -eq 0 ]]
