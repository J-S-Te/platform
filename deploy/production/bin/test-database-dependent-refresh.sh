#!/usr/bin/env bash
set -Eeuo pipefail

# test-database-dependent-refresh.sh 覆盖"数据库容器被重建后刷新强依赖"的逻辑。
#
# 现场事故：compose 只重建配置变化的数据库容器，不会重启持有旧 Hikari 连接的 keycloak；
# keycloak-db 已是新容器、keycloak 仍是旧容器，健康探针只测 TCP 而继续报告 healthy，
# 请求处理被阻塞 20 分钟以上。本测试用真实的 deploy-service.sh 函数体（提取，不复制）
# 加 compose/docker 桩，验证重建判定、映射表、--force-recreate/--no-deps 和证据行。
#
# 覆盖以下容易被现场推翻的判定：
#   - 跨轮次持久化状态（runtime/database-refresh-state）：第一轮发布在记录之前中止，
#     第二轮快照已经看不到重建，仍必须强制重建 keycloak；第三轮不得反复抖动。
#   - 数据库身份探测 fail-closed（X1）：一次瞬时探测失败不能让"未知"退化成"未重建"，
#     连续 3 次失败必须中止平台发布，且绝不记录该数据库的状态。
#   - 强依赖启用范围只解析一次（X2）：清单解析失败、强依赖存在性探测失败都不得被说成
#     "未启用/未部署"，也不得借此记录状态；两者都必须留下持久失败标记。
#   - 持久失败标记（X3）：runtime/database-refresh-failed.json 的 schema、0600 权限、
#     原子替换和"拒绝写穿符号链接"；下一次全成功发布必须删除它。
#   - refresh 处置只告警不中止平台发布（业务子系统不能硬门禁平台发布），且失败的那条
#     数据库不记录状态，下一次发布仍会重试。
#   - 强依赖存在性用 ps -a 判定（已退出容器也要修复）；所属子系统未启用时绝不 up。
#
# 运行环境：bash 4+。所加载的 deploy.sh 及 bin/ 脚本使用 mapfile 等 bash 4 特性，
# macOS 自带 bash 3.2 不足，请在 bash:5 容器内运行（例如
# docker run --rm -v <workspace>:/ws:ro -w /tmp python:3.12-slim-bookworm bash -lc
#   'cp -a /ws/platform/deploy/production /tmp/prod && cd /tmp/prod && bash bin/test-database-dependent-refresh.sh'）。

source_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/database-dependent-refresh-test.XXXXXX")"
# UIP_TEST_KEEP_ARTIFACTS=1 保留每个用例的 compose 调用轨迹与证据输出，便于现场复盘。
trap 'if [[ "${UIP_TEST_KEEP_ARTIFACTS:-0}" == 1 ]]; then printf "保留测试证据目录：%s\n" "$test_root" >&2; else rm -r -- "$test_root"; fi' EXIT
mkdir -p "$test_root/deploy/bin" "$test_root/deploy/manifests" "$test_root/deploy/packages"
cp "$source_dir/bin/deploy.sh" "$source_dir/bin/compose-scope.sh" "$source_dir/bin/subsystem-runtime.py" "$source_dir/bin/start-enabled.sh" "$source_dir/bin/project-background.sh" "$source_dir/bin/provisioner-config-refresh.sh" "$source_dir/bin/offline-configure.sh" "$source_dir/bin/offline-package-metadata.sh" "$source_dir/bin/public-transport.sh" "$test_root/deploy/bin/"
cp "$source_dir"/bin/deploy-{common,import,subsystem,license,doctor,package,status,verify,menu,registry}.sh "$test_root/deploy/bin/"
cp "$source_dir/docker-compose.yml" "$test_root/deploy/"
cp -R "$source_dir/subsystems.d" "$test_root/deploy/"
: > "$test_root/deploy/.env"
: > "$test_root/deploy/.release.env"
chmod 600 "$test_root/deploy/.env" "$test_root/deploy/.release.env"
mkdir -p "$test_root/deploy/data/file-gateway"
source "$test_root/deploy/bin/deploy.sh"

fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
pass() { printf 'PASS: %s\n' "$*"; }

# 被测函数从 deploy-service.sh 原样提取：#3 可给提取出来的函数改名（用于同时保留
# 测试桩与真实实现，例如 temporal_consumers_enabled）。
extract_function() {
  local name="$1" file="$2" target="${3:-$1}" signature target_signature
  signature="${name}() {"
  target_signature="${target}() {"
  awk -v signature="$signature" -v target="$target_signature" '
    $0 == signature {inside=1; print target; next}
    inside {print}
    inside && $0 == "}" {exit}
  ' "$file"
}

for function_name in \
  database_container_id_snapshot \
  database_container_current_id_strict \
  database_snapshot_container_id \
  database_container_recreated \
  database_refresh_state_file \
  database_refresh_state_read \
  database_refresh_state_record \
  database_refresh_state_record_current \
  database_refresh_state_stale \
  database_refresh_failure_marker_file \
  database_refresh_json_string \
  database_refresh_failure_note \
  database_refresh_failure_marker_write \
  database_refresh_failure_marker_clear \
  database_dependent_disposition \
  database_dependent_scope_component \
  database_dependent_kept_running \
  refresh_recreated_database_dependents \
  deploy_platform; do
  function_definition="$(extract_function "$function_name" "$source_dir/bin/deploy-service.sh")"
  [[ -n "$function_definition" ]] || fail "无法从 deploy-service.sh 提取 $function_name"
  [[ "$(printf '%s\n' "$function_definition" | tail -n 1)" == '}' ]] ||
    fail "$function_name 的函数体提取不完整"
  eval "$function_definition"
