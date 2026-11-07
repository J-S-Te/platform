#!/usr/bin/env bash
# 备份保留清理工具的隔离回归。
# 背景（2026-11-06 测试服务器真实事故）：5 个仓并发部署时每次都写发布前数据库备份，
# /opt/basic-platform/backups 撑满磁盘，gzip 报 "No space left on device" 并拒绝 4 个仓发布。
# 本测试只用 /tmp 假备份目录，禁止触碰真实 backups、Docker、MySQL 或网络。
set -Eeuo pipefail

root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../../.." && pwd)"
prune="$root/deploy/production/bin/prune-backups.sh"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/uip-prune-backups.XXXXXX")"
trap 'rm -rf -- "$test_root"' EXIT

fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
pass() { printf 'PASS: %s\n' "$*"; }

[[ -f "$prune" ]] || fail "missing prune tool: $prune"
[[ -x "$prune" ]] || fail "prune tool is not executable: $prune"
bash -n "$prune" || fail 'prune tool has a syntax error'

# GNU date 与 BSD date 的差异只在测试夹具中处理；生产脚本保持 Linux 口径。
timestamp_days_ago() {
  local seconds
  seconds=$(( $(date +%s) - $1 * 86400 ))
  if date -u -r "$seconds" +%Y%m%d%H%M.%S >/dev/null 2>&1; then
    date -u -r "$seconds" +%Y%m%d%H%M.%S
  else
    date -u -d "@$seconds" +%Y%m%d%H%M.%S
  fi
}
set_age() { touch -t "$(timestamp_days_ago "$2")" "$1"; }
set_age_tree() {
  local stamp
  stamp="$(timestamp_days_ago "$2")"
  find "$1" -exec touch -t "$stamp" {} +
}

deploy=""
new_deploy_case() {
  deploy="$test_root/$1/deploy"
  mkdir -p "$deploy/backups"
}

# 灾备批次：backup-all.sh 的真实布局（MANIFEST + SHA256SUMS + sql/tar/清单 + file-gateway 子目录）。
system_group() { # name age_days
  local dir="$deploy/backups/system/$1"
  mkdir -p "$dir/file-gateway"
  printf 'FORMAT=2\n' >"$dir/MANIFEST"
  printf 'deadbeef  MANIFEST\n' >"$dir/SHA256SUMS"
  printf 'VERSION=1\n' >"$dir/VERSION_MANIFEST"
  printf 'dump\n' | gzip -c >"$dir/platform-mysql.sql.gz"
  printf 'archive\n' | gzip -c >"$dir/recovery-state.tar.gz"
  printf 'gateway\n' | gzip -c >"$dir/file-gateway/database.sql.gz"
  chmod 700 "$dir"
  set_age_tree "$dir" "$2"
}

# 发布前数据库备份：deploy-service.sh 的真实布局（<label>-<release>.sql.gz + .sha256）。
release_group() { # name age_days
  local file="$deploy/backups/$1.sql.gz"
  printf 'dump\n' | gzip -c >"$file"
  printf 'deadbeef  %s\n' "$1.sql.gz" >"$file.sha256"
  set_age "$file" "$2"
  set_age "$file.sha256" "$2"
}

# Keycloak 备份：sql.gz 与摘要/位点 sidecar 必须作为同一个组一起处置。
keycloak_group() { # name age_days
  local dir="$deploy/backups/keycloak" file
  mkdir -p "$dir"
  file="$dir/$1.sql.gz"
  printf 'dump\n' | gzip -c >"$file"
  printf 'deadbeef  %s.sql.gz\n' "$1" >"$file.sha256"
  printf 'FORMAT=1\nLOG_BIN=0\n' >"$file.binlog-position"
  printf 'deadbeef  %s.sql.gz.binlog-position\n' "$1" >"$file.binlog-position.sha256"
  local entry
  for entry in "$file" "$file.sha256" "$file.binlog-position" "$file.binlog-position.sha256"; do
    set_age "$entry" "$2"
  done
}

