# 运行配置读取、OIDC 运行时门禁与应用接入目标校验（仅函数定义，加载零副作用）。

project_configured() {
	# 项目后端启动强校验 OIDC 四项接入值；缺少任何一项都视为尚未完成 project_management/dev 接入。
	[[ -n "$(env_value "$project_env_file" OIDC_CLIENT_ID)" ]] &&
		[[ -n "$(env_value "$project_env_file" OIDC_CLIENT_SECRET)" ]] &&
		[[ -n "$(env_value "$project_env_file" OIDC_REDIRECT_URI)" ]] &&
		[[ -n "$(env_value "$project_env_file" OIDC_TENANT_ID)" ]]
}

data_analysis_configured() {
	# 数据看板后端强校验 OIDC 四项接入值；缺少任何一项都视为尚未完成 data_analysis/dev 接入。
	[[ -n "$(env_value "$data_analysis_env_file" OIDC_CLIENT_ID)" ]] &&
		[[ -n "$(env_value "$data_analysis_env_file" OIDC_CLIENT_SECRET)" ]] &&
		[[ -n "$(env_value "$data_analysis_env_file" OIDC_REDIRECT_URI)" ]] &&
		[[ -n "$(env_value "$data_analysis_env_file" OIDC_TENANT_ID)" ]]
}

data_analysis_machine_credentials_configured() {
	[[ -n "$(env_value "$data_analysis_env_file" CONTRACT_MACHINE_CLIENT_ID)" ]] &&
		[[ -n "$(env_value "$data_analysis_env_file" CONTRACT_MACHINE_CLIENT_SECRET)" ]] &&
		[[ -n "$(env_value "$data_analysis_env_file" PROJECT_MACHINE_CLIENT_ID)" ]] &&
		[[ -n "$(env_value "$data_analysis_env_file" PROJECT_MACHINE_CLIENT_SECRET)" ]]
}

# data_analysis_catalog_credentials_live 判断数据看板本地配置里的授权目录同步凭据
# 是否仍被平台接受。平台库重建后 .env.local 仍保留旧凭据，只看 env 会把"已失效"
# 误判成"已接入"；dashboard-api 启动时才在目录同步处 401 并进入重启循环，最终以
# unhealthy 中止整套 up。
data_analysis_catalog_credentials_live() {
	data_analysis_catalog_status=""
	# 目录同步关闭时 dashboard-api 不会申请令牌，也就不存在这类启动失败。
	[[ "$(env_value "$data_analysis_env_file" PLATFORM_AUTHORIZATION_CATALOG_SYNC_ENABLED)" == "true" ]] || return 0
	data_analysis_catalog_status="$(platform_catalog_token_status \
		"$(env_value "$data_analysis_env_file" PLATFORM_AUTHORIZATION_CATALOG_CLIENT_ID)" \
		"$(env_value "$data_analysis_env_file" PLATFORM_AUTHORIZATION_CATALOG_CLIENT_SECRET)")" || data_analysis_catalog_status=""
	[[ "$data_analysis_catalog_status" == "200" ]]
}

# data_analysis_onboarding_remediation 给出可直接执行的接入提示。
data_analysis_onboarding_remediation() {
	cat <<'REMEDIATION'

请先在基础平台完成一次性数据看板接入，然后重新执行 up：

  bash scripts/subsystem.sh onboard \
    --application-code data_analysis \
    --application-name 数据看板与统计分析 \
    --environment dev \
    --api-base-url http://localhost:8081/api/v1 \
    --platform-origin http://localhost:8081 \
    --public-base-url http://localhost:8081 \
    --upstream-url http://dashboard-api:8080 \
    --path-prefix /data_analysis \
    --account <平台管理员账号>

接入完成后按提示回填 data_analysis/.env.local 的 OIDC_* 与
PLATFORM_AUTHORIZATION_CATALOG_* 凭据，再重新执行 up。
REMEDIATION
}

portal_configured() {
	# 不能只看镜像或数据库是否存在；四项接入产物齐全才允许把 Portal 纳入默认 profile 启动。
	[[ -n "$(env_value "$portal_env_file" PORTAL_OIDC_CLIENT_ID)" ]] &&
		[[ -n "$(env_value "$portal_env_file" PORTAL_OIDC_CLIENT_SECRET)" ]] &&
		[[ -n "$(env_value "$portal_env_file" PORTAL_OIDC_TENANT_ID)" ]] &&
		[[ -n "$(env_value "$portal_env_file" PORTAL_ROLE_CONFIG_HASH)" ]]
}

