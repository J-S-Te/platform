#!/usr/bin/env bash
# Basic Platform Linux 运维脚本。
#
# 用于自动化单机 systemd + Nginx + MySQL 部署中的日常巡检、服务控制、迁移、备份、发布和应用版本回退。
# 仅在进程内读取备份所需配置；不打印、不写入日志、不上传密码或密钥。所有会修改生产状态的操作必须显式传入 --yes。
# 仅支持 Linux + Bash + systemd 的原生部署方式，不用于 Docker Compose。

set -Eeuo pipefail
IFS=$'\n\t'
umask 077

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
DEFAULT_PROJECT_ROOT="$(cd -- "${SCRIPT_DIR}/.." && pwd -P)"
PROJECT_ROOT="${BASIC_PLATFORM_SOURCE_ROOT:-${DEFAULT_PROJECT_ROOT}}"
BOOTSTRAP_SCRIPT="${PROJECT_ROOT}/scripts/bootstrap-linux.sh"

API_SERVICE="${BASIC_PLATFORM_API_SERVICE:-basic-platform-api.service}"
WORKER_SERVICE="${BASIC_PLATFORM_WORKER_SERVICE:-basic-platform-worker.service}"
RUN_USER="${BASIC_PLATFORM_RUN_USER:-basic-platform}"
DEPLOY_ROOT="${BASIC_PLATFORM_DEPLOY_ROOT:-/opt/basic-platform}"
ENV_FILE="${ENV_FILE:-/etc/basic-platform/basic-platform.env}"
BACKUP_DIR="${BASIC_PLATFORM_BACKUP_DIR:-/var/backups/basic-platform}"
HEALTH_URL="${BASIC_PLATFORM_HEALTH_URL:-http://127.0.0.1:8080}"
LOG_LINES=100
FOLLOW_LOGS=false
ASSUME_YES=false
RELEASE_ID=""
ROLLBACK_TARGET=""
RETAIN_DAYS=""
SKIP_CODE_VERIFY=false

LOG_DIRECTORY="${BASIC_PLATFORM_OPS_LOG_DIRECTORY:-/var/log/basic-platform}"
LOG_FILE=""
CURRENT_STEP="初始化"

usage() {
    cat <<'USAGE'
用法：
  sudo bash scripts/ops-linux.sh <命令> [选项]

命令：
  doctor                  只读巡检：检查 Linux、环境文件、发布目录、服务、健康端点和 Nginx 配置
  status                  显示 API、Worker、当前发布版本和健康检查状态
  health                  检查 /healthz 与 /readyz；适合监控系统调用
  logs                    查看 systemd 日志；默认 API 和 Worker 最近 100 行
  start                   启动 API 和 Worker 服务
  stop                    停止 API 和 Worker 服务
  restart                 重启 API 和 Worker 服务，并验证就绪状态
  migrate                 使用当前发布版本执行数据库迁移；执行前自动备份数据库
  backup                  备份 MySQL 数据库和本地上传文件
  deploy                  自动备份数据库后，委托 bootstrap-linux.sh 构建、迁移、原子发布并重启服务
  rollback                回退到指定已发布版本；仅回退应用文件，不回退数据库
  reload-nginx            校验并平滑重载 Nginx
  prune-backups           按保留天数清理本脚本生成的历史备份

通用选项：
  --env-file PATH         环境文件，默认 /etc/basic-platform/basic-platform.env
  --deploy-root PATH      发布根目录，默认 /opt/basic-platform
  --backup-dir PATH       备份目录，默认 /var/backups/basic-platform
  --health-url URL        本机 API 地址，默认 http://127.0.0.1:8080
  --lines N               logs 命令显示的日志行数，默认 100
  --follow                logs 命令持续跟踪日志
  --yes                   确认会修改系统、服务、数据库或备份目录的操作
  --help                  显示本帮助

命令专用选项：
  deploy --release-id ID  指定发布版本号；仅允许字母、数字、点、下划线和短横线
  deploy --skip-code-verify
                          跳过发布前代码检查；仅限已由受控 CI 验证的产物源
  rollback --target ID    指定 releases/ 下的目标版本目录名
  prune-backups --retain-days N
                          删除 N 天前的备份；必须同时提供 --yes

示例：
  sudo bash scripts/ops-linux.sh doctor
  sudo bash scripts/ops-linux.sh health
  sudo bash scripts/ops-linux.sh backup --yes
  sudo bash scripts/ops-linux.sh deploy --release-id 20260723.1 --yes
  sudo bash scripts/ops-linux.sh rollback --target 20260722.3 --yes
  sudo bash scripts/ops-linux.sh logs --lines 200 --follow

安全边界：
  1. 本脚本自动化重复性运维操作，不能替代密钥保管、数据库恢复演练、漏洞修复和异常研判。
  2. rollback 不会自动执行数据库回退；迁移上线前必须确认新旧应用与数据库结构兼容。
  3. backup 不会复制环境文件或私钥，避免把密钥写入普通备份目录；密钥须按独立安全流程备份。
  4. Linux 区分大小写。服务名、环境变量、目录、文件和 URL 路径必须按本帮助原样使用。
  5. --backup-dir 与 FILE_STORAGE_ROOT 不能是系统根目录，也不能相同或互为父子目录。
USAGE
}

