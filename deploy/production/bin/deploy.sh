#!/usr/bin/env bash
# =============================================================================
# 统一离线部署入口（部署目录内使用）
#
#   运行位置：部署目录，即本文件所在 bin/ 的上一级同时存在 compose.yaml、
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
compose_file="$deploy_dir/compose.yaml"

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
  import <镜像包路径>           导入镜像包，校验摘要并登记不可变 digest
  deploy platform               发布基础平台（含数据库迁移与首个管理员初始化）
  deploy frontend               发布统一前端
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
  ensure_application_network
  public_transport_prepare "$deploy_dir" "$runtime_file"
  local args=(--project-directory "$deploy_dir" --file "$compose_file" --file "$deploy_dir/compose.observability.yaml")
  public_transport_compose_args args
  docker compose "${args[@]}" --env-file "$runtime_file" --env-file "$release_file" "$@"
}

# 本脚本必须与同一资产包中的 compose.yaml、offline-configure.sh、public-transport.sh
# 放在一起才能运行。交付目录顶层的 server_up.sh 只是本脚本的中文副本，单独执行会在这里
# 被拦截并给出可操作的提示，而不是抛出难以理解的 source 错误。
# 这里只列 source 阶段真正依赖的文件；deploy-service.sh 仅 deploy/upgrade 需要，故意
# 不在此校验，以免破坏只复制部分脚本的 fixture 测试。
missing_siblings=()
for sibling in offline-configure.sh public-transport.sh; do
  [[ -f "$script_dir/$sibling" ]] || missing_siblings+=("$script_dir/$sibling")
done
[[ -f "$deploy_dir/compose.yaml" ]] || missing_siblings+=("$deploy_dir/compose.yaml")
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

ensure_registry() {
  docker image inspect registry:2.8.3 >/dev/null 2>&1 || die "请先导入公共基础设施镜像包"
  if ! docker container inspect uip-offline-registry >/dev/null 2>&1; then
    docker run -d --name uip-offline-registry --restart unless-stopped -p 127.0.0.1:5000:5000 -v uip-offline-registry-data:/var/lib/registry registry:2.8.3 >/dev/null
  elif [[ "$(docker inspect -f '{{.State.Running}}' uip-offline-registry)" != true ]]; then
    docker start uip-offline-registry >/dev/null
  fi
}

manifest_value() {
  local file="$1" key="$2"
  awk -F= -v key="$key" '$1 == key && $0 ~ /^[A-Z_]+=[A-Za-z0-9.,_:\/-]+$/ {sub(/^[^=]*=/, ""); print; exit}' "$file"
}

