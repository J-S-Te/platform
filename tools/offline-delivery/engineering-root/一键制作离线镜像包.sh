#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
builder="${script_dir}/platform/deploy/production/bin/build-offline-packages.sh"
deploy_dir="${script_dir}/platform/deploy/production"
server_installer="${script_dir}/服务器一键离线部署.sh"
operations_tool="${script_dir}/系统运维工具.sh"
fresh_install_reset_tool="${script_dir}/清理首次部署产物.sh"
fresh_install_reset_guide="${script_dir}/操作说明/失败首装清理与重新部署.md"
version=""
output_dir=""
reuse_host_deps_from=""
skip_host_deps=false
preflight_only=false
candidate=false
no_pause=false
component_selection=""
components_explicit=false
portal_directory_mode="${PORTAL_CUSTOMER_DIRECTORY_MODE:-crm}"
extend_from=""
license_file=""
runtime_approval_dir=""
runtime_approval_approved=false
installation_scenario=""
installation_customer=""
legacy_license_mode=""
legacy_license_customer=""
prepare_crm_inventory=""
prepare_crm_image=""
prepare_expand_inventory=""
prepare_upgrade_inventory=""
upgrade_applications=""
expand_applications=""
expand_image_mapping=""
expand_target_store="native"
expand_store_explicit=false
expand_image_packages=()
approval_operator=""
review_from=""
reuse_approved_crm_image=false
release_spec=""
release_build_key=""
release_release_key=""
release_trust_dir=""
release_verifier=""
release_profiles_dir=""
customer_name=""
customer_public_keys=""
customer_verifier=""

case "${1:-}" in
  plan|build|finalize|package|status|resume)
    command_name="$1"
    shift
    exec python3 "${script_dir}/platform/deploy/production/bin/release-workflow.py" "$command_name" "$@"
    ;;
esac

# usage 展示根离线制作入口与发行、运行来源的区别。
# 参数：无。
# 返回值：标准输出帮助文本。
usage() {
  cat <<'EOF'
用法：./一键制作离线镜像包.sh [选项]

选项：
  --version <版本>                 指定发布版本；默认为 UTC 时间戳-offline
  --output <目录>                 指定交付目录
  --components <列表>            指定要打包的业务系统，逗号或空格分隔
  --portal-customer-directory-mode crm|local
                                  发行依赖选择，默认crm；CLI优先于PORTAL_CUSTOMER_DIRECTORY_MODE，运行来源另配
  --extend-from <交付目录>       基于已校验交付增量构建子系统
  --reuse-host-deps-from <目录>  在 macOS 上复用已校验的 Ubuntu 宿主依赖包
  --skip-host-deps                 不交付宿主依赖包（仅限目标机已安装依赖）
  --preflight-only                 检查正式构建输入、工具、Docker、空间和宿主依赖来源
  --candidate                     显式制作未签候选；不保证满足正式发布条件
  --no-pause                      完成后不等待按回车
  --license-file <commercial-license.jws>
                                  将厂商签发的客户许可证打进介质（内置厂商公钥
                                   验签；供安装场景导入/激活，须满足部署及当前 ACK 门禁）
  --runtime-approval-dir <目录>  运行组件批准清单目录（license-evidence.json、
                                   各系统 runtime-license-*.json 与 approval-review.json）
  --runtime-approval-approved     标记批准清单已经厂商审核
  --prepare-crm-runtime-approvals <旧安装JSON>
                                  只准备仅CRM旧安装的隔离批准候选，不构建/部署；
                                  同时指定 --crm-image、--operator、--output
  --crm-image <本机镜像引用>       候选准备使用的新版CRM镜像，检查后固定为实际ID
  --prepare-expand-runtime-approvals <现有安装JSON>
                                  准备新增系统的隔离批准候选，不构建/部署；需要
                                  --expand-applications、--image-mapping、--operator、--output
  --expand-applications <应用编码列表>
                                  明确新增应用，如 contract_management,project_management,data_analysis
  --prepare-upgrade-runtime-approvals <现有安装JSON>
                                  只准备已授权系统的软件升级候选，不构建/部署；
                                  需 --upgrade-applications、--image-mapping、--operator、--output
  --upgrade-applications <应用编码列表>
                                  明确完整已有业务范围；映射必须指定各应用新发布代次
  --image-mapping <目标映射JSON>   完整现有业务与新增业务的真实不可变镜像映射；
                                  六份生产 profile 用于边界检查，不要求购买六个系统
  --target-image-store <存储类型>  候选目标的镜像身份：native（默认）或 containerd
  --image-package <镜像包>        containerd 候选的原始组件包，可重复；严格验证
                                  OCI身份链与本机实际config/descriptor，不能使用手写摘要别名
  --operator <核对人标识>          候选准备人员，如 yglf；生成后仍须审核
  --review-from <旧冻结目录>       正式打包后打印新旧交付审核差异；需 --operator
  --release-integrity-spec <JSON>  发布清单 spec；启用构建证明、manifest 和签名 finalize
  --release-build-key <私钥>       构建角色私钥（0600，DSSE）
  --release-signing-key <私钥>     发布角色私钥（0600，manifest）
  --release-trust-dir <目录>       已安装的发布信任目录（不得来自介质）
  --release-verifier <可执行文件>  已安装的 release-verifier 绝对路径
  --release-profiles-dir <目录>    六份运行 Profile；默认 deploy/production/subsystems.d
  --customer-name <公司>           生成 <公司>/镜像、<公司>/公钥、<公司>/交付清单
  --customer-public-keys <目录>    公钥目录；默认 Key/公钥（不能含私钥）
  --customer-verifier <文件>       客户目录中的 Linux verifier；默认 outs/release-verifier-linux-amd64
  --reuse-approved-crm-image       正式打包复用已批准的本机CRM镜像，不重新构建它
  --installation-scenario <场景>  安装场景：fresh|migrate|platform-only|expand
                                   （全新/旧系统迁移+授权/仅平台/增量子系统+授权）
  --customer-id <客户标识>        安装客户标识；提供许可证时必须与其签名客户一致
  --legacy-license-bridge <客户标识>
                                  旧版无授权：只打兼容基础平台包，保留旧业务镜像；
                                  无需 JWS/批准文件，客户标识仅用于后续初始化提示
  --legacy-license-upgrade <客户标识>
                                  旧版无授权：完整重建存量迁移包（migrate）；
                                  必须显式指定业务组件及已审核的旧/新运行批准，
                                  JWS 可稍后签发，不自动授予新业务使用资格
  -h, --help                      显示帮助

产物：
  .artifacts/offline/<版本>/
  .artifacts/offline/uip-offline-delivery-<版本>-linux-amd64.tar
  .artifacts/offline/uip-offline-delivery-<版本>-linux-amd64.tar.sha256

组件规则：
  common、platform、frontend 和 deployment-assets 是基础平台/基础设施，始终打包。
  可选业务系统：customer-opportunity, customer-portal, contract, project,
                  settlement, data-analysis
  --components all     打包全部业务系统（默认）
  --components none    只打包基础平台与基础设施
  --extend-from        复用基线中的 common/platform/frontend/assets，
                       只重新构建 --components 指定的业务镜像。
                       基线必须包含兼容性锁，且当前 Platform/前端/共享
                       部署输入与基线完全一致；否则必须完整重建。
  增量模式只适用于业务镜像与原平台、前端和部署资产兼容的情况；
  如果 Compose、清单/模板、平台 API 或前端也发生变化，必须制作新的完整基线。

示例：
  ./一键制作离线镜像包.sh --components contract,project
  ./一键制作离线镜像包.sh --version add-crm-v1 \
    --extend-from .artifacts/offline/base-v1 --components customer-opportunity
  ./一键制作离线镜像包.sh --legacy-license-bridge gays-sq --skip-host-deps --no-pause
  ./一键制作离线镜像包.sh --legacy-license-upgrade gays-sq \
    --components customer-opportunity --runtime-approval-dir /实际已审核批准目录 \
    --runtime-approval-approved --skip-host-deps --no-pause

旧版迁移顺序：兼容平台 → 导出真实实例申请 → 审核旧/新批准 → 冻结旧运行范围
              → 受控更新业务 → 迁移确认 → 如有有效 JWS，激活并等待全部当前 ACK。
打包不连接服务器、不冻结基线、不签发许可证；服务器 --upgrade 不自动执行迁移冻结。
EOF
}

die() { printf '[错误] %s\n' "$*" >&2; exit 1; }
info() { printf '[信息] %s\n' "$*"; }
need() { command -v "$1" >/dev/null 2>&1 || die "缺少命令：$1"; }

pause_on_exit() {
  local status="$?"
  if [[ "$no_pause" != true && -t 0 ]]; then
    if ((status == 0)); then
      printf '\n制作完成。'
    else
      printf '\n制作失败，请根据上方错误修复后重试。'
    fi
    read -r -p '按回车键结束……' _ || true
  fi
  return "$status"
}
trap pause_on_exit EXIT

