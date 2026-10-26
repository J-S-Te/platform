#!/usr/bin/env bash
# 统一 Docker 编排入口：一个统一前端，以及彼此隔离的平台、合同、CRM 和客户门户后端。
set -Eeuo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
project_root="$(cd "${script_dir}/.." && pwd)"
workspace_root="$(cd "${project_root}/.." && pwd)"
contract_root="${workspace_root}/contract_management"
customer_root="${workspace_root}/customer_and_opportunity"
compose_file="${project_root}/compose.local.yaml"
platform_template_file="${project_root}/docker/.env.local.example"
env_file="${project_root}/docker/.env.local"
contract_template_file="${contract_root}/.env.example"
contract_env_file="${contract_root}/.env.local"
customer_template_file="${project_root}/docker/.env.customer.local.example"
customer_env_file="${project_root}/docker/.env.customer.local"
portal_template_file="${project_root}/docker/.env.portal.local.example"
portal_env_file="${project_root}/docker/.env.portal.local"
project_management_root="${workspace_root}/project_management"
project_template_file="${project_management_root}/.env.example"
project_env_file="${project_management_root}/.env.local"
data_analysis_root="${workspace_root}/data_analysis"
data_analysis_template_file="${data_analysis_root}/.env.example"
data_analysis_env_file="${data_analysis_root}/.env.local"
settlement_root="${workspace_root}/Settlement"
settlement_template_file="${settlement_root}/.env.local.example"
settlement_env_file="${settlement_root}/.env.local"
presale_worker_env_file="${customer_root}/.env.presale-worker"
lan_override_file="${project_root}/docker/.env.lan"
customer_lan_override_file="${project_root}/docker/.env.customer.lan"
lan_placeholder_file="${project_root}/docker/.env.lan.disabled"
compose_project="basic-platform-local"
command_name="up"
force_build=false
subsystem_environment="${LOCAL_SUBSYSTEM_ENVIRONMENT:-dev}"
admin_display_name="${BASIC_PLATFORM_ADMIN_DISPLAY_NAME:-}"
admin_account_name="${BASIC_PLATFORM_ADMIN_ACCOUNT_NAME:-}"
admin_password="${BASIC_PLATFORM_ADMIN_PASSWORD:-}"
admin_password_stdin=false
frontend_public_origin=""
# 用于 Compose 首次解析的本地 CRM 目录基线。customer-api 真正启动前，
# sync_crm_authorization_catalog 会从当前镜像读取并导出实际哈希，因此这里
# 不替代运行时目录校验；它只避免 .env 模板占位符阻断初始化流程。
local_crm_role_config_hash="sha256:77443efe31deec9ade8836e826b7240edfc377e953b9f5722e37dace011db0bb"
customer_notification_worker_services=(
	customer-opportunity-alert-worker
	customer-owner-notification-worker
	customer-presale-assignment-notification-worker
	customer-presale-progress-notification-worker
	customer-notification-delivery-worker
	contract-transfer-worker
)

log() { printf '[docker-local] %s\n' "$*"; }
fail() { printf '[docker-local] ERROR: %s\n' "$*" >&2; exit 1; }

source "${script_dir}/local-project-background.sh"

# 职责拆分后的函数库：仅定义函数与只读常量，加载零副作用；路径基于 script_dir 解析，
# 按依赖顺序加载：env-templates（env 行读写）→ runtime-validation（依赖 env_value）→
# access-mode（依赖 log/fail 与局域网覆盖文件路径）→ compose（依赖 configured 探测）→
# base-images（依赖 log/fail）。
source "${script_dir}/lib/env-templates.sh"
source "${script_dir}/lib/runtime-validation.sh"
source "${script_dir}/lib/access-mode.sh"
source "${script_dir}/lib/compose.sh"
source "${script_dir}/lib/base-images.sh"

usage() {
    cat <<USAGE
用法：
  bash scripts/docker-local.sh up [选项]
  bash scripts/docker-local.sh down [--volumes]
  bash scripts/docker-local.sh stop
  bash scripts/docker-local.sh restart [选项]
  bash scripts/docker-local.sh ps
  bash scripts/docker-local.sh logs [选项] [服务名...]
  bash scripts/docker-local.sh config
  bash scripts/docker-local.sh verify
  bash scripts/docker-local.sh prepare-license --application CODE --operator NAME [--approve] [--replace]
  bash scripts/docker-local.sh refresh-approvals --operator NAME [--applications a,b] [--approve]
                           为全部已登记应用生成批准候选并打印核对单；--approve 才统一发布
  bash scripts/docker-local.sh recover-license-pins [--apply]
                           校验完整有效批准并预览固定文件恢复；--apply 先备份再原子恢复
  bash scripts/docker-local.sh dev-up            一键启动：命令行能做的全做完，需要人工决定处停下给出指令
  bash scripts/docker-local.sh dev-approve       发布运行批准（必须显式 --approve）
  bash scripts/docker-local.sh status            只读状态总览：卡在哪里、下一步做什么
  bash scripts/docker-local.sh prepare-discovery 为六个子系统创建探测候选（不启动业务进程，失败不阻断）
  bash scripts/docker-local.sh bootstrap-admin  为已运行的控制面追加首个超级管理员（已存在则跳过）
  bash scripts/docker-local.sh build-images     只准备候选镜像：不检查旧运行镜像、不迁移、不启动服务、不改写固定记录
  bash scripts/docker-local.sh up --core-only   只恢复控制面：不执行业务迁移与业务校验，不启动业务 API/Worker
  bash scripts/docker-local.sh refresh-api
  bash scripts/docker-local.sh refresh-frontend
  bash scripts/docker-local.sh refresh-contract-api
  bash scripts/docker-local.sh refresh-customer-api
  bash scripts/docker-local.sh refresh-portal-api
  bash scripts/docker-local.sh refresh-project-api
  bash scripts/docker-local.sh start-presale-worker [--presale-worker-env-file PATH]
  bash scripts/docker-local.sh start-presale-alert-worker

up/restart 选项：
  --build                         重新构建镜像并强制重建使用新镜像的业务容器
  --pull                          兼容选项；启动前默认会串行拉取缺失的基础镜像
  --admin-display-name NAME       首次初始化超级管理员显示名称
  --admin-account-name NAME       首次初始化超级管理员账号
  --admin-password PASSWORD       首次初始化密码；仅为兼容旧调用保留，不建议使用
  --admin-password-stdin          从标准输入读取首次初始化密码；适合 CI Secret
  --env-file PATH                 基础平台环境文件（默认 platform/docker/.env.local）
  --contract-env-file PATH        合同后端环境文件（默认 contract_management/.env.local）
  --customer-env-file PATH        客户与商机后端环境文件（默认 platform/docker/.env.customer.local）
  --portal-env-file PATH          客户自助门户环境文件（默认 platform/docker/.env.portal.local）
  --project-env-file PATH         项目管理系统环境文件（默认 project_management/.env.local）
  --data-analysis-env-file PATH  数据看板与统计分析环境文件（默认 data_analysis/.env.local）
  --settlement-env-file PATH      结算与开票运行环境文件（默认 Settlement/.env.local）
  --subsystem-environment ENV     本地子系统注册环境：dev/test/staging/prod（默认 dev）
  --presale-worker-env-file PATH  售前投递 Worker 环境文件（默认 customer_and_opportunity/.env.presale-worker）
  -h, --help                      显示帮助

日志取证选项：
  --since DURATION                仅显示指定时间范围内的日志，例如 10m、1h
  --tail COUNT                    每个服务显示的末尾行数（或 all），例如 200
  --no-follow                     一次性输出日志后退出（默认持续跟踪）

示例：
  bash scripts/docker-local.sh logs api
  bash scripts/docker-local.sh logs --since 10m --tail 200 --no-follow api
  bash scripts/docker-local.sh logs api subsystem-provisioner --since 30m --no-follow

定向更新：
  refresh-api           只重建基础平台后端镜像，执行基础平台迁移，并重启 api/受控 provisioner
  refresh-frontend      只重建并重启统一 frontend；六个前端模块同时更新
  refresh-contract-api  重建合同管理后端镜像，执行合同迁移，并重启独立 API 与 Worker
  refresh-customer-api  重建客户与商机管理后端、执行 CRM 迁移，并刷新统一前端网关
  refresh-portal-api    重建客户自助门户后端、执行 Portal 迁移；仅在已完成应用接入后启动
  refresh-project-api   重建项目管理系统后端、执行项目迁移；仅在已完成应用接入后启动
  refresh-data-analysis-api  重建数据看板后端（dashboard-api + 聚合 Worker）、执行聚合库迁移；仅在已完成应用接入后启动
  start-presale-worker  构建并启动售前投递 Worker，等待数据库出现真实新鲜心跳（up 已自动执行）
  start-presale-alert-worker  构建并启动售前预警扫描 Worker（up 已自动执行）

  各定向更新都不会删除或重建 Application、Environment、LoginTarget、OAuth Client，
  因此不会影响已经完成的子系统统一登录接入。

应用容器：
  frontend      基础平台 + 合同管理 + 客户与商机管理 + 结算与开票 + 客户自助门户前端（宿主机仅发布 8081）
  api           基础平台 API + Worker
  contract-api     合同管理 API
  contract-worker  独立合同 Temporal Worker
  settlement-api 结算与开票 API
  settlement-worker 结算 Outbox 与催收 Worker
  customer-api  客户与商机管理 API
  portal-api    客户自助门户 API（完成 customer_portal/dev 接入后启用）
  project-api   项目管理系统 API（后台由 sla-notifier 独立运行，接入后启用）
  dashboard-api 数据看板与统计分析 API（嵌入桥）+ aggregation-worker + alert-worker + Metabase（未接入时先创建探测候选容器）
  presale-worker 售前申请审批/PMS 投递 Worker（up 默认启动）

首次启动若数据库中尚未存在超级管理员，必须提供三个管理员参数，或设置：
  BASIC_PLATFORM_ADMIN_DISPLAY_NAME
  BASIC_PLATFORM_ADMIN_ACCOUNT_NAME
  BASIC_PLATFORM_ADMIN_PASSWORD
USAGE
}

