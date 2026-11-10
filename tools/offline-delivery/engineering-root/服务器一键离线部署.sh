#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

script_path="${BASH_SOURCE[0]}"
script_dir="$(cd -- "$(dirname -- "$script_path")" && pwd)"
media_input=""
target_dir="/opt/unified-identity-platform"
check_only=false
check_delivery=false
show_installation_status=false
resume_installation=false
new_installation_session=false
delivery_output_format=human
skip_host_deps=false
skip_configure=false
assume_yes=false
forward_recovery=false
recovery_evidence=""
plan_baseline=""
add_subsystems_raw=""
upgrade_subsystems_raw=""
apply_compose=false
license_customer=""
license_file_arg=""
deployment_mode=auto
interactive_upgrade=false
legacy_license_mode=""
legacy_license_customer=""
export_license_inventory=""
require_release_integrity=false
release_trust_dir=/etc/unified-identity-platform/release-trust
release_verifier=/usr/local/bin/release-verifier

usage() {
  cat <<'EOF'
用法：./服务器一键离线部署.sh [选项]

选项：
  --media <目录或总归档.tar>  指定离线交付介质；默认自动发现
  --target <目录>                 安装目录；默认 /opt/unified-identity-platform
  --export-license-inventory <新JSON文件>
                                 只读导出完整旧安装运行信息供厂商准备迁移批准；
                                 不需要交付介质，不更新容器、配置或数据库
  --check-only                    检查介质、系统、资源、Docker 和隔离资产安装
  --check-delivery                统一核对镜像、资产、组件清单和批准材料；
                                 可在打包电脑执行，无需 Docker、root 或服务器
  --format <human|json>           --check-delivery / --status 的输出格式
  --status                       只读查看目标的安装会话、缺项及下一步执行位置
  --resume                       按原介质、模式和选项继续同一安装会话；
                                 重新核验已完成阶段的真实状态，不重置数据或身份
  --new-session                  旧会话完成或显式放弃后，归档并从全部门禁开启新批次；
                                 首装目标仍不得存在 .env 或 docker-compose.yml
  --skip-host-deps                不安装交付中的 Ubuntu 宿主依赖
  --skip-configure                只允许复用已存在的 .env/.release.env
  --install                      显式首装；目标已有部署时拒绝执行
  --add-subsystems <列表>        向已部署环境增量追加指定业务子系统
  --add-components <列表>        --add-subsystems 的兼容别名
  --upgrade                      升级已部署环境：装资产、合并编排、重载控制面、
                                 发布平台与前端（复用现有 .env/.release.env）
  --upgrade-subsystems <列表>    --upgrade 时一并受控升级这些已部署子系统
                                 （导入 + 暂存，之后仍需在平台页面受控更新）
  --apply-compose                --upgrade 时自动套用"新编排 + 保留现有模块裁剪"
                                 （不加此项只生成 docker-compose.yml.merged 供审阅）
  --forward-recovery             显式选择前向恢复；恢复兼容性未知/不兼容时仍须提供材料
  --recovery-evidence <文件>     前向恢复材料：备份 SHA256、迁移目标、审核人、批准时间、步骤
  --plan-baseline <绝对路径>     升级：保存/复用 check-only 计划 JSON，执行前比较 .env 指纹
  --customer <客户标识>          升级完成后提示核对商业授权实例及导出签发申请
                                 （提示在平台核对身份及导出申请，不自动初始化）
  --license <commercial-license.jws>
                                 升级完成后直接导入已签发许可证（验签+展示明细+提交）
  --legacy-license-bridge <客户标识>
                                 旧版无授权兼容升级：只发布基础平台，保留旧业务；
                                 完成后提示导出真实实例申请
  --legacy-license-upgrade <客户标识>
                                 旧版无授权迁移升级：必须选择 --upgrade-subsystems，
                                 校验 migrate 交付计划和旧/新批准；平台发布后先冻结
                                 旧范围，再导入并暂存所选业务；后续在平台受控更新
                                 和执行 license-migrate 确认，不自动激活整个购买集合
  --yes                          自动确认资源警告（不确认升级或编排差异）
  --require-release-integrity    启用发布完整性门禁；在任何介质脚本执行前
                                 调用独立受信的 release-verifier 验签
  --release-verifier <路径>      受信 verifier 绝对路径（默认 /usr/local/bin/release-verifier）
  --release-trust-dir <目录>     固定信任目录（默认 /etc/unified-identity-platform/release-trust）
  -h, --help                     显示帮助

不会自动执行：
  子系统的平台受控采用、删除旧数据、删除卷、Docker prune、关闭安全校验。

无模式参数时：检查目标目录，空目标走首装，完整既有部署走升级；残缺部署拒绝执行。
自动升级会在执行前及编排有差异时交互确认，确认后在同一次运行内继续。
默认目标仍为 /opt/unified-identity-platform；其他安装位置必须指定 --target。

直接运行示例（本目录只放本次交付包及 SHA256 伴随文件）：
  cd /root/uip-delivery
  bash ./服务器一键离线部署.sh

增量追加示例（不重装基础平台，不再询问 IP）：
  ./服务器一键离线部署.sh --media <交付目录或总归档> \
    --add-subsystems customer-opportunity

升级示例（先审阅编排差异，再落地）：
  ./服务器一键离线部署.sh --media <交付目录或总归档> --upgrade
  ./服务器一键离线部署.sh --media <交付目录或总归档> --upgrade --apply-compose
  ./服务器一键离线部署.sh --media <交付目录或总归档> --upgrade --apply-compose \
    --upgrade-subsystems customer-opportunity

普通升级+商业授权提示示例（旧版迁移请使用下面的专用入口）：
  ./服务器一键离线部署.sh --media <交付目录或总归档> --upgrade --apply-compose \
    --upgrade-subsystems customer-opportunity \
    --customer customer-a                        # 升级后出签发申请（去签发工具签发 JWS）
  ./服务器一键离线部署.sh --media <交付目录或总归档> --upgrade --apply-compose \
    --customer customer-a --license /root/commercial-license.jws   # 直接导入已签发许可证

旧版无授权对应入口（先审阅编排，或明确加 --apply-compose 批准合并）：
  ./服务器一键离线部署.sh --media <兼容基础平台介质> \
    --legacy-license-bridge gays-sq --apply-compose
  ./服务器一键离线部署.sh --media <审核后的 migrate 介质> \
    --legacy-license-upgrade gays-sq --upgrade-subsystems customer-opportunity --apply-compose
  上述命令可加 --check-only，只检查，不初始化/冻结或发布。
  migrate 介质可无 JWS；如需随包交付，厂商打包时使用 --license-file。
  两个入口均禁止 --license；业务更新完成后单独明确执行授权激活。

提示：本脚本自身也在交付介质 SHA256SUMS 清单内。更新工具版本时请把新脚本放在
      介质目录之外运行（--media 指向未被改动的介质），否则介质校验会因清单不匹配
      而 fail-closed。
安装进度保存在 <安装目录>.installation-state（0700，状态文件0600）；
失败后先 --target <原目录> --status，再使用原命令追加 --resume。
待授权/待平台采用会显示 WAITING，基础平台就绪不等于全部业务已正式激活。

可用子系统：
  customer-opportunity, customer-portal, contract, project, settlement, data-analysis
EOF
}

die() { printf '[错误] %s\n' "$*" >&2; exit 1; }
info() { printf '[信息] %s\n' "$*"; }
warn() { printf '[警告] %s\n' "$*" >&2; }
need() { command -v "$1" >/dev/null 2>&1 || die "缺少命令：$1"; }

delivery_tool_path() {
  local name="$1" candidate
  # 辅助工具随本入口一并交付，
  # 不得从未经验证的介质归档中动态提取代码执行。
  for candidate in "$script_dir/platform/deploy/production/bin/$name" "$script_dir/$name" "$target_dir/bin/$name"; do
    if [[ -f "$candidate" && ! -L "$candidate" ]]; then printf '%s\n' "$candidate"; return 0; fi
  done
  return 1
}

configure_legacy_license_mode() {
  [[ -n "$legacy_license_mode" ]] || return 0
  [[ "$legacy_license_customer" =~ ^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$ ]] || die '旧版授权入口需要有效客户标识'
  [[ "$deployment_mode" == auto || "$deployment_mode" == upgrade ]] || die '旧版授权入口只能升级既有部署'
  [[ -z "$license_customer" || "$license_customer" == "$legacy_license_customer" ]] || die '旧版授权入口与 --customer 客户不一致'
  [[ -z "$license_file_arg" ]] || die '旧版授权入口不接受 --license；随包许可证由 migrate 介质交付，业务更新后单独激活'
  deployment_mode=upgrade
  license_customer="$legacy_license_customer"
  if [[ "$legacy_license_mode" == bridge ]]; then
    [[ -z "$upgrade_subsystems_raw" ]] || die 'bridge 只升级基础平台，不能同时 --upgrade-subsystems'
  else
    [[ -n "$upgrade_subsystems_raw" ]] || die '旧版授权迁移必须用 --upgrade-subsystems 显式指定旧业务系统'
  fi
}

subsystem_application_of() {
  case "$1" in
    customer-opportunity) printf '%s\n' customer_and_opportunity ;;
    customer-portal) printf '%s\n' customer_portal ;;
    contract) printf '%s\n' contract_management ;;
    project) printf '%s\n' project_management ;;
    settlement) printf '%s\n' settlement ;;
    data-analysis) printf '%s\n' data_analysis ;;
    *) die "不支持的授权迁移子系统：$1" ;;
  esac
}

validate_legacy_approval_file() {
  local file="$1"
  [[ -f "$file" && ! -L "$file" ]] || die "旧版授权迁移缺少安全批准文件：$(basename -- "$file")"
  [[ "$(wc -c < "$file" | tr -d ' ')" -le 2097152 ]] || die '旧版授权迁移批准文件超过2MiB'
  jq -e -s 'length == 1 and (.[0] | type == "object")' "$file" >/dev/null || die '旧版授权迁移批准必须是单个JSON对象'
}

validate_legacy_license_assets() {
  local root="$1" plan="$1/license-installation.json" approvals="$1/runtime-approvals" component app file
  [[ -n "$legacy_license_mode" ]] || return 0
  if [[ "$legacy_license_mode" == bridge ]]; then
    [[ ! -e "$plan" && ! -L "$plan" && ! -e "$root/license" && ! -L "$root/license" ]] ||
      die 'bridge 介质不能携带授权安装计划或许可证；请使用兼容基础平台包'
    [[ -z "$(find "$approvals" -mindepth 1 -maxdepth 1 -print -quit)" ]] || die 'bridge 介质不能携带迁移或运行批准'
    return 0
  fi
  [[ -f "$plan" && ! -L "$plan" ]] || die '旧版授权迁移介质缺少安全 license-installation.json'
  [[ "$(wc -c < "$plan" | tr -d ' ')" -le 4096 ]] || die '旧版授权迁移安装计划超过4096字节'
  jq -e -s --arg customer "$legacy_license_customer" 'length == 1 and (.[0] | .version == 1 and .scenario == "migrate" and .customer_id == $customer and (keys | sort == ["customer_id","scenario","version"]))' "$plan" >/dev/null ||
    die '旧版授权迁移计划必须是 migrate 且客户与命令一致'
  grep -Eq '^[[:space:]]*license-prepare\)' "$root/bin/deploy.sh" &&
    grep -Eq '^[[:space:]]*license-migrate\)' "$root/bin/deploy.sh" || die '迁移介质缺少新版授权维护入口，请重新打包'
  validate_legacy_approval_file "$approvals/license-evidence.json"
  validate_legacy_approval_file "$approvals/license-migration-evidence.json"
  jq -e '.version == 1 and .installation_boundary == true and (.project | type == "string" and length > 0) and (.services | type == "array" and length > 0) and all(.services[]; .environment == "prod")' "$approvals/license-migration-evidence.json" >/dev/null || die '迁移介质必须批准实际旧业务安装边界'
  for component in "${requested_subsystems[@]}"; do
    app="$(subsystem_application_of "$component")"
    jq -e --arg app "$app" 'any(.services[]; .application == $app and .environment == "prod")' "$approvals/license-migration-evidence.json" >/dev/null || die "请求系统不是批准的旧安装：${component}；新增系统不能使用旧版迁移入口"
    file="$approvals/runtime-license-${app}-prod.json"
    validate_legacy_approval_file "$file"
    jq -e --arg app "$app" '.application == $app and .environment == "prod"' "$file" >/dev/null || die "新版运行批准与请求系统不一致：$component"
  done
}

resolve_deployment_mode() {
  local required_path has_deployment=false
  [[ "$deployment_mode" == auto ]] || return 0
  for required_path in docker-compose.yml .env .release.env bin/deploy.sh runtime/.control-plane-reload-required; do
    if [[ -e "$target_dir/$required_path" || -L "$target_dir/$required_path" ]]; then
      has_deployment=true
    fi
  done
  if [[ "$has_deployment" == false ]]; then
    deployment_mode=install
    info "未发现部署标识，选择首装：${target_dir}"
    return 0
  fi
  for required_path in docker-compose.yml .env .release.env bin/deploy.sh; do
    [[ -f "$target_dir/$required_path" && ! -L "$target_dir/$required_path" ]] ||
      die "发现既有或残缺部署，但缺少安全普通文件 ${target_dir}/${required_path}；拒绝自动首装，请先核对原部署目录或恢复"
  done
  [[ -x "$target_dir/bin/deploy.sh" ]] || die "既有部署入口不可执行：${target_dir}/bin/deploy.sh"
  deployment_mode=upgrade
  interactive_upgrade=true
  info "发现既有部署，选择升级并复用现有配置：${target_dir}"
}

confirm_upgrade_action() {
  local prompt="$1" answer
  [[ -t 0 ]] || die '自动升级需要交互确认；非交互发布请明确指定 --upgrade，编排批准另用 --apply-compose'
  read -r -p "${prompt} 输入 UPGRADE 确认，其他输入取消：" answer || die '未收到升级确认，已停止'
  [[ "$answer" == UPGRADE ]] || die '已取消升级；未绕过任何部署门禁'
}

backup_before_upgrade() {
  local backup_entry="$target_dir/bin/backup-all.sh"
  [[ -f "$backup_entry" && ! -L "$backup_entry" ]] ||
    die "既有部署缺少安全完整备份入口：${backup_entry}；请先补齐并验证备份工具，不能跳过数据库备份升级"
  info '升级前强制完整备份：现有数据库、运行配置及文件网关恢复材料；文件网关可能短暂停写'
  if ! bash "$backup_entry"; then
    die '升级前完整备份失败，未开始导入平台候选或安装新部署资产；请检查容量、数据库状态及备份权限'
  fi
  info "升级前完整备份已通过；备份批次位于 ${target_dir}/backups/system/（敏感数据，请异机保管）"
}

install_operations_tool() {
  local source="$media_dir/系统运维工具.sh" temporary
  [[ -e "$source" || -L "$source" ]] || return 0
  [[ -f "$source" && ! -L "$source" ]] || die "系统运维入口不是安全普通文件：$source"
  temporary="$(mktemp "$target_dir/.系统运维工具.sh.XXXXXX")"
  if ! install -m 755 "$source" "$temporary"; then
    rm -f -- "$temporary"
    die '系统运维入口安装失败'
  fi
  mv -f -- "$temporary" "$target_dir/系统运维工具.sh"
  info "已安装/更新系统运维入口：${target_dir}/系统运维工具.sh"
}

# ---------------------------------------------------------------------------
# 升级辅助：编排合并
#
# 交付模板用 "# BEGIN SUBSYSTEM: <key>" / "# END SUBSYSTEM: <key>" 标记可选模块，
# 模块被裁剪时其内部所有行整体注释。升级时既不能把新模板直接覆盖到运营方编排上
# （会丢掉模块裁剪），也不能只把"看起来新增的几行"手工粘过去——新模板会把既有
# 段落搬位置（例如 runtime-approvals 挂载在服务之间移动），漏掉就会让 Agent 读不到
# 批准文件。因此这里反过来做：解析现有编排的模块启用状态，把同一状态套用到新模板。
# ---------------------------------------------------------------------------
package_component_of() {
  case "$(basename -- "$1")" in
    common-infrastructure-linux-amd64.tar.gz) printf '%s\n' common ;;
    platform-backend-*-linux-amd64.tar.gz) printf '%s\n' platform ;;
    frontend-*-linux-amd64.tar.gz) printf '%s\n' frontend ;;
    customer-opportunity-backend-*-linux-amd64.tar.gz) printf '%s\n' customer-opportunity ;;
    customer-portal-backend-*-linux-amd64.tar.gz) printf '%s\n' customer-portal ;;
    contract-backend-*-linux-amd64.tar.gz) printf '%s\n' contract ;;
    project-backend-*-linux-amd64.tar.gz) printf '%s\n' project ;;
    settlement-backend-*-linux-amd64.tar.gz) printf '%s\n' settlement ;;
    data-analysis-backend-*-linux-amd64.tar.gz) printf '%s\n' data-analysis ;;
    *) printf '%s\n' unknown ;;
  esac
}

