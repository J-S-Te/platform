#!/usr/bin/env bash
# 离线显示时区配置；不校时、不修改 DSN/数据库会话或历史时间。
set -Eeuo pipefail
umask 077
script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
deploy_dir="$(cd -- "$script_dir/.." && pwd -P)"
command_name=configure
compose_file=''
tz_name=Asia/Shanghai
services=()
extra_files=()
yes=false
fail() { printf '[tz] 错误：%s\n' "$*" >&2; exit 1; }
log() { printf '[tz] %s\n' "$*"; }
usage() {
  printf '%s\n' '用法：set-container-timezone.sh check|configure|apply [--dir DIR] [--tz Asia/Shanghai] [--service NAME ...] [--yes]' \
    'check 只读预检；configure 保存独立覆盖层，不重启；apply 委托 deploy.sh timezone-apply。' \
    '兼容 --check-only；check 支持 --compose FILE、--extra-file FILE；变更只使用统一部署配置。' \
    '首次安装前可用 --service 显式选择已有本地镜像的服务；默认只选择当前运行的服务。' \
    '数据库不修改；不联网下载、不校时、不改 DSN、不平移历史数据。'
}
argument() { [[ $# -ge 2 && -n "$2" && "$2" != --* ]] || fail "$1 缺少参数"; }
while [[ $# -gt 0 ]]; do
  case "$1" in
    check|configure|apply|internal-apply) command_name="$1"; shift ;;
    --dir) argument "$@"; deploy_dir="$(cd -- "$2" && pwd -P)"; shift 2 ;;
    --compose) argument "$@"; compose_file="$(cd -- "$(dirname -- "$2")" && pwd -P)/$(basename -- "$2")"; shift 2 ;;
    --extra-file) argument "$@"; extra_files+=("$(cd -- "$(dirname -- "$2")" && pwd -P)/$(basename -- "$2")"); shift 2 ;;
    --tz) argument "$@"; tz_name="$2"; shift 2 ;;
    --service) argument "$@"; [[ "$2" =~ ^[a-zA-Z0-9][a-zA-Z0-9_.-]*$ ]] || fail '服务名无效'; services+=("$2"); shift 2 ;;
    --check-only) command_name=check; shift ;;
    --yes) yes=true; shift ;;
    --shift-history|--recreate) fail "$1 已移除：历史迁移必须单独审核，重建请使用 apply" ;;
    -h|--help) usage; exit 0 ;;
    *) fail "未知参数：$1" ;;
  esac
done
compose_file="${compose_file:-$deploy_dir/docker-compose.yml}"
[[ "$(dirname -- "$compose_file")" == "$deploy_dir" ]] || fail '--compose 必须位于 --dir 部署目录中'
[[ "$tz_name" =~ ^[A-Za-z0-9_+-]+(/[A-Za-z0-9_+-]+)*$ ]] || fail '非法时区标识'
[[ -f "/usr/share/zoneinfo/$tz_name" ]] || fail '宿主机缺少目标时区数据，请导入离线依赖包'
for file in "$compose_file" "$deploy_dir/.env" "$deploy_dir/.release.env"; do
  [[ -f "$file" && ! -L "$file" ]] || fail "配置不存在或为符号链接：$file"