done
# 用例10验证 D5 的真实实现（唯一实现在 compose-scope.sh，两侧共用），因此改名提取，避免覆盖下面的测试桩。
function_definition="$(extract_function temporal_consumers_enabled "$source_dir/bin/compose-scope.sh" temporal_consumers_enabled_real)"
[[ -n "$function_definition" ]] || fail '无法从 compose-scope.sh 提取 temporal_consumers_enabled'
eval "$function_definition"
declare -F deploy_platform >/dev/null || fail 'deploy_platform 未被定义'
debug_dependent_logic="$(declare -f refresh_recreated_database_dependents)"
grep -Fq -- '--no-deps' <<<"$debug_dependent_logic" || fail '强依赖刷新缺失 --no-deps'
grep -Fq -- '--force-recreate' <<<"$debug_dependent_logic" || fail '强依赖刷新缺失 --force-recreate'

# ---- mock 环境：deploy_platform 依赖的容器与外部副作用全部替换 ----
case_index=0
run_index=0
trace_file="$test_root/compose.trace"
log_file="$test_root/deploy.log"
state_dir="$test_root/container-ids"
refresh_state_file="$test_root/deploy/runtime/database-refresh-state"
refresh_failure_marker="$test_root/deploy/runtime/database-refresh-failed.json"
mkdir -p "$state_dir"
recreated_services=""
container_serial=0
temporal_consumers=disabled
# docker_services 是 docker compose ... config --services 的输出，决定 scope_enabled 的结论。
docker_services='platform-api keycloak contract-api contract-worker'
docker_services_error=0
fail_up_services=""
fail_at=""
# 探测重试间隔覆盖为 0：用例要真的触发 3 次重试，但不该把测试拖成秒级等待。
# 重试次数仍保持生产默认 3 次（database_probe_attempts 未设置时取 3）。
database_probe_retry_delay=0

# begin_run 为同一用例内的多次发布切换独立轨迹/日志（重试类用例需要逐轮核对），
# 文件名中的 runN 与该用例内第 N 轮发布一一对应。
begin_run() {
  run_index=$((run_index + 1))
  trace_file="$test_root/compose.${case_index}.run${run_index}.trace"
  log_file="$test_root/deploy.${case_index}.run${run_index}.log"
  : >"$trace_file"
  : >"$log_file"
}

# reset_fixture 重置容器 ID、重建清单与跨轮次状态文件，并为每个用例切换独立的
# trace/日志文件，这样"未变化就不重启"可以直接对着轨迹逐条核对。
reset_fixture() {
  rm -rf -- "$state_dir"
  mkdir -p "$state_dir"
  case_index=$((case_index + 1))
  run_index=0
  trace_file="$test_root/compose.${case_index}.trace"
  log_file="$test_root/deploy.${case_index}.log"
  : >"$trace_file"
  : >"$log_file"
  recreated_services=""
  container_serial=0
  temporal_consumers=disabled
  docker_services='platform-api keycloak contract-api contract-worker'
  docker_services_error=0
  fail_up_services=""
  fail_at=""
  rm -f -- "$refresh_state_file"
  rm -f -- "$refresh_failure_marker"
}

# set_container_id 预置"本轮启动前"的容器 ID；不预置表示该服务尚未部署（首次安装）。
set_container_id() {
  printf '%s\n' "$2" >"$state_dir/$1"
  printf '%s\n' "$2" >"$state_dir/$1.running"
}

# set_exited 模拟"容器已存在但已退出"：ps -a -q 仍可查到，ps -q 查不到。
set_exited() { rm -f -- "$state_dir/$1.running"; }

# set_ps_failures 让某个服务的 compose ps 探测先失败 N 次（999 表示持久失败），
# 用来复现"瞬时探测故障被当成未重建/未部署"的现场缺陷（X1/X2）。
set_ps_failures() {
  printf '%s\n' "$2" >"$state_dir/$1.psfail"
}

# mark_recreated 模拟 compose 只重建配置变化的容器：仅对记录在案的服务改写容器 ID。
mark_recreated() {
  local service="$1"
  case " $recreated_services " in
    *" $service "*) ;;
    *) return 0 ;;
  esac
  container_serial=$((container_serial + 1))
  printf 'mock-container-%s-%s\n' "$service" "$container_serial" >"$state_dir/$service"
  printf 'mock-container-%s-%s\n' "$service" "$container_serial" >"$state_dir/$service.running"
}

# seed_refresh_state 按 "<服务>=<容器 ID>" 写入跨轮次状态文件（0600，真实路径）。
seed_refresh_state() {
  install -d -m 700 "$test_root/deploy/runtime"
  printf '%s=%s\n' "$@" >"$refresh_state_file"
  chmod 600 "$refresh_state_file"
}

# seed_state_matching_current 预置"上一轮已成功处置"的稳态状态，用于验证不再抖动。
seed_state_matching_current() {
  local service
  install -d -m 700 "$test_root/deploy/runtime"
  : >"$refresh_state_file"
  for service in platform-mysql keycloak-db contract-mysql; do
    printf '%s=%s\n' "$service" "$(cat "$state_dir/$service")" >>"$refresh_state_file"
  done
  chmod 600 "$refresh_state_file"
}