ensure_catalog_publisher_credentials_consistent() {
    local runtime_id runtime_secret platform_id platform_secret
    runtime_id="$(env_value "$contract_env_file" PLATFORM_AUTHORIZATION_CATALOG_CLIENT_ID)"
    runtime_secret="$(env_value "$contract_env_file" PLATFORM_AUTHORIZATION_CATALOG_CLIENT_SECRET)"
    platform_id="$(env_value "$env_file" CONTRACT_CATALOG_PUBLISHER_CLIENT_ID)"
    platform_secret="$(env_value "$env_file" CONTRACT_CATALOG_PUBLISHER_CLIENT_SECRET)"

    if [[ -n "$platform_id" && -n "$platform_secret" ]]; then
        if [[ "$platform_id" != "$runtime_id" || "$platform_secret" != "$runtime_secret" ]]; then
            # 平台环境中的长期凭据是既有 OAuth 客户端的运维记录。子系统配置可能
            # 来自更早一次接入，必须由平台向子系统同步，不能用已失效值反向覆盖。
            replace_line_in_file "$contract_env_file" PLATFORM_AUTHORIZATION_CATALOG_CLIENT_ID "$platform_id"
            replace_line_in_file "$contract_env_file" PLATFORM_AUTHORIZATION_CATALOG_CLIENT_SECRET "$platform_secret"
            log "已将平台保存的有效授权目录发布凭据同步到合同后端运行配置"
        fi
        return 0
    fi
    if [[ -n "$runtime_id" && -n "$runtime_secret" ]]; then
        # 兼容首次接入：旧平台环境尚未记录 publisher 凭据，但 provisioner 已将
        # 一次性明文安全写入子系统环境文件。此时补齐平台侧的长期运维记录。
        replace_line_in_file "$env_file" CONTRACT_CATALOG_PUBLISHER_CLIENT_ID "$runtime_id"
        replace_line_in_file "$env_file" CONTRACT_CATALOG_PUBLISHER_CLIENT_SECRET "$runtime_secret"
        log "已从首次接入配置补齐平台授权目录发布凭据"
    fi
}

runtime_value_configured() {
	local value="${1:-}"
	[[ -n "$value" && "$value" != PENDING_ONBOARDING && "$value" != REPLACE_WITH_* ]]
}

# 新环境的浏览器 OIDC 必须由 Keycloak 提供；基础平台只提供在线
# authorization-context，不再充当子系统的 token issuer。以下校验只对已经
# 写入浏览器 Client 的运行时文件生效，因此首次接入前仍可以先启动控制面。
trim_trailing_slash() {
    local value="${1:-}"
    while [[ "$value" == */ ]]; do value="${value%/}"; done
    printf '%s' "$value"
}