# 独立 File Gateway 快照：<UTC stamp>/ 顶层目录即一份。
gateway_group() { # stamp age_days
  local dir="$deploy/backups/file-gateway/$1"
  mkdir -p "$dir"
  printf 'FORMAT=2\n' >"$dir/MANIFEST"
  printf 'deadbeef  database.sql.gz\n' >"$dir/SHA256SUMS"
  printf 'dump\n' | gzip -c >"$dir/database.sql.gz"
  printf 'files\n' | gzip -c >"$dir/files.tar.gz"
  set_age_tree "$dir" "$2"
}

assert_present() { [[ -e "$1" || -L "$1" ]] || fail "expected present: $1"; }
assert_absent() { [[ ! -e "$1" && ! -L "$1" ]] || fail "expected absent: $1"; }

status=0
output=""
run_prune() {
  local log="$1"
  shift
  set +e
  output="$(bash "$prune" "$@" 2>&1)"
  status=$?
  set -e
  printf '%s\n' "$output" >"$log"
}
assert_status() {
  [[ "$status" == "$1" ]] || { printf '%s\n' "$output" >&2; fail "expected exit $1, got $status ($2)"; }
}
assert_log() {
  grep -Fq -- "$2" "$1" || { cat "$1" >&2; fail "$3"; }
}
assert_log_re() {
  grep -Eq -- "$2" "$1" || { cat "$1" >&2; fail "$3"; }
}

# 1. 保留天数 + 保留份数：同时满足才删除，dry-run 默认不动文件，--apply 才真删。
new_deploy_case retention
release_group app-a 30
release_group app-b 20
release_group app-c 5
release_group app-d 4
release_group app-e 3
release_group app-f 2
release_group app-g 1
mkdir -p "$deploy/backups/releases"
printf 'OLD_RELEASE=1\n' >"$deploy/backups/releases/old.env"
set_age "$deploy/backups/releases/old.env" 90
printf 'operator notes\n' >"$deploy/backups/notes.txt"
set_age "$deploy/backups/notes.txt" 90

run_prune "$test_root/retention.dry.log" --deploy-dir "$deploy"
assert_status 0 'default dry-run'
assert_log "$test_root/retention.dry.log" 'dry-run' 'dry-run must be the default and be reported'
assert_log_re "$test_root/retention.dry.log" '^DELETE .*app-a' 'age > keep-days must be planned for deletion'
assert_log_re "$test_root/retention.dry.log" '^DELETE .*app-b' 'age > keep-days must be planned for deletion'
if grep -Eq '^DELETE .*app-[cdefg]' "$test_root/retention.dry.log"; then
  cat "$test_root/retention.dry.log" >&2
  fail 'groups newer than keep-days must not be deleted'
fi
for name in app-a app-b app-c app-d app-e app-f app-g; do
  assert_present "$deploy/backups/$name.sql.gz"
  assert_present "$deploy/backups/$name.sql.gz.sha256"
done
pass 'default dry-run prints the plan without deleting anything'

run_prune "$test_root/retention.apply.log" --deploy-dir "$deploy" --apply
assert_status 0 'apply retention'
assert_absent "$deploy/backups/app-a.sql.gz"
assert_absent "$deploy/backups/app-a.sql.gz.sha256"
assert_absent "$deploy/backups/app-b.sql.gz"
assert_absent "$deploy/backups/app-b.sql.gz.sha256"
for name in app-c app-d app-e app-f app-g; do
  assert_present "$deploy/backups/$name.sql.gz"
  assert_present "$deploy/backups/$name.sql.gz.sha256"
done
assert_present "$deploy/backups/releases/old.env"
assert_present "$deploy/backups/notes.txt"
assert_log "$test_root/retention.apply.log" 'disk before' 'apply must print df -h before deleting'
assert_log "$test_root/retention.apply.log" 'disk after' 'apply must print df -h after deleting'
assert_log_re "$test_root/retention.apply.log" 'deleted 2 group' 'apply must report the deleted group count'
assert_log_re "$test_root/retention.apply.log" 'freed [0-9]+ KiB' 'apply must report freed space'
pass 'keep-days and keep-count must both allow deletion; .sha256 travels with each group'

