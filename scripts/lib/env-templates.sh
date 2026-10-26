# 本地环境文件生成、密钥补齐与原子行替换（仅函数定义，加载零副作用）。

replace_line_in_file() {
    local file="$1" key="$2" value="$3" tmp
    tmp="$(mktemp "${TMPDIR:-/tmp}/docker-local-env.XXXXXX")"
    # 先写入临时文件并设为 0600，再原子替换，避免中断时留下半行配置或短暂放宽密钥权限。
    # 临时文件位于系统临时目录，mv 跨文件系统时未必原子；调用者应避免并发执行多个编排命令。
    awk -v key="$key" -v value="$value" '
        index($0, key "=") == 1 { print key "=" value; found=1; next }
        { print }
        END { if (!found) print key "=" value }
    ' "$file" > "$tmp"
    chmod 600 "$tmp"
    mv "$tmp" "$file"
}

random_hex() { openssl rand -hex "$1" | tr -d '\n'; }
random_key() { openssl rand -base64 32 | tr -d '\n'; }

ensure_platform_jwt_key_pair() {
    local key_dir="${project_root}/data/keys"
    local private_key="${key_dir}/jwt-ed25519-private.pem"
    local public_key="${key_dir}/jwt-ed25519-public.pem"
    if [[ -f "$private_key" && -f "$public_key" ]]; then
        chmod 600 "$private_key" "$public_key"
        return
    fi
    if [[ -e "$private_key" || -e "$public_key" ]]; then
        fail "JWT 密钥对不完整；请恢复配套公私钥，或同时移走 ${private_key} 和 ${public_key} 后重新生成"
    fi

    local temporary_dir
    temporary_dir="$(mktemp -d "${key_dir}/.jwt-keys.XXXXXX")"
    openssl genpkey -algorithm ED25519 -out "${temporary_dir}/private.pem" >/dev/null 2>&1 || {
        rm -rf -- "$temporary_dir"
        fail "生成 Ed25519 JWT 私钥失败"
    }
    openssl pkey -in "${temporary_dir}/private.pem" -pubout -out "${temporary_dir}/public.pem" >/dev/null 2>&1 || {
        rm -rf -- "$temporary_dir"
        fail "生成 Ed25519 JWT 公钥失败"
    }
    chmod 600 "${temporary_dir}/private.pem" "${temporary_dir}/public.pem"
    mv "${temporary_dir}/private.pem" "$private_key"
    mv "${temporary_dir}/public.pem" "$public_key"
    rmdir "$temporary_dir"
    log "已生成本地 Ed25519 JWT 密钥对：${key_dir}"
}

