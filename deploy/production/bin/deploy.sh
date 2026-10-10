#!/usr/bin/env bash
# =============================================================================
# 统一离线部署入口（部署目录内使用）
#
#   运行位置：部署目录，即本文件所在 bin/ 的上一级同时存在 docker-compose.yml、
#             .env、.release.env、subsystems.d/ 的目录（默认 /opt/unified-identity-platform）。
#   前置条件：已解压 deployment-assets-*.tar.gz，并至少执行过一次 configure。
#   完整流程：见同目录 OFFLINE_DEPLOYMENT.md；子命令总览执行 ./bin/deploy.sh help。
#   注意：    交付目录顶层的“参考副本”不能替代本文件运行；本文件被移出完整
#             部署目录时会直接给出缺少哪些文件的明确错误。
# =============================================================================
set -Eeuo pipefail
umask 077

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
deploy_dir="$(cd -- "$script_dir/.." && pwd)"
runtime_file="$deploy_dir/.env"
release_file="$deploy_dir/.release.env"
manifest_dir="$deploy_dir/manifests"
package_dir="$deploy_dir/packages"
registry_address="${UIP_OFFLINE_REGISTRY:-127.0.0.1:5000}"
compose_file="$deploy_dir/docker-compose.yml"
profiles_dir="$deploy_dir/subsystems.d"
asset_install_transaction="$deploy_dir/runtime/.assets-install-transaction"
control_plane_reload_marker="$deploy_dir/runtime/.control-plane-reload-required"
if [[ -e "$asset_install_transaction" || -L "$asset_install_transaction" ]]; then
  printf '错误：检测到未完成的部署资产安装事务：%s\n' "$asset_install_transaction" >&2
  printf '拒绝在脚本/清单可能混合的状态下继续部署。请使用同一交付包中的 install-assets.sh --recover %s 恢复后重试。\n' "$deploy_dir" >&2
  exit 1
fi
source "$script_dir/compose-scope.sh"
source "$script_dir/start-enabled.sh"
source "$script_dir/offline-package-metadata.sh"

die() { printf '错误：%s\n' "$*" >&2; exit 1; }
need() { command -v "$1" >/dev/null || die "缺少所需命令：$1"; }
random_hex() { openssl rand -hex "${1:-32}"; }
random_b64() { openssl rand -base64 32 | tr -d '\n'; }

usage() {
  cat <<'EOF'
用法：./bin/deploy.sh [子命令] [参数]

不带子命令时进入中文交互菜单。

部署主流程（按此顺序）：
  configure                     一次性运行配置：IP、端口、管理员、时区、防火墙
  install                       扫描 packages/ 并自动部署基础平台/前端、准备已提供的子系统包
                                带客户许可证及批准清单时继续受控接入并等待授权确认
  license-renew <许可证路径>   导入同实例安全续期并等待组件确认，不重打业务镜像
  import <镜像包路径>           导入镜像包，校验摘要并登记不可变 digest
  deploy platform               发布基础平台（含数据库迁移与首个管理员初始化）
  deploy frontend               发布统一前端
  start                         分阶段启动公共平台及 YAML 内已接入子系统
  resume <子系统>               恢复单个已接入子系统：依赖健康等待、备份、迁移与启动
  disable <子系统>              显式停止单系统，保留数据卷与密钥
  status [子系统]               查看容器状态；带子系统时显示阶段与唯一下一步
  verify [子系统]               健康检查；带子系统时对该子系统做部署验收
  logs <应用编码>               查看该应用的全部容器日志

业务子系统接入（每个子系统严格按此顺序，不可跳步）：
  import <包> -> prepare <子系统> -> 在平台页面采用 <应用编码>/prod
              -> status <子系统> -> continue <子系统> -> verify <子系统>

日常运维：
  upgrade <组件>                暂存新版本镜像 digest，再由平台页面受控更新
  doctor [子系统]               只读环境自检：配置权限、镜像包摘要、导入记录与
                                .release.env 一致性、Agent 绑定、runtime 占位凭据；
                                带子系统时显示当前阶段与下一步
  reload-control-plane          部署资产变更后成对重建 Agent 与 platform-api，并校验
                                subsystems.d 集合摘要、单文件挂载、Socket 与健康状态
  backup                        对所有运行中的 MySQL 做逻辑备份并校验摘要
  restore --service <服务> --backup <文件> --verify-only
  restore --service <服务> --backup <文件> --confirm RESTORE_MYSQL_SERVICE

支持的子系统参数：
  customer-opportunity  客户与商机管理系统
  customer-portal       客户自助门户
  contract              合同管理系统
  project               项目服务内容管理系统
  settlement            结算与开票管理系统
  data-analysis         数据看板与统计分析系统

help                          显示本帮助
EOF
}

discover_package() {
  local component="$1" pattern
  case "$component" in
    common) pattern='common-infrastructure-linux-amd64.tar.gz' ;;
    platform) pattern='platform-backend-*-linux-amd64.tar.gz' ;;
    frontend) pattern='frontend-*-linux-amd64.tar.gz' ;;
    customer-opportunity) pattern='customer-opportunity-backend-*-linux-amd64.tar.gz' ;;
    customer-portal) pattern='customer-portal-backend-*-linux-amd64.tar.gz' ;;
    contract) pattern='contract-backend-*-linux-amd64.tar.gz' ;;
    project) pattern='project-backend-*-linux-amd64.tar.gz' ;;
    settlement) pattern='settlement-backend-*-linux-amd64.tar.gz' ;;
    data-analysis) pattern='data-analysis-backend-*-linux-amd64.tar.gz' ;;
    *) die "不支持自动发现的组件：$component" ;;
  esac
  local -a matches=()
  shopt -s nullglob
  matches=("$package_dir"/$pattern)
  shopt -u nullglob
  if ((${#matches[@]} > 1)); then
    printf '错误：packages/ 中发现多个 %s 镜像包，无法安全选择：%s\n' "$component" "${matches[*]}" >&2
    return 2
  fi
  ((${#matches[@]} == 1)) || return 1
  [[ -e "${matches[0]}" ]] || return 1
  [[ ! -L "${matches[0]}" ]] || {
    printf '错误：%s 镜像包不能是符号链接：%s\n' "$component" "${matches[0]}" >&2
    return 2
  }
  [[ -f "${matches[0]}" ]] || {
    printf '错误：%s 镜像包必须是普通文件：%s\n' "$component" "${matches[0]}" >&2
    return 2
  }
  printf '%s\n' "${matches[0]}"
}

package_index() {
  case "$1" in
    common) printf '0' ;;
    platform) printf '1' ;;
    frontend) printf '2' ;;
    customer-opportunity) printf '3' ;;
    customer-portal) printf '4' ;;
    contract) printf '5' ;;
    project) printf '6' ;;
    settlement) printf '7' ;;
    data-analysis) printf '8' ;;
    *) return 1 ;;
  esac
}

auto_install_packages() {
  require_initialized
  scope_services >/dev/null || die '统一 Compose 无法解析'
  scope_report
  local component archive index
  local -a packages=()
  local -a ordered=(common platform frontend customer-opportunity customer-portal contract project settlement data-analysis)

  printf '扫描镜像包目录：%s\n' "$package_dir"
  # 先解析全部包名并检查歧义，避免平台启动后才发现包版本冲突。
  for component in "${ordered[@]}"; do
    scope_enabled "$component" || { printf '  %s 未启用，跳过镜像包检查。\n' "$component"; continue; }
    if archive="$(discover_package "$component")"; then
      index="$(package_index "$component")"
      packages[$index]="$archive"
      printf '  已发现 %-20s %s\n' "$component" "$(basename -- "$archive")"
    else
      case "$?" in
        2) die "镜像包发现存在歧义或文件类型不安全：$component" ;;
        *)
          if [[ "$component" == common || "$component" == platform || "$component" == frontend ]]; then
            die "缺少基础组件镜像包：${component}；请先将对应 tar.gz 放入 ${package_dir}"
          fi
          printf '  未提供 %-20s（跳过）\n' "$component"
          ;;
      esac
    fi
  done

  printf '\n阶段 A：导入公共基础设施和基础平台镜像\n'
  import_package "${packages[0]}"
  import_package "${packages[1]}"
  printf '\n阶段 B：启动并检查基础平台\n'
  deploy_component platform
  if ! wait_for_required_service_health platform-api platform-worker file-gateway keycloak temporal; then
    printf '自动安装暂停于基础平台验收；已完成的迁移和服务会保留。修复问题后执行：sudo ./bin/deploy.sh verify\n' >&2
    return 1
  fi

  printf '\n阶段 C：导入并启动统一前端\n'
  import_package "${packages[2]}"
  deploy_component frontend
  if ! verify; then
    printf '自动安装暂停于前端验收；基础平台保持运行。修复问题后执行：sudo ./bin/deploy.sh verify\n' >&2
    return 1
  fi

  # Only the platform maintenance command can establish a signed installation
  # identity. Plain env values must never choose or reset a customer instance.
  if [[ -e "$deploy_dir/license-installation.json" || -L "$deploy_dir/license-installation.json" ]]; then
    prepare_license_installation || return 1
  elif [[ -e "$deploy_dir/license" || -L "$deploy_dir/license" ]]; then
    install_delivery_license import || return 1
  fi

  printf '\n阶段 D：导入并准备已提供的业务子系统镜像\n'
  for component in customer-opportunity customer-portal contract project settlement data-analysis; do
    index="$(package_index "$component")"
    [[ -n "${packages[$index]:-}" ]] || continue
    import_package "${packages[$index]}"
    prepare_subsystem "$component" || return 1
  done
  if [[ -f "$deploy_dir/license-installation.json" ]]; then
    # Only the immutable pre-upgrade baseline selects migration targets; new
    # packages and purchased systems cannot manufacture grandfathering.
    compose run -T --rm --no-deps platform-api ./license-install migrate || return 1
    printf '\n存量迁移确认完成；过渡资格不等于正式授权，新增未授权系统未开放业务。\n'
  fi
  if [[ -e "$deploy_dir/license" || -L "$deploy_dir/license" ]]; then
    install_delivery_license activate || return 1
    printf '\n交付授权已生效：全部必需业务执行组件已确认。\n'
    return 0
  fi
  if [[ -f "$deploy_dir/license-installation.json" ]]; then
    printf '\n四场景安装准备完成：基础平台和前端可用，存量系统已按冻结基线完成迁移确认。\n'
    printf '新增未授权系统不会开放业务；请在授权管理中导入有效许可证并完成组件激活确认。\n'
    return 0
  fi
  printf '\n自动安装阶段完成：基础平台和前端已部署；子系统包已登记并准备候选。\n'
  printf '子系统服务不会自动启动；请在平台页面逐个受控采用 prod，再按 status -> continue -> verify 完成验收。\n'
}

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
  # Platform control can be upgraded first. No business import/update starts
  # until the trusted Agent has frozen the still-running old installation.
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
  # A separate lock avoids holding the Agent deployment lock while activating.
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

env_get() {
  local file="$1" key="$2"
  awk -F= -v key="$key" '$0 !~ /^[[:space:]]*#/ && $1 == key {sub(/^[^=]*=/, ""); print; exit}' "$file"
}

env_set() {
  local file="$1" key="$2" value="$3" temporary
  [[ "$value" != *$'\n'* ]] || die "$key 不允许包含换行符"
  temporary="$(mktemp "$deploy_dir/.env-update.XXXXXX")"
  awk -F= -v key="$key" -v value="$value" '
    BEGIN { found=0 }
    $0 !~ /^[[:space:]]*#/ && $1 == key { print key "=" value; found=1; next }
    { print }
    END { if (!found) print key "=" value }
  ' "$file" > "$temporary"
  chmod 600 "$temporary"
  mv -f "$temporary" "$file"
}