# 2. 最近 keep-count 份必须保留，即使它们早于 keep-days。
new_deploy_case keep-count
release_group old-1 40
release_group old-2 35
release_group old-3 30
release_group old-4 25
run_prune "$test_root/keep-count.log" --deploy-dir "$deploy" --keep-count 2 --apply
assert_status 0 'keep-count protection'
assert_absent "$deploy/backups/old-1.sql.gz"
assert_absent "$deploy/backups/old-2.sql.gz"
assert_present "$deploy/backups/old-3.sql.gz"
assert_present "$deploy/backups/old-4.sql.gz"
assert_present "$deploy/backups/old-3.sql.gz.sha256"
assert_present "$deploy/backups/old-4.sql.gz.sha256"
pass 'the newest keep-count groups are always retained'

# 3. 组完整性：灾备批次目录、Keycloak 摘要/位点 sidecar、File Gateway 批次必须整组处理。
new_deploy_case integrity
release_group newest 1
system_group 20250101T000000Z-100 30
keycloak_group keycloak-20250101T000000Z 40
gateway_group 20250101T000000Z 35
run_prune "$test_root/integrity.log" --deploy-dir "$deploy" --keep-count 1 --apply
assert_status 0 'group integrity'
assert_present "$deploy/backups/newest.sql.gz"
assert_absent "$deploy/backups/system/20250101T000000Z-100"
assert_absent "$deploy/backups/system/20250101T000000Z-100/platform-mysql.sql.gz"
assert_absent "$deploy/backups/keycloak/keycloak-20250101T000000Z.sql.gz"
assert_absent "$deploy/backups/keycloak/keycloak-20250101T000000Z.sql.gz.sha256"
assert_absent "$deploy/backups/keycloak/keycloak-20250101T000000Z.sql.gz.binlog-position"
assert_absent "$deploy/backups/keycloak/keycloak-20250101T000000Z.sql.gz.binlog-position.sha256"
assert_absent "$deploy/backups/file-gateway/20250101T000000Z"
assert_absent "$deploy/backups/file-gateway/20250101T000000Z/files.tar.gz"
pass 'a backup group is deleted as one unit, including sidecars and nested directories'

# 4. 非备份文件、运行配置快照和不完整的组不得删除。
new_deploy_case protected
release_group old 30
release_group newest 1
printf 'dump\n' | gzip -c >"$deploy/backups/incomplete-20250101T000000Z.sql.gz"
set_age "$deploy/backups/incomplete-20250101T000000Z.sql.gz" 30
mkdir -p "$deploy/backups/system/20250101T000000Z-200"
printf 'FORMAT=2\n' >"$deploy/backups/system/20250101T000000Z-200/MANIFEST"
set_age_tree "$deploy/backups/system/20250101T000000Z-200" 30
mkdir -p "$deploy/backups/configuration-20250101-000000.ABCDEF"
printf 'runtime config\n' >"$deploy/backups/configuration-20250101-000000.ABCDEF/.env"
set_age_tree "$deploy/backups/configuration-20250101-000000.ABCDEF" 90
mkdir -p "$deploy/backups/install-assets.ABCDEF"
printf 'asset config\n' >"$deploy/backups/install-assets.ABCDEF/.env"
set_age_tree "$deploy/backups/install-assets.ABCDEF" 90
mkdir -p "$deploy/backups/releases"
printf 'OLD=1\n' >"$deploy/backups/releases/old.env"
set_age "$deploy/backups/releases/old.env" 90
run_prune "$test_root/protected.log" --deploy-dir "$deploy" --keep-count 1 --apply
assert_status 0 'protected entries'
assert_absent "$deploy/backups/old.sql.gz"
assert_present "$deploy/backups/incomplete-20250101T000000Z.sql.gz"
assert_present "$deploy/backups/system/20250101T000000Z-200/MANIFEST"
assert_present "$deploy/backups/configuration-20250101-000000.ABCDEF/.env"
assert_present "$deploy/backups/install-assets.ABCDEF/.env"
assert_present "$deploy/backups/releases/old.env"
assert_log "$test_root/protected.log" 'SKIP' 'skipped entries must be reported'
pass 'non-backup files, runtime config snapshots and incomplete groups are never deleted'

