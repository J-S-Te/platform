#!/usr/bin/env bash
# Basic Platform Linux 环境检测与初始化脚本。
# 仅面向 Linux；支持按发行版安装基础软件，并按项目清单安装 Go、Node.js、MySQL 和项目依赖。

set -Eeuo pipefail
IFS=$'\n\t'
umask 077

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
PROJECT_ROOT="$(cd -- "${SCRIPT_DIR}/.." && pwd -P)"
ENV_FILE="${PROJECT_ROOT}/.env"
ENV_EXAMPLE="${PROJECT_ROOT}/.env.example"
LOG_FILE="${BASIC_PLATFORM_BOOTSTRAP_LOG:-/tmp/basic-platform-bootstrap-$(date +%Y%m%d-%H%M%S)-$$.log}"

MODE="check"
ASSUME_YES=false
DRY_RUN=false
VERBOSE=false
SKIP_MYSQL_SERVER=false
SKIP_DATABASE_INIT=false
SKIP_MIGRATION=false
SKIP_PROJECT_DEPS=false
DEPLOY_ROOT="${BASIC_PLATFORM_DEPLOY_ROOT:-${PROJECT_ROOT}/.deploy}"
DEPLOY_RELEASE_ID=""
KEEP_RELEASES=5
RESTART_SERVICES=false
SKIP_CODE_VERIFY=false
PURGE_DATABASE=false
REMOVE_SYSTEM_USER=""
NGINX_SITE_PATHS=()
RUNTIME_PATHS=()
CURRENT_STEP="启动"
DISTRO_ID=""
DISTRO_NAME=""
DISTRO_VERSION=""
DISTRO_LIKE=""
PACKAGE_MANAGER=""
OS_ARCH=""
GO_ARCH=""
NODE_ARCH=""
REQUIRED_GO_VERSION=""
REQUIRED_NODE_VERSION="${NODE_VERSION:-22.17.0}"
ROOT_PREFIX=(command)
TEMP_PATHS=()
BOOTSTRAP_ADMIN_DISPLAY_NAME=""
BOOTSTRAP_ADMIN_ACCOUNT_NAME=""
BOOTSTRAP_ADMIN_PASSWORD=""
CHECK_FAILURES=0

mkdir -p "$(dirname -- "$LOG_FILE")"
: >"$LOG_FILE"

usage() {
  cat <<'USAGE'
用法：
  bash scripts/bootstrap-linux.sh [选项]

模式：
  --check               仅检测系统、工具链、配置和项目依赖（默认，不修改系统）
  --bootstrap           安装并配置完整开发环境，随后执行验证
  --install-system      仅安装操作系统依赖、Go、Node.js 和本机 MySQL
  --install-project     仅安装 Go Module 与前端 npm 依赖，不执行前端构建
  --configure           仅创建/补齐 .env、数据目录和开发 JWT 密钥
  --verify              执行 Go 格式/静态检查/测试及前端测试
  --deploy              检查环境、构建前后端、执行迁移并原子切换发布版本
  --stop                停止并禁用 API、Worker 服务，不删除任何数据或配置
  --uninstall           卸载原生部署的应用文件、服务单元、运行文件和环境文件

控制选项：
  --env-file PATH       指定环境文件，默认使用项目根目录 .env
  --yes                 非交互确认系统安装和数据库初始化
  --dry-run             只打印将执行的修改命令
  --verbose             同时在终端显示被执行命令的完整输出
  --skip-mysql-server   不安装本机 MySQL Server，适用于使用远程 MySQL
  --skip-database-init  不创建本地数据库和应用账号
  --skip-migration      不执行数据库迁移
  --skip-project-deps   不执行 go mod download 和 npm ci
  --deploy-root PATH    发布根目录，默认 .deploy；生产建议 /opt/basic-platform
  --release-id ID       指定发布版本号；仅允许字母、数字、点、下划线和短横线
  --keep-releases N     成功发布后最多保留 N 个历史版本（默认 5，至少 1）
  --restart-services    切换版本后启用并重启 basic-platform-api 与 basic-platform-worker
  --skip-code-verify    发布前跳过 gofmt、vet、test 和前端测试（不建议）
  --purge-database      仅配合 --uninstall：删除环境文件指定的 MySQL 数据库；本机库还删除脚本创建的应用账号
  --nginx-site PATH     仅配合 --uninstall：删除明确指定的本应用 Nginx 虚拟主机配置，可重复指定
  --remove-system-user USER
                        仅配合 --uninstall：删除明确指定的专用运行用户及其同名空用户组
  -h, --help            显示帮助

环境变量：
  NODE_VERSION                 覆盖默认 Node.js 版本（默认 22.17.0）
  MYSQL_ADMIN_USERNAME         本地 MySQL 管理账号（默认 root）
  MYSQL_ADMIN_PASSWORD         MySQL 管理密码；本机 socket 管理可不设置，远程首次建库必须安全提供
  BASIC_PLATFORM_BOOTSTRAP_LOG 覆盖日志文件路径
  BASIC_PLATFORM_RUNTIME_USER  覆盖原生服务运行用户（默认 basic-platform）
  BASIC_PLATFORM_RUNTIME_GROUP 覆盖原生服务运行用户组（默认 basic-platform）

示例：
  bash scripts/bootstrap-linux.sh --check
  bash scripts/bootstrap-linux.sh --bootstrap --yes
  bash scripts/bootstrap-linux.sh --bootstrap --yes --skip-mysql-server --skip-database-init
  bash scripts/bootstrap-linux.sh --deploy --env-file /etc/basic-platform/basic-platform.env \
    --deploy-root /opt/basic-platform --restart-services --yes
  bash scripts/bootstrap-linux.sh --stop --yes
  bash scripts/bootstrap-linux.sh --uninstall --yes --purge-database \
    --env-file /etc/basic-platform/basic-platform.env --deploy-root /opt/basic-platform \
    --nginx-site /etc/nginx/conf.d/basic-platform.conf --remove-system-user basic-platform
USAGE
}

log() {
  local level="$1"
  shift
  local message="$*"
  printf '[%s] [%s] %s\n' "$(date '+%Y-%m-%d %H:%M:%S')" "$level" "$message" | tee -a "$LOG_FILE"
}

log_command() {
  local rendered=""
  local item
  for item in "$@"; do
    printf -v item '%q' "$item"
    rendered+="${rendered:+ }${item}"
  done
  log "CMD" "$rendered"
}

cleanup() {
  local path
  for path in "${TEMP_PATHS[@]:-}"; do
    if [[ -n "$path" && -e "$path" ]]; then
      rm -rf -- "$path"
    fi
  done
}

error_hint() {
  case "$CURRENT_STEP" in
    *软件包*) printf '%s' '检查软件源、DNS、代理和 sudo 权限，然后根据日志中的包名重试。' ;;
    *Go\ Module*) printf '%s' '检查 GOPROXY、网络和 backend/go.sum；不要手工删除校验失败的依赖记录。' ;;
    *Go*) printf '%s' '检查 go.dev 网络连通性、系统架构和 /usr/local 写权限；也可预先安装 go.mod 要求的 Go 版本。' ;;
    *Node*) printf '%s' '检查 nodejs.org 网络连通性、系统架构和 /usr/local 写权限；也可通过 NODE_VERSION 指定兼容版本。' ;;
    *MySQL*|*数据库*) printf '%s' '检查 MySQL 服务状态、管理员凭据、.env 连接参数和端口占用；远程数据库可使用 --skip-mysql-server --skip-database-init。' ;;
    *npm*) printf '%s' '检查 npm registry、代理、package-lock.json 与 Node.js 版本；清理 frontend/node_modules 后可重试。' ;;
    *迁移*) printf '%s' '检查数据库连接、账号 DDL 权限和迁移校验和；禁止修改已经执行过的历史迁移。' ;;
    *) printf '%s' '查看日志中的首个失败命令，修复后重新执行同一模式。' ;;
  esac
}

on_error() {
  local exit_code=$?
  local line_no="${BASH_LINENO[0]:-unknown}"
  local failed_command="${BASH_COMMAND:-unknown}"
  trap - ERR
  printf '\n' >&2
  log "ERROR" "步骤“${CURRENT_STEP}”失败：退出码=${exit_code}，脚本行=${line_no}。" >&2
  log "ERROR" "失败命令：${failed_command}" >&2
  log "ERROR" "处理建议：$(error_hint)" >&2
  log "ERROR" "完整日志：${LOG_FILE}" >&2
  if [[ -f "$LOG_FILE" ]]; then
    printf '%s\n' '----- 最近日志 -----' >&2
    tail -n 30 "$LOG_FILE" >&2 || true
    printf '%s\n' '--------------------' >&2
  fi
  exit "$exit_code"
}

trap cleanup EXIT
trap on_error ERR

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

run_retry() {
  local attempts="$1"
  shift
  local attempt=1
  while (( attempt <= attempts )); do
    log_command "$@"
    if [[ "$DRY_RUN" == true ]]; then
      return 0
    fi
    if [[ "$VERBOSE" == true ]]; then
      if "$@" 2>&1 | tee -a "$LOG_FILE"; then
        return 0
      fi
    elif "$@" >>"$LOG_FILE" 2>&1; then
      return 0
    fi
    if (( attempt == attempts )); then
      return 1
    fi
    log "WARN" "命令执行失败，5 秒后重试（${attempt}/${attempts}）。"
    sleep 5
    ((attempt++))
  done
}

require_command() {
  local command_name="$1"
  command -v "$command_name" >/dev/null 2>&1 || {
    log "ERROR" "缺少命令：${command_name}"
    return 1
  }
}

confirm() {
  local prompt="$1"
  if [[ "$ASSUME_YES" == true || "$DRY_RUN" == true ]]; then
    return 0
  fi
  if [[ ! -t 0 ]]; then
    log "ERROR" "非交互终端必须增加 --yes 才能执行修改操作。"
    return 1
  fi
  local answer
  read -r -p "${prompt} [y/N] " answer
  [[ "$answer" == "y" || "$answer" == "Y" ]]
}