env_set_generated() {
  local file="$1" key="$2" value="$3" current
  current="$(env_get "$file" "$key")"
  if [[ -n "$current" && "$current" != REPLACE_WITH_* && "$current" != PENDING_* ]]; then
    return 0
  fi
  env_set "$file" "$key" "$value"
}

require_initialized() {
  [[ -f "$runtime_file" && -f "$release_file" ]] || die "请先执行：$0 configure"
  [[ "$(stat -c '%a' "$runtime_file")" == 600 && "$(stat -c '%a' "$release_file")" == 600 ]] || die "配置文件权限必须为 0600"
}

ensure_application_network() {
  local network_name="basic-platform-production"
  if docker network inspect "$network_name" >/dev/null 2>&1; then
    return 0
  fi
  docker network create \
    --driver bridge \
    --subnet 172.31.255.0/24 \
    --gateway 172.31.255.1 \
    "$network_name" >/dev/null || docker network inspect "$network_name" >/dev/null
}

compose() {
  ensure_application_network
  public_transport_prepare "$deploy_dir" "$runtime_file"
  local args=(--project-directory "$deploy_dir" --file "$compose_file")
  public_transport_compose_args args
  docker compose "${args[@]}" --env-file "$runtime_file" --env-file "$release_file" "$@"
}

compose_monitoring() {
  compose "$@"
}

# 本脚本必须与同一资产包中的 docker-compose.yml、offline-configure.sh、public-transport.sh
# 放在一起才能运行。交付目录顶层的 server_up.sh 只是本脚本的中文副本，单独执行会在这里
# 被拦截并给出可操作的提示，而不是抛出难以理解的 source 错误。
# 这里只列 source 阶段真正依赖的文件；deploy-service.sh 仅 deploy/upgrade 需要，故意
# 不在此校验，以免破坏只复制部分脚本的 fixture 测试。
missing_siblings=()
for sibling in offline-configure.sh public-transport.sh; do
  [[ -f "$script_dir/$sibling" ]] || missing_siblings+=("$script_dir/$sibling")