import_package() (
  local archive="${1:?package path required}" temporary manifest component version platform images image_ids expected_images tag local_name pushed digest record index actual_id expected_digest
  need docker; need tar; need gzip; need sha256sum; need awk; need curl
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
  (cd "$(dirname -- "$archive")" && sha256sum -c "$(basename -- "$archive.sha256")")
  temporary="$(mktemp -d "${TMPDIR:-/tmp}/uip-import.XXXXXX")"
  trap 'rm -r -- "$temporary"' EXIT
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
  image_ids="$(manifest_value "$manifest" IMAGE_IDS)"
  [[ -n "$component" && -n "$version" && "$platform" == linux/amd64 && -n "$images" && -n "$image_ids" ]] || die "镜像包元数据不正确"
  case "$component" in
    common) expected_images='mysql:8.4,quay.io/keycloak/keycloak:26.2,temporalio/auto-setup:1.29.7,metabase/metabase:v0.53.7,prom/prometheus:v3.5.0,prom/node-exporter:v1.9.1,registry:2.8.3' ;;
    platform) expected_images="uip-package/platform-backend:$version" ;;
    frontend) expected_images="uip-package/frontend:$version" ;;
    customer-opportunity) expected_images="uip-package/customer-opportunity-backend:$version" ;;
    customer-portal) expected_images="uip-package/customer-portal-backend:$version" ;;
    contract) expected_images="uip-package/contract-backend:$version" ;;
    project) expected_images="uip-package/project-backend:$version" ;;
    settlement) expected_images="uip-package/settlement-backend:$version" ;;
    data-analysis) expected_images="uip-package/data-analysis-backend:$version" ;;
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
    [[ "$actual_id" == "${expected_ids[$index]}" ]] || die "导入后的镜像 ID 与清单不一致：$tag"
  done
  if [[ "$component" != common ]]; then
    ensure_registry
    [[ "${#image_tags[@]}" -eq 1 ]] || die "每个应用镜像包必须且只能包含一个镜像"
    tag="${image_tags[0]}"; local_name="$registry_address/uip/${component}:${version}"
    docker tag "$tag" "$local_name"
    docker push "$local_name" >/dev/null
    pushed="$(docker image inspect "$local_name" --format '{{index .RepoDigests 0}}')"
    [[ "$pushed" == "$registry_address/"*"@sha256:"* ]] || die "本地镜像仓库没有返回不可变摘要"
    digest="${pushed#*@sha256:}"
  else
    pushed="$images"; digest="-"
  fi
  record="$manifest_dir/${component}-${version}.imported"
  printf 'COMPONENT=%s\nVERSION=%s\nIMAGE_REF=%s\nSOURCE_IMAGE_IDS=%s\nIMAGE_DIGEST=%s\nPACKAGE_SHA256=%s\n' "$component" "$version" "$pushed" "$image_ids" "$digest" "$(sha256sum "$archive" | awk '{print $1}')" > "$record"
  chmod 600 "$record"
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
  local component="$1" record
  record="$(find "$manifest_dir" -maxdepth 1 -type f -name "${component}-*.imported" -print | sort | tail -n 1)"
  [[ -n "$record" ]] || die "没有找到已导入的 $component 镜像包；请先执行：$0 import packages/${component}-*-linux-amd64.tar.gz"
  printf '%s\n' "$record"
}

latest_image() {
  env_get "$(latest_record "$1")" IMAGE_REF
}

deploy_component() {
  local component="${1:?component required}" image bootstrap_password bootstrap_display bootstrap_account
  require_initialized
  image="$(latest_image "$component")"
  case "$component" in
    platform)
      "$script_dir/deploy-service.sh" platform "$image"
      bootstrap_password="$(env_get "$runtime_file" IAM_BOOTSTRAP_ADMIN_PASSWORD)"
      bootstrap_display="$(env_get "$runtime_file" IAM_BOOTSTRAP_ADMIN_DISPLAY_NAME)"
      bootstrap_account="$(env_get "$runtime_file" IAM_BOOTSTRAP_ADMIN_ACCOUNT_NAME)"
      [[ -n "$bootstrap_password" && -n "$bootstrap_display" && -n "$bootstrap_account" ]] || die "首个平台管理员配置不完整"
      printf '%s\n' "$bootstrap_password" | compose --profile release run -T --rm platform-migrate ./bootstrap-admin \
        --display-name "$bootstrap_display" --account-name "$bootstrap_account" --password-stdin
      echo '基础平台管理员已初始化或已经存在；受保护的初始凭据仍保存在权限为 0600 的运行时配置文件中，请首次登录后及时修改。'
      if [[ "$(env_get "$runtime_file" OFFLINE_MONITORING_ENABLED)" == true ]]; then
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
refresh_subsystem_provisioner() {
  local container
  container="$(docker ps -q --filter 'label=com.docker.compose.service=subsystem-provisioner' 2>/dev/null || true)"
  if [[ -z "$container" ]]; then
    container="$(docker ps -q --filter 'name=uip-subsystem-provisioner' 2>/dev/null || true)"
  fi
  [[ -n "$container" ]] || return 0
  docker restart "$container" >/dev/null 2>&1 ||
    die '子系统部署 Agent 重启失败；请手动执行 docker restart uip-subsystem-provisioner 后重试'
  printf '已重启 subsystem-provisioner，使其重新绑定更新后的 .release.env\n'
}

