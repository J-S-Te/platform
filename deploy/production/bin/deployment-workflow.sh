#!/usr/bin/env bash
# 日志条目只描述进度；每个发布检查点仍须具备当前包、镜像和健康证明，日志绝不授予授权。

workflow_enabled() { [[ -n "${UIP_INSTALL_SESSION:-}" ]]; }

workflow_update() {
  workflow_enabled || return 0
  local -a args=(update --state-dir "$UIP_INSTALL_STATE_DIR" --session "$UIP_INSTALL_SESSION" --stage "$1" --status "$2")
  [[ -z "${3:-}" ]] || args+=(--reason-code "$3")
  python3 "$UIP_INSTALL_SESSION_TOOL" "${args[@]}"
}

workflow_start() {
  workflow_enabled || return 0
  workflow_update "$1" RUNNING || return $?
  printf '\n[安装阶段] %s — 正在检查或执行\n' "$1"
}

workflow_done() {
  workflow_enabled || return 0
  workflow_update "$1" DONE || return $?
  printf '[安装阶段] %s — 完成（本轮已核验）\n' "$1"
}

workflow_was_done() {
  workflow_enabled || return 1
  local previous="${UIP_INSTALL_PREVIOUS_STATUS:-}"
  [[ -n "$previous" ]] || previous='{}'
  python3 - "$1" "$previous" <<'PY'
import json, sys
data = json.loads(sys.argv[2])
stages = data.get('stages', [])
if isinstance(stages, dict):
    stage = stages.get(sys.argv[1], {})
else:
    stage = next((s for s in stages if s.get('id', s.get('key', s.get('stage'))) == sys.argv[1]), {})
ever_done = stage.get('status') == 'DONE' or any(
    event.get('stage') == sys.argv[1] and event.get('status') == 'DONE'
    for event in data.get('history', []))
raise SystemExit(0 if ever_done else 1)
PY
}

workflow_run() {
  local stage="$1"; shift
  workflow_start "$stage" || return $?
  "$@" || return $?
  workflow_done "$stage"
}

workflow_publish() {
  local stage="$1" proof="$2" action="$3"; shift 3
  workflow_start "$stage" || return $?
  if workflow_was_done "$stage"; then
    if "$proof" "$@"; then
      printf '[恢复核验] %s 的本次目标镜像及当前运行状态一致，保留现有进程。\n' "$stage"
    else
      printf '[恢复阻断] %s 曾完成，但当前真实状态与本次交付不符；请先修复运行状态后 --resume。不会凭日志重放发布或迁移。\n' "$stage" >&2
      return 1
    fi
  else
    "$action" "$@" || return $?
    "$proof" "$@" || { printf '[验收失败] %s 尚未满足本次交付的实际状态证明。\n' "$stage" >&2; return 1; }
  fi
  workflow_done "$stage"
}

workflow_import() {
  local stage="$1" proof="$2" action="$3"; shift 3
  workflow_start "$stage" || return $?
  if "$proof" "$@"; then
    printf '[恢复核验] %s 的包记录、本机镜像及本地仓库对象一致。\n' "$stage"
  else
    # 重新导入确切冻结包可修复被驱逐缓存，不会发布服务或重放数据库迁移。
    "$action" "$@" || return $?
    "$proof" "$@" || return 1
  fi
  workflow_done "$stage"
}

workflow_wait() {
  local stage="$1" reason="$2"
  workflow_update "$stage" WAITING "$reason" || return $?
  printf '\n[安装暂停] %s；已完成成果保留。\n' "$reason"
  workflow_summary
}

workflow_summary() {
  workflow_enabled || return 0
  python3 "$UIP_INSTALL_SESSION_TOOL" status --state-dir "$UIP_INSTALL_STATE_DIR" --format human
  printf '服务器查看：bash %q --target %q --status\n' "$UIP_INSTALL_ENTRY" "$UIP_INSTALL_TARGET"
  printf '服务器继续：使用原安装命令并追加 --resume（介质、模式和选项须保持一致）。\n'
}

workflow_exit() {
  local result=$? stage
  trap - EXIT
  if workflow_enabled && ((result != 0)); then
    stage="$(python3 "$UIP_INSTALL_SESSION_TOOL" status --state-dir "$UIP_INSTALL_STATE_DIR" --format json | \
      python3 -c 'import json,sys; d=json.load(sys.stdin); c=d.get("current_stage"); print(c if any(s.get("status")=="RUNNING" and s.get("id",s.get("key",s.get("stage")))==c for s in d.get("stages",[])) else "")')" || stage=''
    if [[ -n "$stage" ]]; then
      workflow_update "$stage" FAILED COMMAND_FAILED || printf '[警告] 无法写入失败状态；请保留会话目录。\n' >&2
    fi
    workflow_summary || true
  fi
  exit "$result"
}