done
[[ -f "$deploy_dir/docker-compose.yml" ]] || missing_siblings+=("$deploy_dir/docker-compose.yml")
if ((${#missing_siblings[@]} > 0)); then
  printf '错误：当前脚本不在完整的部署目录中，缺少以下文件：\n' >&2
  printf '  %s\n' "${missing_siblings[@]}" >&2
  printf '请先解压 deployment-assets-*.tar.gz 到部署目录，再执行：\n' >&2
  printf '  cd <部署目录> && sudo ./bin/deploy.sh\n' >&2
  printf '交付目录顶层的 server_up.sh 是本脚本的中文副本，仅供查看和审核，不能直接运行。\n' >&2
  exit 1
fi

# shellcheck source=offline-configure.sh
source "$script_dir/offline-configure.sh"

ensure_registry_bridge() {
  # Docker metadata is authoritative; never assume docker0 or 172.17.0.0/16.
  [[ "$(docker inspect -f '{{.HostConfig.NetworkMode}}' uip-offline-registry)" == bridge ]] || return 0
  local metadata bridge subnet gateway prefix addresses link routes
  need ip; need jq
  metadata="$(docker network inspect bridge)" || die '无法读取离线仓库 bridge 配置'
  bridge="$(jq -er '.[0].Options["com.docker.network.bridge.name"]' <<<"$metadata")" || die 'Docker bridge 接口未配置'
  subnet="$(jq -er '[.[0].IPAM.Config[] | select(.Subnet | contains(":" ) | not)] | if length == 1 then .[0].Subnet else error("ambiguous IPv4 subnet") end' <<<"$metadata")" || die 'Docker bridge IPv4 网段不唯一'
  gateway="$(jq -er --arg subnet "$subnet" '.[0].IPAM.Config[] | select(.Subnet == $subnet) | .Gateway' <<<"$metadata")" || die 'Docker bridge IPv4 网关缺失'
  [[ "$bridge" =~ ^[a-zA-Z0-9_.-]+$ && "$subnet" =~ ^[0-9.]+/[0-9]+$ && "$gateway" =~ ^[0-9.]+$ ]] || die 'Docker bridge 配置格式异常'
  prefix="${subnet##*/}"
  link="$(ip -d -j link show dev "$bridge")" || die "Docker bridge 接口不存在：$bridge"
  jq -e 'length == 1 and .[0].linkinfo.info_kind == "bridge"' <<<"$link" >/dev/null || die "拒绝修改非 bridge 接口：$bridge"
  addresses="$(ip -4 -j addr show dev "$bridge")" || die "无法读取桥地址：$bridge"
  if ! jq -e --arg gateway "$gateway" --argjson prefix "$prefix" 'any(.[].addr_info[]?; .local == $gateway and .prefixlen == $prefix)' <<<"$addresses" >/dev/null; then
    jq -e '[.[].addr_info[]? | select(.family == "inet")] | length == 0' <<<"$addresses" >/dev/null || die "桥 $bridge 已有其他 IPv4 地址，拒绝覆盖；请核对 Docker 与宿主网络配置"
    [[ "$EUID" -eq 0 ]] || die "桥 $bridge 缺少 $gateway/$prefix，需要 root 恢复后重试"
    printf '恢复 Docker bridge 缺失地址：%s → %s/%s\n' "$bridge" "$gateway" "$prefix"
    ip address add "$gateway/$prefix" dev "$bridge" || die '恢复 Docker bridge 地址失败'
  fi
  if ! jq -e '.[0].flags | index("UP") != null' <<<"$link" >/dev/null; then
    ip link set dev "$bridge" up || die '启动 Docker bridge 接口失败'
  fi
  routes="$(ip -4 -j route show exact "$subnet")" || die '无法读取 Docker bridge 路由'
  if ! jq -e --arg bridge "$bridge" --arg gateway "$gateway" 'any(.[]; .dev == $bridge and .prefsrc == $gateway and (.gateway == null))' <<<"$routes" >/dev/null; then
    [[ "$(jq 'length' <<<"$routes")" == 0 ]] || die "Docker 网段 $subnet 存在冲突路由，拒绝覆盖"
    ip route add "$subnet" dev "$bridge" src "$gateway" || die '恢复 Docker bridge 路由失败'
  fi
}

ensure_registry() {
  need curl
  docker image inspect registry:2.8.3 >/dev/null 2>&1 || die "请先导入公共基础设施镜像包"
  if ! docker container inspect uip-offline-registry >/dev/null 2>&1; then
    docker run -d --name uip-offline-registry --restart unless-stopped -p 127.0.0.1:5000:5000 -v uip-offline-registry-data:/var/lib/registry registry:2.8.3 >/dev/null
  elif [[ "$(docker inspect -f '{{.State.Running}}' uip-offline-registry)" != true ]]; then
    docker start uip-offline-registry >/dev/null
  fi
  ensure_registry_bridge
  local attempt
  for attempt in {1..15}; do
    if curl --noproxy '*' --fail --silent --connect-timeout 2 --max-time 3 "http://$registry_address/v2/" >/dev/null; then
      printf '离线镜像仓库可达性检查通过：http://%s/v2/\n' "$registry_address"
      return 0
    fi
    sleep 1
  done
  die "离线镜像仓库不可达：http://$registry_address/v2/；已停止镜像推送，请检查 Docker bridge、防火墙及仓库日志"
}

manifest_value() {
  local file="$1" key="$2"
  awk -F= -v key="$key" '$1 == key && $0 ~ /^[A-Z_]+=[A-Za-z0-9.,_:\/-]+$/ {sub(/^[^=]*=/, ""); print; exit}' "$file"
}

verify_archive_sidecar() {
  local archive="${1:?archive required}" sidecar="${2:-$1.sha256}" expected actual
  [[ -f "$sidecar" && ! -L "$sidecar" ]] || die "镜像包校验文件缺失或不是普通文件：$sidecar"
  expected="$(awk -v name="$(basename -- "$archive")" '
    NF != 2 || NR != 1 || length($1) != 64 || tolower($1) ~ /[^0-9a-f]/ || $2 != name {bad=1}
    {digest=tolower($1)}
    END {if (bad || NR != 1) exit 1; print digest}
  ' "$sidecar")" || die "镜像包校验文件必须只包含当前包的一个规范摘要：$sidecar"
  actual="$(sha256sum "$archive" | awk '{print tolower($1)}')"
  [[ "$actual" == "$expected" ]] || die "镜像包 SHA256 校验失败：$archive"
  printf '%s\n' "$actual"
}

# Docker with the containerd image store may expose an image-index digest from
# `docker image inspect .Id`, while format-2 packages deliberately record the
# linux/amd64 image config digest contained in images.tar.  A raw `.Id`
# mismatch is therefore not sufficient to reject an otherwise correctly
# loaded package.  Re-export the exact loaded tag under a private temporary
# alias and resolve its config blob with the same strict archive parser used by
# the builder.  This still rejects a tag bound to different image content; it
# does not weaken the package digest or platform checks.
verify_loaded_image_config_digest() (
  local tag="${1:?image tag required}" expected="${2:?expected config digest required}"
  local work_dir="${3:?work directory required}" sequence="${4:?sequence required}"
  local alias image_tar manifest_file manifest_member actual
  alias="uip-import-verify/${sequence}:local"
  image_tar="$work_dir/loaded-${sequence}.tar"
  manifest_file="$work_dir/loaded-${sequence}-manifest.json"

  cleanup_loaded_image_verification() {
    local status=$?
    trap - EXIT
    docker image rm "$alias" >/dev/null 2>&1 || true
    rm -f -- "$image_tar" "$manifest_file"
    exit "$status"
  }
  trap cleanup_loaded_image_verification EXIT

  docker tag "$tag" "$alias" >/dev/null || return 1
  docker image save --output "$image_tar" "$alias" >/dev/null || return 1
  if manifest_member="$(offline_archive_member "$image_tar" manifest.json 2>/dev/null)"; then
    tar -xOf "$image_tar" "$manifest_member" > "$manifest_file" || return 1
  elif tar -tf "$image_tar" | awk '$0 == "manifest.json" || $0 == "./manifest.json" {found++} END {exit found ? 0 : 1}'; then
    return 1
  else
    : > "$manifest_file"
  fi
  actual="$(offline_saved_image_config_id "$image_tar" "$manifest_file" "$alias")" || return 1
  [[ "$actual" == "$expected" ]]
)

# Do not use /tmp for package extraction and image re-export.  On hardened
# hosts /tmp is commonly a small tmpfs (the acceptance host has 1.7 GiB), while
# the verified common images.tar plus one temporary docker-save archive can
# require several GiB at the same time.  Keep import scratch data on the
# deployment filesystem, which is already covered by the installer space
# budget.  Operators may select another dedicated filesystem explicitly.
prepare_import_temp_root() {
  local temp_root="${UIP_IMPORT_TMPDIR:-$deploy_dir/runtime/import-tmp}"
  [[ ! -L "$temp_root" ]] || die "镜像导入临时目录不能是符号链接：$temp_root"
  if [[ -e "$temp_root" && ! -d "$temp_root" ]]; then
    die "镜像导入临时路径不是目录：$temp_root"
  fi
  mkdir -p -- "$temp_root" || die "无法创建镜像导入临时目录：$temp_root"
  chmod 700 "$temp_root" || die "无法收紧镜像导入临时目录权限：$temp_root"
  printf '%s\n' "$temp_root"
}

import_package() (
  local archive="${1:?package path required}" temporary import_temp_root manifest component version platform images image_ids expected_images tag local_name pushed digest record index actual_id expected_digest
  local record_temporary='' pointer_temporary=''
  local -a data_analysis_keys=(DATA_ANALYSIS_DASHBOARD_API_IMAGE DATA_ANALYSIS_AGGREGATION_WORKER_IMAGE DATA_ANALYSIS_ALERT_WORKER_IMAGE DATA_ANALYSIS_MIGRATE_IMAGE)
  local -a data_analysis_image_refs=()
  need docker; need tar; need gzip; need sha256sum; need awk; need curl; need jq
  # 分开判断三种情况：交付目录 iso/ 下的包不会自动进入部署目录的 packages/，
  # 这是现场最常见的漏步；旧写法用 -f 把“文件不存在”误报成“是符号链接”。
  [[ ! -L "$archive" ]] || die "镜像包不能是符号链接：$archive"
  if [[ ! -e "$archive" ]]; then
    die "镜像包不存在：$archive
交付目录 iso/ 下的镜像包需要先复制到部署目录的 packages/，再执行 import：
  sudo cp <交付目录>/iso/$(basename -- "$archive") $package_dir/
如包已放在其他位置，也可以直接 import 该实际路径，但需同时提供同名的 .sha256 伴随文件。"
  fi
  [[ -f "$archive" ]] || die "镜像包必须是普通文件：$archive"
  require_initialized
  need flock
  exec 8>"$deploy_dir/runtime/.deploy.lock"
  flock -w 60 8 || die '其他导入或发布任务正在运行'
  [[ ! -L "$archive.sha256" ]] || die "镜像包校验文件不能是符号链接：$archive.sha256"
  if [[ ! -f "$archive.sha256" ]]; then
    # 前置安装脚本已依据交付清单生成 packages/SHA256SUMS。后补上传的包只要登记在其中，
    # 就用清单里的摘要补出伴随文件：省掉手工步骤，且摘要仍来自审核清单，不降低信任级别。
    expected_digest="$(awk -v name="$(basename -- "$archive")" '$2 == name {print tolower($1)}' "$package_dir/SHA256SUMS" 2>/dev/null)"
    if [[ "$expected_digest" =~ ^[0-9a-f]{64}$ ]]; then
      printf '%s  %s\n' "$expected_digest" "$(basename -- "$archive")" > "$archive.sha256"
      chmod 600 "$archive.sha256"
      printf '已依据 packages/SHA256SUMS 生成伴随校验文件：%s\n' "$(basename -- "$archive").sha256"
    else
      die "镜像包缺少 SHA256 校验文件：$archive.sha256
该文件未登记在 packages/SHA256SUMS 中，无法自动补出。请在包所在目录执行：
  sha256sum $(basename -- "$archive") > $(basename -- "$archive").sha256"
    fi
  fi
  verify_archive_sidecar "$archive" "$archive.sha256" >/dev/null
  import_temp_root="$(prepare_import_temp_root)"
  temporary="$(mktemp -d "$import_temp_root/uip-import.XXXXXX")" || die "无法创建镜像导入临时工作目录：$import_temp_root"
  trap 'rm -r -- "$temporary"; [[ -z "$record_temporary" ]] || rm -f -- "$record_temporary"; [[ -z "$pointer_temporary" ]] || rm -f -- "$pointer_temporary"' EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM
  local listing details
  listing="$(tar -tzf "$archive")" || die '镜像包无法读取'
  details="$(tar -tvzf "$archive")" || die '镜像包目录无法校验'
  # 兼容旧版离线包中由 macOS COPYFILE 写入的三个 AppleDouble 元数据文件。
  # 仅放行已知 ._ 条目；任何其他额外路径仍按非法包拒绝。
  printf '%s\n' "$listing" | awk '
    $0 == "package.env" || $0 == "SHA256SUMS" || $0 == "images.tar" {seen[$0]++; next}
    $0 == "._package.env" || $0 == "._SHA256SUMS" || $0 == "._images.tar" {metadata[$0]++; next}
    {bad=1}
    END {
      exit bad || seen["package.env"] != 1 || seen["SHA256SUMS"] != 1 || seen["images.tar"] != 1 ||
        metadata["._package.env"] > 1 || metadata["._SHA256SUMS"] > 1 || metadata["._images.tar"] > 1
    }' || die '镜像包条目不符合格式'
  printf '%s\n' "$details" | awk 'substr($1,1,1) != "-" {bad=1} END {exit bad ? 1 : 0}' || die '镜像包包含链接或特殊文件'
  tar -xzf "$archive" --no-same-owner --no-same-permissions -C "$temporary"
  rm -f -- "$temporary/._package.env" "$temporary/._SHA256SUMS" "$temporary/._images.tar"
  awk 'NF != 2 || $2 != "images.tar" || length($1) != 64 || $1 ~ /[^0-9a-fA-F]/ {bad=1} END {exit bad || NR != 1}' "$temporary/SHA256SUMS" || die '镜像内容校验清单无效'
  (cd "$temporary" && sha256sum -c SHA256SUMS)
  manifest="$temporary/package.env"
  component="$(manifest_value "$manifest" COMPONENT)"; version="$(manifest_value "$manifest" VERSION)"
  platform="$(manifest_value "$manifest" PLATFORM)"; images="$(manifest_value "$manifest" IMAGES)"
  # Format 2 records Docker save config digests. IMAGE_IDS remains accepted for
  # packages emitted by the short-lived format-1 builder.
  image_ids="$(manifest_value "$manifest" IMAGE_CONFIG_DIGESTS)"
  image_ids="${image_ids:-$(manifest_value "$manifest" IMAGE_IDS)}"
  [[ -n "$component" && -n "$version" && "$platform" == linux/amd64 && -n "$images" && -n "$image_ids" ]] || die "镜像包元数据不正确"
  [[ "$version" =~ ^[A-Za-z0-9._-]+$ ]] || die "镜像包版本格式不安全：$version"
  case "$component" in
    common) expected_images="mysql:8.4,quay.io/keycloak/keycloak:26.2,temporalio/auto-setup:1.29.7,metabase/metabase:v0.53.7,prom/prometheus:v3.5.0,prom/node-exporter:v1.9.1,registry:2.8.3,tecnativa/docker-socket-proxy:v0.5.0,uip-package/file-gateway:$version" ;;
    platform) expected_images="uip-package/platform-backend:$version" ;;
    frontend) expected_images="uip-package/frontend:$version" ;;
    customer-opportunity) expected_images="uip-package/customer-opportunity-backend:$version" ;;
    customer-portal) expected_images="uip-package/customer-portal-backend:$version" ;;
    contract) expected_images="uip-package/contract-backend:$version" ;;
    project) expected_images="uip-package/project-backend:$version" ;;
    settlement) expected_images="uip-package/settlement-backend:$version" ;;
    data-analysis) expected_images="uip-package/data-analysis-dashboard-api:$version,uip-package/data-analysis-aggregation-worker:$version,uip-package/data-analysis-alert-worker:$version,uip-package/data-analysis-production-migrate:$version" ;;
    *) die "镜像包组件未登记：$component" ;;
  esac
  [[ "$images" == "$expected_images" ]] || die "镜像名称与登记组件 $component 不匹配"
  if ! docker load --input "$temporary/images.tar" >/dev/null; then
    local engine_version
    engine_version="$(docker version --format '{{.Server.Version}}' 2>/dev/null || true)"
    die "docker load 无法读取镜像包（目标 Docker Engine 版本：${engine_version:-未知}）。离线镜像包使用 OCI 镜像布局（blobs/sha256 + index.json + oci-layout），需要目标 Docker Engine 支持该布局，版本过低会在此处失败。请升级 Docker Engine，或先运行交付目录中的“服务器前置检查与安装脚本.sh”，用其中的镜像加载能力探测提前确认。"
  fi
  IFS=, read -r -a image_tags <<< "$images"
  IFS=, read -r -a expected_ids <<< "$image_ids"
  [[ "${#image_tags[@]}" -eq "${#expected_ids[@]}" ]] || die "镜像名称数量与镜像 ID 数量不一致"
  for index in "${!image_tags[@]}"; do
    tag="${image_tags[$index]}"
    [[ "$(docker image inspect "$tag" --format '{{.Architecture}}/{{.Os}}')" == amd64/linux ]] || die "镜像平台不是 linux/amd64：$tag"
    actual_id="$(docker image inspect "$tag" --format '{{.Id}}')"
    if [[ "$actual_id" != "${expected_ids[$index]}" ]]; then
      verify_loaded_image_config_digest "$tag" "${expected_ids[$index]}" "$temporary" "${component}-${index}" || {
        die "导入后的镜像配置摘要与清单不一致：${tag}（Docker 报告 ID：${actual_id}，清单 config digest：${expected_ids[$index]}）"
      }
      printf 'Docker 报告的镜像 ID 不是 config digest，已通过重新导出的归档校验：%s\n' "$tag"
    fi
  done
  if [[ "$component" != common ]]; then
    ensure_registry
    if [[ "$component" == data-analysis ]]; then
      [[ "${#image_tags[@]}" -eq 4 ]] || die "数据看板镜像包必须包含 API、聚合 Worker、告警 Worker 和迁移器四个镜像"
      for index in "${!image_tags[@]}"; do
        tag="${image_tags[$index]}"
        local_name="$registry_address/uip/${tag##*/}"
        docker tag "$tag" "$local_name"
        docker push "$local_name" >/dev/null
        pushed="$(docker image inspect "$local_name" --format '{{index .RepoDigests 0}}')"
        [[ "$pushed" == "$registry_address/"*"@sha256:"* ]] || die "本地镜像仓库没有返回不可变摘要：$local_name"
        data_analysis_image_refs+=("$pushed")
      done
      pushed="${data_analysis_image_refs[0]}"
      digest="${pushed#*@sha256:}"
    else
      [[ "${#image_tags[@]}" -eq 1 ]] || die "每个普通应用镜像包必须且只能包含一个镜像"
      tag="${image_tags[0]}"; local_name="$registry_address/uip/${component}:${version}"
      docker tag "$tag" "$local_name"
      docker push "$local_name" >/dev/null
      pushed="$(docker image inspect "$local_name" --format '{{index .RepoDigests 0}}')"
      [[ "$pushed" == "$registry_address/"*"@sha256:"* ]] || die "本地镜像仓库没有返回不可变摘要"
      digest="${pushed#*@sha256:}"
    fi
  else
    ensure_registry
    local gateway_tag="uip-package/file-gateway:$version" gateway_local pushed_gateway
    gateway_local="$registry_address/uip/file-gateway:$version"
    docker tag "$gateway_tag" "$gateway_local"
    docker push "$gateway_local" >/dev/null
    pushed_gateway="$(docker image inspect "$gateway_local" --format '{{index .RepoDigests 0}}')"
    [[ "$pushed_gateway" == "$registry_address/"*"@sha256:"* ]] || die '本地镜像仓库没有返回 File Gateway 不可变摘要'
    pushed="$images"; digest="-"
  fi
  local pointer
  record="$manifest_dir/${component}-${version}.imported"
  record_temporary="$(mktemp "$manifest_dir/.${component}-${version}.imported.XXXXXX")"
  {
    printf 'COMPONENT=%s\nVERSION=%s\nIMAGE_REF=%s\nSOURCE_IMAGE_IDS=%s\nIMAGE_DIGEST=%s\nPACKAGE_SHA256=%s\n' "$component" "$version" "$pushed" "$image_ids" "$digest" "$(sha256sum "$archive" | awk '{print $1}')"
    if [[ "$component" == common ]]; then
      printf 'FILE_GATEWAY_IMAGE_REF=%s\n' "$pushed_gateway"
    fi
    if [[ "$component" == data-analysis ]]; then
      for index in "${!data_analysis_keys[@]}"; do
        printf '%s=%s\n' "${data_analysis_keys[$index]}" "${data_analysis_image_refs[$index]}"
      done
    fi
  } > "$record_temporary"
  chmod 600 "$record_temporary"
  mv -f -- "$record_temporary" "$record"
  record_temporary=''
  pointer="$manifest_dir/${component}.latest"
  pointer_temporary="$(mktemp "$manifest_dir/.${component}.latest.XXXXXX")"
  printf '%s\n' "$(basename -- "$record")" > "$pointer_temporary"
  chmod 600 "$pointer_temporary"
  mv -f -- "$pointer_temporary" "$pointer"
  pointer_temporary=''
  printf '已导入组件：%s，版本：%s\n' "$component" "$version"
  # 把“下一步”写在成功输出里，避免 import 之后直接跳到平台页面而漏掉 prepare。
  case "$component" in
    platform|frontend)
      printf '下一步：./bin/deploy.sh deploy %s\n' "$component" ;;
    common)
      printf '下一步：继续导入 platform-backend 与 frontend 镜像包，再 deploy platform / deploy frontend\n' ;;
    *)
      printf '下一步（子系统必须执行，不可跳过）：./bin/deploy.sh prepare <子系统>\n'
      printf '  为什么：import 只把摘要登记到 manifests/；把不可变 digest 写入 .release.env 的是 prepare。\n'
      printf '  跳过 prepare 直接到平台页面采用/重试，会报 “production subsystem image must use an immutable digest”。\n'
      printf '  子系统参数：contract | project | settlement | data-analysis | customer-opportunity | customer-portal\n' ;;
  esac
)