# 无参数不再等于 up：旧行为会直接构建、迁移并创建容器，误触发代价高。
# 这里只打印用法，不触碰 Docker、不写文件、不启动任何服务。
if (($# == 0)); then
    usage
    cat >&2 <<'EMPTY'
本地启动没有隐式默认动作：以上命令必须显式给出。

  bash scripts/docker-local.sh status     只看当前状态与下一步
  bash scripts/docker-local.sh dev-up     一键启动（推荐）
EMPTY
    exit 2
fi

# 此只读候选准备不会初始化或重置许可证。
if [[ "${1:-}" == "prepare-license" ]]; then
    shift
    exec python3 "${script_dir}/prepare-local-runtime-license.py" "$@"
fi

# 状态总览：只读，一眼看清卡在哪、下一步做什么。
if [[ "${1:-}" == "status" ]]; then
    shift
    exec python3 "${script_dir}/local-status.py" "$@"
fi

# 批量候选 + 统一发布；候选阶段只读，发布必须显式 --approve。
if [[ "${1:-}" == "refresh-approvals" ]]; then
    shift
    exec python3 "${script_dir}/refresh-local-runtime-approvals.py" "$@"
fi

if [[ "${1:-}" == "recover-license-pins" ]]; then
    shift
    exec python3 "${script_dir}/recover-local-license-pins.py" "$@"
fi

remove_volumes=false
core_only=false
while (($# > 0)); do
    case "$1" in
		up|down|stop|restart|build-images|bootstrap-admin|prepare-discovery|dev-up|dev-approve|ps|logs|config|verify|refresh-api|refresh-frontend|refresh-contract-api|refresh-customer-api|refresh-portal-api|refresh-project-api|refresh-data-analysis-api|start-presale-worker|start-presale-alert-worker)
            command_name="$1"
            shift
            ;;
        --build)
            force_build=true
            shift
            ;;
        --pull)
            # 兼容旧命令；缺失基础镜像现在默认会预拉取。
            shift
            ;;
        --volumes)
            remove_volumes=true
            shift
            ;;
        --core-only)
            # 业务镜像失效时用最小依赖恢复控制面，从而能够走“更新批准 → 受控重试”。
            core_only=true
            shift
            ;;
        --admin-display-name)
            (($# >= 2)) || fail "$1 缺少参数"
            admin_display_name="$2"
            shift 2
            ;;
        --admin-account-name)
            (($# >= 2)) || fail "$1 缺少参数"
            admin_account_name="$2"
            shift 2
            ;;
        --admin-password)
            (($# >= 2)) || fail "$1 缺少参数"
            admin_password="$2"
            log "WARN: --admin-password 会暴露在进程参数中，请改用 --admin-password-stdin"
            shift 2
            ;;
        --admin-password-stdin)
            admin_password_stdin=true
            shift
            ;;
        --subsystem-environment)
            (($# >= 2)) || fail "$1 缺少参数"
            subsystem_environment="${2,,}"
            shift 2
            ;;
        --env-file)
            (($# >= 2)) || fail "$1 缺少参数"
            env_file="$2"
            shift 2
            ;;
        --contract-env-file)
            (($# >= 2)) || fail "$1 缺少参数"
            contract_env_file="$2"
            shift 2
            ;;
		--customer-env-file)
            (($# >= 2)) || fail "$1 缺少参数"
            customer_env_file="$2"
			shift 2
			;;
		--portal-env-file)
			(($# >= 2)) || fail "$1 缺少参数"
			portal_env_file="$2"
			shift 2
			;;
		--project-env-file)
			(($# >= 2)) || fail "$1 缺少参数"
			project_env_file="$2"
			shift 2
			;;
		--data-analysis-env-file)
			(($# >= 2)) || fail "$1 缺少参数"
			data_analysis_env_file="$2"
			shift 2
			;;
		--settlement-env-file)
			(($# >= 2)) || fail "$1 缺少参数"
			settlement_env_file="$2"
			shift 2
			;;
		--presale-worker-env-file)
			(($# >= 2)) || fail "$1 缺少参数"
			presale_worker_env_file="$2"
			shift 2
			;;
        -h|--help)
            usage
            exit 0
            ;;
        *)
            break
            ;;
    esac
done

case "$subsystem_environment" in
	dev|test|staging|prod) ;;
	*) fail "--subsystem-environment 只能是 dev、test、staging 或 prod" ;;
esac

log_services=()
log_since=""
log_tail=""
log_follow=true

if [[ "$command_name" == "logs" ]]; then
    while (($# > 0)); do
        case "$1" in
            --since)
                (($# >= 2)) || fail "$1 缺少参数"
                log_since="$2"
                shift 2
                ;;
            --tail)
                (($# >= 2)) || fail "$1 缺少参数"
                log_tail="$2"
                shift 2
                ;;
            --no-follow)
                log_follow=false
                shift
                ;;
            --)
                shift
                log_services+=("$@")
                break
                ;;
            -*)
                fail "logs 不支持的选项：$1"
                ;;
            *)
                log_services+=("$1")
                shift
                ;;
        esac
    done
fi

command -v docker >/dev/null 2>&1 || fail "未找到 docker 命令"
docker compose version >/dev/null 2>&1 || fail "当前 Docker 不支持 docker compose 子命令"
[[ -f "$compose_file" ]] || fail "Compose 文件不存在：$compose_file"
[[ -d "$contract_root" ]] || fail "合同管理后端目录不存在：$contract_root"
[[ -d "$customer_root" ]] || fail "客户与商机管理后端目录不存在：$customer_root"
[[ -d "$project_management_root" ]] || fail "项目管理系统后端目录不存在：$project_management_root"
[[ -d "$settlement_root" ]] || fail "结算与开票系统后端目录不存在：$settlement_root"

export BASIC_PLATFORM_RUNTIME_ENV_FILE="$env_file"
export CONTRACT_RUNTIME_ENV_FILE="$contract_env_file"
export CUSTOMER_RUNTIME_ENV_FILE="$customer_env_file"
export SETTLEMENT_RUNTIME_ENV_FILE="$settlement_env_file"
export PORTAL_RUNTIME_ENV_FILE="$portal_env_file"
export PROJECT_RUNTIME_ENV_FILE="$project_env_file"
export DATA_ANALYSIS_RUNTIME_ENV_FILE="$data_analysis_env_file"
export PRESALE_WORKER_ENV_FILE="$presale_worker_env_file"
export BASIC_PLATFORM_HOST_PROJECT_ROOT="$project_root"
export SUBSYSTEM_HOST_PROJECTS_ROOT="$workspace_root"
export LOCAL_SUBSYSTEM_ENVIRONMENT="$subsystem_environment"

validate_local_automation_paths() {
    [[ -d "$BASIC_PLATFORM_HOST_PROJECT_ROOT" ]] || \
        fail "基础平台宿主机项目目录不存在：$BASIC_PLATFORM_HOST_PROJECT_ROOT"
    [[ -d "$SUBSYSTEM_HOST_PROJECTS_ROOT" ]] || \
        fail "子系统宿主机根目录不存在：$SUBSYSTEM_HOST_PROJECTS_ROOT"
    [[ -f "$BASIC_PLATFORM_HOST_PROJECT_ROOT/scripts/portal-gateway.sh" ]] || \
        fail "基础平台网关脚本不存在：$BASIC_PLATFORM_HOST_PROJECT_ROOT/scripts/portal-gateway.sh"
    [[ -f "$BASIC_PLATFORM_HOST_PROJECT_ROOT/docker/portal-apps-locations.conf" ]] || \
        fail "基础平台网关配置不存在：$BASIC_PLATFORM_HOST_PROJECT_ROOT/docker/portal-apps-locations.conf"
}

validate_local_automation_paths

configure_access_mode

# 本地默认 HTTP 拓扑必须保持向后兼容：PUBLIC_HTTPS_ENABLED 未配置或为 false
# 时继续使用 localhost:8081 / localhost:18090，并且绝不读取证书路径。只有显式
# 开启 HTTPS 时，才使用与生产发布相同的证书预检与配置派生逻辑。
public_https_enabled="$(awk -F= '$1 == "PUBLIC_HTTPS_ENABLED" {print substr($0, index($0, "=") + 1); exit}' "$env_file" 2>/dev/null || true)"
public_https_enabled="${public_https_enabled:-false}"
public_transport_state="$(awk -F= '$1 == "PUBLIC_TRANSPORT_STATE" {print substr($0, index($0, "=") + 1); exit}' "$env_file" 2>/dev/null || true)"
[[ "$public_https_enabled" == "true" || "$public_https_enabled" == "false" ]] || \
    fail "PUBLIC_HTTPS_ENABLED 只能是 true 或 false"
if [[ "$public_https_enabled" == "true" || "$public_transport_state" == "DISABLING_HTTPS" ]]; then
    public_transport_helper="${project_root}/deploy/production/bin/public-transport.sh"
    [[ -r "$public_transport_helper" ]] || fail "公开传输配置助手不存在：$public_transport_helper"
    # shellcheck source=../deploy/production/bin/public-transport.sh
    source "$public_transport_helper"
    public_transport_prepare "$project_root" "$env_file" "${project_root}/compose.https.yaml" "${project_root}/compose.drain.yaml" || exit 1
    frontend_public_origin="$PUBLIC_PLATFORM_ORIGIN"
    if [[ "$public_transport_state" == "DISABLING_HTTPS" ]]; then
        log "HTTPS→HTTP 会话排空中：HTTP 公开地址 ${PUBLIC_PLATFORM_ORIGIN}，443 暂时保留"
    else
        log "HTTPS 统一访问地址：${PUBLIC_PLATFORM_ORIGIN}（SSO：${PUBLIC_SSO_ORIGIN}）"
    fi
else
    export PUBLIC_HTTPS_ENABLED=false
    export PUBLIC_TRANSPORT_COMPOSE_FILE=""
fi

# 旧版本的 split-runtime profile 可能留下名为 worker 的平台 Worker。
# 当前默认编排不启用该 profile，Compose --remove-orphans 不会处理未激活
# profile 中的服务；down 时只按 Compose 项目和服务标签清理这一类遗留容器，
# 避免它继续占用 basic-platform-local_default 网络。
remove_legacy_split_runtime_containers() {
    local container_id container_ids
    container_ids="$(docker ps -aq \
        --filter "label=com.docker.compose.project=${compose_project}" \
        --filter "label=com.docker.compose.service=worker")"
    while IFS= read -r container_id; do
        [[ -n "$container_id" ]] || continue
        log "移除遗留 split-runtime Worker 容器：$container_id"
        docker rm -f "$container_id" >/dev/null
    done <<< "$container_ids"
}

verify_local_file_gateway() {
    if ! compose_run exec -T api wget -qO- http://file-gateway:8086/readyz >/dev/null; then
        fail "统一文件网关未通过容器网络 readiness 检查"
    fi
    log "统一文件网关容器网络 readiness 检查通过"
}

ensure_platform_control_dependencies() {
    # 定向刷新刻意使用 --no-deps，避免重复执行 one-shot 迁移；因此必须显式
    # 补齐长期依赖。仅有 provisioner Socket 不代表它能访问 Docker。
    compose_up_wait "基础平台数据库与受限 Docker 代理" --no-deps mysql file-gateway-mysql docker-socket-proxy
    compose_up_wait "统一文件网关" --no-deps file-gateway
}

start_customer_notification_workers() {
	local service container_id initial_restarts current_restarts
	log "启动 CRM 通知生成与平台投递 Workers"
	compose_run up -d --no-deps "${customer_notification_worker_services[@]}"
	for service in "${customer_notification_worker_services[@]}"; do
		container_id="$(compose_run ps -q "$service")"
		[[ -n "$container_id" ]] || fail "CRM 通知 Worker 未创建：$service"
		[[ "$(docker inspect "$container_id" --format '{{.State.Running}}')" == "true" ]] || fail "CRM 通知 Worker 未运行：$service"
		initial_restarts="$(docker inspect "$container_id" --format '{{.RestartCount}}')"
		sleep 2
		[[ "$(docker inspect "$container_id" --format '{{.State.Running}}')" == "true" ]] || fail "CRM 通知 Worker 启动后退出：$service"
		current_restarts="$(docker inspect "$container_id" --format '{{.RestartCount}}')"
		[[ "$current_restarts" == "$initial_restarts" ]] || fail "CRM 通知 Worker 启动后发生重启：${service}（${initial_restarts} -> ${current_restarts}）"
	done
	log "CRM 通知 Workers 已稳定运行且未发生重启"
}

# 首次接入时 customer-api 还拿不到目录发布/OIDC 凭据，不能运行；但平台的
# “探测子系统”需要先读到它的受控 Docker 标签。只创建不启动候选容器，
# 接入完成后仍由正常 up/provisioner 流程执行迁移、目录发布和健康启动。
# 子系统探测候选：带 com.basic-platform.discovery 标签、但不启动进程的容器。
# 平台“探测子系统”用 docker ps --all 读这些标签发现尚未接入的子系统；候选必须能被创建
# （不启动），否则会形成“未接入不能启动、未运行不能探测”的死锁。
subsystem_discovery_service() {
	case "$1" in
		contract_management) printf 'contract-api' ;;
		customer_and_opportunity) printf 'customer-api' ;;
		customer_portal) printf 'portal-api' ;;
		project_management) printf 'project-api' ;;
		settlement) printf 'settlement-api' ;;
		data_analysis) printf 'dashboard-api' ;;
		*) return 1 ;;
	esac
}

# 六个子系统统一创建探测候选：不启动任何业务进程，只创建带标签的容器。
# 单个失败不阻断其余候选；失败通常意味着该服务的运行固定镜像尚未刷新。
ensure_subsystem_discovery_candidates() {
	local application="" service="" created=0 skipped=0
	log "创建子系统探测候选（带标准标签，不启动业务进程）"
	for application in contract_management customer_and_opportunity customer_portal project_management settlement data_analysis; do
		service="$(subsystem_discovery_service "$application")" || continue
		# 已在运行的服务容器自带同一组标准标签，探测器可直接发现；跳过候选创建。
		# 否则 skip-override 的基础配置会因配置漂移重建运行中的容器，剥掉固定记录
		# 的 license 卷和 keys 挂载（settlement-api 曾因此丢卷进入重启循环）。
		if docker ps --filter "label=com.docker.compose.project=${compose_project}" \
			--filter "label=com.docker.compose.service=${service}" --format '{{.ID}}' | grep -q .; then
			skipped=$((skipped + 1))
			log "  [运行中] ${application}（${service}）：已有运行容器，跳过候选创建以保留固定记录挂载"
			continue
		fi
		# 探测候选只创建容器、不启动进程，因此不受运行固定记录约束：
		# 否则固定记录尚未受控更新时会形成「候选建不出来 → 探测不到 → 无法接入」的死锁。
		local reason=""
		# 捕获真实原因：失败时最常见的是本地标签镜像已被清理，必须让使用者看到原因，
		# 而不是只看到"创建失败"四个字。
		if reason="$(COMPOSE_SKIP_LICENSE_OVERRIDE=1 compose_run up --no-start --no-deps --no-build "$service" 2>&1)"; then
			created=$((created + 1))
		else
			skipped=$((skipped + 1))
			# grep 无匹配会返回非零，pipefail 下会让赋值失败并中止；这里显式兜底。
			reason="$(printf "%s\n" "$reason" | grep -iE "no such image" | tail -1 || true)"
			log "  [跳过] ${application}（${service}）：${reason:-探测候选创建失败}"
		fi
	done
	log "探测候选：已创建或已存在 $created 个，跳过 $skipped 个"
	if ((skipped > 0)); then
		log "下一步：先补齐缺失的本地镜像（bash scripts/docker-local.sh build-images），再重跑本步骤"
	fi
	# 保留旧函数名以便既有调用与测试兼容。
	return 0
}

ensure_crm_discovery_candidate() {
	compose_run up --no-start --no-deps --no-build customer-api >/dev/null
}

# 数据看板首次接入前同样必须先进入平台“子系统探测”列表。其容器标签已经声明
# application_code / environment / callback 等非敏感元数据，但 dashboard-api 的进程
# 还依赖平台签发的 OIDC、机器 Client 和授权目录凭据，不能在接入前启动后进入重启循环。
# `up --no-start` 会创建带标签的 Docker 容器而不运行进程；探测器使用 `docker ps --all`
# 可读取 created 容器，接入完成后普通 up 会复用并正常启动它。
ensure_data_analysis_discovery_candidate() {
	log "创建数据看板探测候选（不启动业务进程）"
	compose_run up --no-start --no-deps --no-build dashboard-api >/dev/null
}

# 只读预检接缝：测试用替身替换，生产走真实 Docker。
docker_control_available() { docker info >/dev/null 2>&1; }
local_image_present() { docker image inspect "$1" >/dev/null 2>&1; }

# 启动前只读预检：V1 固定记录核对批准，V2 核对 Agent 下发的登记绑定；两者都检查不可变镜像。
# 任何问题都在执行迁移之前一次性报出，避免逐个撞 No such image，也避免业务镜像失效时
# 控制面一并被阻断。本函数不写入任何文件，也不修改固定记录。
# 重建后对比"刚构建的候选镜像"与"固定记录"：固定记录存在时 compose up 用的是已批准镜像，
# 因此新构建不会自动生效。这里只提示，不阻断（阻断见 preflight_local_license_images）。
# 已登记（已固定）的应用不允许用 refresh-* 直接重建：重建会生成新镜像 ID，
# 使固定记录与批准同时失效（运行中的服务仍跑旧镜像，up 预检则报 No such image）。
# 受控路径：build-images 准备候选 → refresh-approvals --approve → 应用接入受控更新 → up。
# 冷启动自愈：上一次项目后台操作停在中间态、而容器已随整体停机消失时，
# 旧状态无法再被证明排空，up 会拒绝启动。这里把状态文件归档（不删除）后重新准备。
recover_stale_project_background_state() {
    local state_file="${project_root}/runtime/project-background/operation.json"
    [[ -f "$state_file" ]] || return 0
    local running existing verdict
    running="$(compose --profile project ps -q project-api sla-notifier 2>/dev/null | grep -c . || true)"
    existing="$(compose --profile project ps -aq project-api sla-notifier 2>/dev/null | grep -c . || true)"
    verdict="$(python3 "${script_dir}/project_background_state.py" "$state_file" "$running" "$existing" 2>/dev/null)" || return 0
    [[ "$verdict" == archive ]] || return 0
    local backup="${state_file}.stale.$(date -u +%Y%m%dT%H%M%SZ)"
    mv "$state_file" "$backup" || return 1
    log "项目后台存在无法排空的旧状态（容器已不存在），已归档到 ${backup}，重新准备"
}

refuse_pinned_rebuild() {
    local pin_file="${project_root}/runtime/local-license.compose.json"
    [[ -f "$pin_file" ]] || return 0
    python3 - "$pin_file" "$@" <<'PYEOF'
import json, sys
pins = json.load(open(sys.argv[1], encoding="utf-8")).get("services", {})
requested = [name for name in sys.argv[2:] if name in pins]
if requested:
    print("[拒绝重建] 这些服务已写入授权固定记录，refresh-* 重建会让固定记录与批准同时失效：")
    for name in requested:
        print("  " + name)
    print("  受控路径：bash scripts/docker-local.sh build-images")
    print("            bash scripts/docker-local.sh refresh-approvals --operator <标识>")
    print("            bash scripts/docker-local.sh dev-approve --replace")
    print("            应用接入页受控更新（或 bash scripts/subsystem.sh retry --application-code <编码>）后执行 up")
    sys.exit(3)
PYEOF
}

report_candidate_drift() {
    local manifest="${project_root}/config/local-license-components.json"
    local pin_file="${project_root}/runtime/local-license.compose.json"
    [[ -f "$manifest" && -f "$pin_file" ]] || return 0
    python3 - "$manifest" "$pin_file" <<'PYEOF' || true
import json, subprocess, sys
manifest, pin_file = sys.argv[1], sys.argv[2]
apps = json.load(open(manifest, encoding="utf-8")).get("applications", {})
pins = json.load(open(pin_file, encoding="utf-8")).get("services", {})
rows = []
for app, spec in apps.items():
    for service, tag in (spec.get("services") or {}).items():
        pinned = (pins.get(service) or {}).get("image")
        if not pinned:
            continue
        actual = subprocess.run(["docker", "image", "inspect", "--format", "{{.Id}}", tag],
                                capture_output=True, text=True).stdout.strip()
        if actual and actual != pinned:
            rows.append((app, service, tag, pinned[:19], actual[:19]))
stale = []
for app, spec in apps.items():
    migration = (spec.get("migration") or {})
    services = spec.get("services") or {}
    api_service = next((name for name in services if name.endswith("-api")), None)
    app_images = [services[api_service]] if api_service else []
    migrate_tag = migration.get("image")
    if not migrate_tag or not app_images:
        continue
    def created(tag):
        return subprocess.run(["docker", "image", "inspect", "--format", "{{.Created}}", tag],
                              capture_output=True, text=True).stdout.strip()
    migrate_created, app_created = created(migrate_tag), [created(t) for t in app_images]
    if migrate_created and any(t and t < migrate_created for t in app_created):
        stale.append((app, migrate_tag))
if stale:
    print("[迁移镜像滞后] 应用镜像比迁移镜像更新，\"migrations are current\" 可能是假象：")
    for app, tag in stale:
        print("  %-24s 迁移镜像=%s" % (app, tag))
    print("  下一步：同一次构建里重建迁移镜像（如 refresh-data-analysis-api 会一并重建 dashboard-migrate）")
if rows:
    print("[候选镜像未生效] 以下服务刚构建的镜像与固定记录不一致，重启后运行的仍是已批准镜像：")
    for app, service, tag, pin, actual in rows:
        print("  %-24s %-28s 固定=%s 候选=%s (%s)" % (app, service, pin, actual, tag))
    print("  下一步：refresh-approvals --operator <标识> → --approve → 应用接入受控更新（或 subsystem.sh retry）→ up")
PYEOF
}

preflight_local_license_images() {
    local override_file="${project_root}/runtime/local-license.compose.json"
    local approvals_dir="${project_root}/runtime-approvals"
    if [[ ! -e "$override_file" && ! -L "$override_file" ]]; then
        # 尚无登记应用：未登记路径自行处理缺失镜像。
        return 0
    fi
    if [[ -L "$override_file" || ! -f "$override_file" ]]; then
        fail "本地授权 Compose 固定文件不安全（符号链接或非普通文件）：${override_file}；请人工核查后再启动"
    fi
    local mode
    mode="$(stat -c '%a' "$override_file" 2>/dev/null || stat -f '%Lp' "$override_file" 2>/dev/null || true)"
    if [[ "$mode" != "600" ]]; then
        fail "本地授权 Compose 固定文件权限不安全（当前 ${mode:-未知}，要求 600）：$override_file"
    fi
    if ! docker_control_available; then
        fail "Docker 不可达：无法核对已批准镜像是否存在。这是依赖不可用，不要按批准或镜像问题处理；请先启动 Docker Engine 后重试"
    fi
    local report problems=0
report="$(PREFLIGHT_OVERRIDE="$override_file" PREFLIGHT_APPROVALS="$approvals_dir" PREFLIGHT_MANIFEST="${project_root}/config/local-license-components.json" python3 - <<'PY'
import glob
import hashlib
import json
import os
import re
import stat
import sys

IMMUTABLE = re.compile(r"^sha256:[0-9a-f]{64}$")
ALIASES = {
    "presale-worker": "customer-presale-worker",
    "contract-transfer-worker": "customer-contract-transfer-worker",
    "sla-notifier": "project-sla-notifier",
    "dashboard-api": "data-analysis-api",
    "aggregation-worker": "data-analysis-aggregation-worker",
    "alert-worker": "data-analysis-alert-worker",
}


def emit(*fields):
    print("\t".join(str(field) for field in fields))


def unique_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError("duplicate key")
        result[key] = value
    return result


def private_environment(path):
    # 只读取 Agent 固定路径，不执行/sourcing env，也不把凭据或解析内容写入诊断。
    if os.path.islink(os.path.dirname(path)):
        raise ValueError("unsafe runtime directory")
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(fd, "r", encoding="utf-8") as handle:
        info = os.fstat(handle.fileno())
        if not stat.S_ISREG(info.st_mode) or stat.S_IMODE(info.st_mode) != 0o600 or info.st_size > 65536:
            raise ValueError("unsafe environment file")
        values = {}
        for line in handle:
            key, sep, value = line.rstrip("\n").partition("=")
            if not sep or not re.fullmatch(r"[A-Z_][A-Z_0-9]*", key) or key in values:
                raise ValueError("invalid environment format")
            if value.startswith('"'):
                value = json.loads(value)
            elif not re.fullmatch(r"[-A-Za-z0-9_./:@,]*", value):
                raise ValueError("invalid environment value")
            if not isinstance(value, str) or any(char in value for char in "\r\n\x00"):
                raise ValueError("invalid environment value")
            values[key] = value
    return values


override = os.environ["PREFLIGHT_OVERRIDE"]
approvals_dir = os.environ["PREFLIGHT_APPROVALS"]
root = os.path.dirname(os.path.dirname(override))
owners = {}
migrations = {}
try:
    with open(os.environ["PREFLIGHT_MANIFEST"], encoding="utf-8") as handle:
        manifest = json.load(handle)
    for application, target in manifest["applications"].items():
        migrations[application] = target["migration"]["service"]
        for name in list(target["services"]) + [target["migration"]["service"]]:
            if name in owners:
                raise ValueError("组件清单有重复服务")
            owners[name] = application
except Exception:
    emit("error", "-", "-", "-", "权威组件清单无法读取或格式非法")
try:
    with open(override, encoding="utf-8") as handle:
        pinned = json.load(handle, object_pairs_hook=unique_object)
except Exception as exc:
    emit("error", "-", "-", "-", "固定文件无法解析：" + str(exc)[:120])
    sys.exit(0)
services = pinned.get("services") if isinstance(pinned, dict) else None
if not isinstance(services, dict) or not services:
    emit("error", "-", "-", "-", "固定文件缺少 services 映射")
    sys.exit(0)
pinned_services = {}
builtin = set()
builtin_apps = set()
for name, spec in services.items():
    image = spec.get("image") if isinstance(spec, dict) else None
    if not isinstance(name, str) or not isinstance(image, str) or not IMMUTABLE.match(image):
        emit("error", owners.get(name, "未知应用"), str(name), json.dumps(image, ensure_ascii=False), "固定记录不是不可变镜像 ID")
        continue
    if name not in owners:
        emit("error", "未知应用", name, image, "服务不在权威组件清单中")
        continue
    pinned_services[name] = image
    if name in migrations.values() or not spec.get("env_file"):
        continue
    expected_env = os.path.join(root, "runtime", "license-" + name + ".env")
    if spec["env_file"] != [expected_env]:
        emit("error", owners[name], name, image, "运行凭据必须引用 Agent 固定路径")
        continue
    try:
        values = private_environment(expected_env)
    except Exception:
        emit("error", owners[name], name, image, "运行凭据文件缺失、不安全或格式非法（要求普通文件、0600）")
        continue
    protocol = values.get("COMMERCIAL_LICENSE_REGISTRATION_PROTOCOL", "1")
    if protocol == "1":
        continue
    emit("builtin", owners[name], name, image, "V2")
    expected_values = {
        "COMMERCIAL_LICENSE_ENABLED": "true", "COMMERCIAL_LICENSE_REGISTRATION_PROTOCOL": "2",
        "COMMERCIAL_LICENSE_CATALOG_VERSION": "1",
        "COMMERCIAL_LICENSE_SERVICE_ID": ALIASES.get(name, name),
        "COMMERCIAL_LICENSE_PLATFORM_PUBLIC_KEY_PATH": "/app/data/keys/jwt-ed25519-public.pem",
        "COMMERCIAL_LICENSE_PLATFORM_BASE_URL": "http://platform-api:8080",
        "COMMERCIAL_LICENSE_ALLOW_HTTP": "true",
    }
    valid = all(values.get(key) == value for key, value in expected_values.items())
    valid = valid and all(values.get("COMMERCIAL_LICENSE_" + key) for key in
                          ("INSTANCE_ID", "ENVIRONMENT", "CLIENT_ID", "CLIENT_SECRET"))
    # 商业实例的 environment 独立于应用 dev；状态文件沿 Agent 身份派生规则核对。
    state_parts = [values.get("COMMERCIAL_LICENSE_" + key, "") for key in
                   ("INSTANCE_ID", "ENVIRONMENT", "CLIENT_ID", "SERVICE_ID", "CATALOG_VERSION")]
    state_parts.append("catalog-1")
    state_id = hashlib.sha256("\x00".join(state_parts).encode("utf-8")).hexdigest()[:32]
    expected_state = "/var/lib/commercial-license/" + ALIASES.get(name, name) + "-" + state_id + ".json"
    valid = valid and values.get("COMMERCIAL_LICENSE_STATE_PATH") == expected_state
    volumes = ["license-state-" + name + ":/var/lib/commercial-license",
               os.path.join(root, "data", "keys") + ":/app/data/keys:ro"]
    valid = valid and spec.get("pull_policy") == "never" and spec.get("volumes") == volumes
    allowed_spec_keys = {"image", "pull_policy", "env_file", "volumes"}
    # 项目后台需要把构建 ID 下发给 api 与 sla-notifier；只允许这两个服务使用
    # 由固定镜像派生出的单一环境变量，其他额外字段仍然拒绝。
    if name in ("project-api", "sla-notifier"):
        expected_build_id = "project-background-sha256-" + image[len("sha256:"):]
        if spec.get("environment") == {"PROJECT_BACKGROUND_BUILD_ID": expected_build_id}:
            allowed_spec_keys.add("environment")
    valid = valid and set(spec) == allowed_spec_keys
    valid = valid and not set(pinned) - {"services", "volumes"}
    declared_volumes = pinned.get("volumes", {})
    valid = valid and isinstance(declared_volumes, dict) and declared_volumes.get("license-state-" + name) == {}
    if not valid:
        emit("error", owners[name], name, image, "V2 登记绑定、目录版本或受控挂载不匹配")
        continue
    builtin.add(name)
    builtin_apps.add(owners[name])
covered = set(builtin)
expected = [(owners[name], name, pinned_services[name]) for name in sorted(builtin)]
for application in sorted(builtin_apps):
    migration = migrations[application]
    spec = services.get(migration, {})
    if migration not in pinned_services:
        emit("unpinned", application, migration, "-", "V2 固定文件缺少迁移镜像")
    elif spec.get("pull_policy") != "never" or set(spec) != {"image", "pull_policy"}:
        emit("error", application, migration, pinned_services[migration], "V2 迁移固定记录非法")
    else:
        covered.add(migration)
        expected.append((application, migration, pinned_services[migration]))
    # 条件邀请补偿组件不因预检自动启用；其余目录消费者必须存在。
    for name in manifest["applications"][application]["services"]:
        if name != "portal-invite-compensation-worker" and name not in pinned_services:
            emit("unpinned", application, name, "-", "V2 固定文件缺少必需消费者")
for path in sorted(glob.glob(os.path.join(approvals_dir, "local-*-dev.json"))):
    label = os.path.basename(path)
    try:
        with open(path, encoding="utf-8") as handle:
            approval = json.load(handle)
    except Exception as exc:
        emit("error", label, "-", "-", "批准文件无法解析：" + str(exc)[:120])
        continue
    if not isinstance(approval, dict) or not isinstance(approval.get("services"), list):
        emit("error", label, "-", "-", "批准文件结构非法")
        continue
    application = approval.get("application") or label
    wanted = [(item.get("name"), item.get("image_digest"))
              for item in (approval.get("services") or []) if isinstance(item, dict)]
    migration = approval.get("migration")
    if isinstance(migration, dict):
        wanted.append((migration.get("service"), migration.get("image_digest")))
    for name, digest in wanted:
        if not isinstance(name, str) or not isinstance(digest, str) or not digest:
            continue
        if name in builtin or (name == migrations.get(owners.get(name)) and owners.get(name) in builtin_apps):
            # V2 Agent 已重新绑定此组件；遗留 V1 文件不再冻结其旧镜像。
            continue
        covered.add(name)
        if name not in pinned_services:
            emit("unpinned", application, name, digest, "固定文件缺少该服务")
        elif pinned_services[name] != digest:
            emit("mismatch", application, name, pinned_services[name], "固定记录与批准不一致")
        expected.append((application, name, digest))
for name, image in sorted(pinned_services.items()):
    if name not in covered:
        emit("unapproved", "-", name, image, "固定文件中的服务没有对应批准")
for application, name, digest in expected:
    emit("need", application, name, digest, "-")
PY
)" || fail "本地授权镜像预检无法完成（内部错误）"

    local kind application service digest detail builtin_mode=false
    while IFS=$'\t' read -r kind application service digest detail; do
        [[ -n "$kind" ]] || continue
        case "$kind" in
            builtin)
                builtin_mode=true
                ;;
            error)
                log "  [固定文件或批准] ${application} / ${service} ${digest}（${detail}）"
                problems=$((problems + 1))
                ;;
            mismatch)
                log "  [固定记录不一致] ${application} / ${service} ${digest}（${detail}）"
                problems=$((problems + 1))
                ;;
            unpinned)
                log "  [固定文件缺服务] ${application} / ${service} ${digest}（${detail}）"
                problems=$((problems + 1))
                ;;
            unapproved)
                log "  [缺少批准] ${service} ${digest}（${detail}）"
                problems=$((problems + 1))
                ;;
            need)
                if ! local_image_present "$digest"; then
                    log "  [镜像缺失] $application / $service $digest"
                    problems=$((problems + 1))
                fi
                ;;
        esac
    done <<<"$report"
    if ((problems > 0)); then
        log "本地授权镜像预检未通过：共 $problems 项；不执行迁移，也不启动任何业务或平台容器"
        if [[ "${preflight_candidates_prepared:-false}" == true ]]; then
            log "本次 --build 已准备候选镜像；旧运行镜像仍需受控更新才会被采用。"
            log "下一步（不要重复执行构建）："
        else
            log "下一步（复用已构建候选；仅在候选镜像缺失时构建）："
            log "  0. 仅候选缺失时：bash scripts/docker-local.sh build-images"
            log "     只准备候选镜像：不检查旧运行镜像、不迁移、不启动服务、不改写固定记录。"
        fi
        if [[ "$builtin_mode" == true ]]; then
            log "  1. V2 组件：恢复固定 ID 对应的本地镜像，通过平台受控更新由 Agent 重新下发登记绑定；不生成旧版批准。"
            log "  2. 若同时有 V1 组件，仍须核对该组件原有批准；不得手工改写运行凭据或固定 ID。"
        else
            log "  1. 为受影响 V1 应用生成批准候选，核对源码、完整组件与迁移镜像："
            log "     bash scripts/docker-local.sh prepare-license --application <应用> --operator <标识>"
            log "  2. 明确审核后发布批准：加 --approve（内容与现有批准不同时再加 --replace）"
            log "     固定文件含非法记录时：bash scripts/docker-local.sh recover-license-pins（预览）"
            log "     完整有效批准和镜像校验通过后加 --apply；先备份再恢复，不改运行凭据。"
        fi
        log "  3. 通过应用接入页或 scripts/subsystem.sh retry 走受控更新，由 Agent 更新固定记录"
        log "  4. 确认对应登记/批准、固定记录与镜像检查通过后重新执行普通 up"
        log "  5. 业务镜像失效导致控制面也无法启动时，可先执行 bash scripts/docker-local.sh up --core-only 恢复控制面"
        fail "本地授权镜像预检失败：按上述顺序恢复；脚本不会用可变标签替代已批准的不可变镜像"
    fi
}