valid_http_origin() {
    local value
    value="$(trim_trailing_slash "${1:-}")"
    [[ "$value" =~ ^https?://[^/@?#]+(:[0-9]{1,5})?$ ]]
}

keycloak_realm_issuer() {
    local public_url realm
    if [[ "${PUBLIC_HTTPS_ENABLED:-false}" == "true" ]]; then
        printf '%s' "$PUBLIC_KEYCLOAK_ISSUER"
        return 0
    fi
    public_url="$(trim_trailing_slash "$(env_value "$env_file" KEYCLOAK_PUBLIC_URL)")"
    realm="$(env_value "$env_file" KEYCLOAK_REALM)"
    runtime_value_configured "$public_url" || fail "KEYCLOAK_PUBLIC_URL 未配置，无法校验子系统 OIDC 运行时"
    runtime_value_configured "$realm" || fail "KEYCLOAK_REALM 未配置，无法校验子系统 OIDC 运行时"
    valid_http_origin "$public_url" || fail "KEYCLOAK_PUBLIC_URL 必须是 HTTP(S) origin：${public_url}"
    printf '%s/realms/%s' "$public_url" "$realm"
}

keycloak_internal_origin() {
    local value
    value="$(trim_trailing_slash "$(env_value "$env_file" KEYCLOAK_INTERNAL_URL)")"
    [[ -n "$value" ]] || value="http://keycloak:8080"
    valid_http_origin "$value" || fail "KEYCLOAK_INTERNAL_URL 必须是容器可达的 HTTP(S) origin：${value}"
    printf '%s' "$value"
}

validate_runtime_value() {
    local description="$1" key="$2" value="$3"
    runtime_value_configured "$value" || fail "${description}运行时 ${key} 未配置或仍是占位符；请先在应用接入页同步 Keycloak Client"
}

# 对一份已接入的子系统运行时文件执行同一套 V2 门禁：
# - 浏览器 issuer 必须是公开 Keycloak Realm；
# - 后通道只能是 Compose 私网 Keycloak origin，不能携带 realm/path；
# - redirect、client、租户、应用/环境自然键和角色目录哈希必须成组一致；
# - online authorization-context 由 Compose 固定注入 platform-api；运行时平台
#   地址只允许是统一入口或 Compose 私网平台 API，不能误配为 Keycloak。
validate_keycloak_runtime() {
    local description="$1" runtime_file="$2" application_code="$3" environment_code="$4"
    local issuer_key="$5" backchannel_key="$6" client_key="$7" secret_key="$8" redirect_key="$9" tenant_key="${10}"
    local platform_base_key="${11}" expected_redirect="${12}" hash_key="${13:-}"
    local issuer backchannel client_id client_secret redirect_uri tenant_id platform_base hash_value
    local expected_issuer expected_backchannel expected_client_id

    client_id="$(env_value "$runtime_file" "$client_key")"
    # 尚未由控制面创建 Client 的可选子系统不会启动；这里不把初始接入过程
    # 误判为错误。只要已有 Client，就必须通过完整门禁。
    runtime_value_configured "$client_id" || return 0

    issuer="$(env_value "$runtime_file" "$issuer_key")"
    backchannel="$(trim_trailing_slash "$(env_value "$runtime_file" "$backchannel_key")")"
    client_secret="$(env_value "$runtime_file" "$secret_key")"
    redirect_uri="$(env_value "$runtime_file" "$redirect_key")"
    tenant_id="$(env_value "$runtime_file" "$tenant_key")"
    platform_base="$(trim_trailing_slash "$(env_value "$runtime_file" "$platform_base_key")")"
    expected_issuer="$(keycloak_realm_issuer)"
    expected_backchannel="$(keycloak_internal_origin)"
    expected_client_id="${application_code}-${environment_code}-web"

    validate_runtime_value "$description" "$issuer_key" "$issuer"
    validate_runtime_value "$description" "$backchannel_key" "$backchannel"
    validate_runtime_value "$description" "$secret_key" "$client_secret"
    validate_runtime_value "$description" "$redirect_key" "$redirect_uri"
    validate_runtime_value "$description" "$tenant_key" "$tenant_id"
    validate_runtime_value "$description" "$platform_base_key" "$platform_base"
    if [[ "${PUBLIC_HTTPS_ENABLED:-false}" != "true" ]]; then
        [[ "$(trim_trailing_slash "$issuer")" == "$expected_issuer" ]] || \
            fail "${description}运行时 ${issuer_key}=${issuer}；本地 Keycloak Realm 应为 ${expected_issuer}"
    fi
    [[ "$backchannel" == "$expected_backchannel" ]] || \
        fail "${description}运行时 ${backchannel_key}=${backchannel}；应为 Compose 私网地址 ${expected_backchannel}（不得包含 /realms 路径）"
    [[ "$client_id" == "$expected_client_id" ]] || \
        fail "${description}运行时 ${client_key}=${client_id}；本地 ${environment_code} 环境要求 ${expected_client_id}"
    if [[ "${PUBLIC_HTTPS_ENABLED:-false}" != "true" ]]; then
        [[ "$redirect_uri" == "$expected_redirect" ]] || \
            fail "${description}运行时 ${redirect_key}=${redirect_uri}；应与统一前端入口一致：${expected_redirect}"
    fi
    if [[ "${PUBLIC_HTTPS_ENABLED:-false}" != "true" ]]; then
        case "$platform_base" in
            "$(trim_trailing_slash "$frontend_public_origin")"|http://platform-api:8080) ;;
            *) fail "${description}运行时 ${platform_base_key}=${platform_base}；只能使用统一前端入口 ${frontend_public_origin} 或 Compose 私网平台地址 http://platform-api:8080，不能指向 Keycloak" ;;
        esac
    fi

    if [[ -n "$hash_key" ]]; then
        hash_value="$(env_value "$runtime_file" "$hash_key")"
        validate_runtime_value "$description" "$hash_key" "$hash_value"
        [[ "$hash_value" =~ ^sha256:[a-f0-9]{64}$ ]] || \
            fail "${description}运行时 ${hash_key} 必须为 sha256:<64位小写十六进制目录哈希>"
    fi
}

# 每个本地子系统可以独立绑定 dev/test/staging/prod。优先使用运行时显式环境，
# 旧 Portal 配置没有该字段时从规范 Client ID 推导，最后才使用命令行默认值。
runtime_target_environment() {
	local runtime_file="$1" application_code="$2" client_key="$3"
	local environment_code client_id client_prefix client_suffix
	environment_code="$(env_value "$runtime_file" PLATFORM_ENVIRONMENT_CODE)"
	case "$environment_code" in
		dev|test|staging|prod) printf '%s' "$environment_code"; return ;;
	esac
	client_id="$(env_value "$runtime_file" "$client_key")"
	client_prefix="${application_code}-"
	client_suffix="-web"
	if [[ "$client_id" == "${client_prefix}"*"${client_suffix}" ]]; then
		environment_code="${client_id#"$client_prefix"}"
		environment_code="${environment_code%"$client_suffix"}"
		case "$environment_code" in
			dev|test|staging|prod) printf '%s' "$environment_code"; return ;;
		esac
	fi
	printf '%s' "$subsystem_environment"
}