latest_record() {
  local component="$1" record pointer selected='' selected_mtime='' mtime name
  local -a records=()
  pointer="$manifest_dir/${component}.latest"
  if [[ -e "$pointer" || -L "$pointer" ]]; then
    [[ -f "$pointer" && ! -L "$pointer" ]] || die "最新导入指针不是普通文件：$pointer"
    name="$(awk 'NF == 1 && NR == 1 {value=$1; next} {bad=1} END {if (bad || NR != 1) exit 1; print value}' "$pointer")" ||
      die "最新导入指针格式无效：$pointer"
    [[ "$name" == "$component"-*.imported && "$name" != */* ]] || die "最新导入指针包含非法记录名：$pointer"
    record="$manifest_dir/$name"
    [[ -f "$record" && ! -L "$record" ]] || die "最新导入指针指向的记录不存在或不安全：${record}；请重新 import 目标版本"
    [[ "$(env_get "$record" COMPONENT)" == "$component" ]] || die "最新导入指针与记录组件不一致：$record"
    printf '%s\n' "$record"
    return 0
  fi

  # Backward compatibility for installations created before *.latest existed:
  # select by import-record modification time, never by version-string ordering.
  # Equal timestamps are ambiguous and therefore rejected until an explicit
  # re-import writes the pointer.
  shopt -s nullglob
  records=("$manifest_dir"/"$component"-*.imported)
  shopt -u nullglob
  for record in "${records[@]}"; do
    [[ -f "$record" && ! -L "$record" ]] || continue
    if mtime="$(stat -c '%Y' "$record" 2>/dev/null)"; then :; else mtime="$(stat -f '%m' "$record")"; fi
    if [[ -z "$selected" || "$mtime" -gt "$selected_mtime" ]]; then
      selected="$record"
      selected_mtime="$mtime"
    elif [[ "$mtime" -eq "$selected_mtime" ]]; then
      die "$component 存在多个同时间的旧导入记录，无法判断最后导入版本；请重新 import 需要使用的包"
    fi
  done
  [[ -n "$selected" ]] || die "没有找到已导入的 $component 镜像包；请先执行：$0 import packages/${component}-*-linux-amd64.tar.gz"
  printf '%s\n' "$selected"
}

latest_image() {
  env_get "$(latest_record "$1")" IMAGE_REF
}

imported_file_gateway_image() {
  local record ref
  record="$(latest_record common)"
  ref="$(env_get "$record" FILE_GATEWAY_IMAGE_REF)"
  [[ "$ref" =~ ^127\.0\.0\.1:[0-9]+/uip/file-gateway@sha256:[a-f0-9]{64}$ ]] || die '公共基础设施包缺少 File Gateway 不可变镜像摘要；请重新导入新版公共基础设施包'
  printf '%s\n' "$ref"
}

stage_data_analysis_images() {
  local record="${1:?import record required}" key image
  local -a keys=(DATA_ANALYSIS_DASHBOARD_API_IMAGE DATA_ANALYSIS_AGGREGATION_WORKER_IMAGE DATA_ANALYSIS_ALERT_WORKER_IMAGE DATA_ANALYSIS_MIGRATE_IMAGE)
  for key in "${keys[@]}"; do
    image="$(env_get "$record" "$key")"
    [[ "$image" =~ ^127\.0\.0\.1:[0-9]+/uip/[a-z0-9._/-]+@sha256:[a-f0-9]{64}$ ]] || die "离线数据看板导入记录缺少不可变镜像：${key}；请重新导入完整的数据看板镜像包"
    env_set "$release_file" "$key" "$image"
  done
  env_set "$release_file" DATA_ANALYSIS_IMAGE "$(env_get "$record" IMAGE_REF)"
}

deploy_component() {
  local component="${1:?component required}" image file_gateway_image bootstrap_password bootstrap_display bootstrap_account
  require_initialized
  scope_require "$component" || return
  image="$(latest_image "$component")"
  case "$component" in
    platform)
      file_gateway_image="$(imported_file_gateway_image)"
      # deploy-service owns the deployment lock and commits both image pointers
      # in one .release.env transaction. Do not mutate the gateway pointer here.
      "$script_dir/deploy-service.sh" platform "$image" "$file_gateway_image"
      bootstrap_password="$(env_get "$runtime_file" IAM_BOOTSTRAP_ADMIN_PASSWORD)"
      bootstrap_display="$(env_get "$runtime_file" IAM_BOOTSTRAP_ADMIN_DISPLAY_NAME)"
      bootstrap_account="$(env_get "$runtime_file" IAM_BOOTSTRAP_ADMIN_ACCOUNT_NAME)"
      [[ -n "$bootstrap_password" && -n "$bootstrap_display" && -n "$bootstrap_account" ]] || die "首个平台管理员配置不完整"
      printf '%s\n' "$bootstrap_password" | compose run -T --rm platform-migrate ./bootstrap-admin \
        --display-name "$bootstrap_display" --account-name "$bootstrap_account" --password-stdin
      echo '基础平台管理员已初始化或已经存在；受保护的初始凭据仍保存在权限为 0600 的运行时配置文件中，请首次登录后及时修改。'
      if scope_services | grep -Fxq prometheus; then
        compose_monitoring up -d prometheus keycloak-backup-metrics
      fi
      ;;
    frontend) "$script_dir/deploy-service.sh" frontend "$image" ;;
    *) die "业务子系统必须依次执行 prepare、基础平台探测接入、continue：$component" ;;
  esac
}

# .release.env 以单文件 bind mount 挂进 subsystem-provisioner。env_set 用 mv 做原子替换后，
# 宿主机 inode 变化，而容器仍绑定旧 inode，会一直读到替换前的占位镜像值 —— 表现为平台页面
# 反复报 “production subsystem image must use an immutable digest”，即使宿主机文件已经正确。
# 写完 .release.env 后必须让 Agent 重新绑定，否则 prepare/upgrade 的成果对 Agent 不可见。
# 如果 platform-api 已经运行，还必须成对重载：API 与 Agent 都在启动时缓存生产清单，
# 只重建一侧会触发 manifest drift 保护，导致受控采用/更新被正确拒绝。
refresh_subsystem_provisioner() {
  refresh_subsystem_control_plane_config ||
    die '子系统控制面未能成对重载或验证当前发布配置；已拒绝继续发布'
}

reload_control_plane() {
  require_initialized
  need flock
  [[ ! -L "$deploy_dir/runtime/.deploy.lock" ]] || die '部署锁不能是符号链接'
  if [[ -L "$control_plane_reload_marker" ]]; then
    die "控制面重载标记不能是符号链接：$control_plane_reload_marker"
  fi
  if [[ -e "$control_plane_reload_marker" && ! -f "$control_plane_reload_marker" ]]; then
    die "控制面重载标记不是普通文件：$control_plane_reload_marker"
  fi
  exec 7>"$deploy_dir/runtime/.deploy.lock"
  flock -w 60 7 || die '平台 Agent 或其他发布任务正在运行；请等待其结束后重试控制面重载'
  if ! refresh_subsystem_control_plane_config; then
    printf '错误：子系统控制面成对重载或一致性验证失败。重载标记已保留；修复后重新执行：%s reload-control-plane\n' "$0" >&2
    flock -u 7
    exec 7>&-
    return 1
  fi
  if [[ -f "$control_plane_reload_marker" ]]; then
    rm -f -- "$control_plane_reload_marker" || {
      printf '错误：控制面已验证，但无法移除重载标记：%s\n' "$control_plane_reload_marker" >&2
      flock -u 7
      exec 7>&-
      return 1
    }
  fi
  printf '子系统控制面重载完成：Agent 与 platform-api 使用同一生产清单集合。\n'
  flock -u 7
  exec 7>&-
}

stage_upgrade() {
  local component="${1:?component required}" image key record
  require_initialized
  scope_require "$component" || return
  record="$(latest_record "$component")"
  image="$(env_get "$record" IMAGE_REF)"
  case "$component" in
    platform|frontend) deploy_component "$component"; return ;;
    customer-opportunity) key=CUSTOMER_CRM_IMAGE ;;
    customer-portal) key=CUSTOMER_PORTAL_IMAGE ;;
    contract) key=CONTRACT_IMAGE ;;
    project) key=PROJECT_IMAGE ;;
    settlement) key=SETTLEMENT_IMAGE ;;
    data-analysis) key=DATA_ANALYSIS_IMAGE ;;
    *) die "不支持的组件：$component" ;;
  esac
  need flock
  [[ ! -L "$deploy_dir/runtime/.deploy.lock" ]] || die '部署锁不能是符号链接'
  exec 7>"$deploy_dir/runtime/.deploy.lock"
  flock -w 60 7 || die '平台 Agent 或其他发布任务正在运行；请等待其结束后重试升级'
  env_set "$release_file" "$key" "$image"
  if [[ "$component" == data-analysis ]]; then
    stage_data_analysis_images "$record"
  fi
  refresh_subsystem_provisioner
  flock -u 7
  exec 7>&-
  printf '组件 %s 的升级镜像已暂存：%s\n' "$component" "$image"
  echo '请打开基础平台的应用页面执行受控更新或重试；平台会依次完成升级前备份、数据库迁移和健康检查。'
}

subsystem_metadata() {
  local component="$1"
  subsystem_candidate="uip-${component}-candidate"
  case "$component" in
    customer-opportunity) subsystem_key=CUSTOMER_CRM_IMAGE; subsystem_service=customer-api; subsystem_app_code=customer_and_opportunity; subsystem_app_name='客户与商机管理系统'; subsystem_internal_host=customer-api; subsystem_internal_port=8090; subsystem_health=/customer-opportunity/healthz; subsystem_path=/customer-opportunity; subsystem_callback=/customer-opportunity/auth/callback; subsystem_runtime="$deploy_dir/runtime/customer.env" ;;
    customer-portal) subsystem_key=CUSTOMER_PORTAL_IMAGE; subsystem_service=portal-api; subsystem_app_code=customer_portal; subsystem_app_name='客户自助门户'; subsystem_internal_host=portal-api; subsystem_internal_port=8091; subsystem_health=/customer-portal/healthz; subsystem_path=/customer-portal; subsystem_callback=/customer-portal/auth/callback; subsystem_runtime="$deploy_dir/runtime/portal.env" ;;
    contract) subsystem_key=CONTRACT_IMAGE; subsystem_service=contract-api; subsystem_app_code=contract_management; subsystem_app_name='合同管理系统'; subsystem_internal_host=contract-api; subsystem_internal_port=8081; subsystem_health=/healthz; subsystem_path=/contract_management; subsystem_callback=/contract_management/auth/callback; subsystem_runtime="$deploy_dir/runtime/contract.env" ;;
    project) subsystem_key=PROJECT_IMAGE; subsystem_service=project-api; subsystem_app_code=project_management; subsystem_app_name='项目服务内容管理系统'; subsystem_internal_host=project-api; subsystem_internal_port=8082; subsystem_health=/healthz; subsystem_path=/project_management; subsystem_callback=/project_management/auth/callback; subsystem_runtime="$deploy_dir/runtime/project.env" ;;
    settlement) subsystem_key=SETTLEMENT_IMAGE; subsystem_service=settlement-api; subsystem_app_code=settlement; subsystem_app_name='结算与开票管理系统'; subsystem_internal_host=settlement-api; subsystem_internal_port=8085; subsystem_health=/healthz; subsystem_path=/settlement; subsystem_callback=/settlement/auth/callback; subsystem_runtime="$deploy_dir/runtime/settlement.env" ;;
    data-analysis) subsystem_key=DATA_ANALYSIS_IMAGE; subsystem_service=data-analysis-api; subsystem_app_code=data_analysis; subsystem_app_name='数据看板与统计分析系统'; subsystem_internal_host=data-analysis-api; subsystem_internal_port=8080; subsystem_health=/data_analysis/healthz; subsystem_path=/data_analysis; subsystem_callback=/data_analysis/auth/callback; subsystem_runtime="$deploy_dir/runtime/data-analysis.env" ;;
    *) die "不支持的子系统：$component" ;;
  esac
}

prepare_subsystem() {
  local component="${1:?component required}" image version phase profile
  require_initialized
  scope_require "$component" || return
  subsystem_metadata "$component"
  need flock
  [[ ! -L "$deploy_dir/runtime/.deploy.lock" ]] || die '部署锁不能是符号链接'
  exec 7>"$deploy_dir/runtime/.deploy.lock"
  flock -w 5 7 || die '平台 Agent 或其他发布任务正在运行；请等待其结束后执行 status'
  profile="$deploy_dir/subsystems.d/${subsystem_app_code}-prod.yaml"
  [[ "$component" != data-analysis ]] || profile="$deploy_dir/subsystems.d/data-analysis-prod.yaml"
  [[ -f "$profile" && ! -L "$profile" ]] || die "生产环境发布配置缺失或不是普通文件：$profile"
  phase="$(subsystem_phase "$component")"
  case "$phase" in
    VERIFIED)
      printf '%s/prod 已完成部署，无需重新创建候选容器。\n' "$subsystem_app_code"
      flock -u 7
      exec 7>&-
      return 0
      ;;
    READY_TO_CONTINUE)
      die "$subsystem_app_code/prod 已由平台部署完成；下一步仅执行：$0 continue $component"
      ;;
    WAITING_FOR_PLATFORM_ADOPTION|PROVISIONING_OR_FAILED)
      printf '候选容器已存在，未重复创建。当前阶段：%s。\n' "$phase"
      print_subsystem_next_action "$component" "$phase"
      flock -u 7
      exec 7>&-
      return 0
      ;;
    INVALID_CANDIDATE)
      die "发现标签不匹配的候选容器 ${subsystem_candidate}；请人工核对，脚本不会覆盖"
      ;;
  esac
  local record
  record="$(latest_record "$component")"
  image="$(env_get "$record" IMAGE_REF)"
  version="$(env_get "$record" VERSION)"
  env_set "$release_file" "$subsystem_key" "$image"
  if [[ "$component" == data-analysis ]]; then
    stage_data_analysis_images "$record"
  fi
  refresh_subsystem_provisioner
  docker create --name "$subsystem_candidate" --network none --entrypoint /bin/sh \
    --label com.basic-platform.discovery=v1 \
    --label "com.basic-platform.application_code=$subsystem_app_code" \
    --label "com.basic-platform.application_name=$subsystem_app_name" \
    --label com.basic-platform.environment=prod \
    --label "com.basic-platform.service_name=$subsystem_service" \
    --label com.basic-platform.service_role=business \
    --label "com.basic-platform.internal_host=$subsystem_internal_host" \
    --label "com.basic-platform.internal_port=$subsystem_internal_port" \
    --label com.basic-platform.protocol=http \
    --label "com.basic-platform.health_endpoint=$subsystem_health" \
    --label "com.basic-platform.path_prefix=$subsystem_path" \
    --label "com.basic-platform.oidc_callback_path=$subsystem_callback" \
    --label com.basic-platform.oidc_callback_supported=true \
    --label "com.basic-platform.version=$version" \
    "$image" -c 'exit 64' >/dev/null
  printf '候选容器已创建：%s\n当前阶段：WAITING_FOR_PLATFORM_ADOPTION\n' "$subsystem_candidate"
  print_subsystem_next_action "$component" WAITING_FOR_PLATFORM_ADOPTION
  flock -u 7
  exec 7>&-
}

runtime_ready() {
  local file="$1" report="${2:-true}" required prefix=OIDC
  [[ -f "$file" ]] || return 1
  required='PLATFORM_APPLICATION_CODE PLATFORM_ENVIRONMENT_CODE PLATFORM_AUDIT_CLIENT_ID PLATFORM_AUDIT_CLIENT_SECRET FILE_GATEWAY_BASE_URL FILE_GATEWAY_APPLICATION_ID FILE_GATEWAY_CLIENT_ID FILE_GATEWAY_CLIENT_SECRET'
  case "${file##*/}" in
    portal.env)
      prefix=PORTAL_OIDC
      required+=' PORTAL_ENCRYPTION_KEY_BASE64 PORTAL_REPORT_INGEST_DESCRIPTOR_KEY_BASE64 PORTAL_HMAC_KEY_BASE64 PORTAL_ROLE_CONFIG_HASH PORTAL_AUTHORIZATION_CONTEXT_URL PORTAL_AUTHORIZATION_CATALOG_APPLICATION_ID PORTAL_AUTHORIZATION_CATALOG_CLIENT_ID PORTAL_AUTHORIZATION_CATALOG_CLIENT_SECRET'
      ;;
    customer.env) required+=' SENSITIVE_ENCRYPTION_KEY_BASE64 SENSITIVE_HMAC_KEY_BASE64 PORTAL_INVITE_PEPPER_BASE64 OIDC_ROLE_CONFIG_HASH' ;;
    contract.env|project.env) required+=' OIDC_SESSION_ENCRYPTION_KEY_BASE64' ;;
    settlement.env)
      required='PLATFORM_ENVIRONMENT_CODE OIDC_SESSION_ENCRYPTION_KEY_BASE64'
      if [[ "$(env_get "$file" SETTLEMENT_FILE_GATEWAY_MODE)" == required ]]; then
        required+=' FILE_GATEWAY_URL SETTLEMENT_FILE_GATEWAY_APPLICATION_ID FILE_GATEWAY_CLIENT_ID FILE_GATEWAY_CLIENT_SECRET FILE_GATEWAY_SCOPE'
      fi
      ;;
    data-analysis.env) required+=' OIDC_CODEC_KEY OIDC_ROLE_CONFIG_HASH METABASE_EMBEDDING_SECRET' ;;
    *) echo '未知子系统配置文件' >&2; return 1 ;;
  esac
  [[ "$prefix" == PORTAL_OIDC ]] || required+=' PLATFORM_AUTHORIZATION_CONTEXT_URL PLATFORM_AUTHORIZATION_CATALOG_APPLICATION_ID PLATFORM_AUTHORIZATION_CATALOG_CLIENT_ID PLATFORM_AUTHORIZATION_CATALOG_CLIENT_SECRET'
  required+=" ${prefix}_ISSUER ${prefix}_CLIENT_ID ${prefix}_CLIENT_SECRET ${prefix}_REDIRECT_URI ${prefix}_TENANT_ID"
  # 只校验明确必需的核心接入字段；可选功能未启用时允许其配置为空。
  awk -v required="$required" -v report="$report" '
    /^[[:space:]]*(#|$)/ {next}
    {
      delimiter=index($0,"=")
      if (!delimiter) {if (report == "true") print "配置格式错误，行 " NR > "/dev/stderr"; bad=1; next}
      key=substr($0,1,delimiter-1); value=substr($0,delimiter+1)
      if (key !~ /^[A-Za-z_][A-Za-z0-9_]*$/ || seen[key]++) {
        if (report == "true") print "无效或重复配置键，行 " NR > "/dev/stderr"; bad=1
      }
      gsub(/^[[:space:]]+|[[:space:]]+$/, "", value)
      gsub(/^\047|\047$|^"|"$/, "", value)
      values[key]=value
    }
    END {
      n=split(required, keys, " ")
      for (i=1;i<=n;i++) if (values[keys[i]] == "" || values[keys[i]] ~ /^(PENDING_|REPLACE_WITH_)/) {
        if (report == "true") print "接入配置缺失：" keys[i] > "/dev/stderr"; bad=1
      }
      exit bad ? 1 : 0
    }' "$file"
}