workflow_open() {
  local identity_file identity_directory previous result
  need python3
  [[ -n "${delivery_session_tool:-}" ]] || die '交付缺少 deployment-session.py，请使用完整新版部署工具'
  UIP_INSTALL_SESSION_TOOL="$delivery_session_tool"
  UIP_INSTALL_STATE_DIR="${target_dir}.installation-state"
  UIP_INSTALL_ENTRY="$(cd -- "$script_dir" && pwd -P)/$(basename -- "$script_path")"
  UIP_INSTALL_TARGET="$target_dir"
  previous='{}'
  if previous="$(python3 "$delivery_session_tool" status --state-dir "$UIP_INSTALL_STATE_DIR" --format json)"; then :
  else
    result=$?
    ((result == 3)) || die '安装会话不安全或损坏，拒绝覆盖；请检查 --status'
    previous='{}'
  fi
  # 父目录安全由会话读取器检查。O_NOFOLLOW、O_EXCL 及链接数、归属和权限检查，防止此独立长期锁截断或跟随无关文件；
  # 须在 begin 推进轮次之前获取。
  python3 - "${target_dir}.installation-run.lock" <<'PY'
import os, stat, sys
path=sys.argv[1]
flags=os.O_RDWR|os.O_NOFOLLOW
try: fd=os.open(path,flags|os.O_CREAT|os.O_EXCL,0o600)
except FileExistsError: fd=os.open(path,flags)
try:
    item=os.fstat(fd)
    if not stat.S_ISREG(item.st_mode) or item.st_nlink!=1 or item.st_uid!=os.geteuid() or stat.S_IMODE(item.st_mode)!=0o600:
        raise SystemExit('安装执行锁不安全，拒绝继续')
finally: os.close(fd)
PY
  exec 6<>"${target_dir}.installation-run.lock"
  flock -n 6 || die '同一目标已有安装流程运行；请等待后查看 --status'
  identity_directory="$(mktemp -d "${TMPDIR:-/tmp}/uip-install-identity.XXXXXX")"
  identity_file="$identity_directory/identity.json"
  if ! python3 - "$identity_file" "$target_dir" "$media_dir" "$deployment_mode" \
    "$skip_host_deps" "$skip_configure" "$apply_compose" "$legacy_license_mode" \
    "$license_customer" "$license_file_arg" "${workflow_components[@]}" <<'PY'
import hashlib, json, os, sys
out,target,media,mode,skipdeps,skipconf,apply,legacy,customer,licensepath,*components=sys.argv[1:]
data={'version':1,'target':os.path.realpath(target),'media':os.path.realpath(media),
      'delivery_id':'sha256:'+hashlib.sha256(open(os.path.join(media,'SHA256SUMS'),'rb').read()).hexdigest(),
      'mode':mode,'components':sorted(set(components)),
      'options':{'skip_host_deps':skipdeps=='true','skip_configure':skipconf=='true',
                 'apply_compose':apply=='true','legacy_license_mode':legacy,
                 'customer':customer,'license_file_path':os.path.realpath(licensepath) if licensepath else '',
                 'license_file_sha256':hashlib.sha256(open(licensepath,'rb').read()).hexdigest() if licensepath else ''}}
with open(out,'w',encoding='utf-8') as stream: json.dump(data,stream,ensure_ascii=False)
PY
  then rm -rf -- "$identity_directory"; die '无法生成安装会话身份'; fi
  local -a begin_args=(begin --state-dir "$UIP_INSTALL_STATE_DIR" --identity-file "$identity_file" --stages "$workflow_stages")
  [[ "$resume_installation" != true ]] || begin_args+=(--resume)
  [[ "${new_installation_session:-false}" != true ]] || begin_args+=(--new-session)
  if UIP_INSTALL_SESSION="$(python3 "$delivery_session_tool" "${begin_args[@]}")"; then :
  else result=$?; rm -rf -- "$identity_directory"; exit "$result"; fi
  rm -rf -- "$identity_directory"
  [[ "${new_installation_session:-false}" != true ]] || previous='{}'
  # 等待可信 Agent 时绝不持有 runtime/.deploy.lock。
  UIP_INSTALL_PREVIOUS_STATUS="$previous"
  export UIP_INSTALL_SESSION_TOOL UIP_INSTALL_STATE_DIR UIP_INSTALL_SESSION UIP_INSTALL_ENTRY UIP_INSTALL_TARGET UIP_INSTALL_PREVIOUS_STATUS
  trap workflow_exit EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM
}