# compose 桩：把每次调用原样写入 trace，ps 从 state_dir 读容器 ID，up 按重建清单改写 ID。
# ps -a 读"存在"（含 exited），ps 读"运行中"，用来复现 D3 的误判。
# state_dir/<服务>.psfail 存在且计数大于零时让该服务的 ps 探测失败，
# 用来复现"瞬时探测故障被当成未重建/未部署"的缺陷（X1/X2）。
compose() {
  local argument index=2 service failing=0 remaining
  printf '%s\n' "$*" >>"$trace_file"
  case "${1:-}" in
    ps)
      service="${@: -1}"
      if [[ -f "$state_dir/$service.psfail" ]]; then
        remaining="$(cat "$state_dir/$service.psfail")"
        if [[ "$remaining" -gt 0 ]]; then
          printf '%s\n' "$((remaining - 1))" >"$state_dir/$service.psfail"
          return 1
        fi
      fi
      if [[ " $* " == *' -a '* ]]; then
        cat "$state_dir/$service" 2>/dev/null || true
      else
        cat "$state_dir/$service.running" 2>/dev/null || true
      fi
      ;;
    up)
      while [[ $index -le $# ]]; do
        argument="${!index}"
        case "$argument" in
          --wait-timeout) index=$((index + 2)) ;;
          -*) index=$((index + 1)) ;;
          *)
            mark_recreated "$argument"
            case " $fail_up_services " in
              *" $argument "*) failing=1 ;;
            esac
            index=$((index + 1))
            ;;
        esac
      done
      [[ "$failing" -eq 0 ]] || return 1
      ;;
    run) return 0 ;;
    *) return 0 ;;
  esac
}

# docker 桩：inspect 供健康检查，compose ... config --services 供 scope_services 解析启用范围。
docker() {
  case "${1:-}" in
    inspect) printf 'healthy\n' ;;
    compose)
      if [[ " $* " == *' config --services '* ]]; then
        [[ "$docker_services_error" == 1 ]] && return 1
        if [[ -n "$docker_services" ]]; then
          printf '%s\n' $docker_services
        fi
        return 0
      fi
      return 0
      ;;
    *) return 0 ;;
  esac
}

# 文件网关根目录属主校验需要 root/10001，桩掉避免测试依赖宿主 uid；
# 但状态文件的 0600 校验必须看真实权限，否则测不出写入的收紧是否生效。
stat() {
  if [[ "${1:-}" == '-c' && "${2:-}" == '%a' ]]; then
    command stat -c '%a' "${@:3}"
    return
  fi
  if [[ "${1:-}" == '-c' && "${2:-}" == '%u:%g' ]]; then
    printf '10001:10001\n'
    return 0
  fi
  command stat "$@"
}

# deploy-service.sh 从命令行参数取得镜像引用与端口；测试直接给定已校验的形态。
image_ref='registry.invalid/fixture@sha256:'"$(printf '%064d' 1)"
file_gateway_image_ref="$image_ref"
env_value() { return 0; }
port_value() { printf '%s\n' "${2:-18080}"; }
prepare_managed_public_proxy_image() { return 0; }
require_mysql_ready() { [[ "$fail_at" != require_mysql_ready ]]; }
backup_database_if_initialized() { return 0; }
initialize_platform_audit_hash_key() { return 0; }
provision_iam_import_client() { return 0; }
ensure_temporal_application_network() { return 0; }
temporal_consumers_enabled() { [[ "$temporal_consumers" == enabled ]]; }
verify_service_image() { return 0; }
verify_service_stable() { return 0; }
verify_subsystem_provisioner_config_consistency() { return 0; }
verify_subsystem_control_plane_profiles() { return 0; }
wait_for_health() { return 0; }
dump_file_gateway_debug() { return 0; }
diagnose_mysql_container() { return 0; }

run_deploy_platform() {
  if deploy_platform >"$log_file" 2>&1; then
    return 0
  fi
  printf 'deploy_platform 失败，输出：\n' >&2
  cat "$log_file" >&2
  return 1
}

# expect_deploy_platform_failure 断言本轮发布失败，且不把冗长日志当成正常输出。
expect_deploy_platform_failure() {
  if deploy_platform >"$log_file" 2>&1; then
    cat "$log_file" >&2
    fail "$1"
  fi
}

# 断言 trace 中存在一条针对指定服务的 up 命令，且参数同时包含给定关键字。
assert_up_line() {
  local service="$1" expectation="$2" matched
  matched="$(grep -E "^up .*${service}( |$)" "$trace_file" | grep -F -- "$expectation" || true)"
  [[ -n "$matched" ]] || fail "缺少针对 $service 的 up 命令（应包含 $expectation）"
}

# 断言 trace 中不存在任何针对指定服务的 up 命令。
assert_no_up_line() {
  local service="$1"
  if grep -Eq "^up .*${service}( |$)" "$trace_file"; then
    fail "不应出现针对 $service 的 up 命令：$(grep -E "^up .*${service}( |$)" "$trace_file")"
  fi
}