package_pattern_of() {
  case "$1" in
    common) printf '%s\n' 'common-infrastructure-linux-amd64.tar.gz' ;;
    platform) printf '%s\n' 'platform-backend-*-linux-amd64.tar.gz' ;;
    frontend) printf '%s\n' 'frontend-*-linux-amd64.tar.gz' ;;
    customer-opportunity) printf '%s\n' 'customer-opportunity-backend-*-linux-amd64.tar.gz' ;;
    customer-portal) printf '%s\n' 'customer-portal-backend-*-linux-amd64.tar.gz' ;;
    contract) printf '%s\n' 'contract-backend-*-linux-amd64.tar.gz' ;;
    project) printf '%s\n' 'project-backend-*-linux-amd64.tar.gz' ;;
    settlement) printf '%s\n' 'settlement-backend-*-linux-amd64.tar.gz' ;;
    data-analysis) printf '%s\n' 'data-analysis-backend-*-linux-amd64.tar.gz' ;;
    *) die "未知组件：$1" ;;
  esac
}

# 解析 BEGIN/END 标记之间的模块启用状态：块内所有非空行都被注释即视为"已裁剪"。
detect_subsystem_block_states() {
  local compose="$1" output="$2"
  if ! awk '
    function blk_key(line,   k) {
      k = line
      sub(/^[[:space:]]*#[[:space:]]*(BEGIN|END)[[:space:]]+SUBSYSTEM[[:space:]]*[:：][[:space:]]*/, "", k)
      sub(/[[:space:]].*$/, "", k)
      return k
    }
    function invalid() { bad = 1; exit 1 }
    /^[[:space:]]*#[[:space:]]*BEGIN[[:space:]]+SUBSYSTEM[[:space:]]*[:：]/ {
      key = blk_key($0)
      if (inblk || key !~ /^[a-z][a-z0-9-]*$/ || seen[key]++) invalid()
      inblk = 1; has_content = 0; next
    }
    /^[[:space:]]*#[[:space:]]*END[[:space:]]+SUBSYSTEM[[:space:]]*[:：]/ {
      if (!inblk || blk_key($0) != key) invalid()
      printf "%s=%s\n", key, (has_content ? "enabled" : "disabled")
      inblk = 0; next
    }
    inblk && $0 !~ /^[[:space:]]*($|#)/ { has_content = 1 }
    END { if (bad || inblk) exit 1 }
  ' "$compose" > "$output"; then
    rm -f -- "$output"
    die "编排模块标记无效（嵌套、重复、未闭合或 BEGIN/END 不匹配）：$compose"
  fi
  [[ -s "$output" ]] || die "无法从编排解析出子系统标记：$compose"
  [[ "$(awk -F= '$1 == "common" {print $2}' "$output")" == enabled ]] ||
    die "编排必须包含启用的 common 基础模块：$compose"
}

# 只保留原部署明确启用的模块；新出现的可选模块默认裁剪，避免扩大升级范围。
synthesize_compose_with_trim() {
  local dist="$1" states="$2" output="$3" template_states
  template_states="$(mktemp "${TMPDIR:-/tmp}/uip-compose-states.XXXXXX")" || die '无法创建编排预检临时文件'
  if ! (detect_subsystem_block_states "$dist" "$template_states"); then
    rm -f -- "$template_states"
    die "新编排模板标记检查失败：$dist"
  fi
  rm -f -- "$template_states"
  awk -v states="$states" '
    BEGIN {
      while ((getline line < states) > 0) {
        if (line == "") continue
        eq = index(line, "=")
        if (eq > 1) state[substr(line, 1, eq - 1)] = substr(line, eq + 1)
      }
      close(states)
    }
    function blk_key(line,   k) {
      k = line
      sub(/^[[:space:]]*#[[:space:]]*BEGIN[[:space:]]+SUBSYSTEM[[:space:]]*[:：][[:space:]]*/, "", k)
      sub(/[[:space:]].*$/, "", k)
      return k
    }
    /^[[:space:]]*#[[:space:]]*BEGIN[[:space:]]+SUBSYSTEM[[:space:]]*[:：]/ {
      current = (state[blk_key($0)] == "enabled") ? "enabled" : "disabled"
      print; next
    }
    /^[[:space:]]*#[[:space:]]*END[[:space:]]+SUBSYSTEM[[:space:]]*[:：]/ {
      current = ""; print; next
    }
    {
      if (current == "disabled" && $0 !~ /^[[:space:]]*$/ && $0 !~ /^[[:space:]]*#/) {
        match($0, /^[[:space:]]*/)
        print substr($0, 1, RLENGTH) "# " substr($0, RLENGTH + 1)
        next
      }
      print
    }
  ' "$dist" > "$output"
}

# 与 deploy.sh 内部一致的编排校验（必须带两个 env 文件，否则镜像变量无法插值）。
compose_config_check() {
  docker compose --project-directory "$target_dir" --file "$target_dir/docker-compose.yml" \
    --env-file "$target_dir/.env" --env-file "$target_dir/.release.env" config >/dev/null
}

upgrade_subsystem_block_key() {
  case "$1" in
    customer-opportunity) printf '%s\n' customer ;;
    customer-portal) printf '%s\n' portal ;;
    *) printf '%s\n' "$1" ;;
  esac
}

validate_requested_upgrade_blocks() {
  local states="$1" context="$2" component key
  for component in "${requested_subsystems[@]}"; do
    key="$(upgrade_subsystem_block_key "$component")"
    [[ "$(awk -F= -v key="$key" '$1 == key {print $2}' "$states")" == enabled ]] ||
      die "${context} 中 ${component} 未启用或缺少模块；不能受控升级，请先核对部署范围"
    if [[ "$component" == customer-portal ]]; then
      [[ "$(awk -F= '$1 == "customer" {print $2}' "$states")" == enabled ]] ||
        die "${context} 中 CRM 未启用，不能升级依赖 CRM 的客户门户"
    fi
  done
}

# 用随包安装器真实安装到隔离空目录，提前验证归档白名单和事务安装契约。
# 空目录没有 .env，因此安装器不会探测或重载现有控制面。
preflight_assets_install() {
  (
    set -Eeuo pipefail
    local scratch command_name
    for command_name in jq flock install; do
      command -v "$command_name" >/dev/null 2>&1 || die "部署资产预检缺少命令：$command_name"
    done
    scratch="$(mktemp -d "${TMPDIR:-/tmp}/uip-assets-preflight.XXXXXX")" || die '无法创建资产预检临时目录'
    trap 'rm -rf -- "$scratch"' EXIT
    unset ASSETS_PLATFORM_UPGRADE_IMAGE
    bash -n "$media_dir/install-assets.sh" || die '资产安装脚本语法检查失败'
    if ! bash "$media_dir/install-assets.sh" "${assets[0]}" "$scratch/deployment" > "$scratch/install.log" 2>&1; then
      cat "$scratch/install.log" >&2
      die '部署资产与随包安装器不兼容；尚未备份或导入镜像，请重新制作匹配的交付介质'
    fi
    [[ -f "$scratch/deployment/docker-compose.yml" && ! -L "$scratch/deployment/docker-compose.yml" &&
       -f "$scratch/deployment/bin/deploy.sh" && ! -L "$scratch/deployment/bin/deploy.sh" &&
       -x "$scratch/deployment/bin/deploy.sh" ]] || die '资产预检未生成完整部署入口和编排'
    bash -n "$scratch/deployment/bin/deploy.sh" || die '资产中的部署入口语法检查失败'
    for command_name in deployment-workflow.sh deployment-session.py delivery-runtime.sh delivery-runtime.py \
        delivery-inspect.py delivery-inspect-images.py delivery-inspect-v2.py delivery-files.py subsystem-runtime.py; do
      [[ -f "$scratch/deployment/bin/$command_name" && ! -L "$scratch/deployment/bin/$command_name" ]] ||
        die "资产缺少分阶段安装工具 bin/${command_name}；请重新制作匹配的平台及资产完整基线，不能将新版入口套在旧资产上"
    done
    [[ ! -e "$scratch/deployment/runtime/.assets-install-transaction" &&
       ! -L "$scratch/deployment/runtime/.assets-install-transaction" &&
       ! -e "$scratch/deployment/runtime/.control-plane-reload-required" &&
       ! -L "$scratch/deployment/runtime/.control-plane-reload-required" ]] || die '资产预检存在未完成的事务或重载门禁'
    if [[ upgrade == "$deployment_mode" ]]; then
      detect_subsystem_block_states "$scratch/deployment/docker-compose.yml" "$scratch/template.states"
      validate_requested_upgrade_blocks "$scratch/template.states" '交付编排模板'
    fi
    validate_legacy_license_assets "$scratch/deployment"
  ) || die '部署资产安装预检失败，未变更现有部署'
  info '部署资产与随包安装器的隔离安装预检通过'
}

preflight_upgrade_target() {
  (
    local required_path states component service services
    [[ -d "$target_dir" && ! -L "$target_dir" ]] || die "升级目标不是安全部署目录：$target_dir"
    for required_path in docker-compose.yml .env .release.env bin/deploy.sh bin/backup-all.sh; do
      [[ -f "$target_dir/$required_path" && ! -L "$target_dir/$required_path" ]] ||
        die "升级目标缺少安全普通文件：$target_dir/$required_path"
    done
    [[ -x "$target_dir/bin/deploy.sh" ]] || die "部署入口不可执行：$target_dir/bin/deploy.sh"
    for required_path in bin packages manifests runtime backups; do
      [[ ! -L "$target_dir/$required_path" && ( ! -e "$target_dir/$required_path" || -d "$target_dir/$required_path" ) ]] ||
        die "升级目标目录不安全：$target_dir/$required_path"
    done
    [[ ! -e "$target_dir/runtime/.assets-install-transaction" && ! -L "$target_dir/runtime/.assets-install-transaction" ]] ||
      die '检测到未完成的资产安装事务；请先用原交付包 install-assets.sh --recover 恢复'
    [[ ! -L "$target_dir/runtime/.deploy.lock" ]] || die '部署锁不能是符号链接'
    [[ ! -L "$target_dir/runtime/.control-plane-reload-required" &&
       ( ! -e "$target_dir/runtime/.control-plane-reload-required" || -f "$target_dir/runtime/.control-plane-reload-required" ) ]] || die '控制面重载标记不是安全普通文件'
    states="$(mktemp "${TMPDIR:-/tmp}/uip-compose-states.XXXXXX")" || die '无法创建编排预检临时文件'
    trap 'rm -f -- "$states"' EXIT
    detect_subsystem_block_states "$target_dir/docker-compose.yml" "$states"
    validate_requested_upgrade_blocks "$states" '现有编排'
    # 与底层 scope_require 相同：模块仍有其他服务时，块状态不能替代 API 服务存在性。
    services="$(docker compose --project-directory "$target_dir" --file "$target_dir/docker-compose.yml" \
      --env-file "$target_dir/.env" --env-file "$target_dir/.release.env" config --services)" || die '现有统一编排无法解析'
    for component in "${requested_subsystems[@]}"; do
      case "$component" in
        customer-opportunity) service=customer-api ;;
        customer-portal) service=portal-api ;;
        *) service="${component}-api" ;;
      esac
      grep -Fxq "$service" <<< "$services" || die "现有编排缺少 ${component} 的 ${service} 服务，不能受控升级"
      if [[ "$component" == customer-portal ]]; then
        grep -Fxq customer-api <<< "$services" || die '现有编排缺少客户门户依赖的 customer-api 服务'
      fi
    done
  ) || die '现有升级目标预检失败，未变更现有部署'
}

install_upgrade_package() {
  local source="$1" package_name component pattern existing name
  package_name="$(basename -- "$source")"
  component="$(package_component_of "$package_name")"
  pattern="$(package_pattern_of "$component")"
  local previous=()
  shopt -s nullglob
  previous=("$target_dir"/packages/$pattern)
  shopt -u nullglob
  for existing in "${previous[@]}"; do
    name="$(basename -- "$existing")"
    [[ -f "$existing" && ! -L "$existing" && ! -L "${existing}.sha256" ]] || die "既有镜像包不安全：$existing"
    # common 包名固定，名称相同也必须比较内容；相同内容可直接复用。
    if [[ "$name" == "$package_name" ]] && cmp -s "$source" "$existing"; then
      continue
    fi
    if [[ -z "$superseded_dir" ]]; then
      superseded_dir="$(mktemp -d "$target_dir/packages/superseded-$(date -u '+%Y%m%dT%H%M%SZ')-XXXXXX")" || die '无法创建旧镜像包归档目录'
    fi
    mv -- "$existing" "$superseded_dir/$name"
    [[ ! -f "${existing}.sha256" ]] || mv -- "${existing}.sha256" "$superseded_dir/${name}.sha256"
    info "同组件旧镜像包已归档：$name"
  done
  if [[ ! -f "$target_dir/packages/$package_name" ]]; then
    install -m 600 "$source" "$target_dir/packages/$package_name"
  fi
  install -m 600 "${source}.sha256" "$target_dir/packages/${package_name}.sha256"
}

while (($#)); do
  case "$1" in
    --media) media_input="${2:?--media 缺少参数}"; shift 2 ;;
    --target) target_dir="${2:?--target 缺少参数}"; shift 2 ;;
    --export-license-inventory)
      [[ -z "$export_license_inventory" ]] || die '不能重复指定 --export-license-inventory'
      export_license_inventory="${2:?--export-license-inventory 缺少输出路径}"; shift 2 ;;
    --check-only) check_only=true; shift ;;
    --check-delivery) check_delivery=true; shift ;;
    --status) show_installation_status=true; shift ;;
    --resume) resume_installation=true; shift ;;
    --new-session) new_installation_session=true; shift ;;
    --format) delivery_output_format="${2:?--format 缺少参数}"; shift 2 ;;
    --skip-host-deps) skip_host_deps=true; shift ;;
    --skip-configure) skip_configure=true; shift ;;
    --install)
      [[ "$deployment_mode" == auto || "$deployment_mode" == install ]] || die '--install 不能与其他部署模式同时使用'
      deployment_mode=install
      shift
      ;;
    --add-subsystems|--add-components)
      [[ -z "$add_subsystems_raw" ]] || die '不能重复指定 --add-subsystems/--add-components'
      [[ "$deployment_mode" == auto || "$deployment_mode" == add-subsystems ]] || die '--add-subsystems 不能与其他部署模式同时使用'
      add_subsystems_raw="${2:?$1 缺少参数}"
      deployment_mode=add-subsystems
      shift 2
      ;;
    --upgrade)
      [[ "$deployment_mode" == auto || "$deployment_mode" == upgrade ]] || die '--upgrade 不能与其他部署模式同时使用'
      deployment_mode=upgrade
      shift
      ;;
    --upgrade-subsystems)
      [[ -z "$upgrade_subsystems_raw" ]] || die '不能重复指定 --upgrade-subsystems'
      upgrade_subsystems_raw="${2:?$1 缺少参数}"
      shift 2
      ;;
    --apply-compose) apply_compose=true; shift ;;
    --forward-recovery) forward_recovery=true; shift ;;
    --recovery-evidence) (($# >= 2)) || die '--recovery-evidence 缺少文件参数'; recovery_evidence="$2"; shift 2 ;;
    --plan-baseline) (($# >= 2)) || die '--plan-baseline 缺少文件参数'; plan_baseline="$2"; shift 2 ;;
    --customer)
      [[ -z "$license_customer" ]] || die '不能重复指定 --customer'
      license_customer="${2:?$1 缺少参数}"
      shift 2
      ;;
    --license)
      [[ -z "$license_file_arg" ]] || die '不能重复指定 --license'
      license_file_arg="${2:?$1 缺少参数}"
      shift 2
      ;;
    --legacy-license-bridge|--legacy-license-upgrade)
      [[ -z "$legacy_license_mode" ]] || die '旧版授权入口只能指定一次，bridge 与 upgrade 不能混用'
      legacy_license_mode="${1#--legacy-license-}"
      legacy_license_customer="${2:?旧版授权入口缺少客户标识}"
      shift 2
      ;;
    --yes) assume_yes=true; shift ;;
    --require-release-integrity) require_release_integrity=true; shift ;;
    --release-verifier) release_verifier="${2:?--release-verifier 缺少参数}"; shift 2 ;;
    --release-trust-dir) release_trust_dir="${2:?--release-trust-dir 缺少参数}"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) usage >&2; die "未知参数：$1" ;;
  esac
