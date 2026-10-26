# 局域网访问地址探测、覆盖环境文件维护与访问模式选择（仅函数定义，加载零副作用）。

# scripts/lan-access.sh 通过 docker/.env.lan 标记临时局域网模式。docker-local
# 必须继续为基础平台、合同后端和客户商机后端加载同源覆盖配置，否则平台
# 发布的 OIDC issuer 与子系统期望的 issuer 会分别落到 LAN 地址和 localhost，
# 业务后端将因 discovery 校验失败而持续重启。
#
# 同时：每次 up 启动时实时检测当前网络的 IPv4 地址（不再信任 docker/.env.lan
# 里写死的值）。当网段切换、远程 ssh 或换 WiFi 后，up 仍然能用正确的 IP 配置
# 平台后端的 OIDC issuer / CORS allowed origin / 回调地址，避免 403。
is_ipv4() {
    local candidate="$1" octet
    [[ "$candidate" =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ ]] || return 1
    IFS='.' read -r -a octets <<< "$candidate"
    for octet in "${octets[@]}"; do
        ((10#$octet <= 255)) || return 1
    done
}

# 实时检测宿主机可用的第一个非 loopback IPv4。优先用 default route 对应接口
# 的地址，否则回退到 ifconfig 列表里第一个非 127.x 的 IPv4。
detect_current_lan_ipv4() {
    local iface candidate
    iface="$(route -n get default 2>/dev/null | awk '/interface:/{print $2; exit}')"
    if [[ -n "$iface" ]]; then
        candidate="$(ipconfig getifaddr "$iface" 2>/dev/null || true)"
        if is_ipv4 "$candidate" && [[ "$candidate" != 127.* ]]; then
            printf '%s' "$candidate"
            return 0
        fi
    fi

    while IFS= read -r candidate; do
        [[ -z "$candidate" ]] && continue
        is_ipv4 "$candidate" || continue
        [[ "$candidate" == 127.* ]] && continue
        printf '%s' "$candidate"
        return 0
    done < <(ifconfig 2>/dev/null | awk '/inet / {print $2}')
    return 1
}

# 用新 IP 覆盖 docker/.env.lan 中所有 http://<旧IP>:<端口> 的值，
# 保持端口与 path 前缀不变，让 docker compose 重新加载后使用新地址。
rewrite_lan_override_file() {
    local new_ip="$1" file_ip="$2"
    local file_port
    file_port="$(awk -F= '$1 == "APP_PUBLIC_BASE_URL" {print substr($0, index($0, "=") + 1); exit}' "$lan_override_file" | sed -E 's|^http://[^:]+:||; s|/.*$||')"
    [[ -z "$file_port" ]] && file_port="${FRONTEND_HTTP_PORT:-8081}"
    sed -i.bak -E "s|http://${file_ip}:${file_port}|http://${new_ip}:${file_port}|g" "$lan_override_file"
    rm -f "$lan_override_file.bak"
    if [[ -f "$customer_lan_override_file" ]]; then
        sed -i.bak -E "s|http://${file_ip}:${file_port}|http://${new_ip}:${file_port}|g" "$customer_lan_override_file"
        rm -f "$customer_lan_override_file.bak"
    fi
}

write_customer_lan_override() {
    local public_origin="$1"
    umask 077
    cat > "$customer_lan_override_file" <<EOF
# 由 scripts/docker-local.sh/lan-access.sh 自动维护；仅用于临时局域网访问。
APP_PUBLIC_ORIGIN=${public_origin}
OIDC_ISSUER=${public_origin}
OIDC_REDIRECT_URI=${public_origin}/customer-opportunity/auth/callback
OIDC_POST_LOGOUT_REDIRECT_URI=${public_origin}/customer-opportunity/
EOF
    chmod 600 "$customer_lan_override_file"
}

configure_access_mode() {
    local public_origin lan_port detected_ip file_ip

    [[ -f "$lan_placeholder_file" ]] || fail "局域网占位环境文件不存在：$lan_placeholder_file"

    if [[ ! -f "$lan_override_file" ]]; then
        export BASIC_PLATFORM_LAN_OVERRIDE_ENV_FILE="$lan_placeholder_file"
        export CONTRACT_LAN_OVERRIDE_ENV_FILE="$lan_placeholder_file"
		export CUSTOMER_LAN_OVERRIDE_ENV_FILE="$lan_placeholder_file"
		export PORTAL_LAN_OVERRIDE_ENV_FILE="$lan_placeholder_file"
		export PROJECT_LAN_OVERRIDE_ENV_FILE="$lan_placeholder_file"
        export FRONTEND_BIND_ADDRESS="${FRONTEND_BIND_ADDRESS:-127.0.0.1}"
        frontend_public_origin="http://localhost:${FRONTEND_HTTP_PORT:-8081}"
        return 0
    fi

    file_ip="$(awk -F= '$1 == "APP_PUBLIC_BASE_URL" {print substr($0, index($0, "=") + 1); exit}' "$lan_override_file" | sed -E 's|^http://||; s|:.*$||')"
    detected_ip="$(detect_current_lan_ipv4 || true)"

    if [[ -z "$detected_ip" ]]; then
        log "WARN: 未检测到可用 IPv4，沿用 docker/.env.lan 中写死的 $file_ip"
    elif [[ "$detected_ip" != "$file_ip" ]]; then
        log "WARN: 当前 IPv4 ($detected_ip) 与 docker/.env.lan 中写死的 $file_ip 不一致，自动用检测值；如需落盘可执行 bash scripts/lan-access.sh enable"
        rewrite_lan_override_file "$detected_ip" "$file_ip"
    fi

    public_origin="$(awk -F= '$1 == "APP_PUBLIC_BASE_URL" {print substr($0, index($0, "=") + 1); exit}' "$lan_override_file")"
    [[ "$public_origin" =~ ^http://([0-9]{1,3}\.){3}[0-9]{1,3}:[1-9][0-9]{0,4}$ ]] || \
        fail "局域网覆盖文件中的 APP_PUBLIC_BASE_URL 无效：${public_origin:-'(空)'}；请重新执行 bash scripts/lan-access.sh enable"

    lan_port="${public_origin##*:}"
    ((lan_port <= 65535)) || fail "局域网覆盖文件中的端口无效：$lan_port"

    write_customer_lan_override "$public_origin"
    export BASIC_PLATFORM_LAN_OVERRIDE_ENV_FILE="$lan_override_file"
	export CONTRACT_LAN_OVERRIDE_ENV_FILE="$lan_override_file"
	export CUSTOMER_LAN_OVERRIDE_ENV_FILE="$customer_lan_override_file"
	export PORTAL_LAN_OVERRIDE_ENV_FILE="$lan_placeholder_file"
	export PROJECT_LAN_OVERRIDE_ENV_FILE="$lan_override_file"
    export FRONTEND_BIND_ADDRESS="0.0.0.0"
    export FRONTEND_HTTP_PORT="$lan_port"
    frontend_public_origin="$public_origin"
    log "统一访问地址：$public_origin"
}