subsystem_phase() {
  local component="$1" record candidate_code candidate_environment container running health
  subsystem_metadata "$component"
  record="$(find "$manifest_dir" -maxdepth 1 -type f -name "${component}-*.imported" -print -quit 2>/dev/null || true)"
  [[ -n "$record" ]] || { printf 'NOT_IMPORTED\n'; return; }

  if docker container inspect "$subsystem_candidate" >/dev/null 2>&1; then
    candidate_code="$(docker container inspect -f '{{index .Config.Labels "com.basic-platform.application_code"}}' "$subsystem_candidate" 2>/dev/null || true)"
    candidate_environment="$(docker container inspect -f '{{index .Config.Labels "com.basic-platform.environment"}}' "$subsystem_candidate" 2>/dev/null || true)"
    if [[ "$candidate_code" != "$subsystem_app_code" || "$candidate_environment" != prod ]]; then
      printf 'INVALID_CANDIDATE\n'
      return
    fi
  else
    container="$(compose ps -q "$subsystem_service" 2>/dev/null || true)"
    if [[ -n "$container" ]]; then
      running="$(docker inspect -f '{{.State.Running}}' "$container" 2>/dev/null || true)"
      health="$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' "$container" 2>/dev/null || true)"
      if [[ "$running" == true && ( "$health" == healthy || "$health" == none ) ]] && runtime_ready "$subsystem_runtime" false; then
        printf 'VERIFIED\n'
        return
      fi
    fi
    printf 'IMPORTED\n'
    return
  fi

  if ! runtime_ready "$subsystem_runtime" false; then
    printf 'WAITING_FOR_PLATFORM_ADOPTION\n'
    return
  fi
  container="$(compose ps -q "$subsystem_service" 2>/dev/null || true)"
  [[ -n "$container" ]] || { printf 'PROVISIONING_OR_FAILED\n'; return; }
  running="$(docker inspect -f '{{.State.Running}}' "$container" 2>/dev/null || true)"
  health="$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' "$container" 2>/dev/null || true)"
  if [[ "$running" == true && ( "$health" == healthy || "$health" == none ) ]]; then
    printf 'READY_TO_CONTINUE\n'
  else
    printf 'PROVISIONING_OR_FAILED\n'
  fi
}