parse_args() {
  while (($# > 0)); do
    case "$1" in
      --check|--bootstrap|--install-system|--install-project|--configure|--verify|--deploy|--stop|--uninstall)
        MODE="${1#--}"
        ;;
      --env-file)
        shift
        (($# > 0)) || { log "ERROR" "--env-file 缺少路径参数"; exit 2; }
        ENV_FILE="$1"
        ;;
      --yes) ASSUME_YES=true ;;
      --dry-run) DRY_RUN=true ;;
      --verbose) VERBOSE=true ;;
      --skip-mysql-server) SKIP_MYSQL_SERVER=true ;;
      --skip-database-init) SKIP_DATABASE_INIT=true ;;
      --skip-migration) SKIP_MIGRATION=true ;;
      --skip-project-deps) SKIP_PROJECT_DEPS=true ;;
      --deploy-root)
        shift
        (($# > 0)) || { log "ERROR" "--deploy-root 缺少路径参数"; exit 2; }
        DEPLOY_ROOT="$1"
        ;;
      --release-id)
        shift
        (($# > 0)) || { log "ERROR" "--release-id 缺少版本号"; exit 2; }
        DEPLOY_RELEASE_ID="$1"
        ;;
      --keep-releases)
        shift
        (($# > 0)) || { log "ERROR" "--keep-releases 缺少数量"; exit 2; }
        KEEP_RELEASES="$1"
        ;;
      --restart-services) RESTART_SERVICES=true ;;
      --skip-code-verify) SKIP_CODE_VERIFY=true ;;
      --purge-database) PURGE_DATABASE=true ;;
      --nginx-site)
        shift
        (($# > 0)) || { log "ERROR" "--nginx-site 缺少路径参数"; exit 2; }
        NGINX_SITE_PATHS+=("$1")
        ;;
      --remove-system-user)
        shift
        (($# > 0)) || { log "ERROR" "--remove-system-user 缺少用户名参数"; exit 2; }
        REMOVE_SYSTEM_USER="$1"
        ;;
      -h|--help) usage; exit 0 ;;
      *) log "ERROR" "未知参数：$1"; usage; exit 2 ;;
    esac
    shift
  done
  local env_directory
  env_directory="$(dirname -- "$ENV_FILE")"
  if [[ -d "$env_directory" ]]; then
    ENV_FILE="$(cd -- "$env_directory" && pwd -P)/$(basename -- "$ENV_FILE")"
  elif [[ "$MODE" == "deploy" || "$MODE" == "uninstall" ]]; then
    # 首次原生发布需要由部署流程创建 /etc 下的环境文件目录；卸载也需要容忍文件已被人工删除。
    if [[ "$ENV_FILE" != /* ]]; then
      ENV_FILE="${PROJECT_ROOT}/${ENV_FILE}"
    fi
    if [[ "$MODE" == "deploy" ]]; then
      log "INFO" "环境文件所在目录不存在：${env_directory}；首次部署将创建该目录和生产环境模板。"
    else
      log "WARN" "环境文件所在目录不存在：${env_directory}；将跳过基于环境文件的运行数据和数据库清理。"
    fi
  else
    log "ERROR" "环境文件所在目录不存在：${env_directory}"
    return 2
  fi

  if [[ "$DEPLOY_ROOT" != /* ]]; then
    DEPLOY_ROOT="${PROJECT_ROOT}/${DEPLOY_ROOT}"
  fi
  if [[ -n "$DEPLOY_RELEASE_ID" && ! "$DEPLOY_RELEASE_ID" =~ ^[A-Za-z0-9][A-Za-z0-9._-]*$ ]]; then
    log "ERROR" "--release-id 仅允许字母、数字、点、下划线和短横线，且必须以字母或数字开头。"
    return 2
  fi
  if [[ ! "$KEEP_RELEASES" =~ ^[1-9][0-9]*$ ]]; then
    log "ERROR" "--keep-releases 必须是大于等于 1 的整数。"
    return 2
  fi
  if [[ "$PURGE_DATABASE" == true && "$MODE" != "uninstall" ]]; then
    log "ERROR" "--purge-database 只能与 --uninstall 一起使用。"
    return 2
  fi
  if ((${#NGINX_SITE_PATHS[@]} > 0)) && [[ "$MODE" != "uninstall" ]]; then
    log "ERROR" "--nginx-site 只能与 --uninstall 一起使用。"
    return 2
  fi
  if [[ -n "$REMOVE_SYSTEM_USER" && "$MODE" != "uninstall" ]]; then
    log "ERROR" "--remove-system-user 只能与 --uninstall 一起使用。"
    return 2
  fi
  if [[ -n "$REMOVE_SYSTEM_USER" && ! "$REMOVE_SYSTEM_USER" =~ ^[a-z_][a-z0-9_-]*[$]?$ ]]; then
    log "ERROR" "--remove-system-user 必须是有效的 Linux 用户名。"
    return 2
  fi
  if [[ -n "$REMOVE_SYSTEM_USER" && ! "$REMOVE_SYSTEM_USER" =~ ^[a-z_][a-z0-9_-]*[$]?$ ]]; then
    log "ERROR" "--remove-system-user 用户名格式无效。"
    return 2
  fi
}

map_architecture() {
  OS_ARCH="$(uname -m)"
  case "$OS_ARCH" in
    x86_64|amd64)
      GO_ARCH="amd64"
      NODE_ARCH="x64"
      ;;
    aarch64|arm64)
      GO_ARCH="arm64"
      NODE_ARCH="arm64"
      ;;
    *)
      log "ERROR" "暂不支持的 CPU 架构：${OS_ARCH}。当前脚本支持 x86_64 和 aarch64。"
      return 1
      ;;
  esac
}

detect_system() {
  CURRENT_STEP="检测 Linux 系统"
  [[ "$(uname -s)" == "Linux" ]] || {
    log "ERROR" "该脚本仅支持 Linux，当前系统为 $(uname -s)。"
    return 1
  }
  [[ -r /etc/os-release ]] || {
    log "ERROR" "无法读取 /etc/os-release，不能安全识别 Linux 发行版。"
    return 1
  }

  # shellcheck disable=SC1091
  source /etc/os-release
  DISTRO_ID="${ID:-unknown}"
  DISTRO_NAME="${PRETTY_NAME:-${NAME:-unknown}}"
  DISTRO_VERSION="${VERSION_ID:-unknown}"
  DISTRO_LIKE="${ID_LIKE:-}"
  map_architecture

  if command -v apt-get >/dev/null 2>&1; then
    PACKAGE_MANAGER="apt"
  elif command -v dnf >/dev/null 2>&1; then
    PACKAGE_MANAGER="dnf"
  elif command -v yum >/dev/null 2>&1; then
    PACKAGE_MANAGER="yum"
  elif command -v zypper >/dev/null 2>&1; then
    PACKAGE_MANAGER="zypper"
  elif command -v pacman >/dev/null 2>&1; then
    PACKAGE_MANAGER="pacman"
  elif command -v apk >/dev/null 2>&1; then
    PACKAGE_MANAGER="apk"
  else
    log "ERROR" "未识别到受支持的软件包管理器。"
    return 1
  fi

  REQUIRED_GO_VERSION="$(awk '$1 == "go" { print $2; exit }' "${PROJECT_ROOT}/backend/go.mod")"
  [[ "$REQUIRED_GO_VERSION" =~ ^[0-9]+\.[0-9]+(\.[0-9]+)?$ ]] || {
    log "ERROR" "backend/go.mod 中的 Go 版本格式无效：${REQUIRED_GO_VERSION:-<空>}"
    return 1
  }
  [[ "$REQUIRED_NODE_VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || {
    log "ERROR" "NODE_VERSION 格式无效：${REQUIRED_NODE_VERSION}"
    return 1
  }

  local hardware_vendor="unknown"
  local hardware_model="unknown"
  [[ -r /sys/devices/virtual/dmi/id/sys_vendor ]] && hardware_vendor="$(tr -d '\n' </sys/devices/virtual/dmi/id/sys_vendor)"
  [[ -r /sys/devices/virtual/dmi/id/product_name ]] && hardware_model="$(tr -d '\n' </sys/devices/virtual/dmi/id/product_name)"

  log "INFO" "系统：${DISTRO_NAME}（ID=${DISTRO_ID}，版本=${DISTRO_VERSION}）"
  log "INFO" "内核：$(uname -r)，架构：${OS_ARCH}，包管理器：${PACKAGE_MANAGER}"
  log "INFO" "设备：${hardware_vendor} ${hardware_model}"
  log "INFO" "项目要求：Go >= ${REQUIRED_GO_VERSION}，Node.js ^20.19.0 或 >=22.12.0，MySQL 8.x"
}

ensure_root_access() {
  if (( EUID == 0 )); then
    # CentOS 7 ships Bash 4.2. Under `set -u`, expanding an empty array via
    # "${ROOT_PREFIX[@]}" raises an unbound-variable error. `command` is a
    # shell builtin that safely acts as a no-op command prefix for root.
    ROOT_PREFIX=(command)
    return 0
  fi
  require_command sudo
  if [[ "$DRY_RUN" == true ]]; then
    ROOT_PREFIX=(sudo)
    return 0
  fi
  CURRENT_STEP="获取 sudo 权限"
  sudo -v
  ROOT_PREFIX=(sudo)
}

install_base_packages() {
  CURRENT_STEP="安装 Linux 基础软件包"
  ensure_root_access
  case "$PACKAGE_MANAGER" in
    apt)
      run_retry 3 "${ROOT_PREFIX[@]}" env DEBIAN_FRONTEND=noninteractive apt-get update
      run_retry 3 "${ROOT_PREFIX[@]}" env DEBIAN_FRONTEND=noninteractive apt-get install -y \
        ca-certificates curl git make openssl tar gzip xz-utils unzip build-essential pkg-config python3 tzdata
      ;;
    dnf)
      run_retry 3 "${ROOT_PREFIX[@]}" dnf install -y \
        ca-certificates curl git make openssl tar gzip xz unzip gcc gcc-c++ glibc-devel pkgconf-pkg-config python3 tzdata
      ;;
    yum)
      run_retry 3 "${ROOT_PREFIX[@]}" yum install -y \
        ca-certificates curl git make openssl tar gzip xz unzip gcc gcc-c++ glibc-devel pkgconfig python3 tzdata
      ;;
    zypper)
      run_retry 3 "${ROOT_PREFIX[@]}" zypper --non-interactive refresh
      run_retry 3 "${ROOT_PREFIX[@]}" zypper --non-interactive install \
        ca-certificates curl git make openssl tar gzip xz unzip gcc gcc-c++ glibc-devel pkg-config python3 timezone
      ;;
    pacman)
      run_retry 3 "${ROOT_PREFIX[@]}" pacman -Syu --needed --noconfirm \
        ca-certificates curl git make openssl tar gzip xz unzip base-devel pkgconf python tzdata
      ;;
    apk)
      run_retry 3 "${ROOT_PREFIX[@]}" apk add --no-cache \
        ca-certificates coreutils curl git make openssl tar gzip xz unzip build-base pkgconf python3 bash tzdata
      ;;
  esac
}

version_ge() {
  local current="${1#v}"
  local required="${2#v}"
  local current_major current_minor current_patch current_suffix
  local required_major required_minor required_patch required_suffix
  local pattern='^([0-9]+)([.]([0-9]+))?([.]([0-9]+))?([^0-9.].*)?$'

  [[ "$current" =~ $pattern ]] || return 1
  current_major="${BASH_REMATCH[1]}"
  current_minor="${BASH_REMATCH[3]:-0}"
  current_patch="${BASH_REMATCH[5]:-0}"
  current_suffix="${BASH_REMATCH[6]:-}"

  [[ "$required" =~ $pattern ]] || return 1
  required_major="${BASH_REMATCH[1]}"
  required_minor="${BASH_REMATCH[3]:-0}"
  required_patch="${BASH_REMATCH[5]:-0}"
  required_suffix="${BASH_REMATCH[6]:-}"

  if (( 10#$current_major != 10#$required_major )); then
    (( 10#$current_major > 10#$required_major ))
  elif (( 10#$current_minor != 10#$required_minor )); then
    (( 10#$current_minor > 10#$required_minor ))
  elif (( 10#$current_patch != 10#$required_patch )); then
    (( 10#$current_patch > 10#$required_patch ))
  elif [[ -n "$current_suffix" && -z "$required_suffix" ]]; then
    return 1
  else
    return 0
  fi
}

current_go_version() {
  local go_binary=""
  if [[ -x /usr/local/bin/go ]]; then
    go_binary="/usr/local/bin/go"
  elif command -v go >/dev/null 2>&1; then
    go_binary="$(command -v go)"
  else
    return 1
  fi
  "$go_binary" version 2>/dev/null | sed -n 's/.* go\([0-9][^ ]*\).*/\1/p'
}

current_node_version() {
  local node_binary=""
  if [[ -x /usr/local/bin/node ]]; then
    node_binary="/usr/local/bin/node"
  elif command -v node >/dev/null 2>&1; then
    node_binary="$(command -v node)"
  else
    return 1
  fi
  "$node_binary" --version 2>/dev/null | sed 's/^v//'
}

node_is_compatible() {
  local version="${1#v}"
  local major="${version%%.*}"
  [[ "$major" =~ ^[0-9]+$ ]] || return 1
  if [[ "$major" == "20" ]]; then
    version_ge "$version" "20.19.0"
  elif (( 10#$major >= 22 )); then
    version_ge "$version" "22.12.0"
  else
    return 1
  fi
}

current_npm_version() {
  local npm_binary=""
  if [[ -x /usr/local/bin/npm ]]; then
    npm_binary="/usr/local/bin/npm"
  elif command -v npm >/dev/null 2>&1; then
    npm_binary="$(command -v npm)"
  else
    return 1
  fi
  "$npm_binary" --version 2>/dev/null
}

npm_is_compatible() {
  # frontend/package-lock.json uses lockfileVersion 3. npm v7+ can read it;
  # older npm v6 clients can fail during npm ci with opaque dependency errors.
  version_ge "${1#v}" "7.0.0"
}

verify_sha256() {
  local file="$1"
  local expected="$2"
  local actual
  actual="$(sha256sum "$file" | awk '{print $1}')"
  [[ "$actual" == "$expected" ]] || {
    log "ERROR" "SHA-256 校验失败：$(basename -- "$file")"
    return 1
  }
}

install_go() {
  CURRENT_STEP="安装 Go 工具链"
  local installed=""
  installed="$(current_go_version || true)"
  if [[ -n "$installed" ]] && version_ge "$installed" "$REQUIRED_GO_VERSION" \
    && { [[ -x /usr/local/bin/gofmt ]] || command -v gofmt >/dev/null 2>&1; }; then
    log "INFO" "Go ${installed} 和 gofmt 已满足要求。"
    return 0
  fi

  ensure_root_access
  require_command curl
  require_command tar
  require_command sha256sum

  local temp_dir archive checksum_file checksum_url archive_url expected target
  temp_dir="$(mktemp -d)"
  TEMP_PATHS+=("$temp_dir")
  archive="${temp_dir}/go${REQUIRED_GO_VERSION}.linux-${GO_ARCH}.tar.gz"
  checksum_file="${archive}.sha256"
  archive_url="https://go.dev/dl/$(basename -- "$archive")"
  checksum_url="${archive_url}.sha256"
  log "INFO" "下载 Go ${REQUIRED_GO_VERSION} 官方发行包。"
  # 发行包和校验文件均通过 run_retry 下载，避免校验文件的瞬时网络失败
  # 绕过重试逻辑，导致发行包已下载成功但部署仍中断。
  run_retry 3 curl --fail --location --show-error --connect-timeout 20 --max-time 120 -o "$archive" "$archive_url"
  if [[ "$DRY_RUN" == true ]]; then
    expected="dry-run"
  else
    CURRENT_STEP="下载 Go 校验文件"
    run_retry 3 curl --fail --location --show-error --connect-timeout 20 --max-time 60 -o "$checksum_file" "$checksum_url"
    expected="$(tr -d '[:space:]' < "$checksum_file")"
    [[ "$expected" =~ ^[a-fA-F0-9]{64}$ ]] || {
      log "ERROR" "Go 官方校验值格式无效。"
      return 1
    }
    CURRENT_STEP="校验 Go 发行包"
    verify_sha256 "$archive" "$expected"
  fi

  target="/usr/local/lib/basic-platform/go/${REQUIRED_GO_VERSION}"
  run_cmd "${ROOT_PREFIX[@]}" rm -rf -- "$target"
  run_cmd "${ROOT_PREFIX[@]}" mkdir -p -- "$(dirname -- "$target")" "$temp_dir/extract"
  run_cmd tar -xzf "$archive" -C "$temp_dir/extract"
  run_cmd "${ROOT_PREFIX[@]}" mv "$temp_dir/extract/go" "$target"
  run_cmd "${ROOT_PREFIX[@]}" ln -sfn "$target/bin/go" /usr/local/bin/go
  run_cmd "${ROOT_PREFIX[@]}" ln -sfn "$target/bin/gofmt" /usr/local/bin/gofmt
  export PATH="/usr/local/bin:${PATH}"
  hash -r
  if [[ "$DRY_RUN" == true ]]; then
    log "INFO" "将安装 Go ${REQUIRED_GO_VERSION}。"
  else
    log "INFO" "Go ${REQUIRED_GO_VERSION} 安装完成。"
  fi
}

install_node() {
  CURRENT_STEP="安装 Node.js 工具链"
  local installed_node=""
  local installed_npm=""
  installed_node="$(current_node_version || true)"
  installed_npm="$(current_npm_version || true)"
  if [[ -n "$installed_node" ]] && node_is_compatible "$installed_node" \
    && [[ -n "$installed_npm" ]] && npm_is_compatible "$installed_npm"; then
    log "INFO" "Node.js ${installed_node} 和 npm ${installed_npm} 已满足要求。"
    return 0
  fi
  if [[ -n "$installed_node" ]] && node_is_compatible "$installed_node"; then
    log "WARN" "检测到 Node.js ${installed_node}，但 npm ${installed_npm:-未安装} 不支持当前 package-lock.json；将安装并切换到配套的 Node.js/npm 工具链。"
  fi
  node_is_compatible "$REQUIRED_NODE_VERSION" || {
    log "ERROR" "NODE_VERSION=${REQUIRED_NODE_VERSION} 不满足 Vite 要求（^20.19.0 或 >=22.12.0）。"
    return 1
  }

  ensure_root_access
  require_command curl
  require_command tar
  require_command sha256sum

  local temp_dir archive base_url checksum_file expected target
  temp_dir="$(mktemp -d)"
  TEMP_PATHS+=("$temp_dir")
  archive="node-v${REQUIRED_NODE_VERSION}-linux-${NODE_ARCH}.tar.xz"
  base_url="https://nodejs.org/dist/v${REQUIRED_NODE_VERSION}"
  checksum_file="${temp_dir}/SHASUMS256.txt"
  log "INFO" "下载 Node.js ${REQUIRED_NODE_VERSION} 官方发行包。"
  run_retry 3 curl --fail --location --show-error --connect-timeout 20 -o "${temp_dir}/${archive}" "${base_url}/${archive}"
  run_retry 3 curl --fail --location --show-error --connect-timeout 20 -o "$checksum_file" "${base_url}/SHASUMS256.txt"
  if [[ "$DRY_RUN" != true ]]; then
    expected="$(awk -v file="$archive" '$2 == file {print $1; exit}' "$checksum_file")"
    [[ "$expected" =~ ^[a-fA-F0-9]{64}$ ]] || {
      log "ERROR" "Node.js 官方校验清单中找不到 ${archive}。"
      return 1
    }
    verify_sha256 "${temp_dir}/${archive}" "$expected"
  fi

  target="/usr/local/lib/basic-platform/node/${REQUIRED_NODE_VERSION}"
  run_cmd "${ROOT_PREFIX[@]}" rm -rf -- "$target"
  run_cmd "${ROOT_PREFIX[@]}" mkdir -p -- "$(dirname -- "$target")" "$target"
  run_cmd tar -xJf "${temp_dir}/${archive}" -C "$target" --strip-components=1
  local binary
  for binary in node npm npx corepack; do
    run_cmd "${ROOT_PREFIX[@]}" ln -sfn "$target/bin/$binary" "/usr/local/bin/$binary"
  done
  export PATH="/usr/local/bin:${PATH}"
  hash -r
  if [[ "$DRY_RUN" == true ]]; then
    log "INFO" "将安装 Node.js ${REQUIRED_NODE_VERSION}。"
  else
    installed_npm="$(current_npm_version || true)"
    if [[ -z "$installed_npm" ]] || ! npm_is_compatible "$installed_npm"; then
      log "ERROR" "Node.js 安装后 npm ${installed_npm:-未安装} 仍不兼容；要求 npm >= 7。请检查 /usr/local/bin/npm 链接。"
      return 1
    fi
    log "INFO" "Node.js ${REQUIRED_NODE_VERSION} 和 npm ${installed_npm} 安装完成。"
  fi
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

mysql_is_oracle() {
  command -v mysql >/dev/null 2>&1 || return 1
  ! mysql --version 2>/dev/null | grep -qi 'mariadb'
}

mysql_is_supported() {
  local version
  mysql_is_oracle || return 1
  version="$(mysql_version || true)"
  [[ -n "$version" ]] && version_ge "$version" "8.0.0"
}

install_mysql_server() {
  if [[ "$SKIP_MYSQL_SERVER" == true ]]; then
    log "INFO" "已跳过本机 MySQL Server 安装。"
    return 0
  fi
  CURRENT_STEP="安装 MySQL Server"
  ensure_root_access
  if mysql_is_supported && command -v mysqld >/dev/null 2>&1; then
    log "INFO" "已检测到 MySQL 8.x Server，跳过安装并确认服务状态。"
    start_mysql_service
    return 0
  fi

  case "$PACKAGE_MANAGER" in
    apt)
      if [[ "$DRY_RUN" != true ]]; then
        local mysql_candidate
        mysql_candidate="$(apt-cache policy mysql-server 2>/dev/null | awk '/Candidate:/ {print $2; exit}')"
        if [[ -z "$mysql_candidate" || "$mysql_candidate" == "(none)" ]]; then
          log "ERROR" "APT 软件源未提供 mysql-server。请先配置 Oracle MySQL 官方仓库，或使用远程 MySQL 并增加 --skip-mysql-server --skip-database-init。"
          return 1
        fi
        if apt-cache depends mysql-server 2>/dev/null | grep -Eqi 'mariadb-(server|client)'; then
          log "ERROR" "APT 中的 mysql-server 将解析为 MariaDB，脚本拒绝替代安装。请配置 Oracle MySQL 官方仓库或使用远程 MySQL。"
          return 1
        fi
      fi
      run_retry 3 "${ROOT_PREFIX[@]}" env DEBIAN_FRONTEND=noninteractive apt-get install -y mysql-server mysql-client
      ;;
    dnf)
      run_retry 3 "${ROOT_PREFIX[@]}" dnf install -y mysql-server mysql
      ;;
    yum)
      run_retry 3 "${ROOT_PREFIX[@]}" yum install -y mysql-server mysql
      ;;
    *)
      log "ERROR" "${DISTRO_NAME} 的默认仓库无法保证提供 Oracle MySQL，脚本拒绝静默安装 MariaDB。请配置 MySQL 官方仓库，或使用 --skip-mysql-server --skip-database-init。"
      return 1
      ;;
  esac

  if [[ "$DRY_RUN" == true ]]; then
    log "INFO" "将安装并启动 Oracle MySQL 8.x；预演模式不检查安装后的客户端和服务。"
    return 0
  fi
  mysql_is_supported || {
    log "ERROR" "安装结果不是受支持的 Oracle MySQL 8.x；本项目不自动切换为 MariaDB 或 MySQL 5.x。"
    return 1
  }
  start_mysql_service
}

start_mysql_service() {
  CURRENT_STEP="启动 MySQL 服务"
  if ! command -v systemctl >/dev/null 2>&1 || [[ ! -d /run/systemd/system ]]; then
    log "WARN" "系统未运行 systemd，请手工启动 MySQL 服务。"
    return 0
  fi
  local service_name=""
  local service_units
  service_units="$(systemctl list-unit-files --type=service --no-legend 2>/dev/null | awk '{print $1}' || true)"
  if grep -qx 'mysql.service' <<<"$service_units"; then
    service_name="mysql"
  elif grep -qx 'mysqld.service' <<<"$service_units"; then
    service_name="mysqld"
  fi
  [[ -n "$service_name" ]] || {
    log "ERROR" "未找到 mysql.service 或 mysqld.service。"
    return 1
  }
  run_cmd "${ROOT_PREFIX[@]}" systemctl enable --now "$service_name"
}

get_env_value() {
  local key="$1"
  local source_file="$ENV_FILE"
  if [[ ! -f "$source_file" ]]; then
    source_file="$ENV_EXAMPLE"
  fi
  [[ -f "$source_file" ]] || return 1
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
  ' "$source_file"
}

set_env_value() {
  local key="$1"
  local value="$2"
  [[ "$key" =~ ^[A-Z][A-Z0-9_]*$ ]] || return 1
  [[ "$value" != *$'\n'* && "$value" != *$'\r'* ]] || return 1
  if [[ "$DRY_RUN" == true ]]; then
    log "INFO" "将补齐 ${key}（值已隐藏）。"
    return 0
  fi
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
      name=(pos > 0 ? substr(normalized,1,pos-1) : "")
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
  chmod 600 "$temp_file"
  mv -- "$temp_file" "$ENV_FILE"
}

ensure_env_secret() {
  local key="$1"
  local generator="$2"
  local current
  current="$(get_env_value "$key" || true)"
  if [[ -n "$current" ]]; then
    log "INFO" "${key} 已配置，保留现有值。"
    return 0
  fi
  local generated
  case "$generator" in
    base64) generated="$(openssl rand -base64 32 | tr -d '\n')" ;;
    hex) generated="$(openssl rand -hex 32)" ;;
    *) return 1 ;;
  esac
  set_env_value "$key" "$generated"
  if [[ "$DRY_RUN" == true ]]; then
    log "INFO" "${key} 将使用安全随机值补齐（未输出明文）。"
  else
    log "INFO" "${key} 已生成并写入环境文件（未输出明文）。"
  fi
}

