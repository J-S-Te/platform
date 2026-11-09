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
  local -a extra=()
  case "$action" in
    # 导入现在默认自动激活；但打包安装先导入、等业务镜像导入和候选准备完成后再显式
    # activate。若这里也自动激活，就会在业务镜像尚未就绪时提前采用并等待部署确认。
    import) extra+=(--no-activate) ;;
    activate) ;;
    *) die '不支持的交付授权操作' ;;
  esac
  [[ -d "$deploy_dir/license" && ! -L "$deploy_dir/license" && -f "$deploy_dir/license/commercial-license.jws" && ! -L "$deploy_dir/license/commercial-license.jws" ]] || die '交付许可证必须是安全常规文件'
  if ! compose run -T --rm --no-deps -v "$deploy_dir/license:/delivery-license:ro" platform-api \
    ./license-install "$action" --file /delivery-license/commercial-license.jws ${extra[@]+"${extra[@]}"}; then
    printf '自动授权暂停（%s）；已完成的安装状态保留。排查明确错误后重试 install，不会重新起算期限。\n' "$action" >&2
    return 1
  fi
}

# license_runtime_gate 在**宣布部署成功之前**核对许可运行态：API 容器健康、许可状态卷
# 挂载、许可实例与平台库一致、受控部署已确认。现场故障（project_management 的 API 容器
# 少了 /var/lib/commercial-license 卷挂载 → 启动即 commercial license configuration
# failed → 每分钟重启 → 平台只显示「部署 Agent 执行失败」，重试 7 次无效）说明这些前提
# 过去是隐含的：重试既不会补卷，也不会纠正实例。因此这里 fail-closed，并把处置顺序写进输出。
# 参数：可选应用编码（project_management 等），以及 --json / --dry-run 等透传给门禁的选项；
#       不带应用编码时检查全部已登记商业应用。
# 返回值：通过返回零；任一检查未通过返回非零，由调用方中止，不得继续打印成功结论。
license_runtime_gate() {
  local apps="" argument explicit_apps=false
  local -a passthrough=() codes=()
  # 取值型选项必须连同它的值一起透传，否则值会被误当成应用编码。
  while (($# > 0)); do
    argument="$1"
    shift
    case "$argument" in
      --deploy-dir|--compose-project|--apps|--fixture|--verify-timeout)
        [[ "$argument" != --apps ]] || explicit_apps=true
        (($# > 0)) || die "$argument 缺少取值"
        passthrough+=("$argument" "$1")
        shift
        ;;
      -*) passthrough+=("$argument") ;;
      *) codes+=("$argument") ;;
    esac
  done
  if ((${#codes[@]} != 0)); then
    [[ "$explicit_apps" == false ]] || die '应用编码与 --apps 只能给出一种形式'
    apps="$(IFS=,; printf '%s' "${codes[*]}")"
  fi
  require_initialized
  local -a gate_args=(--deploy-dir "$deploy_dir" --compose-project "$(scope_project)")
  ((${#passthrough[@]} == 0)) || gate_args+=("${passthrough[@]}")
  [[ -z "$apps" ]] || gate_args+=(--apps "$apps")
  if python3 "$script_dir/check-license-runtime.py" "${gate_args[@]}"; then
    return 0
  fi
  printf '错误：许可运行态门禁未通过，未判定部署成功。\n' >&2
  printf '处置顺序：导入并激活许可 → 重新执行接入/受控更新 → %s license-runtime 复核 → 再宣布部署成功。\n' "$0" >&2
  printf '不要重复点击「重试」接入，也不要手工修改固定记录：它们都不会补上缺失的许可状态卷，也不会纠正许可实例。\n' >&2
  return 1
}

# preserve_renewal_license 把已导入平台、但未能替换为正式交付件的新许可证复制到可写的
# runtime/ 作为恢复件。交付许可证目录整体以只读方式挂载进容器，替换失败本身也可能来自
# 宿主机许可目录不可写，因此恢复件优先落在 runtime/；复制失败时不动暂存件，由调用方打印
# 暂存件路径。成功时输出恢复件的绝对路径，失败时返回非零且不留下空文件。
preserve_renewal_license() {
  local staged="${1:?暂存许可证必填}" recovery
  if [[ ! -d "$deploy_dir/runtime" ]]; then
    mkdir -p -- "$deploy_dir/runtime" || return 1
  fi
  recovery="$(mktemp "$deploy_dir/runtime/.commercial-license.recovery.XXXXXX")" || return 1
  if install -m 600 -- "$staged" "$recovery"; then
    printf '%s\n' "$recovery"
    return 0
  fi
  rm -f -- "$recovery"
  return 1
}

renew_delivery_license() {
  local source="${1:?必须提供已签署续期许可证路径}" source_dir source_name staged recovery
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
    # 失败时是否保留暂存件由 staged 是否被清空决定：导入失败可以清理，替换失败必须保留。
    trap '[[ -z "${staged:-}" ]] || rm -f -- "$staged"' EXIT
    install -m 600 -- "$source_dir/$source_name" "$staged"
    # 续期导入只持久化签名续期；等原子替换为正式许可证文件后，再由下面一次显式
    # activate 完成整条激活流水线，避免对临时暂存文件激活或重复执行同一流水线。
    compose run -T --rm --no-deps -v "$deploy_dir/license:/delivery-license:ro" platform-api \
      ./license-install import --no-activate --file "/delivery-license/$(basename -- "$staged")" || exit 1
    [[ ! -L "$deploy_dir/license/commercial-license.jws" ]] || die '现有许可证不能是符号链接'
    if ! mv -f -- "$staged" "$deploy_dir/license/commercial-license.jws"; then
      # 平台已持久化本次续期；此时删除暂存件会让唯一的新证消失，下次续期又会和旧交付件
      # 冲突（409）。因此保留新证、只打印绝对路径与可直接执行的恢复命令，并且不继续激活。
      recovery="$(preserve_renewal_license "$staged")" || recovery=""
      printf '错误：续期许可证已导入平台，但替换交付许可证文件失败；未执行激活。\n' >&2
      printf '恢复件（0600 普通文件）：%s\n' "${recovery:-$staged}" >&2
      [[ -z "$recovery" ]] || printf '暂存副本（0600）：%s\n' "$staged" >&2
      printf '恢复命令：install -m 600 -- %q %q && %q license-activate\n' \
        "${recovery:-$staged}" "$deploy_dir/license/commercial-license.jws" "$script_dir/deploy.sh" >&2
      printf '先排除许可目录或 runtime 目录不可写、只读挂载等原因再执行；不要删除恢复件，也不要重复导入同一续期许可证。\n' >&2
      staged=''
      exit 1
    fi
  ) || return 1
  install_delivery_license activate
}