# 已登记服务的运行镜像由固定记录决定；普通启动不得重建它们，否则会让已批准记录失效。
pinned_local_service() {
    local service="$1"
    local override_file="${project_root}/runtime/local-license.compose.json"
    [[ -f "$override_file" && ! -L "$override_file" ]] || return 1
    python3 - "$override_file" "$service" <<'PY'
import json
import sys
try:
    with open(sys.argv[1], encoding="utf-8") as handle:
        pinned = json.load(handle)
except Exception:
    sys.exit(1)
sys.exit(0 if sys.argv[2] in (pinned.get("services") or {}) else 1)
PY
}

build_images() {
    # SKIP_IMAGE_PREPARATION=1：完全离线/受限网络环境的逃生舱。调用方保证 :local 镜像
    # 已预置（离线交付包加载、镜像 retag 或预构建），此时跳过基础镜像拉取与 compose build。
    # 它不绕过启动前的镜像存在性预检。
    if [[ "${SKIP_IMAGE_PREPARATION:-}" == "1" ]]; then
        log "SKIP_IMAGE_PREPARATION=1：使用预置镜像，跳过基础镜像拉取与镜像构建（镜像存在性预检仍然生效）"
        return 0
    fi
    # Compose/BuildKit 会并发解析多个 Docker Hub 基础镜像。网络或代理不稳定时，
    # 任意一次匿名令牌请求超时都会让整个构建立即失败。先串行拉取缺失镜像并
    # 对每个镜像重试，使普通的 `up --build` 在全新环境中也具备容错能力。
    prepare_base_images
    # portal-api 即使尚未完成 OIDC 接入也可以安全构建；只是不应在凭据、租户和
    # 角色目录准备好之前启动。始终构建它可确保本地镜像拓扑稳定，且 Worker 与 CRM 使用同一版本。
    local all_services=(api file-gateway contract-api contract-worker settlement-api customer-api customer-presale-alert-worker portal-api project-api sla-notifier dashboard-migrate dashboard-api aggregation-worker alert-worker frontend presale-worker presale-integration-mock)
    if [[ "${core_only:-false}" == true ]]; then
        # 控制面单独恢复只准备控制面镜像；平台迁移与 Agent 复用 api 镜像。
        all_services=(api file-gateway frontend)
    fi
    local build_services=()
    local service="" image_map="" tag=""
    if [[ "$force_build" == true ]]; then
        # --build 只准备候选镜像：不发布批准，也不改写运行固定记录。
        build_services=("${all_services[@]}")
        log "准备候选镜像（--build）：不会替换已批准固定记录；完成受控更新前运行仍使用已批准镜像"
        log "受影响应用需按预检提示生成批准候选并经受控更新后才会采用新镜像"
    else
        image_map="$(compose_default_local_images || true)"
        if [[ -z "$image_map" ]]; then
            # 无法解析镜像映射时保守处理：按原行为准备全部候选，避免漏建导致启动失败。
            build_services=("${all_services[@]}")
            log "无法解析 Compose 镜像映射，按保守策略准备候选镜像"
        else
            for service in "${all_services[@]}"; do
                if pinned_local_service "$service"; then
                    # 已登记：运行镜像由固定记录决定，普通启动不重建。
                    continue
                fi
                tag="$(awk -F'\t' -v wanted="$service" '$1 == wanted { print $2; exit }' <<<"$image_map")"
                if [[ -z "$tag" ]]; then
                    continue
                fi
                if ! local_image_present "$tag"; then
                    build_services+=("$service")
                fi
            done
            if ((${#build_services[@]} == 0)); then
                log "普通启动：现有本地镜像齐备，跳过镜像构建（需要新候选时执行 up --build）"
                return 0
            fi
            log "构建缺失的本地镜像：${build_services[*]}"
        fi
    fi
    # migrate/bootstrap-admin/subsystem-provisioner 都复用 api 构建出的
    # basic-platform/backend:local；CRM、Portal 和售前 Worker 使用不同 target/镜像，不会
    # 把 crm-server、portal-server 和 Worker 运行在同一个业务容器中。
    # 限制并发既降低匿名镜像令牌抖动，也避免本地机器同时编译四个后端造成资源争抢。
    COMPOSE_PARALLEL_LIMIT=1 compose --profile bootstrap --profile customer --profile portal --profile project --profile presale-worker --ansi never build "${build_services[@]}"
}

prepare_gateway_config() {
    local gateway_script="${project_root}/scripts/portal-gateway.sh"
    [[ -x "$gateway_script" ]] || chmod +x "$gateway_script"
    PORTAL_GATEWAY_NGINX_INCLUDE="${project_root}/docker/portal-apps-locations.conf" \
        "$gateway_script" remove contract_management >/dev/null
    PORTAL_GATEWAY_NGINX_INCLUDE="${project_root}/docker/portal-apps-locations.conf" \
        "$gateway_script" remove customer_and_opportunity >/dev/null
	PORTAL_GATEWAY_NGINX_INCLUDE="${project_root}/docker/portal-apps-locations.conf" \
		"$gateway_script" remove customer-opportunity >/dev/null
	PORTAL_GATEWAY_NGINX_INCLUDE="${project_root}/docker/portal-apps-locations.conf" \
		"$gateway_script" remove customer_portal >/dev/null
	PORTAL_GATEWAY_NGINX_INCLUDE="${project_root}/docker/portal-apps-locations.conf" \
		"$gateway_script" remove project_management >/dev/null
	log "已清理内置业务模块的旧式整站反向代理；五个前端均由统一 frontend 容器直接承载"
}

local_image_id() { docker image inspect --format '{{.Id}}' "$1" 2>/dev/null || true; }

# 一键启动：把命令行能做的全部做完（Docker 预检 → 控制面含迁移/超管/探测候选 → 批准候选与核对单 → 状态总览）。
# 幂等，可重复执行。不改写固定记录、不发布批准、不替用户设置任何密码：
# 需要人工决定的步骤只打印唯一指令，不自动跳过。
dev_up() {
    log "本地一键启动（dev-up）：幂等，可重复执行"
    docker_control_available || fail "Docker 不可用：请确认 OrbStack 已启动，并执行 docker context use orbstack"
    # 控制面：含平台迁移、首个超级管理员初始化、六个子系统探测候选。
    start_control_plane_only
    # 运行批准：只生成候选并打印核对单；发布仍是独立的人工审核动作。
    local operator="${LOCAL_DEV_OPERATOR:-${admin_account_name:-local-dev}}"
    log "生成运行批准候选（只生成，不发布；operator=${operator}）"
    python3 "${script_dir}/refresh-local-runtime-approvals.py" \
        --workspace "${workspace_root}" --operator "$operator" || true
    # 收尾：状态总览给出唯一下一步。
    python3 "${script_dir}/local-status.py" || true
    return 0
}

# 发布运行批准：仍要求显式 --approve 才会写入批准文件。
dev_approve() {
    local operator="${LOCAL_DEV_OPERATOR:-${admin_account_name:-local-dev}}"
    exec python3 "${script_dir}/refresh-local-runtime-approvals.py" \
        --workspace "${workspace_root}" --operator "$operator" "$@"
}

# 独立候选构建入口：只准备本地候选镜像。不检查旧运行镜像是否存在、不执行迁移、
# 不启动服务、不发布批准、不改写镜像固定记录。构建始终不加载授权固定文件。
# 旧运行镜像缺失时，普通 up 会被预检阻断，因此恢复必须先经本入口准备候选。
build_candidate_images() {
    prepare_base_images
    log "准备候选镜像：不检查旧运行镜像，不执行迁移，不启动服务，不修改批准与固定记录"
    local all_services=(api file-gateway contract-api contract-worker settlement-api customer-api customer-presale-alert-worker portal-api project-api sla-notifier dashboard-migrate dashboard-api aggregation-worker alert-worker frontend presale-worker presale-integration-mock)
    # compose() 对 build 子命令始终不加载固定文件，这里与普通 up 共用同一入口。
    COMPOSE_PARALLEL_LIMIT=1 compose --profile bootstrap --profile customer --profile portal --profile project --profile presale-worker --ansi never build "${all_services[@]}"
    log "候选镜像已准备（服务 / 标签 / 镜像 ID）："
    local image_map="" service="" tag="" image_id=""
    image_map="$(compose_default_local_images || true)"
    for service in "${all_services[@]}"; do
        tag="$(awk -F'\t' -v wanted="$service" '$1 == wanted { print $2; exit }' <<<"$image_map")"
        [[ -n "$tag" ]] || continue
        image_id="$(local_image_id "$tag")"
        log "  $(printf '%-40s' "$service") $tag $image_id"
    done
    log "下一步：为受影响应用生成批准候选，核对源码、完整组件与迁移镜像："
    log "  bash scripts/docker-local.sh prepare-license --application <应用> --operator <标识>"
    log "明确审核后发布批准（--approve，内容不同再加 --replace），再通过应用接入页或 scripts/subsystem.sh retry 走受控更新。"
}

# 控制面单独恢复：业务镜像失效时普通 up 会被预检阻断，用本入口恢复控制面，
# 从而能够走“更新批准 → 受控重试”。只校验控制面配置；所有 up 显式带 --no-deps，
# 防止 Compose 递归拉起业务服务（frontend 依赖合同与结算 API，temporal 依赖合同库）。
start_control_plane_only() {
    log "控制面单独恢复（--core-only）：不执行业务迁移，不启动业务 API／Worker，不校验业务配置"
    ensure_platform_env_file
    # compose() 固定携带合同环境文件，这里只保证它存在，不校验其业务占位符。
    ensure_contract_env_file true
    build_images
    log "启动受限 Docker 代理、平台 MySQL 与文件网关 MySQL"
    compose_up_wait "控制面基础依赖" --no-deps docker-socket-proxy mysql file-gateway-mysql
    log "执行基础平台数据库迁移"
    compose_run run --rm --no-deps migrate ./migrate
    # 控制面必须自带超级管理员初始化：否则控制面起来了也登不进去。
    # 业务子系统没有独立账号密码，全部通过平台账号走 SSO。
    bootstrap_admin_if_needed
    log "启动统一文件网关"
    compose_up_wait "统一文件网关" --no-deps file-gateway
    log "启动受控子系统 provisioner（部署 Agent）"
    compose_up_wait "部署 Agent" --no-deps subsystem-provisioner
    log "启动 Keycloak 数据库与 Keycloak"
    compose_up_wait "Keycloak" --no-deps keycloak-db keycloak
    log "启动基础平台 API 与内嵌 Worker"
    compose_up_wait "基础平台 API" --no-deps api
    verify_local_file_gateway
    log "启动统一前端（--no-deps：不递归启动合同与结算 API）"
    compose_up_wait "统一前端" --no-deps frontend
    # 优先让平台可探测：只创建带标签的候选容器，不启动业务进程。
    # 运行固定镜像尚未刷新时个别候选会失败，但不得阻断控制面恢复。
    ensure_subsystem_discovery_candidates
    log "控制面恢复完成：平台登录与应用管理可用，业务系统保持未启动"
    log "业务系统在完成受控更新并重新执行普通 up 之前不可用；控制面成功不代表全栈成功"
    log "下一步（按顺序执行；命令均已存在，且不要重复执行候选构建）："
    log "  1. 只读确认当前卡点与下一步：bash scripts/docker-local.sh status"
    log "  2. 生成批准候选并打印核对单（只读、不发布）：bash scripts/docker-local.sh refresh-approvals --operator <标识>"
    log "  3. 明确审核后发布批准（必须显式 --approve；内容不同再加 --replace）：同命令追加 --approve"
    log "  4. 走受控更新刷新固定记录：应用接入页执行，或 bash scripts/subsystem.sh retry --application-code <应用编码>"
    log "  5. 固定记录与批准一致、所需镜像存在后：bash scripts/docker-local.sh up"
    log "  6. 可选：bash scripts/docker-local.sh verify（只校验网关路由，不代表业务可用）"
    log "注意：第 5 步之前执行普通 up 会再次被镜像预检阻断；预检提示中的 build-images 无需重复执行。"
}

run_migrations() {
	# 数据库先就绪、迁移再串行执行，业务 API 只能在全部 schema 成功后启动；禁止由 API 自动建表。
	# 各数据库互相独立，不存在跨库事务；某个后续迁移失败时，已成功数据库按自身幂等迁移规则重试。
	log "启动基础平台、合同、CRM 常驻 MySQL 与 Temporal，并等待健康检查"
	compose_run up -d --wait mysql file-gateway-mysql contract-mysql customer-mysql settlement-mysql temporal
    log "执行基础平台数据库迁移"
    compose_run run --rm --no-deps migrate ./migrate
    log "执行合同管理版本化数据库迁移"
    compose_run run --rm --no-deps contract-migrate
	log "执行客户与商机管理 CRM 清单迁移"
	compose_run run --rm --no-deps customer-migrate
	log "执行结算与开票版本化数据库迁移"
	compose_run run --rm --no-deps settlement-migrate
	if portal_configured; then
		log "启动客户自助门户 MySQL 并执行 Portal 清单迁移"
		compose_run up -d --wait portal-mysql
		compose_run run --rm --no-deps portal-migrate
	fi
	if project_configured; then
		log "启动项目管理系统 MySQL 并执行项目清单迁移"
		compose_run up -d --wait project-mysql
		compose_run run --rm --no-deps project-migrate
	fi
}

bootstrap_admin_if_needed() {
    local status_rc
    log "检查超级管理员是否已初始化"
    # status 的退出码 3 是“尚未初始化”的协议结果，不是脚本错误；局部关闭 errexit 后必须立即恢复。
    set +e
    compose_run --profile bootstrap run --rm --no-deps bootstrap-admin ./bootstrap-admin --status
    status_rc=$?
    set -e
    if [[ "$status_rc" -eq 0 ]]; then
        log "超级管理员已存在，跳过初始化"
        return 0
    fi
    [[ "$status_rc" -eq 3 ]] || fail "检查超级管理员状态失败，退出码：$status_rc"

    if [[ -z "$admin_display_name" && -t 0 ]]; then read -r -p '首次超级管理员显示名称（如 本地管理员）：' admin_display_name; fi
    if [[ -z "$admin_account_name" && -t 0 ]]; then read -r -p '首次超级管理员账号（3-64 位，仅字母数字与 . _ - ，登录用）：' admin_account_name; fi
    if [[ -z "$admin_password" ]]; then
        # 交互场景必须给出提示并关闭回显：--admin-password-stdin 以前在终端下静默读取，
        # 使用者看不到提示，极易把密码填进账号那一行。管道输入保持静默，便于自动化。
        if [[ -t 0 ]]; then
            read -r -s -p '首次超级管理员密码（8-128 位，含大写/小写/数字/符号，不回显）：' admin_password
            printf '\n'
        elif [[ "$admin_password_stdin" == true ]]; then
            IFS= read -r admin_password || true
        fi
    fi
    [[ -n "$admin_display_name" ]] || fail "首次初始化需要 --admin-display-name 或 BASIC_PLATFORM_ADMIN_DISPLAY_NAME"
    [[ -n "$admin_account_name" ]] || fail "首次初始化需要 --admin-account-name 或 BASIC_PLATFORM_ADMIN_ACCOUNT_NAME"
    [[ -n "$admin_password" ]] || fail "首次初始化需要 --admin-password 或 BASIC_PLATFORM_ADMIN_PASSWORD"

    log "初始化第一个超级管理员"
    # 密码经 stdin 进入一次性容器，不拼进容器命令行；完成后清除当前 shell 中的明文变量。
    printf '%s\n' "$admin_password" | compose_run --profile bootstrap run --rm --no-deps -T bootstrap-admin ./bootstrap-admin \
        --display-name "$admin_display_name" \
        --account-name "$admin_account_name" \
        --password-stdin
    unset admin_password BASIC_PLATFORM_ADMIN_PASSWORD
}

# 从统一前端容器内部访问公开路径，返回第一次 HTTP 响应的状态码。
# 使用容器自带的 wget，避免部署主机必须额外安装 curl。
frontend_http_status() {
    local route="$1" output status

    output="$(
        compose_run exec -T frontend \
            wget -S -O /dev/null "http://127.0.0.1${route}" 2>&1 || true
    )"
    status="$(printf '%s\n' "$output" | awk '$1 ~ /^HTTP\// { print $2; exit }')"
    if [[ -z "$status" ]]; then
        printf '%s\n' "$output" >&2
        return 1
    fi
    printf '%s' "$status"
}

# platform_catalog_token_status 用给定客户端凭据向平台换取一次授权目录同步令牌，
# 返回 HTTP 状态码。该请求只换令牌、不写任何数据，可安全用作启动前探针：200 表示
# 凭据仍被平台认可，401 表示该子系统在平台侧已不存在（典型场景是平台库被重建，
# 而子系统 .env.local 保留了旧凭据）。沿用 frontend_http_status 的约定走容器内
# 自带的 wget，避免要求部署主机额外安装 curl；basic 值用已强依赖的 openssl 生成。
platform_catalog_token_status() {
    local client_id="$1" client_secret="$2" output status basic

    [[ -n "$client_id" && -n "$client_secret" ]] || return 1
    basic="$(printf '%s:%s' "$client_id" "$client_secret" | openssl base64 -A)"
    output="$(
        compose_run exec -T frontend \
            wget -S -O /dev/null \
            --header="Authorization: Basic ${basic}" \
            --header="Content-Type: application/x-www-form-urlencoded" \
            --post-data="grant_type=client_credentials&scope=authorization.catalog.sync" \
            "http://127.0.0.1/oauth2/token" 2>&1 || true
    )"
    status="$(printf '%s\n' "$output" | awk '$1 ~ /^HTTP\// { print $2; exit }')"
    [[ -n "$status" ]] || return 1
    printf '%s' "$status"
}

# 后端容器通过 Compose healthcheck 变为 healthy 后，统一 Nginx 仍可能在数秒内
# 复用旧上游连接。启动校验对业务健康地址短暂重试，避免已成功启动的服务被一次 502
# 误判为失败；认证/授权等语义校验仍保持单次、严格检查。
wait_for_frontend_status() {
    local route="$1" expected_status="$2" label="$3" attempt status
    for attempt in $(seq 1 20); do
        status="$(frontend_http_status "$route" 2>/dev/null || true)"
        if [[ "$status" == "$expected_status" ]]; then
            return 0
        fi
        sleep 1
    done
    fail "${label} 返回 ${status:-无响应}，预期为 ${expected_status}"
}

# 返回第一次重定向响应中的 Location。该函数用于验证合同系统实际生成的
# OIDC 授权地址，避免仅检查 /auth/login=302 而遗漏后续 /authorize 转发错误。
frontend_http_location() {
    local route="$1" output location

    output="$(
        compose_run exec -T frontend \
            wget -S -O /dev/null "http://127.0.0.1${route}" 2>&1 || true
    )"
    location="$(printf '%s\n' "$output" | awk 'tolower($1) == "location:" { sub(/\r$/, "", $2); print $2; exit }')"
    if [[ -z "$location" ]]; then
        printf '%s\n' "$output" >&2
        return 1
    fi
    printf '%s' "$location"
}