print_subsystem_next_action() {
  local component="$1" phase="$2"
  subsystem_metadata "$component"
  case "$phase" in
    NOT_IMPORTED) printf '下一步：%s import packages/%s-*-linux-amd64.tar.gz\n' "$0" "$component" ;;
    IMPORTED) printf '下一步：%s prepare %s\n' "$0" "$component" ;;
    WAITING_FOR_PLATFORM_ADOPTION)
      printf '下一步：在基础平台“子系统探测与接入”页面采用 %s/prod。\n' "$subsystem_app_code"
      printf '此阶段不要执行 continue，也不要更新旧的 %s/dev 环境。\n' "$subsystem_app_code"
      ;;
    PROVISIONING_OR_FAILED) printf '下一步：在基础平台查看 %s/prod 部署状态；失败时对该 prod 环境点击“重试”，并查看 subsystem-provisioner 日志。\n' "$subsystem_app_code" ;;
    READY_TO_CONTINUE) printf '下一步：%s continue %s\n' "$0" "$component" ;;
    VERIFIED) printf '%s/prod 已完成部署和验收，无需继续操作。\n' "$subsystem_app_code" ;;
    INVALID_CANDIDATE) printf '下一步：人工核对候选容器 %s 的 application_code/environment 标签；脚本不会自动删除未知候选。\n' "$subsystem_candidate" ;;
    *) die "未知部署阶段：$phase" ;;
  esac
}

subsystem_status() {
  local component="$1" phase
  require_initialized
  subsystem_metadata "$component"
  phase="$(subsystem_phase "$component")"
  printf '组件：%s\n生产目标：%s/prod\n当前阶段：%s\n' "$component" "$subsystem_app_code" "$phase"
  print_subsystem_next_action "$component" "$phase"
}

continue_subsystem() {
  local component="${1:?component required}" phase container health
  require_initialized
  scope_require "$component" || return
  subsystem_metadata "$component"
  need flock
  [[ ! -L "$deploy_dir/runtime/.deploy.lock" ]] || die '部署锁不能是符号链接'
  exec 7>"$deploy_dir/runtime/.deploy.lock"
  flock -w 5 7 || die '平台 Agent 正在部署该子系统；请等待平台任务结束后再执行 continue'
  phase="$(subsystem_phase "$component")"
  if [[ "$phase" == VERIFIED ]]; then
    printf '%s/prod 已完成部署和验收。\n' "$subsystem_app_code"
    flock -u 7
    exec 7>&-
    return 0
  fi
  if [[ "$phase" != READY_TO_CONTINUE ]]; then
    printf '错误：当前阶段 %s 不允许执行 continue。\n' "$phase" >&2
    print_subsystem_next_action "$component" "$phase" >&2
    flock -u 7
    exec 7>&-
    return 1
  fi
  container="$(compose ps -q "$subsystem_service" 2>/dev/null || true)"
  health="$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' "$container")"
  docker rm "$subsystem_candidate" >/dev/null || die '服务已就绪，但候选容器删除失败，请检查后重试'
  printf '%s 正在运行（健康状态：%s）；阶段已进入 VERIFIED，候选容器已删除。\n' "$subsystem_service" "$health"
  flock -u 7
  exec 7>&-
}

# -----------------------------------------------------------------------------
# doctor：只读环境自检
#
# 现场反复出现的四类“环境不一致”都不是脚本缺陷，而是漏步或宿主机/容器视图分叉：
#   1. 镜像包只上传到交付目录的 iso/，没有进入部署目录的 packages/；
#   2. 只 import 没有 prepare，.release.env 里仍是占位镜像；
#   3. .release.env 被 mktemp+mv 原子替换后 inode 变化，以单文件 bind mount 挂载它的
#      subsystem-provisioner 仍绑旧 inode，宿主机文件已正确而 Agent 读到占位值；
#   4. subsystem runtime 中待平台下发的 Client 凭据仍是 PENDING_ONBOARDING，错误信息
#      却容易被误读成文件权限问题。
# doctor 一次性把这些查出来并给出可执行建议。它绝不修改任何文件、不重启或创建容器。
# -----------------------------------------------------------------------------
doctor_failures=0
doctor_warnings=0
doctor_docker_available=false

doctor_ok() { printf '[OK] %s\n' "$*"; }
doctor_warn() { doctor_warnings=$((doctor_warnings + 1)); printf '[警告] %s\n' "$*"; }
doctor_fail() { doctor_failures=$((doctor_failures + 1)); printf '[失败] %s\n' "$*"; }

