# 报销本地接入助手：加载时只定义函数，不生成配置或调用 Docker。

# 用途：首次准备私密接入模板，保留已有配置和持久化身份。
# 参数：无；路径由本地启动入口提供。
# 返回值：模板可用返回0；配置丢失或路径不安全时拒绝继续。
ensure_reimbursement_env_file() {
    [[ ! -L "$reimbursement_env_file" ]] || fail "报销环境文件不能是符号链接"
    if [[ -e "$reimbursement_env_file" ]]; then
        [[ -f "$reimbursement_env_file" ]] || fail "报销环境文件必须是普通文件"
        return 0
    fi
    local volumes
    volumes="$(docker volume ls --filter "label=com.docker.compose.project=${compose_project}" \
        --filter 'label=com.docker.compose.volume=reimbursement-mysql-data' --format '{{.Name}}')" || \
        fail "无法确认报销保留数据卷，拒绝准备新配置"
    [[ -z "$volumes" ]] || fail "报销数据卷已存在但配置丢失；请恢复原配置后受控重试"
    [[ -f "$reimbursement_template_file" ]] || fail "报销本地环境模板不存在"
    (umask 077; set -o noclobber; cat "$reimbursement_template_file" > "$reimbursement_env_file") || \
        fail "无法安全创建报销环境模板"
    log "已准备报销接入模板；数据库与会话密钥、OIDC及机器凭据由平台受控接入下发"
}

# 用途：区分未接入模板与已有浏览器身份的报销运行配置。
# 参数：无；读取本地报销环境文件。
# 返回值：已有非占位OIDC客户端返回0，未接入返回1。
reimbursement_configured() {
    local client
    client="$(env_value "$reimbursement_env_file" OIDC_CLIENT_ID)"
    runtime_value_configured "$client" && [[ "$client" != PENDING_* ]]
}

# 用途：拒绝使用缺失机器凭据或错环境的报销运行配置。
# 参数：无；只检查受控接入写入的文件，不生成或输出密钥。
# 返回值：配置完整返回0，否则报错中止启动。
validate_reimbursement_runtime() {
    local key value environment
    for key in MYSQL_DSN OIDC_ISSUER OIDC_BACKCHANNEL_BASE_URL OIDC_CLIENT_SECRET \
        OIDC_TENANT_ID OIDC_REDIRECT_URI OIDC_SESSION_ENCRYPTION_KEY_BASE64 \
        REIMBURSEMENT_MYSQL_PASSWORD REIMBURSEMENT_MYSQL_ROOT_PASSWORD \
        PLATFORM_AUTHORIZATION_CATALOG_APPLICATION_ID \
        PLATFORM_AUTHORIZATION_CATALOG_CLIENT_ID PLATFORM_AUTHORIZATION_CATALOG_CLIENT_SECRET \
        PROJECT_REFERENCE_CLIENT_ID PROJECT_REFERENCE_CLIENT_SECRET \
        PROJECT_SPEND_CLIENT_ID PROJECT_SPEND_CLIENT_SECRET \
        PLATFORM_OWNER_DIRECTORY_CLIENT_ID PLATFORM_OWNER_DIRECTORY_CLIENT_SECRET \
        PLATFORM_AUDIT_CLIENT_ID PLATFORM_AUDIT_CLIENT_SECRET \
        PLATFORM_NOTIFICATION_CLIENT_ID PLATFORM_NOTIFICATION_CLIENT_SECRET \
        FILE_GATEWAY_CLIENT_ID FILE_GATEWAY_CLIENT_SECRET; do
        value="$(env_value "$reimbursement_env_file" "$key")"
        runtime_value_configured "$value" && [[ "$value" != PENDING_* ]] || \
            fail "报销接入配置缺少 ${key}；请通过平台受控重试，不要复制其他环境凭据"
    done
    [[ "$(env_value "$reimbursement_env_file" PROJECT_REFERENCE_CLIENT_ID)" != \
       "$(env_value "$reimbursement_env_file" PROJECT_SPEND_CLIENT_ID)" ]] || \
        fail "报销项目引用与支出客户端必须独立"
    environment="$(runtime_target_environment "$reimbursement_env_file" reimbursement OIDC_CLIENT_ID)"
    validate_local_runtime_target "报销管理" "$reimbursement_env_file" reimbursement \
        "$environment" OIDC_CLIENT_ID
    validate_keycloak_runtime "报销管理" "$reimbursement_env_file" reimbursement "$environment" \
        OIDC_ISSUER OIDC_BACKCHANNEL_BASE_URL OIDC_CLIENT_ID OIDC_CLIENT_SECRET \
        OIDC_REDIRECT_URI OIDC_TENANT_ID PLATFORM_BASE_URL \
        "${frontend_public_origin}/reimbursement/auth/callback"
}

# 用途：只恢复已接入并绑定固定镜像的报销双进程。
# 参数：无；复用既有许可预检与Compose执行器。
# 返回值：未接入时跳过；任何门禁或启动失败均返回非零。
start_reimbursement_if_configured() {
    if ! reimbursement_configured; then
        log "报销管理尚未接入，跳过API与Worker；请在平台完成 reimbursement 应用接入"
        return 0
    fi
    validate_reimbursement_runtime
    pinned_local_service reimbursement-api && pinned_local_service reimbursement-worker || \
        fail "报销双组件尚未绑定受控固定镜像；请在平台完成接入或重试"
    # 备份、迁移和目录发布由既有provisioner执行，普通up不重复迁移。
    compose_run up -d --wait --no-deps --no-build reimbursement-mysql || return
    compose_run up -d --wait --no-deps --no-build reimbursement-api reimbursement-worker
}