sync_local_subsystem_environment_exports() {
	export CONTRACT_SUBSYSTEM_ENVIRONMENT="$(runtime_target_environment "$contract_env_file" contract_management OIDC_CLIENT_ID)"
	export CUSTOMER_SUBSYSTEM_ENVIRONMENT="$(runtime_target_environment "$customer_env_file" customer_and_opportunity OIDC_CLIENT_ID)"
	export PORTAL_SUBSYSTEM_ENVIRONMENT="$(runtime_target_environment "$portal_env_file" customer_portal PORTAL_OIDC_CLIENT_ID)"
	export PROJECT_SUBSYSTEM_ENVIRONMENT="$(runtime_target_environment "$project_env_file" project_management OIDC_CLIENT_ID)"
	export DATA_ANALYSIS_SUBSYSTEM_ENVIRONMENT="$(runtime_target_environment "$data_analysis_env_file" data_analysis OIDC_CLIENT_ID)"
	export SETTLEMENT_SUBSYSTEM_ENVIRONMENT="$(runtime_target_environment "$settlement_env_file" settlement OIDC_CLIENT_ID)"
}

validate_authorization_context_wiring() {
    local expected='http://platform-api:8080/oauth2/authorization-context'
    local standard_count portal_count
    standard_count="$(grep -F "PLATFORM_AUTHORIZATION_CONTEXT_URL: ${expected}" "$compose_file" | wc -l | tr -d '[:space:]')"
    portal_count="$(grep -F "PORTAL_AUTHORIZATION_CONTEXT_URL: ${expected}" "$compose_file" | wc -l | tr -d '[:space:]')"
    [[ "$standard_count" -ge 3 && "$portal_count" -ge 1 ]] || \
        fail "Compose 未为合同、项目、CRM、Portal 完整注入平台在线 authorization-context：${expected}"
}