# 将公开绝对地址转换为前端容器内可访问的路径，并完整保留查询参数。
public_url_to_route() {
    local public_url="$1" route

    case "$public_url" in
        http://*|https://*)
            route="$(printf '%s' "$public_url" | sed -E 's#^https?://[^/]+##')"
            ;;
        /*)
            route="$public_url"
            ;;
        *)
            return 1
            ;;
    esac
    [[ "$route" == /* ]] || return 1
    printf '%s' "$route"
}

verify_gateway_routes() {
	local platform_membership_status contract_health_status contract_session_status contract_login_status settlement_session_status
	local customer_health_status customer_session_status portal_health_status portal_session_status
	local project_health_status project_session_status
    local contract_authorize_url contract_authorize_route platform_authorize_status platform_login_url contract_issuer

    log "校验统一前端 Nginx 配置"
    compose_run exec -T frontend nginx -t >/dev/null

    # 任职关系接口属于基础平台 API。未携带会话时应被认证层拒绝为 401；
    # 404 代表前端网关或 api 容器仍在使用未包含该路由的旧版本。
    platform_membership_status="$(frontend_http_status /api/v1/memberships)" || \
        fail "无法访问基础平台任职关系接口"
    [[ "$platform_membership_status" == "401" ]] || \
        fail "基础平台任职关系接口返回 ${platform_membership_status}，预期未登录状态为 401；请重新构建并启动 api 与 frontend 容器"

    wait_for_frontend_status /contract_management/healthz 200 "合同管理健康检查路径"
	wait_for_frontend_status /settlement/healthz 200 "结算与开票健康检查路径"
	settlement_session_status="$(frontend_http_status /settlement/api/v1/auth/me)" || \
		fail "无法访问结算与开票登录状态接口"
	# 结算已接入平台统一身份 OIDC；自检请求不携带 Cookie，未登录应为 401。
	# 仅当显式保留 SETTLEMENT_DEVELOPMENT_AUTH=true 的旧开发认证模式时，才允许 200。
	settlement_expected_auth_status=401
	if [[ "$(env_value "$settlement_env_file" SETTLEMENT_DEVELOPMENT_AUTH 2>/dev/null || true)" == "true" ]]; then
		settlement_expected_auth_status=200
	fi
	[[ "$settlement_session_status" == "$settlement_expected_auth_status" ]] || \
		fail "本地结算登录状态接口返回 ${settlement_session_status}，预期为 ${settlement_expected_auth_status}"

    # 自检请求不携带登录 Cookie，因此 /auth/me 应返回 401。若返回 404，说明
    # /contract_management 前缀未被正确移除，部署必须立即失败。
    contract_session_status="$(frontend_http_status /contract_management/api/v1/auth/me)" || \
        fail "无法访问合同管理登录状态接口"
    [[ "$contract_session_status" == "401" ]] || \
        fail "合同管理登录状态接口返回 ${contract_session_status}，预期未登录状态为 401"

    contract_login_status="$(frontend_http_status /contract_management/auth/login)" || \
        fail "无法访问合同管理登录入口"
    [[ "$contract_login_status" == "302" ]] || \
        fail "合同管理登录入口返回 ${contract_login_status}，预期为 302 OIDC 跳转"

    # 继续验证合同后端生成的真实授权地址。若 Nginx 在转发 /authorize 时丢失
    # client_id、redirect_uri、state、nonce 或 PKCE 参数，此处会得到 400 并阻止部署。
    contract_authorize_url="$(frontend_http_location /contract_management/auth/login)" || \
        fail "合同管理登录入口未返回 OIDC 授权地址"
    contract_issuer="$(env_value "$contract_env_file" OIDC_ISSUER)"
    case "$contract_authorize_url" in
        */realms/*/protocol/openid-connect/auth\?*)
            # Keycloak RP：合同 API 启动时已经通过同一环境的私网后通道完成
            # discovery；这里仅验证浏览器被正确导向公开 Realm，而不让前端
            # 容器错误地以 localhost 访问浏览器专用的公开地址。
            [[ "$contract_issuer" == */realms/* ]] || \
                fail "合同管理登录入口跳转到 Keycloak，但 OIDC_ISSUER 不是 Realm 地址"
            ;;
        *)
            # 兼容尚未迁移的基础平台 OIDC 环境。
            contract_authorize_route="$(public_url_to_route "$contract_authorize_url")" || \
                fail "合同管理登录入口返回了无法识别的 OIDC 授权地址"
            [[ "$contract_authorize_route" == /authorize\?* ]] || \
                fail "合同管理登录入口未跳转到基础平台 /authorize 或 Keycloak Realm"

            platform_authorize_status="$(frontend_http_status "$contract_authorize_route")" || \
                fail "无法通过统一前端访问 OIDC 授权端点"
            [[ "$platform_authorize_status" == "302" ]] || \
                fail "OIDC 授权端点返回 ${platform_authorize_status}，预期未登录状态为 302；请检查 /authorize 是否完整保留查询参数"

            platform_login_url="$(frontend_http_location "$contract_authorize_route")" || \
                fail "OIDC 授权端点未返回统一登录页地址"
            case "$platform_login_url" in
                /login.html\?return_to=*|http://*/login.html\?return_to=*|https://*/login.html\?return_to=*) ;;
                *) fail "OIDC 授权端点未跳转到统一登录页" ;;
            esac
            ;;
    esac

    if [[ "${crm_catalog_ready:-true}" == true ]]; then
        wait_for_frontend_status /customer-opportunity/healthz 200 "客户与商机管理健康检查路径"
        customer_session_status="$(frontend_http_status /customer-opportunity/api/v1/auth/me)" || \
            fail "无法访问客户与商机管理登录状态接口"
        case "$customer_session_status" in
            401|503) ;;
            *) fail "客户与商机管理登录状态接口返回 ${customer_session_status}，预期开发认证未携带身份时为 401（或认证未配置时为 503）" ;;
        esac
    fi

	if portal_configured && [[ "${portal_catalog_ready:-true}" == true ]]; then
		wait_for_frontend_status /customer-portal/healthz 200 "客户自助门户健康检查路径"
		portal_session_status="$(frontend_http_status /customer-portal/api/v1/auth/me)" || \
			fail "无法访问客户自助门户登录状态接口"
		[[ "$portal_session_status" == "401" ]] || \
			fail "客户自助门户登录状态接口返回 ${portal_session_status}，预期未登录状态为 401"
	fi

	if project_configured; then
		wait_for_frontend_status /project_management/healthz 200 "项目管理系统健康检查路径"
		# 就绪检查单独校验：库结构落后于代码时 /readyz 返回 503 并列出待执行迁移。
		# 只探 /healthz 会把"部署时漏跑迁移"放过去，随后任何写操作都只报 500「服务暂不可用」。
		wait_for_frontend_status /project_management/readyz 200 "项目管理系统就绪检查（迁移已应用）"
		project_session_status="$(frontend_http_status /project_management/api/v1/auth/me)" || \
			fail "无法访问项目管理系统登录状态接口"
		[[ "$project_session_status" == "401" ]] || \
			fail "项目管理系统登录状态接口返回 ${project_session_status}，预期未登录状态为 401"
	fi

	if data_analysis_configured && [[ "${data_analysis_ready:-true}" == true ]]; then
		wait_for_frontend_status /data_analysis/healthz 200 "数据看板健康检查路径"
		data_analysis_session_status="$(frontend_http_status /data_analysis/api/v1/auth/me)" || \
			fail "无法访问数据看板登录状态接口"
		[[ "$data_analysis_session_status" == "401" ]] || \
			fail "数据看板登录状态接口返回 ${data_analysis_session_status}，预期未登录状态为 401"
	fi

	log "统一网关校验通过：platform API=401，contract healthz=200，settlement healthz=200，customer healthz=200"
}