# 断言持久化状态文件里记录的容器 ID，且文件权限仍是 0600、不是符号链接。
assert_refresh_state() {
  local service="$1" expected="$2" actual
  [[ -f "$refresh_state_file" && ! -L "$refresh_state_file" ]] ||
    fail "跨轮次刷新状态文件缺失或被替换成符号链接：$refresh_state_file"
  actual="$(awk -F= -v key="$service" '$1 == key {print $2; exit}' "$refresh_state_file")"
  [[ "$actual" == "$expected" ]] ||
    fail "刷新状态中 $service 记录为 '$actual'，应为 '$expected'"
  [[ "$(command stat -c '%a' "$refresh_state_file")" == 600 ]] ||
    fail "刷新状态文件权限不是 0600：$(command stat -c '%a' "$refresh_state_file")"
}

assert_no_refresh_state() {
  local service="$1"
  if [[ -f "$refresh_state_file" ]] && awk -F= -v key="$service" '$1 == key {found=1} END {exit !found}' "$refresh_state_file"; then
    fail "刷新状态不应记录 $service（刷新失败的数据库必须留下重试机会）"
  fi
}

# assert_no_failure_marker 断言没有留下强依赖刷新失败标记。
assert_no_failure_marker() {
  [[ ! -e "$refresh_failure_marker" && ! -L "$refresh_failure_marker" ]] ||
    fail "不应留下强依赖刷新失败标记：$refresh_failure_marker（$(cat "$refresh_failure_marker" 2>/dev/null || true)）"
}

# assert_failure_marker 断言失败标记存在、权限 0600、不是符号链接、没有残留临时文件，
# 且 JSON 结构严格符合 uip.database-refresh-failed/v1，并包含指定的 database/service。
assert_failure_marker() {
  local database="$1" service="$2" leftover marker_writer
  [[ -f "$refresh_failure_marker" && ! -L "$refresh_failure_marker" ]] ||
    fail "强依赖刷新失败标记缺失或被替换成符号链接：$refresh_failure_marker"
  [[ "$(command stat -c '%a' "$refresh_failure_marker")" == 600 ]] ||
    fail "失败标记权限不是 0600：$(command stat -c '%a' "$refresh_failure_marker")"
  leftover="$(find "$test_root/deploy/runtime" -maxdepth 1 -name '.database-refresh-failed.*' -print -quit 2>/dev/null || true)"
  [[ -z "$leftover" ]] || fail "失败标记写入后残留临时文件（不是原子替换）：$leftover"
  # 原子写入的实现形状：同目录 mktemp + mv -f 替换目录项，绝不重定向写目标。
  marker_writer="$(declare -f database_refresh_failure_marker_write)"
  grep -Fq 'mktemp "$runtime_dir/.database-refresh-failed.XXXXXX"' <<<"$marker_writer" ||
    fail '失败标记未使用同目录临时文件写入'
  grep -Fq 'mv -f -- "$temporary" "$marker"' <<<"$marker_writer" ||
    fail '失败标记未用 mv -f 原子替换'
  if ! python3 - "$refresh_failure_marker" "$database" "$service" <<'PY'
import json
import sys

path, database, service = sys.argv[1:4]
with open(path, encoding='utf-8') as handle:
    payload = json.load(handle)
if payload.get('schema') != 'uip.database-refresh-failed/v1':
    raise SystemExit('schema 不符合约定：%r' % (payload.get('schema'),))
failures = payload.get('failures')
if not isinstance(failures, list) or not failures:
    raise SystemExit('failures 必须是非空数组：%r' % (payload,))
for item in failures:
    if set(item) != {'database', 'service', 'reason', 'at'}:
        raise SystemExit('failure 字段集合不符合约定：%r' % (item,))
    if not item['database'] or not item['service'] or not item['reason']:
        raise SystemExit('failure 存在空字段：%r' % (item,))
    if not item['at'].endswith('Z') or 'T' not in item['at']:
        raise SystemExit('at 不是 UTC ISO8601：%r' % (item['at'],))
if not any(i['database'] == database and i['service'] == service for i in failures):
    raise SystemExit('未找到 %s -> %s 的失败记录：%r' % (database, service, failures))
PY
  then
    cat "$refresh_failure_marker" >&2
    fail "失败标记内容不符合 uip.database-refresh-failed/v1：$refresh_failure_marker"
  fi
}

echo '== 用例1：发布中途失败后重试仍必须强制重建 keycloak（跨轮次持久化状态） =='
reset_fixture
set_container_id platform-mysql platform-mysql-before
set_container_id keycloak-db keycloak-db-before
set_container_id contract-mysql contract-mysql-before
recreated_services='keycloak-db'
# 第一轮：keycloak-db 被重建，随后在 require_mysql_ready 处中止（现场"发布中止两次"）。
begin_run
fail_at=require_mysql_ready
expect_deploy_platform_failure '用例1：第一轮应当失败（require_mysql_ready）'
fail_at=""
recreated_services=""
[[ ! -e "$refresh_state_file" ]] || fail '用例1：第一轮在记录之前中止，不应留下刷新状态'
keycloak_db_current="$(cat "$state_dir/keycloak-db")"
# 第二轮：快照已是重建后的容器，只靠本轮比对看不到重建，必须靠持久化状态缺失兜底。
begin_run
run_deploy_platform || fail '用例1：第二轮（重试）deploy_platform 返回失败'
assert_up_line keycloak '--force-recreate'
grep -Fq '数据库已重建，强制重建强依赖：keycloak-db -> keycloak' "$log_file" ||
  fail '用例1：第二轮缺少 keycloak-db -> keycloak 证据行'
