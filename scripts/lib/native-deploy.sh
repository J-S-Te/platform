# native 部署脚本共享函数（从 bootstrap-linux.sh / backend-only-linux.sh 逐字抽取）；仅函数定义，加载零副作用。

# ============================ 命令执行与依赖检查 ============================

run_cmd() {
  log_command "$@"
  if [[ "$DRY_RUN" == true ]]; then
    return 0
  fi
  if [[ "$VERBOSE" == true ]]; then
    "$@" 2>&1 | tee -a "$LOG_FILE"
  else
    "$@" >>"$LOG_FILE" 2>&1
  fi
}

require_command() {
  local command_name="$1"
  command -v "$command_name" >/dev/null 2>&1 || {
    log "ERROR" "缺少命令：${command_name}"
    return 1
  }
}

# ============================ MySQL 版本与连接工具 ============================

mysql_version() {
  command -v mysql >/dev/null 2>&1 || return 1
  mysql --version 2>/dev/null | awk '
    {
      for (field_no = 1; field_no <= NF; field_no++) {
        if ($field_no == "Ver" || $field_no == "Distrib") {
          version = $(field_no + 1)
          sub(/[^0-9.].*$/, "", version)
          print version
          exit
        }
      }
    }
  '
}

mysql_option_value() {
  local value="$1"
  value="${value//\\/\\\\}"
  value="${value//\"/\\\"}"
  printf '"%s"' "$value"
}

mysql_defaults_file() {
  local file="$1"
  local user="$2"
  local password="$3"
  local host="${4:-}"
  local port="${5:-}"
  {
    printf '[client]\nuser=%s\n' "$(mysql_option_value "$user")"
    [[ -n "$password" ]] && printf 'password=%s\n' "$(mysql_option_value "$password")"
    [[ -n "$host" ]] && printf 'host=%s\nprotocol=TCP\n' "$(mysql_option_value "$host")"
    [[ -n "$port" ]] && printf 'port=%s\n' "$port"
  } >"$file"
  chmod 600 "$file"
}

is_local_mysql_host() {
  local host="$1"
  [[ "$host" == "127.0.0.1" || "$host" == "localhost" || "$host" == "::1" ]]
}

valid_mysql_account_host() {
  # MySQL 账号主机值支持主机名、IPv4 和 IPv6 字面量及 % 通配符；拼接 SQL 前拒绝引号、空白及路径类字符。
  [[ "$1" =~ ^[A-Za-z0-9._:%-]+$ ]]
}

# ============================ 数据库与应用账号初始化 ============================