resolve_env_path() {
  local value="$1"
  if [[ "$value" == /* ]]; then
    printf '%s\n' "$value"
  else
    printf '%s/%s\n' "$(dirname -- "$ENV_FILE")" "${value#./}"
  fi
}

merge_env_defaults() {
  [[ -f "$ENV_EXAMPLE" ]] || return 0
  local line normalized key default_value current_value
  while IFS= read -r line || [[ -n "$line" ]]; do
    [[ "$line" =~ ^[[:space:]]*$ || "$line" =~ ^[[:space:]]*# ]] && continue
    normalized="$line"
    normalized="${normalized#export }"
    [[ "$normalized" == *=* ]] || continue
    key="${normalized%%=*}"
    key="${key//[[:space:]]/}"
    [[ "$key" =~ ^[A-Z][A-Z0-9_]*$ ]] || continue
    default_value="${normalized#*=}"
    [[ -n "$default_value" ]] || continue
    current_value="$(get_env_value "$key" || true)"
    if [[ -z "$current_value" ]]; then
      set_env_value "$key" "$default_value"
      log "INFO" "${key} 已按 .env.example 的开发默认值补齐。"
    fi
  done <"$ENV_EXAMPLE"
}

configure_environment() {
  CURRENT_STEP="创建项目环境配置"
  require_command openssl
  if [[ ! -f "$ENV_FILE" ]]; then
    [[ -f "$ENV_EXAMPLE" ]] || {
      log "ERROR" "缺少环境模板：${ENV_EXAMPLE}"
      return 1
    }
    if [[ "$DRY_RUN" == true ]]; then
      log "INFO" "将复制 ${ENV_EXAMPLE} 到 ${ENV_FILE}。"
    else
      cp -- "$ENV_EXAMPLE" "$ENV_FILE"
      chmod 600 "$ENV_FILE"
    fi
  else
    chmod 600 "$ENV_FILE" 2>/dev/null || true
    log "INFO" "保留现有非空值，仅补齐缺失或空白的开发配置：${ENV_FILE}"
  fi

  local app_env
  app_env="$(get_env_value APP_ENV || true)"
  if [[ "$app_env" == "production" ]]; then
    log "ERROR" "脚本不会修改 production 环境配置或自动生成密钥、数据库密码，请通过受控配置与密钥系统注入。"
    return 1
  fi

  merge_env_defaults
  ensure_env_secret MYSQL_PASSWORD hex
  ensure_env_secret IAM_MOBILE_ENCRYPTION_KEY base64
  ensure_env_secret IAM_BOOTSTRAP_TOKEN hex

  CURRENT_STEP="创建本地数据目录与 JWT 密钥"
  local storage_root log_directory private_key public_key
  storage_root="$(resolve_env_path "$(get_env_value FILE_STORAGE_ROOT)")"
  log_directory="$(resolve_env_path "$(get_env_value LOG_DIRECTORY)")"
  private_key="$(resolve_env_path "$(get_env_value AUTH_JWT_PRIVATE_KEY_PATH)")"
  public_key="$(resolve_env_path "$(get_env_value AUTH_JWT_PUBLIC_KEY_PATH)")"

  run_cmd mkdir -p -- "$storage_root" "$log_directory" "$(dirname -- "$private_key")" "$(dirname -- "$public_key")"
  if [[ -f "$private_key" && -f "$public_key" ]]; then
    log "INFO" "JWT 密钥已存在，保留现有密钥。"
  elif [[ -e "$private_key" || -e "$public_key" ]]; then
    log "ERROR" "JWT 私钥和公钥必须同时存在；当前仅发现其中一个，请人工核对后重试。"
    return 1
  else
    run_cmd openssl genpkey -algorithm ED25519 -out "$private_key"
    run_cmd openssl pkey -in "$private_key" -pubout -out "$public_key"
    run_cmd chmod 600 "$private_key"
    if [[ "$DRY_RUN" == true ]]; then
      log "INFO" "将生成开发环境 Ed25519 JWT 密钥。"
    else
      log "INFO" "开发环境 Ed25519 JWT 密钥已生成。"
    fi
  fi
}

# Native production deployment preparation. These helpers intentionally create only
# the operating-system resources that can be safely derived from this project. Values
# that identify the operator's domain or database are left for the operator to supply.
RUNTIME_USER="${BASIC_PLATFORM_RUNTIME_USER:-basic-platform}"
RUNTIME_GROUP="${BASIC_PLATFORM_RUNTIME_GROUP:-basic-platform}"
PRODUCTION_ENV_TEMPLATE_CREATED=false

require_root_shell_for_native_deploy() {
  if (( EUID != 0 )); then
    log "ERROR" "原生生产部署会写入 /etc、/opt、/var/lib、/var/log 和 systemd；请使用 sudo bash scripts/bootstrap-linux.sh --deploy ... 运行。"
    return 1
  fi
}

require_safe_native_path() {
  local label="$1"
  local path="$2"
  [[ -n "$path" && "$path" == /* && "$path" != *$'\n'* && "$path" != *$'\r'* ]] || {
    log "ERROR" "${label} 必须是非空绝对路径：${path:-<空>}"
    return 1
  }
}

validate_runtime_identity() {
  local identity_pattern='^[a-z_][a-z0-9_-]*[$]?$'
  [[ "$RUNTIME_USER" =~ $identity_pattern ]] || {
    log "ERROR" "BASIC_PLATFORM_RUNTIME_USER 不是有效的 Linux 用户名：${RUNTIME_USER}"
    return 1
  }
  [[ "$RUNTIME_GROUP" =~ $identity_pattern ]] || {
    log "ERROR" "BASIC_PLATFORM_RUNTIME_GROUP 不是有效的 Linux 用户组名：${RUNTIME_GROUP}"
    return 1
  }
}

ensure_runtime_account() {
  CURRENT_STEP="创建应用运行用户"
  validate_runtime_identity
  require_command getent
  require_command id
  require_command useradd
  require_command usermod
  require_command groupadd

  if ! getent group "$RUNTIME_GROUP" >/dev/null 2>&1; then
    run_cmd groupadd --system "$RUNTIME_GROUP"
    log "INFO" "已创建运行用户组：${RUNTIME_GROUP}"
  fi

  if ! id -u "$RUNTIME_USER" >/dev/null 2>&1; then
    run_cmd useradd --system --gid "$RUNTIME_GROUP" --home-dir /var/lib/basic-platform \
      --create-home --shell /usr/sbin/nologin "$RUNTIME_USER"
    log "INFO" "已创建运行用户：${RUNTIME_USER}"
  else
    log "INFO" "运行用户已存在，保留：${RUNTIME_USER}"
  fi

  if ! id -nG "$RUNTIME_USER" | tr ' ' '\n' | grep -Fx "$RUNTIME_GROUP" >/dev/null 2>&1; then
    run_cmd usermod -a -G "$RUNTIME_GROUP" "$RUNTIME_USER"
    log "INFO" "已将运行用户加入用户组：${RUNTIME_GROUP}"
  fi
}

write_production_env_template() {
  [[ -f "$ENV_FILE" ]] && return 0

  CURRENT_STEP="创建生产环境文件"
  local env_directory template_file
  env_directory="$(dirname -- "$ENV_FILE")"
  template_file="$(mktemp)"
  TEMP_PATHS+=("$template_file")

  cat >"$template_file" <<'ENV_TEMPLATE'
# Basic Platform production configuration.
# Replace every REPLACE_WITH_* value with the value approved for this deployment.
# This file contains secrets. Keep it outside the release directory and do not commit it.
APP_ENV=production
APP_NAME=basic-platform
APP_TIMEZONE=Asia/Shanghai
APP_HTTP_ADDR=127.0.0.1:8080
APP_PUBLIC_BASE_URL=REPLACE_WITH_HTTPS_PUBLIC_BASE_URL
APP_CORS_ALLOWED_ORIGINS=REPLACE_WITH_HTTPS_PUBLIC_ORIGIN

MYSQL_HOST=REPLACE_WITH_MYSQL_HOST
MYSQL_PORT=3306
MYSQL_DATABASE=basic_platform
MYSQL_USERNAME=basic_platform
MYSQL_PASSWORD=REPLACE_WITH_DATABASE_PASSWORD
# For remote MySQL, set the fixed application-server source address or an approved MySQL host pattern.
# For local MySQL this may remain blank. Do not use '%' unless that access has been explicitly approved.
MYSQL_APPLICATION_ALLOWED_HOST=
MYSQL_PARAMS=charset=utf8mb4&parseTime=true&loc=UTC
# Database administrator credentials are deliberately not persisted in this file. For remote first-time
# database creation, provide MYSQL_ADMIN_USERNAME (default root) and MYSQL_ADMIN_PASSWORD only to the
# controlled deployment process.

AUTH_JWT_ISSUER=basic-platform
AUTH_JWT_AUDIENCE=basic-platform-console
AUTH_APPLICATION_JWT_AUDIENCE=basic-platform-integration
# Optional: leave blank to use APP_PUBLIC_BASE_URL as the OIDC issuer.
OIDC_ISSUER=
AUTH_JWT_PRIVATE_KEY_PATH=/var/lib/basic-platform/keys/jwt-ed25519-private.pem
AUTH_JWT_PUBLIC_KEY_PATH=/var/lib/basic-platform/keys/jwt-ed25519-public.pem
AUTH_SESSION_COOKIE_NAME=bp_session
AUTH_SESSION_COOKIE_SECURE=true
AUTH_SESSION_COOKIE_SAME_SITE=Lax
AUTH_SESSION_TTL=8h

# A Base64-encoded 32-byte key used to protect optional mobile numbers at rest.
IAM_MOBILE_ENCRYPTION_KEY=REPLACE_WITH_BASE64_32_BYTE_KEY
# Leave empty to keep the first-super-admin endpoint disabled. Set a random value of at
# least 32 characters only for the controlled first-super-admin initialization, then remove it.
IAM_BOOTSTRAP_TOKEN=
# Used only by the deployment script when no first super administrator exists. Supply approved values
# before the first deployment; IAM_BOOTSTRAP_ADMIN_PASSWORD is automatically cleared after success.
IAM_BOOTSTRAP_ADMIN_DISPLAY_NAME=REPLACE_WITH_INITIAL_ADMIN_DISPLAY_NAME
IAM_BOOTSTRAP_ADMIN_ACCOUNT_NAME=REPLACE_WITH_INITIAL_ADMIN_ACCOUNT_NAME
IAM_BOOTSTRAP_ADMIN_PASSWORD=REPLACE_WITH_INITIAL_ADMIN_PASSWORD

AUDIT_APPLICATION_CODE=platform
AUDIT_ENVIRONMENT_CODE=prod
FILE_STORAGE_ROOT=/var/lib/basic-platform/uploads
ASYNC_WORKER_ID=basic-platform-worker-01
ASYNC_WORKER_POLL_INTERVAL=2s
ASYNC_WORKER_STALE_LOCK_TIMEOUT=5m
LOG_LEVEL=info
LOG_FORMAT=json
LOG_DIRECTORY=/var/log/basic-platform

ENV_TEMPLATE

  run_cmd install -d -o root -g "$RUNTIME_GROUP" -m 0750 "$env_directory"
  run_cmd install -o root -g "$RUNTIME_GROUP" -m 0640 "$template_file" "$ENV_FILE"
  PRODUCTION_ENV_TEMPLATE_CREATED=true
  log "WARN" "已创建生产环境模板：${ENV_FILE}。脚本不会猜测域名、数据库地址或数据库密码；请替换所有 REPLACE_WITH_* 值后重新执行同一部署命令。IAM_BOOTSTRAP_TOKEN 默认留空；请填写 IAM_BOOTSTRAP_ADMIN_* 以供首次部署自动创建超级管理员，成功后密码会自动清除。"
}

validate_production_env_values() {
  CURRENT_STEP="校验生产环境文件"
  local app_env key value failed=false
  app_env="$(get_env_value APP_ENV || true)"
  if [[ "$app_env" != "production" ]]; then
    log "ERROR" "原生生产部署要求 APP_ENV=production，当前值：${app_env:-<空>}"
    return 1
  fi

  local required_keys=(
    APP_PUBLIC_BASE_URL APP_CORS_ALLOWED_ORIGINS
    MYSQL_HOST MYSQL_PORT MYSQL_DATABASE MYSQL_USERNAME MYSQL_PASSWORD
    AUTH_JWT_ISSUER AUTH_JWT_AUDIENCE AUTH_APPLICATION_JWT_AUDIENCE
    AUTH_JWT_PRIVATE_KEY_PATH AUTH_JWT_PUBLIC_KEY_PATH
    IAM_MOBILE_ENCRYPTION_KEY
    AUDIT_APPLICATION_CODE AUDIT_ENVIRONMENT_CODE
    FILE_STORAGE_ROOT ASYNC_WORKER_ID LOG_DIRECTORY
  )
  for key in "${required_keys[@]}"; do
    value="$(get_env_value "$key" || true)"
    if [[ -z "$value" || "$value" == REPLACE_WITH_* ]]; then
      log "ERROR" "生产环境配置 ${key} 未完成填写。"
      failed=true
    fi
  done

  # OIDC_ISSUER is optional. The backend derives it from APP_PUBLIC_BASE_URL when it is
  # absent or blank, but an old template placeholder would be treated as a literal URL.
  value="$(get_env_value OIDC_ISSUER || true)"
  if [[ "$value" == REPLACE_WITH_* ]]; then
    log "ERROR" "OIDC_ISSUER 可以留空并自动使用 APP_PUBLIC_BASE_URL；当前仍为占位符，请改为 OIDC_ISSUER= 或填写实际 HTTPS 地址。"
    failed=true
  fi

  [[ "$failed" == false ]] || return 1
}

prepare_deployment_root() {
  CURRENT_STEP="创建发布目录"
  require_command install
  run_cmd install -d -o root -g root -m 0755 "$DEPLOY_ROOT" "${DEPLOY_ROOT}/releases"
}

generate_ed25519_jwt_key_pair_with_go() {
  local private_key="$1"
  local public_key="$2"
  local generator="${PROJECT_ROOT}/scripts/generate-ed25519-jwt-key-pair.go"

  require_command go
  [[ -f "$generator" ]] || {
    log "ERROR" "未找到 Go JWT 密钥生成器：${generator}"
    return 1
  }
  run_cmd go run "$generator" "$private_key" "$public_key"
}

prepare_runtime_directories_and_jwt() {
  CURRENT_STEP="创建运行目录并生成 JWT 密钥"
  require_command install
  require_command chown
  require_command chmod
  require_command rm
  require_command mktemp

  local private_value public_value storage_value log_value
  local private_key public_key storage_root log_directory
  private_value="$(get_env_value AUTH_JWT_PRIVATE_KEY_PATH || true)"
  public_value="$(get_env_value AUTH_JWT_PUBLIC_KEY_PATH || true)"
  storage_value="$(get_env_value FILE_STORAGE_ROOT || true)"
  log_value="$(get_env_value LOG_DIRECTORY || true)"
  require_safe_native_path "AUTH_JWT_PRIVATE_KEY_PATH" "$private_value"
  require_safe_native_path "AUTH_JWT_PUBLIC_KEY_PATH" "$public_value"
  require_safe_native_path "FILE_STORAGE_ROOT" "$storage_value"
  require_safe_native_path "LOG_DIRECTORY" "$log_value"

  private_key="$private_value"
  public_key="$public_value"
  storage_root="$storage_value"
  log_directory="$log_value"

  run_cmd install -d -o "$RUNTIME_USER" -g "$RUNTIME_GROUP" -m 0750 \
    "$storage_root" "$log_directory" "$(dirname -- "$private_key")" "$(dirname -- "$public_key")"
  run_cmd chown root:"$RUNTIME_GROUP" "$ENV_FILE"
  run_cmd chmod 0640 "$ENV_FILE"

  # Dry runs log the filesystem changes above but must not inspect the results
  # of commands deliberately not executed by run_cmd.
  if [[ "$DRY_RUN" == true ]]; then
    log "INFO" "预演：将保留完整 JWT 密钥对；缺失时优先使用 OpenSSL 生成 Ed25519 密钥，不支持时改用 Go 标准库。"
    return 0
  fi

  # A failed key-generation attempt on older OpenSSL versions can leave an
  # empty staging file. An empty key cannot be valid, so remove only that safe
  # residue and keep any non-empty key for explicit operator review.
  if [[ -e "$private_key" && ! -s "$private_key" ]]; then
    log "WARN" "发现空的 JWT 私钥文件，视为失败残留并删除：${private_key}"
    run_cmd rm -f -- "$private_key"
  fi
  if [[ -e "$public_key" && ! -s "$public_key" ]]; then
    log "WARN" "发现空的 JWT 公钥文件，视为失败残留并删除：${public_key}"
    run_cmd rm -f -- "$public_key"
  fi

  if [[ -f "$private_key" && -f "$public_key" ]]; then
    log "INFO" "JWT 密钥对已存在，保留现有密钥。"
  elif [[ -e "$private_key" || -e "$public_key" ]]; then
    log "ERROR" "JWT 私钥和公钥必须同时存在；当前仅发现其中一个，请人工核对后重试。"
    return 1
  else
    local temporary_directory temporary_private_key temporary_public_key
    temporary_directory="$(mktemp -d)"
    TEMP_PATHS+=("$temporary_directory")
    temporary_private_key="${temporary_directory}/jwt-ed25519-private.pem"
    temporary_public_key="${temporary_directory}/jwt-ed25519-public.pem"

    # CentOS 7 normally carries OpenSSL 1.0.2, which does not implement
    # ED25519. Try OpenSSL first when it is installed; otherwise, or when it
    # rejects ED25519, use the project's standard-library Go generator. Both
    # paths produce the PKCS#8/PKIX PEM encoding expected by the backend.
    if command -v openssl >/dev/null 2>&1 \
      && run_cmd openssl genpkey -algorithm ED25519 -out "$temporary_private_key" \
      && run_cmd openssl pkey -in "$temporary_private_key" -pubout -out "$temporary_public_key"; then
      log "INFO" "已使用 OpenSSL 生成 Ed25519 JWT 密钥对（未输出私钥内容）。"
    else
      run_cmd rm -f -- "$temporary_private_key" "$temporary_public_key"
      if command -v openssl >/dev/null 2>&1; then
        log "WARN" "当前 OpenSSL 不支持 Ed25519；改用 Go 标准库生成兼容的 JWT 密钥对。"
      else
        log "WARN" "未检测到 OpenSSL；改用 Go 标准库生成兼容的 JWT 密钥对。"
      fi
      generate_ed25519_jwt_key_pair_with_go "$temporary_private_key" "$temporary_public_key"
    fi

    [[ -s "$temporary_private_key" && -s "$temporary_public_key" ]] || {
      log "ERROR" "JWT 密钥生成未产生完整的非空密钥对。"
      return 1
    }
    run_cmd install -o "$RUNTIME_USER" -g "$RUNTIME_GROUP" -m 0600 "$temporary_private_key" "$private_key"
    run_cmd install -o "$RUNTIME_USER" -g "$RUNTIME_GROUP" -m 0644 "$temporary_public_key" "$public_key"
  fi

  run_cmd chown "$RUNTIME_USER":"$RUNTIME_GROUP" "$private_key" "$public_key"
  run_cmd chmod 0600 "$private_key"
  run_cmd chmod 0644 "$public_key"
}

write_managed_systemd_unit() {
  local service="$1"
  local description="$2"
  local executable="$3"
  local storage_root="$4"
  local log_directory="$5"
  local unit_path template_file
  unit_path="$(service_unit_path "$service")"

  if [[ -f "$unit_path" ]] && ! grep -Fqx '# Managed by Basic Platform bootstrap-linux.sh' "$unit_path"; then
    log "WARN" "发现非脚本托管的 systemd 单元，保留不覆盖：${unit_path}"
    return 0
  fi

  template_file="$(mktemp)"
  TEMP_PATHS+=("$template_file")
  cat >"$template_file" <<UNIT_TEMPLATE
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
ProtectSystem=strict
ReadWritePaths=${storage_root} ${log_directory}
UMask=0027
LimitNOFILE=65535

[Install]
WantedBy=multi-user.target
UNIT_TEMPLATE
  run_cmd install -o root -g root -m 0644 "$template_file" "$unit_path"
  log "INFO" "已写入 systemd 单元：${unit_path}"
}

install_or_update_systemd_units() {
  CURRENT_STEP="创建 systemd 服务单元"
  require_command systemctl
  [[ -d /run/systemd/system ]] || {
    log "ERROR" "当前主机未运行 systemd，无法创建和管理 Basic Platform 服务。"
    return 1
  }

  local storage_root log_directory
  storage_root="$(get_env_value FILE_STORAGE_ROOT || true)"
  log_directory="$(get_env_value LOG_DIRECTORY || true)"
  require_safe_native_path "FILE_STORAGE_ROOT" "$storage_root"
  require_safe_native_path "LOG_DIRECTORY" "$log_directory"

  write_managed_systemd_unit "basic-platform-api.service" "Basic Platform API" "api" "$storage_root" "$log_directory"
  write_managed_systemd_unit "basic-platform-worker.service" "Basic Platform Worker" "worker" "$storage_root" "$log_directory"
  run_cmd systemctl daemon-reload
}

verify_runtime_user_access() {
  CURRENT_STEP="校验运行用户权限"
  require_command runuser
  local private_key public_key storage_root log_directory
  private_key="$(get_env_value AUTH_JWT_PRIVATE_KEY_PATH || true)"
  public_key="$(get_env_value AUTH_JWT_PUBLIC_KEY_PATH || true)"
  storage_root="$(get_env_value FILE_STORAGE_ROOT || true)"
  log_directory="$(get_env_value LOG_DIRECTORY || true)"

  run_cmd runuser -u "$RUNTIME_USER" -- test -r "$ENV_FILE"
  run_cmd runuser -u "$RUNTIME_USER" -- test -r "$private_key"
  run_cmd runuser -u "$RUNTIME_USER" -- test -r "$public_key"
  run_cmd runuser -u "$RUNTIME_USER" -- test -w "$storage_root"
  run_cmd runuser -u "$RUNTIME_USER" -- test -w "$log_directory"
  log "INFO" "运行用户 ${RUNTIME_USER} 已具备读取配置/密钥及写入运行目录的权限。"
}

prepare_native_production_deployment() {
  require_root_shell_for_native_deploy
  require_safe_native_path "环境文件路径" "$ENV_FILE"
  require_safe_native_path "发布根目录" "$DEPLOY_ROOT"
  ensure_runtime_account
  write_production_env_template
  prepare_deployment_root
  if [[ "$PRODUCTION_ENV_TEMPLATE_CREATED" == true && "$DRY_RUN" == true ]]; then
    log "INFO" "预演：将创建生产环境模板、发布/运行目录、JWT 密钥和 systemd 服务单元。"
    return 0
  fi

  # 创建运行目录、JWT 密钥和托管 systemd 单元不使用数据库、域名或应用密钥。
  # 因此即使模板尚未填写完毕，也先完成这些可安全重复执行的前置资源；真正
  # 构建、迁移和启动服务之前，deploy_application 会统一校验完整生产配置。
  prepare_runtime_directories_and_jwt
  install_or_update_systemd_units
  verify_runtime_user_access
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

run_migrations() {
  if [[ "$SKIP_MIGRATION" == true ]]; then
    log "INFO" "已跳过数据库迁移。"
    return 0
  fi
  CURRENT_STEP="执行 MySQL 迁移"
  run_cmd env ENV_FILE="$ENV_FILE" make -C "${PROJECT_ROOT}/backend" migrate
}

check_item() {
  local label="$1"
  local status="$2"
  local detail="$3"
  if [[ "$status" == "ok" ]]; then
    log "OK" "${label}：${detail}"
  elif [[ "$status" == "warn" ]]; then
    log "WARN" "${label}：${detail}"
  else
    log "MISSING" "${label}：${detail}"
    ((CHECK_FAILURES++)) || true
  fi
}

check_runtime_configuration() {
  [[ -f "$ENV_FILE" ]] || return 0
  local required_key required_value
  local required_keys=(
    APP_NAME APP_HTTP_ADDR APP_PUBLIC_BASE_URL APP_CORS_ALLOWED_ORIGINS
    MYSQL_HOST MYSQL_PORT MYSQL_DATABASE MYSQL_USERNAME MYSQL_PASSWORD
    AUTH_JWT_PRIVATE_KEY_PATH AUTH_JWT_PUBLIC_KEY_PATH
    IAM_MOBILE_ENCRYPTION_KEY
    AUDIT_APPLICATION_CODE AUDIT_ENVIRONMENT_CODE FILE_STORAGE_ROOT LOG_DIRECTORY
  )
  for required_key in "${required_keys[@]}"; do
    required_value="$(get_env_value "$required_key" || true)"
    if [[ -n "$required_value" ]]; then
      check_item "配置 ${required_key}" ok "已设置"
    else
      check_item "配置 ${required_key}" missing "为空"
    fi
  done

  local private_key_value public_key_value storage_root_value log_directory_value
  local private_key public_key storage_root log_directory
  private_key_value="$(get_env_value AUTH_JWT_PRIVATE_KEY_PATH || true)"
  public_key_value="$(get_env_value AUTH_JWT_PUBLIC_KEY_PATH || true)"
  storage_root_value="$(get_env_value FILE_STORAGE_ROOT || true)"
  log_directory_value="$(get_env_value LOG_DIRECTORY || true)"

  if [[ -n "$private_key_value" && -n "$public_key_value" ]]; then
    private_key="$(resolve_env_path "$private_key_value")"
    public_key="$(resolve_env_path "$public_key_value")"
    [[ -f "$private_key" && -f "$public_key" ]] \
      && check_item "JWT 密钥对" ok "私钥和公钥均存在" \
      || check_item "JWT 密钥对" missing "密钥文件不完整"
  fi
  if [[ -n "$storage_root_value" ]]; then
    storage_root="$(resolve_env_path "$storage_root_value")"
    [[ -d "$storage_root" && -w "$storage_root" ]] \
      && check_item "文件存储目录" ok "$storage_root" \
      || check_item "文件存储目录" missing "不存在或不可写：$storage_root"
  fi
  if [[ -n "$log_directory_value" ]]; then
    log_directory="$(resolve_env_path "$log_directory_value")"
    [[ -d "$log_directory" && -w "$log_directory" ]] \
      && check_item "日志目录" ok "$log_directory" \
      || check_item "日志目录" missing "不存在或不可写：$log_directory"
  fi
}

check_database_connection() {
  [[ -f "$ENV_FILE" ]] || return 0
  mysql_is_supported || return 0
  local host port database username password temp_dir defaults_file server_version
  host="$(get_env_value MYSQL_HOST || true)"
  port="$(get_env_value MYSQL_PORT || true)"
  database="$(get_env_value MYSQL_DATABASE || true)"
  username="$(get_env_value MYSQL_USERNAME || true)"
  password="$(get_env_value MYSQL_PASSWORD || true)"
  [[ -n "$host" && -n "$port" && -n "$database" && -n "$username" ]] || return 0
  temp_dir="$(mktemp -d)"
  TEMP_PATHS+=("$temp_dir")
  defaults_file="${temp_dir}/application.cnf"
  mysql_defaults_file "$defaults_file" "$username" "$password" "$host" "$port"
  if server_version="$(mysql --defaults-extra-file="$defaults_file" --database="$database" --batch --skip-column-names --execute='SELECT VERSION()' 2>>"$LOG_FILE")"; then
    if [[ "$server_version" == 8.* ]]; then
      check_item "MySQL 连接" ok "服务端 ${server_version}，数据库 ${database}"
    else
      check_item "MySQL 连接" missing "服务端版本 ${server_version}，要求 MySQL 8.x"
    fi
  else
    check_item "MySQL 连接" missing "连接失败；详细原因见 ${LOG_FILE}"
  fi
}

check_environment() {
  CURRENT_STEP="检测项目环境"
  CHECK_FAILURES=0
  local command_name
  for command_name in curl git make openssl tar gzip xz sha256sum; do
    if command -v "$command_name" >/dev/null 2>&1; then
      check_item "$command_name" ok "$(command -v "$command_name")"
    else
      check_item "$command_name" missing "未安装"
    fi
  done

  local go_version=""
  local go_compatible=false
  go_version="$(current_go_version || true)"
  if [[ -n "$go_version" ]] && version_ge "$go_version" "$REQUIRED_GO_VERSION"; then
    check_item "Go" ok "${go_version}"
    go_compatible=true
  else
    check_item "Go" missing "当前=${go_version:-未安装}，要求>=${REQUIRED_GO_VERSION}"
  fi

  local node_version=""
  local node_compatible=false
  node_version="$(current_node_version || true)"
  if [[ -n "$node_version" ]] && node_is_compatible "$node_version"; then
    check_item "Node.js" ok "$node_version"
    node_compatible=true
  else
    check_item "Node.js" missing "当前=${node_version:-未安装}，要求 ^20.19.0 或 >=22.12.0"
  fi
  local npm_version=""
  npm_version="$(current_npm_version || true)"
  if [[ -n "$npm_version" ]] && npm_is_compatible "$npm_version"; then
    check_item "npm" ok "$npm_version"
  elif [[ -n "$npm_version" ]]; then
    check_item "npm" missing "当前=${npm_version}，项目 package-lock.json 要求 npm >= 7"
  else
    check_item "npm" missing "未安装"
  fi

  if mysql_is_supported; then
    check_item "MySQL Client" ok "$(mysql --version | head -n 1)"
  elif command -v mysql >/dev/null 2>&1; then
    check_item "MySQL Client" missing "要求 Oracle MySQL 8.x；当前为 $(mysql --version | head -n 1)"
  elif [[ "$SKIP_MYSQL_SERVER" == true ]]; then
    check_item "MySQL Client" warn "未安装；远程数据库可由 Go Driver 直接连接"
  else
    check_item "MySQL Client" missing "未安装"
  fi

  if [[ -f "$ENV_FILE" ]]; then
    check_item ".env" ok "$ENV_FILE"
  else
    check_item ".env" missing "不存在，可使用 --configure 创建"
  fi
  check_runtime_configuration
  check_database_connection
  if [[ "$SKIP_PROJECT_DEPS" == true ]]; then
    check_item "Go Module" warn "已按参数跳过依赖完整性检测"
    check_item "前端依赖" warn "已按参数跳过依赖完整性检测"
  else
    if [[ "$go_compatible" == true ]]; then
      if (cd "${PROJECT_ROOT}/backend" && go mod verify >>"$LOG_FILE" 2>&1); then
        check_item "Go Module" ok "go mod verify 通过"
      else
        check_item "Go Module" missing "依赖缺失或校验失败；详细原因见 ${LOG_FILE}"
      fi
    fi

    if [[ ! -d "${PROJECT_ROOT}/frontend/node_modules" ]]; then
      check_item "前端依赖" missing "未安装，执行 npm ci 后生成"
    elif [[ "$node_compatible" == true ]] && command -v npm >/dev/null 2>&1; then
      if (cd "${PROJECT_ROOT}/frontend" && npm ls --depth=0 --silent >>"$LOG_FILE" 2>&1); then
        check_item "前端依赖" ok "npm 依赖树完整"
      else
        check_item "前端依赖" missing "依赖树不完整；详细原因见 ${LOG_FILE}"
      fi
    fi
  fi

  if (( CHECK_FAILURES > 0 )); then
    log "ERROR" "环境检测发现 ${CHECK_FAILURES} 项缺失。可执行：bash scripts/bootstrap-linux.sh --bootstrap --yes"
    return 2
  fi
  log "INFO" "环境检测通过。"
}

verify_environment() {
  CURRENT_STEP="验证后端 Go 代码"
  run_cmd bash -c 'cd "$1" && test -z "$(gofmt -l .)"' _ "${PROJECT_ROOT}/backend"
  run_cmd bash -c 'cd "$1" && go mod verify' _ "${PROJECT_ROOT}/backend"
  run_cmd bash -c 'cd "$1" && go vet ./...' _ "${PROJECT_ROOT}/backend"
  run_cmd bash -c 'cd "$1" && go test ./...' _ "${PROJECT_ROOT}/backend"

  CURRENT_STEP="验证前端代码"
  run_cmd bash -c 'cd "$1" && npm test' _ "${PROJECT_ROOT}/frontend"
  log "INFO" "代码验证通过；未执行前端 build。"
}

bootstrap() {
  confirm "将安装系统软件、工具链并初始化本地开发环境，是否继续？" || {
    log "INFO" "用户取消操作。"
    return 0
  }
  install_base_packages
  install_go
  install_node
  install_mysql_server
  configure_environment
  initialize_database
  install_project_dependencies
  run_migrations
  if [[ "$DRY_RUN" == true ]]; then
    log "INFO" "预演完成；未修改系统，也不对尚未安装的环境执行验证。"
    return 0
  fi
  if [[ "$SKIP_PROJECT_DEPS" == true ]]; then
    log "WARN" "已按参数跳过项目依赖安装，因此不执行依赖代码验证。"
  else
    verify_environment
  fi
  check_environment
}

source_revision() {
  if command -v git >/dev/null 2>&1; then
    git -C "$PROJECT_ROOT" rev-parse --short=12 HEAD 2>/dev/null || printf '%s' source
    return 0
  fi
  printf '%s' source
}

deployment_release_id() {
  if [[ -n "$DEPLOY_RELEASE_ID" ]]; then
    printf '%s\n' "$DEPLOY_RELEASE_ID"
    return 0
  fi

  printf '%s-%s\n' "$(date -u '+%Y%m%dT%H%M%SZ')" "$(source_revision)"
}

check_systemd_services() {
  [[ "$RESTART_SERVICES" == true ]] || return 0
  command -v systemctl >/dev/null 2>&1 || {
    log "ERROR" "--restart-services 需要 systemctl，但当前主机未找到该命令。"
    return 1
  }

  local service
  for service in basic-platform-api.service basic-platform-worker.service; do
    if ! systemctl cat "$service" >>"$LOG_FILE" 2>&1; then
      log "ERROR" "未找到 systemd 单元 ${service}；首次 --deploy 应已创建该单元，请检查 systemd 是否运行以及前置配置步骤是否成功完成。"
      return 1
    fi
  done
}

build_release_artifacts() {
  local staging_dir="$1"
  local build_timestamp="$2"
  local revision="$3"

  CURRENT_STEP="构建后端发布二进制"
  run_cmd mkdir -p "$staging_dir/bin" "$staging_dir/frontend"
  run_cmd bash -c 'cd "$1" && go build -trimpath -o "$2/bin/api" ./cmd/api' _ "${PROJECT_ROOT}/backend" "$staging_dir"
  run_cmd bash -c 'cd "$1" && go build -trimpath -o "$2/bin/worker" ./cmd/worker' _ "${PROJECT_ROOT}/backend" "$staging_dir"
  run_cmd bash -c 'cd "$1" && go build -trimpath -o "$2/bin/migrate" ./cmd/migrate' _ "${PROJECT_ROOT}/backend" "$staging_dir"
  run_cmd bash -c 'cd "$1" && go build -trimpath -o "$2/bin/bootstrap-admin" ./cmd/bootstrap-admin' _ "${PROJECT_ROOT}/backend" "$staging_dir"

  CURRENT_STEP="构建前端静态资源"
  run_cmd bash -c 'cd "$1" && npm run build && cp -a dist/. "$2/frontend/"' _ "${PROJECT_ROOT}/frontend" "$staging_dir"

  CURRENT_STEP="写入发布元数据"
  run_cmd bash -c 'printf "release_id=%s\\nbuilt_at_utc=%s\\nsource_revision=%s\\n" "$1" "$2" "$3" > "$4/release-info"' _ "$DEPLOY_RELEASE_ID" "$build_timestamp" "$revision" "$staging_dir"
  # 发布目录不包含运行密钥；显式开放可执行/遍历权限，供 systemd 运行用户读取二进制和 Nginx 读取静态资源。
  run_cmd chmod 0755 "$staging_dir/bin" "$staging_dir/bin/api" "$staging_dir/bin/worker" "$staging_dir/bin/migrate" "$staging_dir/bin/bootstrap-admin"
  run_cmd find "$staging_dir/frontend" -type d -exec chmod 0755 '{}' +
  run_cmd find "$staging_dir/frontend" -type f -exec chmod 0644 '{}' +
}

remove_expired_releases() {
  local releases_dir="$1"
  local current_target="$2"
  local release_path
  local -a release_paths=()

  while IFS= read -r release_path; do
    [[ "$release_path" == "$current_target" ]] && continue
    release_paths+=("$release_path")
  done < <(find "$releases_dir" -mindepth 1 -maxdepth 1 -type d -printf '%T@ %p\n' | sort -rn | awk '{print $2}')

  # 当前版本不在数组中；因此最多额外保留 KEEP_RELEASES-1 个历史版本。
  local keep_history=$((KEEP_RELEASES - 1))
  local index
  for ((index = keep_history; index < ${#release_paths[@]}; index++)); do
    CURRENT_STEP="清理过期发布版本"
    run_cmd rm -rf -- "${release_paths[$index]}"
  done
}

deploy_application() {
  CURRENT_STEP="确认发布前置条件"
  [[ "$SKIP_PROJECT_DEPS" == false ]] || {
    log "ERROR" "发布不允许使用 --skip-project-deps；必须校验并安装 Go、npm 项目依赖。"
    return 1
  }

  confirm "将准备原生生产运行用户、目录、配置、JWT 密钥和 systemd 单元，并构建前后端、执行数据库迁移、切换发布目录 ${DEPLOY_ROOT}，是否继续？" || {
    log "INFO" "用户取消发布。"
    return 0
  }

  prepare_native_production_deployment
  if [[ "$PRODUCTION_ENV_TEMPLATE_CREATED" == true ]]; then
    if [[ "$DRY_RUN" == true ]]; then
      log "INFO" "预演完成：首次实际执行会先创建 ${ENV_FILE}，请填写生产参数后再执行部署。"
      return 0
    fi
    log "ERROR" "已完成首次部署前置资源创建，但生产环境文件仍含待填写项；请编辑 ${ENV_FILE} 并重新执行相同的 --deploy 命令。"
    return 1
  fi
  [[ -f "$ENV_FILE" ]] || {
    log "ERROR" "发布必须使用存在的环境文件：${ENV_FILE}"
    return 1
  }
  validate_production_env_values

  # 在执行迁移前先确保本机 MySQL 数据库和应用账号存在。该操作使用
  # CREATE ... IF NOT EXISTS/ALTER USER，重复执行是幂等的；远程 MySQL
  # 则由 initialize_database 按安全策略跳过，随后由迁移步骤验证应用账号的实际连接权限。
  initialize_database

  # The release build uses go mod download and npm ci. Ensure both toolchains
  # are installed before resolving project dependencies, rather than relying
  # on an arbitrary Node.js/npm pair already present on the host.
  install_go
  install_node
  install_project_dependencies
  check_environment
  check_systemd_services
  if [[ "$SKIP_CODE_VERIFY" == true ]]; then
    log "WARN" "已跳过发布前代码验证；仅在已由受控 CI 验证的构建源上使用。"
  else
    verify_environment
  fi

  local release_id revision build_timestamp releases_dir staging_dir release_dir current_link current_target
  release_id="$(deployment_release_id)"
  DEPLOY_RELEASE_ID="$release_id"
  revision="$(source_revision)"
  build_timestamp="$(date -u '+%Y-%m-%dT%H:%M:%SZ')"
  releases_dir="${DEPLOY_ROOT}/releases"
  release_dir="${releases_dir}/${release_id}"
  current_link="${DEPLOY_ROOT}/current"

  if [[ -e "$release_dir" || -L "$release_dir" ]]; then
    log "ERROR" "发布版本目录已存在：${release_dir}；请改用新的 --release-id。"
    return 1
  fi

  if [[ "$DRY_RUN" == true ]]; then
    log "INFO" "预演：将构建版本 ${release_id} 到 ${release_dir}，执行迁移并原子更新 ${current_link}。"
    [[ "$RESTART_SERVICES" == true ]] && log "INFO" "预演：将重启 basic-platform-api.service 和 basic-platform-worker.service。"
    return 0
  fi

  CURRENT_STEP="创建发布暂存目录"
  mkdir -p "$releases_dir"
  chmod 0755 "$DEPLOY_ROOT" "$releases_dir"
  staging_dir="$(mktemp -d "${releases_dir}/.staging.XXXXXX")"
  TEMP_PATHS+=("$staging_dir")

  build_release_artifacts "$staging_dir" "$build_timestamp" "$revision"

  CURRENT_STEP="固化发布版本"
  mv "$staging_dir" "$release_dir"
  chmod 0755 "$release_dir"

  if [[ "$SKIP_MIGRATION" == true ]]; then
    log "WARN" "已按参数跳过数据库迁移，同时跳过首个超级管理员自动初始化。"
  else
    CURRENT_STEP="执行发布数据库迁移"
    run_cmd env ENV_FILE="$ENV_FILE" "$release_dir/bin/migrate"
    bootstrap_first_super_admin "${release_dir}/bin/bootstrap-admin"
  fi

  CURRENT_STEP="原子切换当前发布版本"
  ln -s "$release_dir" "${current_link}.new"
  mv -Tf "${current_link}.new" "$current_link"
  current_target="$(readlink -f "$current_link")"
  log "INFO" "已切换当前发布版本：${current_target}"

  if [[ "$RESTART_SERVICES" == true ]]; then
    CURRENT_STEP="启用并重启 API 和 Worker 服务"
    run_cmd systemctl enable basic-platform-api.service basic-platform-worker.service
    run_cmd systemctl restart basic-platform-api.service basic-platform-worker.service
    run_cmd systemctl is-active --quiet basic-platform-api.service
    run_cmd systemctl is-active --quiet basic-platform-worker.service
  else
    log "WARN" "未启用或重启 API 和 Worker；systemd 单元已指向 ${current_link}，确认后可执行：systemctl enable --now basic-platform-api.service basic-platform-worker.service"
  fi

  remove_expired_releases "$releases_dir" "$current_target"
  log "INFO" "发布完成：${release_id}。前端静态文件目录：${current_link}/frontend；后端二进制目录：${current_link}/bin。"
}

application_service_names() {
  printf '%s\n' basic-platform-api.service basic-platform-worker.service
}

service_unit_path() {
  local service="$1"
  printf '/etc/systemd/system/%s\n' "$service"
}

systemd_service_exists() {
  local service="$1"
  [[ -e "$(service_unit_path "$service")" ]] || systemctl cat "$service" >>"$LOG_FILE" 2>&1
}

stop_application_services() {
  local missing_is_error="$1"
  CURRENT_STEP="停止应用服务"
  if ! command -v systemctl >/dev/null 2>&1; then
    if [[ "$missing_is_error" == true ]]; then
      log "ERROR" "关闭应用系统需要 systemctl；当前主机未找到该命令。"
      return 1
    fi
    log "WARN" "当前主机未找到 systemctl；跳过 API 和 Worker 服务停止。"
    return 0
  fi

  local service found=false
  while IFS= read -r service; do
    if systemd_service_exists "$service"; then
      found=true
      run_cmd systemctl disable --now "$service"
      run_cmd systemctl reset-failed "$service"
      log "INFO" "已停止并禁用服务：${service}"
    else
      log "WARN" "未找到服务单元，跳过：${service}"
    fi
  done < <(application_service_names)

  if [[ "$found" == false && "$missing_is_error" == true ]]; then
    log "ERROR" "未找到 Basic Platform 的 API 或 Worker systemd 服务。"
    return 1
  fi
}

validate_uninstall_path() {
  local path="$1"
  [[ "$path" == /* ]] || {
    log "ERROR" "卸载路径必须为绝对路径：${path}"
    return 1
  }
  [[ "$path" != *'/../'* && "$path" != */.. && "$path" != *'/./'* && "$path" != */. ]] || {
    log "ERROR" "卸载路径不允许包含 . 或 .. 路径片段：${path}"
    return 1
  }
  case "$path" in
    /|/etc|/var|/var/lib|/var/log|/opt|/usr|/home|/root|/tmp|/run|"$PROJECT_ROOT")
      log "ERROR" "拒绝删除过宽或受保护的路径：${path}"
      return 1
      ;;
  esac
}