# 5. 非法参数必须失败关闭，且不得删除任何文件。
new_deploy_case invalid
release_group guarded 30
for bad_args in '--keep-days -1' '--keep-days abc' '--keep-days' '--keep-count 0' '--keep-count -2' '--keep-count abc' '--bogus' '--deploy-dir relative/path'; do
  # shellcheck disable=SC2086 # 参数刻意按空格拆分
  run_prune "$test_root/invalid.log" --deploy-dir "$deploy" $bad_args
  [[ "$status" != 0 ]] || { cat "$test_root/invalid.log" >&2; fail "invalid arguments must fail: $bad_args"; }
  assert_present "$deploy/backups/guarded.sql.gz"
done
run_prune "$test_root/invalid.log" --deploy-dir "$test_root/does-not-exist" --apply
[[ "$status" != 0 ]] || fail 'a missing deploy directory must fail'
run_prune "$test_root/invalid.log" --deploy-dir "$deploy" --keep-days 7 --keep-count 5 --apply
assert_status 0 'valid arguments'
pass 'invalid or unsafe arguments fail closed without deleting anything'

# 6. 拒绝符号链接与 deploy-dir 之外的路径；外部目标必须保持不变。
new_deploy_case symlink
outside="$test_root/symlink/outside"
mkdir -p "$outside/group-target"
printf 'do not delete\n' >"$outside/victim.dat"
printf 'do not delete either\n' >"$outside/group-target/payload.dat"
release_group newest 1
printf 'deadbeef  evil.sql.gz\n' >"$deploy/backups/evil-20250101T000000Z.sql.gz.sha256"
mkdir -p "$deploy/backups/system"
ln -s "$outside/victim.dat" "$deploy/backups/evil-20250101T000000Z.sql.gz"
ln -s "$outside/group-target" "$deploy/backups/system/20250101T000000Z-300"
run_prune "$test_root/symlink.log" --deploy-dir "$deploy" --apply
assert_status 0 'symlink handling'
assert_present "$outside/victim.dat"
assert_present "$outside/group-target/payload.dat"
assert_present "$deploy/backups/evil-20250101T000000Z.sql.gz"
assert_present "$deploy/backups/evil-20250101T000000Z.sql.gz.sha256"
assert_present "$deploy/backups/system/20250101T000000Z-300"
assert_log "$test_root/symlink.log" 'symbolic link' 'symlinks must be reported and skipped'
pass 'symbolic links are skipped and never followed'

new_deploy_case symlink-root
outside="$test_root/symlink-root/outside"
mkdir -p "$outside"
printf 'do not delete\n' >"$outside/victim.dat"
rmdir "$deploy/backups"
ln -s "$outside" "$deploy/backups"
run_prune "$test_root/symlink-root.log" --deploy-dir "$deploy" --apply
[[ "$status" != 0 ]] || fail 'a symbolic-link backups root must be rejected'
assert_present "$outside/victim.dat"
pass 'a symbolic-link backups root is rejected before any deletion'

# 7. 幂等：第二次 --apply 不重复删除、不报错。
new_deploy_case idempotent
release_group stale 30
release_group fresh 1
run_prune "$test_root/idempotent.1.log" --deploy-dir "$deploy" --keep-count 1 --apply
assert_status 0 'first apply'
assert_absent "$deploy/backups/stale.sql.gz"
run_prune "$test_root/idempotent.2.log" --deploy-dir "$deploy" --keep-count 1 --apply
assert_status 0 'second apply'
assert_log_re "$test_root/idempotent.2.log" 'deleted 0 group' 'a second run must delete nothing'
assert_present "$deploy/backups/fresh.sql.gz"
assert_present "$deploy/backups/fresh.sql.gz.sha256"
pass 'the prune tool is idempotent'

# 8. 没有 backups 目录时是无害空操作。
deploy="$test_root/empty/deploy"
mkdir -p "$deploy"
run_prune "$test_root/empty.log" --deploy-dir "$deploy"
assert_status 0 'missing backups directory'
assert_log "$test_root/empty.log" 'nothing to do' 'a missing backups directory must be reported as a no-op'
pass 'a deploy dir without backups is a successful no-op'

echo 'ALL PASS: prune-backups retention/safety'