stage_upgrade() {
  local component="${1:?component required}" image key
  require_initialized
  image="$(latest_image "$component")"
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
  env_set "$release_file" "$key" "$image"
  if [[ "$component" == data-analysis ]]; then
    env_set "$release_file" DATA_ANALYSIS_DASHBOARD_API_IMAGE "$image"
    env_set "$release_file" DATA_ANALYSIS_AGGREGATION_WORKER_IMAGE "$image"
    env_set "$release_file" DATA_ANALYSIS_ALERT_WORKER_IMAGE "$image"
    env_set "$release_file" DATA_ANALYSIS_MIGRATE_IMAGE "$image"
  fi
  refresh_subsystem_provisioner
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
  subsystem_metadata "$component"
  need flock
  [[ ! -L "$deploy_dir/runtime/.deploy.lock" ]] || die '部署锁不能是符号链接'
  exec 7>"$deploy_dir/runtime/.deploy.lock"
  flock -w 5 7 || die '平台 Agent 或其他发布任务正在运行；请等待其结束后执行 status'
  profile="$deploy_dir/subsystems.d/${subsystem_app_code}-prod.yaml"
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
      die "发现标签不匹配的候选容器 $subsystem_candidate；请人工核对，脚本不会覆盖"
      ;;
  esac
  image="$(latest_image "$component")"
  version="$(env_get "$(latest_record "$component")" VERSION)"
  env_set "$release_file" "$subsystem_key" "$image"
  if [[ "$component" == data-analysis ]]; then
    env_set "$release_file" DATA_ANALYSIS_DASHBOARD_API_IMAGE "$image"
    env_set "$release_file" DATA_ANALYSIS_AGGREGATION_WORKER_IMAGE "$image"
    env_set "$release_file" DATA_ANALYSIS_ALERT_WORKER_IMAGE "$image"
    env_set "$release_file" DATA_ANALYSIS_MIGRATE_IMAGE "$image"
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
      doctor_fail "$label 不存在：$file；请先执行：$0 configure"
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
      doctor_fail "缺少校验伴随文件：$sidecar；请执行：(cd $(dirname -- "$archive") && sha256sum $base > $base.sha256)"
      continue
    fi
    if (cd "$(dirname -- "$archive")" && sha256sum -c "$(basename -- "$sidecar")" >/dev/null 2>&1); then
      doctor_ok "$base 摘要校验通过"
    else
      doctor_fail "$base 摘要校验失败；文件可能传输损坏，请重新复制该包及其 .sha256 后重试"
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
  local record component image_ref key release_value
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
      doctor_fail "$(basename -- "$record") 的 IMAGE_REF 不是不可变 @sha256: 引用：$image_ref；请重新 import 对应镜像包"
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
      release_value="$(env_get "$release_file" "$key")"
      if [[ -z "$release_value" ]]; then
        doctor_fail "$key 在 .release.env 中未设置；请重新执行 prepare/deploy 使其写入不可变 digest"
        continue
      fi
      if doctor_is_placeholder_image "$release_value"; then
        key_placeholder=true
        continue
      fi
      if [[ "$release_value" != "$image_ref" ]]; then
        doctor_fail "$key 与 $(basename -- "$record") 的 IMAGE_REF 不一致：$release_value != $image_ref；请重新 prepare/升级使两者一致"
      else
        key_checked=true
      fi
    done
    if [[ "$key_placeholder" == true ]]; then
      if [[ "$component" == platform || "$component" == frontend ]]; then
        doctor_warn "$component 已 import，但 ${keys[*]} 仍是占位值；下一步：$0 deploy $component"
      else
        doctor_warn "$component 已 import，但 ${keys[*]} 仍是占位值；下一步：$0 prepare $component（import 只登记 manifests/，写入 .release.env 的是 prepare）"
      fi
    elif [[ "$key_checked" == true ]]; then
      doctor_ok "$component：${keys[*]} 已登记不可变 digest，与导入记录一致"
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
    doctor_warn "无法读取容器 $name 内的 $path（镜像可能缺少 cat/sha256sum）；容器启动时间=${started_at:-未知}，宿主机 .release.env 修改时间=${host_mtime:-未知}。若宿主机文件在容器启动后被替换过，请执行：docker restart $name"
    return 0
  fi
  if [[ "$container_digest" == "$host_digest" ]]; then
    doctor_ok "容器 $name 内读取到的 .release.env 与宿主机内容一致"
  else
    doctor_fail "容器 $name 仍绑定旧的 .release.env（内容摘要不一致）：宿主机=$host_digest 容器内=$container_digest；这是 .release.env 被原子替换后 inode 变化导致的，请执行：docker restart $name"
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
      doctor_fail "不支持的子系统：$component（可用：contract | project | settlement | data-analysis | customer-opportunity | customer-portal）"
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
      doctor_ok "$component 当前阶段：$phase（生产目标：$subsystem_app_code/prod）"
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

verify() {
  require_initialized
  local component="${1:-}" phase
  compose ps
  local api_port
  api_port="$(env_get "$runtime_file" PLATFORM_API_PORT)"; api_port="${api_port:-18080}"
  curl --fail --silent --show-error --connect-timeout 5 --max-time 20 "http://127.0.0.1:$api_port/readyz" >/dev/null
  curl --fail --silent --show-error --connect-timeout 5 --max-time 20 "$PUBLIC_PLATFORM_ORIGIN/healthz" >/dev/null
  curl --fail --silent --show-error --connect-timeout 5 --max-time 20 "$PUBLIC_KEYCLOAK_ISSUER/.well-known/openid-configuration" >/dev/null
  echo '基础平台核心服务健康检查已通过；不代表任何业务子系统已部署，也不代表外部客户端/安全组已验证。'
  if [[ -n "$component" ]]; then
    subsystem_metadata "$component"
    phase="$(subsystem_phase "$component")"
    [[ "$phase" == VERIFIED ]] || {
      printf '错误：%s 尚未完成，当前阶段：%s。\n' "$component" "$phase" >&2
      print_subsystem_next_action "$component" "$phase" >&2
      return 1
    }
    printf '%s/prod 子系统部署验收已通过。\n' "$subsystem_app_code"
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
    printf '\n统一身份认证平台离线部署\n1. 初始化部署配置\n2. 导入镜像包\n3. 部署基础平台\n4. 部署统一前端\n5. 准备子系统供平台探测\n6. 完成已接入子系统部署\n7. 升级已部署模块\n8. 查看容器状态\n9. 查看模块日志\n10. 执行健康检查\n11. 备份\n12. 恢复\n13. 环境自检（doctor，只读）\n0. 退出\n'
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
      0) return ;;
      *) echo '选择无效，请重新输入' >&2 ;;
    esac
  done
}

[[ "${BASH_SOURCE[0]}" == "$0" ]] || return 0
command="${1:-menu}"
case "$command" in
  menu) menu ;;
  configure) shift; configure "$@" ;;
  import) import_package "${2:-}" ;;
  deploy) deploy_component "${2:-}" ;;
  prepare) prepare_subsystem "${2:-}" ;;
  continue) continue_subsystem "${2:-}" ;;
  upgrade) stage_upgrade "${2:-}" ;;
  doctor) doctor "${2:-}" ;;
  status) status "${2:-}" ;;
  logs) logs "${2:-}" ;;
  verify) verify "${2:-}" ;;
  backup) backup_all ;;
  restore) shift; "$script_dir/restore-mysql.sh" "$@" ;;
  help|--help|-h) usage ;;
  *) die "未知命令：$command（执行 $0 help 查看可用子命令）" ;;
esac