initialize_database() {
  if [[ "$SKIP_DATABASE_INIT" == true ]]; then
    log "INFO" "已跳过数据库和应用账号初始化。"
    return 0
  fi

  CURRENT_STEP="初始化 MySQL 数据库与应用账号"
  local host port database username password admin_user admin_password application_allowed_host
  host="$(get_env_value MYSQL_HOST || true)"
  port="$(get_env_value MYSQL_PORT || true)"
  database="$(get_env_value MYSQL_DATABASE || true)"
  username="$(get_env_value MYSQL_USERNAME || true)"
  password="$(get_env_value MYSQL_PASSWORD || true)"
  application_allowed_host="$(get_env_value MYSQL_APPLICATION_ALLOWED_HOST || true)"

  [[ "$port" =~ ^[0-9]+$ && "$port" -ge 1 && "$port" -le 65535 ]] || {
    log "ERROR" "MYSQL_PORT 无效：${port:-<空>}。"
    return 1
  }
  [[ "$database" =~ ^[A-Za-z0-9_]+$ ]] || {
    log "ERROR" "MYSQL_DATABASE 只允许字母、数字和下划线。"
    return 1
  }
  [[ "$username" =~ ^[A-Za-z0-9_.-]+$ ]] || {
    log "ERROR" "MYSQL_USERNAME 只允许字母、数字、点、下划线和短横线。"
    return 1
  }
  [[ "$password" =~ ^[A-Za-z0-9._~!@%+=:-]+$ ]] || {
    log "ERROR" "MYSQL_PASSWORD 必须非空，且仅允许字母、数字和 ._~!@%+=:-，以安全生成 MySQL 初始化 SQL。"
    return 1
  }

  admin_user="${MYSQL_ADMIN_USERNAME:-root}"
  admin_password="${MYSQL_ADMIN_PASSWORD:-}"
  [[ "$admin_user" =~ ^[A-Za-z0-9_.-]+$ ]] || {
    log "ERROR" "MYSQL_ADMIN_USERNAME 包含不安全字符。"
    return 1
  }
  [[ "$admin_password" != *$'\n'* && "$admin_password" != *$'\r'* ]] || {
    log "ERROR" "MYSQL_ADMIN_PASSWORD 不能包含换行符。"
    return 1
  }

  local -a account_hosts=()
  if is_local_mysql_host "$host"; then
    if [[ "$host" == "::1" ]]; then
      account_hosts=("::1" "localhost")
    else
      account_hosts=("127.0.0.1" "localhost")
    fi
    if [[ -n "$application_allowed_host" ]]; then
      valid_mysql_account_host "$application_allowed_host" || {
        log "ERROR" "MYSQL_APPLICATION_ALLOWED_HOST 包含不安全字符。"
        return 1
      }
      local already_present=false account_host
      for account_host in "${account_hosts[@]}"; do
        [[ "$account_host" == "$application_allowed_host" ]] && already_present=true
      done
      [[ "$already_present" == true ]] || account_hosts+=("$application_allowed_host")
    fi
  else
    [[ -n "$admin_password" ]] || {
      log "ERROR" "MYSQL_HOST=${host} 为远程 MySQL。首次自动创建数据库需要通过进程环境安全提供 MYSQL_ADMIN_USERNAME（默认 root）和 MYSQL_ADMIN_PASSWORD；脚本不会猜测或持久化数据库管理员凭据。"
      return 1
    }
    [[ -n "$application_allowed_host" ]] || {
      log "ERROR" "MYSQL_HOST=${host} 为远程 MySQL。请设置 MYSQL_APPLICATION_ALLOWED_HOST 为应用服务器的固定 IP、主机名或经审批的 MySQL 主机模式；脚本不会默认授予 '%' 通配访问。"
      return 1
    }
    valid_mysql_account_host "$application_allowed_host" || {
      log "ERROR" "MYSQL_APPLICATION_ALLOWED_HOST 包含不安全字符。"
      return 1
    }
    account_hosts=("$application_allowed_host")
  fi

  if [[ "$DRY_RUN" != true ]]; then
    require_command mysql
    mysql_is_supported || {
      log "ERROR" "mysql 客户端必须是 Oracle MySQL 8.x，当前客户端不受支持。"
      return 1
    }
  fi

  local temp_dir admin_file app_file sql_file account_host
  temp_dir="$(mktemp -d)"
  TEMP_PATHS+=("$temp_dir")
  admin_file="${temp_dir}/admin.cnf"
  app_file="${temp_dir}/application.cnf"
  sql_file="${temp_dir}/init.sql"

  {
    printf 'CREATE DATABASE IF NOT EXISTS `%s` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;\n' "$database"
    for account_host in "${account_hosts[@]}"; do
      # 这里不得执行 ALTER USER。重新部署不能轮换现有应用凭据或覆盖数据库所有者管理的密码。若已有账号密码与 MYSQL_PASSWORD 不同，
      # 下方连接探测会失败，须由操作人或 DBA 显式修正。
      printf "CREATE USER IF NOT EXISTS '%s'@'%s' IDENTIFIED BY '%s';\n" "$username" "$account_host" "$password"
      printf "GRANT ALL PRIVILEGES ON `%s`.* TO '%s'@'%s';\n" "$database" "$username" "$account_host"
    done
    printf 'FLUSH PRIVILEGES;\n'
  } >"$sql_file"
  chmod 600 "$sql_file"

  if [[ "$DRY_RUN" == true ]]; then
    log "CMD" "mysql --defaults-extra-file=<临时凭据文件> < <受保护的数据库初始化 SQL>"
    log "INFO" "预演：将创建数据库 ${database}、应用账号 ${username} 并仅授权给配置的应用连接来源。"
    return 0
  fi

  if [[ -n "$admin_password" ]]; then
    mysql_defaults_file "$admin_file" "$admin_user" "$admin_password" "$host" "$port"
    run_cmd mysql --defaults-extra-file="$admin_file" <"$sql_file"
  elif is_local_mysql_host "$host"; then
    if [[ "$(id -u)" -eq 0 ]]; then
      log "INFO" "未提供 MYSQL_ADMIN_PASSWORD，尝试使用 root 本机 socket 认证初始化数据库。"
      run_cmd mysql <"$sql_file"
    else
      require_command sudo
      log "INFO" "未提供 MYSQL_ADMIN_PASSWORD，尝试使用 sudo root 本机 socket 认证初始化数据库。"
      run_cmd sudo mysql <"$sql_file"
    fi
  else
    log "ERROR" "远程 MySQL 初始化缺少管理员密码。"
    return 1
  fi

  mysql_defaults_file "$app_file" "$username" "$password" "$host" "$port"
  run_cmd mysql --defaults-extra-file="$app_file" --database="$database" --batch --skip-column-names --execute='SELECT 1'
  log "INFO" "数据库 ${database} 和应用账号 ${username} 初始化完成，应用连接验证通过。"
}