start_stack() {
    ensure_platform_env_file
    ensure_contract_env_file true
	ensure_customer_env_file
	ensure_portal_env_file
	ensure_project_env_file
	ensure_data_analysis_env_file
	ensure_settlement_env_file
	validate_all_local_runtime_targets
	validate_all_keycloak_runtimes
	if portal_configured && ! portal_compensation_configured; then
		fail "客户自助门户已接入，但 Portal 补偿 Worker 的映射/角色分配机器凭据不完整；请在应用接入页重试 customer_portal/dev"
	fi
    ensure_catalog_publisher_credentials_consistent
    disable_contract_startup_catalog_sync
    prepare_gateway_config
    # 顺序不可颠倒：先准备候选镜像（普通 up 只补缺失的未登记镜像；--build 只准备候选、
    # 不改写固定记录），再核对运行固定镜像是否存在。若把预检放在前面，up --build 会在
    # 构建前被拦住，而恢复提示又指向同一条命令，形成死循环。
    build_images
    if [[ "$force_build" == true ]]; then
        preflight_candidates_prepared=true
    fi
    preflight_local_license_images
    if project_configured; then
        recover_stale_project_background_state
        configure_local_project_background_build
        local_project_background prepare
    fi
    run_migrations
    bootstrap_admin_if_needed
    log "启动独立 Keycloak 认证容器与 MySQL"
    compose_up_wait "Keycloak" keycloak-db keycloak
    log "启动统一文件网关"
    compose_up_wait "统一文件网关" file-gateway
    # 分阶段启动，避免四个 API 和 frontend 在同一次 Compose wait 中
    # 把下游的短暂冷启动误报为整套部署失败，同时让错误日志能够准确指向服务。
    log "启动基础平台 API 与受控子系统 provisioner"
    compose_up_wait "基础平台 API" subsystem-provisioner api
    verify_local_file_gateway
    # 统一前端必须先于子系统接入步骤可用：subsystem.sh onboard 要经
    # http://localhost:8081/api/v1 登录平台。若把前端排在 CRM 目录发布之后，
    # 平台库一旦缺少接入记录，up 会在发布处中止而 8081 始终起不来，接入与启动互相等待。
    log "启动统一前端（先于子系统接入步骤，保证门户网关可用于首次接入）"
    compose_up_wait "统一前端" frontend
	log "启动结算与开票 API 与后台 Worker"
	compose_up_wait "结算与开票 API" settlement-api
	compose_run up -d --wait --no-deps settlement-worker
    ensure_subsystem_discovery_candidates
    crm_catalog_ready=true
    if ! sync_crm_authorization_catalog; then
        log "跳过客户与商机管理后端与相关 Worker（尚未接入平台）；完成接入后重新执行 up 即可"
    fi
    log "启动合同管理后端前发布当前镜像内嵌授权目录"
    sync_contract_authorization_catalog
    log "启动合同管理后端"
    compose_up_wait "合同管理 API 与 Worker" contract-api contract-worker
    if [[ "$crm_catalog_ready" == true ]]; then
        log "启动客户与商机管理后端"
        compose_up_wait "客户与商机管理后端" customer-api
        start_customer_notification_workers
        log "启动售前预警扫描 Worker"
        compose_run up -d --wait --no-deps customer-presale-alert-worker
        # 售前申请提交依赖独立 Worker 的新鲜心跳。up 现在默认一并启动本地
        # 集成 Mock 和 Worker，避免只启动 customer-api 后页面始终提示 Worker 未就绪。
        log "启动售前投递 Worker 并确认新鲜心跳"
        start_presale_worker --skip-build --skip-migrate
    fi
	if portal_configured; then
		log "启动已接入的客户自助门户后端"
		portal_catalog_ready=true
		if ! sync_portal_authorization_catalog; then
			log "跳过客户自助门户后端与相关 Worker（尚未接入平台）；完成接入后重新执行 up 即可"
		fi
		if [[ "$portal_catalog_ready" == true ]]; then
			# portal-migrate 已在 run_migrations 中串行成功执行；再次让
			# `up --wait` 解析其 service_completed_successfully 依赖会重建一次性
			# 容器，并可能在等待阶段引用已被 Compose 清理的旧容器 ID。
			compose_run up -d --wait --no-deps portal-api
			compose_run up -d --no-deps portal-invite-compensation-worker
		fi
	else
		log "客户自助门户尚未接入，跳过 portal-api；可在应用接入中创建 customer_portal/dev"
	fi
	if project_configured; then
		log "启动已接入的项目管理系统后端"
		# project-migrate 同样已在 run_migrations 中完成，避免 Compose 在
		# `--wait` 时重复重建一次性迁移容器。
		compose_run up -d --wait --no-deps project-api sla-notifier
		if ! local_project_background activate; then
			# 本地未导入厂商 JWS 时 worker 按 PAUSED_LICENSE 暂停，生产状态机会
			# 拒绝完成切换；不放宽状态机，只是不中断其余启动。候选与恢复记录已保留。
			if ! local_project_background_license_paused; then
				fail "项目后台受控切换失败；请检查许可或依赖"
			fi
			log "项目后台因本地未导入许可证保持暂停（PAUSED_LICENSE）；候选已保留，导入许可证后重跑 up 完成切换"
		fi
	else
		log "项目管理系统尚未接入，跳过 project-api；可在应用接入中创建 project_management/dev"
	fi
	if data_analysis_configured; then
		data_analysis_machine_credentials_configured || fail "数据看板已接入，但缺少 CONTRACT_MACHINE_* / PROJECT_MACHINE_* 分离机器凭据；请在应用接入页重新同步 data_analysis/dev，旧 MACHINE_CLIENT_* 不能替代两组新凭据"
		data_analysis_ready=true
		if ! data_analysis_catalog_credentials_live; then
			# dashboard-api 在启动时会自行同步授权目录，凭据失效会 401 并进入重启
			# 循环，令 `up --wait` 以 unhealthy 中止整套部署。这里提前探测并跳过。
			data_analysis_ready=false
			log "跳过数据看板后端（data_analysis 尚未接入本项目平台：授权目录同步令牌返回 ${data_analysis_catalog_status:-无响应}）"
			data_analysis_onboarding_remediation >&2
		fi
		if [[ "$data_analysis_ready" == true ]]; then
			log "启动已接入的数据看板后端（dashboard-api + 聚合 Worker + Metabase）"
			# 业务 Schema 由独立迁移二进制管理；MySQL 初始化只创建 Metabase 元数据库。
			compose_run up -d --wait dashboard-mysql
			compose_run run --rm --no-deps dashboard-migrate
			compose_run up -d --wait --no-deps dashboard-api aggregation-worker alert-worker metabase
		fi
	else
		log "数据看板尚未接入，跳过 dashboard-api；可在应用接入中创建 data_analysis/dev"
	fi
    # 统一前端已在子系统接入前启动，这里只做网关校验。
    verify_gateway_routes
    compose_run ps
    log "统一访问地址：${frontend_public_origin}"
    log "合同管理前端：${frontend_public_origin}/contract_management/"
	log "结算与开票前端：${frontend_public_origin}/settlement/"
	log "客户与商机管理前端：${frontend_public_origin}/customer-opportunity/"
	if portal_configured; then log "客户自助门户：${frontend_public_origin}/customer-portal/"; fi
	if project_configured; then log "项目管理系统：${frontend_public_origin}/project_management/"; fi
	if data_analysis_configured; then log "数据看板与统计分析：${frontend_public_origin}/data_analysis/"; fi
}

