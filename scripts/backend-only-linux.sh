#!/usr/bin/env bash
# Basic Platform 后端专用 Linux 发布与运行脚本。
#
# 该脚本只处理 Go 后端（API、可选 Worker 和数据库迁移），不会安装或构建前端、
# Node.js、npm、Nginx，也不会修改前端发布文件。它适用于前端部署在独立主机、CDN
# 或由其他团队管理的场景。

set -Eeuo pipefail
IFS=$'\n\t'
umask 077

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
PROJECT_ROOT="$(cd -- "${SCRIPT_DIR}/.." && pwd -P)"
BACKEND_ROOT="${PROJECT_ROOT}/backend"

MODE="deploy"
ENV_FILE="${BASIC_PLATFORM_ENV_FILE:-/etc/basic-platform/basic-platform.env}"
DEPLOY_ROOT="${BASIC_PLATFORM_DEPLOY_ROOT:-/opt/basic-platform}"
RUNTIME_USER="${BASIC_PLATFORM_RUNTIME_USER:-basic-platform}"
RUNTIME_GROUP="${BASIC_PLATFORM_RUNTIME_GROUP:-basic-platform}"
RELEASE_ID=""
KEEP_RELEASES=5
WITH_WORKER=false
SKIP_MIGRATION=false
SKIP_DATABASE_INIT=false
SKIP_VERIFY=false
ASSUME_YES=false
DRY_RUN=false
VERBOSE=false
LOG_FILE="${BASIC_PLATFORM_BACKEND_LOG:-/tmp/basic-platform-backend-$(date +%Y%m%d-%H%M%S)-$$.log}"
CURRENT_STEP="初始化"
TEMP_PATHS=()
BOOTSTRAP_ADMIN_DISPLAY_NAME=""
BOOTSTRAP_ADMIN_ACCOUNT_NAME=""
BOOTSTRAP_ADMIN_PASSWORD=""

mkdir -p "$(dirname -- "$LOG_FILE")"
: >"$LOG_FILE"

usage() {
  cat <<'USAGE'
用法：
  sudo bash scripts/backend-only-linux.sh [选项]

说明：
  仅构建和运行 Basic Platform Go 后端；不会调用 npm、不会构建前端，也不会安装 Node.js。
  默认仅运行 HTTP API。异步任务（例如审计导出）需要增加 --with-worker。

模式：
  --deploy                 构建、可选执行迁移、原子切换版本并启动服务（默认）
  --start                  启动并设为开机自启已有后端服务
  --stop                   停止并禁用已有后端服务
  --restart                重启已有后端服务
  --status                 查看已有后端服务状态
  --migrate                对当前发布版本执行数据库迁移

选项：
  --env-file PATH          运行环境文件；默认 /etc/basic-platform/basic-platform.env
  --deploy-root PATH       后端发布根目录；默认 /opt/basic-platform
  --release-id ID          指定发布版本号（字母、数字、点、下划线、短横线）
  --keep-releases N        保留最近 N 个后端发布版本，默认 5，至少为 1
  --with-worker            同时创建、启动 Worker 服务
  --skip-migration         部署时跳过数据库迁移（仅在确认无需迁移时使用）
  --skip-database-init     跳过本机 MySQL 数据库和应用账号初始化
  --skip-verify            跳过 gofmt、go vet 和 go test（不建议）
  --yes                    不要求交互确认
  --dry-run                仅输出将执行的操作，不修改系统
  --verbose                同时输出命令详细日志

数据库初始化：
  默认会在 MYSQL_HOST=localhost 或 127.0.0.1 时幂等创建 MYSQL_DATABASE、
  MYSQL_USERNAME 及应用账号权限，然后再执行迁移。初始化使用 MYSQL_ADMIN_USERNAME
  （默认 root）和 MYSQL_ADMIN_PASSWORD；本机 root socket 认证可不设置密码。
  远程 MySQL 仅在通过受保护进程环境提供 MYSQL_ADMIN_PASSWORD 并在环境文件设置
  MYSQL_APPLICATION_ALLOWED_HOST 时自动创建；否则需由数据库管理员预先准备，并使用
  --skip-database-init 明确跳过初始化。迁移成功后，未初始化的首个超级管理员会使用
  IAM_BOOTSTRAP_ADMIN_* 一次性配置或交互输入创建，成功后自动清除其中的密码。
  -h, --help               显示帮助

示例：
  # 首次部署 API（不构建前端、不运行 Worker）
  sudo bash scripts/backend-only-linux.sh \
    --deploy --yes \
    --env-file /etc/basic-platform/basic-platform.env \
    --deploy-root /opt/basic-platform

  # 部署 API 和异步 Worker
  sudo bash scripts/backend-only-linux.sh --deploy --with-worker --yes

  # 查看服务状态或仅执行迁移
  sudo bash scripts/backend-only-linux.sh --status --with-worker
  sudo bash scripts/backend-only-linux.sh --migrate --env-file /etc/basic-platform/basic-platform.env
USAGE
}