# ============================ 首个超级管理员初始化 ============================

is_bootstrap_admin_placeholder() {
  [[ "$1" == REPLACE_WITH_* ]]
}

# load_bootstrap_admin_input 读取首次初始化管理员输入，不写回或清空原配置。
# 参数：无；使用受保护环境文件或当前终端输入。
# 返回值：输入完整返回成功；非交互缺项或确认不一致时失败。
load_bootstrap_admin_input() {
  BOOTSTRAP_ADMIN_DISPLAY_NAME="$(get_env_value IAM_BOOTSTRAP_ADMIN_DISPLAY_NAME || true)"
  BOOTSTRAP_ADMIN_ACCOUNT_NAME="$(get_env_value IAM_BOOTSTRAP_ADMIN_ACCOUNT_NAME || true)"
  BOOTSTRAP_ADMIN_PASSWORD="$(get_env_value IAM_BOOTSTRAP_ADMIN_PASSWORD || true)"

  is_bootstrap_admin_placeholder "$BOOTSTRAP_ADMIN_DISPLAY_NAME" && BOOTSTRAP_ADMIN_DISPLAY_NAME=""
  is_bootstrap_admin_placeholder "$BOOTSTRAP_ADMIN_ACCOUNT_NAME" && BOOTSTRAP_ADMIN_ACCOUNT_NAME=""
  is_bootstrap_admin_placeholder "$BOOTSTRAP_ADMIN_PASSWORD" && BOOTSTRAP_ADMIN_PASSWORD=""

  if [[ -z "$BOOTSTRAP_ADMIN_DISPLAY_NAME" || -z "$BOOTSTRAP_ADMIN_ACCOUNT_NAME" || -z "$BOOTSTRAP_ADMIN_PASSWORD" ]]; then
    if [[ "$ASSUME_YES" == true || ! -t 0 ]]; then
      log "ERROR" "首次创建超级管理员需要在 ${ENV_FILE} 设置 IAM_BOOTSTRAP_ADMIN_DISPLAY_NAME、IAM_BOOTSTRAP_ADMIN_ACCOUNT_NAME 和 IAM_BOOTSTRAP_ADMIN_PASSWORD。原配置会保留，仅用于首次初始化；已有账号不会自动改密。"
      return 1
    fi

    if [[ -z "$BOOTSTRAP_ADMIN_DISPLAY_NAME" ]]; then
      read -r -p "首个超级管理员显示名称: " BOOTSTRAP_ADMIN_DISPLAY_NAME
    fi
    if [[ -z "$BOOTSTRAP_ADMIN_ACCOUNT_NAME" ]]; then
      read -r -p "首个超级管理员账号: " BOOTSTRAP_ADMIN_ACCOUNT_NAME
    fi
    if [[ -z "$BOOTSTRAP_ADMIN_PASSWORD" ]]; then
      local confirmation
      read -r -s -p "首个超级管理员密码: " BOOTSTRAP_ADMIN_PASSWORD
      printf '\n'
      read -r -s -p "再次输入首个超级管理员密码: " confirmation
      printf '\n'
      if [[ "$BOOTSTRAP_ADMIN_PASSWORD" != "$confirmation" ]]; then
        log "ERROR" "两次输入的首个超级管理员密码不一致。"
        BOOTSTRAP_ADMIN_PASSWORD=""
        return 1
      fi
    fi
  fi

  [[ -n "$BOOTSTRAP_ADMIN_DISPLAY_NAME" && -n "$BOOTSTRAP_ADMIN_ACCOUNT_NAME" && -n "$BOOTSTRAP_ADMIN_PASSWORD" ]] || {
    log "ERROR" "首个超级管理员显示名称、账号和密码均不能为空。"
    return 1
  }
}