validate_all_keycloak_runtimes() {
	validate_authorization_context_wiring
    validate_keycloak_runtime "合同管理" "$contract_env_file" contract_management "$(runtime_target_environment "$contract_env_file" contract_management OIDC_CLIENT_ID)" \
        OIDC_ISSUER OIDC_BACKCHANNEL_BASE_URL OIDC_CLIENT_ID OIDC_CLIENT_SECRET OIDC_REDIRECT_URI OIDC_TENANT_ID \
        PLATFORM_BASE_URL "${frontend_public_origin}/contract_management/auth/callback"
    validate_keycloak_runtime "客户与商机管理" "$customer_env_file" customer_and_opportunity "$(runtime_target_environment "$customer_env_file" customer_and_opportunity OIDC_CLIENT_ID)" \
        OIDC_ISSUER OIDC_BACKCHANNEL_BASE_URL OIDC_CLIENT_ID OIDC_CLIENT_SECRET OIDC_REDIRECT_URI OIDC_TENANT_ID \
        PLATFORM_BASE_URL "${frontend_public_origin}/customer-opportunity/auth/callback" OIDC_ROLE_CONFIG_HASH
    validate_keycloak_runtime "客户自助门户" "$portal_env_file" customer_portal "$(runtime_target_environment "$portal_env_file" customer_portal PORTAL_OIDC_CLIENT_ID)" \
        PORTAL_OIDC_ISSUER PORTAL_OIDC_BACKCHANNEL_BASE_URL PORTAL_OIDC_CLIENT_ID PORTAL_OIDC_CLIENT_SECRET PORTAL_OIDC_REDIRECT_URI PORTAL_OIDC_TENANT_ID \
        PORTAL_PLATFORM_BASE_URL "${frontend_public_origin}/customer-portal/auth/callback" PORTAL_ROLE_CONFIG_HASH
    validate_keycloak_runtime "项目管理" "$project_env_file" project_management "$(runtime_target_environment "$project_env_file" project_management OIDC_CLIENT_ID)" \
        OIDC_ISSUER OIDC_BACKCHANNEL_BASE_URL OIDC_CLIENT_ID OIDC_CLIENT_SECRET OIDC_REDIRECT_URI OIDC_TENANT_ID \
        PLATFORM_BASE_URL "${frontend_public_origin}/project_management/auth/callback"
}

# 本地 Compose 的发现环境由 --subsystem-environment 显式选择。运行时一旦完成 OIDC 接入，Client、
# application code 和 environment code 必须指向同一自然键；缺省元数据兼容旧 Portal
# 文件，但任何显式冲突都在启动/重建前失败，避免把 prod Client 装入 dev 容器。
validate_local_runtime_target() {
	local description="$1" runtime_file="$2" expected_application="$3" expected_environment="$4" client_key="$5"
	local application_code environment_code client_id expected_client_id
	application_code="$(env_value "$runtime_file" PLATFORM_APPLICATION_CODE)"
	environment_code="$(env_value "$runtime_file" PLATFORM_ENVIRONMENT_CODE)"
	client_id="$(env_value "$runtime_file" "$client_key")"
	expected_client_id="${expected_application}-${expected_environment}-web"

	runtime_value_configured "$client_id" || return 0
	if [[ -n "$application_code" && "$application_code" != "$expected_application" ]]; then
		fail "${description}本地运行配置不匹配：文件=${runtime_file}；当前应用=${application_code}；Compose 发现应用=${expected_application}；请重新执行 ${expected_application}/${expected_environment} 应用接入"
	fi
	if [[ -n "$environment_code" && "$environment_code" != "$expected_environment" ]]; then
		fail "${description}本地运行配置不匹配：文件=${runtime_file}；当前环境=${environment_code}；Compose 目标环境=${expected_environment}；当前 Client=${client_id:-未配置}；期望 Client=${expected_client_id}；请使用 --subsystem-environment ${environment_code}，或重新同步 ${expected_application}/${expected_environment}"
	fi
	if [[ "$client_id" == "$expected_client_id" ]]; then
		return 0
	fi
	# 客户自助门户的浏览器 Client 也可以是平台 bootstrap 托管签发的中转 Client
	# （bootstrap/customer_portal_broker_reconciler 签发并登记为应用 Client，
	# applicationregistry 将其识别为 server-managed broker client）；其余应用
	# 仍必须使用 ${application}-${environment}-web 自然键，禁止跨环境复用。
	if [[ "$expected_application" == customer_portal && "$client_id" == keycloak-customer-portal-broker ]]; then
		return 0
	fi
	fail "${description}本地运行配置不匹配：文件=${runtime_file}；当前环境=${environment_code:-未配置}；当前 Client=${client_id:-未配置}；期望 Client=${expected_client_id}${expected_application:+（customer_portal 亦可为 keycloak-customer-portal-broker）}；请重新同步 ${expected_application}/${expected_environment}，禁止跨环境复用 Client"
}