assert_refresh_state keycloak-db "$keycloak_db_current"
# 第三轮：状态已与当前容器一致，不得再抖动。
begin_run
run_deploy_platform || fail '用例1：第三轮 deploy_platform 返回失败'
if grep -Eq '^up .*--force-recreate.* keycloak( |$)' "$trace_file"; then
  fail '用例1：第三轮状态已一致，不应再强制重建 keycloak'
fi
grep -Fqx 'up -d --wait --wait-timeout 180 keycloak' "$trace_file" ||
  fail '用例1：第三轮 keycloak 启动参数与稳态不一致'
pass '发布中止后重试仍强制重建 keycloak，成功后记录状态，后续轮次不再抖动'

echo '== 用例2：容器 ID 未变化且状态一致，不产生任何额外重启 =='
reset_fixture
set_container_id platform-mysql platform-mysql-same
set_container_id keycloak-db keycloak-db-same
set_container_id contract-mysql contract-mysql-same
seed_state_matching_current
run_deploy_platform || fail '用例2：数据库未重建时 deploy_platform 返回失败'
if grep -Eq '^up .*--force-recreate.* keycloak( |$)' "$trace_file"; then
  fail '用例2：数据库未重建却强制重建了 keycloak'
fi
grep -Fqx 'up -d --wait --wait-timeout 180 keycloak' "$trace_file" ||
  fail '用例2：keycloak 启动参数与改造前不一致'
for unchanged_database in platform-mysql keycloak-db contract-mysql; do
  grep -Fq "数据库未重建，保持现有依赖容器：$unchanged_database" "$log_file" ||
    fail "用例2：缺少 $unchanged_database 未重建证据行"
done
assert_no_up_line contract-api
for unchanged_database in platform-mysql keycloak-db contract-mysql; do
  assert_refresh_state "$unchanged_database" "$(cat "$state_dir/$unchanged_database")"
done
pass '稳态下 keycloak 启动命令保持原样且无额外重启'

echo '== 用例2b：启用 Temporal 时 keycloak 与 temporal 仍按原样合并启动 =='
reset_fixture
set_container_id platform-mysql platform-mysql-same
set_container_id keycloak-db keycloak-db-same
set_container_id contract-mysql contract-mysql-same
seed_state_matching_current
temporal_consumers=enabled
run_deploy_platform || fail '用例2b：启用 Temporal 且数据库未重建时 deploy_platform 返回失败'
grep -Fqx 'up -d --wait --wait-timeout 180 keycloak temporal' "$trace_file" ||
  fail '用例2b：启用 Temporal 时的 keycloak 合并启动命令与改造前不一致'
if grep -Eq '^up .*--force-recreate.* keycloak temporal' "$trace_file"; then
  fail '用例2b：数据库未重建却强制重建了 keycloak/temporal'
fi
pass '启用 Temporal 时 keycloak 合并启动命令保持不变'

echo '== 用例2c：keycloak-db 重建且启用 Temporal 时，不得连带强制重建共享 Temporal =='
reset_fixture
set_container_id platform-mysql platform-mysql-before
set_container_id keycloak-db keycloak-db-before
set_container_id contract-mysql contract-mysql-before
temporal_consumers=enabled
recreated_services='keycloak-db'
run_deploy_platform || fail '用例2c：keycloak-db 重建且启用 Temporal 时 deploy_platform 返回失败'
grep -Fqx 'up -d --wait --wait-timeout 180 --force-recreate keycloak' "$trace_file" ||
  fail '用例2c：keycloak 必须单独带 --force-recreate 启动'
grep -Fqx 'up -d --wait --wait-timeout 180 temporal' "$trace_file" ||
  fail '用例2c：Temporal 必须在 keycloak 之后按原参数单独启动'
if grep -Eq '^up .*--force-recreate.* temporal' "$trace_file"; then
  fail '用例2c：共享 Temporal 不能被 --force-recreate 连带重建'
fi
keycloak_start_count="$(grep -cE '^up .* keycloak( |$)' "$trace_file" || true)"
[[ "$keycloak_start_count" -eq 1 ]] || fail "用例2c：keycloak 启动了 $keycloak_start_count 次，应为 1 次"
pass 'keycloak-db 重建时只强制重建 keycloak，共享 Temporal 不连带重建'

echo '== 用例3：contract-mysql 重建且 contract-api 已存在，强制重建 contract-api =='
reset_fixture
set_container_id platform-mysql platform-mysql-before
set_container_id keycloak-db keycloak-db-before
set_container_id contract-mysql contract-mysql-before
set_container_id contract-api contract-api-before
recreated_services='contract-mysql'
run_deploy_platform || fail '用例3：contract-mysql 重建时 deploy_platform 返回失败'
assert_up_line contract-api '--force-recreate'
assert_up_line contract-api '--no-deps'
grep -Fq 'up -d --force-recreate --no-deps --wait --wait-timeout 240 contract-api' "$trace_file" ||
  fail '用例3：contract-api 刷新命令不符合约定参数'
grep -Fq '数据库已重建，强制重建强依赖：contract-mysql -> contract-api' "$log_file" ||
  fail '用例3：缺少 contract-mysql -> contract-api 证据行'
while IFS= read -r contract_api_line; do
  case "$contract_api_line" in
    *'--no-deps'*) ;;
    *) fail "用例3：contract-api 出现无 --no-deps 的 up 命令：$contract_api_line" ;;
  esac