done
[[ "$delivery_output_format" == human || "$delivery_output_format" == json ]] || die '--format 仅支持 human 或 json'
if [[ "$require_release_integrity" == true ]]; then
  [[ "$release_verifier" == /* ]] || die 'RELEASE_TRUST_UNAVAILABLE：--release-verifier 必须是绝对路径'
  [[ "$release_trust_dir" == /* ]] || die 'RELEASE_TRUST_UNAVAILABLE：--release-trust-dir 必须是绝对路径'
  [[ "$check_delivery" != true && "$show_installation_status" != true && -z "$export_license_inventory" ]] ||
    die '--require-release-integrity 不适用于 --check-delivery/--status/旧安装导出'
fi
if [[ "$check_delivery" == true || "$show_installation_status" == true ]]; then
  [[ "$check_delivery" != "$show_installation_status" ]] || die '--check-delivery 与 --status 不能同时指定'
  [[ "$check_only" == false && "$resume_installation" == false && "$new_installation_session" == false && -z "$export_license_inventory$add_subsystems_raw$upgrade_subsystems_raw$license_customer$license_file_arg$legacy_license_mode" &&
      "$deployment_mode" == auto && "$skip_host_deps" == false && "$skip_configure" == false && "$apply_compose" == false && "$assume_yes" == false ]] || die '只读检查/状态入口不能与安装、升级或恢复选项混用'
  need python3
  if [[ "$show_installation_status" == true ]]; then
    [[ -z "$media_input" ]] || die '--status 只需 --target，不读取交付介质'
    session_reader="$(delivery_tool_path deployment-session.py)" || die '缺少 deployment-session.py，请使用完整新版工具'
    status_target="$(python3 -c 'import os,sys; print(os.path.realpath(sys.argv[1]))' "$target_dir")"
    exec python3 "$session_reader" status --state-dir "${status_target}.installation-state" --format "$delivery_output_format"
  fi
  inspector="$(delivery_tool_path delivery-inspect.py)" || die '缺少 delivery-inspect.py，请使用完整新版工具'
  [[ -n "$media_input" ]] || media_input="$script_dir"
  exec python3 "$inspector" --media "$media_input" --phase prepare --format "$delivery_output_format"
fi
[[ "$delivery_output_format" == human ]] || die '--format 仅用于 --check-delivery 或 --status'
[[ "$new_installation_session" != true || "$resume_installation" != true ]] || die '--new-session 与 --resume 不能同时指定'
[[ "$new_installation_session" != true || "$check_only" != true ]] || die '--check-only 不开启新会话'
if [[ -n "$export_license_inventory" ]]; then
  [[ -z "$media_input$add_subsystems_raw$upgrade_subsystems_raw$license_customer$license_file_arg$legacy_license_mode" &&
      "$deployment_mode" == auto && "$check_only" == false && "$skip_host_deps" == false &&
      "$skip_configure" == false && "$apply_compose" == false && "$assume_yes" == false && "$resume_installation" == false && "$new_installation_session" == false ]] ||
    die '旧安装导出只能与 --target 配合，不能同时安装、升级或激活'
  [[ "$(uname -s)" == Linux ]] || die '旧安装导出须在目标 Linux 服务器执行'
  need python3
  need docker
  inventory_exporter=""
  for candidate_exporter in "$script_dir/platform/deploy/production/bin/export-license-inventory.py" \
      "$script_dir/export-license-inventory.py" "$target_dir/bin/export-license-inventory.py"; do
    if [[ -f "$candidate_exporter" && ! -L "$candidate_exporter" ]]; then
      inventory_exporter="$candidate_exporter"; break
    fi
  done
  [[ -n "$inventory_exporter" ]] || die '请把新版 export-license-inventory.py 与本脚本一起上传，或使用包含该工具的部署目录'
  exec python3 "$inventory_exporter" --target "$target_dir" --output "$export_license_inventory"
fi
configure_legacy_license_mode

if ((EUID != 0)); then
  command -v sudo >/dev/null 2>&1 || die '请使用 root 执行，当前系统没有 sudo'
  sudo_args=(--target "$target_dir")
  [[ -z "$media_input" ]] || sudo_args+=(--media "$media_input")
  [[ "$check_only" != true ]] || sudo_args+=(--check-only)
  [[ "$resume_installation" != true ]] || sudo_args+=(--resume)
  [[ "$new_installation_session" != true ]] || sudo_args+=(--new-session)
  [[ "$skip_host_deps" != true ]] || sudo_args+=(--skip-host-deps)
  [[ "$skip_configure" != true ]] || sudo_args+=(--skip-configure)
  [[ -z "$add_subsystems_raw" ]] || sudo_args+=(--add-subsystems "$add_subsystems_raw")
  [[ "$deployment_mode" != upgrade ]] || sudo_args+=(--upgrade)
  [[ "$deployment_mode" != install ]] || sudo_args+=(--install)
  [[ -z "$upgrade_subsystems_raw" ]] || sudo_args+=(--upgrade-subsystems "$upgrade_subsystems_raw")
  [[ "$apply_compose" != true ]] || sudo_args+=(--apply-compose)
  [[ "$forward_recovery" != true ]] || sudo_args+=(--forward-recovery)
  [[ -z "$recovery_evidence" ]] || sudo_args+=(--recovery-evidence "$recovery_evidence")
  [[ -z "$plan_baseline" ]] || sudo_args+=(--plan-baseline "$plan_baseline")
  [[ -z "$license_customer" ]] || sudo_args+=(--customer "$license_customer")
  [[ -z "$license_file_arg" ]] || sudo_args+=(--license "$license_file_arg")
  [[ -z "$legacy_license_mode" ]] || sudo_args+=("--legacy-license-${legacy_license_mode}" "$legacy_license_customer")
  [[ "$assume_yes" != true ]] || sudo_args+=(--yes)
  [[ "$require_release_integrity" != true ]] || sudo_args+=(--require-release-integrity)
  [[ "$release_verifier" == /usr/local/bin/release-verifier ]] || sudo_args+=(--release-verifier "$release_verifier")
  [[ "$release_trust_dir" == /etc/unified-identity-platform/release-trust ]] || sudo_args+=(--release-trust-dir "$release_trust_dir")
  exec sudo bash "$script_path" "${sudo_args[@]}"
fi

((BASH_VERSINFO[0] > 4 || (BASH_VERSINFO[0] == 4 && BASH_VERSINFO[1] >= 3))) || die '需要 Bash 4.3 或以上版本'
[[ "$(uname -s)" == Linux ]] || die '服务器部署只支持 Linux'
[[ "$(uname -m)" == x86_64 ]] || die "目标服务器必须是 x86_64/amd64，当前为 $(uname -m)"
[[ "$target_dir" == /* && "$target_dir" != / ]] || die '安装目录必须是非根目录的绝对路径'
[[ ! -L "$target_dir" ]] || die "安装目录不能是符号链接：${target_dir}"

# 发布完整性门禁先于介质脚本、依赖安装及目标目录变更执行。
# 验证器与信任目录由宿主机管理，
# 不得来自正在验证的介质。部署签名发布基线
# 和独立引导的验证器之前，此门禁须由操作者显式启用。
release_integrity_gate() {
  [[ "$require_release_integrity" == true ]] || return 0
  local candidate media_real verifier_real trust_real output
  candidate="$media_input"
  [[ -n "$candidate" ]] || candidate="$script_dir"
  case "$candidate" in /*) ;; *) candidate="$(pwd)/$candidate" ;; esac
  [[ -d "$candidate" && ! -L "$candidate" ]] ||
    die 'RELEASE_TRUST_UNAVAILABLE：启用发布完整性时 --media 必须是已解压目录（总归档请先解压）'
  media_real="$(cd -- "$candidate" && pwd -P)"
  [[ -f "$release_verifier" && -x "$release_verifier" && ! -L "$release_verifier" ]] ||
    die 'RELEASE_TRUST_UNAVAILABLE：受信 release-verifier 不可用'
  verifier_real="$(cd -- "$(dirname -- "$release_verifier")" && pwd -P)/$(basename -- "$release_verifier")"
  case "$verifier_real" in "$media_real"/*) die 'RELEASE_TRUST_UNAVAILABLE：release-verifier 不能来自待验证介质' ;; esac
  [[ -d "$release_trust_dir" && ! -L "$release_trust_dir" ]] ||
    die 'RELEASE_TRUST_UNAVAILABLE：固定信任目录不可用'
  trust_real="$(cd -- "$release_trust_dir" && pwd -P)"
  case "$trust_real" in "$media_real"/*) die 'RELEASE_TRUST_UNAVAILABLE：信任目录不能来自待验证介质' ;; esac
  if ! output="$("$verifier_real" verify --media "$media_real" --trust-dir "$trust_real" --output json 2>&1)"; then
    printf '%s\n' "$output" >&2
    die 'RELEASE_SIGNATURE_INVALID：发布完整性验签失败，拒绝执行介质内脚本'
  fi
  printf '%s\n' "$output" | python3 -c 'import json,sys; result=json.load(sys.stdin); sys.exit(0 if isinstance(result,dict) and result.get("status")=="VERIFIED" else 1)' ||
    die 'RELEASE_SIGNATURE_INVALID：发布完整性 verifier 未返回有效 VERIFIED JSON'
  info '发布完整性验签通过：受信 verifier + 固定信任目录'
}
release_integrity_gate

# 在安装目标中持久化已验证的原始发布清单与证明包。
# Agent 的 check-installed 读取此快照，但不将其作为新的信任根。
install_release_integrity_snapshot() {
  [[ "$require_release_integrity" == true ]] || return 0
  local source="$media_dir" destination="$target_dir/runtime/release-integrity" name
  [[ -d "$target_dir" && ! -L "$target_dir" ]] || die '发布完整性快照目标目录不安全'
  install -d -m 700 "$target_dir/runtime"
  install -d -m 700 "$destination"
  for name in release-manifest.json release-manifest.sigstore.json; do
    [[ -f "$source/$name" && ! -L "$source/$name" ]] ||
      die "发布完整性快照缺少安全文件：${name}"
    if [[ -e "$destination/$name" || -L "$destination/$name" ]]; then
      [[ -f "$destination/$name" && ! -L "$destination/$name" ]] &&
        cmp -s "$source/$name" "$destination/$name" ||
        die "已安装发布完整性快照与本次介质不一致：${name}"
    else
      install -m 600 "$source/$name" "$destination/$name"
    fi
  done
  if [[ -d "$source/attestations" && ! -L "$source/attestations" ]]; then
    install -d -m 700 "$destination/attestations"
    for proof in "$source"/attestations/*; do
      [[ -f "$proof" && ! -L "$proof" ]] || die 'attestations 目录含非普通文件'
      name="$(basename -- "$proof")"
      if [[ -e "$destination/attestations/$name" || -L "$destination/attestations/$name" ]]; then
        [[ -f "$destination/attestations/$name" && ! -L "$destination/attestations/$name" ]] &&
          cmp -s "$proof" "$destination/attestations/$name" ||
          die "已安装构建证明与本次介质不一致：attestations/${name}"
      else
        install -m 600 "$proof" "$destination/attestations/$name"
      fi
    done
  fi
}

need python3
target_dir="$(python3 -c 'import os,sys; print(os.path.realpath(sys.argv[1]))' "$target_dir")"
delivery_session_tool="$(delivery_tool_path deployment-session.py)" || die '缺少 deployment-session.py，请使用完整新版部署工具'
delivery_workflow_tool="$(delivery_tool_path deployment-workflow.sh)" || die '缺少 deployment-workflow.sh，请使用完整新版部署工具'
delivery_files_tool="$(delivery_tool_path delivery-files.py)" || die '缺少 delivery-files.py，请使用完整新版部署工具'
delivery_inspector="$(delivery_tool_path delivery-inspect.py)" || die '缺少 delivery-inspect.py，请使用完整新版部署工具'
source "$delivery_workflow_tool"

workflow_root_import_proof() { bash "$target_dir/bin/deploy.sh" delivery-import-proof "$1" "$2"; }
workflow_root_import() { bash "$target_dir/bin/deploy.sh" import "$2"; }
workflow_root_release_proof() { bash "$target_dir/bin/deploy.sh" delivery-proof "$1" "$2" "${3:-}"; }
workflow_root_deploy() {
  if [[ "$1" == platform && "$deployment_mode" == upgrade ]]; then
    DEPLOY_PLATFORM_CONTROL_PLANE_UPGRADE=true bash "$target_dir/bin/deploy.sh" deploy platform
  else bash "$target_dir/bin/deploy.sh" deploy "$1"; fi
}

workflow_assets_install() {
  workflow_start assets
  if [[ "$resume_installation" == true ]] && python3 "$delivery_files_tool" assets --target "$target_dir" --source "${assets[0]}"; then
    info '本次部署资产的实际文件摘要一致，保留已安装资产'
  elif workflow_was_done assets; then
    die '已完成资产的文件证据发生变化；请核对改动后恢复，拒绝按日志自动覆盖'
  else
    [[ ! -e "$target_dir/runtime/.assets-install-transaction" && ! -L "$target_dir/runtime/.assets-install-transaction" ]] ||
      die "资产事务未完成，请先在服务器执行 bash $(printf '%q' "$media_dir/install-assets.sh") --recover $(printf '%q' "$target_dir")，再以原命令 --resume"
    bash -n "$media_dir/install-assets.sh" || die '资产安装脚本语法检查失败'
    if [[ "$deployment_mode" == upgrade ]]; then
      ASSETS_PLATFORM_UPGRADE_IMAGE="$platform_upgrade_image" bash "$media_dir/install-assets.sh" "${assets[0]}" "$target_dir"
    else bash "$media_dir/install-assets.sh" "${assets[0]}" "$target_dir"; fi
    python3 "$delivery_files_tool" assets --target "$target_dir" --source "${assets[0]}" || die '资产安装后实际文件与本次交付不一致'
  fi
  install_release_integrity_snapshot
  if [[ "$deployment_mode" == upgrade ]]; then
    compatibility_gate
  fi
  workflow_done assets
}

# 用途：从已绑定摘要的本批资产读取唯一、普通且有界的兼容性声明。
# 参数：资产归档路径、私有临时输出路径、已核验的资产包 SHA256。
# 返回值：成功写入 0600 声明文件；缺失、重复、链接或摘要不符时失败。
extract_media_compatibility() {
  python3 - "$1" "$2" "$3" <<'PYCOMPAT'
import hashlib
import os
import stat
import sys
import tarfile

source, destination, expected = sys.argv[1:]
try:
    descriptor = os.open(source, os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0))
    with os.fdopen(descriptor, "rb") as stream:
        before = os.fstat(stream.fileno())
        if not stat.S_ISREG(before.st_mode):
            raise ValueError("资产包不是普通文件")
        digest = hashlib.sha256()
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(chunk)
        if digest.hexdigest() != expected:
            raise ValueError("资产包与已核验摘要不符")
        stream.seek(0)
        raw = None
        with tarfile.open(fileobj=stream, mode="r:gz") as archive:
            for count, member in enumerate(archive, 1):
                if count > 8192:
                    raise ValueError("资产包成员过多")
                name = member.name
                while name.startswith("./"):
                    name = name[2:]
                if name.rstrip("/") != "deployment-compatibility.json":
                    continue
                if raw is not None or not member.isfile() or member.size > 128 * 1024:
                    raise ValueError("兼容性声明重复、非普通文件或超过大小限制")
                raw = archive.extractfile(member).read(128 * 1024 + 1)
                if len(raw) != member.size:
                    raise ValueError("兼容性声明读取不完整")
        if raw is None:
            raise ValueError("本批资产缺少兼容性声明")
        after = os.fstat(stream.fileno())
        if (before.st_size, before.st_mtime_ns, before.st_ctime_ns) != (
                after.st_size, after.st_mtime_ns, after.st_ctime_ns):
            raise ValueError("读取期间资产包发生变化")
    flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0)
    with os.fdopen(os.open(destination, flags, 0o600), "wb") as output:
        output.write(raw)
except (OSError, ValueError, tarfile.TarError) as error:
    sys.exit("无法读取本批兼容性声明：" + str(error))
PYCOMPAT
}

# 用途：清理本轮私有兼容性声明，不触碰介质或安装目标。
# 参数：无；使用本轮创建的临时目录。
# 返回值：无。
cleanup_media_compatibility() {
  [[ -z "${media_compatibility_dir:-}" ]] || rm -rf -- "$media_compatibility_dir"
}

# Validate the full supplied plan before the initial upgrade. During resume,
# expected intermediate changes are checked by the existing stage proofs; the
# original plan's path/hash remains bound to the installation session identity.
# 用途：在任何升级写入前核验原计划与本批资产声明。
# 参数：无；读取目标、计划及本轮声明路径。
# 返回值：校验通过返回；不一致或缺失声明时终止。
deployment_baseline_gate() {
  [[ -n "$plan_baseline" && "$check_only" != true ]] || return 0
  [[ "$deployment_mode" == upgrade ]] || die '--plan-baseline 仅用于 upgrade 计划核验'
  [[ "$resume_installation" != true ]] || return 0
  local tool="$target_dir/bin/deployment-plan.py" status=0 component
  [[ -f "$tool" && ! -L "$tool" ]] || die '缺少完整计划核验工具，拒绝按旧计划执行'
  local -a args=(python3 "$tool" --target "$target_dir" --action upgrade --format human
    --component platform --component frontend --baseline-report "$plan_baseline")
  for component in "${requested_subsystems[@]}"; do args+=(--component "$component"); done
  [[ -n "${media_compatibility_file:-}" && -f "$media_compatibility_file" &&
     ! -L "$media_compatibility_file" ]] || die '缺少已核验的本批兼容性声明'
  args+=(--compatibility "$media_compatibility_file")
  [[ "$forward_recovery" != true ]] || args+=(--forward-recovery)
  [[ -z "$recovery_evidence" ]] || args+=(--recovery-evidence "$recovery_evidence")
  "${args[@]}" || status=$?
  [[ "$status" == 0 ]] || die "完整部署计划未通过（退出码 ${status}）；未开始备份、导入或资产替换，请重新生成并核对计划"
}

# 用途：按本批资产声明执行正式升级兼容性门禁。
# 参数：无；读取目标、本轮声明及前向恢复材料。
# 返回值：通过返回；阻断、材料不足或工具异常时终止。
compatibility_gate() {
  local plan_tool="$target_dir/bin/deployment-plan.py" status=0
  [[ -f "$plan_tool" && ! -L "$plan_tool" ]] || die '缺少 deployment-plan.py；不能跳过兼容性门禁'
  local -a args=(python3 "$plan_tool" --target "$target_dir" --action upgrade --compatibility-only --format human)
  [[ -n "${media_compatibility_file:-}" && -f "$media_compatibility_file" &&
     ! -L "$media_compatibility_file" ]] || die '缺少已核验的本批兼容性声明'
  args+=(--compatibility "$media_compatibility_file")
  [[ "$forward_recovery" != true ]] || args+=(--forward-recovery)
  [[ -z "$recovery_evidence" ]] || args+=(--recovery-evidence "$recovery_evidence")
  [[ -z "$plan_baseline" ]] || args+=(--baseline-report "$plan_baseline")
  "${args[@]}" || status=$?
  case "$status" in
    0) ;;
    2) workflow_update assets FAILED COMPATIBILITY_BLOCKED || true; die '兼容性门禁阻断升级；按上面的恢复证据要求处理，不能以旧计划继续' ;;
    3) workflow_update assets FAILED COMPATIBILITY_BLOCKED || true; die '兼容性门禁需要外部材料；补齐完整备份摘要、迁移目标和经验证步骤后重试' ;;
    *) workflow_update assets FAILED COMPATIBILITY_BLOCKED || true; die "兼容性门禁执行失败（退出码 ${status}）；保留输出并联系部署负责人" ;;
  esac
}

workflow_finish_controlled() {
  local component missing=false selected=false license_facts app enabled
  local -a selected_applications=()
  workflow_start controlled
  for component in "${workflow_components[@]}"; do
    case "$component" in common|platform|frontend) continue ;; esac
    enabled="$(bash "$target_dir/bin/deploy.sh" delivery-enabled "$component")" || die '无法核验当前组件范围；请补齐匹配的新版资产和恢复工具'
    case "$enabled" in ENABLED) ;; DISABLED) continue ;; *) die '组件启用状态响应无效，拒绝把未知状态当作未启用' ;; esac
    selected=true
    selected_applications+=("$(subsystem_application_of "$component")")
    if ! bash "$target_dir/bin/deploy.sh" delivery-final-proof "$component" "$media_dir/${package_components[$component]}"; then
      missing=true
      printf '[待完成] %s：请在基础平台处理对应 prod 环境的采用/受控更新，再按 status → continue → verify 完成验收。\n' "$component"
    fi
  done
  if [[ "$missing" == true ]]; then
    installation_pending=true
    workflow_wait controlled WAITING_PLATFORM_ADOPTION
    return 0
  fi
  if [[ "$selected" == true || -f "$target_dir/license-installation.json" || -n "$license_customer$license_file_arg" ]]; then
    if ! license_facts="$(bash "$target_dir/bin/deploy.sh" delivery-license-proof)"; then
      installation_pending=true
      workflow_wait controlled WAITING_AUTHORIZATION
      return 0
    fi
    for app in "${selected_applications[@]}"; do
      if ! jq -e --arg app "$app" '.status == "ENFORCED" and (.applications | index($app) != null)' <<< "$license_facts" >/dev/null; then
        installation_pending=true
        workflow_wait controlled WAITING_AUTHORIZATION
        return 0
      fi
    done
  fi
  installation_pending=false
  workflow_done controlled
  workflow_summary
}

if [[ "$resume_installation" == true && "$deployment_mode" == auto ]]; then
  resume_mode="$(python3 "$delivery_session_tool" status --state-dir "${target_dir}.installation-state" --format json | python3 -c 'import json,sys; print(json.load(sys.stdin)["identity"]["mode"])')" || die '恢复需要安全安装会话；也可明确原 --install/--upgrade 模式'
  case "$resume_mode" in install|upgrade) deployment_mode="$resume_mode" ;; *) die '增装恢复必须明确原 --add-subsystems 列表' ;; esac
fi
resolve_deployment_mode
# 已部署环境预检：一键首装在安装任何资产之前退出。否则资产先落地、控制面重载
# 标记先写入，而旧 Agent 镜像无法解析新版 subsystems.d 清单（runtime binding 字段
# 变更），成对重载会把 platform-api 停在半升级状态。升级必须走受控镜像发布路径。
if [[ "$deployment_mode" == install ]]; then
  if [[ "$resume_installation" != true ]] && { [[ -e "$target_dir/docker-compose.yml" || -L "$target_dir/docker-compose.yml" ]] ||
     [[ -e "$target_dir/.env" || -L "$target_dir/.env" ]]; }; then
    die "目标目录已是已部署环境（存在 docker-compose.yml 或 .env）；一键首装不再继续。
请改用本脚本的升级模式：
  ./服务器一键离线部署.sh --media <介质目录> --upgrade --apply-compose
  （可选）--upgrade-subsystems customer-opportunity,...   一并受控升级已部署子系统
  （可选）--customer <客户标识> [--license <commercial-license.jws>]  升级后核对实例申请/导入商业授权
若资产曾被安装到一半（存在 runtime/.control-plane-reload-required 且 Agent 日志报
invalid production subsystem profile），可按上述 --upgrade 重跑（门禁摘要一致时自动接续），
或用介质内 install-assets.sh --recover 回滚到升级前资产。"
  fi
fi

# 新会话是否可开启仍由会话工具核验 COMPLETED/ABANDONED；
# 首装仅移除无条件禁令，前面的已部署环境保护和后续全部门禁保持生效。
[[ -z "$upgrade_subsystems_raw" || "$deployment_mode" == upgrade ]] || die '--upgrade-subsystems 仅用于升级'
[[ "$apply_compose" != true || "$deployment_mode" == upgrade ]] || die '--apply-compose 仅用于升级'
[[ ( -z "$license_customer" && -z "$license_file_arg" ) || "$deployment_mode" == upgrade ]] ||
  die '--customer/--license 仅用于升级；首装及扩容请使用交付安装计划的受控授权流程'

declare -a requested_subsystems=()
declare -A requested_subsystem_set=()
subsystems_source_raw="$add_subsystems_raw"
[[ -n "$upgrade_subsystems_raw" ]] && subsystems_source_raw="${add_subsystems_raw:+${add_subsystems_raw} }${upgrade_subsystems_raw//,/ }"
if [[ -n "$subsystems_source_raw" ]]; then
  normalized_subsystems="${subsystems_source_raw//,/ }"
  for requested_subsystem in $normalized_subsystems; do
    case "$requested_subsystem" in
      customer-opportunity|customer-portal|contract|project|settlement|data-analysis) ;;
      *) die "不支持的增量子系统：${requested_subsystem}" ;;
    esac
    [[ -n "${requested_subsystem_set[$requested_subsystem]:-}" ]] && continue
    requested_subsystem_set["$requested_subsystem"]=1
  done
  ((${#requested_subsystem_set[@]} > 0)) || die '--add-subsystems 至少需要一个子系统'
  # 追加门户时需要同时准备 CRM；升级门户只验证既有 CRM，保留显式升级范围。
  if [[ "$deployment_mode" == add-subsystems && -n "${requested_subsystem_set[customer-portal]:-}" ]]; then
    requested_subsystem_set[customer-opportunity]=1
  fi
  for requested_subsystem in customer-opportunity customer-portal contract project settlement data-analysis; do
    [[ -z "${requested_subsystem_set[$requested_subsystem]:-}" ]] || requested_subsystems+=("$requested_subsystem")
  done
fi

[[ "$legacy_license_mode" != upgrade || ${#requested_subsystems[@]} -gt 0 ]] || die '旧版授权迁移至少需要一个明确的旧业务系统'
for command_name in tar gzip sha256sum awk sed grep find sort stat mktemp df du uname; do need "$command_name"; done

confirm_resource_warning() {
  local message="$1" answer
  warn "$message"
  [[ "$assume_yes" == true ]] && return 0
  [[ -t 0 ]] || die '非交互执行遇到资源警告；确认后可传入 --yes'
  read -r -p '仍要继续？请输入 YES：' answer
  [[ "$answer" == YES ]] || die '已取消部署'
}

validate_outer_archive() {
  local archive="$1" sidecar="${1}.sha256" entry top_levels expected actual archive_name
  [[ -f "$archive" && ! -L "$archive" ]] || die "总归档不是安全普通文件：${archive}"
  [[ -f "$sidecar" && ! -L "$sidecar" ]] || die "缺少总归档校验文件：${sidecar}"
  archive_name="$(basename -- "$archive")"
  expected="$(awk -v name="$archive_name" '
    NF != 2 || NR != 1 || length($1) != 64 || tolower($1) ~ /[^0-9a-f]/ || $2 != name {bad=1}
    {digest=tolower($1)}
    END {if (bad || NR != 1) exit 1; print digest}
  ' "$sidecar")" || die '总归档 SHA256 伴随文件必须只绑定当前归档'
  actual="$(sha256sum "$archive" | awk '{print tolower($1)}')"
  [[ "$actual" == "$expected" ]] || die '总归档 SHA256 校验失败'
  while IFS= read -r entry; do
    [[ -n "$entry" && "$entry" != /* ]] || die '总归档包含绝对路径或空路径'
    case "/$entry/" in */../*) die "总归档包含路径逃逸：${entry}" ;; esac
  done < <(tar -tf "$archive")
  if ! tar -tvf "$archive" | awk '{t=substr($1,1,1); if (t != "-" && t != "d") exit 1}'; then
    die '总归档包含符号链接、硬链接或设备文件'
  fi
  top_levels="$(tar -tf "$archive" | awk -F/ 'NF {print $1}' | LC_ALL=C sort -u)"
  [[ "$(printf '%s\n' "$top_levels" | sed '/^$/d' | wc -l | tr -d ' ')" == 1 ]] || die '总归档必须只有一个顶层目录'
  [[ "$top_levels" =~ ^[A-Za-z0-9._-]+$ ]] || die "总归档顶层目录名不安全：${top_levels}"
  printf '%s\n' "$top_levels"
}

verify_existing_media_manifest() {
  local archive="$1" top_level="$2" directory="$3" manifest_count archive_digest directory_digest
  [[ -d "$directory" && ! -L "$directory" ]] || die "解压目标不是安全目录：${directory}"
  [[ -f "$directory/SHA256SUMS" && ! -L "$directory/SHA256SUMS" ]] ||
    die "已有解压目录缺少安全 SHA256SUMS：${directory}"
  manifest_count="$(tar -tf "$archive" | awk -v name="$top_level/SHA256SUMS" '$0 == name {n++} END {print n+0}')" ||
    die '无法读取总归档内的交付清单'
  [[ "$manifest_count" == 1 ]] || die '总归档必须恰好包含一份交付清单'
  archive_digest="$(tar -xOf "$archive" "$top_level/SHA256SUMS" | sha256sum | awk '{print $1}')" ||
    die '无法读取总归档内的交付清单'
  directory_digest="$(sha256sum "$directory/SHA256SUMS" | awk '{print $1}')" ||
    die '无法读取已有解压目录的交付清单'
  [[ "$archive_digest" == "$directory_digest" ]] ||
    die '总归档与解压目录清单不一致，拒绝复用旧目录'
  # 这里只确认介质身份，后续第 1/6 阶段仍校验清单和每个实际文件。
}

extract_outer_archive() {
  local archive="$1" top_level stage parent final
  top_level="$(validate_outer_archive "$archive")" || die '总归档校验失败'
  parent="$(cd -- "$(dirname -- "$archive")" && pwd)"
  final="${parent}/${top_level}"
  if [[ -d "$final" && ! -L "$final" ]]; then
    verify_existing_media_manifest "$archive" "$top_level" "$final" || die '已有解压目录校验失败'
    info "复用与总归档清单一致的解压目录，继续校验全部介质文件：${final}" >&2
    printf '%s\n' "$final"
    return
  fi
  [[ ! -e "$final" && ! -L "$final" ]] || die "解压目标被非目录占用：${final}"
  stage="$(mktemp -d "${parent}/.uip-delivery-extract.XXXXXX")"
  if ! tar --no-same-owner --no-same-permissions -xf "$archive" -C "$stage"; then
    rm -rf -- "$stage"
    die '总归档解压失败'
  fi
  [[ -d "$stage/$top_level" && ! -L "$stage/$top_level" ]] || { rm -rf -- "$stage"; die '总归档结构不正确'; }
  mv "$stage/$top_level" "$final"
  # umask 077 会叠加 --no-same-permissions 使解压产物丢失可执行位；
  # 按构建侧 install -m 755 的交付权限显式恢复两个安装入口。
  for media_installer in install-assets.sh install-host-dependencies.sh; do
    [[ -f "$final/$media_installer" ]] || continue
    chmod 755 "$final/$media_installer"
  done
  rmdir "$stage"
  printf '%s\n' "$final"
}

discover_media() {
  local candidate candidates=() archives=()
  local archive_top
  if [[ -n "$media_input" ]]; then
    case "$media_input" in /*) candidate="$media_input" ;; *) candidate="$(pwd)/$media_input" ;; esac
    if [[ -f "$candidate" ]]; then extract_outer_archive "$candidate"; return; fi
    [[ -d "$candidate" ]] || die "交付介质不存在：${candidate}"
    printf '%s\n' "$(cd -- "$candidate" && pwd)"
    return
  fi
  [[ -f "$script_dir/SHA256SUMS" ]] && { printf '%s\n' "$script_dir"; return; }
  shopt -s nullglob
  for candidate in "$script_dir"/*; do
    [[ -d "$candidate" && -f "$candidate/SHA256SUMS" ]] && candidates+=("$candidate")
  done
  archives=("$script_dir"/uip-offline-delivery-*-linux-amd64.tar)
  ((${#candidates[@]} <= 1)) || die '发现多个交付目录，请使用 --media 指定'
  ((${#archives[@]} <= 1)) || die '发现多个总归档，请使用 --media 指定，不会按文件时间猜测最新版'
  if ((${#candidates[@]} == 1)); then
    if ((${#archives[@]} == 1)); then
      # 再次运行可复用同一包的解压目录，但不能忽略旁边刚上传的新版本。
      archive_top="$(validate_outer_archive "${archives[0]}")" || die '总归档校验失败'
      [[ "${candidates[0]}" == "$script_dir/$archive_top" ]] ||
        die '总归档与解压目录不是同一交付，请使用 --media 明确选择'
      verify_existing_media_manifest "${archives[0]}" "$archive_top" "${candidates[0]}" ||
        die '已有解压目录校验失败'
    fi
    printf '%s\n' "${candidates[0]}"
    return
  fi
  ((${#archives[@]} == 1)) || die '未唯一发现交付目录或总归档，请使用 --media 指定'
  extract_outer_archive "${archives[0]}"
}

media_dir="$(discover_media)" || die '交付介质发现或解包失败'
[[ -d "$media_dir" && ! -L "$media_dir" ]] || die "交付目录不安全：${media_dir}"
[[ -f "$media_dir/SHA256SUMS" && ! -L "$media_dir/SHA256SUMS" ]] || die '交付目录缺少 SHA256SUMS'

# 安全（SEC-3）：发布完整性门禁默认不启用，以保持既有自动化流程可用。
# 介质内 SHA256SUMS 只能证明“文件与清单一致”，改包后重算摘要即可自洽；
# 它不能证明发布方身份。因此介质带签名清单/证明包而操作者未显式要求验签时，
# 必须显著提示只做了自证摘要校验，并给出启用方式；不改变默认行为、不阻塞。
release_integrity_advisory() {
  [[ "$require_release_integrity" == true ]] && return 0
  local name found=()
  for name in release-manifest.json release-manifest.sigstore.json; do
    [[ -f "$media_dir/$name" && ! -L "$media_dir/$name" ]] && found+=("$name")
  done
  ((${#found[@]} > 0)) || return 0
  warn '================ 安全警告：本次未验证发布签名 ================'
  warn "交付介质包含发布签名材料（${found[*]}），但本次未启用发布完整性验签。"
  warn '当前只校验介质内的 SHA256SUMS 自证摘要：该清单可与介质一起被改动后重算，'
  warn '因此不能证明发布方身份，也不能证明介质未被替换或植入。'
  warn '如需在执行介质脚本前验证签名与构建证明，请加上 --require-release-integrity 重新执行'
  warn '（该门禁要求宿主机已就绪受信 release-verifier 与固定信任目录）。'
  warn '本次按默认行为继续执行，不阻塞；是否启用由操作者决定。'
  warn '=============================================================='
}
release_integrity_advisory

printf '\n========== 1/6 校验交付介质 ==========\n'
awk '
  NF != 2 || length($1) != 64 || tolower($1) ~ /[^0-9a-f]/ {exit 1}
  $2 !~ /^[A-Za-z0-9._-]+$/ && $2 != "服务器一键离线部署.sh" && $2 != "系统运维工具.sh" && $2 != "清理首次部署产物.sh" {exit 1}
  ++seen[$2] != 1 {exit 1}
  END {if (NR == 0) exit 1}
' "$media_dir/SHA256SUMS" || die 'SHA256SUMS 包含非法、重复或跨目录条目'
while read -r _ manifest_name; do
  [[ -f "$media_dir/$manifest_name" && ! -L "$media_dir/$manifest_name" ]] ||
    die "SHA256SUMS 引用了缺失或非普通文件：${manifest_name}"
done < "$media_dir/SHA256SUMS"
(cd "$media_dir" && sha256sum --check SHA256SUMS)
require_media_binding() {
  local name="${1:?介质文件名}"
  [[ -f "$media_dir/$name" && ! -L "$media_dir/$name" ]] || die "介质文件不安全或缺失：$name"
  [[ "$(awk -v name="$name" '$2 == name {n++} END {print n+0}' "$media_dir/SHA256SUMS")" == 1 ]] ||
    die "介质 SHA256SUMS 未唯一绑定文件：$name"
}
require_media_sidecar() {
  local name="${1:?介质文件名}" sidecar="$media_dir/${1}.sha256" expected bound actual sidecar_count
  require_media_binding "$name"
  [[ -f "$sidecar" && ! -L "$sidecar" ]] || die "缺少安全单包 SHA256 文件：${sidecar}"
  # 旧打包器没有把冗余 sidecar 列入总清单。兼容时仍由总清单绑定的
  # payload 摘要做权威校验，绝不能只相信可独立修改的 sidecar。
  expected="$(awk -v name="$name" '
    NF != 2 || NR != 1 || length($1) != 64 || tolower($1) ~ /[^0-9a-f]/ || $2 != name {bad=1}
    {digest=tolower($1)}
    END {if (bad || NR != 1) exit 1; print digest}
  ' "$sidecar")" || die "单包 SHA256 必须仅绑定当前文件：${name}"
  bound="$(awk -v name="$name" '$2 == name {print tolower($1)}' "$media_dir/SHA256SUMS")"
  [[ "$expected" == "$bound" ]] || die "单包 SHA256 与交付总清单不一致：${name}"
  actual="$(sha256sum "$media_dir/$name" | awk '{print tolower($1)}')"
  [[ "$actual" == "$bound" ]] || die "实际文件 SHA256 与交付总清单不一致：${name}"
  sidecar_count="$(awk -v name="${name}.sha256" '$2 == name {n++} END {print n+0}' "$media_dir/SHA256SUMS")"
  [[ "$sidecar_count" == 0 || "$sidecar_count" == 1 ]] || die "单包 SHA256 清单条目重复：${name}"
  if [[ "$sidecar_count" == 1 ]]; then
    bound="$(awk -v name="${name}.sha256" '$2 == name {print tolower($1)}' "$media_dir/SHA256SUMS")"
    actual="$(sha256sum "$sidecar" | awk '{print tolower($1)}')"
    [[ "$actual" == "$bound" ]] || die "单包 SHA256 文件被修改：${name}.sha256"
  else
    info "旧版介质 sidecar 已与总清单及实际文件三方核验：${name}.sha256"
  fi
}
if find "$media_dir" -maxdepth 1 -type l -print -quit | grep -q .; then die '交付目录顶层不得包含符号链接'; fi
shopt -s nullglob
image_packages=()
for package_path in "$media_dir"/*-linux-amd64.tar.gz; do
  [[ "$(basename -- "$package_path")" != host-dependencies-* ]] || continue
  image_packages+=("$package_path")
done
(( ${#image_packages[@]} >= 3 && ${#image_packages[@]} <= 9 )) ||
  die "交付介质应包含 3-9 个 linux/amd64 镜像包，实际为 ${#image_packages[@]}"
declare -A package_components=()
for package_path in "${image_packages[@]}"; do
  require_media_binding "$(basename -- "$package_path")"
  require_media_sidecar "$(basename -- "$package_path")"
  [[ -f "${package_path}.sha256" && ! -L "${package_path}.sha256" ]] || die "缺少单包 sidecar：${package_path}.sha256"
  tar -xOf "$package_path" package.env | grep -Fxq 'PLATFORM=linux/amd64' || die "镜像包平台不正确：${package_path}"
  package_name="$(basename -- "$package_path")"
  case "$package_name" in
    common-infrastructure-linux-amd64.tar.gz) package_component=common ;;
    platform-backend-*-linux-amd64.tar.gz) package_component=platform ;;
    frontend-*-linux-amd64.tar.gz) package_component=frontend ;;
    customer-opportunity-backend-*-linux-amd64.tar.gz) package_component=customer-opportunity ;;
    customer-portal-backend-*-linux-amd64.tar.gz) package_component=customer-portal ;;
    contract-backend-*-linux-amd64.tar.gz) package_component=contract ;;
    project-backend-*-linux-amd64.tar.gz) package_component=project ;;
    settlement-backend-*-linux-amd64.tar.gz) package_component=settlement ;;
    data-analysis-backend-*-linux-amd64.tar.gz) package_component=data-analysis ;;
    *) die "不支持的镜像包文件名：${package_name}" ;;
  esac
  declared_component="$(tar -xOf "$package_path" package.env | awk -F= '$1=="COMPONENT" {n++; value=substr($0,index($0,"=")+1)} END {if(n!=1)exit 1; print value}')" ||
    die "镜像包组件声明缺失或重复：$package_name"
  [[ "$declared_component" == "$package_component" ]] || die "镜像包文件名与真实组件不一致：$package_name"
  [[ -z "${package_components[$package_component]:-}" ]] || die "同一组件存在多个镜像包：${package_component}"
  package_components["$package_component"]="$package_name"
done
for required_component in common platform frontend; do
  [[ -n "${package_components[$required_component]:-}" ]] || die "缺少必选基础镜像包：${required_component}"
done
if [[ -n "${package_components[customer-portal]:-}" && -z "${package_components[customer-opportunity]:-}" ]]; then
  die '客户自助门户依赖客户与商机系统，交付介质缺少 customer-opportunity'
fi
if [[ "$deployment_mode" == add-subsystems || "$deployment_mode" == upgrade ]]; then
  for requested_subsystem in "${requested_subsystems[@]}"; do
    [[ -n "${package_components[$requested_subsystem]:-}" ]] ||
      die "${deployment_mode} 请求 ${requested_subsystem}，但交付介质缺少对应镜像包"
  done
  info "${deployment_mode} 指定子系统：${requested_subsystems[*]}"
fi
info "必选基础镜像已齐备；介质内共 ${#image_packages[@]} 个镜像包"
assets=("$media_dir"/deployment-assets-*.tar.gz)
[[ "${#assets[@]}" == 1 ]] || die '部署资产包必须恰好一个'
require_media_binding "$(basename -- "${assets[0]}")"
require_media_sidecar "$(basename -- "${assets[0]}")"
require_media_binding install-assets.sh
[[ -z "$legacy_license_mode" ]] || require_media_binding license-activate.sh
[[ ! -e "$media_dir/系统运维工具.sh" ]] || require_media_binding 系统运维工具.sh
[[ ! -e "$media_dir/清理首次部署产物.sh" && ! -L "$media_dir/清理首次部署产物.sh" ]] || require_media_binding 清理首次部署产物.sh
[[ ! -e "$media_dir/FIRST_INSTALL_RESET.md" && ! -L "$media_dir/FIRST_INSTALL_RESET.md" ]] || require_media_binding FIRST_INSTALL_RESET.md
[[ ! -e "$media_dir/BUILD_INFO.txt" ]] || require_media_binding BUILD_INFO.txt
if [[ -n "$license_file_arg" ]]; then
  require_media_binding license-activate.sh
  [[ -f "$license_file_arg" && ! -L "$license_file_arg" ]] || die '--license 必须是安全普通许可证文件'
  license_file_arg="$(cd -- "$(dirname -- "$license_file_arg")" && pwd)/$(basename -- "$license_file_arg")"
fi
[[ -x "$media_dir/install-assets.sh" && ! -L "$media_dir/install-assets.sh" ]] || die '缺少可执行的 install-assets.sh'
compatibility_lock="$media_dir/PLATFORM_ASSETS_COMPATIBILITY.env"
[[ -f "$compatibility_lock" && ! -L "$compatibility_lock" ]] ||
  die '交付介质缺少 PLATFORM_ASSETS_COMPATIBILITY.env；无法证明 Platform 镜像与共享部署资产来自同一基线'
[[ "$(awk '$2 == "PLATFORM_ASSETS_COMPATIBILITY.env" {count++} END {print count+0}' "$media_dir/SHA256SUMS")" == 1 ]] ||
  die '交付 SHA256SUMS 未唯一绑定 PLATFORM_ASSETS_COMPATIBILITY.env'

compatibility_value() {
  local file="$1" key="$2"
  awk -F= -v key="$key" '
    $1 == key {if (++seen != 1) exit 1; value=substr($0, length(key) + 2)}
    END {if (seen != 1 || value == "") exit 1; print value}
  ' "$file"
}

validate_compatibility_lock() {
  local file="$1" key package expected_name expected_digest actual_digest
  grep -Fqx 'FORMAT=1' "$file" || die 'Platform/部署资产兼容性锁格式不支持'
  awk -F= '
    !/^[A-Z0-9_]+=[A-Za-z0-9._@:\/-]+$/ {exit 1}
    !($1 == "FORMAT" || $1 == "PLATFORM_ASSETS_INPUT_SHA256" ||
      $1 == "COMMON_PACKAGE" || $1 == "COMMON_PACKAGE_SHA256" ||
      $1 == "PLATFORM_PACKAGE" || $1 == "PLATFORM_PACKAGE_SHA256" ||
      $1 == "FRONTEND_PACKAGE" || $1 == "FRONTEND_PACKAGE_SHA256" ||
      $1 == "ASSETS_PACKAGE" || $1 == "ASSETS_PACKAGE_SHA256") {exit 1}
    ++seen[$1] != 1 {exit 1}
    seen[$1] == 1 {count++}
    END {if (count != 10) exit 1}
  ' "$file" || die 'Platform/部署资产兼容性锁非法或不完整'
  expected_digest="$(compatibility_value "$file" PLATFORM_ASSETS_INPUT_SHA256)" ||
    die '兼容性锁缺少共享输入指纹'
  [[ "$expected_digest" =~ ^[a-f0-9]{64}$ ]] || die '兼容性锁的共享输入指纹格式错误'

  for key in COMMON PLATFORM FRONTEND ASSETS; do
    case "$key" in
      COMMON) package="$media_dir/${package_components[common]}" ;;
      PLATFORM) package="$media_dir/${package_components[platform]}" ;;
      FRONTEND) package="$media_dir/${package_components[frontend]}" ;;
      ASSETS) package="${assets[0]}" ;;
    esac
    expected_name="$(compatibility_value "$file" "${key}_PACKAGE")" ||
      die "兼容性锁缺少 ${key}_PACKAGE"
    expected_digest="$(compatibility_value "$file" "${key}_PACKAGE_SHA256")" ||
      die "兼容性锁缺少 ${key}_PACKAGE_SHA256"
    [[ "$expected_name" == "$(basename -- "$package")" ]] ||
      die "兼容性锁与 ${key} 包名不一致"
    [[ "$expected_digest" =~ ^[a-f0-9]{64}$ ]] ||
      die "兼容性锁中 ${key} 摘要格式错误"
    actual_digest="$(sha256sum "$package" | awk '{print tolower($1)}')"
    [[ "$actual_digest" == "$expected_digest" ]] ||
      die "兼容性锁与 ${key} 包内容不一致"
  done
}

validate_compatibility_lock "$compatibility_lock"
info "交付介质完整：${media_dir}"
printf '\n========== 统一交付检查：镜像 / 资产 / 组件 / 批准材料 ==========\n'
python3 "$delivery_inspector" --media "$media_dir" --phase install --format human ||
  die '交付材料不匹配或声明的授权交付缺少必需材料；请按统一检查中的位置补齐后重试'

media_compatibility_file=""
if [[ "$deployment_mode" == upgrade ]]; then
  # 先通过独立验签（要求开启时）、包摘要与完整结构检查，再读本批声明。
  # 预检不能使用尚未更新的 target 声明，也不能相信未经绑定的顶层副本。
  media_compatibility_dir="$(mktemp -d "${TMPDIR:-/tmp}/uip-compatibility.XXXXXX")" ||
    die '无法创建兼容性声明临时目录'
  trap cleanup_media_compatibility EXIT
  media_compatibility_file="$media_compatibility_dir/deployment-compatibility.json"
  assets_digest="$(compatibility_value "$compatibility_lock" ASSETS_PACKAGE_SHA256)" ||
    die '兼容性锁缺少资产包摘要'
  extract_media_compatibility "${assets[0]}" "$media_compatibility_file" "$assets_digest" ||
    die '本批兼容性声明无效；尚未备份、导入或替换资产'
  if [[ -e "$media_dir/deployment-compatibility.json" || -L "$media_dir/deployment-compatibility.json" ]]; then
    require_media_binding deployment-compatibility.json
    cmp -s "$media_compatibility_file" "$media_dir/deployment-compatibility.json" ||
      die '介质顶层兼容性声明与本批资产不一致'
  fi
fi

printf '\n========== 2/6 检查服务器资源与 Docker ==========\n'
cpu_count="$(getconf _NPROCESSORS_ONLN 2>/dev/null || echo 0)"
memory_kib="$(awk '/^MemTotal:/ {print $2}' /proc/meminfo)"
media_kib="$(du -sk "$media_dir" | awk '{print $1}')"
command -v docker >/dev/null 2>&1 || die 'Docker Engine 未安装；镜像包不包含 Docker 系统软件'
docker info >/dev/null 2>&1 || die 'Docker Engine 不可用'
compose_version="$(docker compose version --short 2>/dev/null || true)"
compose_major="${compose_version#v}"; compose_major="${compose_major%%.*}"
[[ "$compose_major" =~ ^[0-9]+$ ]] && ((compose_major >= 2)) || die "需要 Docker Compose 2+，当前为 ${compose_version:-未知}"
docker_root="$(docker info --format '{{.DockerRootDir}}' 2>/dev/null || true)"
info "Docker Engine=$(docker version --format '{{.Server.Version}}' 2>/dev/null || echo unknown)，Compose=${compose_version}，DockerRoot=${docker_root:-unknown}"

existing_parent="$target_dir"
while [[ ! -d "$existing_parent" ]]; do
  next_parent="$(dirname -- "$existing_parent")"
  [[ "$next_parent" != "$existing_parent" ]] || break
  existing_parent="$next_parent"
done
[[ -d "$existing_parent" ]] || die "无法确定部署目录所在分区：${target_dir}"
[[ -n "$docker_root" && -d "$docker_root" ]] || die "Docker 数据目录不存在：${docker_root:-unknown}"

validate_existing_deployment_target() {
  local required_path
  [[ -d "$target_dir" && ! -L "$target_dir" ]] || die "增量追加要求已存在的安全部署目录：${target_dir}"
  for required_path in .env .release.env docker-compose.yml bin/deploy.sh; do
    [[ -f "$target_dir/$required_path" && ! -L "$target_dir/$required_path" ]] ||
      die "增量追加目标缺少安全普通文件：${target_dir}/${required_path}"
  done
  for required_path in packages manifests runtime subsystems.d subsystem-templates; do
    [[ -d "$target_dir/$required_path" && ! -L "$target_dir/$required_path" ]] ||
      die "增量追加目标缺少安全目录：${target_dir}/${required_path}"
  done
  for required_path in bin/deployment-workflow.sh bin/deployment-session.py bin/delivery-runtime.sh \
      bin/delivery-runtime.py bin/delivery-inspect.py bin/delivery-inspect-images.py bin/delivery-inspect-v2.py bin/delivery-files.py bin/subsystem-runtime.py; do
    [[ -f "$target_dir/$required_path" && ! -L "$target_dir/$required_path" ]] ||
      die "既有目标缺少分阶段增装工具 ${required_path}；请先用匹配完整基线执行显式 --upgrade，不能把新版增装入口套在旧资产上"
  done
  [[ ! -e "$target_dir/runtime/.assets-install-transaction" && ! -L "$target_dir/runtime/.assets-install-transaction" ]] ||
    die '检测到未完成的资产安装事务；请先用原交付包 install-assets.sh --recover 恢复'
}

validate_incremental_base_packages() {
  local component package_name source installed
  for component in common platform frontend; do
    package_name="${package_components[$component]}"
    source="$media_dir/$package_name"
    installed="$target_dir/packages/$package_name"
    [[ -f "$installed" && ! -L "$installed" ]] ||
      die "增量交付与已部署基线不一致：目标缺少 ${package_name}；请制作与当前服务器同基线的 --extend-from 交付，或执行完整基线升级"
    cmp -s "$source" "$installed" ||
      die "增量交付与已部署基线不一致：$package_name 内容不同；拒绝混用基础镜像，请执行完整基线升级"
  done
}

validate_incremental_shared_assets() {
  local archive="$1" listing member relative expected actual
  local archive_members target_members

  # 增量模式不覆盖共享部署资产，因此必须在任何镜像导入前证明
  # 介质中的生产审核清单和运行时模板与目标服务器完全一致。
  # 否则 platform-api 和 Agent 可能分别缓存新旧清单，导致 manifest drift。
  listing="$(tar -tzf "$archive")" || die "无法读取部署资产包：$archive"
  printf '%s\n' "$listing" | awk '
    { path=$0; sub(/^\.\//, "", path) }
    path == "" || path == "." { next }
    path ~ /^\// || path ~ /(^|\/)\.\.($|\/)/ { exit 1 }
  ' || die '部署资产包包含不安全路径'
  tar -tvzf "$archive" | awk 'substr($1,1,1) != "-" && substr($1,1,1) != "d" {exit 1}' ||
    die '部署资产包包含链接或特殊文件'

  archive_members="$(printf '%s\n' "$listing" | sed 's#^\./##' |
    awk '!/\/$/ && ($0 ~ /^subsystems\.d\// || $0 ~ /^subsystem-templates\//) {print}' |
    LC_ALL=C sort)"
  [[ -n "$archive_members" ]] || die '部署资产包缺少生产审核清单或运行时模板'
  target_members="$(
    cd "$target_dir"
    find subsystems.d subsystem-templates -type f -print | LC_ALL=C sort
  )"
  [[ "$archive_members" == "$target_members" ]] ||
    die '增量交付的共享清单/模板集合与已部署基线不一致；拒绝留下混合版本，请执行完整基线升级'

  while IFS= read -r relative; do
    [[ -f "$target_dir/$relative" && ! -L "$target_dir/$relative" ]] ||
      die "已部署共享资产不安全或缺失：$relative"
    member="$(printf '%s\n' "$listing" | awk -v wanted="$relative" '
      { original=$0; path=$0; sub(/^\.\//, "", path) }
      path == wanted { ++count; selected=original }
      END { if (count != 1) exit 1; print selected }
    ')" || die "部署资产包条目不唯一：$relative"
    expected="$(tar -xOzf "$archive" "$member" | sha256sum | awk '{print tolower($1)}')" ||
      die "无法读取部署资产包条目：$relative"
    actual="$(sha256sum "$target_dir/$relative" | awk '{print tolower($1)}')" ||
      die "无法校验已部署共享资产：$relative"
    [[ "$expected" == "$actual" ]] ||
      die "增量交付的共享资产与目标不一致：${relative}；拒绝只替换 Agent 或业务镜像，请执行完整基线升级"
  done <<< "$archive_members"
}

if [[ "$deployment_mode" == add-subsystems ]]; then
  validate_existing_deployment_target
  installed_compatibility_lock="$target_dir/packages/PLATFORM_ASSETS_COMPATIBILITY.delivery.env"
  [[ -f "$installed_compatibility_lock" && ! -L "$installed_compatibility_lock" ]] ||
    die '现有部署缺少已保存的 Platform/资产兼容性锁；旧基线无法安全猜测兼容，请先执行完整基线升级'
  cmp -s "$compatibility_lock" "$installed_compatibility_lock" ||
    die '增量交付与已部署 Platform/资产兼容性锁不一致；拒绝混用版本，请执行完整基线升级'
  validate_incremental_base_packages
  validate_incremental_shared_assets "${assets[0]}"
  declare -A incremental_package_already_present=()
  for requested_subsystem in "${requested_subsystems[@]}"; do
    package_name="${package_components[$requested_subsystem]}"
    package_path="$media_dir/$package_name"
    case "$requested_subsystem" in
      customer-opportunity) package_pattern='customer-opportunity-backend-*-linux-amd64.tar.gz' ;;
      customer-portal) package_pattern='customer-portal-backend-*-linux-amd64.tar.gz' ;;
      contract) package_pattern='contract-backend-*-linux-amd64.tar.gz' ;;
      project) package_pattern='project-backend-*-linux-amd64.tar.gz' ;;
      settlement) package_pattern='settlement-backend-*-linux-amd64.tar.gz' ;;
      data-analysis) package_pattern='data-analysis-backend-*-linux-amd64.tar.gz' ;;
    esac
    shopt -s nullglob
    existing_component_packages=("$target_dir"/packages/$package_pattern)
    shopt -u nullglob
    ((${#existing_component_packages[@]} <= 1)) ||
      die "packages/ 中 ${requested_subsystem} 已存在多个版本，拒绝自动选择"
    if ((${#existing_component_packages[@]} == 1)); then
      existing_component_package="${existing_component_packages[0]}"
      [[ "$(basename -- "$existing_component_package")" == "$package_name" ]] ||
        die "${requested_subsystem} 已存在其他版本 $(basename -- "$existing_component_package")；更新已存在系统必须使用受控 upgrade 流程"
      cmp -s "$existing_component_package" "$package_path" ||
        die "同名 ${requested_subsystem} 镜像包内容不同，拒绝覆盖"
      incremental_package_already_present["$requested_subsystem"]=1
      info "${requested_subsystem} 镜像包已存在且内容一致，继续幂等恢复"
    fi
  done

  info "已确认增量目标是完整现有部署：${target_dir}"
  info '已确认必选基础镜像、生产审核清单和运行时模板均与增量交付同基线'
fi

containerd_config_value() {
  local section="$1" key="$2"
  awk -v wanted_section="$section" -v wanted_key="$key" '
    /^[[:space:]]*\[/ {section=$0; gsub(/[[:space:]]/, "", section); next}
    section == wanted_section && $0 ~ "^[[:space:]]*" wanted_key "[[:space:]]*=" {
      value=$0; sub(/^[^=]*=[[:space:]]*/, "", value)
      if (value !~ /^\047[^\047]*\047[[:space:]]*$/ && value !~ /^"[^"\\]*"[[:space:]]*$/) exit 1
      sub(/[[:space:]]*$/, "", value); value=substr(value, 2, length(value)-2)
      if (++count != 1 || value == "") exit 1
      selected=value
    }
    END {if (count != 1) exit 1; print selected}
  '
}