# 机器客户端也必须和本地目标环境自然键一致。仅校验 Client ID，不读取、打印或
# 猜测 Secret；密钥只能由接入控制面写入受限权限的 env 文件。
validate_local_service_client_target() {
	local description="$1" runtime_file="$2" expected_application="$3" expected_environment="$4" client_key="$5" expected_suffix="$6"
	local client_id expected_client_id
	client_id="$(env_value "$runtime_file" "$client_key")"
	runtime_value_configured "$client_id" || return 0
	expected_client_id="${expected_application}-${expected_environment}-${expected_suffix}"
	[[ "$client_id" == "$expected_client_id" ]] || \
		fail "${description}本地运行配置错配：${client_key}=${client_id}；当前 Compose 是 ${expected_application}/${expected_environment}，必须使用 ${expected_client_id}。请通过子系统 retry/update 重新下发对应环境凭据，禁止跨环境复制 Secret"
}

require_local_service_client_target() {
	local description="$1" runtime_file="$2" expected_application="$3" expected_environment="$4" client_key="$5" secret_key="$6" expected_suffix="$7"
	local client_id secret
	client_id="$(env_value "$runtime_file" "$client_key")"
	secret="$(env_value "$runtime_file" "$secret_key")"
	runtime_value_configured "$client_id" && runtime_value_configured "$secret" || \
		fail "${description}凭据未由接入控制面完整下发；请对 ${expected_application}/${expected_environment} 执行 subsystem-retry，禁止手工复制其他环境密钥"
	validate_local_service_client_target "$description" "$runtime_file" "$expected_application" "$expected_environment" "$client_key" "$expected_suffix"
}

validate_all_local_runtime_targets() {
	validate_local_runtime_target "合同管理" "$contract_env_file" contract_management "$(runtime_target_environment "$contract_env_file" contract_management OIDC_CLIENT_ID)" OIDC_CLIENT_ID
	validate_local_runtime_target "客户与商机管理" "$customer_env_file" customer_and_opportunity "$(runtime_target_environment "$customer_env_file" customer_and_opportunity OIDC_CLIENT_ID)" OIDC_CLIENT_ID
	validate_customer_local_service_clients
	validate_local_runtime_target "客户自助门户" "$portal_env_file" customer_portal "$(runtime_target_environment "$portal_env_file" customer_portal PORTAL_OIDC_CLIENT_ID)" PORTAL_OIDC_CLIENT_ID
	validate_local_runtime_target "项目管理" "$project_env_file" project_management "$(runtime_target_environment "$project_env_file" project_management OIDC_CLIENT_ID)" OIDC_CLIENT_ID
}

validate_customer_local_service_clients() {
	local customer_environment
	customer_environment="$(runtime_target_environment "$customer_env_file" customer_and_opportunity OIDC_CLIENT_ID)"
	require_local_service_client_target "客户与商机管理审计" "$customer_env_file" customer_and_opportunity "$customer_environment" PLATFORM_AUDIT_CLIENT_ID PLATFORM_AUDIT_CLIENT_SECRET audit-publisher
	require_local_service_client_target "客户与商机管理通知" "$customer_env_file" customer_and_opportunity "$customer_environment" PLATFORM_NOTIFICATION_CLIENT_ID PLATFORM_NOTIFICATION_CLIENT_SECRET notification-publisher
}

portal_compensation_configured() {
	local key
	for key in PORTAL_PROVISION_CLIENT_ID PORTAL_PROVISION_CLIENT_SECRET PLATFORM_ROLE_ASSIGN_CLIENT_ID PLATFORM_ROLE_ASSIGN_CLIENT_SECRET; do
		runtime_value_configured "$(env_value "$customer_env_file" "$key")" || return 1
	done
}

disable_contract_startup_catalog_sync() {
    # 本地授权目录由隔离 provisioner 在接入/更新流程中发布。旧环境若仍启用
    # contract-api 启动同步，失效的 publisher 凭据会令 API 和 Worker 一起重启。
    if [[ "$(env_value "$contract_env_file" PLATFORM_AUTHORIZATION_CATALOG_SYNC_ENABLED)" == "true" ]]; then
        replace_line_in_file "$contract_env_file" PLATFORM_AUTHORIZATION_CATALOG_SYNC_ENABLED false
        log "已关闭合同后端启动期授权目录同步；目录改由受控 provisioner 发布"
    fi
}