log() {
  local level="$1"
  shift
  printf '[%s] [%s] %s\n' "$(date '+%Y-%m-%d %H:%M:%S')" "$level" "$*" | tee -a "$LOG_FILE"
}

log_command() {
  local rendered="" item
  for item in "$@"; do
    printf -v item '%q' "$item"
    rendered+="${rendered:+ }${item}"
  done
  log "CMD" "$rendered"
}

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

on_error() {
  local exit_code=$?
  local line_no="${BASH_LINENO[0]:-unknown}"
  local failed_command="${BASH_COMMAND:-unknown}"
  trap - ERR
  log "ERROR" "步骤“${CURRENT_STEP}”失败：退出码=${exit_code}，脚本行=${line_no}。"
  log "ERROR" "失败命令：${failed_command}"
  log "ERROR" "完整日志：${LOG_FILE}"
  exit "$exit_code"
}

cleanup() {
  local path
  for path in "${TEMP_PATHS[@]:-}"; do
    [[ -n "$path" ]] && rm -rf -- "$path" 2>/dev/null || true
  done
}

trap cleanup EXIT
trap on_error ERR

require_command() {
  local command_name="$1"
  command -v "$command_name" >/dev/null 2>&1 || {
    log "ERROR" "缺少命令：${command_name}"
    return 1
  }
}

require_root() {
  if [[ "$DRY_RUN" == true ]]; then
    return 0
  fi
  if [[ "$(id -u)" -ne 0 ]]; then
    log "ERROR" "${MODE} 模式需要 root 权限；请使用 sudo bash scripts/backend-only-linux.sh ...。"
    return 1
  fi
}

confirm() {
  local prompt="$1"
  if [[ "$ASSUME_YES" == true || "$DRY_RUN" == true ]]; then
    return 0
  fi
  if [[ ! -t 0 ]]; then
    log "ERROR" "非交互终端执行修改操作时必须传入 --yes。"
    return 1
  fi
  local answer
  read -r -p "${prompt} [y/N] " answer
  [[ "$answer" == "y" || "$answer" == "Y" ]]
}

validate_release_id() {
  [[ "$RELEASE_ID" =~ ^[A-Za-z0-9._-]+$ ]] || {
    log "ERROR" "--release-id 仅允许字母、数字、点、下划线和短横线。"
    return 1
  }
}

get_env_value() {
  local key="$1"
  [[ -f "$ENV_FILE" ]] || return 1
  awk -v wanted="$key" '
    /^[[:space:]]*#/ || /^[[:space:]]*$/ { next }
    {
      line=$0
      sub(/^[[:space:]]*export[[:space:]]+/, "", line)
      pos=index(line, "=")
      if (pos == 0) next
      name=substr(line, 1, pos-1)
      gsub(/^[[:space:]]+|[[:space:]]+$/, "", name)
      if (name != wanted) next
      value=substr(line, pos+1)
      gsub(/^[[:space:]]+|[[:space:]]+$/, "", value)
      if ((substr(value,1,1) == "\"" && substr(value,length(value),1) == "\"") ||
          (substr(value,1,1) == "\047" && substr(value,length(value),1) == "\047")) {
        value=substr(value,2,length(value)-2)
      }
      print value
      exit
    }
  ' "$ENV_FILE"
}