remove_uninstall_path() {
  local path="$1"
  local label="$2"
  validate_uninstall_path "$path"
  if [[ -e "$path" || -L "$path" ]]; then
    CURRENT_STEP="删除${label}"
    run_cmd rm -rf -- "$path"
    log "INFO" "已删除${label}：${path}"
  else
    log "INFO" "${label}不存在，跳过：${path}"
  fi
}

remove_application_systemd_units() {
  local service unit_path
  while IFS= read -r service; do
    unit_path="$(service_unit_path "$service")"
    if [[ -e "$unit_path" || -L "$unit_path" ]]; then
      remove_uninstall_path "$unit_path" "systemd 单元文件"
    else
      log "INFO" "systemd 单元文件不存在，跳过：${unit_path}"
    fi
  done < <(application_service_names)

  if command -v systemctl >/dev/null 2>&1; then
    CURRENT_STEP="重载 systemd 配置"
    run_cmd systemctl daemon-reload
  else
    log "WARN" "当前主机未找到 systemctl；已清理单元文件，但无法执行 daemon-reload。"
  fi
}

collect_runtime_paths_from_environment() {
  # CentOS 7 自带 Bash 4.2，不使用 Bash 4.3 才支持的 nameref（local -n）。
  RUNTIME_PATHS=()
  [[ -f "$ENV_FILE" ]] || {
    log "WARN" "环境文件不存在，无法自动识别上传目录、日志目录和 JWT 密钥文件：${ENV_FILE}"
    return 0
  }

  local key value path
  for key in FILE_STORAGE_ROOT LOG_DIRECTORY AUTH_JWT_PRIVATE_KEY_PATH AUTH_JWT_PUBLIC_KEY_PATH; do
    value="$(get_env_value "$key" || true)"
    [[ -n "$value" ]] || continue
    path="$(resolve_env_path "$value")"
    RUNTIME_PATHS+=("$path")
  done
}