ensure_platform_env_file() {
    command -v openssl >/dev/null 2>&1 || fail "未找到 openssl，无法生成本地密码和密钥"
    mkdir -p "$(dirname "$env_file")" "${project_root}/data/keys" "${project_root}/data/logs" "${project_root}/data/uploads"
    chmod 700 "${project_root}/data/keys" "${project_root}/data/logs" "${project_root}/data/uploads"
    ensure_platform_jwt_key_pair

    # 仅在文件不存在时生成密钥；重复 up 绝不轮换既有数据库密码和加密密钥。
    if [[ ! -f "$env_file" ]]; then
        # Docker 数据卷会保留 MySQL 初始化时的账号密码。若环境文件丢失但卷还在，
        # 重新随机生成 MYSQL_PASSWORD 会让迁移和 API 永久无法登录旧库。此处明确
        # 停止并要求恢复环境文件或走受控密码恢复，绝不在用户不知情时删除数据卷。
        # compose.local.yaml 为本地卷指定了固定 name（使用连字符，而不是
        # Compose 默认的下划线名称）。平台库和 Keycloak 库任意一个存在，都
        # 说明不能安全地依据模板重新随机化 .env.local 中的数据库密码。
        local retained_volume
        for retained_volume in \
            "${compose_project}-mysql-data" \
            "${compose_project}-keycloak-mysql-data" \
            "${compose_project}-file-gateway-mysql-data"
        do
            if command -v docker >/dev/null 2>&1 && docker volume inspect "$retained_volume" >/dev/null 2>&1; then
                fail "检测到已有本地数据卷 ${retained_volume}，但环境文件缺失：${env_file}。请先恢复旧 .env.local，或按数据库密码恢复流程重置账号；不要删除数据卷。"
            fi
        done
        [[ -f "$platform_template_file" ]] || fail "基础平台环境模板不存在：$platform_template_file"
        cp "$platform_template_file" "$env_file"
        chmod 600 "$env_file"
        replace_line_in_file "$env_file" MYSQL_PASSWORD "$(random_hex 24)"
        replace_line_in_file "$env_file" MYSQL_ROOT_PASSWORD "$(random_hex 32)"
        replace_line_in_file "$env_file" IAM_MOBILE_ENCRYPTION_KEY "$(random_key)"
        replace_line_in_file "$env_file" IAM_FEDERATED_PROVIDER_SECRET_ENCRYPTION_KEY "$(random_key)"
        replace_line_in_file "$env_file" IAM_EXTERNAL_LOGIN_STATE_ENCRYPTION_KEY "$(random_key)"
        replace_line_in_file "$env_file" IAM_BOOTSTRAP_TOKEN "$(random_hex 32)"
        replace_line_in_file "$env_file" KEYCLOAK_DB_PASSWORD "$(random_hex 24)"
        replace_line_in_file "$env_file" KEYCLOAK_DB_ROOT_PASSWORD "$(random_hex 32)"
        replace_line_in_file "$env_file" KEYCLOAK_ADMIN_PASSWORD "$(random_hex 32)"
        log "已根据模板生成基础平台环境文件：$env_file"
    fi

    # Keycloak 使用独立 MySQL 卷；升级到该编排版本时，仅为新增字段
    # 生成凭据，不轮换已有真实凭据。
    local keycloak_db_password keycloak_db_root_password keycloak_admin_username keycloak_admin_password
    keycloak_db_password="$(env_value "$env_file" KEYCLOAK_DB_PASSWORD)"
    keycloak_db_root_password="$(env_value "$env_file" KEYCLOAK_DB_ROOT_PASSWORD)"
    keycloak_admin_username="$(env_value "$env_file" KEYCLOAK_ADMIN_USERNAME)"
    keycloak_admin_password="$(env_value "$env_file" KEYCLOAK_ADMIN_PASSWORD)"
    if [[ -z "$keycloak_db_password" || "$keycloak_db_password" == REPLACE_WITH_* ]]; then
        replace_line_in_file "$env_file" KEYCLOAK_DB_PASSWORD "$(random_hex 24)"
    fi
    if [[ -z "$keycloak_db_root_password" || "$keycloak_db_root_password" == REPLACE_WITH_* ]]; then
        replace_line_in_file "$env_file" KEYCLOAK_DB_ROOT_PASSWORD "$(random_hex 32)"
    fi
    if [[ -z "$keycloak_admin_password" || "$keycloak_admin_password" == REPLACE_WITH_* ]]; then
        replace_line_in_file "$env_file" KEYCLOAK_ADMIN_PASSWORD "$(random_hex 32)"
    fi
    if [[ -z "$keycloak_admin_username" || "$keycloak_admin_username" == REPLACE_WITH_* ]]; then
        replace_line_in_file "$env_file" KEYCLOAK_ADMIN_USERNAME admin
    fi
    local gateway_db_password gateway_db_root_password
    gateway_db_password="$(env_value "$env_file" FILE_GATEWAY_DB_PASSWORD)"
    gateway_db_root_password="$(env_value "$env_file" FILE_GATEWAY_DB_ROOT_PASSWORD)"
    if [[ -z "$gateway_db_password" || "$gateway_db_password" == REPLACE_WITH_* || -z "$gateway_db_root_password" || "$gateway_db_root_password" == REPLACE_WITH_* ]]; then
        if docker volume inspect "${compose_project}-file-gateway-mysql-data" >/dev/null 2>&1; then
            fail "检测到已有 File Gateway MySQL 数据卷，但运行配置缺少数据库凭据：${env_file}。请恢复原凭据；不要删除数据卷或自动轮换密码。"
        fi
        if [[ -z "$gateway_db_password" || "$gateway_db_password" == REPLACE_WITH_* ]]; then
            replace_line_in_file "$env_file" FILE_GATEWAY_DB_PASSWORD "$(random_hex 24)"
        fi
        if [[ -z "$gateway_db_root_password" || "$gateway_db_root_password" == REPLACE_WITH_* ]]; then
            replace_line_in_file "$env_file" FILE_GATEWAY_DB_ROOT_PASSWORD "$(random_hex 32)"
        fi
    fi
    # 管理页面通过平台 API 初始化 Realm 和 Client；这里仅是非敏感的本地路由默认值。
    replace_line_in_file "$env_file" KEYCLOAK_MANAGEMENT_ENABLED true
    replace_line_in_file "$env_file" KEYCLOAK_ADMIN_URL http://keycloak:8080
    replace_line_in_file "$env_file" KEYCLOAK_PUBLIC_URL http://localhost:18090
    replace_line_in_file "$env_file" KEYCLOAK_REALM basic-platform
    # 新接入环境默认走 Keycloak；已有环境的 issuer_alias 存在于平台数据库，
    # 不会因这里变更而被隐式切换，仍需通过应用接入页完成受控迁移。
    replace_line_in_file "$env_file" SUBSYSTEM_DEFAULT_ISSUER_ALIAS keycloak

    # OIDC_ROLE_CONFIG_HASH 是 CRM Token Claims 的目录一致性校验值。旧模板
    # 使用占位符时在 Compose 解析前填入可信本地基线；启动 CRM 前仍会用镜像
    # 实际目录哈希复核，目录发生变化不会被这个基线静默掩盖。
    local role_config_hash
    role_config_hash="$(env_value "$env_file" OIDC_ROLE_CONFIG_HASH)"
    if [[ -z "$role_config_hash" || "$role_config_hash" == REPLACE_WITH_* ]]; then
        replace_line_in_file "$env_file" OIDC_ROLE_CONFIG_HASH "$local_crm_role_config_hash"
    fi

    local unresolved
    unresolved="$(grep -E 'REPLACE_WITH_' "$env_file" | grep -v '^#' || true)"
    [[ -z "$unresolved" ]] || fail "基础平台环境文件仍包含未填写占位符：$env_file"
}