refresh_platform_api() {
    # 仅刷新基础平台后端：当前 API 不会包含新增路由时，使用该命令即可。
    # compose() 固定携带合同环境文件，所以这里只保证它存在，不校验其 OIDC 占位符。
    prepare_operational_env
    ensure_catalog_publisher_credentials_consistent
    prepare_go_backend_base_images "基础平台后端"

    log "重新构建基础平台后端镜像（不构建 frontend、contract-api 或 customer-api）"
    # 固定记录预检：重建后仍会按已批准镜像重启，先确认固定镜像与批准可用。
    preflight_local_license_images
    refuse_pinned_rebuild api
    COMPOSE_PARALLEL_LIMIT=1 compose --profile bootstrap --ansi never build api

    log "确认基础平台控制面依赖，并执行基础平台数据库迁移"
    ensure_platform_control_dependencies
    compose_run run --rm --no-deps migrate ./migrate

    # api 依赖 provisioner 的 Unix Socket。先以新镜像重建 provisioner，再重启 api，
    # 避免 API 启动后连接到过期 helper 的情况。
    log "重建受控子系统 provisioner"
    compose_run up -d --wait --no-deps subsystem-provisioner
    log "重建基础平台 API"
    compose_run up -d --wait --no-deps api
    verify_local_file_gateway
    report_candidate_drift
    compose_run ps api subsystem-provisioner docker-socket-proxy mysql file-gateway file-gateway-mysql
    log "基础平台后端已刷新；统一前端、合同/CRM 后端、可选门户后端和现有统一登录接入配置保持不变"
}