purge_database_resources() {
  [[ -f "$ENV_FILE" ]] || {
    log "ERROR" "--purge-database 需要存在的环境文件，以确认要删除的数据库。"
    return 1
  }
  CURRENT_STEP="删除 MySQL 项目数据库"
  if [[ "$DRY_RUN" != true ]]; then
    require_command mysql
    mysql_is_supported || {
      log "ERROR" "mysql 客户端不是受支持的 Oracle MySQL 8.x，脚本不会删除数据库。"
      return 1
    }
  fi

  local host port database username admin_user admin_password temp_dir admin_file sql_file
  host="$(get_env_value MYSQL_HOST || true)"
  port="$(get_env_value MYSQL_PORT || true)"
  database="$(get_env_value MYSQL_DATABASE || true)"
  username="$(get_env_value MYSQL_USERNAME || true)"
  [[ "$host" =~ ^[A-Za-z0-9._:-]+$ && "$port" =~ ^[0-9]+$ ]] || {
    log "ERROR" "MYSQL_HOST 或 MYSQL_PORT 无效，拒绝删除数据库。"
    return 1
  }
  [[ "$database" =~ ^[A-Za-z0-9_]+$ ]] || {
    log "ERROR" "MYSQL_DATABASE 只允许字母、数字和下划线，拒绝删除数据库。"
    return 1
  }
  [[ "$username" =~ ^[A-Za-z0-9_.-]+$ ]] || {
    log "ERROR" "MYSQL_USERNAME 包含不安全字符，拒绝删除数据库。"
    return 1
  }

  temp_dir="$(mktemp -d)"
  TEMP_PATHS+=("$temp_dir")
  admin_file="${temp_dir}/admin.cnf"
  sql_file="${temp_dir}/uninstall.sql"
  admin_user="${MYSQL_ADMIN_USERNAME:-root}"
  admin_password="${MYSQL_ADMIN_PASSWORD:-}"

  {
    printf 'DROP DATABASE IF EXISTS `%s`;\n' "$database"
    # 初始化脚本只在本机 MySQL 上创建下列两个账号；远程数据库的账号归属无法安全推断。
    if [[ "$host" == "127.0.0.1" || "$host" == "localhost" ]]; then
      printf "DROP USER IF EXISTS '%s'@'127.0.0.1';\n" "$username"
      printf "DROP USER IF EXISTS '%s'@'localhost';\n" "$username"
      printf 'FLUSH PRIVILEGES;\n'
    fi
  } >"$sql_file"
  chmod 600 "$sql_file"

  if [[ -z "$admin_password" && ( "$host" == "127.0.0.1" || "$host" == "localhost" ) ]]; then
    log "INFO" "尝试通过本机管理员 socket 删除数据库。"
    if [[ "$DRY_RUN" == true ]]; then
      log "CMD" "sudo mysql < <受保护的卸载 SQL>"
    elif (( EUID == 0 )); then
      mysql <"$sql_file" >>"$LOG_FILE" 2>&1
    else
      sudo mysql <"$sql_file" >>"$LOG_FILE" 2>&1
    fi
  else
    [[ -n "$admin_password" ]] || {
      log "ERROR" "远程 MySQL 或非 socket 管理需要设置 MYSQL_ADMIN_PASSWORD，脚本不会删除数据库。"
      return 1
    }
    mysql_defaults_file "$admin_file" "$admin_user" "$admin_password" "$host" "$port"
    if [[ "$DRY_RUN" == true ]]; then
      log "CMD" "mysql --defaults-extra-file=<临时管理员凭据文件> < <受保护的卸载 SQL>"
    else
      mysql --defaults-extra-file="$admin_file" <"$sql_file" >>"$LOG_FILE" 2>&1
    fi
  fi
  log "INFO" "已删除 MySQL 数据库：${database}"
  if [[ "$host" == "127.0.0.1" || "$host" == "localhost" ]]; then
    log "INFO" "已删除初始化脚本创建的本地应用账号：${username}@127.0.0.1、${username}@localhost"
  else
    log "WARN" "远程 MySQL 的应用账号未自动删除；账号的 Host 范围无法由当前环境文件安全推断，请由数据库管理员按实际账号清理。"
  fi
}