done
if [[ "$command_name" != check ]]; then
  [[ ${#extra_files[@]} == 0 && "$compose_file" == "$deploy_dir/docker-compose.yml" ]] || fail '变更必须使用统一部署配置；自定义覆盖层仅允许只读 check'
  for marker in "$deploy_dir/runtime/.assets-install-transaction" "$deploy_dir/runtime/.control-plane-reload-required"; do
    [[ ! -e "$marker" && ! -L "$marker" ]] || fail '存在未完成的受控资产/控制面升级，请先恢复再配置时区'
  done
fi
overlay="$deploy_dir/runtime/compose.timezone.yaml"
[[ ! -L "$overlay" ]] || fail '时区覆盖层不能是符号链接'
if [[ "$command_name" == apply ]]; then
  [[ ${#services[@]} == 0 && ${#extra_files[@]} == 0 && "$compose_file" == "$deploy_dir/docker-compose.yml" ]] || fail 'apply 使用已保存计划，不接受自定义范围'
  [[ -f "$overlay" ]] || fail '请先执行 configure'
  exec bash "$deploy_dir/bin/deploy.sh" timezone-apply
fi
for tool in docker jq mktemp flock; do command -v "$tool" >/dev/null || fail "缺少 ${tool}，请安装离线依赖包"; done
[[ -d "$deploy_dir/runtime" && ! -L "$deploy_dir/runtime" ]] || fail '缺少安全的 runtime 目录'
if [[ "$command_name" != check ]]; then
  [[ ! -L "$deploy_dir/runtime/.deploy.lock" ]] || fail '部署锁不能是符号链接'
  exec 8>"$deploy_dir/runtime/.deploy.lock"
  flock -w 30 8 || fail '另一项部署正在执行'
fi
scratch="$(mktemp -d "${TMPDIR:-/tmp}/uip-timezone.XXXXXX")"
trap 'rm -r -- "$scratch"' EXIT
source "$script_dir/public-transport.sh"
public_transport_prepare "$deploy_dir" "$deploy_dir/.env" || fail '公开传输配置校验失败'
args=(--project-directory "$deploy_dir" --file "$compose_file")
public_transport_compose_args args
for file in ${extra_files[@]+"${extra_files[@]}"}; do
  [[ -f "$file" && ! -L "$file" ]] || fail '附加配置不存在或不安全'
  args+=(--file "$file")
done
args+=(--env-file "$deploy_dir/.env" --env-file "$deploy_dir/.release.env")
compose() { docker compose "${args[@]}" "$@"; }
# config JSON 可能含凭据，只留在 0700 临时目录，错误原文不打印。
compose config --format json >"$scratch/config.json" 2>"$scratch/error" || fail 'Compose 解析失败，请在受保护终端诊断'
jq -e '.services | type == "object"' "$scratch/config.json" >/dev/null || fail 'Compose 输出无效'
if [[ "$command_name" == internal-apply ]]; then
  [[ "${UIP_TIMEZONE_CONTROLLED_APPLY:-}" == 1 && -f "$overlay" ]] || fail '内部应用只能由 deploy.sh 调用'
  while IFS= read -r svc; do services+=("$svc"); done < <(jq -r '.services | keys[]' "$overlay")
  tz_name="$(jq -er '[.services[].environment.TZ] | unique | select(length == 1) | .[0]' "$overlay")" || fail '覆盖层时区不一致'
elif [[ ${#services[@]} == 0 ]]; then
  compose ps --status running --services >"$scratch/services" 2>"$scratch/error" || fail '无法查询运行服务'
  while IFS= read -r svc; do [[ -z "$svc" ]] || services+=("$svc"); done <"$scratch/services"
fi
[[ ${#services[@]} -gt 0 ]] || fail '无运行服务；首装前请用 --service 指定本地镜像服务'
printf '{"services":{}}\n' >"$scratch/plan.json"
printf '{"services":{}}\n' >"$scratch/pins.json"
printf '{"services":{}}\n' >"$scratch/rollback.json"
selected=()
for svc in "${services[@]}"; do
  image="$(jq -er --arg svc "$svc" '.services[$svc].image' "$scratch/config.json")" || fail "服务不存在或缺镜像：$svc"
  # 数据库的 SYSTEM 时区会改变 NOW()/TIMESTAMP 语义，显示工具必须排除。
  if [[ "$svc" == *mysql* || "$svc" == *postgres* || "$image" =~ (^|/)(mysql|mariadb|postgres)(:|@|$) ]]; then
    log "跳过数据库：${svc}（保持既有时间语义）"; continue
  fi
  configured_id="$(docker image inspect --format '{{.Id}}' "$image" 2>"$scratch/error")" || fail "镜像不在本机：${svc}；请先导入离线包"
  compose ps --status running -q "$svc" >"$scratch/ids" 2>"$scratch/error" || fail "查询失败：$svc"
  count="$(awk 'NF {n++} END {print n+0}' "$scratch/ids")"
  [[ "$count" -le 1 ]] || fail "暂不支持多副本服务：$svc"
  id="$(head -n 1 "$scratch/ids")"
  if [[ -n "$id" ]]; then
    docker inspect "$id" >"$scratch/container.json" 2>"$scratch/error" || fail "无法核验容器：$svc"
    jq -e --arg image "$configured_id" '.[0].Image == $image and .[0].State.Running == true' "$scratch/container.json" >/dev/null || fail "运行镜像与发布配置不一致：$svc"
    docker exec "$id" test -r "/usr/share/zoneinfo/$tz_name" 2>"$scratch/error" || fail "镜像缺少时区支持或 test 工具：${svc}；请受控导入升级包"
  else
    [[ "$command_name" != internal-apply ]] || fail "计划服务已停止：${svc}；不会自动安装或启动"
    docker run --rm --network none --pull never --entrypoint test "$configured_id" -r "/usr/share/zoneinfo/$tz_name" >"$scratch/output" 2>"$scratch/error" || fail "离线镜像缺少时区支持或 test 工具：$svc"
  fi
  jq --arg svc "$svc" --arg tz "$tz_name" --slurpfile cfg "$scratch/config.json" \
    '.services[$svc] = {environment: {TZ: $tz}} | if $cfg[0].services[$svc].environment.MB_JAVA_TIMEZONE != null then .services[$svc].environment.MB_JAVA_TIMEZONE = $tz else . end' \
    "$scratch/plan.json" >"$scratch/next"; mv "$scratch/next" "$scratch/plan.json"
  if [[ "$command_name" == internal-apply ]]; then
    jq --arg svc "$svc" --arg image "$configured_id" '.services[$svc] = {image: $image}' "$scratch/pins.json" >"$scratch/next"; mv "$scratch/next" "$scratch/pins.json"
    jq --arg svc "$svc" --slurpfile c "$scratch/container.json" \
      'def env($k): [$c[0][0].Config.Env[] | select(startswith($k + "=")) | ltrimstr($k + "=")][0]; .services[$svc] = {image: $c[0][0].Image, environment: {TZ: env("TZ"), MB_JAVA_TIMEZONE: env("MB_JAVA_TIMEZONE")}}' \
      "$scratch/rollback.json" >"$scratch/next"; mv "$scratch/next" "$scratch/rollback.json"
  fi
  selected+=("$svc"); log "预检通过：$svc"
done
[[ ${#selected[@]} -gt 0 ]] || fail '没有可配置的非数据库服务'
if command -v timedatectl >/dev/null; then
  sync="$(timedatectl show -p NTPSynchronized --value 2>/dev/null || true)"
  [[ "$sync" == yes ]] && log '宿主机报告已同步；不修改时钟' || log '时间准确性未确认：请核对内网 NTP 或人工核对，脚本不校时'
else log '时间准确性未确认：未提供时间同步状态，脚本不校时'; fi
if [[ "$command_name" == check ]]; then log '只读预检完成；不代表业务时间或授权验收通过'; exit 0; fi
if [[ "$command_name" == configure ]]; then
  if [[ -f "$overlay" ]] && cmp -s "$overlay" "$scratch/plan.json"; then log '配置相同，无变更'; exit 0; fi
  compose --file "$scratch/plan.json" config --quiet >"$scratch/output" 2>"$scratch/error" || fail '目标配置校验失败，未修改文件'
  if ! $yes; then read -r -p '[tz] 仅写显示时区覆盖层，不改数据库、不重启。继续？[y/N] ' answer; [[ "$answer" == y || "$answer" == Y ]] || fail '已取消'; fi
  backup="$(mktemp -d "$deploy_dir/runtime/timezone-backup.XXXXXX")"
  [[ ! -f "$overlay" ]] || cp -p -- "$overlay" "$backup/compose.timezone.yaml"
  for file in "$compose_file" "$deploy_dir/.env" "$deploy_dir/.release.env"; do cp -p -- "$file" "$backup/$(basename "$file")"; chmod 600 "$backup/$(basename "$file")"; done
  temporary="$(mktemp "$deploy_dir/runtime/.timezone.XXXXXX")"
  install -m 600 "$scratch/plan.json" "$temporary"; mv -- "$temporary" "$overlay"
  log "配置已保存，容器未重建；备份：$backup"; exit 0
fi
# 对每个当前组件固定真实镜像，拒绝启动停止系统、联网拉取或构建。
compose --file "$scratch/pins.json" config --quiet >"$scratch/output" 2>"$scratch/error" || fail '应用计划无效'
remaining=("${selected[@]}"); touched=()
rollback() {
  local svc result=0
  for svc in ${touched[@]+"${touched[@]}"}; do
    compose --file "$scratch/rollback.json" up -d --no-deps --no-build --pull never --force-recreate --wait --wait-timeout 180 "$svc" >"$scratch/output" 2>"$scratch/error" || result=1
  done
  [[ "$result" == 0 ]] && log '已恢复本轮已处理容器的原显示配置；目标覆盖层保留，可排查后重试' || log '部分容器恢复失败，请停止发布并人工恢复；目标覆盖层保留'
}
trap 'rollback; exit 1' INT TERM
trap 'status=$?; trap - ERR; rollback; exit "$status"' ERR
while [[ ${#remaining[@]} -gt 0 ]]; do
  next=(); progressed=false
  for svc in "${remaining[@]}"; do
    blocked=false
    while IFS= read -r dep; do
      [[ -n "$dep" ]] || continue
      for pending in "${remaining[@]}"; do [[ "$pending" != "$dep" ]] || blocked=true; done
      compose ps --status running -q "$dep" >"$scratch/deps" 2>"$scratch/error" || { rollback; fail "依赖查询失败：$svc"; }
      [[ -s "$scratch/deps" ]] || { rollback; fail "依赖未运行：$svc/$dep"; }
      while IFS= read -r cid; do
        docker inspect "$cid" >"$scratch/dependency.json" 2>"$scratch/error" || { rollback; fail '依赖读取失败'; }
        jq -e '.[0].State.Running == true and ((.[0].State.Health.Status // "healthy") == "healthy")' "$scratch/dependency.json" >/dev/null || { rollback; fail "依赖未健康：$svc/$dep"; }
      done <"$scratch/deps"
    done < <(jq -r --arg svc "$svc" '.services[$svc].depends_on // {} | keys[]' "$scratch/config.json")
    if $blocked; then next+=("$svc"); continue; fi
    touched+=("$svc")
    if ! compose --file "$scratch/pins.json" up -d --no-deps --no-build --pull never --force-recreate --wait --wait-timeout 180 "$svc" >"$scratch/output" 2>"$scratch/error"; then rollback; fail "服务更新失败：${svc}（原始日志留在 Docker，避免打印凭据）"; fi
    compose ps --status running -q "$svc" >"$scratch/newids" 2>"$scratch/error" || { rollback; fail '更新后查询失败'; }
    [[ -s "$scratch/newids" ]] || { rollback; fail "更新后服务未运行：$svc"; }
    while IFS= read -r cid; do
      docker inspect "$cid" >"$scratch/verified.json" 2>"$scratch/error" || { rollback; fail '更新后核验失败'; }
      jq -e --arg tz "TZ=$tz_name" --arg image "$(jq -r --arg svc "$svc" '.services[$svc].image' "$scratch/pins.json")" '.[0].Image == $image and (.[0].Config.Env | index($tz)) != null and .[0].State.Running == true' "$scratch/verified.json" >/dev/null || { rollback; fail "镜像或 TZ 核验失败：$svc"; }
    done <"$scratch/newids"
    progressed=true; log "已应用：$svc"
  done
  $progressed || { rollback; fail '依赖顺序存在环，停止应用'; }
  remaining=(${next[@]+"${next[@]}"})
done
log '显示配置应用完成；数据库时间语义未修改。业务有效期仍需独立验收。'