while (($#)); do
  case "$1" in
    --legacy-build) shift ;;
    --version) version="${2:?--version 缺少参数}"; shift 2 ;;
    --output) output_dir="${2:?--output 缺少参数}"; shift 2 ;;
    --portal-customer-directory-mode) portal_directory_mode="${2:?缺少Portal发行来源}"; shift 2 ;;
    --components) component_selection="${2:?--components 缺少参数}"; components_explicit=true; shift 2 ;;
    --extend-from) extend_from="${2:?--extend-from 缺少参数}"; shift 2 ;;
    --reuse-host-deps-from) reuse_host_deps_from="${2:?--reuse-host-deps-from 缺少参数}"; shift 2 ;;
    --skip-host-deps) skip_host_deps=true; shift ;;
    --preflight-only) preflight_only=true; shift ;;
    --candidate) candidate=true; shift ;;
    --no-pause) no_pause=true; shift ;;
    --license-file) license_file="${2:?--license-file 缺少参数}"; shift 2 ;;
    --runtime-approval-dir) runtime_approval_dir="${2:?--runtime-approval-dir 缺少参数}"; shift 2 ;;
    --runtime-approval-approved) runtime_approval_approved=true; shift ;;
    --prepare-crm-runtime-approvals) prepare_crm_inventory="${2:?缺少旧安装JSON}"; shift 2 ;;
    --crm-image) prepare_crm_image="${2:?缺少CRM镜像引用}"; shift 2 ;;
    --prepare-expand-runtime-approvals) prepare_expand_inventory="${2:?缺少现有安装JSON}"; shift 2 ;;
    --prepare-upgrade-runtime-approvals) prepare_upgrade_inventory="${2:?缺少已有安装JSON}"; shift 2 ;;
    --upgrade-applications) upgrade_applications="${2:?缺少升级应用列表}"; shift 2 ;;
    --expand-applications) expand_applications="${2:?缺少新增应用编码列表}"; shift 2 ;;
    --image-mapping) expand_image_mapping="${2:?缺少目标镜像映射JSON}"; shift 2 ;;
    --target-image-store) expand_target_store="${2:?缺少目标镜像存储类型}"; expand_store_explicit=true; shift 2 ;;
    --image-package) expand_image_packages+=("${2:?缺少镜像包路径}"); shift 2 ;;
    --operator) approval_operator="${2:?缺少核对人标识}"; shift 2 ;;
    --review-from) review_from="${2:?--review-from 缺少参数}"; shift 2 ;;
    --release-integrity-spec) release_spec="${2:?缺少 release spec}"; shift 2 ;;
    --release-build-key) release_build_key="${2:?缺少构建签名私钥}"; shift 2 ;;
    --release-signing-key) release_release_key="${2:?缺少发布签名私钥}"; shift 2 ;;
    --release-trust-dir) release_trust_dir="${2:?缺少发布信任目录}"; shift 2 ;;
    --release-verifier) release_verifier="${2:?缺少 release-verifier}"; shift 2 ;;
    --release-profiles-dir) release_profiles_dir="${2:?缺少发布 Profile 目录}"; shift 2 ;;
    --customer-name) customer_name="${2:?缺少客户名称}"; shift 2 ;;
    --customer-public-keys) customer_public_keys="${2:?缺少公钥目录}"; shift 2 ;;
    --customer-verifier) customer_verifier="${2:?缺少 verifier}"; shift 2 ;;
    --reuse-approved-crm-image) reuse_approved_crm_image=true; shift ;;
    --installation-scenario) installation_scenario="${2:?--installation-scenario 缺少参数}"; shift 2 ;;
    --customer-id) installation_customer="${2:?--customer-id 缺少参数}"; shift 2 ;;
    --legacy-license-bridge|--legacy-license-upgrade)
      [[ -z "$legacy_license_mode" ]] || die '旧版授权便捷入口只能指定一次，bridge 与 upgrade 不能混用'
      legacy_license_mode="${1#--legacy-license-}"
      legacy_license_customer="${2:?旧版授权便捷入口缺少客户标识}"
      shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) usage >&2; die "未知参数：$1" ;;
  esac
done

case "$portal_directory_mode" in crm|local) ;; *) die 'Portal发行来源必须是 crm 或 local' ;; esac

if [[ -n "$release_spec" ]]; then
  [[ -f "$release_spec" && ! -L "$release_spec" ]] || die 'release spec 缺失或不安全'
  if [[ -n "$release_build_key$release_release_key$release_trust_dir$release_verifier" ]]; then
    [[ -n "$release_build_key" && -n "$release_release_key" && -n "$release_trust_dir" && -n "$release_verifier" ]] ||
      die 'release finalize 必须同时指定 spec、构建私钥、发布私钥、trust dir 和 verifier'
    [[ -f "$release_build_key" && ! -L "$release_build_key" ]] || die '构建签名私钥缺失或不安全'
    [[ -f "$release_release_key" && ! -L "$release_release_key" ]] || die '发布签名私钥缺失或不安全'
    [[ -d "$release_trust_dir" && ! -L "$release_trust_dir" ]] || die '发布信任目录缺失或不安全'
    [[ -f "$release_verifier" && -x "$release_verifier" && ! -L "$release_verifier" ]] || die 'release-verifier 缺失或不可执行'
    [[ -z "$release_profiles_dir" ]] || [[ -d "$release_profiles_dir" && ! -L "$release_profiles_dir" ]] ||
      die 'release profiles 目录缺失或不安全'
    [[ "$preflight_only" != true ]] || die 'preflight-only 不能与 release finalize 同时使用'
    [[ -z "$prepare_crm_inventory$prepare_expand_inventory$prepare_upgrade_inventory" ]] ||
      die '候选准备模式不能与 release finalize 同时使用'
  fi
fi

