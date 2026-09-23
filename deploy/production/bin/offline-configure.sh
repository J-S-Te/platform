#!/usr/bin/env bash
# 被 deploy.sh 加载；配置先写入暂存文件，验证成功后才替换正式 .env。
source "${script_dir}/public-transport.sh"

valid_port() { public_transport_valid_port "$1"; }
valid_ip() { public_transport_is_ip "$1"; }

configuration_prompt() {
  local label="$1" default="$2" value
  read -r -p "$label${default:+ [$default]}：" value || die "未收到配置输入；请在交互终端运行 configure"
  # 兼容从 Windows 或聊天窗口粘贴时带入的 CRLF 和地址两侧空白。
  value="${value//$'\r'/}"
  value="${value#"${value%%[![:space:]]*}"}"
  value="${value%"${value##*[![:space:]]}"}"
  printf '%s' "${value:-$default}"
}

configuration_bound_publicly() {
  case "$1" in 127.*|::1|'[::1]'|localhost) return 1 ;; *) return 0 ;; esac
}

configuration_safe_directory() {
  local path="$1" resolved component prefix=''
  local -a components
  [[ "$path" == /* && "$path" != *'/../'* && "$path" != */.. && "$path" != *'/./'* && "$path" != */. && ! -L "$path" ]] || die "拒绝非规范或符号链接目录：$path"
  IFS=/ read -r -a components <<< "$path"
  for component in "${components[@]}"; do
    [[ -n "$component" ]] || continue
    prefix="$prefix/$component"
    [[ ! -L "$prefix" ]] || die "拒绝数据目录中的符号链接路径：$prefix"
  done
  resolved="${path%/}"
  if [[ -d "$path" ]]; then resolved="$(cd -- "$path" && pwd -P)"; fi
  case "$resolved" in ''|/|/root|/home|/usr|/etc|/dev|/proc|/sys|/run|/tmp|/var|/opt|/bin|/sbin|/lib|/lib64) die "拒绝使用系统根目录作为应用数据目录：$resolved" ;; esac
  [[ "$deploy_dir/" != "$resolved/"* && "$resolved" != "$deploy_dir/data" ]] || die "拒绝把部署根目录或其父目录作为应用数据目录：$resolved"
}

configuration_platform_exists() {
  compgen -G "$manifest_dir/platform-*.imported" >/dev/null && return 0
  local container
  for container in uip-platform-api uip-platform-mysql uip-keycloak; do
    docker container inspect "$container" >/dev/null 2>&1 && return 0
  done
  return 1
}

configuration_check_port() {
  local port="$1" container="$2" listeners mapped
  listeners="$(ss -H -ltn "sport = :$port")" || die "无法检查 TCP 端口 $port"
  [[ -n "$listeners" ]] || return 0
  mapped="$(docker container inspect --format '{{range .NetworkSettings.Ports}}{{range .}}{{println .HostPort}}{{end}}{{end}}' "$container" 2>/dev/null || true)"
  if ! printf '%s\n' "$mapped" | awk -v p="$port" '$0 == p {found=1} END {exit found ? 0 : 1}'; then
    die "TCP 端口 $port 已被占用，且不是预期容器 $container 的端口；请更换端口或人工检查"
  fi
}