detect_docker_image_storage() {
  local status docker_exec containerd_exec endpoint configured_root config_path dump address
  local section_address top_level_address
  local -a config_args=()
  image_store=classic
  containerd_root=''
  status="$(docker info --format '{{range .DriverStatus}}{{if eq (index . 0) "driver-type"}}{{index . 1}}{{end}}{{end}}' 2>/dev/null)" || die '无法检查 Docker image store'
  if [[ -z "$status" ]]; then
    return 0
  fi
  [[ "$status" == io.containerd.snapshotter.v1 ]] || die '未识别的 Docker image store；无法确认实际镜像存储分区'
  image_store=containerd
  command -v systemctl >/dev/null || die 'containerd image store 需要检查实际服务启动配置；缺少 systemctl'
  docker_exec="$(systemctl show docker.service -p ExecStart --value --no-pager 2>/dev/null)" || die '无法读取 Docker 服务启动配置'
  [[ "$(awk '{n=gsub(/--containerd([=[:space:]]|$)/,"&");print n}' <<< "$docker_exec")" -le 1 ]] || die 'Docker containerd endpoint 启动参数重复，无法确认实际存储'
  if [[ "$docker_exec" =~ --containerd[=[:space:]]+([^[:space:];]+) ]]; then
    endpoint="${BASH_REMATCH[1]}"
    endpoint="${endpoint#unix://}"
  else
    # Docker 管理的 containerd 将持久化状态存放在 DockerRootDir 下。
    # 此时不把无关的系统 containerd 守护进程计入容量预算。
    [[ "$docker_exec" == *dockerd* ]] || die '无法确认 Docker 管理的 containerd；请检查实际启动配置'
    containerd_root="$docker_root/containerd/daemon"
    [[ -d "$containerd_root" && ! -L "$containerd_root" ]] || die "Docker 管理的 containerd 存储目录不存在或不安全：$containerd_root"
    return 0
  fi
  [[ "$endpoint" == /* && "$endpoint" != *[\"\'\\]* ]] || die '不支持的 Docker containerd endpoint；无法确认实际存储'
  command -v containerd >/dev/null || die '无法读取外置 containerd 的有效配置：缺少 containerd 命令'
  containerd_exec="$(systemctl show containerd.service -p ExecStart --value --no-pager 2>/dev/null)" || die '无法读取 containerd 服务启动配置'
  [[ "$containerd_exec" == *containerd* ]] || die '无法确认外置 containerd 服务；请检查 Docker endpoint 对应的实际服务'
  local option
  for option in '--root' '(--config|-c)' '(--address|-a)'; do
    [[ "$(awk -v name="$option" '{n=gsub("(^|[[:space:]])" name "([=[:space:]]|$)","&");print n}' <<< "$containerd_exec")" -le 1 ]] || die 'containerd 配置/root/address 启动参数重复，无法确认实际存储'
  done
  if [[ "$containerd_exec" =~ --config[=[:space:]]+([^[:space:];]+) || "$containerd_exec" =~ [[:space:]]-c[[:space:]]+([^[:space:];]+) ]]; then
    config_path="${BASH_REMATCH[1]}"
    [[ "$config_path" == /* && "$config_path" != *[\"\'\\]* ]] || die 'containerd config 路径格式不受支持'
    config_args+=(--config "$config_path")
  fi
  if [[ "$containerd_exec" =~ --root[=[:space:]]+([^[:space:];]+) ]]; then
    configured_root="${BASH_REMATCH[1]}"
    [[ "$configured_root" == /* && "$configured_root" != *[\"\'\\]* ]] || die 'containerd root 启动参数格式不受支持'
    config_args+=(--root "$configured_root")
  fi
  dump="$(containerd ${config_args[@]+"${config_args[@]}"} config dump 2>/dev/null)" || die '无法读取外置 containerd 有效配置'
  containerd_root="$(containerd_config_value '' root <<< "$dump")" || die 'containerd 有效配置缺少唯一绝对 root'
  [[ -z "${configured_root:-}" || "$containerd_root" == "$configured_root" ]] || die 'containerd root 启动参数与有效配置不一致'
  # containerd 1.x 与 2.3+/config-v4 的 gRPC socket 在 [grpc] 段内；2.0–2.2
  # （config v3）的有效配置把它提升为顶层键，[grpc] 段只剩 uid/gid。
  section_address="$(containerd_config_value '[grpc]' address <<< "$dump")" || section_address=''
  top_level_address="$(containerd_config_value '' address <<< "$dump")" || top_level_address=''
  if [[ -n "$section_address" && -n "$top_level_address" ]]; then
    [[ "$section_address" == "$top_level_address" ]] ||
      die 'containerd 有效配置 [grpc] 与顶层 address 不一致，无法确认唯一 gRPC address'
    address="$section_address"
  else
    address="${section_address:-$top_level_address}"
  fi
  if [[ -z "$address" && -S "$endpoint" ]]; then
    # containerd 2.3 及以上版本的 v4 配置可能不在 config dump 中输出默认 gRPC 套接字。
    # 此时以 Docker 显式指定的 --containerd 端点
    # 作为实际地址，并要求对应套接字存在。
    address="$endpoint"
  fi
  [[ -n "$address" ]] || die 'containerd 有效配置缺少唯一 gRPC address'
  if [[ "$containerd_exec" =~ --address[=[:space:]]+([^[:space:];]+) || "$containerd_exec" =~ [[:space:]]-a[[:space:]]+([^[:space:];]+) ]]; then
    address="${BASH_REMATCH[1]}"
  fi
  [[ "$address" == "$endpoint" ]] || die 'Docker endpoint 与被检查的 containerd 服务不一致，拒绝误判存储目录'
  [[ "$containerd_root" == /* && "$containerd_root" != / && "$containerd_root" != *'/../'* && "$containerd_root" != */.. && "$containerd_root" != *'/./'* && "$containerd_root" != */. && -d "$containerd_root" && ! -L "$containerd_root" ]] || die 'containerd root 必须为实际存在的规范绝对目录'
}