set_env_value() {
  local key="$1"
  local value="$2"
  [[ "$key" =~ ^[A-Z][A-Z0-9_]*$ ]] || return 1
  [[ "$value" != *$'\n'* && "$value" != *$'\r'* ]] || return 1

  local temp_file
  temp_file="$(mktemp "$(dirname -- "$ENV_FILE")/.env.update.XXXXXX")"
  TEMP_PATHS+=("$temp_file")
  awk -v wanted="$key" -v replacement="${key}=${value}" '
    BEGIN { found=0 }
    {
      line=$0
      normalized=line
      sub(/^[[:space:]]*export[[:space:]]+/, "", normalized)
      pos=index(normalized, "=")
      name=(pos > 0 ? substr(normalized, 1, pos-1) : "")
      gsub(/^[[:space:]]+|[[:space:]]+$/, "", name)
      if (name == wanted) {
        if (!found) print replacement
        found=1
      } else {
        print line
      }
    }
    END { if (!found) print replacement }
  ' "$ENV_FILE" >"$temp_file"
  chmod 0600 "$temp_file"
  mv -- "$temp_file" "$ENV_FILE"
}

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

mysql_is_supported() {
  local version
  command -v mysql >/dev/null 2>&1 || return 1
  mysql --version 2>/dev/null | grep -qi 'mariadb' && return 1
  version="$(mysql_version || true)"
  [[ -n "$version" ]] && [[ "${version%%.*}" == 8 ]]
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
  # MySQL account host values support host names, IPv4/IPv6 literals and the % wildcard.
  # Quotes, whitespace and path-like characters are rejected before interpolating into SQL.
  [[ "$1" =~ ^[A-Za-z0-9._:%-]+$ ]]
}

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
      # Do not issue ALTER USER here. Re-deploying must not rotate an existing
      # application credential or overwrite a password managed by the database owner.
      # A pre-existing account whose password differs from MYSQL_PASSWORD will fail the
      # connection probe below and requires an explicit operator/DBA correction.
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

parse_args() {
  while (($# > 0)); do
    case "$1" in
      --deploy|--start|--stop|--restart|--status|--migrate)
        MODE="${1#--}"
        ;;
      --env-file)
        shift
        (($# > 0)) || { log "ERROR" "--env-file 缺少路径参数。"; exit 2; }
        ENV_FILE="$1"
        ;;
      --deploy-root)
        shift
        (($# > 0)) || { log "ERROR" "--deploy-root 缺少路径参数。"; exit 2; }
        DEPLOY_ROOT="$1"
        ;;
      --release-id)
        shift
        (($# > 0)) || { log "ERROR" "--release-id 缺少参数。"; exit 2; }
        RELEASE_ID="$1"
        ;;
      --keep-releases)
        shift
        (($# > 0)) || { log "ERROR" "--keep-releases 缺少参数。"; exit 2; }
        KEEP_RELEASES="$1"
        ;;
      --with-worker) WITH_WORKER=true ;;
      --skip-migration) SKIP_MIGRATION=true ;;
      --skip-database-init) SKIP_DATABASE_INIT=true ;;
      --skip-verify) SKIP_VERIFY=true ;;
      --yes) ASSUME_YES=true ;;
      --dry-run) DRY_RUN=true ;;
      --verbose) VERBOSE=true ;;
      -h|--help) usage; exit 0 ;;
      *) log "ERROR" "未知参数：$1"; usage >&2; exit 2 ;;
    esac
    shift
  done

  [[ "$KEEP_RELEASES" =~ ^[1-9][0-9]*$ ]] || {
    log "ERROR" "--keep-releases 必须是大于 0 的整数。"
    exit 2
  }
  [[ -z "$RELEASE_ID" ]] || validate_release_id
}

source_revision() {
  if command -v git >/dev/null 2>&1; then
    git -C "$PROJECT_ROOT" rev-parse --short=12 HEAD 2>/dev/null || printf '%s' source
    return 0
  fi
  printf '%s' source
}

default_release_id() {
  printf '%s-%s\n' "$(date -u '+%Y%m%dT%H%M%SZ')" "$(source_revision)"
}

service_name() {
  local component="$1"
  printf 'basic-platform-%s.service\n' "$component"
}

service_unit_path() {
  local component="$1"
  printf '/etc/systemd/system/%s\n' "$(service_name "$component")"
}

service_exists() {
  local component="$1"
  [[ -f "$(service_unit_path "$component")" ]] || systemctl cat "$(service_name "$component")" >>"$LOG_FILE" 2>&1
}

selected_components() {
  # 该函数通过进程替换和命令替换被多次调用；未启用 Worker 时也必须显式返回成功，
  # 否则在 set -e 下，末尾的条件判断会被误认为脚本失败。
  printf '%s\n' api
  if [[ "$WITH_WORKER" == true ]]; then
    printf '%s\n' worker
  fi
  return 0
}