done < <(grep -E '^up .* contract-api( |$)' "$trace_file" || true)
# D4：Temporal 不自动重启，但必须留下明确证据，避免运维误以为漏刷。
grep -Fq '数据库已重建，保持运行不重启：contract-mysql -> temporal' "$log_file" ||
  fail '用例3：缺少 Temporal 保持运行不重启的证据行'
pass 'contract-api 已存在时按 --force-recreate --no-deps 刷新，Temporal 只输出证据'

echo '== 用例4：contract-mysql 重建但 contract-api 未部署，跳过且不失败 =='
reset_fixture
set_container_id platform-mysql platform-mysql-before
set_container_id keycloak-db keycloak-db-before
set_container_id contract-mysql contract-mysql-before
recreated_services='contract-mysql'
run_deploy_platform || fail '用例4：未部署 contract-api 时 deploy_platform 必须仍然成功'
assert_no_up_line contract-api
grep -Fq '数据库已重建，强依赖未部署，跳过：contract-mysql -> contract-api' "$log_file" ||
  fail '用例4：缺少 contract-api 跳过的证据行'
pass '未部署的 contract-api 不会被创建，发布继续成功'

echo '== 用例5：首次安装（启动前全部不存在）不触发依赖刷新 =='
reset_fixture
recreated_services='platform-mysql keycloak-db contract-mysql contract-api'
run_deploy_platform || fail '用例5：首次安装 deploy_platform 返回失败'
keys_after="$(cat "$state_dir/keycloak-db" 2>/dev/null || true)"
[[ -n "$keys_after" ]] || fail '用例5：模拟未创建 keycloak-db 容器'
if grep -Eq '^up .*--force-recreate.* keycloak( |$)' "$trace_file"; then
  fail '用例5：首次安装不应强制重建 keycloak'
fi
assert_no_up_line contract-api
for fresh_database in platform-mysql keycloak-db contract-mysql; do
  grep -Fq "数据库未重建，保持现有依赖容器：$fresh_database" "$log_file" ||
    fail "用例5：首次安装缺少 $fresh_database 未重建证据行"
done
# 首装仍要落状态，否则下一次发布会因为没有历史记录而多刷新一次强依赖。
assert_refresh_state keycloak-db "$keys_after"
pass '首次安装按未重建处理，不做额外重启，并记录初始容器 ID'

echo '== 用例6：contract-mysql 重建时 contract-api 与 contract-worker 一起刷新 =='
reset_fixture
set_container_id platform-mysql platform-mysql-before
set_container_id keycloak-db keycloak-db-before
set_container_id contract-mysql contract-mysql-before
set_container_id contract-api contract-api-before
set_container_id contract-worker contract-worker-before
recreated_services='contract-mysql'
run_deploy_platform || fail '用例6：contract-worker 刷新时 deploy_platform 返回失败'
assert_up_line contract-api '--force-recreate'
assert_up_line contract-worker '--force-recreate'
grep -Fq 'up -d --force-recreate --no-deps --wait --wait-timeout 240 contract-worker' "$trace_file" ||
  fail '用例6：contract-worker 刷新命令不符合约定参数'
grep -Fq '数据库已重建，强制重建强依赖：contract-mysql -> contract-worker' "$log_file" ||
  fail '用例6：缺少 contract-mysql -> contract-worker 证据行'
pass 'contract-worker 与 contract-api 一起被刷新'

echo '== 用例7：contract-api 刷新失败只告警，平台发布继续，且不记录状态 =='
reset_fixture
set_container_id platform-mysql platform-mysql-before
set_container_id keycloak-db keycloak-db-before
set_container_id contract-mysql contract-mysql-before
set_container_id contract-api contract-api-before
recreated_services='keycloak-db contract-mysql'
fail_up_services='contract-api'
run_deploy_platform || fail '用例7：业务子系统刷新失败不得中止平台发布'
grep -Fq '[警告] 数据库已重建，但强依赖刷新失败：contract-mysql -> contract-api' "$log_file" ||
  fail '用例7：缺少可 grep 的强依赖刷新失败告警'
grep -Fq '平台发布继续；该子系统随后由自身发布/verify 阶段判定' "$log_file" ||
  fail '用例7：告警未说明平台发布继续与后续判定路径'
grep -Fq '数据库刷新未完成，不记录刷新状态，下次发布将重试：contract-mysql' "$log_file" ||
  fail '用例7：缺少"不记录刷新状态"的证据行'
assert_no_refresh_state contract-mysql
# 关键回归：contract-api 失败不能连带跳过同一轮里 keycloak 的修复。
assert_up_line keycloak '--force-recreate'
grep -Fqx 'up -d --force-recreate --no-deps --wait --wait-timeout 240 contract-api' "$trace_file" ||
  fail '用例7：仍然必须真实尝试过 contract-api 刷新'
pass 'contract-api 刷新失败只告警、不记录状态，keycloak 修复照常执行'

echo '== 用例8：已退出的 contract-api（ps -q 为空）仍必须被刷新 =='
reset_fixture
set_container_id platform-mysql platform-mysql-before
set_container_id keycloak-db keycloak-db-before
set_container_id contract-mysql contract-mysql-before
set_container_id contract-api contract-api-before
set_exited contract-api
recreated_services='contract-mysql'
[[ -z "$(cat "$state_dir/contract-api.running" 2>/dev/null || true)" ]] || fail '用例8：桩未把 contract-api 置为已退出'
run_deploy_platform || fail '用例8：已退出强依赖时 deploy_platform 返回失败'
grep -Fq 'ps -a -q contract-api' "$trace_file" ||
  fail '用例8：必须用 ps -a -q 判定强依赖是否存在'