refresh_unified_frontend() {
    prepare_operational_env
    prepare_frontend_base_images
    prepare_gateway_config

    log "重新构建统一前端镜像（基础平台 + 合同管理 + 客户与商机管理 + 客户自助门户 + 项目管理系统前端）"
    # 固定记录预检：重建后仍会按已批准镜像重启，先确认固定镜像与批准可用。
    preflight_local_license_images
    refuse_pinned_rebuild frontend
    COMPOSE_PARALLEL_LIMIT=1 compose --ansi never build frontend
    log "重建统一 frontend 容器；平台/合同/CRM 后端、可选门户/项目后端和统一登录接入配置保持不变"
    compose_run up -d --wait --no-deps frontend
    verify_gateway_routes
    report_candidate_drift
    compose_run ps frontend
}

refresh_contract_backend() {
    ensure_platform_env_file
    ensure_contract_env_file true
	ensure_customer_env_file
	ensure_project_env_file
	ensure_portal_env_file
	ensure_settlement_env_file
	validate_local_runtime_target "合同管理" "$contract_env_file" contract_management "$(runtime_target_environment "$contract_env_file" contract_management OIDC_CLIENT_ID)" OIDC_CLIENT_ID
    ensure_catalog_publisher_credentials_consistent
    disable_contract_startup_catalog_sync
    prepare_go_backend_base_images "合同管理后端"

    # 平台 API 读取同一组 publisher 凭据为受控 update 流程服务。若脚本刚完成
    # 凭据归一化，必须重建 API 与 provisioner，不能让旧容器继续持有旧环境。
    ensure_platform_control_dependencies
    log "重建基础平台 API 与 provisioner，使授权目录发布凭据保持一致"
    compose_run up -d --wait --force-recreate --no-deps subsystem-provisioner api

    log "重新构建合同管理后端镜像（不构建 frontend 或基础平台 api）"
    # 固定记录预检：重建后仍会按已批准镜像重启，先确认固定镜像与批准可用。
    preflight_local_license_images
    refuse_pinned_rebuild contract-api contract-worker
    COMPOSE_PARALLEL_LIMIT=1 compose --ansi never build contract-api contract-worker
    log "启动合同数据库与 Temporal，并执行合同管理数据库迁移"
    compose_run up -d --wait contract-mysql temporal
    compose_run run --rm --no-deps contract-migrate
	log "发布当前合同镜像内嵌的授权目录"
	sync_contract_authorization_catalog
    log "重建 contract-api 与 contract-worker 容器；统一前端、基础平台后端和统一登录接入配置保持不变"
    compose_run up -d --wait --no-deps contract-api contract-worker
    verify_gateway_routes
    report_candidate_drift
    compose_run ps contract-api contract-worker contract-mysql temporal
}

refresh_customer_backend() {
    ensure_platform_env_file
    ensure_contract_env_file false
	ensure_customer_env_file
	ensure_portal_env_file
	ensure_settlement_env_file
	validate_local_runtime_target "客户与商机管理" "$customer_env_file" customer_and_opportunity "$(runtime_target_environment "$customer_env_file" customer_and_opportunity OIDC_CLIENT_ID)" OIDC_CLIENT_ID
	validate_customer_local_service_clients
    prepare_go_backend_base_images "客户与商机管理后端"

    log "重新构建客户与商机管理后端镜像（不构建 frontend、基础平台 api 或 contract-api）"
    # 固定记录预检：重建后仍会按已批准镜像重启，先确认固定镜像与批准可用。
    preflight_local_license_images
    refuse_pinned_rebuild customer-api customer-presale-alert-worker
    COMPOSE_PARALLEL_LIMIT=1 compose --ansi never build customer-api customer-presale-alert-worker
	ensure_crm_discovery_candidate
    sync_crm_authorization_catalog
    prepare_gateway_config
    log "重新构建统一前端网关，使客户与商机管理路径转发到 customer-api"
    COMPOSE_PARALLEL_LIMIT=1 compose --ansi never build frontend
    log "启动客户与商机数据库并执行 CRM 清单迁移"
    compose_run up -d --wait customer-mysql
    compose_run run --rm --no-deps customer-migrate
	log "重建 customer-api 与统一前端网关；另外两个后端和统一登录接入配置保持不变"
	compose_run up -d --wait --no-deps customer-api customer-presale-alert-worker
	start_customer_notification_workers
    compose_run up -d --wait --no-deps frontend
    verify_gateway_routes
    report_candidate_drift
	compose_run ps customer-api customer-mysql
}

# CRM 启动时会严格校验基础平台已发布目录的 claims_role_config_hash。
# 所有启动入口都必须在启动 customer-api 前完成同一镜像目录的发布，
# 否则服务会因旧目录进入重启循环，而发布命令也无法稳定执行。
sync_crm_authorization_catalog() {
    local local_crm_hash
    local_crm_hash="$(compose_run run --rm --no-deps customer-api ./authz-catalog print crm \
        | awk -F= '$1 == "claims_role_config_hash" { print $2; exit }')"
    [[ "$local_crm_hash" =~ ^sha256:[a-f0-9]{64}$ ]] || fail "无法从本地 CRM 镜像读取有效授权目录哈希"
    export OIDC_ROLE_CONFIG_HASH="$local_crm_hash"
    replace_line_in_file "$customer_env_file" OIDC_ROLE_CONFIG_HASH "$OIDC_ROLE_CONFIG_HASH"
    log "已写入当前 CRM 授权目录哈希到客户与商机运行时配置：$OIDC_ROLE_CONFIG_HASH"
    log "使用本地 CRM 镜像内嵌授权目录哈希：$OIDC_ROLE_CONFIG_HASH"
    log "启动 CRM 前发布客户与商机管理授权目录"
    if compose_run run --rm --no-deps -e OIDC_ROLE_CONFIG_HASH="$OIDC_ROLE_CONFIG_HASH" customer-api ./authz-catalog publish crm; then
        crm_catalog_ready=true
    else
        crm_catalog_ready=false
        log "CRM 授权目录发布失败：customer_and_opportunity 尚未接入本项目平台（平台库重建后 Application/OAuth Client 会一并消失）"
        crm_onboarding_remediation >&2
        return 1
    fi
}

# 合同 API 的本地启动同步被有意关闭，目录只能由受控部署步骤发布。必须使用
# 即将启动的同一镜像内嵌清单，避免平台目录仍停留在旧版本而新 API 拒绝登录。
sync_contract_authorization_catalog() {
	compose_run run --rm --no-deps contract-api ./authz-catalog publish
}

# crm_onboarding_remediation 给出可直接执行的首次接入命令：发布失败的原始输出只有
# "platform authorization catalog token returned HTTP 401"，使用者无从判断该做什么。
crm_onboarding_remediation() {
    cat <<'REMEDIATION'

请先完成一次性应用接入，然后重新执行 up：

  bash scripts/subsystem.sh onboard \
    --application-code customer_and_opportunity \
    --application-name 客户与商机管理系统 \
    --environment dev \
    --api-base-url http://localhost:8081/api/v1 \
    --platform-origin http://localhost:8081 \
    --public-base-url http://localhost:8081 \
    --upstream-url http://customer-api:8090 \
    --path-prefix /customer-opportunity \
    --account <平台管理员账号>

门户网关 http://localhost:8081（up 会先启动统一前端）可直接执行上面的命令。
REMEDIATION
}

# Portal 与 CRM 使用独立的授权目录和运行时哈希。Portal 镜像升级后，必须在
# portal-api 启动前先把同一镜像内嵌的目录发布到平台，否则 Portal 会因目录
# 哈希不一致直接退出并被 Compose 标记为 unhealthy。
sync_portal_authorization_catalog() {
    local local_portal_hash
    local_portal_hash="$(compose_run run --rm --no-deps portal-api ./authz-catalog print portal \
        | awk -F= '$1 == "claims_role_config_hash" { print $2; exit }')"
    [[ "$local_portal_hash" =~ ^sha256:[a-f0-9]{64}$ ]] || fail "无法从本地 Portal 镜像读取有效授权目录哈希"
    export PORTAL_ROLE_CONFIG_HASH="$local_portal_hash"
    replace_line_in_file "$portal_env_file" PORTAL_ROLE_CONFIG_HASH "$PORTAL_ROLE_CONFIG_HASH"
    log "已写入当前 Portal 授权目录哈希到客户门户运行时配置：$PORTAL_ROLE_CONFIG_HASH"
    log "启动 Portal 前发布客户自助门户授权目录"
    if compose_run run --rm --no-deps -e PORTAL_ROLE_CONFIG_HASH="$PORTAL_ROLE_CONFIG_HASH" portal-api ./authz-catalog publish portal; then
        portal_catalog_ready=true
    else
        portal_catalog_ready=false
        log "Portal 授权目录发布失败：customer_portal 尚未接入本项目平台（平台库重建后 Application/OAuth Client 会一并消失）"
        portal_onboarding_remediation >&2
        return 1
    fi
}

# portal_onboarding_remediation 给出可直接执行的首次接入命令，与 CRM 同理。
portal_onboarding_remediation() {
    cat <<'REMEDIATION'

请先完成一次性客户自助门户接入，然后重新执行 up：

  bash scripts/subsystem.sh onboard \
    --preset customer-portal-local \
    --account <平台管理员账号>

门户网关 http://localhost:8081（up 会先启动统一前端）可直接执行上面的命令。
REMEDIATION
}

refresh_portal_backend() {
	ensure_platform_env_file
	ensure_contract_env_file false
	ensure_customer_env_file
	ensure_portal_env_file
	ensure_project_env_file
	ensure_settlement_env_file
	portal_configured || fail "客户自助门户尚未完成 customer_portal/dev 应用接入，不能启动 portal-api"
	validate_local_runtime_target "客户自助门户" "$portal_env_file" customer_portal "$(runtime_target_environment "$portal_env_file" customer_portal PORTAL_OIDC_CLIENT_ID)" PORTAL_OIDC_CLIENT_ID
	portal_compensation_configured || fail "Portal 补偿 Worker 的映射/角色分配机器凭据不完整；请在应用接入页重试 customer_portal/dev"
	prepare_go_backend_base_images "客户自助门户后端"

	log "重新构建客户自助门户独立后端镜像"
	# 固定记录预检：重建后仍会按已批准镜像重启，先确认固定镜像与批准可用。
	preflight_local_license_images
    refuse_pinned_rebuild portal-api
	COMPOSE_PARALLEL_LIMIT=1 compose --ansi never build portal-api
	prepare_gateway_config
	log "刷新统一前端网关中的客户门户路由"
	COMPOSE_PARALLEL_LIMIT=1 compose --ansi never build frontend
	log "启动门户数据库并执行 Portal 清单迁移"
	compose_run up -d --wait portal-mysql
	compose_run run --rm --no-deps portal-migrate
	log "重建 portal-api、Portal 补偿 Worker 与统一前端"
	compose_run up -d --wait --no-deps portal-api
	compose_run up -d --no-deps portal-invite-compensation-worker
	compose_run up -d --wait --no-deps frontend
	verify_gateway_routes
    report_candidate_drift
	compose_run ps portal-api portal-invite-compensation-worker portal-mysql frontend
}