declare -A required_kib=() available_by_device=() inode_by_device=() mount_by_device=()
add_space_requirement() {
  local path="$1" amount="$2" row device available inodes mount
  row="$(df -Pk "$path" | awk 'NR==2 {print $1, $4, $6}')"
  read -r device available mount <<< "$row"
  [[ -n "$device" && "$available" =~ ^[0-9]+$ ]] || die "无法读取分区容量：${path}"
  inodes="$(df -Pi "$path" | awk 'NR==2 {print $4}')"
  [[ "$inodes" =~ ^[0-9]+$ ]] || die "无法读取分区 inode：${path}"
  required_kib["$device"]=$(( ${required_kib["$device"]:-0} + amount ))
  available_by_device["$device"]="$available"
  inode_by_device["$device"]="$inodes"
  mount_by_device["$device"]="$mount"
}
# 先确认当前使用的镜像存储。Docker 的 data-root 不会迁移外部 containerd 存储；
# 须分别计算两个文件系统的容量预算，不修改其配置。
detect_docker_image_storage
info "镜像存储=${image_store}${containerd_root:+，containerdRoot=$containerd_root}"
# 首装需要容纳整份介质及完整基础镜像展开；增量追加只按本次
# 选中的子系统包计算，不应把介质中未使用的基础包重复计入。
if [[ "$deployment_mode" == add-subsystems ]]; then
  selected_media_kib=0
  for requested_subsystem in "${requested_subsystems[@]}"; do
    selected_media_kib=$((selected_media_kib + $(du -sk "$media_dir/${package_components[$requested_subsystem]}" | awk '{print $1}')))
  done
  add_space_requirement "$existing_parent" "$((selected_media_kib + 2 * 1024 * 1024))"
  if [[ "$image_store" == containerd ]]; then
    add_space_requirement "$docker_root" "$((2 * 1024 * 1024))"
    add_space_requirement "$containerd_root" "$((5 * 1024 * 1024))"
  else
    add_space_requirement "$docker_root" "$((5 * 1024 * 1024))"
  fi