assert_up_line contract-api '--force-recreate'
grep -Fq '数据库已重建，强依赖未部署，跳过：contract-mysql -> contract-api' "$log_file" &&
  fail '用例8：已退出的 contract-api 不应被当成未部署'
pass '已退出的强依赖被识别并刷新，而不是当成未部署跳过'

echo '== 用例9：contract 子系统未启用时不做任何 up，只输出诚实跳过行 =='
reset_fixture
set_container_id platform-mysql platform-mysql-before
set_container_id keycloak-db keycloak-db-before
set_container_id contract-mysql contract-mysql-before
set_container_id contract-api contract-api-before
set_container_id contract-worker contract-worker-before
docker_services='platform-api keycloak'
recreated_services='contract-mysql'
run_deploy_platform || fail '用例9：contract 子系统未启用时 deploy_platform 返回失败'
assert_no_up_line contract-api
assert_no_up_line contract-worker
grep -Fq '数据库已重建，强依赖所属子系统未启用，跳过：contract-mysql -> contract-api（当前 docker-compose.yml 未启用 contract 子系统）' "$log_file" ||
  fail '用例9：缺少子系统未启用的跳过证据行'
pass '子系统未启用时不 up 任何强依赖，跳过原因可核对'

echo '== 用例10：scope 解析失败时 temporal_consumers_enabled 视为启用（保守默认） =='
reset_fixture
docker_services_error=1
if ! temporal_consumers_enabled_real; then
  fail '用例10：scope 解析失败时应按保守默认返回"启用"（0）'
fi
docker_services_error=0
docker_services='platform-api keycloak'
if temporal_consumers_enabled_real; then
  fail '用例10：服务清单读取成功且无消费者时不应返回"启用"'
fi
docker_services='platform-api contract-api'
if ! temporal_consumers_enabled_real; then
  fail '用例10：contract-api 启用时必须返回"启用"（0）'
fi
pass '解析失败视为启用，只有明确读到"无消费者"才停用 Temporal'

echo '== 用例11（X1）：快照探测持续失败必须中止平台发布，且不记状态、不重建 keycloak =='
reset_fixture
set_container_id platform-mysql platform-mysql-before
set_container_id keycloak-db keycloak-db-before
set_container_id contract-mysql contract-mysql-before
recreated_services='keycloak-db'
set_ps_failures keycloak-db 999
expect_deploy_platform_failure '用例11：数据库身份探测失败时 deploy_platform 必须返回非零'
if grep -Eq '^up .*--force-recreate.* keycloak( |$)' "$trace_file"; then
  fail '用例11：身份未知时不得强制重建 keycloak（那是在没有证据的情况下动作）'
fi
if grep -Eq '^up .* platform-mysql keycloak-db contract-mysql( |$)' "$trace_file"; then
  fail '用例11：快照失败必须在 compose up 数据库之前中止'
fi
[[ ! -e "$refresh_state_file" ]] ||
  fail '用例11：探测失败的数据库绝不能写刷新状态（会永久抑制修复）'
probe_attempts="$(grep -c 'ps -a -q keycloak-db' "$trace_file" || true)"
[[ "$probe_attempts" -eq 3 ]] ||
  fail "用例11：探测应重试 3 次，实际 $probe_attempts 次"
grep -Fq '无法探测数据库容器身份：服务=keycloak-db，探测命令=compose ps -a -q keycloak-db' "$log_file" ||
  fail '用例11：错误信息必须点名失败的数据库和探测命令'
grep -Fq '中止平台发布' "$log_file" ||
  fail '用例11：错误信息必须明确"中止平台发布"而不是静默降级'
assert_no_failure_marker
pass '探测未知即中止发布：没有 up、没有状态记录、错误点名到探测命令'

echo '== 用例12（X1）：探测失败两次后成功，重试生效并按正常发布继续 =='
reset_fixture
set_container_id platform-mysql platform-mysql-same
set_container_id keycloak-db keycloak-db-same
set_container_id contract-mysql contract-mysql-same
seed_state_matching_current
set_ps_failures keycloak-db 2
run_deploy_platform || fail '用例12：两次探测失败后应重试成功，deploy_platform 却返回失败'
grep -Fq '探测容器状态失败，0 秒后重试（第 1/3 次）：compose ps -a -q keycloak-db' "$log_file" ||
  fail '用例12：缺少第 1 次探测失败的重试证据'
grep -Fq '探测容器状态失败，0 秒后重试（第 2/3 次）：compose ps -a -q keycloak-db' "$log_file" ||
  fail '用例12：缺少第 2 次探测失败的重试证据'
if grep -Eq '^up .*--force-recreate.* keycloak( |$)' "$trace_file"; then
  fail '用例12：重试成功后数据库未重建，不应强制重建 keycloak'
fi
grep -Fqx 'up -d --wait --wait-timeout 180 keycloak' "$trace_file" ||
  fail '用例12：重试成功后的 keycloak 启动参数应与稳态一致'
assert_refresh_state keycloak-db "$(cat "$state_dir/keycloak-db")"
assert_no_failure_marker
pass '瞬时探测故障被重试吸收，行为与正常发布完全一致'