run_bootstrap_admin_status() {
  local bootstrap_binary="$1"
  CURRENT_STEP="检查首个超级管理员初始化状态"
  log "CMD" "runuser -u ${RUNTIME_USER} -- env ENV_FILE=<受保护环境文件> ${bootstrap_binary} --status"
  if [[ "$DRY_RUN" == true ]]; then
    return 3
  fi
  # 必须在命令之后立即读取 $?：if/fi 之后的 $? 是复合命令的退出状态（没有 else 分支时为 0），
  # 会把"尚未初始化"的 3 和真实错误都变成 0，调用方据此跳过首个管理员创建。
  local status=0
  runuser -u "$RUNTIME_USER" -- env "ENV_FILE=${ENV_FILE}" "$bootstrap_binary" --status >>"$LOG_FILE" 2>&1 || status=$?
  if [[ "$status" -eq 0 ]]; then
    return 0
  fi
  [[ "$status" -eq 3 ]] && return 3
  log "ERROR" "无法检查首个超级管理员初始化状态。请先确认迁移已成功执行且运行用户可读取环境文件。"
  return "$status"
}

run_bootstrap_admin_initialize() {
  local bootstrap_binary="$1"
  CURRENT_STEP="初始化首个超级管理员"
  log "CMD" "runuser -u ${RUNTIME_USER} -- env ENV_FILE=<受保护环境文件> ${bootstrap_binary} --display-name <已配置> --account-name <已配置> --password-stdin"

  if [[ "$VERBOSE" == true ]]; then
    if printf '%s' "$BOOTSTRAP_ADMIN_PASSWORD" | runuser -u "$RUNTIME_USER" -- env "ENV_FILE=${ENV_FILE}" "$bootstrap_binary" \
      --display-name "$BOOTSTRAP_ADMIN_DISPLAY_NAME" --account-name "$BOOTSTRAP_ADMIN_ACCOUNT_NAME" --password-stdin 2>&1 | tee -a "$LOG_FILE"; then
      return 0
    fi
  elif printf '%s' "$BOOTSTRAP_ADMIN_PASSWORD" | runuser -u "$RUNTIME_USER" -- env "ENV_FILE=${ENV_FILE}" "$bootstrap_binary" \
    --display-name "$BOOTSTRAP_ADMIN_DISPLAY_NAME" --account-name "$BOOTSTRAP_ADMIN_ACCOUNT_NAME" --password-stdin >>"$LOG_FILE" 2>&1; then
    return 0
  fi
  return 1
}

# ============================ 源码版本信息 ============================

source_revision() {
  if command -v git >/dev/null 2>&1; then
    git -C "$PROJECT_ROOT" rev-parse --short=12 HEAD 2>/dev/null || printf '%s' source
    return 0
  fi
  printf '%s' source
}