verify_layout() {
  CURRENT_STEP="检查后端项目结构"
  [[ -f "${BACKEND_ROOT}/go.mod" ]] || {
    log "ERROR" "未找到后端 go.mod：${BACKEND_ROOT}/go.mod"
    return 1
  }
  [[ -f "${BACKEND_ROOT}/cmd/api/main.go" ]] || {
    log "ERROR" "未找到 API 入口：${BACKEND_ROOT}/cmd/api/main.go"
    return 1
  }
  [[ -f "${BACKEND_ROOT}/cmd/migrate/main.go" ]] || {
    log "ERROR" "未找到迁移入口：${BACKEND_ROOT}/cmd/migrate/main.go"
    return 1
  }
  [[ -f "${BACKEND_ROOT}/cmd/worker/main.go" ]] || {
    log "ERROR" "未找到 Worker 入口：${BACKEND_ROOT}/cmd/worker/main.go"
    return 1
  }
}

verify_prerequisites() {
  CURRENT_STEP="检查后端运行前置条件"
  require_command go
  require_command systemctl
  require_command runuser
  [[ -d /run/systemd/system ]] || {
    log "ERROR" "当前主机未运行 systemd，无法管理后端服务。"
    return 1
  }
  id "$RUNTIME_USER" >/dev/null 2>&1 || {
    log "ERROR" "运行用户不存在：${RUNTIME_USER}。请先通过 bootstrap-linux.sh 的 --deploy 完成首次运行用户和环境文件初始化，或由运维创建该用户。"
    return 1
  }
  getent group "$RUNTIME_GROUP" >/dev/null 2>&1 || {
    log "ERROR" "运行用户组不存在：${RUNTIME_GROUP}。"
    return 1
  }
  [[ -f "$ENV_FILE" ]] || {
    log "ERROR" "未找到环境文件：${ENV_FILE}。后端配置由 ENV_FILE 指向该文件读取。"
    return 1
  }
  runuser -u "$RUNTIME_USER" -- test -r "$ENV_FILE" || {
    log "ERROR" "运行用户 ${RUNTIME_USER} 无法读取环境文件：${ENV_FILE}。"
    return 1
  }
  log "INFO" "Go：$(go version)"
  log "INFO" "环境文件：${ENV_FILE}；发布根目录：${DEPLOY_ROOT}；组件：$(tr '\n' ',' <<<"$(selected_components)" | sed 's/,$//')"
}

write_service_unit() {
  local component="$1"
  local description executable unit_path template
  case "$component" in
    api)
      description="Basic Platform API"
      executable="api"
      ;;
    worker)
      description="Basic Platform Worker"
      executable="worker"
      ;;
    *)
      log "ERROR" "未知后端组件：${component}"
      return 2
      ;;
  esac

  unit_path="$(service_unit_path "$component")"
  # 与 bootstrap-linux.sh 保持相同的托管标记；两份脚本可以接力维护同一服务单元。
  if [[ -f "$unit_path" ]] && ! grep -Fqx '# Managed by Basic Platform bootstrap-linux.sh' "$unit_path"; then
    log "ERROR" "发现非项目托管的 systemd 单元，拒绝覆盖：${unit_path}"
    return 1
  fi

  template="$(mktemp)"
  TEMP_PATHS+=("$template")
  cat >"$template" <<UNIT
# Managed by Basic Platform bootstrap-linux.sh
[Unit]
Description=${description}
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=${RUNTIME_USER}
Group=${RUNTIME_GROUP}
WorkingDirectory=${DEPLOY_ROOT}/current
Environment=ENV_FILE=${ENV_FILE}
ExecStart=${DEPLOY_ROOT}/current/bin/${executable}
Restart=on-failure
RestartSec=5s
TimeoutStopSec=30s
KillSignal=SIGTERM
NoNewPrivileges=true
PrivateTmp=true
ProtectHome=true
ProtectSystem=full
# 文件存储和日志目录通常位于 /var；ProtectSystem=full 保护 /usr、/boot、/etc，
# 但保留 /var 等运行目录可写，避免在未读取环境文件的情况下猜测业务目录。
UMask=0027
LimitNOFILE=65535

[Install]
WantedBy=multi-user.target
UNIT
  run_cmd install -o root -g root -m 0644 "$template" "$unit_path"
  log "INFO" "已写入 systemd 单元：${unit_path}"
}