doctor_is_placeholder_image() {
  local value="$1"
  [[ -z "$value" || "$value" == *:pending || "$value" == *:latest || "$value" == uip-package/* ]]
}

doctor_release_keys() {
  case "$1" in
    platform) printf 'PLATFORM_IMAGE\n' ;;
    frontend) printf 'FRONTEND_IMAGE\n' ;;
    customer-opportunity) printf 'CUSTOMER_CRM_IMAGE\n' ;;
    customer-portal) printf 'CUSTOMER_PORTAL_IMAGE\n' ;;
    contract) printf 'CONTRACT_IMAGE\n' ;;
    project) printf 'PROJECT_IMAGE\n' ;;
    settlement) printf 'SETTLEMENT_IMAGE\n' ;;
    data-analysis)
      printf '%s\n' DATA_ANALYSIS_IMAGE DATA_ANALYSIS_DASHBOARD_API_IMAGE \
        DATA_ANALYSIS_AGGREGATION_WORKER_IMAGE DATA_ANALYSIS_ALERT_WORKER_IMAGE DATA_ANALYSIS_MIGRATE_IMAGE
      ;;
    *) return 1 ;;
  esac
}

doctor_check_config_files() {
  printf '\n[1/6] 运行配置文件存在性与权限\n'
  local file label mode
  for file in "$runtime_file" "$release_file"; do
    label="${file##*/}"
    if [[ -L "$file" ]]; then
      doctor_fail "$label 是符号链接：$file"
      continue
    fi
    if [[ ! -e "$file" ]]; then
      doctor_fail "$label 不存在：${file}；请先执行：$0 configure"
      continue
    fi
    if [[ ! -f "$file" ]]; then
      doctor_fail "$label 不是普通文件：$file"
      continue
    fi
    mode="$(stat -c '%a' "$file" 2>/dev/null || true)"
    if [[ "$mode" == 600 ]]; then
      doctor_ok "$label 存在且权限为 0600"
    else
      doctor_fail "$label 权限为 ${mode:-未知}，必须为 0600：chmod 600 $file"
    fi
  done
}

doctor_check_packages() {
  printf '\n[2/6] packages/ 镜像包伴随校验文件\n'
  shopt -s nullglob
  local archives=("$package_dir"/*-linux-amd64.tar.gz)
  shopt -u nullglob
  if ((${#archives[@]} == 0)); then
    doctor_warn "未在 $package_dir 找到 *-linux-amd64.tar.gz；确认交付目录 iso/ 里的镜像包已复制到部署目录 packages/（见 OFFLINE_DEPLOYMENT.md 0.4）"
    return 0
  fi
  local archive sidecar base
  for archive in "${archives[@]}"; do
    base="$(basename -- "$archive")"
    sidecar="$archive.sha256"
    if [[ -L "$sidecar" ]]; then
      doctor_fail "校验伴随文件是符号链接：$sidecar"
      continue
    fi
    if [[ ! -f "$sidecar" ]]; then
      doctor_fail "缺少校验伴随文件：${sidecar}；请执行：(cd $(dirname -- "$archive") && sha256sum $base > $base.sha256)"
      continue
    fi
    if (verify_archive_sidecar "$archive" "$sidecar" >/dev/null 2>&1); then
      doctor_ok "$base 摘要校验通过"
    else
      doctor_fail "$base 摘要或伴随文件绑定失败；请重新复制当前包及与其同名的 .sha256 后重试"
    fi
  done
}

doctor_check_manifests() {
  printf '\n[3/6] manifests/ 导入记录与 .release.env 不可变引用\n'
  shopt -s nullglob
  local records=("$manifest_dir"/*.imported)
  shopt -u nullglob
  if ((${#records[@]} == 0)); then
    doctor_warn "未在 $manifest_dir 找到 *.imported；尚未导入任何镜像包"
  fi
  local record component image_ref key release_value expected_ref expected_field
  for record in "${records[@]}"; do
    component="$(env_get "$record" COMPONENT)"
    image_ref="$(env_get "$record" IMAGE_REF)"
    if [[ -z "$component" || -z "$image_ref" ]]; then
      doctor_fail "导入记录缺少 COMPONENT 或 IMAGE_REF：$(basename -- "$record")"
      continue
    fi
    if [[ "$component" == common ]]; then
      doctor_ok "$(basename -- "$record")：公共基础设施包按 IMAGES 清单发布，不产生单一不可变 digest"
      continue
    fi
    if [[ ! "$image_ref" =~ @sha256:[0-9a-f]{64}$ ]]; then
      doctor_fail "$(basename -- "$record") 的 IMAGE_REF 不是不可变 @sha256: 引用：${image_ref}；请重新 import 对应镜像包"
      continue
    fi
    if [[ ! -f "$release_file" ]]; then
      doctor_fail ".release.env 不存在，无法核对 $(basename -- "$record") 的镜像键；请先执行：$0 configure"
      continue
    fi
    local keys=()
    mapfile -t keys < <(doctor_release_keys "$component")
    if ((${#keys[@]} == 0)); then
      doctor_warn "未登记组件 $component 的 .release.env 键名，跳过一致性比较"
      continue
    fi
    local key_checked=false key_placeholder=false
    for key in "${keys[@]}"; do
      expected_field=IMAGE_REF
      expected_ref="$image_ref"
      # data-analysis 是一个包内包含四个生产镜像的复合组件。兼容键
      # DATA_ANALYSIS_IMAGE 仍指向 dashboard API，其余键必须逐项核对
      # 导入记录中的同名字段，不能拿通用 IMAGE_REF 误判 Worker/Migrate。
      if [[ "$component" == data-analysis && "$key" != DATA_ANALYSIS_IMAGE ]]; then
        expected_field="$key"
        expected_ref="$(env_get "$record" "$expected_field")"
        if [[ -z "$expected_ref" ]]; then
          doctor_fail "$(basename -- "$record") 缺少 ${expected_field}；请重新 import 对应镜像包"
          continue
        fi
        if [[ ! "$expected_ref" =~ @sha256:[0-9a-f]{64}$ ]]; then
          doctor_fail "$(basename -- "$record") 的 $expected_field 不是不可变 @sha256: 引用：${expected_ref}；请重新 import 对应镜像包"
          continue
        fi
      fi
      release_value="$(env_get "$release_file" "$key")"
      if [[ -z "$release_value" ]]; then
        doctor_fail "$key 在 .release.env 中未设置；请重新执行 prepare/deploy 使其写入不可变 digest"
        continue
      fi
      if doctor_is_placeholder_image "$release_value"; then
        key_placeholder=true
        continue
      fi
      if [[ "$release_value" != "$expected_ref" ]]; then
        doctor_fail "$key 与 $(basename -- "$record") 的 $expected_field 不一致：$release_value != ${expected_ref}；请重新 prepare/升级使两者一致"
      else
        key_checked=true
      fi
    done
    if [[ "$key_placeholder" == true ]]; then
      if [[ "$component" == platform || "$component" == frontend ]]; then
        doctor_warn "$component 已 import，但 ${keys[*]} 仍是占位值；下一步：$0 deploy $component"
      else
        doctor_warn "$component 已 import，但 ${keys[*]} 仍是占位值；下一步：$0 prepare ${component}（import 只登记 manifests/，写入 .release.env 的是 prepare）"
      fi
    elif [[ "$key_checked" == true ]]; then
      doctor_ok "${component}：${keys[*]} 已登记不可变 digest，与导入记录一致"
    fi
  done

  if [[ -f "$release_file" && ! -L "$release_file" ]]; then
    local placeholders=() line key value
    while IFS= read -r line; do
      [[ -n "${line//[[:space:]]/}" ]] || continue
      if [[ "$line" =~ ^[[:space:]]*# ]]; then continue; fi
      [[ "$line" == *=* ]] || continue
      key="${line%%=*}"; value="${line#*=}"
      if doctor_is_placeholder_image "$value"; then placeholders+=("$key=$value"); fi
    done < "$release_file"
    if ((${#placeholders[@]} > 0)); then
      doctor_warn ".release.env 仍有 ${#placeholders[@]} 个镜像键是占位值："
      printf '        %s\n' "${placeholders[@]}"
      printf '        提示：import 只把摘要登记到 manifests/；写入 .release.env 的是 prepare（子系统）或 deploy（platform/frontend）。\n'
    else
      doctor_ok ".release.env 中所有镜像键都已是不可变引用"
    fi
  fi
}

doctor_check_provisioner_binding() {
  printf '\n[4/6] subsystem-provisioner 容器内 .release.env 绑定一致性\n'
  if [[ "$doctor_docker_available" != true ]]; then
    doctor_warn '未找到 docker 命令，跳过容器内 .release.env 比对'
    return 0
  fi
  local container
  container="$(docker ps -q --filter 'label=com.docker.compose.service=subsystem-provisioner' 2>/dev/null || true)"
  [[ -n "$container" ]] || container="$(docker ps -q --filter 'name=uip-subsystem-provisioner' 2>/dev/null || true)"
  if [[ -z "$container" ]]; then
    doctor_ok 'subsystem-provisioner 未运行，无需比对容器内绑定'
    return 0
  fi
  local name path host_digest container_digest
  name="$(docker inspect -f '{{.Name}}' "$container" 2>/dev/null | sed 's#^/##' || true)"
  name="${name:-$container}"
  # 容器内路径优先取自 Agent 自己的环境变量，其次从绑定挂载的目的地推导。
  path="$(docker inspect -f '{{range .Config.Env}}{{println .}}{{end}}' "$container" 2>/dev/null |
    awk -F= '$1 == "SUBSYSTEM_PRODUCTION_RELEASE_ENV_PATH" {print $2; exit}' || true)"
  if [[ -z "$path" ]]; then
    path="$(docker inspect -f '{{range .Mounts}}{{println .Source "|" .Destination}}{{end}}' "$container" 2>/dev/null |
      awk -F'|' -v host="$release_file" '$1 == host {print $2; exit}' || true)"
  fi
  if [[ -z "$path" ]]; then
    doctor_warn "无法确定容器 $name 内 .release.env 的路径，跳过比对；如页面仍报 immutable digest，请执行：docker restart $name"
    return 0
  fi
  if [[ ! -f "$release_file" ]]; then
    doctor_fail "宿主机 .release.env 不存在，无法比对：$release_file"
    return 0
  fi
  host_digest="$(sha256sum "$release_file" | awk '{print $1}')"
  # 内容摘要优先：.release.env 是单文件 bind mount，原子替换后 inode 变化而内容比对能直接
  # 暴露“容器仍绑旧 inode”。镜像缺少 cat 时退回容器内 sha256sum。
  if container_digest="$(docker exec "$container" cat "$path" 2>/dev/null | sha256sum | awk '{print $1}')" &&
    docker exec "$container" test -f "$path" >/dev/null 2>&1; then
    :
  else
    container_digest="$(docker exec "$container" sha256sum "$path" 2>/dev/null | awk '{print $1}' || true)"
  fi
  if [[ -z "$container_digest" ]]; then
    local started_at host_mtime
    started_at="$(docker inspect -f '{{.State.StartedAt}}' "$container" 2>/dev/null || true)"
    host_mtime="$(stat -c '%y' "$release_file" 2>/dev/null || true)"
    doctor_warn "无法读取容器 $name 内的 ${path}（镜像可能缺少 cat/sha256sum）；容器启动时间=${started_at:-未知}，宿主机 .release.env 修改时间=${host_mtime:-未知}。若宿主机文件在容器启动后被替换过，请执行：docker restart $name"
    return 0
  fi
  if [[ "$container_digest" == "$host_digest" ]]; then
    doctor_ok "容器 $name 内读取到的 .release.env 与宿主机内容一致"
  else
    doctor_fail "容器 $name 仍绑定旧的 .release.env（内容摘要不一致）：宿主机=$host_digest 容器内=${container_digest}；这是 .release.env 被原子替换后 inode 变化导致的，请执行：docker restart $name"
  fi
}

doctor_check_runtime_placeholders() {
  printf '\n[5/6] runtime/*.env 待平台下发的凭据\n'
  shopt -s nullglob
  local files=("$deploy_dir"/runtime/*.env)
  shopt -u nullglob
  if ((${#files[@]} == 0)); then
    doctor_ok 'runtime/ 下暂无子系统配置文件'
    return 0
  fi
  local file pending=() key
  for file in "${files[@]}"; do
    [[ -f "$file" && ! -L "$file" ]] || continue
    pending=()
    mapfile -t pending < <(awk -F'=' '
      /^[[:space:]]*#/ {next}
      /^[[:space:]]*$/ {next}
      {
        key=$1
        value=substr($0, index($0,"=") + 1)
        gsub(/^[[:space:]]+|[[:space:]]+$/, "", key)
        gsub(/^[[:space:]]+|[[:space:]]+$/, "", value)
        if (value == "" || value == "PENDING_ONBOARDING" || value ~ /^REPLACE_WITH_/) print key "=" value
      }' "$file")
    if ((${#pending[@]} > 0)); then
      doctor_warn "$(basename -- "$file") 有 ${#pending[@]} 个键仍是占位值或空值："
      printf '        %s\n' "${pending[@]}"
    else
      doctor_ok "$(basename -- "$file") 未发现占位值或空值"
    fi
  done
  printf '        说明：这些值由基础平台在受控采用/重试时下发。出现 PENDING_ONBOARDING 不代表文件权限问题，\n'
  printf '        也不要手工填入猜测值；按 status <子系统> 的下一步操作即可。\n'
}

doctor_check_subsystem() {
  local component="$1"
  printf '\n[6/6] 子系统 %s\n' "$component"
  case "$component" in
    customer-opportunity|customer-portal|contract|project|settlement|data-analysis) ;;
    *)
      doctor_fail "不支持的子系统：${component}（可用：contract | project | settlement | data-analysis | customer-opportunity | customer-portal）"
      return 0
      ;;
  esac
  subsystem_metadata "$component"
  if [[ "$doctor_docker_available" != true ]]; then
    doctor_warn "未找到 docker 命令，无法读取 $component 的部署阶段"
    return 0
  fi
  local phase
  phase="$(subsystem_phase "$component" 2>/dev/null || true)"
  case "$phase" in
    NOT_IMPORTED)
      doctor_fail "$component 尚未 import；请执行：$0 import packages/$component-*-linux-amd64.tar.gz"
      ;;
    '')
      doctor_fail "无法读取 $component 的部署阶段；请确认 Docker 可用且已执行 $0 configure"
      ;;
    *)
      doctor_ok "$component 当前阶段：${phase}（生产目标：$subsystem_app_code/prod）"
      print_subsystem_next_action "$component" "$phase"
      ;;
  esac
}

doctor() {
  local component="${1:-}"
  doctor_failures=0
  doctor_warnings=0
  command -v docker >/dev/null 2>&1 && doctor_docker_available=true || doctor_docker_available=false
  printf '环境自检（只读）：%s\n' "$deploy_dir"
  printf '本命令不修改任何文件，也不会重启或创建任何容器。\n'
  doctor_check_config_files
  doctor_check_packages
  doctor_check_manifests
  doctor_check_provisioner_binding
  doctor_check_runtime_placeholders
  if [[ -n "$component" ]]; then
    doctor_check_subsystem "$component"
  fi
  printf '\n自检结果：%d 项失败，%d 项警告\n' "$doctor_failures" "$doctor_warnings"
  if ((doctor_failures > 0)); then
    printf '存在阻断问题：请按上面的建议处理后重新执行 doctor。\n' >&2
    return 1
  fi
  if ((doctor_warnings > 0)); then
    printf '未发现阻断问题，但有 %d 项警告。\n' "$doctor_warnings"
    return 0
  fi
  printf '未发现阻断问题。\n'
  return 0
}

status() {
  scope_report || return
  if [[ -n "${1:-}" ]]; then
    subsystem_status "$1"
    return
  fi
  printf '容器名称\t所属子系统\t镜像\t运行状态\t健康状态\t重启次数\t端口\n'
  docker ps -aq --filter 'name=uip' | while read -r container; do
    [[ -n "$container" ]] || continue
    docker inspect --format '{{.Name}}\t{{index .Config.Labels "com.basic-platform.application_code"}}\t{{.Config.Image}}\t{{.State.Status}}\t{{if .State.Health}}{{.State.Health.Status}}{{else}}-{{end}}\t{{.RestartCount}}\t{{json .NetworkSettings.Ports}}' "$container"
  done | sed 's#^/##'
}

wait_for_required_service_health() {
  local timeout="${UIP_HEALTH_WAIT_SECONDS:-180}" deadline service container state health restarts pending
  [[ "$timeout" =~ ^[0-9]+$ ]] && ((timeout >= 1 && timeout <= 3600)) || { printf '健康等待时间必须是 1 到 3600 之间的整数秒\n' >&2; return 1; }
  deadline=$((SECONDS + timeout))
  while :; do
    pending=''
    for service in "$@"; do
      container="$(compose ps -q "$service")" || return
      if [[ -z "$container" ]]; then pending+=" $service"; continue; fi
      state="$(docker inspect "$container" --format '{{.State.Status}}')" || return
      health="$(docker inspect "$container" --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}')" || return
      [[ "$state" == running && "$health" == healthy ]] || pending+=" $service"
    done
    if [[ -z "$pending" ]]; then
      for service in "$@"; do
        container="$(compose ps -q "$service")" || return
        state="$(docker inspect "$container" --format '{{.State.Status}}')" || return
        health="$(docker inspect "$container" --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}')" || return
        restarts="$(docker inspect "$container" --format '{{.RestartCount}}')" || return
        [[ "$state" == running && "$health" == healthy ]] || {
          printf '核心服务在生成验收证据前状态发生变化：service=%s status=%s health=%s\n' \
            "$service" "${state:-unknown}" "${health:-unknown}" >&2
          return 1
        }
        [[ "$restarts" =~ ^[0-9]+$ ]] || {
          printf '无法读取核心服务 %s 的 RestartCount\n' "$service" >&2
          return 1
        }
        printf '[验收证据] service=%s container=%s status=running health=healthy restarts=%s\n' \
          "$service" "$container" "$restarts"
      done
      printf '必要服务均已健康。\n'
      return 0
    fi
    if ((SECONDS >= deadline)); then
      printf '等待必要服务健康超过 %s 秒：%s\n' "$timeout" "$pending" >&2
      for service in "$@"; do compose logs --tail 80 "$service" >&2 || true; done
      return 1
    fi
    sleep 2
  done
}

verify_current_services() {
  local stability_seconds="${UIP_VERIFY_STABILITY_SECONDS:-10}"
  local service container state health restarts current_container current_state current_health current_restarts index
  local -a stable_services=()
  local -a stable_containers=()
  local -a stable_restarts=()

  [[ "$stability_seconds" =~ ^[0-9]+$ ]] && ((stability_seconds >= 1 && stability_seconds <= 60)) || {
    printf '无探针 Worker 稳定观察时间必须是 1 到 60 之间的整数秒\n' >&2
    return 1
  }

  for service in "$@"; do
    container="$(compose ps -q "$service" 2>/dev/null || true)"
    if [[ -z "$container" ]]; then
      printf '[验收失败] service=%s container=missing；必需长期服务未运行\n' "$service" >&2
      return 1
    fi
    state="$(docker inspect "$container" --format '{{.State.Status}}' 2>/dev/null || true)"
    health="$(docker inspect "$container" --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' 2>/dev/null || true)"
    restarts="$(docker inspect "$container" --format '{{.RestartCount}}' 2>/dev/null || true)"
    if [[ "$state" != running ]]; then
      printf '[验收失败] service=%s container=%s status=%s health=%s restarts=%s\n' \
        "$service" "$container" "${state:-unknown}" "${health:-unknown}" "${restarts:-unknown}" >&2
      return 1
    fi
    [[ "$restarts" =~ ^[0-9]+$ ]] || {
      printf '[验收失败] service=%s container=%s 无法读取 RestartCount\n' "$service" "$container" >&2
      return 1
    }
    case "$health" in
      healthy)
        printf '[验收证据] service=%s container=%s status=running health=healthy restarts=%s\n' \
          "$service" "$container" "$restarts"
        ;;
      none)
        stable_services+=("$service")
        stable_containers+=("$container")
        stable_restarts+=("$restarts")
        ;;
      *)
        printf '[验收失败] service=%s container=%s status=running health=%s restarts=%s\n' \
          "$service" "$container" "${health:-unknown}" "$restarts" >&2
        return 1
        ;;
    esac
  done

  if ((${#stable_services[@]} > 0)); then
    printf '观察 %d 个无 healthcheck 的长期 Worker %s 秒，校验容器不替换且 RestartCount 不增加。\n' \
      "${#stable_services[@]}" "$stability_seconds"
    sleep "$stability_seconds"
    for ((index=0; index<${#stable_services[@]}; index++)); do
      service="${stable_services[$index]}"
      current_container="$(compose ps -q "$service" 2>/dev/null || true)"
      current_state="$(docker inspect "$current_container" --format '{{.State.Status}}' 2>/dev/null || true)"
      current_health="$(docker inspect "$current_container" --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' 2>/dev/null || true)"
      current_restarts="$(docker inspect "$current_container" --format '{{.RestartCount}}' 2>/dev/null || true)"
      if [[ "$current_container" != "${stable_containers[$index]}" || "$current_state" != running || "$current_health" != none || \
            "$current_restarts" != "${stable_restarts[$index]}" ]]; then
        printf '[验收失败] service=%s container=%s->%s status=%s health=%s restarts=%s->%s stability=%ss\n' \
          "$service" "${stable_containers[$index]}" "${current_container:-missing}" "${current_state:-unknown}" \
          "${current_health:-unknown}" "${stable_restarts[$index]}" "${current_restarts:-unknown}" "$stability_seconds" >&2
        return 1
      fi
      printf '[验收证据] service=%s container=%s status=running health=none restarts=%s stability=%ss\n' \
        "$service" "$current_container" "$current_restarts" "$stability_seconds"
    done
  fi
}

verify_subsystem_current_health() {
  local component="$1" flag
  local -a services=()
  subsystem_metadata "$component"
  runtime_ready "$subsystem_runtime" || {
    printf '错误：%s 当前 runtime 配置不完整，不能使用历史 VERIFIED 结果通过验收。\n' "$component" >&2
    return 1
  }
  if docker container inspect "$subsystem_candidate" >/dev/null 2>&1; then
    printf '错误：%s 仍存在未完成采用的候选容器 %s。\n' "$component" "$subsystem_candidate" >&2
    return 1
  fi

  # 只列出长期服务；migrate/init/catalog-sync 是受控一次性任务，不能按常驻容器验收。
  case "$component" in
    contract)
      services=(contract-mysql contract-api)
      [[ "$(env_get "$subsystem_runtime" CRM_REFERENCE_ENABLED)" != true ]] || services+=(customer-api)
      [[ "$(env_get "$subsystem_runtime" PROJECT_INTEGRATION_ENABLED)" != true ]] || services+=(project-api)
      ;;
    project) services=(project-mysql project-api project-sla-notifier) ;;
    customer-opportunity)
      services=(customer-mysql customer-api \
        customer-opportunity-alert-worker customer-owner-notification-worker \
        customer-presale-alert-worker customer-presale-assignment-notification-worker \
        customer-presale-progress-notification-worker customer-notification-delivery-worker \
        customer-presale-worker)
      for flag in CONTRACT_VERIFICATION_ENABLED; do
        [[ "$(env_get "$subsystem_runtime" "$flag")" != true ]] || services+=(contract-api)
      done
      ;;
    customer-portal)
      services=(portal-mysql portal-api)
      if portal_compensation_ready; then
        services+=(portal-invite-compensation-worker customer-mysql customer-api)
        printf '[验收证据] Portal 邀请补偿凭据已启用，纳入补偿 Worker 及 CRM 跨系统依赖。\n'
      else
        printf '[验收证据] Portal 邀请补偿未满足既有启用条件，不要求补偿 Worker 常驻。\n'
      fi
      ;;
    settlement) services=(settlement-mysql settlement-api settlement-worker) ;;
    data-analysis)
      services=(data-analysis-mysql data-analysis-api data-analysis-aggregation-worker \
        data-analysis-alert-worker data-analysis-metabase contract-api project-api)
      ;;
    *) die "不支持的子系统：$component" ;;
  esac
  verify_current_services "${services[@]}"
}

verify() {
  require_initialized
  local component="${1:-}"
  [[ -z "$component" ]] || scope_require "$component" || return
  wait_for_required_service_health \
    platform-api platform-worker file-gateway keycloak temporal subsystem-provisioner frontend || return
  compose ps
  local api_port
  api_port="$(env_get "$runtime_file" PLATFORM_API_PORT)"; api_port="${api_port:-18080}"
  curl --fail --silent --show-error --connect-timeout 5 --max-time 20 "http://127.0.0.1:$api_port/readyz" >/dev/null
  curl --fail --silent --show-error --connect-timeout 5 --max-time 20 "$PUBLIC_PLATFORM_ORIGIN/healthz" >/dev/null
  curl --fail --silent --show-error --connect-timeout 5 --max-time 20 "$PUBLIC_KEYCLOAK_ISSUER/.well-known/openid-configuration" >/dev/null
  compose exec -T platform-api wget -qO- http://file-gateway:8086/readyz >/dev/null || die 'platform-api 容器无法访问 http://file-gateway:8086/readyz'
  gateway_proxy_status="$(curl --silent --show-error --connect-timeout 5 --max-time 20 --output /dev/null --write-out '%{http_code}' "$PUBLIC_PLATFORM_ORIGIN/file-gateway/api/v2/upload-sessions")"
  [[ "$gateway_proxy_status" == 405 || "$gateway_proxy_status" == 401 ]] || die "File Gateway 代理未到达受保护 API（HTTP ${gateway_proxy_status}）"
  echo 'File Gateway 代理到达受保护 API；匿名请求被正确拒绝。'
  echo '基础平台核心服务健康检查已通过；不代表任何业务子系统已部署，也不代表外部客户端/安全组已验证。'
  if [[ -n "$component" ]]; then
    verify_subsystem_current_health "$component" || return
    printf '%s/prod 子系统当前数据库、API、Worker 与跨系统依赖验收已通过。\n' "$subsystem_app_code"
  else
    echo '如需验收业务子系统，请执行：deploy.sh verify <component>。'
  fi
}

logs() {
  local component="${1:?component required}"
  docker ps -a --filter "label=com.basic-platform.application_code=$component" --format '{{.Names}}' | while read -r name; do
    [[ -n "$name" ]] || continue
    printf '\n===== %s =====\n' "$name"
    docker logs --tail 200 "$name"
  done
}

backup_all() {
  require_initialized
  "$script_dir/backup-all.sh"
}

disable_component() (
  require_initialized
  need flock
  [[ ! -L "$deploy_dir/runtime/.deploy.lock" ]] || die '部署锁不能是符号链接'
  exec 7>"$deploy_dir/runtime/.deploy.lock"
  flock -w 30 7 || die '平台接入或其他发布正在进行，请稍后停用'
  scope_disable "${1:?请指定子系统}"
)

restore_menu() {
  local service backup answer
  read -r -p 'MySQL 服务名称：' service
  read -r -p '备份 .sql.gz 文件路径：' backup
  "$script_dir/restore-mysql.sh" --service "$service" --backup "$backup" --verify-only
  read -r -p '恢复会覆盖数据库。确认继续请输入 RESTORE_MYSQL_SERVICE：' answer
  [[ "$answer" == RESTORE_MYSQL_SERVICE ]] || { echo '已取消恢复'; return 0; }
  "$script_dir/restore-mysql.sh" --service "$service" --backup "$backup" --confirm RESTORE_MYSQL_SERVICE
}

menu() {
  while true; do
    printf '\n统一身份认证平台离线部署\n1. 初始化部署配置\n2. 导入镜像包\n3. 部署基础平台\n4. 部署统一前端\n5. 准备子系统供平台探测\n6. 完成已接入子系统部署\n7. 升级已部署模块\n8. 查看容器状态\n9. 查看模块日志\n10. 执行健康检查\n11. 备份\n12. 恢复\n13. 环境自检（doctor，只读）\n14. 自动安装 packages/ 中的镜像包\n15. 成对重载子系统控制面\n0. 退出\n'
    read -r -p '请选择操作：' choice
    case "$choice" in
      1) configure ;;
      2) read -r -p '镜像包路径：' path; import_package "$path" ;;
      3) deploy_component platform ;;
      4) deploy_component frontend ;;
      5) read -r -p '子系统参数：' name; prepare_subsystem "$name" ;;
      6) read -r -p '子系统参数：' name; continue_subsystem "$name" ;;
      7) read -r -p '组件参数：' name; stage_upgrade "$name" ;;
      8) read -r -p '子系统参数（直接回车查看全部容器）：' name; status "$name" ;;
      9) read -r -p '应用编码：' name; logs "$name" ;;
      10) verify ;;
      11) backup_all ;;
      12) restore_menu ;;
      13) read -r -p '子系统参数（直接回车检查全部）：' name; doctor "$name" || true ;;
      14) auto_install_packages ;;
      15) reload_control_plane ;;
      0) return ;;
      *) echo '选择无效，请重新输入' >&2 ;;
    esac
  done
}

[[ "${BASH_SOURCE[0]}" == "$0" ]] || return 0
command="${1:-menu}"
if [[ -e "$control_plane_reload_marker" || -L "$control_plane_reload_marker" ]]; then
  case "$command" in
    reload-control-plane|doctor|status|logs|help|--help|-h) ;;
    *)
      die "检测到部署资产已更新但子系统控制面尚未成对重载：$control_plane_reload_marker
为防止 platform-api 与 subsystem-provisioner 使用不同清单摘要，当前仅允许只读诊断。
请先执行：$0 reload-control-plane"
      ;;
  esac
fi
case "$command" in
  menu) menu ;;
  configure) shift; configure "$@" ;;
  install) auto_install_packages ;;
  license-renew) renew_delivery_license "${2:-}" ;;
  start) start_enabled ;;
  resume) require_initialized; start_registered "${2:?请指定子系统}" ;;
  disable) disable_component "${2:?请指定子系统}" ;;
  import) import_package "${2:-}" ;;
  deploy) deploy_component "${2:-}" ;;
  prepare) prepare_subsystem "${2:-}" ;;
  continue) continue_subsystem "${2:-}" ;;
  upgrade) stage_upgrade "${2:-}" ;;
  reload-control-plane) reload_control_plane ;;
  doctor) doctor "${2:-}" ;;
  status) status "${2:-}" ;;
  logs) logs "${2:-}" ;;
  verify) verify "${2:-}" ;;
  backup) backup_all ;;
  restore) shift; "$script_dir/restore-mysql.sh" "$@" ;;
  help|--help|-h) usage ;;
  *) die "未知命令：${command}（执行 $0 help 查看可用子命令）" ;;
esac