else
  add_space_requirement "$existing_parent" "$((media_kib + 5 * 1024 * 1024))"
  if [[ "$image_store" == containerd ]]; then
    add_space_requirement "$docker_root" "$((5 * 1024 * 1024))"
    add_space_requirement "$containerd_root" "$((20 * 1024 * 1024))"
  else
    add_space_requirement "$docker_root" "$((20 * 1024 * 1024))"
  fi
fi
for device in "${!required_kib[@]}"; do
  device_available="${available_by_device[$device]}"
  device_required="${required_kib[$device]}"
  device_inodes="${inode_by_device[$device]}"
  device_mount="${mount_by_device[$device]}"
  info "分区 ${device_mount}：可用=$((device_available / 1024)) MiB，预算=$((device_required / 1024)) MiB，inode=${device_inodes}"
  ((device_available > device_required)) || die "分区 ${device_mount} 空间不足"
  ((device_inodes >= 10000)) || die "分区 ${device_mount} 可用 inode 少于 10000"
done
info "CPU=${cpu_count}，内存=$((memory_kib / 1024)) MiB，交付介质=$((media_kib / 1024)) MiB"
if ((memory_kib < 16 * 1024 * 1024)); then
  if [[ "$deployment_mode" == add-subsystems ]]; then
    confirm_resource_warning "物理内存少于 16 GiB；追加 ${requested_subsystems[*]} 前必须确认已有服务与新服务的合计内存需求"
  else
    confirm_resource_warning '物理内存少于 16 GiB，完整启动全部子系统和数据分析可能耗尽内存'
  fi
fi

