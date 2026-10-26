#!/usr/bin/env bash
# License install/renew helpers. Sourced by deploy.sh.

prepare_license_installation() {
  local plan="$deploy_dir/license-installation.json" scenario customer
  [[ -f "$plan" && ! -L "$plan" ]] || die '安装场景配置必须是普通文件'
  [[ "$(wc -c < "$plan" | tr -d ' ')" -le 4096 ]] || die '安装场景配置超限'
  jq -e -s 'length == 1 and (.[0] | .version == 1 and (.scenario == "fresh" or .scenario == "migrate" or .scenario == "platform-only" or .scenario == "expand") and (.customer_id | type == "string" and test("^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$")) and (keys | sort == ["customer_id","scenario","version"]))' "$plan" >/dev/null || die '安装场景配置无效'
  scenario="$(jq -er '.scenario' "$plan")"
  customer="$(jq -er '.customer_id' "$plan")"
  local -a command=(run -T --rm --no-deps)
  if [[ -e "$deploy_dir/license" || -L "$deploy_dir/license" ]]; then
    [[ -d "$deploy_dir/license" && ! -L "$deploy_dir/license" && -f "$deploy_dir/license/commercial-license.jws" && ! -L "$deploy_dir/license/commercial-license.jws" ]] || die '交付许可证不是安全普通文件'
    command+=(-v "$deploy_dir/license:/delivery-license:ro")
  fi
  command+=(platform-api ./license-install prepare --scenario "$scenario" --customer "$customer")
  [[ ! -d "$deploy_dir/license" ]] || command+=(--file /delivery-license/commercial-license.jws)
  # 可以先升级平台控制面，但必须等可信 Agent 冻结仍运行的旧安装后，才能开始业务导入或更新。
  compose "${command[@]}" || { printf '升级前基线冻结失败，禁止更新业务镜像；现有业务保持运行。\n' >&2; return 1; }
}

install_delivery_license() {
  local action="${1:?授权操作必填}"
  case "$action" in import|activate) ;; *) die '不支持的交付授权操作' ;; esac
  [[ -d "$deploy_dir/license" && ! -L "$deploy_dir/license" && -f "$deploy_dir/license/commercial-license.jws" && ! -L "$deploy_dir/license/commercial-license.jws" ]] || die '交付许可证必须是安全常规文件'
  if ! compose run -T --rm --no-deps -v "$deploy_dir/license:/delivery-license:ro" platform-api \
    ./license-install "$action" --file /delivery-license/commercial-license.jws; then
    printf '自动授权暂停（%s）；已完成的安装状态保留。排查明确错误后重试 install，不会重新起算期限。\n' "$action" >&2
    return 1
  fi
}

renew_delivery_license() {
  local source="${1:?必须提供已签署续期许可证路径}" source_dir source_name staged
  require_initialized
  [[ -f "$source" && ! -L "$source" ]] || die '续期许可证必须是常规文件'
  source_dir="$(cd -- "$(dirname -- "$source")" && pwd)"
  source_name="$(basename -- "$source")"
  [[ "$source_name" != *:* && "$source_dir" != *:* ]] || die '续期路径不能包含挂载分隔符'
  [[ -d "$deploy_dir/license" && ! -L "$deploy_dir/license" ]] || die '只能续期已交付授权安装'
  # 使用独立锁，避免激活期间持有 Agent 部署锁。
  [[ ! -L "$deploy_dir/runtime/.license-import.lock" ]] || die '授权锁不能是符号链接'
  (
    exec 8>"$deploy_dir/runtime/.license-import.lock"
    flock -w 5 8 || die '另一个授权操作正在执行'
    staged="$(mktemp "$deploy_dir/license/.renewal.XXXXXX")"
    trap 'rm -f -- "$staged"' EXIT
    install -m 600 -- "$source_dir/$source_name" "$staged"
    compose run -T --rm --no-deps -v "$deploy_dir/license:/delivery-license:ro" platform-api \
      ./license-install import --file "/delivery-license/$(basename -- "$staged")" || exit 1
    [[ ! -L "$deploy_dir/license/commercial-license.jws" ]] || die '现有许可证不能是符号链接'
    mv -f -- "$staged" "$deploy_dir/license/commercial-license.jws"
  ) || return 1
  install_delivery_license activate
}