install_service_units() {
  CURRENT_STEP="创建后端 systemd 服务单元"
  local component
  while IFS= read -r component; do
    write_service_unit "$component"
  done < <(selected_components)
  run_cmd systemctl daemon-reload
}

verify_backend_code() {
  [[ "$SKIP_VERIFY" == false ]] || {
    log "WARN" "已跳过 Go 格式、静态检查和测试。"
    return 0
  }
  CURRENT_STEP="验证后端 Go 代码"
  run_cmd bash -c 'cd "$1" && test -z "$(gofmt -l .)"' _ "$BACKEND_ROOT"
  run_cmd bash -c 'cd "$1" && go mod verify' _ "$BACKEND_ROOT"
  run_cmd bash -c 'cd "$1" && go vet ./...' _ "$BACKEND_ROOT"
  run_cmd bash -c 'cd "$1" && go test ./...' _ "$BACKEND_ROOT"
}

build_backend_release() {
  local staging_dir="$1"
  local build_timestamp="$2"
  local revision="$3"

  CURRENT_STEP="下载后端 Go Module 依赖"
  run_cmd bash -c 'cd "$1" && go mod download' _ "$BACKEND_ROOT"

  verify_backend_code

  CURRENT_STEP="构建后端发布二进制"
  run_cmd mkdir -p "$staging_dir/bin"
  run_cmd bash -c 'cd "$1" && go build -trimpath -o "$2/bin/api" ./cmd/api' _ "$BACKEND_ROOT" "$staging_dir"
  run_cmd bash -c 'cd "$1" && go build -trimpath -o "$2/bin/migrate" ./cmd/migrate' _ "$BACKEND_ROOT" "$staging_dir"
  run_cmd bash -c 'cd "$1" && go build -trimpath -o "$2/bin/bootstrap-admin" ./cmd/bootstrap-admin' _ "$BACKEND_ROOT" "$staging_dir"
  # 始终打包 Worker：即使本次不启用 Worker，已有 Worker 服务重启后也不能因
  # current 已切换而找不到二进制文件。--with-worker 只决定是否由本脚本管理该服务。
  run_cmd bash -c 'cd "$1" && go build -trimpath -o "$2/bin/worker" ./cmd/worker' _ "$BACKEND_ROOT" "$staging_dir"

  CURRENT_STEP="写入后端发布元数据"
  run_cmd bash -c 'printf "release_id=%s\nbuilt_at_utc=%s\nsource_revision=%s\n" "$1" "$2" "$3" > "$4/release-info"' _ "$RELEASE_ID" "$build_timestamp" "$revision" "$staging_dir"
  run_cmd chmod 0755 "$staging_dir/bin" "$staging_dir/bin/api" "$staging_dir/bin/migrate" "$staging_dir/bin/bootstrap-admin" "$staging_dir/bin/worker"
}

run_migrations_from() {
  local migrate_binary="$1"
  CURRENT_STEP="执行数据库迁移"
  [[ -x "$migrate_binary" ]] || {
    log "ERROR" "未找到迁移二进制：${migrate_binary}"
    return 1
  }
  if ! run_cmd runuser -u "$RUNTIME_USER" -- env "ENV_FILE=${ENV_FILE}" "$migrate_binary"; then
    log "ERROR" "数据库迁移命令执行失败。请检查数据库地址、账号密码、网络连通性，以及环境文件中的 MySQL 配置。"
    log "ERROR" "以下为迁移命令写入日志的最后 60 行："
    tail -n 60 "$LOG_FILE" >&2 || true
    return 1
  fi
}

is_bootstrap_admin_placeholder() {
  [[ "$1" == REPLACE_WITH_* ]]
}

clear_bootstrap_admin_password() {
  local configured_password
  configured_password="$(get_env_value IAM_BOOTSTRAP_ADMIN_PASSWORD || true)"
  [[ -n "$configured_password" ]] || return 0

  CURRENT_STEP="清除一次性超级管理员密码"
  set_env_value IAM_BOOTSTRAP_ADMIN_PASSWORD ""
  run_cmd chown root:"$RUNTIME_GROUP" "$ENV_FILE"
  run_cmd chmod 0640 "$ENV_FILE"
  log "INFO" "已清除环境文件中的一次性 IAM_BOOTSTRAP_ADMIN_PASSWORD；现有管理员未被修改。"
}