log() {
    local level="$1"
    shift
    local message="$*"
    local line
    line="[$(date '+%Y-%m-%d %H:%M:%S %z')] [${level}] ${message}"
    printf '%s\n' "$line"
    if [[ -n "$LOG_FILE" ]]; then
        printf '%s\n' "$line" >>"$LOG_FILE"
    fi
}

fatal() {
    log "ERROR" "$*"
    exit 1
}

on_error() {
    local exit_code=$?
    trap - ERR
    log "ERROR" "步骤“${CURRENT_STEP}”失败，退出码=${exit_code}。完整运维日志：${LOG_FILE:-未创建}" >&2
    exit "$exit_code"
}

trap on_error ERR

initialize_log() {
    local timestamp
    timestamp="$(date '+%Y%m%d-%H%M%S')"
    # 日志不记录密钥，但可能包含部署路径和故障细节；目录不可用时降级到仅当前用户可读的临时文件。
    if mkdir -p -- "$LOG_DIRECTORY" 2>/dev/null && chmod 0750 -- "$LOG_DIRECTORY" 2>/dev/null; then
        LOG_FILE="${LOG_DIRECTORY}/ops-${timestamp}-$$.log"
    else
        LOG_FILE="/tmp/basic-platform-ops-${timestamp}-$$.log"
        log "WARN" "无法写入 ${LOG_DIRECTORY}，已改用临时日志 ${LOG_FILE}。"
    fi
    : >"$LOG_FILE"
    chmod 0600 -- "$LOG_FILE"
}

require_linux() {
    [[ "$(uname -s)" == "Linux" ]] || fatal "本脚本仅支持 Linux，当前系统不是 Linux。"
}

require_root() {
    [[ "${EUID}" -eq 0 ]] || fatal "命令需要 root 权限，请使用：sudo bash scripts/ops-linux.sh …"
}

require_command() {
    local command_name="$1"
    command -v "$command_name" >/dev/null 2>&1 || fatal "未找到命令 ${command_name}；请先按 docs/04_后端开发/Linux环境自动安装脚本说明.md 安装依赖。"
}

validate_service_name() {
    local service_name="$1"
    [[ "$service_name" =~ ^[A-Za-z0-9_.@-]+\.service$ ]] || fatal "非法 systemd 服务名：${service_name}"
}

validate_release_id() {
    local value="$1"
    [[ "$value" =~ ^[A-Za-z0-9._-]+$ ]] || fatal "发布版本号仅允许字母、数字、点、下划线和短横线：${value}"
}