validate_nginx_site_path() {
  local path="$1"
  validate_uninstall_path "$path"
  case "$path" in
    /etc/nginx/*|/usr/local/nginx/conf/*) ;;
    *)
      log "ERROR" "--nginx-site 只能删除 /etc/nginx/ 或 /usr/local/nginx/conf/ 下的站点配置：${path}"
      return 1
      ;;
  esac
}

remove_nginx_sites() {
  ((${#NGINX_SITE_PATHS[@]} > 0)) || return 0
  local site_path
  for site_path in "${NGINX_SITE_PATHS[@]}"; do
    validate_nginx_site_path "$site_path"
    remove_uninstall_path "$site_path" "Nginx 站点配置"
  done

  if ! command -v nginx >/dev/null 2>&1; then
    log "WARN" "未找到 nginx 命令；已删除指定站点文件，但未执行配置校验或重载。"
    return 0
  fi
  CURRENT_STEP="校验 Nginx 配置"
  if [[ "$DRY_RUN" == true ]]; then
    log "CMD" "nginx -t && systemctl reload nginx"
    return 0
  fi
  if nginx -t >>"$LOG_FILE" 2>&1; then
    if command -v systemctl >/dev/null 2>&1; then
      systemctl reload nginx >>"$LOG_FILE" 2>&1
      log "INFO" "Nginx 配置已校验并重载。"
    else
      log "WARN" "Nginx 配置已校验，但未找到 systemctl；请按实际管理方式重载 Nginx。"
    fi
  else
    log "WARN" "已删除指定 Nginx 站点文件，但 nginx -t 失败，未重载 Nginx；请先修复现有 Nginx 配置。"
  fi
}

remove_dedicated_system_user() {
  [[ -n "$REMOVE_SYSTEM_USER" ]] || return 0
  CURRENT_STEP="删除专用运行用户"
  if ! command -v userdel >/dev/null 2>&1; then
    log "ERROR" "未找到 userdel，无法删除专用运行用户：${REMOVE_SYSTEM_USER}"
    return 1
  fi
  if id "$REMOVE_SYSTEM_USER" >/dev/null 2>&1; then
    run_cmd userdel --remove "$REMOVE_SYSTEM_USER"
    log "INFO" "已删除专用运行用户：${REMOVE_SYSTEM_USER}"
  else
    log "INFO" "专用运行用户不存在，跳过：${REMOVE_SYSTEM_USER}"
  fi

  if command -v getent >/dev/null 2>&1 && getent group "$REMOVE_SYSTEM_USER" >/dev/null 2>&1; then
    local group_members
    group_members="$(getent group "$REMOVE_SYSTEM_USER" | awk -F: '{print $4}')"
    if [[ -z "$group_members" ]]; then
      if command -v groupdel >/dev/null 2>&1; then
        run_cmd groupdel "$REMOVE_SYSTEM_USER"
        log "INFO" "已删除同名空用户组：${REMOVE_SYSTEM_USER}"
      fi
    else
      log "WARN" "同名用户组仍包含成员，未删除：${REMOVE_SYSTEM_USER}"
    fi
  fi
}

stop_application() {
  confirm "将停止并禁用 Basic Platform 的 API、Worker 服务；不会删除发布文件、数据、数据库、环境文件或 Nginx 配置，是否继续？" || {
    log "INFO" "用户取消关闭应用系统。"
    return 0
  }
  stop_application_services true
  log "INFO" "应用服务已关闭。Nginx 仍可能提供已发布的静态页面；若需要从外部完全下线，请在确认其未被其他站点共用后，使用 --uninstall --nginx-site 删除本应用的站点配置。"
}

uninstall_application() {
  local database_scope="不删除数据库"
  [[ "$PURGE_DATABASE" == true ]] && database_scope="删除环境文件指定的 MySQL 数据库"
  confirm "将停止并禁用 API、Worker，删除发布目录 ${DEPLOY_ROOT}、应用 systemd 单元、环境文件及其配置的上传/日志/JWT 文件；${database_scope}。此操作不可恢复，是否继续？" || {
    log "INFO" "用户取消卸载。"
    return 0
  }

  # 先停止进程，再删除二进制、密钥和数据，避免正在运行的进程继续写入。
  stop_application_services false
  if [[ "$PURGE_DATABASE" == true ]]; then
    purge_database_resources
  fi

  collect_runtime_paths_from_environment
  local runtime_path
  for runtime_path in "${RUNTIME_PATHS[@]}"; do
    remove_uninstall_path "$runtime_path" "运行文件"
  done
  remove_uninstall_path "$DEPLOY_ROOT" "发布目录"

  if [[ -f "$ENV_FILE" || -L "$ENV_FILE" ]]; then
    [[ "$ENV_FILE" != "$ENV_EXAMPLE" ]] || {
      log "ERROR" "拒绝删除环境模板文件：${ENV_FILE}"
      return 1
    }
    remove_uninstall_path "$ENV_FILE" "环境文件"
  else
    log "INFO" "环境文件不存在，跳过：${ENV_FILE}"
  fi

  remove_application_systemd_units
  remove_nginx_sites
  remove_dedicated_system_user
  log "INFO" "应用卸载完成。未指定 --nginx-site 的站点配置、未指定 --remove-system-user 的运行用户，以及 Go、Node.js、MySQL、Nginx 等共享系统软件不会被删除。"
}

main() {
  parse_args "$@"
  cd "$PROJECT_ROOT"
  detect_system
  log "INFO" "项目根目录：${PROJECT_ROOT}"
  log "INFO" "执行模式：${MODE}，日志：${LOG_FILE}"

  case "$MODE" in
    check) check_environment || return $? ;;
    bootstrap) bootstrap ;;
    install-system)
      confirm "将安装系统软件和工具链，是否继续？" || return 0
      install_base_packages
      install_go
      install_node
      install_mysql_server
      ;;
    install-project) install_project_dependencies ;;
    configure) configure_environment ;;
    verify) verify_environment ;;
    deploy) deploy_application ;;
    stop) stop_application ;;
    uninstall) uninstall_application ;;
    *) log "ERROR" "未实现的模式：${MODE}"; return 2 ;;
  esac

  log "INFO" "执行完成。"
}

main "$@"
