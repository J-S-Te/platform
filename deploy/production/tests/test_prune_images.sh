#!/usr/bin/env bash
# 镜像保留清理工具的隔离回归。
# 背景（2026-11-07 测试服务器真实事故）：发布反复拉取不可变 digest 镜像，旧镜像累积把
# 磁盘打到 100%，发布被 fail-closed 拒绝。
# 本测试只用 /tmp 假 docker（PATH 前置并记录 argv），禁止触碰真实 Docker、网络或磁盘。
set -Eeuo pipefail

root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../../.." && pwd)"
prune="$root/deploy/production/bin/prune-images.sh"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/uip-prune-images.XXXXXX")"
trap 'rm -rf -- "$test_root"' EXIT

fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
pass() { printf 'PASS: %s\n' "$*"; }

[[ -f "$prune" ]] || fail "missing prune tool: $prune"
[[ -x "$prune" ]] || fail "prune tool is not executable: $prune"
bash -n "$prune" || fail 'prune tool has a syntax error'

fake_bin="$test_root/fake-bin"
mkdir -p "$fake_bin"
fake_log="$test_root/docker-argv.log"

# 假 docker：只记录 argv 并返回稳定的 docker system df / prune 输出，不做任何真实操作。
cat >"$fake_bin/docker" <<'FAKE'
#!/usr/bin/env bash
set -Eeuo pipefail
printf '%s\n' "$*" >>"${FAKE_DOCKER_LOG:?FAKE_DOCKER_LOG is required}"
if [[ "${FAKE_DOCKER_NO_DAEMON:-false}" == true ]]; then
  echo 'Cannot connect to the Docker daemon at unix:///var/run/docker.sock' >&2
  exit 1
fi
case "${1:-}" in
  info) exit 0 ;;
  system)
    [[ "${2:-}" == df ]] || exit 0
    printf 'TYPE            TOTAL     ACTIVE    SIZE      RECLAIMABLE\n'
    printf 'Images          12        5         8.4GB     3.1GB (36%%)\n'
    exit 0
    ;;
  image)
    [[ "${2:-}" == prune ]] || exit 0
    printf 'Deleted Images:\nuntagged: sha256:deadbeef\n\nTotal reclaimed space: 1.5GB\n'
    exit 0
    ;;
esac
exit 0
FAKE
chmod 700 "$fake_bin/docker"

reset_log() { : >"$fake_log"; }
run_tool() { FAKE_DOCKER_LOG="$fake_log" PATH="$fake_bin:$PATH" bash "$prune" "$@"; }
image_prune_calls() { grep -c '^image prune' "$fake_log" || true; }
system_df_calls() { grep -c '^system df' "$fake_log" || true; }

# expect_exit：断言给定参数下的退出码；调用前应先 reset_log。
expect_exit() {
  local expected="$1" status=0
  shift
  run_tool "$@" >/dev/null 2>&1 || status=$?
  [[ "$status" -eq "$expected" ]] || fail "expected exit $expected for '$*', got $status"
}

# 1) 默认 dry-run：只打印 docker system df 与将执行的命令，绝不调用 image prune。
reset_log
dry_out="$(run_tool 2>&1)" || fail 'dry-run must exit 0'
grep -q 'docker system df' <<<"$dry_out" || fail 'dry-run must print a docker system df snapshot'
grep -q 'Images' <<<"$dry_out" || fail 'dry-run must show the docker system df table'
grep -q 'dry-run' <<<"$dry_out" || fail 'dry-run must state that nothing was deleted'
grep -q 'docker image prune -a -f --filter until=24h' <<<"$dry_out" || \
  fail 'dry-run must print the planned docker image prune command with the default 24h'
[[ "$(image_prune_calls)" -eq 0 ]] || fail 'dry-run must not call docker image prune'
[[ "$(system_df_calls)" -eq 1 ]] || fail 'dry-run must query docker system df exactly once'
grep -q '^info' "$fake_log" || fail 'dry-run must verify the docker daemon before reporting a plan'
pass 'dry-run prints docker system df and the planned command without pruning'

# 2) --apply：只调用一次 docker image prune，参数必须是 -a -f --filter until=24h。
reset_log
apply_out="$(run_tool --apply 2>&1)" || fail 'apply must exit 0'
[[ "$(image_prune_calls)" -eq 1 ]] || fail 'apply must call docker image prune exactly once'
grep -qx 'image prune -a -f --filter until=24h' "$fake_log" || \
  fail 'apply must pass: image prune -a -f --filter until=24h'