configure_firewall() {
  local file="${1:-$runtime_file}" port bind zone backend='' state http_port https_port keycloak_port
  local -a ports=() zones=()
  local -A seen=()
  http_port="$(env_get "$file" PUBLIC_HTTP_PORT)"
  https_port="$(env_get "$file" PUBLIC_HTTPS_PORT)"; https_port="${https_port:-443}"
  keycloak_port="$(env_get "$file" KEYCLOAK_HTTP_PORT)"
  bind="$(env_get "$file" FRONTEND_BIND_ADDRESS)"; bind="${bind:-127.0.0.1}"
  if configuration_bound_publicly "$bind"; then
    ports+=("$http_port")
    if [[ "$(env_get "$file" PUBLIC_HTTPS_ENABLED)" == true || "$(env_get "$file" PUBLIC_TRANSPORT_STATE)" == DISABLING_HTTPS ]]; then ports+=("$https_port"); fi
  fi
  bind="$(env_get "$file" KEYCLOAK_BIND_ADDRESS)"; bind="${bind:-127.0.0.1}"
  if configuration_bound_publicly "$bind"; then ports+=("$keycloak_port"); fi
  if ((${#ports[@]} == 0)); then
    printf '公开入口仅绑定回环地址，未新增防火墙放行规则。\n'
    return 0
  fi
  if command -v ufw >/dev/null 2>&1; then
    state="$(LC_ALL=C ufw status 2>/dev/null)" || die '无法读取 UFW 状态'
    if printf '%s\n' "$state" | awk 'NR == 1 && $2 == "active" {found=1} END {exit found ? 0 : 1}'; then backend=ufw; fi
  fi
  if [[ -z "$backend" ]] && command -v firewall-cmd >/dev/null 2>&1 && firewall-cmd --quiet --state 2>/dev/null; then
    backend=firewalld
    zone="${FIREWALL_ZONE:-$(env_get "$file" FIREWALL_ZONE)}"
    if [[ -z "$zone" ]]; then
      state="$(firewall-cmd --get-active-zones)" || die '无法读取 firewalld 活动区域'
      mapfile -t zones < <(printf '%s\n' "$state" | awk '/^[^[:space:]]/ {print $1}')
      ((${#zones[@]} == 1)) || die 'firewalld 存在零个或多个活动区域，请设置 FIREWALL_ZONE 为业务网卡所在区域后重试'
      zone="${zones[0]}"
    fi
    [[ "$zone" =~ ^[A-Za-z0-9_-]+$ ]] || die 'FIREWALL_ZONE 格式不正确'
    state="$(firewall-cmd --get-zones)" || die '无法读取 firewalld 区域'
    [[ " $state " == *" $zone "* ]] || die "firewalld 区域不存在：$zone"
  fi
  for port in "${ports[@]}"; do
    valid_port "$port" || die "防火墙端口无效：$port"
    port=$((10#$port))
    [[ -z "${seen[$port]:-}" ]] || continue
    seen[$port]=true
    case "$backend" in
      ufw) LC_ALL=C ufw allow "${port}/tcp" >/dev/null || die "UFW 开放端口失败：$port" ;;
      firewalld)
        firewall-cmd --quiet --zone="$zone" --permanent --query-port="${port}/tcp" || firewall-cmd --quiet --zone="$zone" --permanent --add-port="${port}/tcp" || die "firewalld 持久规则写入失败：$port"
        firewall-cmd --quiet --zone="$zone" --query-port="${port}/tcp" || firewall-cmd --quiet --zone="$zone" --add-port="${port}/tcp" || die "firewalld 运行规则写入失败：$port"
        ;;
      '') printf '未检测到活动 UFW/firewalld，未修改防火墙；需人工确认 TCP %s。\n' "$port"; continue ;;
    esac
    printf '已配置 %s%s TCP %s 放行规则。\n' "$backend" "${zone:+/$zone}" "$port"
  done
  printf '云安全组、边界防火墙及外部可达性尚未验证；不会自动启用或关闭主机防火墙。\n'
}

configure() (
  set -Eeuo pipefail
  umask 077
  ((BASH_VERSINFO[0] > 4 || (BASH_VERSINFO[0] == 4 && BASH_VERSINFO[1] >= 3))) || die 'configure 需要 Bash 4.3 或以上'
  ((EUID == 0)) || die '请使用 root 或 sudo 执行 configure，以正确设置容器目录权限'
  local command_name force=false first=false saved=false imported=false staging backup='' actual_runtime="$runtime_file"
  local host old_host mode frontend_port old_frontend platform_port old_platform keycloak_port old_keycloak https_port admin timezone monitoring key runtime_name keys_dir gateway_root private public derived
  for command_name in docker openssl awk install stat chown flock ss cmp; do need "$command_name"; done
  while (($#)); do case "$1" in --force) force=true ;; *) die "configure 未知参数：$1" ;; esac; shift; done
  for runtime_name in runtime data backups logs manifests packages; do
    [[ ! -L "$deploy_dir/$runtime_name" ]] || die "拒绝符号链接目录：$deploy_dir/$runtime_name"
    install -d -m 700 "$deploy_dir/$runtime_name"
  done
  [[ ! -L "$deploy_dir/runtime/.deploy.lock" ]] || die '拒绝符号链接部署锁'
  exec 9>"$deploy_dir/runtime/.deploy.lock"
  flock -w 900 9 || die '等待部署锁超时，未修改配置'
  [[ ! -L "$actual_runtime" && ! -L "$release_file" ]] || die '拒绝符号链接配置文件'
  docker info >/dev/null || die '无法连接 Docker 服务'
  staging="$(mktemp -d "$deploy_dir/.configure-stage.XXXXXX")"
  runtime_file="$(mktemp "$deploy_dir/.env-configure.XXXXXX")"
  trap 'rm -f -- "$runtime_file"; rm -r -- "$staging"' EXIT
  # 保持与正式 .env 同一父目录，证书等相对路径在验证阶段也使用真实部署基准。
  if [[ -f "$actual_runtime" ]]; then install -m 600 "$actual_runtime" "$runtime_file"; else first=true; install -m 600 "$deploy_dir/.env.example" "$runtime_file"; fi
  if [[ -f "$release_file" ]]; then install -m 600 "$release_file" "$staging/.release.env"; else install -m 600 "$deploy_dir/.release.env.example" "$staging/.release.env"; fi
  host="$(env_get "$runtime_file" PUBLIC_PLATFORM_HOST)"; old_host="$host"
  mode="$(env_get "$runtime_file" PUBLIC_ACCESS_MODE)"
  if [[ -z "$mode" ]]; then
    if valid_ip "$host" && [[ "$host" == "$(env_get "$runtime_file" PUBLIC_SSO_HOST)" ]]; then mode=ip; else mode=domain; fi
  fi
  frontend_port="$(env_get "$runtime_file" PUBLIC_HTTP_PORT)"; old_frontend="$frontend_port"
  platform_port="$(env_get "$runtime_file" PLATFORM_API_PORT)"; old_platform="$platform_port"
  keycloak_port="$(env_get "$runtime_file" KEYCLOAK_HTTP_PORT)"; old_keycloak="$keycloak_port"
  if valid_port "$old_frontend"; then old_frontend=$((10#$old_frontend)); fi
  if valid_port "$old_platform"; then old_platform=$((10#$old_platform)); fi
  if valid_port "$old_keycloak"; then old_keycloak=$((10#$old_keycloak)); fi
  admin="$(env_get "$runtime_file" OFFLINE_ADMIN_ACCOUNT)"; admin="${admin:-$(env_get "$runtime_file" IAM_BOOTSTRAP_ADMIN_ACCOUNT_NAME)}"
  timezone="$(env_get "$runtime_file" APP_TIMEZONE)"; timezone="${timezone:-Asia/Shanghai}"
  monitoring="$(env_get "$runtime_file" OFFLINE_MONITORING_ENABLED)"; monitoring="${monitoring:-false}"
  if [[ "$first" == false ]] && public_transport_valid_host "$host" && valid_port "$frontend_port" && valid_port "$platform_port" && valid_port "$keycloak_port" && [[ "$admin" =~ ^[A-Za-z0-9._@-]+$ ]]; then saved=true; fi
  configuration_platform_exists && imported=true
  if [[ "$first" == true || "$saved" == false || "$force" == true ]]; then
    if [[ "$first" == true ]]; then mode=ip; host=''; frontend_port=8081; platform_port=18080; keycloak_port=18090; fi
    if [[ "$mode" == ip ]]; then
      host="$(configuration_prompt '服务器 IP' "$host")"
      valid_ip "$host" || die "请输入有效的 IPv4 或 IPv6 地址（收到：$(printf '%q' "$host")）"
      host="${host#[}"; host="${host%]}"
      platform_port="$(configuration_prompt '基础平台 API 端口' "${platform_port:-18080}")"
      frontend_port="$(configuration_prompt '统一前端 HTTP 端口' "${frontend_port:-8081}")"
      keycloak_port="$(configuration_prompt 'Keycloak HTTP 端口' "${keycloak_port:-18090}")"
    fi
    admin="$(configuration_prompt '基础平台管理员账号' "${admin:-admin}")"
    timezone="$(configuration_prompt '时区' "$timezone")"
    monitoring="$(configuration_prompt '启用监控？true/false' "$monitoring")"
    case "$monitoring" in y|Y|true) monitoring=true ;; n|N|false) monitoring=false ;; *) die '监控选项必须为 true/false 或 y/n' ;; esac
  fi
  valid_port "$platform_port" && valid_port "$frontend_port" && valid_port "$keycloak_port" || die '端口必须是 1–65535 的整数，最多五位'
  platform_port=$((10#$platform_port)); frontend_port=$((10#$frontend_port)); keycloak_port=$((10#$keycloak_port))
  [[ "$platform_port" != "$frontend_port" && "$platform_port" != "$keycloak_port" && "$frontend_port" != "$keycloak_port" ]] || die 'API、前端、Keycloak 端口不能重复'
  [[ "$admin" =~ ^[A-Za-z0-9._@-]+$ ]] || die '管理员账号格式不正确'
  [[ "$timezone" != /* && "$timezone" != *..* && "$timezone" =~ ^[A-Za-z0-9_+/-]+$ && -f "/usr/share/zoneinfo/$timezone" ]] || die '时区无效或服务器未安装对应 tzdata'
  if [[ "$imported" == true && "$mode" == ip ]]; then
    [[ "$host" == "$old_host" && "$frontend_port" == "$old_frontend" && "$keycloak_port" == "$old_keycloak" && "$platform_port" == "$old_platform" ]] || die '已有平台导入/容器记录，不能直接修改 IP 或端口；请先规划并迁移数据库中的 OAuth 回调、客户端与公开传输配置'
  fi
  configuration_check_port "$platform_port" uip-platform-api
  configuration_check_port "$frontend_port" uip-frontend
  configuration_check_port "$keycloak_port" uip-keycloak
  env_set "$runtime_file" PUBLIC_ACCESS_MODE "$mode"
  if [[ "$first" == true ]]; then
    env_set "$runtime_file" COMPOSE_PROJECT_NAME uip
    env_set "$runtime_file" PUBLIC_HTTPS_ENABLED false
    env_set "$runtime_file" FRONTEND_BIND_ADDRESS 0.0.0.0
    env_set "$runtime_file" KEYCLOAK_BIND_ADDRESS 0.0.0.0
    env_set "$runtime_file" KEYCLOAK_HOSTNAME_STRICT false
    env_set "$runtime_file" KEYCLOAK_REQUIRE_HTTPS false
    env_set "$runtime_file" SUBSYSTEM_ALLOW_INSECURE_HTTP_SESSION true
    env_set "$runtime_file" AUTH_SESSION_COOKIE_SECURE false
    env_set "$runtime_file" AUTH_OAUTH_CLIENT_ALLOW_INSECURE_HTTP_REDIRECT_URIS true
  fi
  if [[ "$mode" == ip ]]; then
    env_set "$runtime_file" PUBLIC_PLATFORM_HOST "$host"
    env_set "$runtime_file" PUBLIC_SSO_HOST "$host"
    env_set "$runtime_file" PUBLIC_HTTP_PORT "$frontend_port"
    env_set "$runtime_file" FRONTEND_PORT "$frontend_port"
    env_set "$runtime_file" PLATFORM_API_PORT "$platform_port"
    env_set "$runtime_file" KEYCLOAK_HTTP_PORT "$keycloak_port"
  fi
  public_transport_prepare "$deploy_dir" "$runtime_file" || die '公开传输配置校验失败'
  if [[ "$PUBLIC_HTTPS_ENABLED" == true || "$PUBLIC_TRANSPORT_STATE" == DISABLING_HTTPS ]]; then
    https_port="$PUBLIC_HTTPS_PORT"
    [[ "$https_port" != "$frontend_port" && "$https_port" != "$platform_port" && "$https_port" != "$keycloak_port" ]] || die 'HTTPS 端口不能与 HTTP、API、Keycloak 端口重复'
    configuration_check_port "$https_port" uip-frontend
  fi
  if [[ "$mode" == ip ]]; then
    env_set "$runtime_file" KEYCLOAK_PUBLIC_URL "$PUBLIC_SSO_ORIGIN"
    env_set "$runtime_file" APP_PUBLIC_BASE_URL "$PUBLIC_PLATFORM_ORIGIN"
    env_set "$runtime_file" OIDC_ISSUER "$PUBLIC_PLATFORM_ORIGIN"
    env_set "$runtime_file" SUBSYSTEM_OIDC_ISSUER "$PUBLIC_KEYCLOAK_ISSUER"
    env_set "$runtime_file" DATA_ANALYSIS_PUBLIC_ORIGIN "$PUBLIC_PLATFORM_ORIGIN"
    if [[ "$first" == true ]]; then env_set "$runtime_file" APP_CORS_ALLOWED_ORIGINS "$PUBLIC_PLATFORM_ORIGIN"; fi
  fi
  env_set "$runtime_file" SUBSYSTEM_PRODUCTION_HOST_DEPLOY_ROOT "$deploy_dir"
  env_set "$runtime_file" APP_TIMEZONE "$timezone"
  env_set "$runtime_file" OFFLINE_ADMIN_ACCOUNT "$admin"
  env_set "$runtime_file" IAM_BOOTSTRAP_ADMIN_DISPLAY_NAME "$admin"
  env_set "$runtime_file" IAM_BOOTSTRAP_ADMIN_ACCOUNT_NAME "$admin"
  env_set "$runtime_file" OFFLINE_MONITORING_ENABLED "$monitoring"
  for key in MYSQL_PASSWORD MYSQL_ROOT_PASSWORD KEYCLOAK_DB_PASSWORD KEYCLOAK_DB_ROOT_PASSWORD KEYCLOAK_ADMIN_PASSWORD CONTRACT_MYSQL_PASSWORD CONTRACT_MYSQL_ROOT_PASSWORD CUSTOMER_MYSQL_PASSWORD CUSTOMER_MYSQL_ROOT_PASSWORD PORTAL_MYSQL_PASSWORD PORTAL_MYSQL_ROOT_PASSWORD PROJECT_MYSQL_PASSWORD PROJECT_MYSQL_ROOT_PASSWORD SETTLEMENT_MYSQL_PASSWORD SETTLEMENT_MYSQL_ROOT_PASSWORD DASHBOARD_MYSQL_PASSWORD DASHBOARD_MYSQL_ROOT_PASSWORD FILE_GATEWAY_DB_PASSWORD FILE_GATEWAY_DB_ROOT_PASSWORD; do env_set_generated "$runtime_file" "$key" "$(random_hex 24)"; done
  for key in IAM_MOBILE_ENCRYPTION_KEY IAM_BOOTSTRAP_ADMIN_PASSWORD; do env_set_generated "$runtime_file" "$key" "$(random_b64)"; done
  env_set_generated "$runtime_file" IAM_BOOTSTRAP_TOKEN "$(random_hex 32)"
  env_set_generated "$runtime_file" CUSTOMER_PORTAL_INITIAL_PASSWORD "$(random_hex 24)"
  mkdir "$staging/runtime"
  for runtime_name in contract customer portal project settlement data-analysis; do
    [[ ! -L "$deploy_dir/runtime/$runtime_name.env" ]] || die "拒绝符号链接运行配置：$runtime_name"
    if [[ -f "$deploy_dir/runtime/$runtime_name.env" ]]; then install -m 600 "$deploy_dir/runtime/$runtime_name.env" "$staging/runtime/$runtime_name.env"; else install -m 600 "$deploy_dir/subsystem-templates/$runtime_name.env.example" "$staging/runtime/$runtime_name.env"; fi
  done
  for runtime_name in contract project settlement; do env_set_generated "$staging/runtime/$runtime_name.env" OIDC_SESSION_ENCRYPTION_KEY_BASE64 "$(random_b64)"; done
  for key in SENSITIVE_ENCRYPTION_KEY_BASE64 SENSITIVE_HMAC_KEY_BASE64 PORTAL_INVITE_PEPPER_BASE64; do env_set_generated "$staging/runtime/customer.env" "$key" "$(random_b64)"; done
  for key in PORTAL_ENCRYPTION_KEY_BASE64 PORTAL_REPORT_INGEST_DESCRIPTOR_KEY_BASE64 PORTAL_HMAC_KEY_BASE64; do env_set_generated "$staging/runtime/portal.env" "$key" "$(random_b64)"; done
  env_set_generated "$staging/runtime/data-analysis.env" OIDC_CODEC_KEY "$(random_hex 32)"
  env_set_generated "$staging/runtime/data-analysis.env" METABASE_EMBEDDING_SECRET "$(random_hex 32)"
  keys_dir="$(env_get "$runtime_file" PLATFORM_KEYS_DIR)"; keys_dir="${keys_dir:-./data/platform/keys}"
  keys_dir="${keys_dir#./}"
  [[ "$keys_dir" == /* ]] || keys_dir="$deploy_dir/$keys_dir"
  configuration_safe_directory "$keys_dir"
  private="$keys_dir/jwt-ed25519-private.pem"; public="$keys_dir/jwt-ed25519-public.pem"
  [[ ! -L "$private" && ! -L "$public" ]] || die '拒绝符号链接密钥文件'
  if [[ ! -f "$private" && ( -e "$public" || "$imported" == true ) ]]; then die '私钥缺失但公钥或部署记录已存在，拒绝自动轮换签名密钥；请从备份恢复'; fi
  install -d -m 700 "$keys_dir"
  derived="$staging/derived-public.pem"
  if [[ ! -f "$private" ]]; then
    openssl genpkey -algorithm ED25519 -out "$staging/private.pem"
    openssl pkey -in "$staging/private.pem" -pubout -out "$derived"
  else
    openssl pkey -in "$private" -pubout -out "$derived" || die '无法读取现有私钥，拒绝覆盖'
    if [[ -f "$public" ]] && ! cmp -s "$derived" "$public"; then die '现有公私钥不匹配，拒绝自动覆盖；请人工检查备份'; fi
  fi
  gateway_root="$(env_get "$runtime_file" FILE_GATEWAY_HOST_ROOT)"
  if [[ "$first" == true || -z "$gateway_root" ]]; then gateway_root="$deploy_dir/data/file-gateway"; fi
  configuration_safe_directory "$gateway_root"
  env_set "$runtime_file" FILE_GATEWAY_HOST_ROOT "$gateway_root"
  env_set "$runtime_file" PLATFORM_KEYS_DIR "$keys_dir"
  env_set "$runtime_file" OFFLINE_CONFIGURATION_COMPLETE true
  if [[ -f "$actual_runtime" ]]; then
    backup="$(mktemp -d "$deploy_dir/backups/configuration-$(date +%Y%m%d-%H%M%S).XXXXXX")"
    install -m 600 "$actual_runtime" "$backup/.env"
    [[ ! -f "$release_file" ]] || install -m 600 "$release_file" "$backup/.release.env"
    mkdir "$backup/runtime"
    for runtime_name in contract customer portal project settlement data-analysis; do [[ ! -f "$deploy_dir/runtime/$runtime_name.env" ]] || install -m 600 "$deploy_dir/runtime/$runtime_name.env" "$backup/runtime/$runtime_name.env"; done
  fi
  install -d -m 750 "$gateway_root"
  for runtime_name in temporary quarantine; do
    [[ ! -L "$gateway_root/$runtime_name" ]] || die '拒绝文件网关子目录符号链接'
    install -d -m 750 "$gateway_root/$runtime_name"
    chown 10001:10001 "$gateway_root/$runtime_name"
  done
  chown 10001:10001 "$gateway_root"
  if [[ -f "$staging/private.pem" ]]; then install -m 600 "$staging/private.pem" "$private"; fi
  if [[ ! -f "$public" ]]; then install -m 644 "$derived" "$public"; fi
  chmod 600 "$private"; chmod 644 "$public"
  configure_firewall "$runtime_file"
  for runtime_name in contract customer portal project settlement data-analysis; do mv -f "$staging/runtime/$runtime_name.env" "$deploy_dir/runtime/$runtime_name.env"; done
  if [[ ! -f "$release_file" ]]; then mv "$staging/.release.env" "$release_file"; fi
  chmod 600 "$release_file"
  mv -f "$runtime_file" "$actual_runtime"
  printf '配置及运行目录检查完成：%s；未输出或重置已有密码、接入凭据。\n' "$actual_runtime"
  [[ -z "$backup" ]] || printf '配置备份：%s\n' "$backup"
  printf '平台：%s；SSO：%s。已有管理员账号不会因修改初始化配置而在数据库中更名。\n' "$PUBLIC_PLATFORM_ORIGIN" "$PUBLIC_SSO_ORIGIN"
)