printf '\n========== 3/6 检查并安装宿主依赖 ==========\n'
[[ -r /etc/os-release ]] || die '无法读取 /etc/os-release'
# shellcheck disable=SC1091
source /etc/os-release
os_codename="${VERSION_CODENAME:-${UBUNTU_CODENAME:-}}"
host_package=""
if [[ "${ID:-}" == ubuntu && -n "${VERSION_ID:-}" && -n "$os_codename" ]]; then
  matches=("$media_dir"/host-dependencies-ubuntu-"${VERSION_ID}"-"${os_codename}"-*-linux-amd64.tar.gz)
  ((${#matches[@]} <= 1)) || die '发现多个匹配当前 Ubuntu 的宿主依赖包'
  ((${#matches[@]} == 0)) || host_package="${matches[0]}"
fi
[[ "$deployment_mode" != upgrade ]] || preflight_upgrade_target
deployment_baseline_gate
assets_preflight_done=false
if [[ "$deployment_mode" != add-subsystems ]] && command -v jq >/dev/null 2>&1 &&
    command -v flock >/dev/null 2>&1 && command -v install >/dev/null 2>&1; then
  preflight_assets_install
  assets_preflight_done=true
fi
if [[ "$interactive_upgrade" == true && "$check_only" != true ]]; then
  info "本次交付：${media_dir}"
  info "升级目标：${target_dir}；平台与前端将更新，未明确指定的业务子系统不会受控升级"
  confirm_upgrade_action '是否开始升级（可能短暂中断平台访问，保留现有数据及配置）？'
fi
# 在备份、导入或变更资产前，将显式同意应用 Compose 的选择记入会话身份。
# 这样仅执行审核后再添加 --apply-compose 时，
# 无需重置已有安装身份。
if [[ "$deployment_mode" == upgrade && "$check_only" != true && "$apply_compose" != true ]]; then
  review_directory="$(mktemp -d "${TMPDIR:-/tmp}/uip-compose-review.XXXXXX")"
  if ! python3 - "${assets[0]}" "$review_directory/template.yml" <<'PY'
import sys, tarfile
with tarfile.open(sys.argv[1], 'r:gz') as archive:
    members=[item for item in archive if item.name.removeprefix('./')=='docker-compose.yml' and item.isfile()]
    if len(members)!=1: raise SystemExit(1)
    with archive.extractfile(members[0]) as stream, open(sys.argv[2],'wb') as out: out.write(stream.read(4*1024*1024+1))
PY
  then rm -rf -- "$review_directory"; die '无法提取已核验的编排审阅模板'; fi
  detect_subsystem_block_states "$target_dir/docker-compose.yml" "$review_directory/states"
  synthesize_compose_with_trim "$review_directory/template.yml" "$review_directory/states" "$review_directory/merged.yml"
  if ! cmp -s "$target_dir/docker-compose.yml" "$review_directory/merged.yml"; then
    [[ ! -L "$target_dir/docker-compose.yml.merged" && ( ! -e "$target_dir/docker-compose.yml.merged" || -f "$target_dir/docker-compose.yml.merged" ) ]] || die '编排审阅路径不安全'
    install -m 600 "$review_directory/merged.yml" "$target_dir/docker-compose.yml.merged"
    diff -u "$target_dir/docker-compose.yml" "$review_directory/merged.yml" | head -200 || true
    rm -rf -- "$review_directory"
    if [[ "$interactive_upgrade" == true ]]; then
      confirm_upgrade_action "完整编排差异已保存 ${target_dir}/docker-compose.yml.merged，是否采用并继续？"
      apply_compose=true
    else
      die "请先审阅 ${target_dir}/docker-compose.yml.merged，再使用 --upgrade --apply-compose；尚未创建安装会话、备份、导入或更换资产"
    fi
  else rm -rf -- "$review_directory"; fi
fi
if [[ "$check_only" != true ]]; then
  workflow_components=(common platform frontend)
  if [[ "$deployment_mode" == install ]]; then
    for workflow_component in customer-opportunity customer-portal contract project settlement data-analysis; do
      [[ -z "${package_components[$workflow_component]:-}" ]] || workflow_components+=("$workflow_component")
    done
    workflow_stage_list=(host-deps assets packages configure common-import platform-import platform frontend-import frontend license-prepare)
    for workflow_component in "${workflow_components[@]}"; do
      case "$workflow_component" in common|platform|frontend) continue ;; esac
      workflow_stage_list+=("${workflow_component}-import" "${workflow_component}-prepare")
    done
    workflow_stage_list+=(license verify controlled)
  elif [[ "$deployment_mode" == upgrade ]]; then
    workflow_components+=("${requested_subsystems[@]}")
    workflow_stage_list=(host-deps backup common-import platform-import assets compose platform packages frontend-import frontend)
    for workflow_component in "${requested_subsystems[@]}"; do workflow_stage_list+=("${workflow_component}-import" "${workflow_component}-stage"); done
    workflow_stage_list+=(verify license controlled)
  else
    workflow_components=("${requested_subsystems[@]}")
    workflow_stage_list=(host-deps control-plane)
    for workflow_component in "${requested_subsystems[@]}"; do workflow_stage_list+=("${workflow_component}-import" "${workflow_component}-prepare"); done
    workflow_stage_list+=(verify controlled)
  fi
  workflow_stages="$(IFS=,; printf '%s' "${workflow_stage_list[*]}")"
  workflow_open
  workflow_start host-deps
fi
if [[ "$skip_host_deps" != true && -n "$host_package" ]]; then
  require_media_binding "$(basename -- "$host_package")"
  require_media_sidecar "$(basename -- "$host_package")"
  require_media_binding install-host-dependencies.sh
  [[ -x "$media_dir/install-host-dependencies.sh" && ! -L "$media_dir/install-host-dependencies.sh" ]] || die '缺少 install-host-dependencies.sh'
  if [[ "$check_only" == true ]]; then
    info "已匹配宿主依赖包：$(basename -- "$host_package")（check-only 未安装）"
  else
    bash "$media_dir/install-host-dependencies.sh" "$host_package"
  fi
elif [[ "$skip_host_deps" != true ]]; then
  for command_name in jq curl update-ca-certificates openssl flock ss cmp install; do
    command -v "$command_name" >/dev/null 2>&1 || die "交付中没有匹配 ${ID:-unknown} ${VERSION_ID:-unknown} ${os_codename:-unknown} 的宿主依赖包，且缺少命令 ${command_name}"
  done
  warn '交付中没有匹配当前系统的宿主依赖包；已确认必需命令由系统提供'
else
  for command_name in jq curl update-ca-certificates openssl flock ss cmp install; do
    command -v "$command_name" >/dev/null 2>&1 || die "--skip-host-deps 已指定，但系统仍缺少命令：${command_name}"
  done
  info '已显式跳过宿主依赖安装，必需命令已检查'
fi
if [[ -f "$media_dir/release-plan.json" && ! -L "$media_dir/release-plan.json" ]]; then
  python3 - "$media_dir/release-plan.json" <<'PY' || die '冻结材料计划无效或为 BLOCKED，拒绝继续安装'
import json, sys
with open(sys.argv[1], encoding="utf-8") as stream:
    value = json.load(stream)
if not isinstance(value, dict) or value.get("schema") != "uip.release-materials/v1":
    raise SystemExit(2)
if value.get("overall_status") not in ("READY", "NEEDS_ACTION"):
    raise SystemExit(2)
PY
fi
[[ "$check_only" == true ]] || workflow_done host-deps

assets_preflight_skipped=false
if [[ "$deployment_mode" != add-subsystems && "$assets_preflight_done" != true ]]; then
  if [[ "$check_only" == true ]]; then
    # check-only 不安装宿主依赖；缺基础命令时无法隔离试装资产，明确标为未检查而不是误报通过。
    missing_preflight_commands=()
    for command_name in jq flock install; do
      command -v "$command_name" >/dev/null 2>&1 || missing_preflight_commands+=("$command_name")
    done
    if (( ${#missing_preflight_commands[@]} > 0 )); then
      warn "宿主依赖尚未安装（缺少：${missing_preflight_commands[*]}），本次 --check-only 未执行资产安装预检；正式安装会先自动安装随包宿主依赖后再预检。"
      assets_preflight_skipped=true
    else
      preflight_assets_install
    fi
  else
    preflight_assets_install
  fi
fi

if [[ "$check_only" == true ]]; then
  # 已安装目标复用同一只读计划核心；新目标尚无 deploy.sh，保留介质/资产预检结果。
  plan_action=""
  case "$deployment_mode" in
    install) plan_action=install ;;
    upgrade) plan_action=upgrade ;;
    add-subsystems) plan_action=add ;;
  esac
  if [[ -n "$plan_action" && -f "$target_dir/bin/deploy.sh" && -f "$target_dir/bin/deployment-plan.py" && -f "$target_dir/.env" ]]; then
    plan_args=(plan --action "$plan_action" --format "$delivery_output_format" --component platform --component frontend)
    [[ "$forward_recovery" != true ]] || plan_args+=(--forward-recovery)
    [[ -z "$recovery_evidence" ]] || plan_args+=(--recovery-evidence "$recovery_evidence")
    if [[ "$deployment_mode" == upgrade ]]; then
      [[ -n "$media_compatibility_file" && -f "$media_compatibility_file" &&
         ! -L "$media_compatibility_file" ]] || die '缺少已核验的本批兼容性声明'
      plan_args+=(--compatibility "$media_compatibility_file")
    fi
    for requested_subsystem in "${requested_subsystems[@]}"; do
      plan_args+=(--component "$requested_subsystem")
    done
    plan_status=0
    (cd "$target_dir" && bash ./bin/deploy.sh "${plan_args[@]}") || plan_status=$?
    if ((plan_status == 2)); then
      die '统一预检存在阻断项；按上面的检查证据处理后重新执行 --check-only，不能凭旧结果继续'
    elif ((plan_status == 3)); then
      warn '统一预检存在待办项；--check-only 保持介质检查通过，但正式执行前仍需处理上面的待办'
    elif ((plan_status != 0)); then
      die "统一预检执行失败（退出码 ${plan_status}）；保留输出并联系部署负责人"
    fi
    if [[ -n "$plan_baseline" && $plan_status == 0 ]]; then
      baseline_tmp="$(mktemp "$(dirname "$plan_baseline")/.plan-baseline.XXXXXX")" || die '无法创建计划基线临时文件'
      if (cd "$target_dir" && bash ./bin/deploy.sh "${plan_args[@]}" --format json) >"$baseline_tmp" && chmod 600 "$baseline_tmp" && mv -f -- "$baseline_tmp" "$plan_baseline"; then
        info "计划基线已保存：$plan_baseline"
      else
        rm -f -- "$baseline_tmp"
        die '无法保存计划基线；执行前不会启用 .env 指纹比对'
      fi
    elif [[ -n "$plan_baseline" ]]; then
      warn '计划不是 READY，未保存计划基线；正式执行前需重新生成'
    fi
  fi
  if [[ "$deployment_mode" == add-subsystems ]]; then
    printf '\n增量检查完成：交付介质、现有部署、架构、资源、Docker、宿主依赖与资产安装条件通过；未修改现有部署或启动容器。\n'
  elif [[ "$assets_preflight_skipped" == true ]]; then
    printf '\n检查完成：交付介质、架构、资源、Docker 与已装宿主依赖通过；宿主依赖未安装，资产安装预检未执行（正式安装会先自动安装随包依赖）；未修改现有部署或启动容器。\n'
  else
    printf '\n检查完成：交付介质、架构、资源、Docker、宿主依赖与资产安装条件通过；未修改现有部署或启动容器。\n'
  fi
  exit 0
fi

printf '\n========== 4/6 事务安装部署资产与镜像包 ==========\n'
if [[ "$deployment_mode" == add-subsystems ]]; then
  # 增量追加不安装或更换部署资产，避免覆盖现有 Compose 裁剪、IP、
  # 密钥和运行配置。新子系统所需清单必须已由首装或显式资产升级提供。
  cd "$target_dir"
  # 由 deploy.sh 在统一部署锁内成对重建 platform-api 与 Agent，
  # 并校验两者看到的 subsystems.d 摘要。入口不复制特权 Compose 逻辑。
  workflow_start control-plane
  if ! workflow_was_done control-plane; then bash ./bin/deploy.sh reload-control-plane; fi
  bash ./bin/deploy.sh verify
  workflow_done control-plane


  printf '\n========== 5/6 复用现有运行配置 ==========\n'
  info '增量模式不执行 configure，已保留原 IP、端口、管理员、密钥和接入凭据'

  printf '\n========== 6/6 仅导入并准备指定子系统 ==========\n'
  for requested_subsystem in "${requested_subsystems[@]}"; do
    package_name="${package_components[$requested_subsystem]}"
    package_path="$media_dir/$package_name"
    # 先用 deploy.sh 完整验证内层清单、架构、config digest 和 Registry digest，
    # 成功后才把包留在 packages/，避免损坏包成为后续自动发现的输入。
    workflow_import "${requested_subsystem}-import" workflow_root_import_proof workflow_root_import "$requested_subsystem" "$package_path"
    if [[ -z "${incremental_package_already_present[$requested_subsystem]:-}" ]]; then
      install -m 600 "$package_path" "$target_dir/packages/$package_name"
    fi
    install -m 600 "${package_path}.sha256" "$target_dir/packages/${package_name}.sha256"
    workflow_run "${requested_subsystem}-prepare" bash ./bin/deploy.sh delivery-prepare "$requested_subsystem" "$package_path"
  done
  (
    cd "$target_dir/packages"
    mapfile -t installed_image_packages < <(
      find . -maxdepth 1 -type f -name '*-linux-amd64.tar.gz' ! -name 'host-dependencies-*' -printf '%f\n' | LC_ALL=C sort
    )
    ((${#installed_image_packages[@]} > 0)) || die 'packages/ 中没有可登记的镜像包'
    sha256sum -- "${installed_image_packages[@]}" > SHA256SUMS
    sha256sum --check SHA256SUMS
  )
  install_operations_tool
  workflow_start verify
  bash ./bin/deploy.sh verify
  for requested_subsystem in "${requested_subsystems[@]}"; do
    bash ./bin/deploy.sh status "$requested_subsystem"
  done
  workflow_done verify
  workflow_finish_controlled

  if [[ "$installation_pending" == true ]]; then printf '\n========== 增量准备就绪，受控接入/授权待完成 ==========\n'
  else printf '\n========== 增量交付验收完成 ==========\n'; fi
  printf '基础平台、基础设施、统一前端、已有子系统及数据未重新部署。\n'
  if [[ "$installation_pending" == true ]]; then
    printf '请按会话中的待办在平台采用对应 prod 环境；只有 READY_TO_CONTINUE 才执行 continue，然后 verify。\n'
  else printf '本次所选系统的实际镜像、运行组件、平台清单及当前商业授权确认已通过。\n'; fi
  exit 0
fi

# ===========================================================================
# 升级模式：已部署环境（平台 + 前端，可选受控升级已部署子系统）
#
# 与首装/增量的区别：复用现有 .env/.release.env，不执行 configure/install；依次做
# 装资产 → 合并编排（保留模块裁剪）→ 重载控制面 → 归档同组件旧包并导入新包
# → 发布平台与前端 → 按需把子系统镜像暂存给平台受控升级。
# ===========================================================================
if [[ "$deployment_mode" == upgrade ]]; then
  workflow_run backup backup_before_upgrade
  # 导入只登记不可变镜像，不启动业务。必须先取得新版平台候选，不能让旧
  # Agent/API 先解析新版清单；成对切换由 deploy-service 的备份/迁移门禁完成。
  cd "$target_dir"
  workflow_run common-import bash ./bin/deploy.sh import "$media_dir/${package_components[common]}"
  workflow_run platform-import bash ./bin/deploy.sh import "$media_dir/${package_components[platform]}"
  platform_record="$(awk 'NR==1 && NF==1 {v=$0; next} {bad=1} END {if (bad || NR!=1) exit 1; print v}' manifests/platform.latest)" || die '平台导入指针无效'
  [[ "$platform_record" == platform-*.imported && "$platform_record" != */* ]] || die '平台导入记录名称无效'
  [[ -f "manifests/$platform_record" && ! -L "manifests/$platform_record" ]] || die '平台导入记录不安全'
  platform_upgrade_image="$(awk -F= '$1=="IMAGE_REF" {n++; v=substr($0,index($0,"=")+1)} END {if(n!=1)exit 1; print v}' "manifests/$platform_record")" || die '平台导入摘要不唯一'
  [[ "$platform_upgrade_image" =~ ^127\.0\.0\.1:[0-9]+/uip/platform@sha256:[a-f0-9]{64}$ ]] || die '平台升级候选不是受控本地仓库不可变摘要'

  printf '\n========== 4/6 安装部署资产 ==========\n'
  # 本模式分两趟执行（先出差异、审阅后再 --apply-compose 落地）。第一趟已安装资产并
  # 写入重载门禁，第二趟若再调用 install-assets.sh 会被门禁正确拒绝。因此当门禁登记的
  # 资产摘要与本次介质一致时跳过重复安装；摘要不同则必须先把上一次重载收尾。
  workflow_assets_install

  printf '\n========== 合并编排（保留现有模块裁剪）==========\n'
  workflow_start compose
  cd "$target_dir"
  [[ -d runtime && ! -L runtime && ! -L runtime/.deploy.lock ]] || die '升级运行目录/部署锁不安全'
  exec 9>runtime/.deploy.lock
  flock -w 60 9 || die '其他发布任务正在运行；请等待后重试编排合并'
  if [[ ! -f docker-compose.yml.dist ]]; then
    info '未生成 docker-compose.yml.dist；现有编排与本次资产一致，跳过合并'
  else
    states_file="$(mktemp "$target_dir/.compose-states.XXXXXX")"
    detect_subsystem_block_states docker-compose.yml "$states_file"
    while IFS='=' read -r block_key block_state; do
      [[ -n "$block_key" ]] || continue
      info "模块 ${block_key}: ${block_state}"
    done < "$states_file"

    merged_file="$target_dir/docker-compose.yml.merged"
    synthesize_compose_with_trim docker-compose.yml.dist "$states_file" "$merged_file"
    rm -f -- "$states_file"

    if cmp -s docker-compose.yml "$merged_file"; then
      info '合并结果与现有编排一致，无需变更'
      rm -f -- "$merged_file"
    else
      printf '\n--- 合并结果相对当前编排的差异（最多显示 200 行）---\n'
      diff -u docker-compose.yml "$merged_file" | head -200 || true
      printf '%s\n' '--- 差异结束 ---'
      if [[ "$apply_compose" != true && "$interactive_upgrade" == true ]]; then
        info "完整合并结果：${merged_file}；请核对自定义挂载、网络及服务配置（上方差异可能截断）"
        confirm_upgrade_action '是否采用上述合并编排并继续本次升级？'
        apply_compose=true
      fi
      if [[ "$apply_compose" != true ]]; then
        die "请先审阅上面的差异，再重新执行并追加 --apply-compose 落地合并结果。
合并结果已保存：${merged_file}（现有编排未被修改）"
      fi
      backup_compose="$(mktemp "$target_dir/docker-compose.yml.bak-$(date -u '+%Y%m%dT%H%M%SZ')-XXXXXX")" || die '无法创建原编排备份文件'
      cp -a docker-compose.yml "$backup_compose"
      mv -f -- "$merged_file" docker-compose.yml
      if compose_config_check; then
        info "已套用合并编排；原编排备份：${backup_compose}"
      else
        cp -a "$backup_compose" docker-compose.yml
        die "合并后的编排校验失败，已回退到原编排（备份保留在 ${backup_compose}）"
      fi
    fi
  fi
  flock -u 9
  exec 9>&-
  workflow_done compose

  printf '\n========== 5/6 协调发布平台与控制面 ==========\n'
  # 首次导入可能使用旧解析器。发布代理与镜像的组合信息前，
  # 须用新解析器重新读取已经验证的 common 包。
  bash ./bin/deploy.sh import "$media_dir/${package_components[common]}"
  workflow_publish platform workflow_root_release_proof workflow_root_deploy platform "$media_dir/${package_components[platform]}" "$media_dir/${package_components[common]}"
  [[ ! -e runtime/.control-plane-reload-required && ! -L runtime/.control-plane-reload-required ]] ||
    die '控制面重载后标记仍存在，拒绝在可能混合的版本上继续升级'
  bash ./bin/deploy.sh verify

  if [[ "$legacy_license_mode" == upgrade ]]; then
    printf '\n--- 冻结升级前仍运行的旧业务范围 ---\n'
    bash ./bin/deploy.sh license-prepare || die '旧业务基线冻结失败；平台升级和已有授权状态保留，未导入或暂存业务新镜像'
  fi

  printf '\n========== 6/6 导入并发布平台与前端 ==========\n'
  workflow_start packages
  superseded_dir=""
  mkdir -p "$target_dir/packages"
  chmod 750 "$target_dir/packages"
  for package_path in "${image_packages[@]}"; do
    package_component="$(package_component_of "$package_path")"
    case "$package_component" in
      common|platform|frontend) ;;
      *) [[ -n "${requested_subsystem_set[$package_component]:-}" ]] || continue ;;
    esac
    install_upgrade_package "$package_path"
  done
  [[ -z "$superseded_dir" ]] || info "被替换的旧镜像包归档在：$superseded_dir"
  (
    cd "$target_dir/packages"
    mapfile -t upgrade_image_packages < <(
      find . -maxdepth 1 -type f -name '*-linux-amd64.tar.gz' ! -name 'host-dependencies-*' -printf '%f\n' | LC_ALL=C sort
    )
    (( ${#upgrade_image_packages[@]} > 0 )) || die 'packages/ 中没有可登记的镜像包'
    sha256sum -- "${upgrade_image_packages[@]}" > SHA256SUMS
    sha256sum --check SHA256SUMS
  )
  install -m 600 "$compatibility_lock" "$target_dir/packages/PLATFORM_ASSETS_COMPATIBILITY.delivery.env"
  [[ ! -f "$media_dir/BUILD_INFO.txt" ]] || install -m 600 "$media_dir/BUILD_INFO.txt" "$target_dir/packages/BUILD_INFO.delivery.txt"
  workflow_done packages

  printf '\n--- 平台与前端 ---\n'
  if [[ -e runtime/.control-plane-reload-required || -L runtime/.control-plane-reload-required ]]; then
    info '平台发布后仍需成对重载控制面'
    bash ./bin/deploy.sh reload-control-plane
  fi
  workflow_import frontend-import workflow_root_import_proof workflow_root_import frontend "$target_dir/packages/${package_components[frontend]}"
  workflow_publish frontend workflow_root_release_proof workflow_root_deploy frontend "$target_dir/packages/${package_components[frontend]}"

  if (( ${#requested_subsystems[@]} > 0 )); then
    printf '\n--- 受控升级子系统（导入 + 暂存）---\n'
    for requested_subsystem in "${requested_subsystems[@]}"; do
      package_name="${package_components[$requested_subsystem]:-}"
      [[ -n "$package_name" ]] || die "介质中缺少 ${requested_subsystem} 镜像包"
      workflow_import "${requested_subsystem}-import" workflow_root_import_proof workflow_root_import "$requested_subsystem" "$target_dir/packages/$package_name"
      workflow_run "${requested_subsystem}-stage" bash ./bin/deploy.sh upgrade "$requested_subsystem"
    done
    info '业务镜像已暂存。若本批次交付新的运行批准，必须在平台受控更新前按 OFFLINE_RUNBOOK.md 5.5 执行 bin/publish-runtime-approvals.py；不要手工覆盖 runtime-approvals。'
  fi

  workflow_start verify
  bash ./bin/deploy.sh verify
  bash ./bin/deploy.sh status
  workflow_done verify

  # 商业授权（可选）：--customer 提示核对授权实例并导出申请；--license 直接导入
  # 已签发许可证。两者可单独使用；导入前会自动验签并展示各系统授权明细。
  workflow_start license
  if [[ -n "$license_customer" && -z "$license_file_arg" && "$legacy_license_mode" != upgrade ]]; then
    info '请在平台商业授权页面核对既有实例身份并导出签发申请；本入口不自动初始化或替换现有实例'
    info "申请的客户标识应为：${license_customer}（既有实例以平台持久化身份为准）"
  fi
  if [[ -n "$license_file_arg" ]] && workflow_was_done license; then
    info '原许可证导入已处理，保留平台可能已独立续期的当前授权，由最终核验确认；不重导旧版本'
  elif [[ -n "$license_file_arg" ]]; then
    activator="$media_dir/license-activate.sh"
    [[ -f "$activator" && ! -L "$activator" ]] ||
      die "介质中缺少 license-activate.sh，无法在本流程内完成商业授权；可单独获取该脚本后执行：
  $activator 位置的替代：将 license-activate.sh 放入介质目录后重跑，或手工执行其 --customer/--license 参数"
    printf '\n========== 商业授权 ==========\n'
    activator_args=(--deploy-dir "$target_dir")
    [[ -z "$license_customer" ]] || activator_args+=(--customer "$license_customer")
    [[ -z "$license_file_arg" ]] || activator_args+=(--license "$license_file_arg")
    bash "$activator" "${activator_args[@]}" ||
      die '商业授权步骤失败；升级成果已保留。可修复后单独重跑 license-activate.sh，不需要重新升级'
  fi
  workflow_done license
  workflow_finish_controlled

  install_operations_tool

  if [[ "$installation_pending" == true ]]; then printf '\n========== 平台升级就绪，受控收尾/授权待完成 ==========\n'
  else printf '\n========== 本次升级验收完成 ==========\n'; fi
  printf '平台与前端已发布；现有数据、密钥、模块裁剪和运行配置均未重置。\n'
  if [[ "$legacy_license_mode" == bridge ]]; then
    printf '兼容升级完成，旧业务运行镜像保留；下一步导出真实实例申请（已有身份须匹配）：\n'
    printf '  bash %q --deploy-dir %q --customer %q --confirm-initialize\n' "$media_dir/license-activate.sh" "$target_dir" "$legacy_license_customer"
  fi
  if (( ${#requested_subsystems[@]} > 0 )) && [[ "$installation_pending" == true ]]; then
    printf '以下子系统的镜像已暂存，请在基础平台应用页面执行"受控更新/重试"，然后：\n'
    for requested_subsystem in "${requested_subsystems[@]}"; do
      printf '  ./bin/deploy.sh status %s\n' "$requested_subsystem"
      printf '  ./bin/deploy.sh verify %s\n' "$requested_subsystem"
    done
    printf '只有 status 为 READY_TO_CONTINUE 时才执行 continue <子系统>；VERIFIED 时直接 verify，并核对实际运行镜像为本次目标版本。\n'
  fi
  if [[ "$legacy_license_mode" == upgrade ]]; then
    printf '旧范围已冻结，指定业务镜像已暂存；在平台完成受控更新后执行：\n'
    printf '  cd %q && bash ./bin/deploy.sh license-migrate\n' "$target_dir"
    printf 'license-migrate 会处理整个实际冻结旧集合；若有其他旧系统，先核对它们的批准及更新安排。\n'
    printf '迁移确认不等于正式授权。随包带有效 JWS 时，确认购买范围后单独执行：\n'
    printf '  cd %q && bash ./bin/deploy.sh license-activate\n' "$target_dir"
    printf 'license-activate 会处理整个签署购买集合并等待全部必需组件当前 ACK，不自动从本入口调用。\n'
  fi
  [[ ! -x "$target_dir/系统运维工具.sh" ]] || printf '日常运维：cd %s && sudo ./系统运维工具.sh\n' "$target_dir"
  exit 0
fi

if [[ -e "$target_dir/packages" || -L "$target_dir/packages" ]]; then
  [[ -d "$target_dir/packages" && ! -L "$target_dir/packages" ]] || die "已有 packages 路径不是安全目录：${target_dir}/packages"
  declare -A incoming_package_names=()
  for package_path in "${image_packages[@]}"; do
    incoming_package_names["$(basename -- "$package_path")"]=1
  done
  shopt -s nullglob
  existing_packages=("$target_dir"/packages/*-linux-amd64.tar.gz)
  for existing_package in "${existing_packages[@]}"; do
    [[ -f "$existing_package" && ! -L "$existing_package" ]] || die "已有镜像包不是安全普通文件：${existing_package}"
    existing_name="$(basename -- "$existing_package")"
    [[ -n "${incoming_package_names[$existing_name]:-}" ]] ||
      die "packages/ 中存在其他版本镜像包 ${existing_name}；为避免自动发现歧义，请改用升级流程或先人工隔离旧包"
  done
fi
info '自动检查资产安装脚本并事务安装部署资产；镜像推送前将检查 Docker 桥地址、路由和仓库可达性'
workflow_assets_install
if [[ -f "$target_dir/docker-compose.yml.dist" ]]; then
  if cmp -s "$target_dir/docker-compose.yml" "$target_dir/docker-compose.yml.dist"; then
    rm -f -- "$target_dir/docker-compose.yml.dist"
  else
    die '检测到已有经裁剪或旧版 docker-compose.yml；一键首装不会擅自覆盖，请按升级流程合并 docker-compose.yml.dist'
  fi
fi
install_operations_tool
workflow_start packages
mkdir -p "$target_dir/packages"
chmod 750 "$target_dir/packages"
for package_path in "${image_packages[@]}"; do
  install -m 600 "$package_path" "$target_dir/packages/$(basename -- "$package_path")"
  install -m 600 "${package_path}.sha256" "$target_dir/packages/$(basename -- "${package_path}.sha256")"
done
[[ ! -f "$media_dir/BUILD_INFO.txt" ]] || install -m 644 "$media_dir/BUILD_INFO.txt" "$target_dir/packages/BUILD_INFO.delivery.txt"
compatibility_temporary="$(mktemp "$target_dir/packages/.PLATFORM_ASSETS_COMPATIBILITY.delivery.env.XXXXXX")"
if ! install -m 600 "$compatibility_lock" "$compatibility_temporary"; then
  rm -f -- "$compatibility_temporary"
  die '无法保存 Platform/资产兼容性锁'
fi
mv -f -- "$compatibility_temporary" "$target_dir/packages/PLATFORM_ASSETS_COMPATIBILITY.delivery.env"
(
  cd "$target_dir/packages"
  packages=(common-infrastructure-linux-amd64.tar.gz *-backend-*-linux-amd64.tar.gz frontend-*-linux-amd64.tar.gz)
  sha256sum -- "${packages[@]}" > SHA256SUMS
  sha256sum --check SHA256SUMS
)
workflow_done packages

printf '\n========== 5/6 生成或复用一次性运行配置 ==========\n'
cd "$target_dir"
workflow_start configure
if [[ "$resume_installation" == true ]] && python3 "$delivery_files_tool" config --target "$target_dir"; then
  info '已有安全运行配置，恢复时保留 IP、密钥、管理员设置及实例身份'
elif workflow_was_done configure; then
  die '已完成运行配置当前缺失或权限不正确；请修复原配置后恢复，不能重建身份'
elif [[ "$skip_configure" == true ]]; then
  [[ -f .env && ! -L .env && -f .release.env && ! -L .release.env ]] || die '--skip-configure 只能用于已存在安全 .env/.release.env 的部署'
  info '复用已有运行配置'
else
  [[ ! -e .env && ! -e .release.env && ! -L .env && ! -L .release.env ]] ||
    die '发现未完成的一次性配置，请先核对并补齐原 .env/.release.env 后 --resume；不会自动覆盖已生成密钥'
  bash ./bin/deploy.sh configure
fi
python3 "$delivery_files_tool" config --target "$target_dir" || die '配置未完整生成或权限不是0600'
workflow_done configure

printf '\n========== 6/6 按基础平台优先顺序安装 ==========\n'
if [[ -e runtime/.control-plane-reload-required || -L runtime/.control-plane-reload-required ]]; then
  [[ -f runtime/.control-plane-reload-required && ! -L runtime/.control-plane-reload-required ]] ||
    die '控制面重载标记不是安全普通文件'
  info '部署资产已更新，先成对重载 platform-api 与 subsystem-provisioner'
  bash ./bin/deploy.sh reload-control-plane
  [[ ! -e runtime/.control-plane-reload-required && ! -L runtime/.control-plane-reload-required ]] ||
    die '控制面重载后标记仍存在，拒绝在可能混合的版本上继续安装'
fi
bash ./bin/deploy.sh install
if [[ -e runtime/.control-plane-reload-required || -L runtime/.control-plane-reload-required ]]; then
  bash ./bin/deploy.sh reload-control-plane
fi
workflow_start verify
bash ./bin/deploy.sh verify
bash ./bin/deploy.sh status
workflow_done verify
workflow_finish_controlled

platform_origin="$(awk -F= '$1=="APP_PUBLIC_BASE_URL" {sub(/^[^=]*=/,""); print; exit}' .env)"
keycloak_origin="$(awk -F= '$1=="KEYCLOAK_PUBLIC_URL" {sub(/^[^=]*=/,""); print; exit}' .env)"
if [[ "$installation_pending" == true ]]; then printf '\n========== 基础部署就绪，业务接入/授权待完成 ==========\n'
else printf '\n========== 本次交付部署验收完成 ==========\n'; fi
printf '基础平台：%s\n' "$platform_origin"
printf 'Keycloak/SSO：%s\n' "$keycloak_origin"
printf '基础平台、文件网关、Keycloak 和统一前端已安装并验收。\n'
[[ ! -x "$target_dir/系统运维工具.sh" ]] || printf '日常运维：cd %s && sudo ./系统运维工具.sh\n' "$target_dir"
if [[ "$installation_pending" == true ]]; then
  printf '请按会话中的待办完成业务受控接入和正式授权；只有 READY_TO_CONTINUE 才执行 continue，然后 verify。\n'
else
  printf '本次启用业务系统的目标镜像、运行组件、平台清单及当前商业授权确认已通过。\n'
fi
