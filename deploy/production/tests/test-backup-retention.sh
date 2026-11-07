#!/usr/bin/env bash
# backup-all.sh 备份保留策略的接线回归：env 解析、默认值、非法值回落与失败不改变发布结论。
# 使用 prune 桩，不接触 Docker/MySQL/真实 backups。
set -Eeuo pipefail

root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../../.." && pwd)"
backup_all="$root/deploy/production/bin/backup-all.sh"
prune="$root/deploy/production/bin/prune-backups.sh"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/uip-backup-retention.XXXXXX")"
trap 'rm -rf -- "$test_root"' EXIT

fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
pass() { printf 'PASS: %s\n' "$*"; }

[[ -f "$backup_all" ]] || fail "missing backup-all.sh: $backup_all"
bash -n "$backup_all" || fail 'backup-all.sh has a syntax error'
bash -n "$prune" || fail 'prune-backups.sh has a syntax error'

extract_function() {
  local name="$1" file="$2"
  awk -v signature="${name}() {" '
    $0 == signature {inside=1}
    inside {print}
    inside && $0 == "}" {exit}
  ' "$file"
}

body="$(extract_function run_backup_retention "$backup_all")"
[[ -n "$body" ]] || fail 'backup-all.sh does not define run_backup_retention'
eval "$body"

stub_dir="$test_root/stub"
mkdir -p "$stub_dir"
deploy_dir="$test_root/deploy"
mkdir -p "$deploy_dir/backups/system"
batch="$deploy_dir/backups/system/20261106T000000Z-100"
calls="$test_root/prune.calls"
export PRUNE_CALLS="$calls"
cat >"$stub_dir/prune-backups.sh" <<'SH'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$PRUNE_CALLS"
exit "${PRUNE_EXIT:-0}"
SH
chmod +x "$stub_dir/prune-backups.sh"
script_dir="$stub_dir"

status=0
output=""
run_retention() {
  : >"$calls"
  set +e
  output="$(run_backup_retention 2>&1)"
  status=$?
  set -e
}
assert_calls_empty() {
  [[ ! -s "$calls" ]] || { cat "$calls" >&2; fail "$1"; }
}
assert_log() {
  grep -Fq -- "$1" <<<"$output" || { printf '%s\n' "$output" >&2; fail "$2"; }
}
assert_log_re() {
  grep -Eq -- "$1" <<<"$output" || { printf '%s\n' "$output" >&2; fail "$2"; }
}

# 1. 默认策略：7 天 / 最少 5 份，成功后以 --apply 调用 prune。
unset BACKUP_RETENTION_DAYS BACKUP_RETENTION_MIN_COUNT 2>/dev/null || true
run_retention
[[ "$status" == 0 ]] || { printf '%s\n' "$output" >&2; fail 'default retention must succeed'; }
grep -Fxq -- "--deploy-dir $deploy_dir --keep-days 7 --keep-count 5 --apply" "$calls" ||
  { cat "$calls" >&2; fail 'default policy did not call prune with 7 days / 5 groups / --apply'; }
assert_log 'keep-days=7' 'retention log must state keep-days'
assert_log 'keep-count=5' 'retention log must state keep-count'
pass 'defaults are 7 days / 5 groups and prune is invoked with --apply'

# 2. 环境变量覆盖必须原样传给 prune。
BACKUP_RETENTION_DAYS=3 BACKUP_RETENTION_MIN_COUNT=2 run_retention
[[ "$status" == 0 ]] || fail 'overridden retention must succeed'
grep -Fxq -- "--deploy-dir $deploy_dir --keep-days 3 --keep-count 2 --apply" "$calls" ||
  { cat "$calls" >&2; fail 'override policy was not forwarded to prune'; }
pass 'BACKUP_RETENTION_DAYS/BACKUP_RETENTION_MIN_COUNT overrides are forwarded'

# 3. days <= 0 关闭清理，且不得调用 prune。
for disabled in 0 -1; do
  BACKUP_RETENTION_DAYS="$disabled" run_retention
  [[ "$status" == 0 ]] || fail "disabled retention ($disabled) must succeed"
  assert_calls_empty "retention must not run when BACKUP_RETENTION_DAYS=$disabled"
  assert_log 'disabled' "disabled retention ($disabled) must be logged"
done
pass 'BACKUP_RETENTION_DAYS <= 0 disables retention'

# 4. 非法值 WARN 并回落默认，不阻断发布。
BACKUP_RETENTION_DAYS=seven BACKUP_RETENTION_MIN_COUNT=0 run_retention
[[ "$status" == 0 ]] || fail 'invalid env values must not fail the release'
grep -Fxq -- "--deploy-dir $deploy_dir --keep-days 7 --keep-count 5 --apply" "$calls" ||
  { cat "$calls" >&2; fail 'invalid env values did not fall back to defaults'; }
assert_log_re 'WARNING: invalid BACKUP_RETENTION_DAYS' 'invalid days must warn'
assert_log_re 'WARNING: invalid BACKUP_RETENTION_MIN_COUNT' 'invalid count must warn'
BACKUP_RETENTION_MIN_COUNT=-3 run_retention
assert_log_re 'WARNING: invalid BACKUP_RETENTION_MIN_COUNT' 'negative count must warn and fall back'
grep -Fq -- '--keep-count 5' "$calls" || fail 'negative count did not fall back to 5'
pass 'invalid retention env values warn and fall back to defaults'

# 5. prune 失败只 WARN，不改变发布/备份结论。
PRUNE_EXIT=1 run_retention
[[ "$status" == 0 ]] || fail 'a failing prune must not change the release result'
assert_log_re 'WARNING:.*prune|WARNING:.*retention|WARNING:.*清理' 'prune failure must be reported as a warning'
grep -Fq -- '--apply' "$calls" || fail 'prune was not invoked before its failure was warned'
pass 'a failing prune is a warning, never a release failure'

# 6. 静态接线：只在新备份校验并落盘之后调用，且不早于成功路径。
call_line="$(grep -n '^run_backup_retention$' "$backup_all" | head -n1 | cut -d: -f1)"
[[ -n "$call_line" ]] || fail 'run_backup_retention is never called'
[[ "$(grep -c '^run_backup_retention$' "$backup_all")" == 1 ]] || fail 'run_backup_retention must be called exactly once'
verify_line="$(grep -n '^verify_disaster_batch "\$work"$' "$backup_all" | head -n1 | cut -d: -f1)"
commit_line="$(grep -n '^mv "\$work" "\$batch"$' "$backup_all" | head -n1 | cut -d: -f1)"
[[ -n "$verify_line" && -n "$commit_line" ]] || fail 'backup-all success path anchors are missing'
[[ "$call_line" -gt "$verify_line" && "$call_line" -gt "$commit_line" ]] ||
  fail 'retention must run only after the new backup is verified and committed'
grep -Fq 'BACKUP_RETENTION_DAYS' "$backup_all" || fail 'BACKUP_RETENTION_DAYS is not wired'
grep -Fq 'BACKUP_RETENTION_MIN_COUNT' "$backup_all" || fail 'BACKUP_RETENTION_MIN_COUNT is not wired'
grep -Fq 'prune-backups.sh' "$backup_all" || fail 'backup-all does not call prune-backups.sh'
pass 'retention is wired only to the verified backup success path'

echo 'ALL PASS: backup retention wiring'