validate_absolute_path() {
    local path="$1"
    [[ "$path" == /* ]] || fatal "生产路径必须是绝对路径：${path}"
}

canonical_path() {
    readlink -m -- "$1"
}

reject_dangerous_directory() {
    local label="$1" path
    path="$(canonical_path "$2")"
    case "$path" in
        /|/bin|/boot|/dev|/etc|/home|/lib|/lib64|/opt|/proc|/root|/run|/sbin|/sys|/tmp|/usr|/var)
            fatal "${label} 不能直接指向系统目录：${path}"
            ;;
    esac
}

initialize_settings() {
    require_linux
    validate_service_name "$API_SERVICE"
    validate_service_name "$WORKER_SERVICE"
    validate_absolute_path "$DEPLOY_ROOT"
    validate_absolute_path "$ENV_FILE"
    validate_absolute_path "$BACKUP_DIR"
    reject_dangerous_directory "备份目录" "$BACKUP_DIR"
    [[ "$HEALTH_URL" =~ ^https?://[^/]+(:[0-9]+)?$ ]] || fatal "--health-url 仅接受不带路径的 HTTP/HTTPS 地址，例如 http://127.0.0.1:8080。"
    initialize_log
}

confirm() {
    local message="$1"
    if [[ "$ASSUME_YES" == true ]]; then
        return 0
    fi
    local answer
    read -r -p "${message} [y/N] " answer
    [[ "$answer" == "y" || "$answer" == "Y" ]]
}

current_release_path() {
    local current_link="${DEPLOY_ROOT}/current"
    [[ -L "$current_link" || -d "$current_link" ]] || return 1
    readlink -f -- "$current_link"
}

current_release_is_valid() {
    local release_path
    release_path="$(current_release_path)" || return 1
    [[ "$release_path" == "${DEPLOY_ROOT}/releases/"* ]] || return 1
    [[ -x "${release_path}/bin/api" ]] || return 1
    [[ -x "${release_path}/bin/worker" ]] || return 1
    [[ -x "${release_path}/bin/migrate" ]] || return 1
}

require_current_release() {
    local release_path
    release_path="$(current_release_path)" || fatal "未找到当前发布版本：${DEPLOY_ROOT}/current"
    [[ "$release_path" == "${DEPLOY_ROOT}/releases/"* ]] || fatal "当前发布版本不在 releases 目录内：${release_path}"
    [[ -x "${release_path}/bin/api" ]] || fatal "当前 API 二进制不存在或不可执行：${release_path}/bin/api"
    [[ -x "${release_path}/bin/worker" ]] || fatal "当前 Worker 二进制不存在或不可执行：${release_path}/bin/worker"
    [[ -x "${release_path}/bin/migrate" ]] || fatal "当前迁移二进制不存在或不可执行：${release_path}/bin/migrate"
    printf '%s\n' "$release_path"
}

require_env_file() {
    [[ -f "$ENV_FILE" ]] || fatal "环境文件不存在：${ENV_FILE}"
    [[ -r "$ENV_FILE" ]] || fatal "环境文件不可读：${ENV_FILE}"
}

# 与后端 internal/shared/config/dotenv.go 保持相同的简单 KEY=VALUE、可选引号解析规则。
# 不 source 环境文件，避免环境文件被当作 Shell 代码执行。
read_env_value() {
    local requested_key="$1"
    awk -v requested_key="$requested_key" '
        function trim(value) {
            sub(/^[[:space:]]+/, "", value)
            sub(/[[:space:]]+$/, "", value)
            return value
        }
        {
            line = trim($0)
            if (line == "" || substr(line, 1, 1) == "#") {
                next
            }
            if (substr(line, 1, 7) == "export ") {
                line = trim(substr(line, 8))
            }
            equals = index(line, "=")
            if (equals == 0) {
                next
            }
            key = trim(substr(line, 1, equals - 1))
            if (key != requested_key) {
                next
            }
            value = trim(substr(line, equals + 1))
            if (length(value) >= 2) {
                first = substr(value, 1, 1)
                last = substr(value, length(value), 1)
                if ((first == "\"" && last == "\"") || (first == "\047" && last == "\047")) {
                    value = substr(value, 2, length(value) - 2)
                }
            }
            print value
            exit
        }
    ' "$ENV_FILE"
}

require_nonempty_env_value() {
    local key="$1"
    local value
    value="$(read_env_value "$key")"
    [[ -n "$value" ]] || fatal "环境文件缺少非空配置 ${key}：${ENV_FILE}"
    printf '%s\n' "$value"
}

run_as_app_user() {
    require_command runuser
    runuser -u "$RUN_USER" -- env "ENV_FILE=${ENV_FILE}" "$@"
}

wait_for_ready() {
    local maximum_attempts="${1:-12}"
    local attempt=1
    while (( attempt <= maximum_attempts )); do
        if curl --fail --silent --show-error --max-time 3 "${HEALTH_URL}/readyz" >/dev/null; then
            log "INFO" "就绪检查成功：${HEALTH_URL}/readyz"
            return 0
        fi
        sleep 2
        ((attempt++))
    done
    return 1
}

check_service_state() {
    local service_name="$1"
    if systemctl is-active --quiet "$service_name"; then
        log "INFO" "服务运行中：${service_name}"
        return 0
    fi
    log "ERROR" "服务未运行：${service_name}"
    return 1
}

check_file_mode() {
    local path="$1"
    local mode
    mode="$(stat -c '%a' -- "$path")"
    if (( 10#$mode <= 640 )); then
        log "INFO" "文件权限符合最小要求：${path} (${mode})"
    else
        log "WARN" "文件权限可能过宽：${path} (${mode})，建议收紧为 0600 或 0640。"
    fi
}

command_health() {
    CURRENT_STEP="执行 HTTP 健康检查"
    require_command curl
    local failed=false
    if curl --fail --silent --show-error --max-time 3 "${HEALTH_URL}/healthz" >/dev/null; then
        log "INFO" "存活检查成功：${HEALTH_URL}/healthz"
    else
        log "ERROR" "存活检查失败：${HEALTH_URL}/healthz"
        failed=true
    fi
    if curl --fail --silent --show-error --max-time 3 "${HEALTH_URL}/readyz" >/dev/null; then
        log "INFO" "就绪检查成功：${HEALTH_URL}/readyz"
    else
        log "ERROR" "就绪检查失败：${HEALTH_URL}/readyz"
        failed=true
    fi
    [[ "$failed" == false ]]
}

command_status() {
    CURRENT_STEP="读取服务状态"
    require_command systemctl
    local release_path=""
    if release_path="$(current_release_path 2>/dev/null)"; then
        log "INFO" "当前发布版本：${release_path}"
        if [[ -f "${release_path}/release-info" ]]; then
            while IFS= read -r line; do
                log "INFO" "发布元数据：${line}"
            done <"${release_path}/release-info"
        fi
    else
        log "ERROR" "当前发布版本不存在：${DEPLOY_ROOT}/current"
    fi

    local failed=false
    check_service_state "$API_SERVICE" || failed=true
    check_service_state "$WORKER_SERVICE" || failed=true
    command_health || failed=true
    [[ "$failed" == false ]]
}

command_doctor() {
    CURRENT_STEP="执行部署巡检"
    require_command systemctl
    require_command curl
    local failed=false

    if [[ -f "$ENV_FILE" && -r "$ENV_FILE" ]]; then
        log "INFO" "环境文件可读：${ENV_FILE}"
        check_file_mode "$ENV_FILE"
    else
        log "ERROR" "环境文件不可读或不存在：${ENV_FILE}"
        failed=true
    fi

    if [[ -d "$DEPLOY_ROOT" ]]; then
        log "INFO" "发布根目录存在：${DEPLOY_ROOT}"
    else
        log "ERROR" "发布根目录不存在：${DEPLOY_ROOT}"
        failed=true
    fi

    if current_release_is_valid; then
        log "INFO" "当前发布二进制完整：$(current_release_path)"
    else
        log "ERROR" "当前发布二进制不完整；请先执行 deploy 或检查 ${DEPLOY_ROOT}/current。"
        failed=true
    fi

    if systemctl cat "$API_SERVICE" >/dev/null 2>&1; then
        log "INFO" "已找到 systemd 单元：${API_SERVICE}"
    else
        log "ERROR" "未找到 systemd 单元：${API_SERVICE}"
        failed=true
    fi
    if systemctl cat "$WORKER_SERVICE" >/dev/null 2>&1; then
        log "INFO" "已找到 systemd 单元：${WORKER_SERVICE}"
    else
        log "ERROR" "未找到 systemd 单元：${WORKER_SERVICE}"
        failed=true
    fi

    check_service_state "$API_SERVICE" || failed=true
    check_service_state "$WORKER_SERVICE" || failed=true
    command_health || failed=true

    if command -v nginx >/dev/null 2>&1; then
        if nginx -t >/dev/null 2>&1; then
            log "INFO" "Nginx 配置校验成功。"
        else
            log "ERROR" "Nginx 配置校验失败；请执行 nginx -t 查看具体错误。"
            failed=true
        fi
    else
        log "WARN" "未检测到 nginx；如果由外部网关提供 TLS/反向代理，可忽略此项。"
    fi

    if command -v df >/dev/null 2>&1; then
        df -h "$DEPLOY_ROOT" "$BACKUP_DIR" 2>/dev/null || df -h "$DEPLOY_ROOT" || true
    fi

    [[ "$failed" == false ]]
}

command_logs() {
    CURRENT_STEP="读取 systemd 日志"
    require_command journalctl
    require_command systemctl
    local services=("$API_SERVICE" "$WORKER_SERVICE")
    local arguments=(-n "$LOG_LINES" --no-pager)
    [[ "$FOLLOW_LOGS" == true ]] && arguments=(-f -n "$LOG_LINES")
    journalctl -u "${services[0]}" -u "${services[1]}" "${arguments[@]}"
}

command_service_control() {
    local action="$1"
    CURRENT_STEP="${action} 应用服务"
    require_root
    require_command systemctl
    confirm "将 ${action} ${API_SERVICE} 和 ${WORKER_SERVICE}，是否继续？" || {
        log "INFO" "操作已取消。"
        return 0
    }
    systemctl "$action" "$API_SERVICE" "$WORKER_SERVICE"
    if [[ "$action" == "start" || "$action" == "restart" ]]; then
        check_service_state "$API_SERVICE"
        check_service_state "$WORKER_SERVICE"
        if ! wait_for_ready; then
            fatal "服务已执行 ${action}，但 API 未在预期时间内就绪；请查看 logs 和 systemctl status。"
        fi
    fi
    log "INFO" "服务操作完成：${action}"
}

backup_database() {
    CURRENT_STEP="备份 MySQL 数据库"
    require_env_file
    require_command mysqldump
    require_command gzip
    require_command sha256sum
    mkdir -p -- "${BACKUP_DIR}/mysql"
    chmod 0700 -- "$BACKUP_DIR" "${BACKUP_DIR}/mysql"

    local mysql_host mysql_port mysql_database mysql_username mysql_password timestamp temporary_file backup_file
    mysql_host="$(require_nonempty_env_value MYSQL_HOST)"
    mysql_port="$(require_nonempty_env_value MYSQL_PORT)"
    mysql_database="$(require_nonempty_env_value MYSQL_DATABASE)"
    mysql_username="$(require_nonempty_env_value MYSQL_USERNAME)"
    mysql_password="$(require_nonempty_env_value MYSQL_PASSWORD)"
    timestamp="$(date -u '+%Y%m%dT%H%M%SZ')"
    temporary_file="$(mktemp "${BACKUP_DIR}/mysql/.${mysql_database}-${timestamp}.XXXXXX.sql.gz")"
    backup_file="${BACKUP_DIR}/mysql/${mysql_database}-${timestamp}.sql.gz"

    log "INFO" "开始备份 MySQL 数据库 ${mysql_database} 到 ${backup_file}。"
    # --single-transaction 为事务表提供一致性快照且不长时间锁表；DDL 仍会破坏快照，
    # 因此发布流程必须在迁移前完成备份，并避免同时运行人工 schema 变更。
    if ! MYSQL_PWD="$mysql_password" mysqldump \
        --host="$mysql_host" \
        --port="$mysql_port" \
        --user="$mysql_username" \
        --single-transaction \
        --quick \
        --routines \
        --events \
        --triggers \
        --no-tablespaces \
        --databases "$mysql_database" | gzip -9 >"$temporary_file"; then
        rm -f -- "$temporary_file"
        fatal "MySQL 备份失败；请检查数据库连通性、账号权限和磁盘空间。"
    fi
    # 临时文件与最终文件位于同一目录，mv 只在压缩流完整结束后原子发布备份名称。
    mv -- "$temporary_file" "$backup_file"
    chmod 0600 -- "$backup_file"
    sha256sum -- "$backup_file" >"${backup_file}.sha256"
    chmod 0600 -- "${backup_file}.sha256"
    log "INFO" "MySQL 备份完成：${backup_file}"
}

backup_uploads() {
    CURRENT_STEP="备份本地上传文件"
    require_env_file
    require_command tar
    require_command sha256sum
    mkdir -p -- "${BACKUP_DIR}/uploads"
    chmod 0700 -- "$BACKUP_DIR" "${BACKUP_DIR}/uploads"

    local storage_root timestamp temporary_file backup_file
    storage_root="$(require_nonempty_env_value FILE_STORAGE_ROOT)"
    [[ "$storage_root" == /* ]] || fatal "生产环境 FILE_STORAGE_ROOT 必须是绝对路径：${storage_root}"
    reject_dangerous_directory "FILE_STORAGE_ROOT" "$storage_root"
    local canonical_storage canonical_backup
    canonical_storage="$(canonical_path "$storage_root")"
    canonical_backup="$(canonical_path "$BACKUP_DIR")"
    [[ "$canonical_storage" != "$canonical_backup" && "$canonical_storage" != "$canonical_backup/"* && "$canonical_backup" != "$canonical_storage/"* ]] || \
        fatal "FILE_STORAGE_ROOT 与备份目录不能相同或互为父子目录：${canonical_storage} / ${canonical_backup}"
    [[ -d "$storage_root" ]] || fatal "上传文件目录不存在：${storage_root}"
    timestamp="$(date -u '+%Y%m%dT%H%M%SZ')"
    temporary_file="$(mktemp "${BACKUP_DIR}/uploads/.uploads-${timestamp}.XXXXXX.tar.gz")"
    backup_file="${BACKUP_DIR}/uploads/uploads-${timestamp}.tar.gz"

    log "INFO" "开始备份本地上传文件目录 ${storage_root}。"
    if ! tar --create --gzip --file="$temporary_file" --numeric-owner --directory="$(dirname -- "$storage_root")" "$(basename -- "$storage_root")"; then
        rm -f -- "$temporary_file"
        fatal "本地上传文件备份失败；请检查文件权限、磁盘空间和 tar 输出。"
    fi
    mv -- "$temporary_file" "$backup_file"
    chmod 0600 -- "$backup_file"
    sha256sum -- "$backup_file" >"${backup_file}.sha256"
    chmod 0600 -- "${backup_file}.sha256"
    log "INFO" "上传文件备份完成：${backup_file}"
}

command_backup() {
    require_root
    confirm "将创建 MySQL 和本地上传文件备份，是否继续？" || {
        log "INFO" "备份已取消。"
        return 0
    }
    backup_database
    backup_uploads
    log "INFO" "备份完成。请将 ${BACKUP_DIR} 同步到独立、安全的异机或对象存储位置。"
}

command_migrate() {
    CURRENT_STEP="执行数据库迁移"
    require_root
    require_env_file
    local release_path
    release_path="$(require_current_release)"
    confirm "将先备份数据库，再执行当前版本的数据库迁移，是否继续？" || {
        log "INFO" "迁移已取消。"
        return 0
    }
    backup_database
    log "INFO" "开始执行数据库迁移：${release_path}/bin/migrate"
    run_as_app_user "${release_path}/bin/migrate"
    log "INFO" "数据库迁移完成。"
}

command_deploy() {
    CURRENT_STEP="执行受控发布"
    require_root
    require_env_file
    [[ -f "$BOOTSTRAP_SCRIPT" ]] || fatal "未找到发布脚本：${BOOTSTRAP_SCRIPT}"
    if [[ -n "$RELEASE_ID" ]]; then
        validate_release_id "$RELEASE_ID"
    fi
    confirm "将自动备份数据库、构建前后端、执行迁移、切换发布版本并重启服务，是否继续？" || {
        log "INFO" "发布已取消。"
        return 0
    }
    # 备份与后续迁移不是一个事务，但备份先完成且带校验和，至少保证失败时存在可验证的迁移前恢复点。
    backup_database

    local deploy_arguments=(--deploy --yes --env-file "$ENV_FILE" --deploy-root "$DEPLOY_ROOT" --restart-services)
    [[ -n "$RELEASE_ID" ]] && deploy_arguments+=(--release-id "$RELEASE_ID")
    [[ "$SKIP_CODE_VERIFY" == true ]] && deploy_arguments+=(--skip-code-verify)
    log "INFO" "开始受控发布；发布脚本会运行项目依赖安装、代码校验、构建和迁移。"
    bash "$BOOTSTRAP_SCRIPT" "${deploy_arguments[@]}"
    if ! wait_for_ready; then
        fatal "发布流程已完成，但 API 就绪检查失败。脚本不会自动回退，因为数据库迁移可能与旧版本不兼容；请先研判后再执行 rollback。"
    fi
    log "INFO" "发布完成且 API 已就绪。"
}

replace_current_link() {
    local target_path="$1"
    local temporary_link="${DEPLOY_ROOT}/.current.new.$$"
    # 先构造临时符号链接再原子替换 current，systemd 不会观察到“链接短暂不存在”的中间状态。
    ln -s -- "$target_path" "$temporary_link"
    mv -Tf -- "$temporary_link" "${DEPLOY_ROOT}/current"
}

command_rollback() {
    CURRENT_STEP="回退应用发布版本"
    require_root
    require_command systemctl
    [[ -n "$ROLLBACK_TARGET" ]] || fatal "rollback 必须提供 --target <发布版本号>。"
    validate_release_id "$ROLLBACK_TARGET"

    local target_path previous_path
    target_path="$(readlink -f -- "${DEPLOY_ROOT}/releases/${ROLLBACK_TARGET}" 2>/dev/null || true)"
    [[ -n "$target_path" && -d "$target_path" ]] || fatal "目标发布版本不存在：${DEPLOY_ROOT}/releases/${ROLLBACK_TARGET}"
    [[ "$target_path" == "${DEPLOY_ROOT}/releases/"* ]] || fatal "目标发布版本越出了 releases 目录：${target_path}"
    [[ -x "${target_path}/bin/api" && -x "${target_path}/bin/worker" ]] || fatal "目标发布版本缺少可执行 API 或 Worker 二进制：${target_path}"
    previous_path="$(require_current_release)"
    [[ "$previous_path" != "$target_path" ]] || fatal "目标版本已经是当前版本：${ROLLBACK_TARGET}"

    log "WARN" "应用回退不会回退 MySQL 数据库。目标=${target_path}，当前=${previous_path}。"
    confirm "将切换 current 并重启 API、Worker；确认已评估数据库兼容性后继续？" || {
        log "INFO" "回退已取消。"
        return 0
    }

    # 先切应用并验证就绪；失败时只恢复应用指针，绝不猜测数据库逆迁移。
    replace_current_link "$target_path"
    systemctl restart "$API_SERVICE" "$WORKER_SERVICE"
    if wait_for_ready; then
        log "INFO" "应用回退成功：${ROLLBACK_TARGET}"
        return 0
    fi

    log "ERROR" "回退后的 API 未就绪，正在恢复原发布版本：${previous_path}"
    replace_current_link "$previous_path"
    systemctl restart "$API_SERVICE" "$WORKER_SERVICE"
    if wait_for_ready; then
        fatal "目标版本未能就绪，已自动恢复原版本：${previous_path}"
    fi
    fatal "目标版本和原版本均未能通过就绪检查，请立即人工介入。"
}

command_reload_nginx() {
    CURRENT_STEP="校验并重载 Nginx"
    require_root
    require_command nginx
    require_command systemctl
    confirm "将校验并平滑重载 Nginx，是否继续？" || {
        log "INFO" "Nginx 重载已取消。"
        return 0
    }
    nginx -t
    systemctl reload nginx
    log "INFO" "Nginx 已平滑重载。"
}

command_prune_backups() {
    CURRENT_STEP="清理历史备份"
    require_root
    [[ -n "$RETAIN_DAYS" && "$RETAIN_DAYS" =~ ^[1-9][0-9]*$ ]] || fatal "prune-backups 必须提供正整数 --retain-days N。"
    [[ -d "$BACKUP_DIR" ]] || fatal "备份目录不存在：${BACKUP_DIR}"
    confirm "将删除 ${BACKUP_DIR}/mysql 与 ${BACKUP_DIR}/uploads 中超过 ${RETAIN_DAYS} 天、由本脚本生成的备份及校验文件，是否继续？" || {
        log "INFO" "清理已取消。"
        return 0
    }

    local backup_subdirectory
    # 只删除脚本约定扩展名且位于两个固定子目录中的文件，避免清理人工备份或密钥。
    for backup_subdirectory in "$BACKUP_DIR/mysql" "$BACKUP_DIR/uploads"; do
        [[ -d "$backup_subdirectory" ]] || continue
        find "$backup_subdirectory" -type f \( -name '*.sql.gz' -o -name '*.sql.gz.sha256' -o -name '*.tar.gz' -o -name '*.tar.gz.sha256' \) -mtime "+${RETAIN_DAYS}" -print -delete
    done
    log "INFO" "历史备份清理完成。"
}

parse_arguments() {
    ACTION="${1:-help}"
    if [[ "$ACTION" == "-h" || "$ACTION" == "--help" ]]; then
        ACTION="help"
        shift
    elif [[ $# -gt 0 ]]; then
        shift
    fi
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --env-file)
                [[ $# -ge 2 ]] || fatal "--env-file 缺少路径。"
                ENV_FILE="$2"
                shift 2
                ;;
            --deploy-root)
                [[ $# -ge 2 ]] || fatal "--deploy-root 缺少路径。"
                DEPLOY_ROOT="$2"
                shift 2
                ;;
            --backup-dir)
                [[ $# -ge 2 ]] || fatal "--backup-dir 缺少路径。"
                BACKUP_DIR="$2"
                shift 2
                ;;
            --health-url)
                [[ $# -ge 2 ]] || fatal "--health-url 缺少 URL。"
                HEALTH_URL="${2%/}"
                shift 2
                ;;
            --lines)
                [[ $# -ge 2 && "$2" =~ ^[1-9][0-9]*$ ]] || fatal "--lines 必须是正整数。"
                LOG_LINES="$2"
                shift 2
                ;;
            --follow)
                FOLLOW_LOGS=true
                shift
                ;;
            --yes)
                ASSUME_YES=true
                shift
                ;;
            --release-id)
                [[ $# -ge 2 ]] || fatal "--release-id 缺少版本号。"
                RELEASE_ID="$2"
                shift 2
                ;;
            --target)
                [[ $# -ge 2 ]] || fatal "--target 缺少发布版本号。"
                ROLLBACK_TARGET="$2"
                shift 2
                ;;
            --retain-days)
                [[ $# -ge 2 ]] || fatal "--retain-days 缺少天数。"
                RETAIN_DAYS="$2"
                shift 2
                ;;
            --skip-code-verify)
                SKIP_CODE_VERIFY=true
                shift
                ;;
            -h|--help)
                ACTION="help"
                shift
                ;;
            *)
                fatal "未知选项：$1；请使用 --help 查看帮助。"
                ;;
        esac
    done
}

main() {
    parse_arguments "$@"
    if [[ "$ACTION" == "help" ]]; then
        usage
        return 0
    fi
    initialize_settings

    case "$ACTION" in
        doctor) command_doctor ;;
        status) command_status ;;
        health) command_health ;;
        logs) command_logs ;;
        start|stop|restart) command_service_control "$ACTION" ;;
        migrate) command_migrate ;;
        backup) command_backup ;;
        deploy) command_deploy ;;
        rollback) command_rollback ;;
        reload-nginx) command_reload_nginx ;;
        prune-backups) command_prune_backups ;;
        *)
            usage >&2
            fatal "未知命令：${ACTION}"
            ;;
    esac
}

main "$@"