ensure_contract_env_file() {
    local strict="${1:-false}"
    command -v openssl >/dev/null 2>&1 || fail "未找到 openssl，无法生成合同数据库密码"

    # Compose 显式命名此卷，文件缺失绝不代表新数据库。
    local volume_inventory persistent_key persistent_value
    volume_inventory="$(docker volume ls --format '{{.Name}}')" || fail "无法确认合同保留数据卷；拒绝生成新凭据"
    if grep -Fxq 'basic-platform-local-contract-mysql-data' <<< "$volume_inventory"; then
        [[ -f "$contract_env_file" ]] || fail "合同数据卷已存在但配置丢失；请恢复原配置，不要重置数据库或加密密钥"
        for persistent_key in CONTRACT_MYSQL_PASSWORD CONTRACT_MYSQL_ROOT_PASSWORD OIDC_SESSION_ENCRYPTION_KEY_BASE64 SIGNING_PHONE_ENCRYPTION_KEY_BASE64; do
            persistent_value="$(env_value "$contract_env_file" "$persistent_key")"
            [[ -n "$persistent_value" && "$persistent_value" != REPLACE_WITH_* ]] || fail "合同保留数据的持久化配置缺失：${persistent_key}；请恢复原配置"
            if [[ "$persistent_key" == *_BASE64 ]]; then
                [[ "$(printf '%s' "$persistent_value" | openssl base64 -d -A 2>/dev/null | wc -c | tr -d ' ')" == 32 ]] || fail "合同保留数据的加密密钥无效：${persistent_key}；请恢复原配置"
            fi
        done
    fi

    if [[ ! -f "$contract_env_file" ]]; then
        [[ -f "$contract_template_file" ]] || fail "合同管理环境模板不存在：$contract_template_file"
        cp "$contract_template_file" "$contract_env_file"
        chmod 600 "$contract_env_file"
        replace_line_in_file "$contract_env_file" CONTRACT_MYSQL_PASSWORD "$(random_hex 24)"
        replace_line_in_file "$contract_env_file" CONTRACT_MYSQL_ROOT_PASSWORD "$(random_hex 32)"
        replace_line_in_file "$contract_env_file" PLATFORM_BASE_URL "http://localhost:8081"
        replace_line_in_file "$contract_env_file" OIDC_ISSUER "http://localhost:18090/realms/basic-platform"
        replace_line_in_file "$contract_env_file" OIDC_BACKCHANNEL_BASE_URL "http://keycloak:8080"
        replace_line_in_file "$contract_env_file" OIDC_REDIRECT_URI "http://localhost:8081/contract_management/auth/callback"
        replace_line_in_file "$contract_env_file" OIDC_SESSION_ENCRYPTION_KEY_BASE64 "$(random_key)"
        # SEC-D7b：签署人手机号列加密密钥（Base64 32 字节）与 session 键一样在首次生成时写入。
        replace_line_in_file "$contract_env_file" SIGNING_PHONE_ENCRYPTION_KEY_BASE64 "$(random_key)"
        replace_line_in_file "$contract_env_file" APP_PUBLIC_URL "http://localhost:8081/contract_management/dashboard"
        replace_line_in_file "$contract_env_file" APP_PATH_PREFIX "/contract_management"
        log "已生成合同管理环境文件：$contract_env_file"
        log "请先在基础平台完成合同管理系统接入，并填写 OIDC_CLIENT_ID、OIDC_CLIENT_SECRET、OIDC_TENANT_ID。"
    fi

    if [[ -z "$(env_value "$contract_env_file" OIDC_SESSION_ENCRYPTION_KEY_BASE64)" || "$(env_value "$contract_env_file" OIDC_SESSION_ENCRYPTION_KEY_BASE64)" == REPLACE_WITH_* ]]; then
        replace_line_in_file "$contract_env_file" OIDC_SESSION_ENCRYPTION_KEY_BASE64 "$(random_key)"
    fi

    # SEC-D7b：签署人手机号列加密密钥必填（AES-GCM，Base64 32 字节）；既有 env 文件缺失或
    # 占位时补齐，否则 contract /readyz 失败关闭、寄送写入被拒绝。幂等：已有合法键不覆盖。
    if [[ -z "$(env_value "$contract_env_file" SIGNING_PHONE_ENCRYPTION_KEY_BASE64)" || "$(env_value "$contract_env_file" SIGNING_PHONE_ENCRYPTION_KEY_BASE64)" == REPLACE_WITH_* ]]; then
        replace_line_in_file "$contract_env_file" SIGNING_PHONE_ENCRYPTION_KEY_BASE64 "$(random_key)"
    fi

    if [[ "$strict" == true ]]; then
        local unresolved
        unresolved="$(grep -E 'REPLACE_WITH_' "$contract_env_file" | grep -v '^#' || true)"
        [[ -z "$unresolved" ]] || fail "合同管理环境文件仍包含接入占位符：$contract_env_file"
    fi

    # 旧版本曾在基础平台环境文件中保存合同数据库凭据。统一部署继续使用已有
    # 数据卷时，优先沿用这组凭据，避免新建合同环境文件后静默生成第二套密码。
    # MySQL 的 MYSQL_PASSWORD 只在空数据目录首次初始化时生效，单纯修改 env
    # 文件不会更新已有数据库账号。
    local legacy_contract_password legacy_contract_root_password current_contract_password current_contract_root_password
    legacy_contract_password="$(env_value "$env_file" CONTRACT_MYSQL_PASSWORD)"
    legacy_contract_root_password="$(env_value "$env_file" CONTRACT_MYSQL_ROOT_PASSWORD)"
    current_contract_password="$(env_value "$contract_env_file" CONTRACT_MYSQL_PASSWORD)"
    current_contract_root_password="$(env_value "$contract_env_file" CONTRACT_MYSQL_ROOT_PASSWORD)"
    if [[ -n "$legacy_contract_password" && -n "$legacy_contract_root_password" ]] && \
       [[ "$legacy_contract_password" != "$current_contract_password" || "$legacy_contract_root_password" != "$current_contract_root_password" ]]; then
        replace_line_in_file "$contract_env_file" CONTRACT_MYSQL_PASSWORD "$legacy_contract_password"
        replace_line_in_file "$contract_env_file" CONTRACT_MYSQL_ROOT_PASSWORD "$legacy_contract_root_password"
        log "已沿用基础平台环境文件中的既有合同数据库凭据，避免已有数据卷认证失败"
    fi

    # OIDC_ISSUER 是浏览器可见地址；容器自身不能通过 localhost 访问宿主机
    # 的 8081。运行时文件没有后通道时，按 Issuer 补齐对应 Compose 内网地址。
    # 已由应用接入流程写入的非空值绝不覆盖，避免干扰已切换的环境。
    local oidc_issuer oidc_backchannel keycloak_public_url
    oidc_issuer="$(env_value "$contract_env_file" OIDC_ISSUER)"
    oidc_backchannel="$(env_value "$contract_env_file" OIDC_BACKCHANNEL_BASE_URL)"
    keycloak_public_url="$(env_value "$env_file" KEYCLOAK_PUBLIC_URL)"
    if [[ -z "$oidc_backchannel" ]]; then
        if [[ "$oidc_issuer" == "http://localhost:8081" || "$oidc_issuer" == "http://127.0.0.1:8081" ]]; then
            replace_line_in_file "$contract_env_file" OIDC_BACKCHANNEL_BASE_URL "http://platform-api:8080"
            log "已为合同管理补齐基础平台 OIDC 容器内后通道"
        elif [[ -n "$keycloak_public_url" && "$oidc_issuer" == "${keycloak_public_url%/}/realms/"* ]]; then
            replace_line_in_file "$contract_env_file" OIDC_BACKCHANNEL_BASE_URL "http://keycloak:8080"
            log "已为合同管理补齐 Keycloak OIDC 容器内后通道"
        fi
    elif [[ -n "$keycloak_public_url" && "$oidc_issuer" == "${keycloak_public_url%/}/realms/"* && "$oidc_backchannel" == */realms/* ]]; then
        # OIDC_BACKCHANNEL_BASE_URL 是来源地址，不是公共签发者。旧本地配置误存 /realms/<realm> 会使合同服务启动前即失败。
        # 值必须保持在 Compose 私网内，且只修复这一已知无效形式。
        replace_line_in_file "$contract_env_file" OIDC_BACKCHANNEL_BASE_URL "http://keycloak:8080"
        log "已修复合同管理 Keycloak 后通道：移除了错误的 /realms 路径"
    fi
}

ensure_customer_env_file() {
    command -v openssl >/dev/null 2>&1 || fail "未找到 openssl，无法生成客户与商机管理数据库密码和敏感字段密钥"
    # CRM 的加密键与检索 HMAC 键职责不同，首次生成后都必须稳定保留，否则历史敏感字段不可读/不可查。
    if [[ ! -f "$customer_env_file" ]]; then
        [[ -f "$customer_template_file" ]] || fail "客户与商机管理环境模板不存在：$customer_template_file"
        cp "$customer_template_file" "$customer_env_file"
        chmod 600 "$customer_env_file"

        local password root_password encryption_key hmac_key
        password="$(random_hex 24)"
        root_password="$(random_hex 32)"
        encryption_key="$(random_key)"
        hmac_key="$(random_key)"
        replace_line_in_file "$customer_env_file" CUSTOMER_MYSQL_PASSWORD "$password"
        replace_line_in_file "$customer_env_file" CUSTOMER_MYSQL_ROOT_PASSWORD "$root_password"
        replace_line_in_file "$customer_env_file" MYSQL_DSN "customer:${password}@tcp(customer-mysql:3306)/customer_opportunity?charset=utf8mb4&parseTime=True&loc=Asia%2FShanghai&multiStatements=true"
        replace_line_in_file "$customer_env_file" SENSITIVE_ENCRYPTION_KEY_BASE64 "$encryption_key"
        replace_line_in_file "$customer_env_file" SENSITIVE_HMAC_KEY_BASE64 "$hmac_key"
        log "已生成客户与商机管理环境文件：$customer_env_file"
    fi

    local unresolved
    unresolved="$(grep -E 'REPLACE_WITH_' "$customer_env_file" | grep -v '^#' || true)"
    [[ -z "$unresolved" ]] || fail "客户与商机管理环境文件仍包含未填写占位符：$customer_env_file"

    # OIDC_BACKCHANNEL_BASE_URL 只接受容器内可达的 HTTP(S) origin，不能携带
    # /realms/<realm> 路径。修复历史本地配置，避免 CRM 在 Compose 健康检查前循环重启。
    local oidc_backchannel
    oidc_backchannel="$(env_value "$customer_env_file" OIDC_BACKCHANNEL_BASE_URL)"
    if [[ "$oidc_backchannel" == */realms/* ]]; then
        replace_line_in_file "$customer_env_file" OIDC_BACKCHANNEL_BASE_URL "http://keycloak:8080"
        log "已修复客户与商机管理 Keycloak 后通道：移除了错误的 /realms 路径"
    fi
}

ensure_portal_env_file() {
	command -v openssl >/dev/null 2>&1 || fail "未找到 openssl，无法生成客户门户数据库密码和安全密钥"
	if [[ ! -f "$portal_env_file" ]]; then
		[[ -f "$portal_template_file" ]] || fail "客户自助门户环境模板不存在：$portal_template_file"
		cp "$portal_template_file" "$portal_env_file"
		chmod 600 "$portal_env_file"

		local password root_password encryption_key descriptor_key hmac_key
		password="$(random_hex 24)"
		root_password="$(random_hex 32)"
		encryption_key="$(random_key)"
		descriptor_key="$(random_key)"
		hmac_key="$(random_key)"
		replace_line_in_file "$portal_env_file" PORTAL_MYSQL_PASSWORD "$password"
		replace_line_in_file "$portal_env_file" PORTAL_MYSQL_ROOT_PASSWORD "$root_password"
		replace_line_in_file "$portal_env_file" PORTAL_MYSQL_DSN "portal:${password}@tcp(portal-mysql:3306)/customer_portal?charset=utf8mb4&parseTime=true&loc=UTC&multiStatements=true"
		replace_line_in_file "$portal_env_file" PORTAL_ENCRYPTION_KEY_BASE64 "$encryption_key"
		replace_line_in_file "$portal_env_file" PORTAL_REPORT_INGEST_DESCRIPTOR_KEY_BASE64 "$descriptor_key"
		replace_line_in_file "$portal_env_file" PORTAL_HMAC_KEY_BASE64 "$hmac_key"
		log "已生成客户自助门户环境文件：$portal_env_file"
	fi

	local unresolved
	unresolved="$(grep -E 'REPLACE_WITH_' "$portal_env_file" | grep -v '^#' || true)"
	[[ -z "$unresolved" ]] || fail "客户自助门户环境文件仍包含未填写占位符：$portal_env_file"
}

ensure_project_env_file() {
	command -v openssl >/dev/null 2>&1 || fail "未找到 openssl，无法生成项目管理系统数据库密码"
	if [[ ! -f "$project_env_file" ]]; then
		[[ -f "$project_template_file" ]] || fail "项目管理系统环境模板不存在：$project_template_file"
		cp "$project_template_file" "$project_env_file"
		chmod 600 "$project_env_file"

		local password root_password
		password="$(random_hex 24)"
		root_password="$(random_hex 32)"
		replace_line_in_file "$project_env_file" PROJECT_MYSQL_PASSWORD "$password"
		replace_line_in_file "$project_env_file" PROJECT_MYSQL_ROOT_PASSWORD "$root_password"
		replace_line_in_file "$project_env_file" MYSQL_DSN "project:${password}@tcp(project-mysql:3306)/project_management?parseTime=true&charset=utf8mb4&collation=utf8mb4_unicode_ci"
		replace_line_in_file "$project_env_file" PLATFORM_BASE_URL "http://localhost:8081"
		replace_line_in_file "$project_env_file" PROJECT_PLATFORM_BACKCHANNEL_BASE_URL "http://platform-api:8080"
        replace_line_in_file "$project_env_file" OIDC_ISSUER "http://localhost:18090/realms/basic-platform"
        replace_line_in_file "$project_env_file" OIDC_BACKCHANNEL_BASE_URL "http://keycloak:8080"
        replace_line_in_file "$project_env_file" OIDC_REDIRECT_URI "http://localhost:8081/project_management/auth/callback"
        replace_line_in_file "$project_env_file" OIDC_SESSION_ENCRYPTION_KEY_BASE64 "$(random_key)"
        replace_line_in_file "$project_env_file" OIDC_POST_LOGOUT_REDIRECT_URI "http://localhost:8081/project_management/logged-out"
        replace_line_in_file "$project_env_file" APP_PATH_PREFIX "/project_management"
        replace_line_in_file "$project_env_file" OIDC_SESSION_COOKIE_SECURE "false"
        replace_line_in_file "$project_env_file" PLATFORM_ENVIRONMENT_CODE "dev"
        log "已生成项目管理系统环境文件：$project_env_file"
        log "请先在基础平台完成项目管理系统接入，并填写 OIDC_CLIENT_ID、OIDC_CLIENT_SECRET、OIDC_TENANT_ID。"
    fi

    if [[ -z "$(env_value "$project_env_file" OIDC_SESSION_ENCRYPTION_KEY_BASE64)" || "$(env_value "$project_env_file" OIDC_SESSION_ENCRYPTION_KEY_BASE64)" == REPLACE_WITH_* ]]; then
        replace_line_in_file "$project_env_file" OIDC_SESSION_ENCRYPTION_KEY_BASE64 "$(random_key)"
    fi
}

ensure_settlement_env_file() {
	command -v openssl >/dev/null 2>&1 || fail "未找到 openssl，无法生成结算与开票数据库密码和会话密钥"
	if [[ ! -f "$settlement_env_file" ]]; then
		[[ -f "$settlement_template_file" ]] || fail "结算与开票环境模板不存在：$settlement_template_file"
		cp "$settlement_template_file" "$settlement_env_file"
		chmod 600 "$settlement_env_file"
		local password root_password
		password="$(random_hex 24)"
		root_password="$(random_hex 32)"
		replace_line_in_file "$settlement_env_file" SETTLEMENT_MYSQL_PASSWORD "$password"
		replace_line_in_file "$settlement_env_file" SETTLEMENT_MYSQL_ROOT_PASSWORD "$root_password"
		replace_line_in_file "$settlement_env_file" SETTLEMENT_MYSQL_DSN "settlement:${password}@tcp(settlement-mysql:3306)/settlement?parseTime=true&charset=utf8mb4&loc=UTC"
		replace_line_in_file "$settlement_env_file" SETTLEMENT_PUBLIC_ORIGIN "$frontend_public_origin"
		replace_line_in_file "$settlement_env_file" PLATFORM_BASE_URL "http://platform-api:8080"
		replace_line_in_file "$settlement_env_file" OIDC_SESSION_ENCRYPTION_KEY_BASE64 "$(random_key)"
		log "已生成结算与开票运行环境文件：$settlement_env_file"
	fi

	if [[ -z "$(env_value "$settlement_env_file" OIDC_SESSION_ENCRYPTION_KEY_BASE64)" || "$(env_value "$settlement_env_file" OIDC_SESSION_ENCRYPTION_KEY_BASE64)" == REPLACE_WITH_* ]]; then
		replace_line_in_file "$settlement_env_file" OIDC_SESSION_ENCRYPTION_KEY_BASE64 "$(random_key)"
	fi
	local unresolved
	unresolved="$(grep -E 'REPLACE_WITH_' "$settlement_env_file" | grep -v '^#' || true)"
	[[ -z "$unresolved" ]] || fail "结算与开票环境文件仍包含未填写占位符：$settlement_env_file"
}

ensure_data_analysis_env_file() {
	command -v openssl >/dev/null 2>&1 || fail "未找到 openssl，无法生成数据看板数据库密码"
	if [[ ! -f "$data_analysis_env_file" ]]; then
		[[ -f "$data_analysis_template_file" ]] || fail "数据看板环境模板不存在：$data_analysis_template_file"
		cp "$data_analysis_template_file" "$data_analysis_env_file"
		chmod 600 "$data_analysis_env_file"

		local password root_password
		password="$(random_hex 24)"
		root_password="$(random_hex 32)"
		replace_line_in_file "$data_analysis_env_file" DASHBOARD_MYSQL_PASSWORD "$password"
		replace_line_in_file "$data_analysis_env_file" DASHBOARD_MYSQL_ROOT_PASSWORD "$root_password"
		replace_line_in_file "$data_analysis_env_file" OIDC_CODEC_KEY "$(random_hex 32)"
		replace_line_in_file "$data_analysis_env_file" METABASE_EMBEDDING_SECRET "$(random_hex 32)"
		replace_line_in_file "$data_analysis_env_file" PLATFORM_BASE_URL "http://localhost:8081"
		replace_line_in_file "$data_analysis_env_file" OIDC_ISSUER "http://localhost:18090/realms/basic-platform"
		replace_line_in_file "$data_analysis_env_file" OIDC_BACKCHANNEL_BASE_URL "http://keycloak:8080"
		replace_line_in_file "$data_analysis_env_file" OIDC_REDIRECT_URI "http://localhost:8081/data_analysis/auth/callback"
		replace_line_in_file "$data_analysis_env_file" APP_PATH_PREFIX "/data_analysis"
		replace_line_in_file "$data_analysis_env_file" APP_PUBLIC_ORIGIN "http://localhost:8081"
		replace_line_in_file "$data_analysis_env_file" COOKIE_SECURE "false"
		log "已生成数据看板环境文件：$data_analysis_env_file"
		log "请先在基础平台完成数据看板接入，并填写 OIDC_CLIENT_ID、OIDC_CLIENT_SECRET、OIDC_TENANT_ID。"
	fi

	if [[ -z "$(env_value "$data_analysis_env_file" OIDC_CODEC_KEY)" || "$(env_value "$data_analysis_env_file" OIDC_CODEC_KEY)" == REPLACE_WITH_* ]]; then
		replace_line_in_file "$data_analysis_env_file" OIDC_CODEC_KEY "$(random_hex 32)"
	fi
}

env_value() {
    local file="$1" key="$2" value
    [[ -f "$file" ]] || return 0
    value="$(awk -v key="$key" '
        index($0, key "=") == 1 {
            value=substr($0, length(key) + 2)
            print value
            exit
        }
    ' "$file")"
    if [[ ${#value} -ge 2 ]]; then
        case "$value" in
            \"*\") value="${value:1:${#value}-2}" ;;
            \'*\') value="${value:1:${#value}-2}" ;;
        esac
    fi
    printf '%s' "$value"
}