refresh_project_backend() {
	ensure_platform_env_file
	ensure_contract_env_file false
	ensure_customer_env_file
	ensure_portal_env_file
	ensure_project_env_file
	ensure_settlement_env_file
	project_configured || fail "项目管理系统尚未完成 project_management/dev 应用接入，不能启动 project-api"
	validate_local_runtime_target "项目管理" "$project_env_file" project_management "$(runtime_target_environment "$project_env_file" project_management OIDC_CLIENT_ID)" OIDC_CLIENT_ID
	prepare_go_backend_base_images "项目管理系统后端"

	log "重新构建项目管理系统独立后端镜像"
	# 固定记录预检：重建后仍会按已批准镜像重启，先确认固定镜像与批准可用。
	preflight_local_license_images
    refuse_pinned_rebuild project-api sla-notifier
	COMPOSE_PARALLEL_LIMIT=1 compose --profile project --ansi never build project-api sla-notifier
	recover_stale_project_background_state
	configure_local_project_background_build
	local_project_background prepare
	log "启动项目管理系统 MySQL 并执行项目清单迁移"
	compose_run up -d --wait project-mysql temporal
	compose_run run --rm --no-deps project-migrate
	log "重建 project-api 容器；统一前端、基础平台后端和统一登录接入配置保持不变"
	compose_run up -d --wait --no-deps project-api sla-notifier
	if ! local_project_background activate; then
		local_project_background_license_paused ||
			fail "项目后台受控切换失败；请检查许可或依赖"
		log "项目后台因本地未导入许可证保持暂停（PAUSED_LICENSE）；候选已保留，导入许可证后重跑 refresh-project-api 完成切换"
	fi
	verify_gateway_routes
    report_candidate_drift
	compose_run ps project-api sla-notifier project-mysql
}

refresh_data_analysis_backend() {
	ensure_platform_env_file
	ensure_contract_env_file false
	ensure_customer_env_file
	ensure_portal_env_file
	ensure_project_env_file
	ensure_data_analysis_env_file
	ensure_settlement_env_file
	data_analysis_configured || fail "数据看板尚未完成 data_analysis/dev 应用接入，不能启动 dashboard-api"
	data_analysis_machine_credentials_configured || fail "数据看板已接入，但缺少 CONTRACT_MACHINE_* / PROJECT_MACHINE_* 分离机器凭据；请在应用接入页重新同步 data_analysis/dev，旧 MACHINE_CLIENT_* 不能替代两组新凭据"
	prepare_go_backend_base_images "数据看板后端"

	log "重新构建数据看板独立后端镜像"
	# 固定记录预检：重建后仍会按已批准镜像重启，先确认固定镜像与批准可用。
	preflight_local_license_images
    refuse_pinned_rebuild dashboard-migrate dashboard-api aggregation-worker alert-worker
	COMPOSE_PARALLEL_LIMIT=1 compose --profile data-analysis --ansi never build dashboard-migrate dashboard-api aggregation-worker alert-worker
	log "启动数据看板 MySQL并执行版本化迁移"
	compose_run up -d --wait dashboard-mysql
	compose_run run --rm --no-deps dashboard-migrate
	log "重建 dashboard-api 与 Worker；统一前端、基础平台后端和统一登录接入配置保持不变"
	compose_run up -d --wait --no-deps dashboard-api aggregation-worker alert-worker metabase
	verify_gateway_routes
    report_candidate_drift
	compose_run ps dashboard-api aggregation-worker alert-worker dashboard-mysql
}

start_presale_worker() {
	local skip_build=false skip_migrate=false option
	for option in "$@"; do
		case "$option" in
			--skip-build) skip_build=true ;;
			--skip-migrate) skip_migrate=true ;;
			*) fail "start-presale-worker 不支持参数：$option" ;;
		esac
	done
	ensure_platform_env_file
	ensure_contract_env_file false
	ensure_customer_env_file
	ensure_portal_env_file
	ensure_project_env_file
	ensure_settlement_env_file
	validate_local_runtime_target "客户与商机管理" "$customer_env_file" customer_and_opportunity "$(runtime_target_environment "$customer_env_file" customer_and_opportunity OIDC_CLIENT_ID)" OIDC_CLIENT_ID
	[[ -f "$presale_worker_env_file" ]] || \
		fail "售前投递 Worker 环境文件不存在：${presale_worker_env_file}；请从 customer_and_opportunity/.env.presale-worker.example 复制并填写实际环境值"
	[[ -s "$presale_worker_env_file" ]] || \
		fail "售前投递 Worker 环境文件为空：${presale_worker_env_file}"
	if [[ "$skip_build" != true ]]; then
		prepare_go_backend_base_images "售前投递 Worker"
		log "构建售前投递 Worker 与本地集成 Mock；统一认证、审批和 PMS 地址仅从指定环境文件注入"
		COMPOSE_PARALLEL_LIMIT=1 compose --profile presale-worker --ansi never build presale-worker presale-integration-mock
	fi
	if [[ "$skip_migrate" != true ]]; then
		log "启动客户与商机数据库并执行 CRM 清单迁移"
		compose_run --profile presale-worker up -d --wait customer-mysql
		compose_run --profile presale-worker run --rm --no-deps customer-migrate
	fi
	# presale-worker 使用 --no-deps 启动，必须在此前显式拉起 Temporal；否则
	# 单独执行 start-presale-worker（尤其是 --skip-migrate）时会连接失败。
	compose_run --profile presale-worker up -d --wait temporal
	log "启动本地售前集成 Mock（仅开发联调使用）"
	compose_run --profile presale-worker up -d --wait presale-integration-mock
	log "启动售前投递 Worker"
	compose_run --profile presale-worker up -d --no-deps presale-worker

	local attempt heartbeat
	# 获许可 worker 在激活前或到期后刻意暂停新业务；应观察真实当前运行时 ACK，不以业务活动作为依据。
	if [[ "$(env_value "${project_root}/runtime/license-presale-worker.env" COMMERCIAL_LICENSE_ENABLED)" == "true" ]]; then
		local confirmation
		for attempt in $(seq 1 45); do
			confirmation="$(compose_run exec -T mysql sh -c 'MYSQL_PWD="$MYSQL_PASSWORD" mysql --protocol=TCP -h 127.0.0.1 -u"$MYSQL_USER" -D "$MYSQL_DATABASE" -Nse "SELECT COUNT(*)=9 AND COALESCE(MIN(m.ready_at IS NOT NULL AND m.ack_at IS NOT NULL AND m.issued_revision=a.revision AND m.ack_revision=a.revision AND m.ready_at >= UTC_TIMESTAMP(3) - INTERVAL 60 SECOND AND m.ack_at >= UTC_TIMESTAMP(3) - INTERVAL 60 SECOND),0) FROM license_runtime_member m JOIN license_runtime_application a ON a.application=m.application WHERE m.application=0x637573746f6d65725f616e645f6f70706f7274756e697479 AND m.environment=0x646576 AND m.retired_at IS NULL"' 2>/dev/null || true)"
			if [[ "$confirmation" == "1" ]]; then
				log "本地 CRM 九组件当期授权确认通过；业务执行继续由商业授权决定"
				return 0
			fi
			sleep 1
		done
		fail "本地 CRM 组件尚未完成当期授权确认；请检查授权同步状态"
	fi
	for attempt in $(seq 1 20); do
		heartbeat="$(
			compose_run exec -T customer-mysql sh -c \
				'MYSQL_PWD="$MYSQL_PASSWORD" mysql --protocol=TCP -h 127.0.0.1 -u"$MYSQL_USER" -D "$MYSQL_DATABASE" -Nse "SELECT EXISTS(SELECT 1 FROM crm_worker_heartbeats WHERE worker_type=0x70726573616c655f64656c6976657279 AND heartbeat_at >= UTC_TIMESTAMP(3) - INTERVAL 15 SECOND)"' \
				2>/dev/null || true
		)"
		if [[ "$heartbeat" == "1" ]]; then
			compose_run --profile presale-worker ps presale-worker
			log "售前投递 Worker 已启动，数据库新鲜心跳已确认"
			return 0
		fi
		sleep 1
	done

	compose_run --profile presale-worker ps -a presale-worker >&2 || true
	compose_run --profile presale-worker logs --tail 80 presale-worker >&2 || true
	fail "售前投递 Worker 未在 20 秒内产生新鲜心跳；申请入口保持安全关闭"
}

start_presale_alert_worker() {
	ensure_platform_env_file
	ensure_customer_env_file
	ensure_settlement_env_file
	validate_local_runtime_target "客户与商机管理" "$customer_env_file" customer_and_opportunity "$(runtime_target_environment "$customer_env_file" customer_and_opportunity OIDC_CLIENT_ID)" OIDC_CLIENT_ID
	prepare_go_backend_base_images "售前预警扫描 Worker"
	log "构建售前预警扫描 Worker"
	COMPOSE_PARALLEL_LIMIT=1 compose --profile customer --ansi never build customer-presale-alert-worker
	log "启动客户与商机数据库并执行 CRM 清单迁移"
	compose_run up -d --wait customer-mysql
	compose_run run --rm --no-deps customer-migrate
	log "启动售前预警扫描 Worker"
	compose_run up -d --wait --no-deps customer-presale-alert-worker
	compose_run ps customer-presale-alert-worker customer-mysql
}

prepare_operational_env() {
    ensure_platform_env_file
    ensure_contract_env_file false
	ensure_customer_env_file
	ensure_portal_env_file
	ensure_project_env_file
	ensure_settlement_env_file
}

case "$command_name" in
    up|restart)
        if [[ "$core_only" == true ]]; then
            start_control_plane_only
        else
            start_stack
        fi
        ;;
    build-images)
        build_candidate_images
        ;;
    dev-up)
        dev_up
        ;;
    dev-approve)
        dev_approve "$@"
        ;;
    prepare-discovery)
        # 优先走探测：为六个子系统创建带标签的候选容器（不启动业务进程），
        # 使界面「应用接入 → 探测子系统」可以立即发现尚未接入的子系统。
        ensure_platform_env_file
        ensure_contract_env_file true
        preflight_local_license_images
        ensure_subsystem_discovery_candidates
        ;;
    bootstrap-admin)
        # 为已在运行的控制面追加首个超级管理员。已初始化时只探测后返回，不重复创建。
        # 业务子系统没有独立账号密码，全部通过平台账号走 SSO。
        ensure_platform_env_file
        ensure_contract_env_file true
        bootstrap_admin_if_needed
        ;;
    down)
        prepare_operational_env
        local_project_background teardown
        # 清理旧版本或已从当前 Compose 文件移除的服务容器。否则残留的
        # profile/worker 容器会继续占用项目网络，导致 `docker compose down`
        # 报 Resource is still in use，脚本无法完成收尾。
        remove_legacy_split_runtime_containers
        if [[ "$remove_volumes" == true ]]; then
            compose_run down --volumes --remove-orphans
        else
            compose_run down --remove-orphans
        fi
        # Compose 在发现残留 endpoint 时可能已经输出过一次占用提示；容器
        # 清理后再次尝试移除网络，让 down 最终状态与实际资源一致。
        local_project_background stopped
        docker network rm "${compose_project}_default" >/dev/null 2>&1 || true
        ;;
    stop)
        prepare_operational_env
        local_project_background teardown
        compose_run stop
        local_project_background stopped
        ;;
    ps)
        prepare_operational_env
        compose_run ps
        ;;
    logs)
        prepare_operational_env
        log_args=(logs)
        [[ -n "$log_since" ]] && log_args+=(--since "$log_since")
        [[ -n "$log_tail" ]] && log_args+=(--tail "$log_tail")
        [[ "$log_follow" == true ]] && log_args+=(-f)
        if ((${#log_services[@]} > 0)); then
            compose_run "${log_args[@]}" "${log_services[@]}"
        else
            compose_run "${log_args[@]}"
        fi
        ;;
	config)
        ensure_platform_env_file
        ensure_contract_env_file true
		ensure_customer_env_file
		ensure_portal_env_file
		ensure_project_env_file
		ensure_settlement_env_file
		validate_all_local_runtime_targets
		validate_all_keycloak_runtimes
		if portal_configured && ! portal_compensation_configured; then
			fail "Portal 补偿 Worker 的映射/角色分配机器凭据不完整；请在应用接入页重试 customer_portal/dev"
		fi
        ensure_catalog_publisher_credentials_consistent
        disable_contract_startup_catalog_sync
        compose_run config --quiet
        log "Compose 配置校验通过"
        ;;
    verify)
        prepare_operational_env
        verify_gateway_routes
        ;;
    refresh-api)
        refresh_platform_api
        ;;
    refresh-frontend)
        refresh_unified_frontend
        ;;
    refresh-contract-api)
        refresh_contract_backend
        ;;
	refresh-customer-api)
		refresh_customer_backend
		;;
	refresh-portal-api)
		refresh_portal_backend
		;;
	refresh-project-api)
		refresh_project_backend
		;;
	refresh-data-analysis-api)
		refresh_data_analysis_backend
		;;
	start-presale-worker)
		start_presale_worker "$@"
		;;
	start-presale-alert-worker)
		start_presale_alert_worker "$@"
		;;
esac
