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
SKIP_VERIFY=false
ASSUME_YES=false
DRY_RUN=false
VERBOSE=false
LOG_FILE="${BASIC_PLATFORM_BACKEND_LOG:-/tmp/basic-platform-backend-$(date +%Y%m%d-%H%M%S)-$$.log}"
CURRENT_STEP="初始化"
TEMP_PATHS=()

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
  --skip-verify            跳过 gofmt、go vet 和 go test（不建议）
  --yes                    不要求交互确认
  --dry-run                仅输出将执行的操作，不修改系统
  --verbose                同时输出命令详细日志
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
  # 始终打包 Worker：即使本次不启用 Worker，已有 Worker 服务重启后也不能因
  # current 已切换而找不到二进制文件。--with-worker 只决定是否由本脚本管理该服务。
  run_cmd bash -c 'cd "$1" && go build -trimpath -o "$2/bin/worker" ./cmd/worker' _ "$BACKEND_ROOT" "$staging_dir"

  CURRENT_STEP="写入后端发布元数据"
  run_cmd bash -c 'printf "release_id=%s\nbuilt_at_utc=%s\nsource_revision=%s\n" "$1" "$2" "$3" > "$4/release-info"' _ "$RELEASE_ID" "$build_timestamp" "$revision" "$staging_dir"
  run_cmd chmod 0755 "$staging_dir/bin" "$staging_dir/bin/api" "$staging_dir/bin/migrate" "$staging_dir/bin/worker"
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
    log "WARN" "已跳过数据库迁移。"
  else
    # 必须使用刚刚构建出的候选版本执行迁移，不能在切换前误用旧 current 版本。
    run_migrations_from "${release_dir}/bin/migrate"
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
      run_current_migrations
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