echo '== 用例13（X2）：清单解析失败不得声称未启用/未部署，且留持久标记、不记状态 =='
reset_fixture
set_container_id platform-mysql platform-mysql-before
set_container_id keycloak-db keycloak-db-before
set_container_id contract-mysql contract-mysql-before
set_container_id contract-api contract-api-before
set_container_id contract-worker contract-worker-before
docker_services_error=1
recreated_services='contract-mysql'
run_deploy_platform || fail '用例13：清单解析失败不得升级为平台发布失败（业务子系统不硬门禁平台发布）'
assert_no_up_line contract-api
assert_no_up_line contract-worker
if grep -Fq '强依赖所属子系统未启用' "$log_file"; then
  fail '用例13：清单解析失败不得声称"子系统未启用"（原因从未成立）'
fi
if grep -Fq -- '-> contract-api' "$log_file" && grep -Fq '强依赖未部署，跳过' "$log_file"; then
  fail '用例13：清单解析失败不得声称"强依赖未部署"（原因从未成立）'
fi
grep -Fq '无法解析统一 Compose 服务清单，本条数据库不记录刷新状态：contract-mysql' "$log_file" ||
  fail '用例13：缺少"清单无法解析，本条数据库不记录刷新状态"的明确证据'
assert_no_refresh_state contract-mysql
assert_failure_marker contract-mysql contract-api
pass '解析失败按"无法判定"处置：不猜未启用，不记状态，留持久失败标记'

echo '== 用例14（X2）：强依赖存在性探测失败不得当成未部署，且不记状态、留标记 =='
reset_fixture
set_container_id platform-mysql platform-mysql-before
set_container_id keycloak-db keycloak-db-before
set_container_id contract-mysql contract-mysql-before
set_container_id contract-api contract-api-before
recreated_services='contract-mysql'
set_ps_failures contract-api 999
run_deploy_platform || fail '用例14：强依赖探测失败不得升级为平台发布失败'
assert_no_up_line contract-api
if grep -Fq '强依赖未部署，跳过：contract-mysql -> contract-api' "$log_file"; then
  fail '用例14：探测失败不得被说成"强依赖未部署"'
fi
grep -Fq '无法探测强依赖容器状态：contract-mysql -> contract-api' "$log_file" ||
  fail '用例14：缺少"强依赖探测失败"的明确证据'
grep -Fq '数据库刷新未完成，不记录刷新状态，下次发布将重试：contract-mysql' "$log_file" ||
  fail '用例14：缺少不记录刷新状态的证据'
assert_no_refresh_state contract-mysql
assert_failure_marker contract-mysql contract-api
pass '强依赖探测失败按失败处置：不虚报未部署，不记状态，留持久标记'

echo '== 用例15（X3）：刷新失败写持久标记，下一次全成功发布删除标记 =='
reset_fixture
set_container_id platform-mysql platform-mysql-before
set_container_id keycloak-db keycloak-db-before
set_container_id contract-mysql contract-mysql-before
set_container_id contract-api contract-api-before
recreated_services='contract-mysql'
fail_up_services='contract-api'
begin_run
run_deploy_platform || fail '用例15：业务子系统刷新失败不得中止平台发布'
grep -Fq '[警告] 数据库已重建，但强依赖刷新失败：contract-mysql -> contract-api' "$log_file" ||
  fail '用例15：缺少可 grep 的刷新失败告警'
assert_no_refresh_state contract-mysql
assert_failure_marker contract-mysql contract-api
# 第二轮完全成功：必须删除历史失败标记，且真正记录该数据库的刷新状态。
fail_up_services=""
begin_run
run_deploy_platform || fail '用例15：第二轮全部成功时 deploy_platform 返回失败'
assert_refresh_state contract-mysql "$(cat "$state_dir/contract-mysql")"
assert_no_failure_marker
grep -Fq '数据库强依赖刷新已全部成功，已清除历史失败标记' "$log_file" ||
  fail '用例15：缺少清除历史失败标记的证据'
pass '失败留标记、成功清标记：运维与 status/verify 有持久信号'

echo '== 用例16（X3）：失败标记 0600、原子写入，并拒绝写穿符号链接 =='
reset_fixture
set_container_id platform-mysql platform-mysql-before
set_container_id keycloak-db keycloak-db-before
set_container_id contract-mysql contract-mysql-before
set_container_id contract-api contract-api-before
recreated_services='contract-mysql'
fail_up_services='contract-api'
begin_run
run_deploy_platform || fail '用例16：标记用例的发布不应失败'
assert_failure_marker contract-mysql contract-api
[[ "$(command stat -c '%a' "$refresh_failure_marker")" == 600 ]] ||
  fail "失败标记权限不是 0600：$(command stat -c '%a' "$refresh_failure_marker")"
# 符号链接：拒绝写入，链接目标不得被创建或改写。
attacker_marker="$test_root/attacker-marker.json"
rm -f -- "$refresh_failure_marker"
ln -s "$attacker_marker" "$refresh_failure_marker"
begin_run
run_deploy_platform || fail '用例16：标记路径是符号链接时发布不应因此失败（只告警）'
[[ -L "$refresh_failure_marker" ]] ||
  fail '用例16：拒绝写入后标记路径应仍是原来的符号链接'
[[ ! -e "$attacker_marker" ]] ||
  fail '用例16：失败标记不得写穿符号链接'
grep -Fq '拒绝写入符号链接的数据库刷新失败标记' "$log_file" ||
  fail '用例16：缺少拒绝写穿符号链接的明确证据'
pass '失败标记 0600、原子替换、拒绝写穿符号链接'

echo 'database dependent refresh tests passed'