grep -q 'Total reclaimed space' <<<"$apply_out" || fail 'apply must surface the docker prune output'
grep -q 'reclaimed 1.5GB' <<<"$apply_out" || fail 'apply must report reclaimed bytes parsed from docker'
[[ "$(system_df_calls)" -eq 2 ]] || fail 'apply must print docker system df before and after pruning'
pass 'apply prunes once with -a -f --filter until=24h and reports reclaimed space'

# 3) --keep-hours 0：until=0h（清理所有未被容器使用的镜像）。
reset_log
run_tool --keep-hours 0 --apply >/dev/null 2>&1 || fail '--keep-hours 0 --apply must exit 0'
grep -qx 'image prune -a -f --filter until=0h' "$fake_log" || \
  fail '--keep-hours 0 must translate to until=0h'
pass '--keep-hours 0 translates to until=0h'

# 4) 非法保留值/未知参数：exit 2，且不触碰 docker。
reset_log
expect_exit 2 --keep-hours abc
expect_exit 2 --keep-hours -1
expect_exit 2 --keep-hours 1.5
expect_exit 2 --keep-hours
expect_exit 2 --bogus
[[ ! -s "$fake_log" ]] || fail 'invalid arguments must exit 2 before invoking docker'
pass 'invalid keep-hours and unknown arguments exit 2 without touching docker'

# 5) docker CLI 不存在：exit 1。用只含 bash 绝对路径的最小 PATH，避免命中真实 docker。
empty_bin="$test_root/no-docker-bin"
mkdir -p "$empty_bin"
status=0
FAKE_DOCKER_LOG="$fake_log" PATH="$empty_bin" "${BASH:-/bin/bash}" "$prune" --apply >/dev/null 2>&1 || status=$?
[[ "$status" -eq 1 ]] || fail "missing docker CLI must exit 1, got $status"
pass 'missing docker CLI exits 1'

# 6) daemon 不可达：exit 1，且不执行 prune。
reset_log
status=0
FAKE_DOCKER_LOG="$fake_log" FAKE_DOCKER_NO_DAEMON=true PATH="$fake_bin:$PATH" bash "$prune" --apply >/dev/null 2>&1 || status=$?
[[ "$status" -eq 1 ]] || fail "unreachable docker daemon must exit 1, got $status"
[[ "$(image_prune_calls)" -eq 0 ]] || fail 'unreachable daemon must not attempt docker image prune'
pass 'unreachable docker daemon exits 1 without pruning'

# 7) 幂等：重复 --apply 都是成功且各调用一次 prune。
reset_log
run_tool --apply >/dev/null 2>&1 || fail 'first --apply must exit 0'
run_tool --apply >/dev/null 2>&1 || fail 'repeated --apply must exit 0 (idempotent)'
[[ "$(image_prune_calls)" -eq 2 ]] || fail 'each --apply run must invoke docker image prune exactly once'
pass 'repeated --apply is safe (idempotent)'

# 8) 安全边界：脚本绝不使用 docker system prune，也不删除卷/容器。
# 静态检查只看以 docker 开头的命令行，避免把注释和 usage 文本里的“不要用 system prune”算成用法。
bad_docker_lines="$(grep -nE '^[[:space:]]*docker[[:space:]]' "$prune" \
  | grep -vE 'docker[[:space:]]+(info|system[[:space:]]+df)([[:space:]]|$)' || true)"
[[ -z "$bad_docker_lines" ]] || fail "unexpected docker invocation: $bad_docker_lines"
grep -qE '^[[:space:]]*prune_command=\(docker image prune -a -f --filter' "$prune" || \
  fail 'prune tool must build its command as docker image prune -a -f --filter'
# 运行时检查：假 docker 记录到的 argv 只允许 info / system df / image prune -a -f --filter。
bad_argv="$(grep -vE '^(info|system df|image prune -a -f --filter until=[0-9]+h)$' "$fake_log" || true)"
[[ -z "$bad_argv" ]] || fail "unexpected docker argv recorded: $bad_argv"
pass 'tool only uses docker info / system df / image prune (no system prune, no container/volume deletion)'

echo 'image prune tests: pass'