load_bootstrap_admin_input() {
  BOOTSTRAP_ADMIN_DISPLAY_NAME="$(get_env_value IAM_BOOTSTRAP_ADMIN_DISPLAY_NAME || true)"
  BOOTSTRAP_ADMIN_ACCOUNT_NAME="$(get_env_value IAM_BOOTSTRAP_ADMIN_ACCOUNT_NAME || true)"
  BOOTSTRAP_ADMIN_PASSWORD="$(get_env_value IAM_BOOTSTRAP_ADMIN_PASSWORD || true)"

  is_bootstrap_admin_placeholder "$BOOTSTRAP_ADMIN_DISPLAY_NAME" && BOOTSTRAP_ADMIN_DISPLAY_NAME=""
  is_bootstrap_admin_placeholder "$BOOTSTRAP_ADMIN_ACCOUNT_NAME" && BOOTSTRAP_ADMIN_ACCOUNT_NAME=""
  is_bootstrap_admin_placeholder "$BOOTSTRAP_ADMIN_PASSWORD" && BOOTSTRAP_ADMIN_PASSWORD=""

  if [[ -z "$BOOTSTRAP_ADMIN_DISPLAY_NAME" || -z "$BOOTSTRAP_ADMIN_ACCOUNT_NAME" || -z "$BOOTSTRAP_ADMIN_PASSWORD" ]]; then
    if [[ "$ASSUME_YES" == true || ! -t 0 ]]; then
      log "ERROR" "首次创建超级管理员需要在 ${ENV_FILE} 设置 IAM_BOOTSTRAP_ADMIN_DISPLAY_NAME、IAM_BOOTSTRAP_ADMIN_ACCOUNT_NAME 和 IAM_BOOTSTRAP_ADMIN_PASSWORD。密码仅在成功初始化后自动清除。"
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
  if runuser -u "$RUNTIME_USER" -- env "ENV_FILE=${ENV_FILE}" "$bootstrap_binary" --status >>"$LOG_FILE" 2>&1; then
    return 0
  fi
  local status=$?
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

bootstrap_first_super_admin() {
  local bootstrap_binary="$1"
  [[ -x "$bootstrap_binary" ]] || {
    log "ERROR" "未找到首个超级管理员初始化二进制：${bootstrap_binary}"
    return 1
  }

  if [[ "$DRY_RUN" == true ]]; then
    log "INFO" "预演：数据库迁移完成后将检查首个超级管理员；未初始化时将从受保护环境文件或交互输入读取一次性凭据。"
    return 0
  fi

  local status
  if run_bootstrap_admin_status "$bootstrap_binary"; then
    log "INFO" "首个超级管理员已初始化，跳过创建且不覆盖现有账号。"
    clear_bootstrap_admin_password
    return 0
  else
    status=$?
  fi
  if [[ "$status" -ne 3 ]]; then
    return "$status"
  fi

  load_bootstrap_admin_input
  if ! run_bootstrap_admin_initialize "$bootstrap_binary"; then
    log "ERROR" "首个超级管理员初始化失败；未清除一次性密码，以便修复配置后重试。"
    return 1
  fi

  clear_bootstrap_admin_password
  BOOTSTRAP_ADMIN_PASSWORD=""
  log "INFO" "首个超级管理员初始化完成。"
}

run_current_migrations() {
  run_migrations_from "${DEPLOY_ROOT}/current/bin/migrate"
}

remove_expired_releases() {
  local releases_dir="$1"
  local current_target="$2"
  local release_path index
  local -a release_paths=()

  while IFS= read -r release_path; do
    [[ "$release_path" == "$current_target" ]] && continue
    release_paths+=("$release_path")
  done < <(find "$releases_dir" -mindepth 1 -maxdepth 1 -type d -printf '%T@ %p\n' | sort -rn | awk '{print $2}')

  for ((index = KEEP_RELEASES - 1; index < ${#release_paths[@]}; index++)); do
    CURRENT_STEP="清理过期后端发布版本"
    run_cmd rm -rf -- "${release_paths[$index]}"
  done
}

restart_selected_services() {
  local component service
  while IFS= read -r component; do
    service="$(service_name "$component")"
    run_cmd systemctl enable "$service"
    run_cmd systemctl restart "$service"
    run_cmd systemctl is-active --quiet "$service"
  done < <(selected_components)
}

deploy_backend() {
  require_root
  verify_layout
  verify_prerequisites
  confirm "将仅构建并发布 Go 后端到 ${DEPLOY_ROOT}，不会构建前端，是否继续？" || {
    log "INFO" "用户取消部署。"
    return 0
  }

  if [[ -z "$RELEASE_ID" ]]; then
    RELEASE_ID="$(default_release_id)"
  fi
  validate_release_id

  local releases_dir="${DEPLOY_ROOT}/releases"
  local release_dir="${releases_dir}/${RELEASE_ID}"
  local staging_dir current_link current_target
  [[ ! -e "$release_dir" && ! -L "$release_dir" ]] || {
    log "ERROR" "发布版本已存在：${release_dir}；请改用 --release-id。"
    return 1
  }

  initialize_database

  if [[ "$DRY_RUN" == true ]]; then
    local component_description="API"
    [[ "$WITH_WORKER" == true ]] && component_description="API 和 Worker"
    log "INFO" "预演：构建 ${component_description} 到 ${release_dir}，执行迁移并切换 ${DEPLOY_ROOT}/current。"
    return 0
  fi

  CURRENT_STEP="创建后端发布目录"
  run_cmd install -d -o root -g root -m 0755 "$DEPLOY_ROOT" "$releases_dir"
  staging_dir="$(mktemp -d "${releases_dir}/.staging.XXXXXX")"
  TEMP_PATHS+=("$staging_dir")

  build_backend_release "$staging_dir" "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" "$(source_revision)"

  CURRENT_STEP="固化后端发布版本"
  run_cmd mv "$staging_dir" "$release_dir"
  # staging_dir 已原子移动为正式发布目录，退出清理时不能再删除它。
  TEMP_PATHS=()
  run_cmd chmod 0755 "$release_dir"

  if [[ "$SKIP_MIGRATION" == true ]]; then
    log "WARN" "已跳过数据库迁移，同时跳过首个超级管理员自动初始化。"
  else
    # 必须使用刚刚构建出的候选版本执行迁移，不能在切换前误用旧 current 版本。
    run_migrations_from "${release_dir}/bin/migrate"
    bootstrap_first_super_admin "${release_dir}/bin/bootstrap-admin"
  fi

  CURRENT_STEP="原子切换后端当前版本"
  current_link="${DEPLOY_ROOT}/current"
  run_cmd ln -s "$release_dir" "${current_link}.new"
  run_cmd mv -Tf "${current_link}.new" "$current_link"
  current_target="$(readlink -f "$current_link")"

  install_service_units
  restart_selected_services
  remove_expired_releases "$releases_dir" "$current_target"
  log "INFO" "后端发布完成：${current_target}。运行组件：$(tr '\n' ',' <<<"$(selected_components)" | sed 's/,$//')。"
}

require_existing_services() {
  require_command systemctl
  local component missing=false
  while IFS= read -r component; do
    if ! service_exists "$component"; then
      log "ERROR" "未找到后端服务单元：$(service_name "$component")。请先执行 --deploy。"
      missing=true
    fi
  done < <(selected_components)
  [[ "$missing" == false ]]
}

manage_services() {
  case "$MODE" in
    start|stop|restart)
      require_root
      verify_prerequisites
      require_existing_services
      confirm "将执行后端服务 ${MODE} 操作，是否继续？" || return 0
      local component service
      while IFS= read -r component; do
        service="$(service_name "$component")"
        case "$MODE" in
          start) run_cmd systemctl enable --now "$service" ;;
          stop) run_cmd systemctl disable --now "$service" ;;
          restart) run_cmd systemctl restart "$service" ;;
        esac
      done < <(selected_components)
      ;;
    status)
      require_command systemctl
      require_existing_services
      local component
      while IFS= read -r component; do
        systemctl --no-pager --full status "$(service_name "$component")" || true
      done < <(selected_components)
      ;;
    migrate)
      require_root
      verify_prerequisites
      initialize_database
      run_current_migrations
      bootstrap_first_super_admin "${DEPLOY_ROOT}/current/bin/bootstrap-admin"
      ;;
  esac
}

main() {
  parse_args "$@"
  cd "$PROJECT_ROOT"
  log "INFO" "项目根目录：${PROJECT_ROOT}"
  log "INFO" "执行模式：${MODE}；日志：${LOG_FILE}"

  case "$MODE" in
    deploy) deploy_backend ;;
    start|stop|restart|status|migrate) manage_services ;;
    *) log "ERROR" "未实现的模式：${MODE}"; return 2 ;;
  esac

  log "INFO" "执行完成。"
}

main "$@"