if [[ -n "$customer_name" ]]; then
  [[ -n "$release_spec" ]] || die '客户交付目录必须同时启用 --release-integrity-spec'
  [[ -n "$customer_public_keys" ]] || customer_public_keys="$script_dir/Key/公钥"
  [[ -n "$customer_verifier" ]] || customer_verifier="$script_dir/outs/release-verifier-linux-amd64"
  [[ "$customer_name" != "." && "$customer_name" != ".." && "$customer_name" != */* && "$customer_name" != *\\* ]] ||
    die '客户名称必须是单一路径分量'
  [[ -d "$customer_public_keys" && ! -L "$customer_public_keys" ]] || die '客户公钥目录无效'
  for public_file in trust-policy.json build.pub release.pub; do
    [[ -f "$customer_public_keys/$public_file" && ! -L "$customer_public_keys/$public_file" ]] ||
      die "客户公钥目录缺少：$public_file"
  done
  if grep -RIl --include='*.key' --include='*.pem' --include='trust-policy.json' 'PRIVATE KEY' "$customer_public_keys" >/dev/null 2>&1; then
    die '客户公钥目录包含私钥，拒绝交付：请使用 Key/公钥'
  fi
  if [[ -n "$customer_verifier" ]]; then
    [[ -f "$customer_verifier" && ! -L "$customer_verifier" ]] || die '客户 verifier 无效'
  fi
fi

if [[ -n "$prepare_upgrade_inventory" ]]; then
  [[ -n "$upgrade_applications" && -n "$expand_image_mapping" && -n "$approval_operator" && -n "$output_dir" ]] ||
    die '升级候选需要 --upgrade-applications、--image-mapping、--operator 和新的 --output'
  [[ -z "$prepare_expand_inventory$expand_applications$prepare_crm_inventory$prepare_crm_image$version$component_selection$extend_from$license_file$runtime_approval_dir$installation_scenario$installation_customer$legacy_license_mode$reuse_host_deps_from" &&
      "$skip_host_deps" == false && "$preflight_only" == false && "$runtime_approval_approved" == false && "$reuse_approved_crm_image" == false ]] ||
    die '升级候选不能混用构建、新装、迁移、扩容或声明已批准'
  need go
  need docker
  for upgrade_input in "$prepare_upgrade_inventory" "$expand_image_mapping"; do
    [[ -f "$upgrade_input" && ! -L "$upgrade_input" ]] || die '升级库存与目标映射必须是普通文件'
  done
  prepare_upgrade_inventory="$(cd -- "$(dirname -- "$prepare_upgrade_inventory")" && pwd)/$(basename -- "$prepare_upgrade_inventory")"
  expand_image_mapping="$(cd -- "$(dirname -- "$expand_image_mapping")" && pwd)/$(basename -- "$expand_image_mapping")"
  [[ -d "$(dirname -- "$output_dir")" ]] || die '输出父目录必须已存在'
  output_dir="$(cd -- "$(dirname -- "$output_dir")" && pwd)/$(basename -- "$output_dir")"
  case "$expand_target_store" in
    native) ((${#expand_image_packages[@]} == 0)) || die 'native 候选不接受镜像归档参数' ;;
    containerd) ((${#expand_image_packages[@]} > 0)) || die 'containerd 升级候选需要原始镜像包' ;;
    *) die '目标镜像存储仅支持 native/containerd' ;;
  esac
  upgrade_cli_args=(-workspace "$script_dir" -upgrade-inventory "$prepare_upgrade_inventory"
    -upgrade-applications "$upgrade_applications" -mapping "$expand_image_mapping"
    -candidate-only -operator "$approval_operator" -output "$output_dir"
    -target-image-store "$expand_target_store")
  for upgrade_package in ${expand_image_packages[@]+"${expand_image_packages[@]}"}; do
    [[ -f "$upgrade_package" && ! -L "$upgrade_package" ]] || die '镜像包必须是普通文件'
    upgrade_package="$(cd -- "$(dirname -- "$upgrade_package")" && pwd)/$(basename -- "$upgrade_package")"
    upgrade_cli_args+=(-image-package "$upgrade_package")
  done
  (cd "$script_dir/commercial-license-tool" && GOWORK=off go run ./cmd/runtime-approval "${upgrade_cli_args[@]}")
  info "升级候选已生成：${output_dir}；尚未批准、安装或变更许可证"
  exit 0
fi
[[ -z "$upgrade_applications" ]] || die '--upgrade-applications 仅用于升级候选'

if [[ -n "$prepare_expand_inventory" ]]; then
  [[ -n "$expand_applications" && -n "$expand_image_mapping" && -n "$approval_operator" && -n "$output_dir" ]] ||
    die '准备新增系统候选需要 --expand-applications、--image-mapping、--operator 和新的 --output 目录'
  [[ -z "$prepare_crm_inventory$prepare_crm_image$version$component_selection$extend_from$license_file$runtime_approval_dir$installation_scenario$installation_customer$legacy_license_mode$reuse_host_deps_from" &&
      "$skip_host_deps" == false && "$preflight_only" == false && "$runtime_approval_approved" == false && "$reuse_approved_crm_image" == false ]] ||
    die '新增系统候选准备不能同时构建、准备CRM迁移、打包许可证或声明已批准'
  need go
  need docker
  for expand_input in "$prepare_expand_inventory" "$expand_image_mapping"; do
    [[ -f "$expand_input" && ! -L "$expand_input" ]] || die '现有安装和目标镜像映射必须是普通文件'
  done
  prepare_expand_inventory="$(cd -- "$(dirname -- "$prepare_expand_inventory")" && pwd)/$(basename -- "$prepare_expand_inventory")"
  expand_image_mapping="$(cd -- "$(dirname -- "$expand_image_mapping")" && pwd)/$(basename -- "$expand_image_mapping")"
  [[ -d "$(dirname -- "$output_dir")" ]] || die '候选输出的父目录必须已存在'
  output_dir="$(cd -- "$(dirname -- "$output_dir")" && pwd)/$(basename -- "$output_dir")"
  case "$expand_target_store" in
    native) ((${#expand_image_packages[@]} == 0)) || die 'native 候选不接受 --image-package' ;;
    containerd) ((${#expand_image_packages[@]} > 0)) || die 'containerd 候选需要实际 --image-package 证明' ;;
    *) die '--target-image-store 只允许 native 或 containerd' ;;
  esac
  expand_cli_args=(-workspace "$script_dir" -expand-inventory "$prepare_expand_inventory"
    -add-applications "$expand_applications" -mapping "$expand_image_mapping"
    -candidate-only -operator "$approval_operator" -output "$output_dir"
    -target-image-store "$expand_target_store")
  for expand_package in ${expand_image_packages[@]+"${expand_image_packages[@]}"}; do
    [[ -f "$expand_package" && ! -L "$expand_package" ]] || die '--image-package 必须是普通文件'
    expand_package="$(cd -- "$(dirname -- "$expand_package")" && pwd)/$(basename -- "$expand_package")"
    expand_cli_args+=(-image-package "$expand_package")
  done
  (cd "$script_dir/commercial-license-tool" && GOWORK=off go run ./cmd/runtime-approval "${expand_cli_args[@]}")
  info "新增系统候选已生成：${output_dir}；须复核完整安装边界及最终镜像，不代表已批准或激活"
  exit 0
fi
[[ -z "$expand_applications$expand_image_mapping" ]] || die '--expand-applications/--image-mapping 仅用于 --prepare-expand-runtime-approvals'
[[ "$expand_store_explicit" == false && ${#expand_image_packages[@]} == 0 ]] || die '--target-image-store/--image-package 仅用于扩容或升级候选'

if [[ -n "$prepare_crm_inventory" ]]; then
  [[ -n "$prepare_crm_image" && -n "$approval_operator" && -n "$output_dir" ]] || die '准备CRM候选需要 --crm-image、--operator 和新的 --output 目录'
  [[ -z "$version$component_selection$extend_from$license_file$runtime_approval_dir$installation_scenario$installation_customer$legacy_license_mode$reuse_host_deps_from" &&
      "$skip_host_deps" == false && "$preflight_only" == false && "$runtime_approval_approved" == false && "$reuse_approved_crm_image" == false ]] ||
    die '候选准备不能同时构建、复用批准、打包许可证或声明已批准'
  need go
  need docker
  need jq
  [[ -f "$prepare_crm_inventory" && ! -L "$prepare_crm_inventory" ]] || die '旧安装JSON必须是普通文件'
  prepare_crm_inventory="$(cd -- "$(dirname -- "$prepare_crm_inventory")" && pwd)/$(basename -- "$prepare_crm_inventory")"
  [[ -d "$(dirname -- "$output_dir")" ]] || die '候选输出的父目录必须已存在'
  output_dir="$(cd -- "$(dirname -- "$output_dir")" && pwd)/$(basename -- "$output_dir")"
  [[ "$prepare_crm_image" != -* && "$prepare_crm_image" != *$'\n'* && "$prepare_crm_image" != *$'\r'* ]] || die 'CRM镜像引用无效'
  crm_candidate_id="$(docker image inspect "$prepare_crm_image" --format '{{.Id}}')" || die '本机找不到新版CRM镜像，请先核对并导入镜像包'
  [[ "$crm_candidate_id" =~ ^sha256:[a-f0-9]{64}$ ]] || die '新版CRM镜像必须有真实配置ID'
  info "候选将绑定新版CRM镜像：${crm_candidate_id}"
  (cd "$script_dir/commercial-license-tool" && GOWORK=off go run ./cmd/runtime-approval \
    -workspace "$script_dir" -legacy-crm-inventory "$prepare_crm_inventory" \
    -crm-image "$crm_candidate_id" -candidate-only -operator "$approval_operator" -output "$output_dir")
  jq -r '"旧安装项目：" + .project, (.services[] | "旧组件：" + .name + " | 镜像=" + .image_digest + " | 副本=" + (.replicas|tostring))' \
    "$output_dir/license-migration-evidence.json"
  jq -r '"目标项目：" + .project, (.services[] | "新组件：" + .name + " | 镜像=" + .image_digest + " | 版本=" + .version + " | 协议=" + .protocol + " | 副本=" + (.replicas|tostring))' \
    "$output_dir/license-evidence.json"
  info "候选已生成：${output_dir}；请核对旧/新镜像、八组件和完整安装范围后再正式打包"
  exit 0
fi
[[ -z "$prepare_crm_image$approval_operator" ]] || die '--crm-image/--operator 仅用于 --prepare-crm-runtime-approvals'
if [[ "$reuse_approved_crm_image" == true ]]; then
  [[ -n "$runtime_approval_dir" && "$runtime_approval_approved" == true ]] || die '复用CRM镜像需要已审核的 --runtime-approval-dir 和 --runtime-approval-approved'
fi

# 兼容包不能冻结空业务基线；完整迁移则沿用底层真实采证和授权门禁。
if [[ -n "$legacy_license_mode" ]]; then
  [[ "$legacy_license_customer" =~ ^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$ ]] || die '旧版授权便捷入口需要有效客户标识'
  [[ -z "$extend_from" ]] || die '旧版授权便捷入口需要重建基础平台，不支持 --extend-from'
  [[ -z "$installation_scenario" || ( "$legacy_license_mode" == upgrade && "$installation_scenario" == migrate ) ]] ||
    die '旧版授权便捷入口与 --installation-scenario 冲突'
  [[ -z "$installation_customer" || "$installation_customer" == "$legacy_license_customer" ]] || die '旧版授权便捷入口与 --customer-id 客户不一致'
  if [[ "$legacy_license_mode" == bridge ]]; then
    [[ -z "$license_file" && -z "$runtime_approval_dir" && "$runtime_approval_approved" != true && -z "$installation_customer" ]] ||
      die 'bridge 只制作兼容基础平台包；许可证/批准/安装客户参数请使用 --legacy-license-upgrade'
    [[ "$components_explicit" != true || "$component_selection" == none ]] || die 'bridge 只接受 --components none，旧业务必须保留到冻结后升级'
    component_selection=none
    components_explicit=true
    info "旧版无授权兼容包：客户 ${legacy_license_customer}；只重建基础平台，后续初始化时核对真实实例客户"
  else
    [[ "$components_explicit" == true ]] || die '旧版授权迁移必须用 --components 显式指定实际升级的业务系统'
    [[ -n "$runtime_approval_dir" && "$runtime_approval_approved" == true ]] ||
      die '旧版授权迁移需要 --runtime-approval-dir 和 --runtime-approval-approved；暂无批准请先用 --legacy-license-bridge'
    installation_scenario=migrate
    installation_customer="$legacy_license_customer"
    info "旧版无授权迁移包：客户 ${installation_customer}；JWS 可稍后签发，部署前必须冻结实际旧运行范围"
  fi
fi

# 商业授权参数校验：组合规则与底层打包器保持一致，提前在封装层给出清晰报错。
license_enabled=false
if [[ -n "$license_file" || -n "$runtime_approval_dir" || -n "$installation_scenario" || -n "$installation_customer" ]]; then
  license_enabled=true
  [[ -f "$builder" ]] || die "离线构建器不存在：${builder}"
  if [[ -n "$license_file" && ! -f "$license_file" ]]; then
    die "许可证文件不存在：${license_file}"
  fi
  case "$installation_scenario" in
    ''|fresh|migrate|platform-only|expand) ;;
    *) die "安装场景无效：${installation_scenario}（可选 fresh|migrate|platform-only|expand）" ;;
  esac
  [[ -n "$installation_scenario" || -z "$installation_customer" ]] ||
    die "--customer-id 必须与 --installation-scenario 同时提供"
  builtin_v2_only=false
  if [[ -n "$license_file" && -z "$runtime_approval_dir" && -n "$release_spec" ]]; then
    if python3 "$deploy_dir/bin/release-materials.py" --v2-only \
        --spec "$release_spec" --format json >/dev/null 2>&1; then
      builtin_v2_only=true
    fi
  fi
  [[ -z "$license_file" || -n "$runtime_approval_dir" || "$builtin_v2_only" == true ]] ||
    die "--license-file 需要 --runtime-approval-dir，或提供全部应用为 V2 的 release spec"
  if [[ "$runtime_approval_approved" == true || -n "$runtime_approval_dir" ]]; then
    [[ -n "$runtime_approval_dir" && "$runtime_approval_approved" == true ]] ||
      die "--runtime-approval-dir 必须与 --runtime-approval-approved 同时提供"
    [[ -d "$runtime_approval_dir" ]] || die "运行批准目录不存在：${runtime_approval_dir}"
  fi
  command -v go >/dev/null || die "携带许可证需要 Go（用于平台内置公钥验签）"
  command -v jq >/dev/null || die "携带许可证需要 jq"
fi

[[ -f "$builder" && ! -L "$builder" ]] || die "离线构建器不存在：${builder}"
[[ -f "$server_installer" && ! -L "$server_installer" ]] || die "服务器部署入口不存在：${server_installer}"
[[ -f "$operations_tool" && ! -L "$operations_tool" ]] || die "系统运维入口不存在：${operations_tool}"
[[ -f "$fresh_install_reset_tool" && ! -L "$fresh_install_reset_tool" ]] || die "失败首装清理入口不存在：${fresh_install_reset_tool}"
[[ -f "$fresh_install_reset_guide" && ! -L "$fresh_install_reset_guide" ]] || die "失败首装清理说明不存在：${fresh_install_reset_guide}"

default_version="$(date -u +%Y%m%dT%H%M%SZ)-offline"
if [[ -z "$version" ]]; then
  if [[ -t 0 ]]; then
    read -r -p "发布版本 [${default_version}]：" version || true
  fi
  version="${version:-$default_version}"
fi
[[ "$version" =~ ^[A-Za-z0-9._-]+$ && "$version" != . && "$version" != .. ]] ||
  die '版本号只允许字母、数字、点、下划线和连字符'

optional_components='customer-opportunity customer-portal contract project settlement data-analysis'
if [[ "$components_explicit" != true && -t 0 ]]; then
  printf '\n基础平台必选：common + platform + frontend + deployment-assets\n'
  printf '可选业务系统：%s\n' "$optional_components"
  read -r -p '请输入要打包的业务系统 [all]：' component_selection || true
fi
component_selection="${component_selection:-all}"

# normalize_components 按显式发行来源归一化系统组合，默认保留Portal依赖CRM。
# 参数：raw为用户选择的业务组件。
# 返回值：固定顺序组件；未知选择失败，不更改任何运行来源。
normalize_components() {
  local raw="$1" token candidate requested=" " selected=""
  raw="${raw//,/ }"
  raw="$(printf '%s\n' "$raw" | awk '{$1=$1; print}')"
  raw="${raw:-all}"
  for token in $raw; do
    case "$token" in
      all)
        [[ "$raw" == all ]] || die '--components all 不能与其他组件混用'
        requested=" $optional_components "
        break
        ;;
      none)
        [[ "$raw" == none ]] || die '--components none 不能与其他组件混用'
        requested=" "
        break
        ;;
      common|platform|frontend)
        # 基础组件始终包含，显式写入时不重复。
        ;;
      customer-opportunity|customer-portal|contract|project|settlement|data-analysis)
        case "$requested" in *" $token "*) ;; *) requested="${requested}${token} " ;; esac
        ;;
      *) die "不支持的打包组件：${token}" ;;
    esac
  done
  if [[ "${portal_directory_mode:-crm}" == crm && "$requested" == *' customer-portal '* && "$requested" != *' customer-opportunity '* ]]; then
    info '客户自助门户依赖客户与商机系统，已自动加入 customer-opportunity' >&2
    requested="${requested}customer-opportunity "
  fi
  for candidate in $optional_components; do
    [[ "$requested" == *" $candidate "* ]] && selected="${selected}${selected:+ }${candidate}"
  done
  printf '%s\n' "$selected"
}

selected_optional_components="$(normalize_components "$component_selection")"
info "Portal发行客户来源：${portal_directory_mode}（不自动配置运行来源）"
if [[ "$portal_directory_mode" == local && " $selected_optional_components " == *' customer-portal '* ]]; then
  info '首次独立安装必须在 runtime/portal.env 明确选择 PORTAL_CUSTOMER_DIRECTORY_MODE=local，并通过Portal authority锁和严格运行依赖检查。'
fi
[[ "$reuse_approved_crm_image" != true || " $selected_optional_components " == *' customer-opportunity '* ]] || die '复用CRM镜像必须选择 customer-opportunity'
if [[ "$legacy_license_mode" == upgrade ]]; then
  [[ -n "$selected_optional_components" ]] || die '旧版授权迁移至少需要一个业务子系统；只更新兼容平台请使用 --legacy-license-bridge'
  [[ ! -L "$runtime_approval_dir" ]] || die '旧版授权迁移批准目录不能是符号链接'
  legacy_required_approvals=(license-evidence.json license-migration-evidence.json)
  for legacy_component in $selected_optional_components; do
    case "$legacy_component" in
      customer-opportunity) legacy_app=customer_and_opportunity ;;
      customer-portal) legacy_app=customer_portal ;;
      contract) legacy_app=contract_management ;;
      project) legacy_app=project_management ;;
      settlement) legacy_app=settlement ;;
      data-analysis) legacy_app=data_analysis ;;
    esac
    legacy_required_approvals+=("runtime-license-${legacy_app}-prod.json")
  done
  for legacy_approval in "${legacy_required_approvals[@]}"; do
    legacy_approval_path="$runtime_approval_dir/$legacy_approval"
    [[ -f "$legacy_approval_path" && ! -L "$legacy_approval_path" ]] || die "旧版授权迁移缺少安全批准文件：${legacy_approval}"
    [[ "$(wc -c < "$legacy_approval_path" | tr -d ' ')" -le 2097152 ]] || die "运行批准文件超过 2MiB：${legacy_approval}"
    jq -e -s 'length == 1 and (.[0] | type == "object")' "$legacy_approval_path" >/dev/null || die "批准文件必须是单个 JSON 对象：${legacy_approval}"
    if [[ "$legacy_approval" == runtime-license-* ]]; then
      legacy_app="${legacy_approval#runtime-license-}"
      legacy_app="${legacy_app%-prod.json}"
      jq -e --arg app "$legacy_app" '.application == $app and .environment == "prod"' "$legacy_approval_path" >/dev/null ||
        die "运行批准的系统或环境与固定文件名不符：${legacy_approval}"
    fi
  done
fi
if [[ -n "$extend_from" ]]; then
  [[ "$components_explicit" == true ]] || die '--extend-from 必须同时显式指定 --components'
  [[ -n "$selected_optional_components" ]] || die '--extend-from 至少需要一个业务子系统，不接受 --components none'
  components="$selected_optional_components"
  info '增量制作模式：必选基础组件从已校验基线复用，不重新构建'
else
  components="common platform frontend${selected_optional_components:+ ${selected_optional_components}}"
  info "必选基础组件：common platform frontend assets"
fi
info "可选业务组件：${selected_optional_components:-无}"

output_dir="${output_dir:-${script_dir}/.artifacts/offline/${version}}"
case "$output_dir" in /*) ;; *) output_dir="${script_dir}/${output_dir}" ;; esac
[[ ! -e "$output_dir" && ! -L "$output_dir" ]] || die "输出目录已存在，请换一个新版本：${output_dir}"

host_os="$(uname -s)"
host_arch="$(uname -m)"
if [[ "$host_os" == Darwin ]]; then
  # Docker Desktop 卸载后 desktop-linux context 会消失（例如改用 OrbStack）。
  # 仅在显式未指定且默认 context 确实不存在时回退到当前 context，
  # 避免写死的默认值把构建直接挡在第 399 行的预检上。
  default_docker_context="desktop-linux"
  if [[ -z "${UIP_DOCKER_CONTEXT:-}" ]] && ! docker context inspect "$default_docker_context" >/dev/null 2>&1; then
    default_docker_context="$(docker context show 2>/dev/null || true)"
    [[ -n "$default_docker_context" ]] || die '未找到可用的 Docker Context；请先启动 Docker Engine'
    info "默认 context desktop-linux 不存在，回退到当前 context：${default_docker_context}"
  fi
  export DOCKER_CONTEXT="${UIP_DOCKER_CONTEXT:-$default_docker_context}"
  export BUILDX_BUILDER="${UIP_BUILDX_BUILDER:-$DOCKER_CONTEXT}"
elif [[ "$host_os" == Linux ]]; then
  info "Linux 构建主机：${host_arch}"
else
  die "不支持的构建操作系统：${host_os}"
fi

for command_name in docker jq tar gzip sha256sum mktemp awk sed grep find sort install; do need "$command_name"; done
if [[ "$host_os" == Darwin ]]; then
  docker context inspect "$DOCKER_CONTEXT" >/dev/null 2>&1 || die "Docker Context 不存在：${DOCKER_CONTEXT}"
  docker buildx inspect "$BUILDX_BUILDER" >/dev/null 2>&1 || die "Buildx Builder 不存在：${BUILDX_BUILDER}"
  info "macOS 交叉构建：Docker Context=${DOCKER_CONTEXT}，Builder=${BUILDX_BUILDER}"
fi
docker info >/dev/null 2>&1 || die 'Docker Engine 不可用，请先启动 Docker Desktop/Engine'
docker buildx version >/dev/null 2>&1 || die 'Docker Buildx 不可用'

# Formal checks precede every build/save and are also part of --preflight-only.
# Candidate mode is explicit and cannot be combined with inline signing.
if [[ "$candidate" == true ]]; then
  [[ -z "$release_build_key$release_release_key" ]] || die '--candidate 不能与正式签名同时使用'
  info '未签候选模式：仍保留构建后来源、许可证与完整性检查'
else
  need python3
  if [[ -z "${UIP_BUILD_INVOCATION_ID:-}" ]]; then
    UIP_BUILD_INVOCATION_ID="$(python3 -B -c 'import uuid; print("cli-" + str(uuid.uuid4()))')"
    export UIP_BUILD_INVOCATION_ID
  fi
  source "$deploy_dir/bin/offline-build-inputs.sh"
  formal_args=()
  if [[ -z "$extend_from" ]]; then
    while IFS= read -r common_image; do
      # The managed proxy has its own immutable upstream/identity gate in the builder.
      [[ "$common_image" == tecnativa/docker-socket-proxy:v0.5.0 ]] && continue
      formal_args+=(--common-image "$common_image")
    done < <(offline_common_base_images)
  fi
  python3 -B "$deploy_dir/bin/release-preflight.py" --workspace "$script_dir" \
    --components "platform frontend $components" --check-identity ${formal_args[@]+"${formal_args[@]}"} ||
    die '正式发布输入预检未通过；尚未构建镜像。请修正上述输入后重试'
fi

# 会话治理副本防漂移：工作区级校验；工作区根不是 git 仓库，无法接入任一单仓 CI，
# 因此在打包预检阶段强制 --check，差异必须先 --sync 或修正后才能打包。
bash "$script_dir/scripts/check-session-governance-sync.sh" --check ||
  die '会话治理副本不一致：先运行 bash scripts/check-session-governance-sync.sh --sync 或修正差异'

verify_bound_sidecar() {
  local file="$1" sidecar="${1}.sha256" expected actual name
  [[ -f "$file" && ! -L "$file" ]] || die "基线交付文件缺失或不安全：${file}"
  [[ -f "$sidecar" && ! -L "$sidecar" ]] || die "基线交付缺少 sidecar：${sidecar}"
  name="$(basename -- "$file")"
  expected="$(awk -v name="$name" '
    NF != 2 || NR != 1 || length($1) != 64 || tolower($1) ~ /[^0-9a-f]/ || $2 != name {bad=1}
    {digest=tolower($1)}
    END {if (bad || NR != 1) exit 1; print digest}
  ' "$sidecar")" || die "基线 sidecar 必须只绑定当前文件：${sidecar}"
  actual="$(sha256sum "$file" | awk '{print tolower($1)}')"
  [[ "$actual" == "$expected" ]] || die "基线交付文件摘要不一致：${file}"
}

# Platform API、subsystem-provisioner、File Gateway、统一前端和生产部署资产
# 共同构成一个兼容单元。增量业务镜像只能在这些输入逐字节未变时
# 复用旧基线，避免 API 使用旧清单摘要、Agent 加载新清单的漂移故障。
source "$script_dir/platform/deploy/production/bin/offline-build-inputs.sh"
source "$deploy_dir/bin/offline-frontend-selection.sh"
# platform_assets_input_fingerprint 绑定前端发行选择，避免复用缺少目标模块的基线。
# 参数：无。
# 返回值：共享源码及前端构建参数的 SHA256。
platform_assets_input_fingerprint() {
  local source_digest frontend_selection
  source_digest="$(offline_build_input_fingerprint "$script_dir" shared)" || return 1
  frontend_selection="$(offline_frontend_enabled_modules true "$selected_optional_components" "${portal_directory_mode:-crm}")" || return 1
  offline_bind_build_arguments "$source_digest" "VITE_ENABLED_MODULES=$frontend_selection" "PORTAL_CUSTOMER_DIRECTORY_MODE=${portal_directory_mode:-crm}" \
    "NPM_CONFIG_REGISTRY=${OFFLINE_NPM_CONFIG_REGISTRY:-https://registry.npmjs.org}"
}

compatibility_value() {
  local file="$1" key="$2"
  awk -F= -v key="$key" '
    $1 == key {if (++seen != 1) exit 1; value=substr($0, length(key) + 2)}
    END {if (seen != 1 || value == "") exit 1; print value}
  ' "$file"
}

validate_platform_assets_compatibility() {
  local compatibility_file="$1" common_package="$2" platform_package="$3" frontend_package="$4" asset_package="$5"
  local current_input expected_input expected_name expected_digest actual_digest key package
  [[ -f "$compatibility_file" && ! -L "$compatibility_file" ]] ||
    die '基线缺少 PLATFORM_ASSETS_COMPATIBILITY.env；旧基线无法证明 Platform 与部署资产兼容，请重做完整基线'
  grep -Fqx 'FORMAT=1' "$compatibility_file" || die '基线 Platform/部署资产兼容性锁格式不支持，请重做完整基线'
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
  ' "$compatibility_file" || die '基线 Platform/部署资产兼容性锁非法或不完整'

  expected_input="$(compatibility_value "$compatibility_file" PLATFORM_ASSETS_INPUT_SHA256)" ||
    die '基线兼容性锁缺少共享输入指纹'
  [[ "$expected_input" =~ ^[a-f0-9]{64}$ ]] || die '基线共享输入指纹格式错误'
  current_input="$platform_assets_input_digest"
  if [[ "$current_input" != "$expected_input" ]]; then
    python3 -B "$deploy_dir/bin/release-preflight.py" --workspace "$script_dir" --shared-diff "$baseline_dir" || true
    die '当前 Platform/前端/共享部署输入已与基线不同；禁止复用旧资产，请不带 --extend-from 完整重建新基线'
  fi

  for key in COMMON PLATFORM FRONTEND ASSETS; do
    case "$key" in
      COMMON) package="$common_package" ;;
      PLATFORM) package="$platform_package" ;;
      FRONTEND) package="$frontend_package" ;;
      ASSETS) package="$asset_package" ;;
    esac
    expected_name="$(compatibility_value "$compatibility_file" "${key}_PACKAGE")" ||
      die "基线兼容性锁缺少 ${key}_PACKAGE"
    [[ "$expected_name" == "$(basename -- "$package")" ]] ||
      die "基线兼容性锁与 ${key} 包名不一致"
    expected_digest="$(compatibility_value "$compatibility_file" "${key}_PACKAGE_SHA256")" ||
      die "基线兼容性锁缺少 ${key} 摘要"
    [[ "$expected_digest" =~ ^[a-f0-9]{64}$ ]] || die "基线 ${key} 包摘要格式错误"
    actual_digest="$(sha256sum "$package" | awk '{print tolower($1)}')"
    [[ "$actual_digest" == "$expected_digest" ]] ||
      die "基线兼容性锁与 ${key} 包内容不一致"
  done
}

write_platform_assets_compatibility() {
  local destination="$1" common_package="$2" platform_package="$3" frontend_package="$4" asset_package="$5"
  local temporary
  temporary="$(mktemp "$(dirname -- "$destination")/.platform-assets-compatibility.XXXXXX")"
  {
    printf 'FORMAT=1\n'
    printf 'PLATFORM_ASSETS_INPUT_SHA256=%s\n' "$platform_assets_input_digest"
    printf 'COMMON_PACKAGE=%s\n' "$(basename -- "$common_package")"
    printf 'COMMON_PACKAGE_SHA256=%s\n' "$(sha256sum "$common_package" | awk '{print tolower($1)}')"
    printf 'PLATFORM_PACKAGE=%s\n' "$(basename -- "$platform_package")"
    printf 'PLATFORM_PACKAGE_SHA256=%s\n' "$(sha256sum "$platform_package" | awk '{print tolower($1)}')"
    printf 'FRONTEND_PACKAGE=%s\n' "$(basename -- "$frontend_package")"
    printf 'FRONTEND_PACKAGE_SHA256=%s\n' "$(sha256sum "$frontend_package" | awk '{print tolower($1)}')"
    printf 'ASSETS_PACKAGE=%s\n' "$(basename -- "$asset_package")"
    printf 'ASSETS_PACKAGE_SHA256=%s\n' "$(sha256sum "$asset_package" | awk '{print tolower($1)}')"
  } > "$temporary"
  chmod 644 "$temporary"
  mv -f -- "$temporary" "$destination"
}

validate_extension_baseline() {
  local expected_component="$1" package="$2"
  [[ -f "$package" && ! -L "$package" ]] || die "基线缺少 ${expected_component} 镜像包"
  verify_bound_sidecar "$package"
  tar -xOf "$package" package.env | grep -Fxq "COMPONENT=${expected_component}" ||
    die "基线镜像包组件元数据不正确：${package}"
  tar -xOf "$package" package.env | grep -Fxq 'PLATFORM=linux/amd64' ||
    die "基线镜像包不是 linux/amd64：${package}"
}

validate_extension_asset_compatibility() {
  local asset_package="$1" profile
  case " ${selected_optional_components} " in
    *' data-analysis '*)
      profile="$(tar -xOf "$asset_package" subsystems.d/data-analysis-prod.yaml 2>/dev/null)" ||
        die '基线 deployment-assets 缺少 data-analysis 生产清单；不能制作数据分析增量包，请重做完整基线'
      grep -E '^[[:space:]]*allowed_service_bindings:' <<<"$profile" | grep -Fq 'file_gateway_write' ||
        die '基线 data-analysis 清单未允许 file_gateway_write；不能复用旧资产，请重做完整基线'
      grep -Eq '^[[:space:]]+FILE_GATEWAY_APPLICATION_ID:[[:space:]]+application_id[[:space:]]*$' <<<"$profile" &&
        grep -Eq '^[[:space:]]+FILE_GATEWAY_CLIENT_ID:[[:space:]]+service\.file_gateway_write\.client_id[[:space:]]*$' <<<"$profile" &&
        grep -Eq '^[[:space:]]+FILE_GATEWAY_CLIENT_SECRET:[[:space:]]+service\.file_gateway_write\.client_secret[[:space:]]*$' <<<"$profile" ||
        die '基线 data-analysis 清单缺少 File Gateway 运行时绑定；不能复用旧资产，请重做完整基线'
      ;;
  esac
}

platform_assets_input_digest="$(platform_assets_input_fingerprint)"
[[ "$platform_assets_input_digest" =~ ^[a-f0-9]{64}$ ]] || die '无法计算 Platform/部署资产共享输入指纹'

baseline_dir=""
if [[ -n "$extend_from" ]]; then
  case "$extend_from" in /*) ;; *) extend_from="${script_dir}/${extend_from}" ;; esac
  [[ -d "$extend_from" && ! -L "$extend_from" ]] || die "--extend-from 必须是已解压的安全交付目录：${extend_from}"
  baseline_dir="$(cd -- "$extend_from" && pwd)"
  [[ -f "$baseline_dir/SHA256SUMS" && ! -L "$baseline_dir/SHA256SUMS" ]] || die '基线交付缺少 SHA256SUMS'
  awk '
    NF != 2 || length($1) != 64 || tolower($1) ~ /[^0-9a-f]/ {exit 1}
    $2 !~ /^[A-Za-z0-9._-]+$/ && $2 != "服务器一键离线部署.sh" && $2 != "系统运维工具.sh" && $2 != "清理首次部署产物.sh" && $2 != "license-activate.sh" {exit 1}
    ++seen[$2] != 1 {exit 1}
    END {if (NR == 0) exit 1}
  ' "$baseline_dir/SHA256SUMS" || die '基线 SHA256SUMS 包含非法或重复条目'
  while read -r _ baseline_manifest_name; do
    [[ -f "$baseline_dir/$baseline_manifest_name" && ! -L "$baseline_dir/$baseline_manifest_name" ]] ||
      die "基线 SHA256SUMS 引用了缺失或非普通文件：${baseline_manifest_name}"
  done < "$baseline_dir/SHA256SUMS"
  (cd "$baseline_dir" && sha256sum --check SHA256SUMS)
  shopt -s nullglob
  baseline_common_matches=("$baseline_dir"/common-infrastructure-linux-amd64.tar.gz)
  baseline_platform_matches=("$baseline_dir"/platform-backend-*-linux-amd64.tar.gz)
  baseline_frontend_matches=("$baseline_dir"/frontend-*-linux-amd64.tar.gz)
  baseline_asset_matches=("$baseline_dir"/deployment-assets-*.tar.gz)
  shopt -u nullglob
  ((${#baseline_common_matches[@]} == 1)) || die '基线 common 镜像包必须恰好一个'
  ((${#baseline_platform_matches[@]} == 1)) || die '基线 platform 镜像包必须恰好一个'
  ((${#baseline_frontend_matches[@]} == 1)) || die '基线 frontend 镜像包必须恰好一个'
  ((${#baseline_asset_matches[@]} == 1)) || die '基线 deployment-assets 必须恰好一个'
  validate_extension_baseline common "${baseline_common_matches[0]}"
  validate_extension_baseline platform "${baseline_platform_matches[0]}"
  validate_extension_baseline frontend "${baseline_frontend_matches[0]}"
  verify_bound_sidecar "${baseline_asset_matches[0]}"
  baseline_compatibility="$baseline_dir/PLATFORM_ASSETS_COMPATIBILITY.env"
  [[ "$(awk '$2 == "PLATFORM_ASSETS_COMPATIBILITY.env" {count++} END {print count+0}' "$baseline_dir/SHA256SUMS")" == 1 ]] ||
    die '基线 SHA256SUMS 未唯一绑定 PLATFORM_ASSETS_COMPATIBILITY.env；请重做完整基线'
  validate_platform_assets_compatibility \
    "$baseline_compatibility" \
    "${baseline_common_matches[0]}" \
    "${baseline_platform_matches[0]}" \
    "${baseline_frontend_matches[0]}" \
    "${baseline_asset_matches[0]}"
  frontend_selection="$(offline_frontend_enabled_modules true "$selected_optional_components" "${portal_directory_mode:-crm}")" || die '前端组合无效'
  [[ "$(awk '$2 == "frontend-modules.json" {count++} END {print count+0}' "$baseline_dir/SHA256SUMS")" == 1 ]] ||
    die '基线总校验未绑定 frontend-modules.json；请完整重建基线'
  offline_validate_frontend_manifest "$baseline_dir/frontend-modules.json" "$frontend_selection" ||
    die '基线前端组合与本次交付不一致；请完整重建基线'
  validate_extension_asset_compatibility "${baseline_asset_matches[0]}"
  [[ -f "$baseline_dir/install-assets.sh" && ! -L "$baseline_dir/install-assets.sh" && -x "$baseline_dir/install-assets.sh" ]] ||
    die '基线交付缺少可执行 install-assets.sh'
  if [[ "$skip_host_deps" != true && -z "$reuse_host_deps_from" ]]; then
    reuse_host_deps_from="$baseline_dir"
  fi
  info "基线交付已校验：${baseline_dir}"
fi

validate_reusable_host_dependencies() {
  local archive="$1" metadata requested package
  metadata="$(tar -xOf "$archive" host-dependencies.env)" || die '宿主依赖包缺少可读元数据，请重新制作'
  requested="$(awk -F= '$1=="REQUESTED_PACKAGES" {n++; v=$2} END {if(n!=1)exit 1; print v}' <<< "$metadata")" || die '宿主依赖请求清单缺失或重复'
  for package in jq curl ca-certificates tar gzip coreutils util-linux tzdata python3; do
    case ",$requested," in
      *",$package,"*) ;;
      *) die "复用宿主依赖包缺少 $package；请在匹配目标 Ubuntu 的构建机重新制作，不把旧包用于本版离线交付" ;;
    esac
  done
}

find_reusable_host_dependencies() {
  local candidate
  if [[ -n "$reuse_host_deps_from" ]]; then
    [[ -d "$reuse_host_deps_from" && ! -L "$reuse_host_deps_from" ]] || die "宿主依赖复用目录无效：${reuse_host_deps_from}"
    printf '%s\n' "$(cd -- "$reuse_host_deps_from" && pwd)"
    return 0
  fi
  while IFS= read -r candidate; do
    [[ "$candidate" != "$output_dir" ]] || continue
    if compgen -G "$candidate/host-dependencies-*-linux-amd64.tar.gz" >/dev/null &&
       [[ -f "$candidate/install-host-dependencies.sh" && ! -L "$candidate/install-host-dependencies.sh" ]]; then
      printf '%s\n' "$candidate"
      return 0
    fi
  done < <(find "${script_dir}/.artifacts/offline" -mindepth 1 -maxdepth 1 -type d -print 2>/dev/null | LC_ALL=C sort -r)
  return 1
}

reusable_dir=""
if [[ "$skip_host_deps" != true ]]; then
  if [[ -n "$reuse_host_deps_from" ]]; then
    reusable_dir="$(find_reusable_host_dependencies)"
  elif [[ "$host_os" == Linux && "$host_arch" == x86_64 && -r /etc/os-release ]]; then
    # shellcheck disable=SC1091
    source /etc/os-release
    [[ "${ID:-}" == ubuntu ]] || die '宿主依赖包只能在 Ubuntu linux/amd64 构建机上生成；可用 --reuse-host-deps-from 复用已校验包'
    info "将构建 Ubuntu ${VERSION_ID:-unknown} ${VERSION_CODENAME:-unknown} 宿主依赖包"
  else
    reusable_dir="$(find_reusable_host_dependencies || true)"
    [[ -n "$reusable_dir" ]] || die 'macOS 不能生成 Ubuntu APT 依赖包，且未找到可复用产物；请传入 --reuse-host-deps-from'
  fi
  if [[ -n "$reusable_dir" ]]; then
    info "复用已校验宿主依赖：${reusable_dir}"
    shopt -s nullglob
    host_packages=("$reusable_dir"/host-dependencies-*-linux-amd64.tar.gz)
    ((${#host_packages[@]} >= 1)) || die "宿主依赖包缺失：${reusable_dir}"
    for host_package in "${host_packages[@]}"; do
      [[ -f "$host_package.sha256" && ! -L "$host_package.sha256" ]] ||
        die "宿主依赖包缺少 sidecar：${host_package}.sha256"
    done
    (
      cd "$reusable_dir"
      for host_sidecar in host-dependencies-*-linux-amd64.tar.gz.sha256; do
        sha256sum --check "$host_sidecar"
      done
    )
    for host_package in "${host_packages[@]}"; do validate_reusable_host_dependencies "$host_package"; done
    [[ -f "$reusable_dir/install-host-dependencies.sh" && ! -L "$reusable_dir/install-host-dependencies.sh" ]] ||
      die "宿主依赖安装入口缺失：${reusable_dir}"
  fi
else
  info '已显式跳过宿主依赖包；目标机必须预先安装所有部署命令'
fi

output_probe="$(dirname -- "$output_dir")"
while [[ ! -d "$output_probe" ]]; do
  next_probe="$(dirname -- "$output_probe")"
  [[ "$next_probe" != "$output_probe" ]] || break
  output_probe="$next_probe"
done
[[ -d "$output_probe" ]] || die "无法确定输出目录所在分区：${output_dir}"
output_available_kib="$(df -Pk "$output_probe" | awk 'NR==2 {print $4}')"
((output_available_kib >= 10 * 1024 * 1024)) || die '交付输出分区可用空间少于 10 GiB'
info "输出分区可用：$((output_available_kib / 1024)) MiB"
if [[ "$host_os" == Linux ]]; then
  docker_root="$(docker info --format '{{.DockerRootDir}}')"
  [[ -d "$docker_root" ]] || die "Docker 数据目录不存在：${docker_root}"
  output_device="$(df -Pk "$output_probe" | awk 'NR==2 {print $1}')"
  docker_device="$(df -Pk "$docker_root" | awk 'NR==2 {print $1}')"
  docker_available_kib="$(df -Pk "$docker_root" | awk 'NR==2 {print $4}')"
  if [[ "$output_device" == "$docker_device" ]]; then
    ((docker_available_kib >= 40 * 1024 * 1024)) || die '输出与 Docker 共用分区时需要至少 40 GiB 可用空间'
  else
    ((docker_available_kib >= 30 * 1024 * 1024)) || die 'Docker 数据分区可用空间少于 30 GiB'
  fi
  info "Docker 数据分区可用：$((docker_available_kib / 1024)) MiB"
fi

if [[ "$preflight_only" == true ]]; then
  printf '\n构建前检查通过；未创建产物。\n'
  exit 0
fi

# BUILD_INFO 同时绑定授权输入、源码与工具链输入。
# 同一交付的所有组件均须使用相同绑定，不能只绑定部署资产。
license_assets_args=()
[[ "$candidate" == true ]] || license_assets_args+=(--formal-build)
if [[ "$license_enabled" == true ]]; then
  [[ -n "$license_file" ]] && license_assets_args+=(--license-file "$license_file")
  [[ -n "$runtime_approval_dir" ]] && license_assets_args+=(--runtime-approval-dir "$runtime_approval_dir")
  [[ "$runtime_approval_approved" == true ]] && license_assets_args+=(--runtime-approval-approved)
  [[ "$reuse_approved_crm_image" == true ]] && license_assets_args+=(--reuse-approved-crm-image)
  [[ -n "$installation_scenario" ]] && license_assets_args+=(--installation-scenario "$installation_scenario")
  [[ -n "$installation_customer" ]] && license_assets_args+=(--customer-id "$installation_customer")
  if [[ "${builtin_v2_only:-false}" == true ]]; then
    license_assets_args+=(--builtin-v2-no-runtime-approvals --release-spec "$release_spec")
  fi
fi

for component_name in $components; do
  printf '\n========== 构建 %s（linux/amd64） ==========\n' "$component_name"
  bash "$builder" --portal-customer-directory-mode "$portal_directory_mode" \
    --component "$component_name" --version "$version" --output "$output_dir" \
    --selected-components "$selected_optional_components" \
    ${license_assets_args[@]+"${license_assets_args[@]}"}
done

if [[ -n "$baseline_dir" ]]; then
  # 先让底层构建器在空输出目录创建本次 BUILD_INFO，再添加已校验基线。
  # 否则底层的混合发布门禁会正确拒绝“有包但没有 BUILD_INFO”的目录。
  for baseline_file in \
    "${baseline_common_matches[0]}" "${baseline_common_matches[0]}.sha256" \
    "${baseline_platform_matches[0]}" "${baseline_platform_matches[0]}.sha256" \
    "${baseline_frontend_matches[0]}" "${baseline_frontend_matches[0]}.sha256" \
    "$baseline_dir/frontend-modules.json" \
    "$baseline_compatibility"; do
    install -m 600 "$baseline_file" "$output_dir/$(basename -- "$baseline_file")"
  done
  if [[ "$license_enabled" != true ]]; then
    install -m 600 "${baseline_asset_matches[0]}" "${baseline_asset_matches[0]}.sha256" "$output_dir/"
  fi
  if [[ "$license_enabled" != true ]]; then
    install -m 755 "$baseline_dir/install-assets.sh" "$output_dir/install-assets.sh"
    [[ ! -f "$baseline_dir/install-assets.sh.sha256" ]] ||
      install -m 600 "$baseline_dir/install-assets.sh.sha256" "$output_dir/install-assets.sh.sha256"
  fi
  baseline_manifest_digest="$(sha256sum "$baseline_dir/SHA256SUMS" | awk '{print tolower($1)}')"
  {
    printf 'BASE_DELIVERY=%s\n' "$(basename -- "$baseline_dir")"
    printf 'BASE_SHA256SUMS_SHA256=%s\n' "$baseline_manifest_digest"
    printf 'COMMON_PACKAGE=%s\n' "$(basename -- "${baseline_common_matches[0]}")"
    printf 'PLATFORM_PACKAGE=%s\n' "$(basename -- "${baseline_platform_matches[0]}")"
    printf 'FRONTEND_PACKAGE=%s\n' "$(basename -- "${baseline_frontend_matches[0]}")"
    printf 'ASSETS_PACKAGE=%s\n' "$(basename -- "${baseline_asset_matches[0]}")"
    printf 'PLATFORM_ASSETS_COMPATIBILITY_SHA256=%s\n' \
      "$(sha256sum "$baseline_compatibility" | awk '{print tolower($1)}')"
  } > "$output_dir/BASE_DELIVERY_INFO.txt"
  chmod 600 "$output_dir/BASE_DELIVERY_INFO.txt"
  info '已复用基线 common/platform/frontend/deployment-assets；未触发基础镜像重建'
  if [[ "$license_enabled" == true ]]; then
    # 基线资产摘要仅用于追溯来源，不作为新的授权摘要。
    # 须使用本次交付的绑定重新生成具有唯一版本号的资产。
    info '增量携带商业授权：重建部署资产包（客户许可证与安装场景写入交付）'
    bash "$builder" --portal-customer-directory-mode "$portal_directory_mode" \
      --component assets --version "$version" --output "$output_dir" \
      --selected-components "$selected_optional_components" \
      "${license_assets_args[@]}"
    new_assets_file="$output_dir/deployment-assets-${version}.tar.gz"
    verify_bound_sidecar "$new_assets_file"
    write_platform_assets_compatibility "$output_dir/PLATFORM_ASSETS_COMPATIBILITY.env" \
      "${baseline_common_matches[0]}" "${baseline_platform_matches[0]}" "${baseline_frontend_matches[0]}" "$new_assets_file"
    printf 'DELIVERY_ASSETS_PACKAGE=%s\nDELIVERY_ASSETS_SHA256=%s\n' \
      "$(basename -- "$new_assets_file")" "$(sha256sum "$new_assets_file" | awk '{print tolower($1)}')" \
      >> "$output_dir/BASE_DELIVERY_INFO.txt"
    info '增量介质已携带商业授权；部署时须按安装场景完成存量确认及授权激活门禁'
  fi
fi

if [[ "$skip_host_deps" != true ]]; then
  if [[ -z "$reusable_dir" ]]; then
    printf '\n========== 构建 Ubuntu 宿主依赖 ==========\n'
    bash "$builder" --portal-customer-directory-mode "$portal_directory_mode" \
      --component host-deps --version "$version" --output "$output_dir" \
      --selected-components "$selected_optional_components" ${license_assets_args[@]+"${license_assets_args[@]}"}
  else
    shopt -s nullglob
    host_files=("$reusable_dir"/host-dependencies-*-linux-amd64.tar.gz "$reusable_dir"/host-dependencies-*-linux-amd64.tar.gz.sha256)
    for host_file in "${host_files[@]}"; do install -m 600 "$host_file" "$output_dir/"; done
    install -m 755 "$reusable_dir/install-host-dependencies.sh" "$output_dir/install-host-dependencies.sh"
    [[ ! -f "$reusable_dir/install-host-dependencies.sh.sha256" ]] ||
      install -m 600 "$reusable_dir/install-host-dependencies.sh.sha256" "$output_dir/install-host-dependencies.sh.sha256"
  fi
fi

if [[ -z "$baseline_dir" ]]; then
  printf '\n========== 生成部署资产 ==========\n'
  if [[ "$license_enabled" == true ]]; then
    info '携带商业授权：客户许可证经平台内置公钥验签后随部署资产交付'
    bash "$builder" --portal-customer-directory-mode "$portal_directory_mode" \
      --component assets --version "$version" --output "$output_dir" \
      --selected-components "$selected_optional_components" \
      "${license_assets_args[@]}"
  else
    bash "$builder" --portal-customer-directory-mode "$portal_directory_mode" \
      --component assets --version "$version" --output "$output_dir" \
      --selected-components "$selected_optional_components"
  fi

  shopt -s nullglob
  output_common_matches=("$output_dir"/common-infrastructure-linux-amd64.tar.gz)
  output_platform_matches=("$output_dir"/platform-backend-*-linux-amd64.tar.gz)
  output_frontend_matches=("$output_dir"/frontend-*-linux-amd64.tar.gz)
  output_asset_matches=("$output_dir"/deployment-assets-*.tar.gz)
  shopt -u nullglob
  ((${#output_common_matches[@]} == 1)) || die '完整基线必须恰好包含一个 common 镜像包'
  ((${#output_platform_matches[@]} == 1)) || die '完整基线必须恰好包含一个 platform 镜像包'
  ((${#output_frontend_matches[@]} == 1)) || die '完整基线必须恰好包含一个 frontend 镜像包'
  ((${#output_asset_matches[@]} == 1)) || die '完整基线必须恰好包含一个 deployment-assets 包'
  [[ "$(platform_assets_input_fingerprint)" == "$platform_assets_input_digest" ]] ||
    die '构建期间 Platform/前端/共享部署输入发生变化；拒绝生成兼容性锁，请使用新版本重建'
  write_platform_assets_compatibility \
    "$output_dir/PLATFORM_ASSETS_COMPATIBILITY.env" \
    "${output_common_matches[0]}" \
    "${output_platform_matches[0]}" \
    "${output_frontend_matches[0]}" \
    "${output_asset_matches[0]}"
  validate_platform_assets_compatibility \
    "$output_dir/PLATFORM_ASSETS_COMPATIBILITY.env" \
    "${output_common_matches[0]}" \
    "${output_platform_matches[0]}" \
    "${output_frontend_matches[0]}" \
    "${output_asset_matches[0]}"
  scope_args=()
  [[ -z "$release_spec" ]] || scope_args+=(--spec "$release_spec")
  python3 "$deploy_dir/bin/check-delivery-scope.py" \
    --media "$output_dir" \
    --selected-components "$selected_optional_components" \
    ${scope_args[@]+"${scope_args[@]}"} ||
    die '交付范围一致性校验失败：release spec、镜像包和部署资产不一致；拒绝出厂'
else
  printf '\n========== 复用基线部署资产 ==========\n'
fi
install -m 755 "$server_installer" "$output_dir/服务器一键离线部署.sh"
install -m 755 "$operations_tool" "$output_dir/系统运维工具.sh"
# 清理工具只随介质交付，不装入将被删除的 target，也不由安装器自动执行。
install -m 755 "$fresh_install_reset_tool" "$output_dir/清理首次部署产物.sh"
install -m 644 "$fresh_install_reset_guide" "$output_dir/FIRST_INSTALL_RESET.md"
# 授权与恢复入口随介质交付并绑定摘要，不要求离线客户另行获取源码。
for license_helper in license-activate.sh license-upgrade.sh forward-recover.sh; do
  [[ -f "$script_dir/$license_helper" && ! -L "$script_dir/$license_helper" ]] || die "缺少安全授权入口：$license_helper"
  install -m 755 "$script_dir/$license_helper" "$output_dir/$license_helper"
  bash -n "$output_dir/$license_helper"
done
bash -n "$output_dir/服务器一键离线部署.sh"
bash -n "$output_dir/系统运维工具.sh"
bash -n "$output_dir/清理首次部署产物.sh"
# 构建工作站与目标服务器均在发布前运行同一检查器。
# 检查器依赖的可信文件须随包交付，并置于资产归档之外。
delivery_tools=(delivery-inspect.py delivery-inspect-images.py delivery-inspect-v2.py subsystem-runtime.py deployment-session.py deployment-workflow.sh delivery-files.py)
for delivery_tool in "${delivery_tools[@]}"; do
  [[ -f "$deploy_dir/bin/$delivery_tool" && ! -L "$deploy_dir/bin/$delivery_tool" ]] || die "缺少交付检查/恢复工具：$delivery_tool"
  install -m 644 "$deploy_dir/bin/$delivery_tool" "$output_dir/$delivery_tool"
done

# 构建器不了解仓库根目录的中文一键入口，最后重建一次完整清单。
(
  cd "$output_dir"
  shopt -s nullglob
  checksum_files=(*.tar.gz *.sha256 BUILD_INFO.txt install-assets.sh 服务器一键离线部署.sh 系统运维工具.sh 清理首次部署产物.sh FIRST_INSTALL_RESET.md)
  [[ ! -f frontend-modules.json ]] || checksum_files+=(frontend-modules.json)
  [[ ! -f build-evidence.json ]] || checksum_files+=(build-evidence.json)
  [[ ! -f source-input-index.tsv ]] || checksum_files+=(source-input-index.tsv)
  [[ ! -f base-image-evidence.tsv ]] || checksum_files+=(base-image-evidence.tsv)
  [[ ! -f PLATFORM_ASSETS_COMPATIBILITY.env ]] || checksum_files+=(PLATFORM_ASSETS_COMPATIBILITY.env)
  [[ ! -f BASE_DELIVERY_INFO.txt ]] || checksum_files+=(BASE_DELIVERY_INFO.txt)
  checksum_files+=(license-activate.sh license-upgrade.sh forward-recover.sh)
  checksum_files+=("${delivery_tools[@]}")
  [[ ! -f install-host-dependencies.sh ]] || checksum_files+=(install-host-dependencies.sh)
  sha256sum -- "${checksum_files[@]}" > SHA256SUMS
  sha256sum --check SHA256SUMS
  # 交付物（交付目录完整性清单，无敏感内容）：显式 644，保持与补 umask 077 前一致的可读性。
  chmod 644 SHA256SUMS
  for checksum_file in ./*.sha256; do sha256sum --check "$checksum_file"; done
  zip_or_apple="$(find . -maxdepth 2 \( -name '*.zip' -o -name '__MACOSX' -o -name '._*' \) -print -quit 2>/dev/null || true)"
  [[ -z "$zip_or_apple" ]] || {
    echo "交付输出包含 zip/Finder 元数据：$zip_or_apple；只能交付 tar+.sha256" >&2
    exit 1
  }
)

if [[ -n "$release_spec" && -z "$release_build_key$release_release_key$release_trust_dir$release_verifier" ]]; then
  info 'release spec 仅用于材料/协议门禁；未提供签名密钥，跳过 finalize'
fi
if [[ -n "$release_spec" && -n "$release_build_key" && -n "$release_release_key" && -n "$release_trust_dir" && -n "$release_verifier" ]]; then
  printf '\n========== 发布完整性 finalize ==========\n'
  release_profiles_dir="${release_profiles_dir:-$deploy_dir/subsystems.d}"
  bash "$deploy_dir/bin/finalize-release-integrity.sh" \
    --media "$output_dir" --spec "$release_spec" --profiles-dir "$release_profiles_dir" \
    --build-key "$release_build_key" --release-key "$release_release_key" \
    --trust-dir "$release_trust_dir" --verifier "$release_verifier" ||
    die '发布完整性 finalize 失败，拒绝生成总归档'
fi

if [[ -n "$review_from" ]]; then
  [[ -n "$approval_operator" ]] || die '--review-from 需要同时提供 --operator'
  [[ -d "$review_from" && ! -L "$review_from" ]] || die '--review-from 必须是安全目录'
  review_from="$(cd -- "$review_from" && pwd)"
  printf '\n========== 审核差异 ==========\n'
  python3 "$deploy_dir/bin/release-workflow.py" diff --before "$review_from" --after "$output_dir" --format human ||
    die '审核差异生成失败，拒绝生成总归档'
fi

[[ "$(platform_assets_input_fingerprint)" == "$platform_assets_input_digest" ]] ||
  die '构建期间 Platform/前端/共享部署输入发生变化；已拒绝发布，请使用新版本重建'

selected_optional_count="$(printf '%s\n' $selected_optional_components | sed '/^$/d' | wc -l | tr -d ' ')"
expected_image_package_count=$((3 + selected_optional_count))
image_package_count="$({ find "$output_dir" -maxdepth 1 -type f -name '*-linux-amd64.tar.gz' ! -name 'host-dependencies-*'; } | wc -l | tr -d ' ')"
[[ "$image_package_count" == "$expected_image_package_count" ]] ||
  die "应有 ${expected_image_package_count} 个镜像包，实际为 ${image_package_count}"
while IFS= read -r image_package; do
  tar -xOf "$image_package" package.env | grep -Fxq 'PLATFORM=linux/amd64' || die "镜像包架构不正确：${image_package}"
done < <(find "$output_dir" -maxdepth 1 -type f -name '*-linux-amd64.tar.gz' ! -name 'host-dependencies-*' -print)

printf '\n========== 统一交付检查 ==========\n'
python3 "$deploy_dir/bin/delivery-inspect.py" --media "$output_dir" --phase prepare --format human ||
  die '镜像、资产、组件或批准材料检查未通过，拒绝发布总归档'

output_parent="$(cd -- "$(dirname -- "$output_dir")" && pwd)"
output_name="$version"
archive_name="uip-offline-delivery-${version}-linux-amd64.tar"
archive_path="${output_parent}/${archive_name}"
[[ ! -e "$archive_path" && ! -L "$archive_path" && ! -e "$archive_path.sha256" ]] || die "总归档已存在：${archive_path}"
temporary_archive="$(mktemp "${output_parent}/.${archive_name}.XXXXXX")"
# macOS bsdtar 支持 --no-xattrs，但简版 --help 不列出该选项。
# 与底层镜像/资产打包器一致，显式禁用扩展属性，避免 Linux 解包时的 Apple PAX 告警。
(
  archive_stage="$(mktemp -d "${output_parent}/.uip-archive-root.XXXXXX")"
  trap 'rm -rf -- "$archive_stage"' EXIT
  # 与输出同分区硬链接，避免复制大型镜像；归档内部根名只取已校验版本。
  python3 - "$output_dir" "$archive_stage/$output_name" <<'PYROOT'
import os
from pathlib import Path
import stat
import sys

source, destination = map(Path, sys.argv[1:])
for current, directories, files in os.walk(source, followlinks=False):
    current = Path(current)
    target = destination / current.relative_to(source)
    target.mkdir(mode=0o700)
    for name in directories:
        if not stat.S_ISDIR((current / name).lstat().st_mode):
            sys.exit("归档源包含不安全目录")
    for name in files:
        path = current / name
        if not stat.S_ISREG(path.lstat().st_mode):
            sys.exit("归档源包含非普通文件")
        os.link(path, target / name, follow_symlinks=False)
PYROOT
  COPYFILE_DISABLE=1 tar --no-xattrs -cf "$temporary_archive" -C "$archive_stage" -- "$output_name"
)
chmod 600 "$temporary_archive"
mv "$temporary_archive" "$archive_path"
(
  cd "$output_parent"
  sha256sum "$archive_name" > "$archive_name.sha256"
  sha256sum --check "$archive_name.sha256"
  # 交付物（总归档 sidecar，随 .tar 一并交付给甲方上传）：显式 644，保持与补 umask 077 前一致的可读性。
  chmod 644 "$archive_name.sha256"
)

if tar -tf "$archive_path" | grep -Eq '(^|/)(\.DS_Store|\._)'; then
  die '最终归档中发现 Apple 元数据'
fi

if [[ -n "$customer_name" ]]; then
  printf '\n========== 整理客户交付目录 ==========\n'
  bash "$script_dir/整理客户交付目录.sh" \
    --customer "$customer_name" \
    --media "$output_dir" \
    --archive "$archive_path" \
    --public-keys "$customer_public_keys" \
    --verifier "$customer_verifier" \
    --output-parent "$output_parent" ||
    die '客户交付目录整理失败'
fi

printf '\n========== 制作完成 ==========\n'
printf '交付目录：%s\n' "$output_dir"
printf '单文件归档：%s\n' "$archive_path"
printf '归档校验：%s.sha256\n' "$archive_path"
printf '目标架构：linux/amd64\n'
printf '服务器操作：上传 .tar、.sha256，解压后执行 ./服务器一键离线部署.sh\n'
printf '材料检查：bash ./服务器一键离线部署.sh --media <总归档或目录> --check-delivery\n'
printf '安装进度：bash ./服务器一键离线部署.sh --target <部署目录> --status；中断后以原命令追加 --resume\n'
printf '失败首装重置：介质顶层 清理首次部署产物.sh（默认只盘点），先阅读 FIRST_INSTALL_RESET.md；仅限可丢弃数据的失败首装\n'
printf '部署完成后：在部署目录执行 ./系统运维工具.sh 进入安全运维菜单\n'
if [[ "$legacy_license_mode" == bridge ]]; then
  printf '兼容包下一步：仅升级基础平台，保留旧业务；随后用 license-activate.sh --customer %s --confirm-initialize 导出申请（已有身份须匹配）。\n' "$legacy_license_customer"
elif [[ "$legacy_license_mode" == upgrade ]]; then
  printf '迁移包下一步：先发布控制面并执行 license-install prepare --scenario migrate --customer %s，冻结成功后才更新旧业务，再执行 migrate。\n' "$installation_customer"
  printf '服务器一键入口 --upgrade 不自动冻结；正式授权须有效 JWS 及全部必需组件当前 ACK。\n'
fi
