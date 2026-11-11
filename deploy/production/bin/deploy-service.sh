#!/usr/bin/env bash
set -Eeuo pipefail
rollback_skipped=false
recovery_config_failed=false
migration_state=not_started

# 每次发布只接受带 sha256 digest 的阿里云镜像。版本指针写入 .release.env，运行密钥仍留在
# 独立 .env 中，避免发布产物或历史镜像记录携带数据库、OAuth 等敏感配置。

usage() {
  echo "usage: $0 {frontend|contract|project|reimbursement|settlement} <acr-host>/<namespace>/<image>@sha256:<64-hex-digest>" >&2
  echo "       $0 platform <platform-image@sha256:...> <file-gateway-image@sha256:...> [<socket-proxy-image@sha256:...>]" >&2
  echo "       $0 data-analysis <dashboard-api@sha256:...> <aggregation-worker@sha256:...> <alert-worker@sha256:...> <production-migrate@sha256:...>" >&2
  exit 2
}

service="$1"
if [[ "$service" == "data-analysis" ]]; then
  [[ $# -eq 5 ]] || usage
  data_analysis_dashboard_image="$2"
  data_analysis_aggregation_image="$3"
  data_analysis_alert_image="$4"
  data_analysis_migrate_image="$5"
  image_ref="$data_analysis_dashboard_image"
elif [[ "$service" == "platform" ]]; then
  [[ $# -eq 3 || $# -eq 4 ]] || usage
  image_ref="$2"
  file_gateway_image_ref="$3"
  docker_socket_proxy_image_ref="${4:-}"
  platform_image_refs=("$image_ref" "$file_gateway_image_ref")
  if [[ $# -eq 4 ]]; then
    [[ -n "$docker_socket_proxy_image_ref" ]] || usage
    platform_image_refs+=("$docker_socket_proxy_image_ref")
  fi
else
  [[ $# -eq 2 ]] || usage
  image_ref="$2"
fi
case "$service" in
  frontend) image_key=FRONTEND_IMAGE ;;
  platform) image_key=PLATFORM_IMAGE; file_gateway_image_key=FILE_GATEWAY_IMAGE; docker_socket_proxy_image_key=DOCKER_SOCKET_PROXY_IMAGE ;;
  contract) image_key=CONTRACT_IMAGE ;;
  project) image_key=PROJECT_IMAGE ;;
  reimbursement) image_key=REIMBURSEMENT_IMAGE ;;
  settlement) image_key=SETTLEMENT_IMAGE ;;
  data-analysis)
    image_key=DATA_ANALYSIS_DASHBOARD_API_IMAGE
    data_analysis_image_keys=(DATA_ANALYSIS_DASHBOARD_API_IMAGE DATA_ANALYSIS_AGGREGATION_WORKER_IMAGE DATA_ANALYSIS_ALERT_WORKER_IMAGE DATA_ANALYSIS_MIGRATE_IMAGE)
    data_analysis_image_refs=("$data_analysis_dashboard_image" "$data_analysis_aggregation_image" "$data_analysis_alert_image" "$data_analysis_migrate_image")
    ;;
  *) usage ;;
esac

acr_enterprise_or_new_personal='^[a-z0-9.-]+\.cr\.aliyuncs\.com/[a-z0-9._/-]+@sha256:[a-f0-9]{64}$'
acr_legacy_personal='^registry(-vpc)?\.[a-z0-9-]+\.aliyuncs\.com/[a-z0-9._/-]+@sha256:[a-f0-9]{64}$'
offline_local_registry='^127\.0\.0\.1:[0-9]+/[a-z0-9._/-]+@sha256:[a-f0-9]{64}$'
if [[ "$service" == "data-analysis" ]]; then
  for data_analysis_image in "${data_analysis_image_refs[@]}"; do
    if [[ ! "$data_analysis_image" =~ $acr_enterprise_or_new_personal && ! "$data_analysis_image" =~ $acr_legacy_personal && ! "$data_analysis_image" =~ $offline_local_registry ]]; then
      echo "拒绝可变或格式错误的数据看板镜像引用：$data_analysis_image" >&2
      exit 2
    fi
  done
elif [[ "$service" == "platform" ]]; then
  for platform_image in "${platform_image_refs[@]}"; do
    if [[ ! "$platform_image" =~ $acr_enterprise_or_new_personal && ! "$platform_image" =~ $acr_legacy_personal && ! "$platform_image" =~ $offline_local_registry ]]; then
      echo "拒绝可变或格式错误的平台/File Gateway 镜像引用：$platform_image" >&2
      exit 2
    fi
  done
else
  if [[ ! "$image_ref" =~ $acr_enterprise_or_new_personal && ! "$image_ref" =~ $acr_legacy_personal && ! "$image_ref" =~ $offline_local_registry ]]; then
    echo "拒绝可变或格式错误的镜像引用：$image_ref" >&2
    exit 2
  fi
fi

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
deploy_dir="$(cd -- "$script_dir/.." && pwd)"
release_file="$deploy_dir/.release.env"
runtime_file="$deploy_dir/.env"
contract_runtime_file="$deploy_dir/runtime/contract.env"
contract_runtime_template="$deploy_dir/subsystem-templates/contract.env.example"
project_runtime_file="$deploy_dir/runtime/project.env"
project_runtime_template="$deploy_dir/subsystem-templates/project.env.example"
reimbursement_runtime_file="$deploy_dir/runtime/reimbursement.env"
reimbursement_runtime_template="$deploy_dir/subsystem-templates/reimbursement.env.example"
settlement_runtime_file="$deploy_dir/runtime/settlement.env"
settlement_runtime_template="$deploy_dir/subsystem-templates/settlement.env.example"
data_analysis_runtime_file="$deploy_dir/runtime/data-analysis.env"
data_analysis_runtime_template="$deploy_dir/subsystem-templates/data-analysis.env.example"
compose_file="$deploy_dir/docker-compose.yml"
transport_helper="$deploy_dir/bin/public-transport.sh"
provisioner_refresh_helper="$deploy_dir/bin/provisioner-config-refresh.sh"
profiles_dir="$deploy_dir/subsystems.d"
export CONTRACT_RUNTIME_ENV_FILE="$contract_runtime_file"
export PROJECT_RUNTIME_ENV_FILE="$project_runtime_file"
export REIMBURSEMENT_RUNTIME_ENV_FILE="$reimbursement_runtime_file"
export SETTLEMENT_RUNTIME_ENV_FILE="$settlement_runtime_file"
export DATA_ANALYSIS_RUNTIME_ENV_FILE="$data_analysis_runtime_file"

for command_name in docker curl gzip flock awk mktemp install stat df ln sha256sum wc; do
  command -v "$command_name" >/dev/null || {
    echo "缺少命令：$command_name" >&2
    exit 1
  }
done
relax_runtime_perm_check="${DEPLOY_RELAX_RUNTIME_PERM_CHECK:-false}"
# CI/CD 发布项目服务时必须确认运行凭据已经生成并完成真实启动。默认关闭，
# 以保留首次接入时由平台先暂存不可变镜像的兼容行为；项目 workflow 会显式开启。
fail_if_runtime_not_ready="${DEPLOY_FAIL_IF_RUNTIME_NOT_READY:-false}"
docker compose version >/dev/null
[[ -f "$runtime_file" ]] || { echo "缺少 $runtime_file" >&2; exit 1; }
[[ -f "$release_file" ]] || { echo "缺少 $release_file" >&2; exit 1; }
[[ -f "$compose_file" ]] || { echo "缺少 $compose_file" >&2; exit 1; }
[[ -f "$transport_helper" ]] || { echo "缺少 $transport_helper" >&2; exit 1; }
[[ -f "$provisioner_refresh_helper" ]] || { echo "缺少 $provisioner_refresh_helper" >&2; exit 1; }
[[ -d "$profiles_dir" ]] || { echo "缺少生产子系统审核清单目录：$profiles_dir" >&2; exit 1; }
compgen -G "$profiles_dir/*.yaml" >/dev/null || { echo "生产子系统审核清单目录中没有 YAML 文件" >&2; exit 1; }

# shellcheck source=public-transport.sh
source "$transport_helper"
# shellcheck source=provisioner-config-refresh.sh
source "$provisioner_refresh_helper"
public_transport_prepare "$deploy_dir"
public_transport_persist "$runtime_file" || { echo '公开传输配置无法持久化到 .env，拒绝继续发布' >&2; exit 1; }
source "$script_dir/compose-scope.sh"
source "$script_dir/project-background.sh"
# 即使 config --services 也会展开全部必需镜像变量。仅在提交发布指针前检查范围时使用已验证候选引用，父进程环境保持不变，以供回滚。
(
  export "$image_key=$image_ref"
  if [[ "$service" == platform ]]; then
    export "$file_gateway_image_key=$file_gateway_image_ref"
    if [[ -n "$docker_socket_proxy_image_ref" ]]; then
      export "$docker_socket_proxy_image_key=$docker_socket_proxy_image_ref"
    fi
  elif [[ "$service" == data-analysis ]]; then
    for index in "${!data_analysis_image_keys[@]}"; do
      export "${data_analysis_image_keys[$index]}=${data_analysis_image_refs[$index]}"
    done
  fi
  scope_require "$service" || exit 1
  scope_report
) || exit 1

# 部署资产替换后必须先成对重载控制面（install-assets.sh 会写 runtime/.control-plane-reload-required）。
# 这里在解析与网络创建之前失败，避免重建出与前一侧清单不一致的容器。
require_service_control_plane_clearance() {
  if [[ "$service" == platform ]]; then
    require_control_plane_reload_clearance platform-upgrade
  else
    require_control_plane_reload_clearance
  fi
}
require_service_control_plane_clearance || exit 1

ensure_application_network() {
  local network_name="basic-platform-production"
  if docker network inspect "$network_name" >/dev/null 2>&1; then
    return 0
  fi

  echo "创建生产共享网络：$network_name"
  docker network create \
    --driver bridge \
    --subnet 172.31.255.0/24 \
    --gateway 172.31.255.1 \
    "$network_name" >/dev/null || docker network inspect "$network_name" >/dev/null
}

# application 是合同、项目、CRM、门户等多个独立发布流程共同使用的长生命周期网络。
# 必须在 Compose 解析 external 网络前创建；已存在时绝不重建，避免单服务发布导致
# Docker 尝试删除仍有 active endpoints 的共享网络。
release_permission_error() {
  local current_user current_group owner mode
  current_user="$(id -un 2>/dev/null || printf unknown)"
  current_group="$(id -gn 2>/dev/null || printf unknown)"
  owner="$(stat -c '%U:%G' "$release_file" 2>/dev/null || printf unknown)"
  mode="$(stat -c '%a' "$release_file" 2>/dev/null || printf unknown)"
  echo "发布配置权限不足：${release_file}（当前用户=${current_user}:${current_group}，文件属主=${owner}，权限=${mode}）" >&2
  echo "请使用 root 执行：chown ${current_user}:${current_group} $release_file && chmod 600 $release_file" >&2
  exit 1
}

runtime_permission_error() {
  local target="$1" current_user current_group owner mode
  current_user="$(id -un 2>/dev/null || printf unknown)"
  current_group="$(id -gn 2>/dev/null || printf unknown)"
  owner="$(stat -c '%U:%G' "$target" 2>/dev/null || printf unknown)"
  mode="$(stat -c '%a' "$target" 2>/dev/null || printf unknown)"
  echo "运行配置权限不足：${target}（当前用户=${current_user}:${current_group}，文件属主=${owner}，权限=${mode}）" >&2
  echo "请使用 root 执行：chown ${current_user}:${current_group} $target && chmod 600 $target" >&2
  exit 1
}

# .release.env 存放不可变镜像版本指针，发布会在同目录创建临时文件并原子替换；
# 因此 CI 部署用户必须可读该文件且可写部署根目录。避免手工 root 发布后只留下模糊的 cp 权限错误。
[[ -r "$release_file" && -w "$deploy_dir" ]] || release_permission_error

ensure_runtime_file_mode_0600() {
  local target="$1" temporary
  [[ -r "$target" && -w "$deploy_dir/runtime" ]] || runtime_permission_error "$target"
  if chmod 600 "$target" 2>/dev/null; then
    return 0
  fi

  if [[ "$relax_runtime_perm_check" == "true" ]]; then
    if [[ -r "$target" ]]; then
      echo "跳过运行配置权限收紧（仅发布前端时允许）：$target" >&2
      return 0
    fi

    echo "无法读取运行配置文件：${target}；请检查权限后重试" >&2
    return 1
  fi

  temporary="$(mktemp "$deploy_dir/runtime/.runtime-permissions.XXXXXX")" || {
    echo "无法创建运行配置权限修复临时文件：${target}；请由文件属主或 root 执行部署" >&2
    exit 1
  }
  if ! install -m 600 "$target" "$temporary" 2>/dev/null || ! mv -f "$temporary" "$target"; then
    rm -f "$temporary"
    echo "无法将运行配置权限收紧为 0600：${target}；请检查文件属主、runtime 目录写权限，或使用 root 执行部署" >&2
    exit 1
  fi
}

prepare_runtime_file() {
  local target="$1" template="$2" label="$3"
  install -d -m 700 "$deploy_dir/runtime"
  if [[ ! -f "$target" ]]; then
    [[ -f "$template" ]] || {
      echo "缺少 ${target}，且没有可用于初始化的 $template" >&2
      exit 1
    }
    install -m 600 "$template" "$target"
    echo "已初始化 ${target}；${label}接入前仍需由平台写入运行凭据"
  fi
  [[ ! -L "$target" ]] || {
    echo "拒绝符号链接运行配置：$target" >&2
    exit 1
  }
  ensure_runtime_file_mode_0600 "$target"
  if [[ "$relax_runtime_perm_check" != "true" ]]; then
    [[ "$(stat -c '%a' "$target")" == "600" ]] || {
      echo "运行配置权限无法收紧为 0600：$target" >&2
      exit 1
    }
  fi
}

# SEC-N10b：凭据仅经进程环境及 0600 文件传递，不进入命令行参数或日志。
# 用途：通过 Compose 的 dotenv 规则读取运行参数，避免误读凭据的引号和转义。
# 参数：key 为 .env 中的运行参数变量名；其余环境与正式 Compose 调用一致。
# 返回值：解码后的变量值；非法变量名或配置解析失败返回非零。
dsn_env_value() {
  local key="$1" value
  [[ "$key" =~ ^[A-Z_][A-Z0-9_]*$ ]] || return 1
  # Compose 的非法插值报错可能包含整段密钥；仅在整条管道成功后发布解码值。
  if ! value="$(
    printf 'services:\n  probe:\n    image: scratch\n    environment:\n      VALUE: "${%s:-}"\n' "$key" |
      docker compose --project-directory "$deploy_dir" --project-name runtime-dsn \
        --env-file "$runtime_file" --env-file "$release_file" --file - \
        config --format json 2>/dev/null |
      python3 -c 'import json,sys
# Compose config 的可复用输出会把所有 $ 翻倍；还原后才是容器接收的值。
value = json.load(sys.stdin)["services"]["probe"]["environment"]["VALUE"]
sys.stdout.write(value.replace("$$", "$"))' 2>/dev/null
  )"; then
    echo '运行配置解析失败，拒绝读取运行参数' >&2
    return 1
  fi
  printf '%s' "$value"
}

# 用途：原子写入可经 Compose 无损解码的 DSN，保留其他配置并收紧权限。
# 参数：file 为服务运行文件；key 为 DSN 变量名；value 为已解码的 DSN。
# 返回值：成功返回零；多行输入、符号链接或文件操作失败返回非零。
write_runtime_dsn_value() {
  local file="$1" key="$2" value="$3"
  DSN_WRITE_FILE="$file" DSN_WRITE_KEY="$key" DSN_WRITE_VALUE="$value" python3 - <<'PYTHON'
import os
from pathlib import Path
import tempfile

path = Path(os.environ['DSN_WRITE_FILE'])
key, value = os.environ['DSN_WRITE_KEY'], os.environ['DSN_WRITE_VALUE']
if '\n' in value or '\r' in value or path.is_symlink():
    raise SystemExit('拒绝多行 DSN 或符号链接运行文件')
escaped = value.replace('\\', '\\\\').replace('"', '\\"').replace('$', '$$')
entry = key + '="' + escaped + '"'
lines = path.read_text().splitlines()
found = any(line.startswith(key + '=') for line in lines)
lines = [entry if line.startswith(key + '=') else line for line in lines]
if not found:
    lines.append(entry)
fd, temporary = tempfile.mkstemp(prefix='.runtime-dsn.', dir=path.parent)
try:
    with os.fdopen(fd, 'w') as output:
        output.write('\n'.join(lines) + '\n')
    os.replace(temporary, path)
finally:
    if os.path.exists(temporary):
        os.unlink(temporary)
PYTHON
}

write_service_runtime_dsns() {
  local target_name="$1" target key password dsn
  case "$target_name" in
    contract)
      target="$contract_runtime_file"
      key="MYSQL_DSN"
      password="$(dsn_env_value CONTRACT_MYSQL_PASSWORD)"
      dsn="contract:${password}@tcp(contract-mysql:3306)/contract_management?parseTime=true&charset=utf8mb4&collation=utf8mb4_unicode_ci"
      ;;
    project)
      target="$project_runtime_file"
      key="MYSQL_DSN"
      password="$(dsn_env_value PROJECT_MYSQL_PASSWORD)"
      dsn="project:${password:-PENDING_PROJECT_MYSQL_PASSWORD}@tcp(project-mysql:3306)/project_management?parseTime=true&charset=utf8mb4&collation=utf8mb4_unicode_ci"
      ;;
    reimbursement)
      target="$reimbursement_runtime_file"
      key=MYSQL_DSN
      password="$(dsn_env_value REIMBURSEMENT_MYSQL_PASSWORD)"
      dsn="reimbursement:${password:-PENDING_REIMBURSEMENT_MYSQL_PASSWORD}@tcp(reimbursement-mysql:3306)/reimbursement?parseTime=true&charset=utf8mb4&loc=UTC"
      ;;
    settlement)
      target="$settlement_runtime_file"
      key="SETTLEMENT_MYSQL_DSN"
      password="$(dsn_env_value SETTLEMENT_MYSQL_PASSWORD)"
      dsn="settlement:${password:-PENDING_SETTLEMENT_MYSQL_PASSWORD}@tcp(settlement-mysql:3306)/settlement?parseTime=true&charset=utf8mb4&loc=Asia%2FShanghai"
      ;;
    data-analysis)
      target="$data_analysis_runtime_file"
      key="DASHBOARD_MYSQL_DSN"
      password="$(dsn_env_value DASHBOARD_MYSQL_PASSWORD)"
      dsn="dashboard:${password:-PENDING_DASHBOARD_MYSQL_PASSWORD}@tcp(data-analysis-mysql:3306)/dashboard_aggregation?parseTime=true&charset=utf8mb4&collation=utf8mb4_unicode_ci"
      ;;
    *)
      return 0
      ;;
  esac
  write_runtime_dsn_value "$target" "$key" "$dsn"
}

install -d -m 700 "$deploy_dir/runtime"
mkdir -p "$deploy_dir/backups/releases"
[[ -w "$deploy_dir/backups/releases" ]] || {
  echo "发布备份目录不可写：$deploy_dir/backups/releases；请将其属主调整为当前 CI 部署用户" >&2
  exit 1
}
exec 9>"$deploy_dir/runtime/.deploy.lock"
# 锁覆盖“备份—迁移—切镜像—健康检查—回退”完整窗口，防止并发发布互相覆盖 .release.env。
flock -w 900 9 || {
  echo "等待其他发布任务超时" >&2
  exit 1
}
require_service_control_plane_clearance || exit 1
# 运行配置读取、DSN 原子改写和后续 .release.env 提交必须位于同一个锁窗口。
# 否则等待锁期间 Agent 可能刚下发新凭据，发布脚本却用锁外读取的旧文件覆盖它。
[[ -f "$script_dir/backup-all.sh" && ! -L "$script_dir/backup-all.sh" ]] || { echo '缺少安全完整备份入口，拒绝发布' >&2; exit 1; }
# 首次安装尚未完成且平台阶段从未发布时，不存在可回滚的既有发布，也不承载业务数据；
# 此时要求完整灾难备份会让从未启动的 File Gateway 数据库阻断续跑。
# 判断工具缺失或输出异常时一律按“必须备份”处理，不放宽既有部署。
release_backup_policy=backup
if [[ -f "$script_dir/release-backup-policy.py" && ! -L "$script_dir/release-backup-policy.py" ]]; then
  if ! release_backup_policy="$(python3 "$script_dir/release-backup-policy.py" \
      --state-dir "${deploy_dir}.installation-state" \
      --session-tool "$script_dir/deployment-session.py" 2>/dev/null)"; then
    release_backup_policy=backup
  fi
fi
if [[ "$release_backup_policy" == skip ]]; then
  echo '[信息] 首次安装尚未完成且平台阶段从未发布，没有可回滚的既有发布；跳过发布前完整备份（平台库迁移前备份仍保留）' >&2
elif ! bash "$script_dir/backup-all.sh" --if-existing --deployment-lock-fd 9; then
  echo '发布前完整备份失败，未修改运行配置或镜像版本指针；拒绝继续更新' >&2
  exit 1
fi
case "$service" in
  contract)
    prepare_runtime_file "$contract_runtime_file" "$contract_runtime_template" "合同服务"
    write_service_runtime_dsns contract
    ;;
  project)
    prepare_runtime_file "$project_runtime_file" "$project_runtime_template" "项目管理服务"
    write_service_runtime_dsns project
    ;;
  reimbursement)
    prepare_runtime_file "$reimbursement_runtime_file" "$reimbursement_runtime_template" "报销服务"
    write_service_runtime_dsns reimbursement
    ;;
  settlement)
    prepare_runtime_file "$settlement_runtime_file" "$settlement_runtime_template" "结算服务"
    write_service_runtime_dsns settlement
    ;;
  data-analysis)
    prepare_runtime_file "$data_analysis_runtime_file" "$data_analysis_runtime_template" "数据看板服务"
    write_service_runtime_dsns data-analysis
    ;;
esac
# 传输协调器持有同一把锁，将切换状态持久化到运行文件。等待获得锁后重新读取，防止仅服务部署因旧变量而误丢弃排空覆盖配置。
public_transport_prepare "$deploy_dir"
public_transport_persist "$runtime_file" || { echo '公开传输配置无法持久化到 .env，拒绝继续发布' >&2; exit 1; }
ensure_application_network

compose() {
  local command=(docker compose \
    --project-directory "$deploy_dir" \
    --file "$compose_file" \
    --env-file "$runtime_file" \
    --env-file "$release_file")
  public_transport_compose_args command
  "${command[@]}" "$@"
}

frontend_compose() {
  compose "$@"
}

ensure_temporal_application_network() {
  local container attached
  container="$(compose ps -a -q temporal)" || return
  [[ -n "$container" ]] || return 0
  attached="$(docker inspect "$container" --format '{{if index .NetworkSettings.Networks "basic-platform-production"}}attached{{end}}')" || return
  [[ "$attached" != attached ]] || return 0
  echo 'Temporal 未连接生产共享网络；保留持久化数据并重建网络连接'
  # 漂移容器可能已无法解析 contract-mysql。先恢复共享数据库，再仅重建 Temporal，不要每次业务服务发布都惯例重启工作流服务器。
  compose up -d --wait --wait-timeout 240 contract-mysql || return
  compose up -d --force-recreate --no-deps --wait --wait-timeout 240 temporal
}

env_value() {
  local key="$1"
  awk -F= -v key="$key" '
    $0 !~ /^[[:space:]]*#/ && $1 == key {
      sub(/^[^=]*=/, "")
      print
      exit
    }
  ' "$runtime_file"
}

env_get() {
  awk -F= -v key="$2" '$0 !~ /^[[:space:]]*#/ && $1 == key {sub(/^[^=]*=/, ""); print; exit}' "$1"
}

# deploy-service.sh 由 deploy.sh 以独立子进程调用，不继承 deploy.sh 里的 shell 函数。
# 首次发布时 provision_iam_import_client 要把一次性客户端 secret 写回 .env；这里提供
# 与 deploy.sh 同语义的原子写入，不能用“脚本能通过语法检查”代替这条真实初始化路径。
env_set() {
  local file="$1" key="$2" value="$3" temporary
  [[ "$value" != *$'\n'* ]] || {
    echo "拒绝为 $key 写入包含换行的值" >&2
    return 1
  }
  temporary="$(mktemp "$deploy_dir/.env-update.XXXXXX")" || return 1
  # 经环境变量把值交给 awk，避免 -v 对反斜杠/百分号等字符做转义。
  if ! ENV_WRITE_KEY="$key" ENV_WRITE_VALUE="$value" awk -F= '
    BEGIN {
      key = ENVIRON["ENV_WRITE_KEY"]
      value = ENVIRON["ENV_WRITE_VALUE"]
      found = 0
    }
    $0 !~ /^[[:space:]]*#/ && $1 == key { print key "=" value; found = 1; next }
    { print }
    END { if (!found) print key "=" value }
  ' "$file" >"$temporary"; then
    rm -f -- "$temporary"
    return 1
  fi
  chmod 600 "$temporary" || { rm -f -- "$temporary"; return 1; }
  mv -f -- "$temporary" "$file" || { rm -f -- "$temporary"; return 1; }
  chmod 600 "$file"
}

contract_env_value() {
  local key="$1"
  awk -F= -v key="$key" '
    $0 !~ /^[[:space:]]*#/ && $1 == key {
      sub(/^[^=]*=/, "")
      print
      exit
    }
  ' "$contract_runtime_file"
}

project_env_value() {
  local key="$1"
  awk -F= -v key="$key" '
    $0 !~ /^[[:space:]]*#/ && $1 == key {
      sub(/^[^=]*=/, "")
      print
      exit
    }
  ' "$project_runtime_file"
}

settlement_env_value() {
  local key="$1"
  awk -F= -v key="$key" '
    $0 !~ /^[[:space:]]*#/ && $1 == key {
      sub(/^[^=]*=/, "")
      print
      exit
    }
  ' "$settlement_runtime_file"
}

data_analysis_env_value() {
  local key="$1"
  awk -F= -v key="$key" '
    $0 !~ /^[[:space:]]*#/ && $1 == key {
      sub(/^[^=]*=/, "")
      print
      exit
    }
  ' "$data_analysis_runtime_file"
}

require_project_runtime_value() {
  local key="$1"
  local value
  value="$(project_env_value "$key")"
  if [[ -z "$value" || "$value" == REPLACE_WITH_* || "$value" == PENDING_* ]]; then
    echo "项目管理运行配置缺失或仍为占位值：$key" >&2
    return 1
  fi
}

project_runtime_ready() {
  local key value
  for key in     OIDC_CLIENT_ID     OIDC_CLIENT_SECRET     OIDC_TENANT_ID     OIDC_SESSION_ENCRYPTION_KEY_BASE64     PLATFORM_AUTHORIZATION_CATALOG_APPLICATION_ID     PLATFORM_AUTHORIZATION_CATALOG_CLIENT_ID     PLATFORM_AUTHORIZATION_CATALOG_CLIENT_SECRET; do
    value="$(project_env_value "$key")"
    if [[ -z "$value" || "$value" == REPLACE_WITH_* || "$value" == PENDING_* ]]; then
      return 1
    fi
  done
  [[ "$(project_env_value PLATFORM_AUTHORIZATION_CATALOG_SYNC_ENABLED)" == "true" ]]
}

settlement_runtime_ready() {
  local key value
  for key in \
    OIDC_CLIENT_ID \
    OIDC_CLIENT_SECRET \
    OIDC_TENANT_ID \
    OIDC_SESSION_ENCRYPTION_KEY_BASE64 \
    PLATFORM_AUTHORIZATION_CATALOG_APPLICATION_ID \
    PLATFORM_AUTHORIZATION_CATALOG_CLIENT_ID \
    PLATFORM_AUTHORIZATION_CATALOG_CLIENT_SECRET; do
    value="$(settlement_env_value "$key")"
    if [[ -z "$value" || "$value" == REPLACE_WITH_* || "$value" == PENDING_* ]]; then
      return 1
    fi
  done
  [[ "$(settlement_env_value PLATFORM_AUTHORIZATION_CATALOG_SYNC_ENABLED)" == "true" ]] || return 1
  require_runtime_value SETTLEMENT_MYSQL_PASSWORD || return 1
  require_runtime_value SETTLEMENT_MYSQL_ROOT_PASSWORD
}

# 用途：核对数析身份、授权和已选能力所需的运行材料。
# 参数：无；读取当前受控运行文件。
# 返回值：材料完整返回零，未配置或非法能力返回非零。
data_analysis_runtime_ready() {
  local key value
  for key in \
    OIDC_CLIENT_ID \
    OIDC_CLIENT_SECRET \
    OIDC_TENANT_ID \
    OIDC_CODEC_KEY \
    PLATFORM_AUTHORIZATION_CATALOG_APPLICATION_ID \
    PLATFORM_AUTHORIZATION_CATALOG_CLIENT_ID \
    PLATFORM_AUTHORIZATION_CATALOG_CLIENT_SECRET \
    PLATFORM_AUDIT_CLIENT_ID \
    PLATFORM_AUDIT_CLIENT_SECRET; do
    value="$(data_analysis_env_value "$key")"
    if [[ -z "$value" || "$value" == REPLACE_WITH_* || "$value" == PENDING_* ]]; then
      return 1
    fi
  done
  [[ "$(data_analysis_env_value PLATFORM_AUTHORIZATION_CATALOG_SYNC_ENABLED)" == "true" ]] || return 1
  scope_runtime_services data-analysis >/dev/null || return 1
  require_runtime_value DASHBOARD_MYSQL_PASSWORD || return 1
  require_runtime_value DASHBOARD_MYSQL_ROOT_PASSWORD
}

require_runtime_value() {
  local key="$1"
  local value
  value="$(env_value "$key")"
  if [[ -z "$value" || "$value" == REPLACE_WITH_* || "$value" == PENDING_* ]]; then
    echo "运行配置缺失或仍为占位值：$key" >&2
    return 1
  fi
}

require_contract_runtime_value() {
  local key="$1"
  local value
  value="$(contract_env_value "$key")"
  if [[ -z "$value" || "$value" == REPLACE_WITH_* || "$value" == PENDING_* ]]; then
    echo "合同运行配置缺失或仍为占位值：$key" >&2
    return 1
  fi
}

contract_runtime_ready() {
  local key value
  for key in \
    OIDC_CLIENT_ID \
    OIDC_CLIENT_SECRET \
    OIDC_TENANT_ID \
    OIDC_SESSION_ENCRYPTION_KEY_BASE64 \
    PLATFORM_AUTHORIZATION_CATALOG_APPLICATION_ID \
    PLATFORM_AUTHORIZATION_CATALOG_CLIENT_ID \
    PLATFORM_AUTHORIZATION_CATALOG_CLIENT_SECRET \
    PLATFORM_AUDIT_CLIENT_ID \
    PLATFORM_AUDIT_CLIENT_SECRET; do
    value="$(contract_env_value "$key")"
    if [[ -z "$value" || "$value" == REPLACE_WITH_* || "$value" == PENDING_* ]]; then
      return 1
    fi
  done
  if [[ "$(contract_env_value CRM_REFERENCE_ENABLED)" == "true" ]]; then
    for key in CRM_REFERENCE_BASE_URL CRM_REFERENCE_TOKEN_URL CRM_REFERENCE_CLIENT_ID CRM_REFERENCE_CLIENT_SECRET; do
      value="$(contract_env_value "$key")"
      if [[ -z "$value" || "$value" == REPLACE_WITH_* || "$value" == PENDING_* ]]; then
        return 1
      fi
    done
  fi
  if [[ "$(contract_env_value PROJECT_INTEGRATION_ENABLED)" == "true" ]]; then
    for key in PROJECT_API_BASE_URL PROJECT_INTEGRATION_TOKEN_URL PROJECT_INTEGRATION_CLIENT_ID PROJECT_INTEGRATION_CLIENT_SECRET; do
      value="$(contract_env_value "$key")"
      if [[ -z "$value" || "$value" == REPLACE_WITH_* || "$value" == PENDING_* ]]; then
        return 1
      fi
    done
  fi
  [[ "$(contract_env_value PLATFORM_AUTHORIZATION_CATALOG_SYNC_ENABLED)" == "true" ]]
}

port_value() {
  local key="$1"
  local fallback="$2"
  local value
  value="$(env_value "$key")"
  if [[ "$value" =~ ^[0-9]+$ ]] && ((value >= 1 && value <= 65535)); then
    echo "$value"
  else
    echo "$fallback"
  fi
}

wait_for_health() {
  local url="$1"
  local attempts=60
  echo "等待健康检查：$url"
  for ((i = 1; i <= attempts; i++)); do
    if curl --fail --silent --show-error --max-time 3 "$url" >/dev/null 2>&1; then
      echo "健康检查通过"
      return 0
    fi
    sleep 2
  done
  echo "健康检查超时：$url" >&2
  return 1
}

# wait_container_healthy 等待运行中容器转回 healthy。Docker healthcheck 持续执行，
# unhealthy 不是终态；服务器编排里部分服务的健康窗口较窄，compose 依赖等待可能
# 在应用尚未完成启动时就误判失败，此时用本函数兜底等待而不是立即回滚。
wait_container_healthy() {
  local service_name="$1"
  local attempts="${2:-60}" container health
  echo "兜底等待容器转 healthy：$service_name"
  for ((i = 1; i <= attempts; i++)); do
    container="$(compose ps -q "$service_name" 2>/dev/null || true)"
    if [[ -n "$container" ]]; then
      health="$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' "$container" 2>/dev/null || true)"
      if [[ "$health" == "healthy" ]]; then
        echo "$service_name 已转 healthy"
        return 0
      fi
    fi
    sleep 2
  done
  echo "$service_name 在兜底等待窗口内未达到 healthy" >&2
  compose logs --no-color --tail 120 "$service_name" >&2 || true
  return 1
}

verify_service_image() {
  local compose_kind="$1" service_name="$2" expected_image="$3" container_id actual_image
  if [[ "$compose_kind" == "frontend" ]]; then
    container_id="$(frontend_compose ps -q "$service_name" 2>/dev/null || true)"
  else
    container_id="$(compose ps -q "$service_name" 2>/dev/null || true)"
  fi
  [[ -n "$container_id" ]] || {
    echo "未找到已启动的 $service_name 容器，无法确认镜像是否生效" >&2
    return 1
  }
  actual_image="$(docker inspect "$container_id" --format '{{.Config.Image}}' 2>/dev/null || true)"
  [[ "$actual_image" == "$expected_image" ]] || {
    echo "$service_name 镜像未生效：期望=${expected_image}，实际=$actual_image" >&2
    return 1
  }
  echo "$service_name 已运行目标不可变镜像：$expected_image"
}

verify_service_stable() {
  local service_name="$1" window="${2:-${DEPLOY_STABILITY_WINDOW_SECONDS:-10}}"
  local container_id before after
  [[ "$window" =~ ^[0-9]+$ ]] || {
    echo "稳定性观察时长必须是非负整数秒：$window" >&2
    return 1
  }
  container_id="$(compose ps -q "$service_name" 2>/dev/null || true)"
  [[ -n "$container_id" ]] || {
    echo "未找到 $service_name 容器，无法执行稳定性门禁" >&2
    return 1
  }
  before="$(docker inspect -f '{{.State.Status}} {{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}} {{.RestartCount}}' "$container_id" 2>/dev/null || true)"
  [[ "$before" =~ ^running\ healthy\ ([0-9]+)$ ]] || {
    echo "$service_name 未达到 running/healthy：${before:-missing}" >&2
    return 1
  }
  ((window == 0)) || sleep "$window"
  after="$(docker inspect -f '{{.State.Status}} {{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}} {{.RestartCount}}' "$container_id" 2>/dev/null || true)"
  [[ "$after" == "$before" ]] || {
    echo "$service_name 在 ${window}s 稳定性窗口内状态或重启次数发生变化：before=[$before] after=[${after:-missing}]" >&2
    compose logs --no-color --tail 120 "$service_name" >&2 || true
    return 1
  }
  echo "${service_name} 已通过 ${window}s 存活/健康稳定性门禁（${after}）"
}

dump_subsystem_provisioner_debug() {
  local container_id
  container_id="$(compose ps -q subsystem-provisioner 2>/dev/null || true)"
  if [[ -n "$container_id" ]]; then
    echo "---- subsystem-provisioner 容器状态 ----"
    docker inspect "$container_id" --format '{{json .State}}' || true
    echo "---- subsystem-provisioner 最近日志 ----"
    compose logs --no-color --tail 200 subsystem-provisioner || true
    echo "---- subsystem-provisioner compose ps ----"
    compose ps subsystem-provisioner || true
  else
    echo "未获取到 subsystem-provisioner 容器 ID" >&2
  fi
}

dump_file_gateway_debug() {
  local container_id
  container_id="$(compose ps -q file-gateway 2>/dev/null || true)"
  if [[ -n "$container_id" ]]; then
    echo "---- file-gateway 容器状态 ----"
    docker inspect "$container_id" --format '{{json .State}}' || true
    echo "---- file-gateway 最近日志 ----"
    compose logs --no-color --tail 200 file-gateway || true
    echo "---- file-gateway compose ps ----"
    compose ps file-gateway || true
  else
    echo "未获取到 file-gateway 容器 ID" >&2
    compose ps file-gateway || true
  fi
}

backup_database() {
  local mysql_service="$1"
  local database="$2"
  local backup_name="${3:-$service}"
  local output="$deploy_dir/backups/${backup_name}-${release_id}.sql.gz"
  local temporary
  temporary="$(mktemp "$deploy_dir/backups/.${backup_name}-${release_id}.XXXXXX.sql.gz")"
  chmod 600 "$temporary"
  echo "备份数据库到 $output"
  # single-transaction 为 InnoDB 提供一致性快照，备份管道任一环节失败都会因 pipefail 中止发布。
  # 安全（SEC-F5c）：root 密码只经容器内 MYSQL_PWD 环境变量传给 mysqldump，
  # 不再以 -p 加明文密码的方式拼进 argv，避免密码出现在 /proc/*/cmdline。
  if ! compose exec -T "$mysql_service" sh -c \
    'exec env MYSQL_PWD="$MYSQL_ROOT_PASSWORD" mysqldump --single-transaction --routines --triggers --events --all-databases -uroot' \
    _ "$database" \
    | gzip -9 >"$temporary"; then
    rm -f "$temporary"
    echo "数据库备份失败，已清理未完成备份文件" >&2
    return 1
  fi
  if ! gzip -t "$temporary" || [[ "$(gzip -dc "$temporary" | wc -c)" -eq 0 ]]; then
    rm -f -- "$temporary"
    echo '数据库备份损坏或SQL为空，拒绝迁移/发布' >&2
    return 1
  fi
  mv -f "$temporary" "$output" || return 1
  if ! (cd "$deploy_dir/backups" && sha256sum "$(basename -- "$output")" > "$(basename -- "$output").sha256" &&
      chmod 600 "$(basename -- "$output").sha256" && sha256sum --check "$(basename -- "$output").sha256"); then
    echo '数据库备份摘要校验失败，拒绝迁移/发布' >&2
    return 1
  fi
}

# mysql_query_scalar 在目标容器内做一次真实 SQL 探测。
# 不能只依赖 Docker healthcheck：mysqladmin ping 在“访问被拒绝”时也可能返回 0，
# 且健康状态与“此刻能否执行 SQL”之间存在竞态——容器可能刚被 OOM 杀掉并在重启，
# 此时 compose exec 会成功进入新容器，而 mysqld 尚未监听，mysqldump 只会报 socket 连接失败。
mysql_query_scalar() {
  local mysql_service="$1" sql="$2"
  compose exec -T "$mysql_service" sh -c \
    'exec env MYSQL_PWD="$MYSQL_ROOT_PASSWORD" mysql -uroot -Nse "$1"' _ "$sql" 2>/dev/null
}

# require_mysql_ready 在 compose 报告启动成功后再确认数据库真的能执行 SQL，
# 失败时输出容器状态、重启/OOM 证据与宿主内存，避免把“数据库没起来”误报成备份失败。
require_mysql_ready() {
  local mysql_service="$1"
  local timeout="${2:-120}"
  # 注意：local 的展开发生在内建命令执行之前，因此不能在同一条 local 里
  # 引用刚声明的变量（set -u 下会报 unbound variable）。
  local deadline=$((SECONDS + timeout)) probe
  while ((SECONDS < deadline)); do
    if probe="$(mysql_query_scalar "$mysql_service" 'SELECT 1')" && [[ "$probe" == "1" ]]; then
      return 0
    fi
    sleep 2
  done
  echo "数据库容器在 ${timeout}s 内无法执行 SQL：$mysql_service" >&2
  diagnose_mysql_container "$mysql_service"
  return 1
}

# diagnose_mysql_container 收集判断“为什么数据库没起来”所需的最小证据集。
diagnose_mysql_container() {
  local mysql_service="$1" container
  container="$(compose ps -q "$mysql_service" 2>/dev/null || true)"
  {
    echo "---- 诊断：$mysql_service ----"
    if [[ -z "$container" ]]; then
      echo "容器不存在或服务未创建：$mysql_service"
    else
      docker inspect --format '状态={{.State.Status}} 健康={{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}} 退出码={{.State.ExitCode}} 重启次数={{.RestartCount}} OOMKilled={{.State.OOMKilled}}' "$container" 2>&1 || true
      echo "最近 40 行容器日志："
      docker logs --tail 40 "$container" 2>&1 || true
    fi
    echo "宿主机内存："
    free -m 2>/dev/null || true
    echo "提示：MySQL 首次初始化需要数百 MiB 内存；宿主机可用内存不足（基线 16 GiB）或容器内存上限过低时，"
    echo "mysqld 会被 OOM 杀掉并反复重启，表现为长时间不健康、随后 mysqldump 报 socket 连接失败。"
    echo "请按 OFFLINE_RUNBOOK 的内存基线扩容，或减少并发启动的数据库数量后重试。"
  } >&2
}

# backup_database_if_initialized 区分“首次安装”和“升级发布”。
# 首次安装时目标库尚未创建，没有可备份的数据；此时跳过备份是正确行为，
# 不能因为 mysqldump 连不上 socket 就把首次发布判为失败（并掩盖真正的启动问题）。
backup_database_if_initialized() {
  local mysql_service="$1" database="$2" backup_name="${3:-$service}" count
  if ! count="$(mysql_query_scalar "$mysql_service" "SELECT COUNT(*) FROM information_schema.schemata WHERE schema_name = '${database}'")"; then
    echo "无法确认数据库是否已初始化：${mysql_service}（MySQL 未就绪）" >&2
    diagnose_mysql_container "$mysql_service"
    return 1
  fi
  if [[ "$count" == "0" ]]; then
    echo "数据库 ${database} 尚未创建（首次安装），跳过发布前备份"
    return 0
  fi
  backup_database "$mysql_service" "$database" "$backup_name"
}
require_backup_space() {
  local available_kib minimum_kib
  minimum_kib="${BASIC_PLATFORM_MIN_FREE_KIB:-262144}"
  [[ "$minimum_kib" =~ ^[0-9]+$ ]] || {
    echo "BASIC_PLATFORM_MIN_FREE_KIB 必须是非负整数 KiB" >&2
    return 1
  }
  available_kib="$(df -Pk "$deploy_dir" | awk 'NR == 2 { print $4 }')"
  [[ "$available_kib" =~ ^[0-9]+$ ]] || {
    echo "无法读取 $deploy_dir 的可用磁盘空间" >&2
    return 1
  }
  if ((available_kib < minimum_kib)); then
    echo "发布前磁盘空间不足：${available_kib} KiB 可用，至少需要 ${minimum_kib} KiB；请先清理 backups/releases、Docker 无用层或扩容磁盘" >&2
    return 1
  fi
}


# 用途：识别已配置的导入机器客户端，保留既有自定义 ID 和密钥。
# 参数：无，从当前运行配置按 Compose dotenv 规则读取客户端与密钥。
# 返回值：已配置返回零；缺失或占位返回一；解析失败或非法 ID 返回二。
iam_import_client_configured() {
  local client_id client_secret
  client_id="$(dsn_env_value IAM_IMPORT_FILE_GATEWAY_CLIENT_ID)" || return 2
  client_secret="$(dsn_env_value IAM_IMPORT_FILE_GATEWAY_CLIENT_SECRET)" || return 2
  case "$client_id" in ''|REPLACE_WITH_*) return 1 ;; esac
  [[ "$client_id" =~ ^[A-Za-z0-9._-]{3,128}$ ]] || return 2
  [[ -n "${client_secret//[[:space:]]/}" && "$client_secret" != REPLACE_WITH_* ]]
}

# 用途：仅在导入凭据缺失或为占位时初始化，避免升级重复签发和覆盖既有密钥。
# 参数：无，使用当前平台租户及运行配置。
# 返回值：已配置或初始化成功返回零；配置无效、签发或写回失败返回非零。
provision_iam_import_client() {
  local tenant_id client_id fragment line key value configured_status found=false
  if iam_import_client_configured; then
    return 0
  else
    configured_status=$?
    [[ "$configured_status" == 1 ]] || {
      echo '人员导入客户端配置无效，拒绝自动签发或替换凭据' >&2
      return 1
    }
  fi
  tenant_id="$(dsn_env_value SUBSYSTEM_PRODUCTION_ALLOWED_TENANT_ID)" || return 1
  [[ -n "$tenant_id" ]] || {
    echo '缺少 SUBSYSTEM_PRODUCTION_ALLOWED_TENANT_ID，无法初始化人员导入文件网关客户端' >&2
    return 1
  }
  client_id="$(dsn_env_value IAM_IMPORT_FILE_GATEWAY_CLIENT_ID)" || return 1
  case "$client_id" in ''|REPLACE_WITH_*) client_id=platform-iam-import-file-gateway ;; esac
  fragment="$(compose run -T --rm --no-deps \
    -e IAM_IMPORT_PROVISION_TENANT_ID="$tenant_id" \
    -e IAM_IMPORT_PROVISION_APPLICATION_CODE=platform \
    -e IAM_IMPORT_PROVISION_ENVIRONMENT=prod \
    -e IAM_IMPORT_PROVISION_CLIENT_ID="$client_id" \
    -e IAM_IMPORT_PROVISION_OUTPUT_PATH=- \
    platform-migrate ./provision-iam-import-client)" || return 1
  while IFS= read -r line; do
    case "$line" in
      IAM_IMPORT_FILE_GATEWAY_BASE_URL=*|IAM_IMPORT_FILE_GATEWAY_TOKEN_URL=*|IAM_IMPORT_FILE_GATEWAY_CLIENT_ID=*|IAM_IMPORT_FILE_GATEWAY_CLIENT_SECRET=*|IAM_IMPORT_FILE_GATEWAY_SCOPE=*) ;;
      *) continue ;;
    esac
    key="${line%%=*}"
    value="${line#*=}"
    env_set "$runtime_file" "$key" "$value" || return 1
    found=true
  done <<< "$fragment"
  [[ "$found" == true ]] || {
    echo '人员导入文件网关客户端已创建，但未取得可写入 .env 的凭据片段' >&2
    return 1
  }
  echo '人员导入文件网关客户端已初始化；凭据已写入受保护的 .env。'
}

# initialize_platform_audit_hash_key 补齐审计必开模式所需密钥且保护历史可验证性。
# 参数：无，使用当前发布的运行配置与平台数据库。
# 返回值：已配置或安全初始化返回零；缺失历史密钥、备份或查询失败则阻断发布。
initialize_platform_audit_hash_key() {
  local helper="$script_dir/initialize-audit-hash-key.py" check_status history history_query database_name
  if python3 "$helper" --env "$runtime_file" --check; then
    return 0
  else
    check_status=$?
  fi
  [[ "$check_status" == 3 ]] || return 1
  database_name="$(env_value MYSQL_DATABASE)"
  [[ "$database_name" =~ ^[a-zA-Z_][a-zA-Z0-9_]*$ ]] || {
    echo '平台数据库名缺失或不支持安全标识符，拒绝初始化审计密钥' >&2
    return 1
  }
  history_query="SELECT (EXISTS(SELECT 1 FROM ${database_name}.audit_event WHERE "
  history_query+="UPPER(TRIM(COALESCE(payload_hash_alg, ''))) NOT IN ('', 'SHA-256')) OR "
  # 已清理归档或链头仍可能依赖旧密钥；无法从活动行证明无历史时不能自动换钥。
  history_query+="EXISTS(SELECT 1 FROM ${database_name}.audit_archive_item WHERE purged_at IS NOT NULL) OR "
  history_query+="EXISTS(SELECT 1 FROM ${database_name}.audit_event_chain))"
  if ! history="$(mysql_query_scalar platform-mysql "$history_query")"; then
    echo '平台审计历史查询失败，未生成或替换密钥' >&2
    return 1
  fi
  [[ "$history" == 0 || "$history" == 1 ]] || {
    echo '无法核验历史审计摘要，拒绝初始化密钥' >&2
    return 1
  }
  python3 "$helper" --env "$runtime_file" \
    --backup "$deploy_dir/backups/platform-audit-hash-key.initial.env" \
    --has-hmac-history "$history" || return 1
  echo '审计密钥已完成受保护备份与运行配置写入；未输出密钥内容'
}

# dump_platform_runtime_debug 收集候选 API/Worker 在回退前的健康状态与错误日志。
# 参数：无，使用当前发布的 Compose 与服务标识。
# 返回值：无；诊断失败不覆盖原始发布错误，不读取容器环境或运行密钥。
dump_platform_runtime_debug() {
  local runtime_service container_id
  compose logs --no-color --tail 120 platform-api platform-worker >&2 || true
  for runtime_service in platform-api platform-worker; do
    container_id="$(compose ps -a -q "$runtime_service" 2>/dev/null || true)"
    [[ -n "$container_id" ]] || continue
    docker inspect --format \
      '服务={{.Name}} 状态={{.State.Status}} 退出码={{.State.ExitCode}} 重启={{.RestartCount}} OOM={{.State.OOMKilled}} 健康={{json .State.Health}}' \
      "$container_id" >&2 || true
  done
}

# database_probe_attempts / database_probe_retry_delay 控制数据库身份探测的重试节奏。
# 参数：无。
# 说明：取值不影响 fail-closed 语义——连续 database_probe_attempts 次失败仍然中止平台
#      发布。测试可把间隔覆盖为 0 以免真的等待；生产固定 3 次、间隔 2 秒。
database_probe_attempts=3
database_probe_retry_delay=2

# database_container_id_snapshot 记录一组数据库服务在本轮 compose up 之前的容器 ID。
# 参数：$1.. 为 docker-compose.yml 中的数据库服务名（如 platform-mysql keycloak-db）。
# 输出：标准输出每行 "服务=容器 ID"；服务尚未部署（首次安装）时写空值。
# 返回值：成功为零；任一数据库的容器身份探测连续失败则返回非零，且不输出任何快照。
#        必须 fail-closed：快照缺失会让"本轮是否重建"永远判定为否，进而把新容器 ID
#        记成"已处置"，让 keycloak 带着指向已消失容器的 Hikari 池永久失修。
database_container_id_snapshot() {
  local database container_id snapshot=""
  for database in "$@"; do
    container_id="$(database_container_current_id_strict "$database")" || return 1
    snapshot+="$database=$container_id"$'\n'
  done
  printf '%s' "$snapshot"
}

# database_container_current_id_strict 严格读取某个服务当前（包含已退出容器）的容器 ID。
# 参数：$1 为服务名。
# 输出：容器 ID；探测成功但容器不存在（尚未部署）时输出空行——"空"只表示未部署。
# 返回值：探测成功返回零；连续 database_probe_attempts 次探测失败返回非零且不输出内容。
# 说明：这里既不做 2>/dev/null，也不加 || true，重试 3 次仍失败即把"未知"显式交给调用方
#      处理（中止平台发布），绝不用空字符串冒充"未部署/未重建"——一次瞬时 docker/
#      compose 故障不能让数据库生命周期守卫静默降级（现场 P0 的根因）。
#      这里必须用 ps -a：ps -q 只列 running/restarting，会把已退出的强依赖误判成
#      "未部署"，于是永远跳过修复。
database_container_current_id_strict() {
  local service="$1"
  local attempts="${database_probe_attempts:-3}" delay="${database_probe_retry_delay:-2}"
  local attempt=1 output line first_line
  while ((attempt <= attempts)); do
    if output="$(compose ps -a -q "$service")"; then
      first_line=""
      while IFS= read -r line; do
        first_line="$line"
        break
      done <<<"$output"
      printf '%s\n' "$first_line"
      return 0
    fi
    if ((attempt < attempts)); then
      printf '[警告] 探测容器状态失败，%s 秒后重试（第 %s/%s 次）：compose ps -a -q %s\n' \
        "$delay" "$attempt" "$attempts" "$service" >&2
      sleep "$delay"
    fi
    attempt=$((attempt + 1))
  done
  printf '[错误] 无法探测数据库容器身份：服务=%s，探测命令=compose ps -a -q %s；已尝试 %s 次仍失败\n' \
    "$service" "$service" "$attempts" >&2
  printf '[错误] 数据库生命周期守卫无法评估（身份未知不等于"未重建"），中止平台发布；请先修复 docker/compose 后重跑\n' >&2
  return 1
}

# database_snapshot_container_id 从快照文本中取出某数据库在本轮启动前的容器 ID。
# 参数：$1 为快照文本；$2 为数据库服务名。
# 输出：启动前的容器 ID；首次安装（启动前不存在）输出空行。
database_snapshot_container_id() {
  printf '%s\n' "$1" | awk -F= -v key="$2" '$1 == key {print $2; exit}'
}

# database_container_recreated 判断某个数据库服务在本轮 compose up 之后是否被重建。
# 参数：$1 为 database_container_id_snapshot 输出的快照文本；$2 为数据库服务名；
#      $3 为本轮启动后的容器 ID，必须由 database_container_current_id_strict 取得。
# 返回值：容器 ID 发生变化返回零（本轮被重建）；未变化返回一；快照中为空（首次安装，
#        启动前不存在）同样返回一，保证首次安装不会触发额外的依赖重启。
# 说明：本函数不再自行探测"启动后 ID"——那会把一次探测失败悄悄退化成"未重建"。
database_container_recreated() {
  local snapshot="$1" database="$2" before after="${3:-}"
  before="$(database_snapshot_container_id "$snapshot" "$database")"
  [[ -n "$before" ]] || return 1
  [[ -n "$after" ]] || return 1
  [[ "$after" != "$before" ]]
}

# database_refresh_state_file 输出"数据库强依赖刷新状态"文件路径。
#
# 这是跨轮次的持久化事实：本轮快照只能证明"这次 compose up 有没有重建数据库"，
# 一旦发布在记录之前失败（现场序列：require_mysql_ready/备份/config-check/migrate
# 任一失败后重跑 --resume），下一次的快照已经是被重建之后的新容器 ID，只靠本轮
# 比对会得出"未重建"，于是 keycloak 带着指向已消失容器的 Hikari 池继续运行。
# 参数：无。
# 输出：runtime/database-refresh-state 的绝对路径。
# 返回值：恒为零。
database_refresh_state_file() {
  printf '%s\n' "$deploy_dir/runtime/database-refresh-state"
}

# database_refresh_state_read 读取某个数据库"上一次已成功处置强依赖"时的容器 ID。
# 参数：$1 为数据库服务名。
# 输出：已记录的容器 ID。
# 返回值：读到返回零；状态文件不存在、是符号链接、不是普通文件、权限不是 0600，
#        或其中没有该数据库的记录时返回一（一律按"没有历史记录"处理，保守地再
#        刷新一次强依赖，宁可多做一次重建也不要留下失效连接池）。
# 说明：文件只保存容器 ID（不含凭据），仍拒绝符号链接与宽权限文件，避免有人用
#      可替换文件伪造"已刷新"而跳过真正的修复。
database_refresh_state_read() {
  local database="$1" state_file value
  state_file="$(database_refresh_state_file)"
  if [[ -L "$state_file" ]]; then
    printf '拒绝读取符号链接的数据库刷新状态文件：%s；按首次安装处理\n' "$state_file" >&2
    return 1
  fi
  if [[ ! -f "$state_file" ]]; then
    return 1
  fi
  if [[ "$(stat -c '%a' "$state_file" 2>/dev/null || true)" != '600' ]]; then
    printf '数据库刷新状态文件权限过宽（期望 0600），忽略其内容：%s\n' "$state_file" >&2
    return 1
  fi
  value="$(awk -F= -v key="$database" '
    $0 ~ /^[[:space:]]*#/ {next}
    $1 == key {print $2; exit}
  ' "$state_file" 2>/dev/null || true)"
  if [[ -z "$value" ]]; then
    return 1
  fi
  printf '%s\n' "$value"
}

# database_refresh_state_record 把某个数据库当前的容器 ID 记为"强依赖已处置"。
# 参数：$1 为数据库服务名；$2 为当前容器 ID（为空表示容器还没创建，跳过不记）。
# 副作用：原子更新 runtime/database-refresh-state（0600，先 mktemp 再 mv，mv 只替换
#      目录项，绝不像重定向那样写穿符号链接）。
# 返回值：恒为零。状态文件写不进去只告警，不阻断已经通过健康检查的发布；代价只是
#        下一次发布多刷新一次强依赖（安全方向），不会留下未修复的死连接池。
database_refresh_state_record() {
  local database="$1" container_id="$2" state_file runtime_dir temporary
  [[ -n "$container_id" ]] || return 0
  state_file="$(database_refresh_state_file)"
  runtime_dir="$deploy_dir/runtime"
  if [[ -L "$state_file" ]]; then
    printf '拒绝写入符号链接的数据库刷新状态文件：%s；本次不记录，下次发布将再次刷新强依赖\n' "$state_file" >&2
    return 0
  fi
  if ! install -d -m 700 "$runtime_dir"; then
    printf '无法创建运行时目录，跳过数据库刷新状态记录：%s\n' "$runtime_dir" >&2
    return 0
  fi
  temporary="$(mktemp "$runtime_dir/.database-refresh-state.XXXXXX")" || {
    printf '无法创建数据库刷新状态临时文件，跳过记录：%s\n' "$runtime_dir" >&2
    return 0
  }
  if {
    printf '# 数据库强依赖刷新状态：记录上一次成功处置后各数据库容器的 ID，\n'
    printf '# 供下一次发布判断跨轮次是否重建。由 bin/deploy-service.sh 自动维护，请勿手工编辑。\n'
    if [[ -f "$state_file" ]]; then
      awk -F= -v key="$database" '
        $0 ~ /^[[:space:]]*#/ {next}
        $1 == key {next}
        NF {print}
      ' "$state_file"
    fi
    printf '%s=%s\n' "$database" "$container_id"
  } >"$temporary"; then
    if mv -f "$temporary" "$state_file"; then
      chmod 600 "$state_file" 2>/dev/null || true
      return 0
    fi
  fi
  rm -f "$temporary"
  printf '数据库刷新状态写入失败：%s；本次不记录，下次发布将再次刷新强依赖\n' "$state_file" >&2
  return 0
}

# database_refresh_state_record_current 把若干数据库当前的容器 ID 一并记为已处置。
# 参数：$1.. 为数据库服务名。
# 返回值：恒为零；只读容器 ID 并写状态文件。
# 说明：容器 ID 必须经 fail-closed 探测取得；探测失败时跳过该数据库的记录（宁可下次
#      多刷新一次强依赖，也不能把未知 ID 记成"已处置"）。
database_refresh_state_record_current() {
  local database container_id
  for database in "$@"; do
    if ! container_id="$(database_container_current_id_strict "$database")"; then
      printf '[警告] 无法探测数据库容器 ID，跳过刷新状态记录：%s；下次发布将再次刷新其强依赖\n' \
        "$database" >&2
      continue
    fi
    database_refresh_state_record "$database" "$container_id"
  done
  return 0
}

# database_refresh_failure_marker_file 输出"数据库强依赖刷新失败"持久标记的文件路径。
#
# 背景（X3）：强依赖刷新失败原来只写两行标准错误，长时间发布日志里滚过去就没了，
# status/verify/doctor 也读不到任何信号。因此把失败落成持久标记，供运维与后续检查
# 命令直接读取：
#   路径：<deploy_dir>/runtime/database-refresh-failed.json
#   结构：{"schema":"uip.database-refresh-failed/v1","failures":[
#            {"database":"<数据库服务名>","service":"<强依赖服务名>",
#             "reason":"<失败原因>","at":"<UTC ISO8601 时间>"}]}
#   生命周期：本轮平台发布里任一强依赖刷新/探测失败即原子写入（0600，拒绝符号链接）；
#            之后某一次平台发布的强依赖处置全部成功时删除它。
# 文件只含服务名与固定原因文本，不含凭据、地址或容器 ID。
# 参数：无。
# 输出：runtime/database-refresh-failed.json 的绝对路径。
# 返回值：恒为零。
database_refresh_failure_marker_file() {
  printf '%s\n' "$deploy_dir/runtime/database-refresh-failed.json"
}

# database_refresh_json_string 把文本转义成合法的 JSON 字符串内容。
# 参数：$1 为原文。
# 输出：转义后的内容（不含两侧双引号）。
# 返回值：恒为零。
database_refresh_json_string() {
  local text="$1"
  text="${text//\\/\\\\}"
  text="${text//\"/\\\"}"
  text="${text//$'\n'/\\n}"
  text="${text//$'\r'/\\r}"
  text="${text//$'\t'/\\t}"
  printf '%s' "$text"
}

# database_refresh_failures 在进程内累计本次发布的强依赖刷新失败记录。
# 元素格式：<数据库服务名>\t<强依赖服务名>\t<原因>\t<UTC ISO8601>。
# 说明：只在进程内聚合，落盘只发生在 database_refresh_failure_marker_write。
database_refresh_failures=()

# database_refresh_failure_note 记录一条强依赖刷新/探测失败，供本轮结束时写持久标记。
# 参数：$1 为数据库服务名；$2 为强依赖服务名（数据库自身身份探测失败时填该数据库名）；
#      $3 为固定原因文本（不得包含凭据、地址等现场敏感内容）。
# 返回值：恒为零。
database_refresh_failure_note() {
  database_refresh_failures+=("$1"$'\t'"$2"$'\t'"$3"$'\t'"$(date -u +%Y-%m-%dT%H:%M:%SZ)")
  return 0
}

# database_refresh_failure_marker_write 原子写入"强依赖刷新失败"持久标记。
# 参数：无（读取进程内 database_refresh_failures）。
# 副作用：在 runtime 目录内先 mktemp 写临时文件，再 mv -f 替换目标；chmod 600；
#       目标已是符号链接时拒绝写入（mv 只替换目录项，绝不像重定向那样写穿链接）。
# 返回值：恒为零。标记写不进去只告警，不改变发布结论（刷新失败本身仍是"只告警、
#        不记录状态、下次重试"），代价只是少一个持久信号。
database_refresh_failure_marker_write() {
  local marker runtime_dir temporary entry database service reason at first=true
  marker="$(database_refresh_failure_marker_file)"
  runtime_dir="$deploy_dir/runtime"
  if [[ -L "$marker" ]]; then
    printf '拒绝写入符号链接的数据库刷新失败标记：%s；本次不写标记，请人工核对\n' "$marker" >&2
    return 0
  fi
  if ! install -d -m 700 "$runtime_dir"; then
    printf '无法创建运行时目录，跳过数据库刷新失败标记：%s\n' "$runtime_dir" >&2
    return 0
  fi
  temporary="$(mktemp "$runtime_dir/.database-refresh-failed.XXXXXX")" || {
    printf '无法创建数据库刷新失败标记临时文件，跳过写入：%s\n' "$runtime_dir" >&2
    return 0
  }
  if {
    printf '{"schema":"uip.database-refresh-failed/v1","failures":['
    if ((${#database_refresh_failures[@]} > 0)); then
      for entry in "${database_refresh_failures[@]}"; do
        IFS=$'\t' read -r database service reason at <<<"$entry"
        if [[ "$first" != true ]]; then
          printf ','
        fi
        first=false
        printf '{"database":"%s","service":"%s","reason":"%s","at":"%s"}' \
          "$(database_refresh_json_string "$database")" \
          "$(database_refresh_json_string "$service")" \
          "$(database_refresh_json_string "$reason")" \
          "$(database_refresh_json_string "$at")"
      done
    fi
    printf ']}\n'
  } >"$temporary"; then
    if mv -f -- "$temporary" "$marker"; then
      chmod 600 -- "$marker" 2>/dev/null || true
      return 0
    fi
  fi
  rm -f -- "$temporary"
  printf '数据库刷新失败标记写入失败：%s；本次不留持久信号，请查看上方 [警告]\n' "$marker" >&2
  return 0
}

# database_refresh_failure_marker_clear 删除持久失败标记（本次发布刷新全部成功）。
# 参数：无。
# 返回值：恒为零。rm 不跟随符号链接，因此即使有人把标记换成链接也只会删掉链接本身。
database_refresh_failure_marker_clear() {
  local marker
  marker="$(database_refresh_failure_marker_file)"
  [[ -e "$marker" || -L "$marker" ]] || return 0
  if rm -f -- "$marker"; then
    printf '数据库强依赖刷新已全部成功，已清除历史失败标记：%s\n' "$marker"
  else
    printf '[警告] 无法清除数据库刷新失败标记：%s；请人工核对上一次失败是否已经修复\n' "$marker" >&2
  fi
  return 0
}

# database_refresh_state_stale 判断跨轮次是否需要按"数据库已重建"处置（D1 修复核心）。
# 参数：$1 为数据库服务名；$2 为当前容器 ID；$3 为本轮 compose up 之前的容器 ID。
# 返回值：需要刷新强依赖时返回零：
#        - 持久化记录存在且与当前容器 ID 不同：数据库在平台发布之外被重建过；
#        - 持久化记录缺失、但本轮启动前该数据库容器已存在：上一轮发布在记录之前
#          中止（现场"发布中止两次"的序列），必须重试修复，不能当成未重建。
#        当前容器尚不存在（首次安装）或记录与当前 ID 一致时返回一。
database_refresh_state_stale() {
  local database="$1" current_id="$2" before_id="$3" persisted
  [[ -n "$current_id" ]] || return 1
  if persisted="$(database_refresh_state_read "$database")"; then
    if [[ "$persisted" == "$current_id" ]]; then
      return 1
    fi
    return 0
  fi
  [[ -n "$before_id" ]] || return 1
  return 0
}

# database_dependent_disposition 登记"数据库容器被重建后其强依赖如何处置"的唯一映射表。
#
# 背景（现场 P0 事故，2026-11-05 协调升级）：deploy_platform 只用一条
# compose up -d --wait 启动 platform-mysql、keycloak-db、contract-mysql。compose 会
# 重建配置变化的数据库容器，但不会重建使用这些数据库的服务。现场 keycloak-db 已是
# 2026-11-05T04:00:04Z 的新容器，keycloak 仍是 2026-10-19 的旧容器：Hikari 连接池
# 指向已消失的旧容器，Infinispan clientscopes 缓存卡死；因为健康探针只测 TCP，
# keycloak 仍报告 healthy，全部请求处理被阻塞 20 分钟以上，platform-worker 随后在
# Keycloak 管理 API 上崩溃重启，发布被中止两次。因此数据库容器重建后必须显式刷新
# 强依赖，不能指望 compose 或健康检查兜底。
#
# 处置方式（第一列）：
#   inline  由 deploy_platform 在同一条启动命令里追加 --force-recreate，避免同一服务
#           在一次发布里被启动两次（keycloak 与按需 Temporal 合并启动）。
#   refresh 由 refresh_recreated_database_dependents 立即强制重建，只针对已存在且所属
#           子系统仍在当前 Compose 中的容器，且必须带 --no-deps；失败只告警不中止
#           平台发布（业务子系统不能硬门禁平台发布）。
#   later   该强依赖在 deploy_platform 后续步骤中已经无条件 --force-recreate，此处只输出
#           证据，不再重复重启，避免一次发布内两次重建同一服务。
#   none    未登记的数据库：不改动任何容器（保守默认）。
# 参数：$1 为数据库服务名。
# 输出：标准输出一行 "<处置方式> [强依赖服务名...]"。
database_dependent_disposition() {
  case "$1" in
    # Keycloak 进程内持有到 keycloak-db 的 Hikari 连接和 Infinispan clientscopes 缓存，
    # 数据库容器重建后必须一起重建（本次事故的直接原因）。
    keycloak-db) printf 'inline keycloak\n' ;;
    # contract-api 与 contract-worker 进程内都持有到 contract-mysql 的连接池；
    # worker 还会按自己的重试节奏反复报错，因此两者必须一起刷新。
    contract-mysql) printf 'refresh contract-api contract-worker\n' ;;
    # platform-api/platform-worker/subsystem-provisioner 在本函数后续步骤中已经
    # 无条件 --force-recreate，这里不重复重启。
    platform-mysql) printf 'later platform-api platform-worker subsystem-provisioner\n' ;;
    # file-gateway 同样在后续步骤中无条件 --force-recreate。
    file-gateway-mysql) printf 'later file-gateway\n' ;;
    *) printf 'none\n' ;;
  esac
}

# database_dependent_scope_component 登记强依赖所属的子系统编码，供 scope_enabled 校验。
# 参数：$1 为强依赖服务名。
# 输出：子系统编码；未登记返回非零（表示不受子系统开关约束）。
database_dependent_scope_component() {
  case "$1" in
    contract-api|contract-worker) printf 'contract\n' ;;
    *) return 1 ;;
  esac
}

# database_dependent_kept_running 登记"数据库重建后明确不自动重启"的依赖及原因。
# 参数：$1 为数据库服务名。
# 输出：每行 "<服务名>\t<原因>"；没有登记时输出空。
# 说明：Temporal 的可见性库也在 contract-mysql 中，但 Go database/sql 会自行重连，
#      且重启会打断在途工作流；这里只输出证据，避免运维误以为漏刷了 Temporal。
database_dependent_kept_running() {
  case "$1" in
    contract-mysql) printf 'temporal\t共享 Temporal 的可见性库同在 contract-mysql，database/sql 会自行重连；重启会打断在途工作流，因此不随平台发布重建\n' ;;
    *) : ;;
  esac
}

# refresh_recreated_database_dependents 刷新被重建的数据库容器的强依赖服务。
# 参数：$1 为 database_container_id_snapshot 生成的快照文本；$2.. 为本轮 compose up
#      启动过的数据库服务名。
# 输出：标准输出逐行打印可 grep 的中文证据，例如
#      "数据库已重建，强制重建强依赖：keycloak-db -> keycloak"、
#      "数据库未重建，保持现有依赖容器：platform-mysql"。
#      refresh 处置失败时向标准错误打印以 "[警告]" 开头的行，明确"平台发布继续；
#      该子系统随后由自身发布/verify 阶段判定"。
# 副作用：把需要调用方在同一条启动命令里追加 --force-recreate 的强依赖写入
#      database_refresh_inline_dependents（空格分隔）；把 inline 处置对应的数据库写入
#      database_refresh_inline_databases（数组，供调用方在强依赖真正启动成功后记录状态）。
#      每个数据库处置成功后把当前容器 ID 写入 runtime/database-refresh-state。
#      本轮只要有强依赖刷新或探测失败，就把失败明细原子写入
#      runtime/database-refresh-failed.json（0600）；本轮全部成功则删除该标记。
# 返回值：成功为零；已存在强依赖的强制重建失败只告警并继续（业务子系统故障不能升级为
#      平台发布失败），但失败的那条数据库不记录状态，下一次发布仍会重试；
#      未部署或所属子系统未启用的强依赖只打印跳过证据，绝不新建容器或启动停用子系统。
#      唯一例外：数据库自身的容器身份无法探测时返回非零（生命周期守卫不可评估，不能
#      静默降级成"未重建"），由 deploy_platform 中止平台发布。
refresh_recreated_database_dependents() {
  local snapshot="$1"
  shift
  local database disposition dependent container_id current_id before_id component dependent_service
  local refresh_failed defer_record kept reason_text index
  local scope_resolved=false identity_unknown=false enabled_services=""
  local -a fields
  database_refresh_inline_dependents=""
  database_refresh_inline_databases=()
  database_refresh_failures=()
  # 启用范围只解析一次：每条数据库都要判定"强依赖所属子系统是否启用"，逐条重解析既慢，
  # 又会把一次瞬时解析失败误传成多条"未启用/未部署"的假结论（X2）。解析失败时下面
  # 绝不猜"未启用"，而是按"无法判定"处理：不记录状态并留下持久失败标记。
  if enabled_services="$(scope_services)"; then
    scope_resolved=true
  else
    printf '[警告] 无法解析统一 Compose 服务清单；本轮无法判定强依赖是否启用，相关数据库不记录刷新状态\n' >&2
  fi
  for database in "$@"; do
    before_id="$(database_snapshot_container_id "$snapshot" "$database")"
    if ! current_id="$(database_container_current_id_strict "$database")"; then
      # 数据库身份未知：既不能当作"未重建"，也不能记成"已处置"。
      refresh_failed=true
      identity_unknown=true
      printf '[错误] 无法探测数据库容器身份，本条数据库不记录刷新状态：%s\n' "$database" >&2
      database_refresh_failure_note "$database" "$database" \
        '无法探测数据库容器身份（compose ps -a -q 连续失败）'
      continue
    fi
    refresh_failed=false
    defer_record=false
    if ! database_container_recreated "$snapshot" "$database" "$current_id" &&
       ! database_refresh_state_stale "$database" "$current_id" "$before_id"; then
      printf '数据库未重建，保持现有依赖容器：%s\n' "$database"
      database_refresh_state_record "$database" "$current_id"
      continue
    fi
    read -r -a fields <<<"$(database_dependent_disposition "$database")"
    disposition="${fields[0]:-none}"
    case "$disposition" in
      inline)
        for ((index = 1; index < ${#fields[@]}; index++)); do
          dependent="${fields[index]}"
          printf '数据库已重建，强制重建强依赖：%s -> %s\n' "$database" "$dependent"
          database_refresh_inline_dependents="${database_refresh_inline_dependents:+$database_refresh_inline_dependents }$dependent"
        done
        # inline 的强依赖在本函数返回后才真正启动，因此由调用方在启动成功后记录；
        # 否则启动失败会被记成"已刷新"，下一次发布就再也不会修复。
        defer_record=true
        database_refresh_inline_databases+=("$database")
        ;;
      refresh)
        for ((index = 1; index < ${#fields[@]}; index++)); do
          dependent="${fields[index]}"
          # 子系统被注释掉时必须什么都不做：对不存在的服务 compose up 会直接失败，
          # 那会把"未启用该子系统"变成平台发布故障。
          component="$(database_dependent_scope_component "$dependent" 2>/dev/null || true)"
          if [[ -n "$component" ]]; then
            if [[ "$scope_resolved" != true ]]; then
              # 解析失败 ≠ 未启用：不能把一次探针故障永久固化成"该子系统不部署"。
              printf '[警告] 无法解析统一 Compose 服务清单，本条数据库不记录刷新状态：%s（强依赖 %s 是否启用无法判定）\n' \
                "$database" "$dependent" >&2
              refresh_failed=true
              database_refresh_failure_note "$database" "$dependent" \
                '无法解析统一 Compose 服务清单，未能判定强依赖是否启用'
              continue
            fi
            dependent_service="$(scope_service_for "$component" 2>/dev/null || true)"
            if [[ -n "$dependent_service" ]] &&
               ! grep -Fxq "$dependent_service" <<<"$enabled_services"; then
              printf '数据库已重建，强依赖所属子系统未启用，跳过：%s -> %s（当前 docker-compose.yml 未启用 %s 子系统）\n' \
                "$database" "$dependent" "$component"
              continue
            fi
          fi
          # 只刷新已经存在的容器，且必须用 ps -a：ps -q 只列 running/restarting，
          # 会把已退出（exited）的强依赖误判成"未部署"而永远不修复。
          # 探测失败同样不等于"未部署"：重试 3 次仍失败就按失败处置（不记录状态、
          # 留持久标记），绝不用「未部署」把一次探针故障说成事实。
          if ! container_id="$(database_container_current_id_strict "$dependent")"; then
            printf '[警告] 无法探测强依赖容器状态：%s -> %s（compose ps -a -q 连续失败）；本条数据库不记录刷新状态，下次发布将重试\n' \
              "$database" "$dependent" >&2
            refresh_failed=true
            database_refresh_failure_note "$database" "$dependent" \
              '无法探测强依赖容器状态（compose ps -a -q 连续失败）'
            continue
          fi
          if [[ -z "$container_id" ]]; then
            printf '数据库已重建，强依赖未部署，跳过：%s -> %s\n' "$database" "$dependent"
            continue
          fi
          printf '数据库已重建，强制重建强依赖：%s -> %s\n' "$database" "$dependent"
          # 等待上限必须覆盖 contract-api/contract-worker 自身的健康检查视界：
          # healthcheck interval 10s × retries 15 + start_period 10s ≈ 160s。原来的
          # 120s 会在"慢但最终健康"时误报失败，于是该数据库不被记录，导致每一次平台
          # 发布都强制重建一次；这里取 240s 留余量，其他超时保持原值。
          if ! compose up -d --force-recreate --no-deps --wait --wait-timeout 240 "$dependent"; then
            printf '[警告] 数据库已重建，但强依赖刷新失败：%s -> %s；平台发布继续；该子系统随后由自身发布/verify 阶段判定。请在该子系统发布阶段重新部署它，否则它可能继续使用指向旧数据库容器的连接池\n' \
              "$database" "$dependent" >&2
            refresh_failed=true
            database_refresh_failure_note "$database" "$dependent" \
              '强依赖强制重建未通过 --wait 健康检查'
            continue
          fi
        done
        ;;
      later)
        printf '数据库已重建，强依赖由后续发布步骤强制重建：%s\n' "$database"
        ;;
      *)
        printf '数据库已重建，未登记强依赖，保持不变：%s\n' "$database"
        ;;
    esac
    while IFS=$'\t' read -r kept reason_text; do
      [[ -n "$kept" ]] || continue
      printf '数据库已重建，保持运行不重启：%s -> %s（%s）\n' "$database" "$kept" "$reason_text"
    done < <(database_dependent_kept_running "$database")
    if [[ "$refresh_failed" == true ]]; then
      printf '数据库刷新未完成，不记录刷新状态，下次发布将重试：%s\n' "$database" >&2
    elif [[ "$defer_record" == true ]]; then
      printf '数据库强依赖将在启动成功后记录刷新状态：%s\n' "$database"
    else
      database_refresh_state_record "$database" "$current_id"
    fi
  done
  # 本轮所有数据库处置完毕后统一落持久信号：有失败写标记，全部成功则清除历史标记。
  # 标记是给运维和 status/verify 看的持久证据，不影响"业务子系统故障不阻断平台发布"
  # 的既有语义。
  if ((${#database_refresh_failures[@]} > 0)); then
    database_refresh_failure_marker_write
  else
    database_refresh_failure_marker_clear
  fi
  [[ "$identity_unknown" != true ]] || return 1
  return 0
}

# deploy_platform 备份并迁移数据库，再升级同版本平台控制面与后台进程。
# 参数：无，使用已校验的候选镜像及当前部署目录。
# 返回值：成功为零；备份、迁移、配置、镜像或健康校验失败则中止发布。
deploy_platform() {
	local file_gateway_root file_gateway_db_name file_gateway_container file_gateway_health
	local database_snapshot database database_refresh_inline_dependents=""
	local -a database_refresh_inline_databases=()
	local -a keycloak_start_options
  prepare_managed_public_proxy_image || return 1
	file_gateway_root="$(env_value FILE_GATEWAY_HOST_ROOT)"
	file_gateway_root="${file_gateway_root:-$deploy_dir/data/file-gateway}"
	file_gateway_db_name="$(env_value FILE_GATEWAY_DB_NAME)"
	file_gateway_db_name="${file_gateway_db_name:-file_gateway}"
	if [[ -L "$file_gateway_root" ]]; then
		echo "拒绝符号链接文件网关目录：$file_gateway_root" >&2
		return 1
	fi
	if [[ ! -d "$file_gateway_root" ]]; then
		echo "缺少文件网关持久化目录：${file_gateway_root}；请使用 root 创建 temporary、quarantine 并执行 chown -R 10001:10001" >&2
		return 1
	fi
	# 根目录为 0750 且归属专用 UID 后，普通 CI 账号不能继续穿越并 stat 子目录。
	# 子目录由容器 root 入口幂等创建和收紧，发布端只校验不可替换的挂载根边界。
	if [[ "$(stat -c '%u:%g' "$file_gateway_root")" != "10001:10001" ]]; then
		echo "文件网关目录属主不正确：${file_gateway_root}；请使用 root 执行 chown -R 10001:10001" >&2
		return 1
	fi
  compose run --rm --no-deps platform-key-init || return
  # 数据库容器配置变化时 compose 只重建数据库本身，持有旧连接的强依赖会继续运行。
  # 启动前记录容器 ID，启动后比对即可判断"本轮是否重建"；跨轮次的重建（上一次发布
  # 在记录之前中止，或数据库在平台发布之外被重建）由 runtime/database-refresh-state
  # 中的持久化容器 ID 兜底，见 database_refresh_state_stale。
  # 首次安装时 ID 为空，按"未重建"处理，避免首装多做一次重启。
  # 快照探测是 fail-closed 的：任一数据库的容器身份连续探测失败即中止发布，
  # 不能带着空快照继续（那会把"未知"变成"未重建"，再记成"已处置"而永久失修）。
  database_snapshot="$(database_container_id_snapshot platform-mysql keycloak-db contract-mysql)" || return 1
  compose up -d --wait --wait-timeout "${BASIC_PLATFORM_DB_START_TIMEOUT:-600}" platform-mysql keycloak-db contract-mysql || {
    diagnose_mysql_container platform-mysql
    diagnose_mysql_container keycloak-db
    diagnose_mysql_container contract-mysql
    return 1
  }
  # compose 报告成功不等于此刻能执行 SQL：必须再做一次真实连接探测，
  # 否则 mysqld 被杀/重启时会把“数据库没起来”误报成备份失败。
  require_mysql_ready platform-mysql "${BASIC_PLATFORM_DB_START_TIMEOUT:-600}" || return
  backup_database_if_initialized platform-mysql basic_platform platform || return
  compose run --rm --no-deps platform-migrate ./api config-check || return
  migration_state=attempted
  compose run --rm --no-deps platform-migrate ./migrate || return
  migration_state=completed
  initialize_platform_audit_hash_key || return 1
  provision_iam_import_client || return 1
  ensure_temporal_application_network || return
  # ensure_temporal_application_network 也可能重建 contract-mysql，因此在其之后比对快照。
  refresh_recreated_database_dependents "$database_snapshot" platform-mysql keycloak-db contract-mysql || return
  # C11：Temporal 按需启动。无任何 Temporal 消费子系统时平台发布不再拉起共享
  # Temporal；已运行的实例不受影响（compose 不提及即不触碰）。
  # keycloak-db 本轮被重建时，keycloak 必须与数据库一起重建，否则会复用指向旧容器的
  # Hikari 连接和卡死的 Infinispan clientscopes 缓存；未重建时启动参数保持不变。
  if [[ " $database_refresh_inline_dependents " == *' keycloak '* ]]; then
    # --force-recreate 作用于该命令列出的全部服务：共享 Temporal 不能被连带重建，
    # 否则会打断在途工作流，因此拆成两条命令，Temporal 仍按原参数单独启动。
    if ! compose up -d --wait --wait-timeout 180 --force-recreate keycloak; then
      # 这里已经重建过数据库，keycloak 却没能起来：必须留下持久失败标记，
      # 否则运维只会在长日志里错过这次失败（X3）。
      if ((${#database_refresh_inline_databases[@]} > 0)); then
        for database in "${database_refresh_inline_databases[@]}"; do
          database_refresh_failure_note "$database" keycloak \
            'inline 强制重建 keycloak 未通过 --wait 健康检查'
        done
      fi
      database_refresh_failure_marker_write
      return 1
    fi
    # keycloak 确实重建成功后才把 keycloak-db 记成"已处置"：启动失败时保持待刷新，
    # 下一次发布仍会强制重建 keycloak，不会再次留下指向旧数据库容器的连接池。
    if ((${#database_refresh_inline_databases[@]} > 0)); then
      database_refresh_state_record_current "${database_refresh_inline_databases[@]}"
    fi
    if temporal_consumers_enabled; then
      compose up -d --wait --wait-timeout 180 temporal || return
    else
      echo '未启用 Temporal 消费子系统，跳过共享 Temporal 启动'
    fi
  else
    keycloak_start_options=(-d --wait --wait-timeout 180)
    if temporal_consumers_enabled; then
      compose up "${keycloak_start_options[@]}" keycloak temporal || return
    else
      compose up "${keycloak_start_options[@]}" keycloak || return
      echo '未启用 Temporal 消费子系统，跳过共享 Temporal 启动'
    fi
  fi
	compose up -d --wait --wait-timeout "${BASIC_PLATFORM_DB_START_TIMEOUT:-600}" file-gateway-mysql || {
	  diagnose_mysql_container file-gateway-mysql
	  return 1
	}
	require_mysql_ready file-gateway-mysql "${BASIC_PLATFORM_DB_START_TIMEOUT:-600}" || return
	backup_database_if_initialized file-gateway-mysql "$file_gateway_db_name" file-gateway || return
	if ! compose up -d --force-recreate --wait --wait-timeout 180 file-gateway; then
		echo "file-gateway 健康启动失败" >&2
		dump_file_gateway_debug
		return 1
	fi
	file_gateway_container="$(compose ps -q file-gateway)"
	file_gateway_health="$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' "$file_gateway_container")"
	if [[ "$file_gateway_health" != healthy ]]; then
		echo 'File Gateway 未通过 /readyz 健康检查；拒绝继续启动平台 API' >&2
		dump_file_gateway_debug
		return 1
	fi
  verify_service_image compose file-gateway "$file_gateway_image_ref" || return 1
  # 迁移已完成。修改 Agent 前停止旧 API，防止接入请求跨越不同清单或解析器版本。
  compose stop --timeout 60 platform-api || return 1
  # 平台 API 只通过共享 Unix Socket 调用生产接入 Agent。先让同一平台镜像中的
  # Agent 健康，再切 API，避免新旧协议短暂不一致或页面误报 Agent 未启用。
  # Agent 需要强制重建以重载 subsystems.d 清单（无 HTTP 流量，秒级恢复）；
  # API 与 Worker 必须使用同一不可变镜像；Worker 独立运行，不能依赖 API
  # 入口脚本的双进程模式，否则 Keycloak 用户投影可能没有消费者。
  compose run --rm --no-deps subsystem-provisioner-socket-init || return
  compose up -d --wait --wait-timeout 60 docker-socket-proxy || return
  if ! compose up -d --force-recreate --wait --wait-timeout 60 --no-deps subsystem-provisioner; then
    echo "subsystem-provisioner 健康启动失败" >&2
    dump_subsystem_provisioner_debug
    return 1
  fi
  verify_subsystem_provisioner_config_consistency "$(compose ps -q subsystem-provisioner)" || return
  verify_service_image compose subsystem-provisioner "$image_ref" || return 1
  if ! compose up -d --force-recreate --no-deps --wait --wait-timeout 120 platform-api platform-worker; then
    dump_platform_runtime_debug
    return 1
  fi
  wait_for_health "http://127.0.0.1:$(port_value PLATFORM_API_PORT 18080)/readyz" || {
    # 兜底：新镜像首启异常时强制重建一次并再次等待，避免平台 API 停留在宕机状态。
    echo "platform-api 未通过健康检查，强制重建一次后重试" >&2
    compose up -d --force-recreate --no-deps --wait --wait-timeout 120 platform-api platform-worker || return
    wait_for_health "http://127.0.0.1:$(port_value PLATFORM_API_PORT 18080)/readyz" || return
  }
  verify_service_image compose platform-api "$image_ref" || return 1
  verify_service_image compose platform-worker "$image_ref" || return 1
  verify_service_stable platform-worker || return 1
  verify_subsystem_control_plane_profiles "$(compose ps -q subsystem-provisioner)" \
    "$(compose ps -q platform-api)" || return 1
  # 迁移、Agent 和 API 替换及健康检查期间始终保留标记；只有这对已验证进程能够解除待处理资产部署门禁。
  if [[ -f "$deploy_dir/runtime/.control-plane-reload-required" ]]; then
    rm -f -- "$deploy_dir/runtime/.control-plane-reload-required" || return 1
  fi
}

# warn 输出非致命提示，与服务器一键离线部署.sh、bin/deploy-doctor.sh 同一格式。
#
# 参数：$* 为中文提示内容。
# 返回值：恒为 0，调用方因此不会因告警本身中断发布。
warn() { printf '[警告] %s\n' "$*" >&2; }

# audit_contract_number_registry 发布成功后执行一次只读合同编号巡检并落盘证据。
#
# 巡检命令 ./contract-number-audit 与 ./migrate 同镜像、同 env_file（MYSQL_DSN 由
# runtime/contract.env 提供），因此不需要新增数据库凭据；它自己开只读会话
# （SET SESSION TRANSACTION READ ONLY），不会修改业务库，也不需要额外写权限。
#
# 这是现场非致命校验，不是发布门禁：发现漂移、巡检自身失败、部署目录的
# docker-compose.yml 还没有该服务、镜像里没有该二进制，都只打印 [警告] 并写入
# 证据文件，绝不返回非零 —— 失败返回会进入发布失败分支并回滚刚通过健康检查的
# 镜像，把“数据有问题”误判成“发布有问题”。
#
# 证据落在脚本已创建并校验可写的 backups/ 目录，命名与其他发布件同用 release_id。
#
# 参数：无（使用全局 deploy_dir、release_id、image_ref 和 compose 包装器）。
# 返回值：恒为 0。
audit_contract_number_registry() {
  local report="$deploy_dir/backups/contract-number-audit-${release_id}.txt"
  local temporary status=0 result
  if ! temporary="$(mktemp "$deploy_dir/backups/.contract-number-audit-${release_id}.XXXXXX")"; then
    warn "合同编号巡检跳过（未执行）：无法在 $deploy_dir/backups 创建证据临时文件，请检查目录属主与磁盘空间"
    return 0
  fi
  echo "执行合同编号巡检并落盘证据：$report"
  # compose run 的退出码就是巡检结论（0 一致 / 1 有问题或无法完成）。用 || 捕获，
  # 避免 set -e 在巡检失败时中止发布；-T 不分配 TTY，让证据文件保持纯文本行尾。
  compose run --rm --no-deps -T contract-number-audit >"$temporary" 2>&1 || status=$?
  # 「无此服务 / 找不到二进制」= 部署目录的 compose 或合同镜像还是旧版：这是「跳过」，
  # 不是数据问题，但必须明确告警，否则运维会误以为本次发布已经校验过合同编号。
  if grep -Eqi 'no such service|no such file|not found' "$temporary"; then
    result="skipped-missing-binary"
    warn "合同编号巡检跳过：docker compose 未找到 contract-number-audit 服务或其二进制（退出码 ${status}），本次发布未做合同编号校验；请确认 docker-compose.yml 与合同镜像都已更新，再单独补跑"
  else
    case "$status" in
      0)
        result="clean"
        echo "合同编号巡检通过：未发现重复编号、未登记或登记错配"
        ;;
      1)
        result="findings-or-error"
        warn "合同编号巡检报告存在漂移或未能完成（退出码 1）；发布不回滚，证据见 ${report}，请按清单受控更正后重新巡检"
        ;;
      *)
        result="error"
        warn "合同编号巡检未能正常执行（退出码 ${status}）；发布不回滚，请检查证据：${report}"
        ;;
    esac
  fi
  if {
    printf 'COMMAND=compose run --rm --no-deps -T contract-number-audit\n'
    printf 'EXIT_CODE=%s\n' "$status"
    printf 'RESULT=%s\n' "$result"
    printf 'RELEASE_ID=%s\n' "$release_id"
    printf 'CONTRACT_IMAGE=%s\n' "${image_ref:-unknown}"
    printf 'COLLECTED_AT_UTC=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    printf -- '--- 巡检输出 ---\n'
    cat "$temporary"
  } >"$report"; then
    chmod 600 "$report" || warn "合同编号巡检证据权限收紧失败，请手工执行 chmod 600：$report"
    # 变量必须写成 ${name}：中文全角括号紧跟 $name 时，Bash 会把多字节序列的一部分
    # 并进变量名，在 set -u 下直接报 unbound variable 并中止发布。
    echo "合同编号巡检证据已写入：${report}（退出码 ${status}）"
  else
    rm -f "$report"
    warn "合同编号巡检证据写入失败：$report"
  fi
  rm -f "$temporary"
  return 0
}

deploy_contract() {
  local selected runtime_service
  local -a runtime_services=()
  selected="$(scope_runtime_services contract)" || return
  while read -r runtime_service; do
    [[ -n "$runtime_service" ]] && runtime_services+=("$runtime_service")
  done <<< "$selected"
  ((${#runtime_services[@]} > 0)) || return 1
  # 子系统只有在浏览器 OIDC 与机器目录发布凭据全部就绪后才能启动，避免容器进入 401 重启循环。
  require_contract_runtime_value OIDC_CLIENT_ID || return
  require_contract_runtime_value OIDC_CLIENT_SECRET || return
  require_contract_runtime_value OIDC_TENANT_ID || return
  require_contract_runtime_value PLATFORM_AUTHORIZATION_CATALOG_APPLICATION_ID || return
  require_contract_runtime_value PLATFORM_AUTHORIZATION_CATALOG_CLIENT_ID || return
  require_contract_runtime_value PLATFORM_AUTHORIZATION_CATALOG_CLIENT_SECRET || return
  if [[ "$(contract_env_value CRM_REFERENCE_ENABLED)" == "true" ]]; then
    require_contract_runtime_value CRM_REFERENCE_BASE_URL || return
    require_contract_runtime_value CRM_REFERENCE_TOKEN_URL || return
    require_contract_runtime_value CRM_REFERENCE_CLIENT_ID || return
    require_contract_runtime_value CRM_REFERENCE_CLIENT_SECRET || return
  fi
  if [[ "$(contract_env_value PROJECT_INTEGRATION_ENABLED)" == "true" ]]; then
    require_contract_runtime_value PROJECT_API_BASE_URL || return
    require_contract_runtime_value PROJECT_INTEGRATION_TOKEN_URL || return
    require_contract_runtime_value PROJECT_INTEGRATION_CLIENT_ID || return
    require_contract_runtime_value PROJECT_INTEGRATION_CLIENT_SECRET || return
  fi
  if [[ "$(contract_env_value PLATFORM_AUTHORIZATION_CATALOG_SYNC_ENABLED)" != "true" ]]; then
    echo "PLATFORM_AUTHORIZATION_CATALOG_SYNC_ENABLED 必须为 true" >&2
    return 1
  fi
  ensure_temporal_application_network || return
  compose up -d --wait --wait-timeout "${BASIC_PLATFORM_DB_START_TIMEOUT:-600}" contract-mysql temporal || return
  require_mysql_ready contract-mysql "${BASIC_PLATFORM_DB_START_TIMEOUT:-600}" || return
  backup_database_if_initialized contract-mysql contract_management contract || return
  # 迁移命令由 docker-compose.yml 固定；非零退出会在替换 API 镜像前终止发布。
  compose run --rm --no-deps contract-migrate || return
  if ! compose up -d --force-recreate --no-deps --wait --wait-timeout 120 "${runtime_services[@]}"; then
    echo "---- 合同运行组件启动失败日志 ----" >&2
    compose logs --no-color --tail 120 "${runtime_services[@]}" >&2 || true
    return 1
  fi
  if ! wait_for_health "http://127.0.0.1:$(port_value CONTRACT_API_PORT 18081)/healthz"; then
    echo "---- contract-api 最近日志 ----" >&2
    compose logs --no-color --tail 120 contract-api >&2 || true
    return 1
  fi
  for runtime_service in "${runtime_services[@]}"; do
    verify_service_image compose "$runtime_service" "$image_ref" || return 1
    verify_service_stable "$runtime_service" || return 1
  done
  # 合同编号巡检是非致命现场校验：它恒返回 0，只采集证据并告警，
  # 既不在 || return 链上，也不改变本次发布的成功判定。
  audit_contract_number_registry
}

deploy_project() {
  # 子系统只有在浏览器 OIDC 与机器目录发布凭据全部就绪后才能启动，避免容器进入 401 重启循环。
  require_project_runtime_value OIDC_CLIENT_ID || return
  require_project_runtime_value OIDC_CLIENT_SECRET || return
  require_project_runtime_value OIDC_TENANT_ID || return
  require_project_runtime_value PLATFORM_AUTHORIZATION_CATALOG_APPLICATION_ID || return
  require_project_runtime_value PLATFORM_AUTHORIZATION_CATALOG_CLIENT_ID || return
  require_project_runtime_value PLATFORM_AUTHORIZATION_CATALOG_CLIENT_SECRET || return
  if [[ "$(project_env_value PLATFORM_AUTHORIZATION_CATALOG_SYNC_ENABLED)" != "true" ]]; then
    echo "PLATFORM_AUTHORIZATION_CATALOG_SYNC_ENABLED 必须为 true" >&2
    return 1
  fi
  # 生产必须关闭进程内后台 Worker（由 project-sla-notifier 承担）。服务端
  # docker-compose.yml 按模块裁剪保留旧值，compose environment 未必生效，
  # 因此发布前在受管运行文件里统一纠正，env_file 与 notifier 都读取该值。
  env_set "$project_runtime_file" PROJECT_RUN_WORKER_WITH_API false || return
  ensure_temporal_application_network || return
  compose up -d --wait --wait-timeout "${BASIC_PLATFORM_DB_START_TIMEOUT:-600}" project-mysql temporal || return
  require_mysql_ready project-mysql "${BASIC_PLATFORM_DB_START_TIMEOUT:-600}" || return
  backup_database_if_initialized project-mysql project_management project || return
  project_background_prepare || return
  # 迁移命令由 docker-compose.yml 固定；非零退出会在替换 API 镜像前终止发布。
  # 服务端 Compose 可能保留旧 environment（模块裁剪不覆盖），用 run -e 强制迁移进程关闭内嵌 Worker。
  compose run --rm --no-deps -e PROJECT_RUN_WORKER_WITH_API=false project-migrate || return
  compose up -d --force-recreate --no-deps --wait --wait-timeout 120 project-api project-sla-notifier || return
  if ! wait_for_health "http://127.0.0.1:$(port_value PROJECT_API_PORT 18085)/healthz"; then
    echo "---- project-api 最近日志 ----" >&2
    compose logs --no-color --tail 120 project-api >&2 || true
    return 1
  fi
  verify_service_image compose project-api "$image_ref" || return 1
  verify_service_image compose project-sla-notifier "$image_ref" || return 1
  project_background_activate || return 1
}

# reimbursement_runtime_ready 核验报销首次发布及实际部署所需的完整运行材料。
# 参数：无；仅使用受控发布上下文中的运行文件路径。
# 返回值：全部材料已配置返回零，文件缺失或占位材料返回非零。
reimbursement_runtime_ready() {
  [[ -f "$reimbursement_runtime_file" ]] || return 1
  local key value
  for key in \
    MYSQL_DSN PUBLIC_BASE_URL PLATFORM_BASE_URL PLATFORM_ENVIRONMENT_CODE \
    OIDC_ISSUER OIDC_CLIENT_ID OIDC_CLIENT_SECRET OIDC_TENANT_ID OIDC_REDIRECT_URI \
    OIDC_SESSION_ENCRYPTION_KEY_BASE64 \
    PROJECT_REFERENCE_CLIENT_ID PROJECT_REFERENCE_CLIENT_SECRET \
    PROJECT_SPEND_CLIENT_ID PROJECT_SPEND_CLIENT_SECRET \
    FILE_GATEWAY_CLIENT_ID FILE_GATEWAY_CLIENT_SECRET \
    PLATFORM_AUDIT_CLIENT_ID PLATFORM_AUDIT_CLIENT_SECRET \
    PLATFORM_AUTHORIZATION_CATALOG_APPLICATION_ID \
    PLATFORM_AUTHORIZATION_CATALOG_CLIENT_ID \
    PLATFORM_AUTHORIZATION_CATALOG_CLIENT_SECRET; do
    value="$(awk -F= -v key="$key" '$1==key {sub(/^[^=]*=/, ""); print; exit}' "$reimbursement_runtime_file")"
    if [[ -z "$value" || "$value" == PENDING_* || "$value" == REPLACE_WITH_* ]]; then
      echo "报销运行配置未就绪：$key" >&2
      return 1
    fi
  done
}

# deploy_reimbursement 以真实备份和独立目录发布推进报销API与Worker。
# 参数：无；镜像及受控runtime由当前发布上下文提供。
# 返回值：全部步骤成功返回0，缺凭据、备份或许可失败即停止。
deploy_reimbursement() {
  reimbursement_runtime_ready || return 1
  compose up -d --wait --wait-timeout "${BASIC_PLATFORM_DB_START_TIMEOUT:-600}" reimbursement-mysql || return
  require_mysql_ready reimbursement-mysql "${BASIC_PLATFORM_DB_START_TIMEOUT:-600}" || return
  backup_database reimbursement-mysql reimbursement reimbursement || return
  compose run --rm --no-deps -e REIMBURSEMENT_MIGRATION_BACKUP_VERIFIED=true reimbursement-migrate || return
  compose run --rm --no-deps reimbursement-catalog-sync || return
  compose up -d --force-recreate --no-deps --wait --wait-timeout 180 reimbursement-api reimbursement-worker || return
  verify_service_image compose reimbursement-api "$image_ref" || return
  verify_service_image compose reimbursement-worker "$image_ref" || return
}

deploy_settlement() {
  # Settlement 的浏览器会话和权限目录必须先由平台接入流程写入 runtime 文件；
  # 不能用空 Client 启动后再让 API 以 401 循环重试。
  for key in \
    OIDC_CLIENT_ID \
    OIDC_CLIENT_SECRET \
    OIDC_TENANT_ID \
    PLATFORM_AUTHORIZATION_CATALOG_APPLICATION_ID \
    PLATFORM_AUTHORIZATION_CATALOG_CLIENT_ID \
    PLATFORM_AUTHORIZATION_CATALOG_CLIENT_SECRET; do
    value="$(settlement_env_value "$key")"
    if [[ -z "$value" || "$value" == REPLACE_WITH_* || "$value" == PENDING_* ]]; then
      echo "结算运行配置缺失或仍为占位值：$key" >&2
      return 1
    fi
  done
  [[ "$(settlement_env_value PLATFORM_AUTHORIZATION_CATALOG_SYNC_ENABLED)" == "true" ]] || {
    echo "PLATFORM_AUTHORIZATION_CATALOG_SYNC_ENABLED 必须为 true" >&2
    return 1
  }
  compose up -d --wait --wait-timeout "${BASIC_PLATFORM_DB_START_TIMEOUT:-600}" settlement-mysql || return
  require_mysql_ready settlement-mysql "${BASIC_PLATFORM_DB_START_TIMEOUT:-600}" || return
  backup_database_if_initialized settlement-mysql settlement settlement || return
  compose run --rm --no-deps settlement-migrate || return
  compose up -d --force-recreate --no-deps --wait --wait-timeout 120 settlement-api settlement-worker || return
  if ! wait_for_health "http://127.0.0.1:$(port_value SETTLEMENT_API_PORT 18087)/healthz"; then
    echo "---- settlement-api 最近日志 ----" >&2
    compose logs --no-color --tail 120 settlement-api settlement-worker >&2 || true
    return 1
  fi
  compose run --rm --no-deps settlement-catalog-sync || return
  verify_service_image compose settlement-api "$image_ref" || return 1
  verify_service_image compose settlement-worker "$image_ref" || return 1
}

# update_runtime_value 原子更新受管 runtime env 中的单个键；不存在时追加。
update_runtime_value() {
  local file="$1" key="$2" value="$3" temporary
  temporary="$(mktemp "$deploy_dir/runtime/.runtime-update.XXXXXX")"
  chmod 600 "$temporary"
  if ! awk -F= -v key="$key" -v value="$value" '
    BEGIN { found=0 }
    $1 == key { print key "=" value; found=1; next }
    { print }
    END { if (!found) print key "=" value }
  ' "$file" >"$temporary"; then
    rm -f "$temporary"
    return 1
  fi
  if ! mv -f "$temporary" "$file"; then
    rm -f "$temporary"
    return 1
  fi
  chmod 600 "$file" || return 1
}

# 数据看板发布会在启动前把新镜像内嵌的授权目录哈希写入 runtime 文件。若发布失败，
# 只回滚镜像指针会留下"旧镜像 + 新哈希"：旧程序按内嵌目录校验哈希后 fail-closed 退出，
# 自动回滚也无法恢复服务。因此必须同时恢复哈希，未恢复前不得重建容器。
data_analysis_previous_role_config_hash=""
data_analysis_previous_role_config_hash_present=false

capture_data_analysis_role_config_hash() {
  local file="$1"
  data_analysis_previous_role_config_hash="$(awk -F= '$1 == "OIDC_ROLE_CONFIG_HASH" {sub(/^[^=]*=/, ""); print; exit}' "$file" 2>/dev/null)"
  if [[ -f "$file" ]] && awk -F= '$1 == "OIDC_ROLE_CONFIG_HASH" {found=1} END {exit !found}' "$file"; then
    data_analysis_previous_role_config_hash_present=true
  else
    data_analysis_previous_role_config_hash_present=false
  fi
}

restore_data_analysis_role_config_hash() {
  local file="$1" temporary
  if [[ "$data_analysis_previous_role_config_hash_present" == true ]]; then
    update_runtime_value "$file" OIDC_ROLE_CONFIG_HASH "$data_analysis_previous_role_config_hash" || return 1
    echo "已恢复数据看板授权目录哈希：${data_analysis_previous_role_config_hash:-<空值>}"
    return 0
  fi
  [[ -f "$file" ]] || return 0
  temporary="$(mktemp "$deploy_dir/runtime/.runtime-rollback.XXXXXX")" || return 1
  chmod 600 "$temporary"
  if ! awk -F= '$1 != "OIDC_ROLE_CONFIG_HASH" {print}' "$file" >"$temporary"; then
    rm -f -- "$temporary"
    return 1
  fi
  if ! mv -f -- "$temporary" "$file"; then
    rm -f -- "$temporary"
    return 1
  fi
  chmod 600 "$file" || return 1
  echo '已移除本次发布新增的数据看板授权目录哈希'
}

# 用途：按受控能力发布数析进程并校验实际不可变镜像。
# 参数：无；使用当前已通过发布门禁的发行与运行配置。
# 返回值：成功返回零，备份、迁移、能力或健康校验失败返回非零。
deploy_data_analysis() {
  # 注意：这里的循环变量不能叫 service——那会遮蔽全局 $service（发布目标名），
  # 使 backup_database_if_initialized 的默认 backup_name 变成空串，产出以 "-" 开头的
  # 备份文件名，进而被 sha256sum 当成选项（invalid option），备份校验必然失败。
  local selected initializers runtime_service
  local -a runtime_services=() remaining_services=()
  selected="$(scope_runtime_services data-analysis)" || return
  initializers="$(scope_initialization_services data-analysis)" || return
  while IFS= read -r runtime_service; do
    [[ -n "$runtime_service" ]] || continue
    runtime_services+=("$runtime_service")
    [[ "$runtime_service" == data-analysis-api ]] || remaining_services+=("$runtime_service")
  done <<< "$selected"
  # data_analysis 由 API、两个常驻 Worker 和一次性迁移镜像组成，四个镜像必须
  # 独立校验，不能用单一 tag 或只校验 dashboard-api 代替。
  compose up -d --wait --wait-timeout "${BASIC_PLATFORM_DB_START_TIMEOUT:-600}" data-analysis-mysql || return
  require_mysql_ready data-analysis-mysql "${BASIC_PLATFORM_DB_START_TIMEOUT:-600}" || return
  backup_database_if_initialized data-analysis-mysql dashboard_aggregation data-analysis || return
  while IFS= read -r runtime_service; do
    [[ -n "$runtime_service" ]] || continue
    compose run --rm --no-deps "$runtime_service" || return
  done <<< "$initializers"
  compose run --rm --no-deps data-analysis-migrate || return
  # 角色配置哈希必须与实际运行的不可变镜像一致：从迁移镜像内嵌的 authz-catalog
  # 读取哈希并原子写回受管 runtime/data-analysis.env；否则新镜像启动会因
  # OIDC 角色配置与内嵌授权目录不一致而 fail-closed，健康检查超时并回滚。
  embedded_data_analysis_hash="$(docker run --rm --entrypoint /app/authz-catalog "${data_analysis_image_refs[3]}" print data_analysis 2>/dev/null \
    | awk -F= '$1 == "claims_role_config_hash" { print $2; exit }')"
  if [[ ! "$embedded_data_analysis_hash" =~ ^sha256:[a-f0-9]{64}$ ]]; then
    echo "无法从数据看板不可变镜像读取有效授权目录哈希" >&2
    return 1
  fi
  capture_data_analysis_role_config_hash "$data_analysis_runtime_file"
  update_runtime_value "$data_analysis_runtime_file" OIDC_ROLE_CONFIG_HASH "$embedded_data_analysis_hash" || return 1
  echo "已写入数据看板镜像内嵌授权目录哈希：$embedded_data_analysis_hash"
  compose up -d --force-recreate --no-deps --wait --wait-timeout 120 \
    "${runtime_services[@]}" || {
    # 服务器编排中 data-analysis-api 的健康窗口较窄；新版本启动超过该窗口时
    # compose 依赖等待会先行失败，但容器仍在运行且 Docker healthcheck 持续执行。
    # 先兜底等待 API 转 healthy 再补拉依赖它的 Worker，不立即回滚。
    wait_container_healthy data-analysis-api || return
    compose up -d --no-deps --wait --wait-timeout 120 \
      "${remaining_services[@]}" || return
  }
  if ! wait_for_health "http://127.0.0.1:$(port_value DATA_ANALYSIS_API_PORT 18086)/data_analysis/readyz"; then
    echo "---- data-analysis-api 最近日志 ----" >&2
    compose logs --no-color --tail 120 data-analysis-api >&2 || true
    return 1
  fi
  verify_service_image compose data-analysis-api "${data_analysis_image_refs[0]}" || return 1
  verify_service_image compose data-analysis-aggregation-worker "${data_analysis_image_refs[1]}" || return 1
  verify_service_image compose data-analysis-alert-worker "${data_analysis_image_refs[2]}" || return 1
  docker image inspect "${data_analysis_image_refs[3]}" >/dev/null || {
    echo "迁移镜像未成功拉取：${data_analysis_image_refs[3]}" >&2
    return 1
  }
}

deploy_frontend() {
  public_transport_install_certificates "$deploy_dir" || return
  frontend_compose up -d --force-recreate --no-deps --wait --wait-timeout 120 frontend || return
  wait_for_health "http://127.0.0.1:${PUBLIC_HTTP_PORT}/" || return 1
  verify_service_image frontend frontend "$image_ref" || return 1
}

rollback_ref_is_immutable() {
  local ref="$1"
  [[ "$ref" =~ ^[^[:space:]]+@sha256:[a-f0-9]{64}$ ]]
}

release_image_value() {
  local key="$1"
  awk -F= -v key="$key" '$1 == key {sub(/^[^=]*=/, ""); print; exit}' "$release_file"
}

restore_legacy_file_gateway_pointer() {
  local container image temporary project
  [[ "$service" == platform ]] || return 0
  [[ -z "$(release_image_value FILE_GATEWAY_IMAGE)" ]] || return 0
  project="$(scope_project)" || return
  container="$(docker ps -a -q --filter "label=com.docker.compose.project=$project" \
    --filter 'label=com.docker.compose.service=file-gateway')" || return
  [[ -n "$container" ]] || return 0
  [[ "$container" != *$'\n'* ]] || {
    echo '发现多个 File Gateway 容器，无法确定旧镜像用于回滚' >&2
    return 1
  }
  image="$(docker inspect "$container" --format '{{.Config.Image}}')" || return
  if [[ ! "$image" =~ $acr_enterprise_or_new_personal && ! "$image" =~ $acr_legacy_personal && ! "$image" =~ $offline_local_registry ]]; then
    echo '旧 File Gateway 缺少可验证的不可变镜像；请先恢复 FILE_GATEWAY_IMAGE 后重试发布' >&2
    return 1
  fi
  # 拍摄回滚快照前先获取实际运行镜像，绝不能将新候选伪装成原 File Gateway 版本。
  temporary="$(mktemp "$deploy_dir/.release.env.gateway-recovery.XXXXXX")" || return
  if ! awk -F= -v value="$image" '$1 != "FILE_GATEWAY_IMAGE" {print} END {print "FILE_GATEWAY_IMAGE=" value}' \
    "$release_file" >"$temporary"; then
    rm -f -- "$temporary"
    return 1
  fi
  if [[ "$(id -u)" == 0 ]]; then
    chown --reference="$release_file" "$temporary" || { rm -f -- "$temporary"; return 1; }
  fi
  chmod 600 "$temporary" || { rm -f -- "$temporary"; return 1; }
  mv -f -- "$temporary" "$release_file" || { rm -f -- "$temporary"; return 1; }
  echo '已从既有 File Gateway 容器恢复不可变版本指针，后续发布可按原镜像回滚'
}

record_recovery_result() {
  local result="$1" message="$2" migration="${migration_state:-unknown}" output="$deploy_dir/runtime/recovery-status.json" temporary
  mkdir -p "$deploy_dir/runtime" || return 0
  temporary="$(mktemp "$deploy_dir/runtime/.recovery-status.XXXXXX")" || return 0
  if python3 - "$output" "$result" "$service" "$message" "$migration" >"$temporary" <<'PY'
import json, sys
from datetime import datetime, timezone
print(json.dumps({
    "schema": "uip.recovery-status/v1",
    "at": datetime.now(timezone.utc).isoformat(timespec="seconds").replace("+00:00", "Z"),
    "service": sys.argv[3],
    "status": sys.argv[2],
    "migration": sys.argv[5],
    "message": sys.argv[4],
    "verified": sys.argv[2] == "recovered",
}, ensure_ascii=False, sort_keys=True))
PY
  then
    chmod 600 "$temporary" && mv -f -- "$temporary" "$output" || rm -f -- "$temporary"
  else
    rm -f -- "$temporary"
    echo '警告：无法写入恢复结果记录；请保留现场并人工核对。' >&2
  fi
}

rollback_runtime() {
  rollback_skipped=false
  local -a restored_images=()
  local restored_image_key restored_image
  case "$service" in
    frontend) restored_images+=("$(release_image_value FRONTEND_IMAGE)") ;;
    platform)
      restored_images+=("$(release_image_value PLATFORM_IMAGE)")
      restored_images+=("$(release_image_value FILE_GATEWAY_IMAGE)")
      ;;
    contract) restored_images+=("$(release_image_value CONTRACT_IMAGE)") ;;
    project) restored_images+=("$(release_image_value PROJECT_IMAGE)") ;;
    reimbursement) restored_images+=("$(release_image_value REIMBURSEMENT_IMAGE)") ;;
    settlement) restored_images+=("$(release_image_value SETTLEMENT_IMAGE)") ;;
    data-analysis)
      for restored_image_key in \
        DATA_ANALYSIS_DASHBOARD_API_IMAGE \
        DATA_ANALYSIS_AGGREGATION_WORKER_IMAGE \
        DATA_ANALYSIS_ALERT_WORKER_IMAGE; do
        restored_images+=("$(release_image_value "$restored_image_key")")
      done
      ;;
  esac
  for restored_image in "${restored_images[@]}"; do
    if ! rollback_ref_is_immutable "$restored_image"; then
      echo "上一发布配置没有可回滚的不可变镜像（${restored_image:-空值}）；这是首次安装或旧配置占位值，保留已健康的基础设施并跳过运行态回滚" >&2
      rollback_skipped=true
      return 0
    fi
  done

  case "$service" in
    frontend) frontend_compose up -d --force-recreate --no-deps frontend ;;
    platform)
      local restored_platform_image restored_gateway_image provisioner_container
      prepare_managed_public_proxy_image || return 1
      restored_platform_image="${restored_images[0]}"
      restored_gateway_image="${restored_images[1]}"
      compose run --rm --no-deps subsystem-provisioner-socket-init || return
      compose up -d --wait --wait-timeout 60 docker-socket-proxy || return
      # 失败可能发生在基础设施启动阶段；先确保平台数据库恢复，再启动 Agent，
      # 最后恢复 API/Worker。回滚不得让 API 在数据库仍停止时进入重启循环。
      compose up -d --wait --wait-timeout 180 --no-deps platform-mysql || true
      compose up -d --force-recreate --wait --wait-timeout 180 --no-deps file-gateway || return 1
      if ! compose up -d --force-recreate --wait --wait-timeout 60 --no-deps subsystem-provisioner; then
        echo "回滚时 subsystem-provisioner 重建失败，继续尝试重建 platform-api" >&2
        dump_subsystem_provisioner_debug
        return 1
      fi
      provisioner_container="$(compose ps -q subsystem-provisioner)"
      verify_subsystem_provisioner_config_consistency "$provisioner_container" || return 1
      compose up -d --force-recreate --no-deps --wait --wait-timeout 120 platform-api platform-worker || return 1
      wait_for_health "http://127.0.0.1:$(port_value PLATFORM_API_PORT 18080)/readyz" || return 1
      verify_service_image compose file-gateway "$restored_gateway_image" || return 1
      verify_service_image compose platform-api "$restored_platform_image" || return 1
      verify_service_image compose platform-worker "$restored_platform_image" || return 1
      verify_service_stable platform-worker || return 1
      ;;
    contract)
      local restored_contract_image selected_contract_runtime contract_runtime_service
      local -a restored_contract_services=()
      restored_contract_image="${restored_images[0]}"
      selected_contract_runtime="$(scope_runtime_services contract)" || return 1
      while read -r contract_runtime_service; do
        [[ -n "$contract_runtime_service" ]] && restored_contract_services+=("$contract_runtime_service")
      done <<< "$selected_contract_runtime"
      ((${#restored_contract_services[@]} > 0)) || return 1
      compose up -d --force-recreate --no-deps --wait --wait-timeout 120 "${restored_contract_services[@]}" || return 1
      for contract_runtime_service in "${restored_contract_services[@]}"; do
        verify_service_image compose "$contract_runtime_service" "$restored_contract_image" || return 1
        verify_service_stable "$contract_runtime_service" || return 1
      done
      ;;
    project)
      local restored_project_image
      restored_project_image="${restored_images[0]}"
      compose up -d --force-recreate --no-deps --wait --wait-timeout 120 project-api project-sla-notifier || return 1
      wait_for_health "http://127.0.0.1:$(port_value PROJECT_API_PORT 18085)/healthz" || return 1
      verify_service_image compose project-api "$restored_project_image" || return 1
      verify_service_image compose project-sla-notifier "$restored_project_image" || return 1
      project_background_activate || return 1
      ;;
    reimbursement) compose up -d --force-recreate --no-deps reimbursement-api reimbursement-worker ;;
    settlement) compose up -d --force-recreate --no-deps settlement-api settlement-worker ;;
    data-analysis)
      compose up -d --force-recreate --no-deps \
        data-analysis-api data-analysis-aggregation-worker data-analysis-alert-worker
      ;;
  esac
}

# 拒绝旧项目编排必须早于候选指针提交。此时还没有后台生命周期操作，
# 不能依赖失败后的 drain/rollback 来恢复一个尚未 prepare 的操作。
if [[ "$service" == project ]]; then
  _project_background_preflight || exit 1
fi

require_backup_space || exit 1
restore_legacy_file_gateway_pointer || exit 1
release_id="$(date -u +%Y%m%dT%H%M%SZ)"
previous_release="$(mktemp "$deploy_dir/.release.env.previous.XXXXXX")"
next_release="$(mktemp "$deploy_dir/.release.env.next.XXXXXX")"
chmod 600 "$previous_release" "$next_release"
# 同一文件系统内使用硬链接保存旧版本指针，不额外消耗数据块；磁盘接近满时仍可原子回退。
rm -f "$previous_release"
ln "$release_file" "$previous_release"
# 每次发布保留旧版本指针快照，但不复制包含运行密钥的 .env。
ln "$release_file" "$deploy_dir/backups/releases/${release_id}.env"

if [[ "$service" == "data-analysis" ]]; then
  awk -F= \
    -v k1="${data_analysis_image_keys[0]}" -v v1="${data_analysis_image_refs[0]}" \
    -v k2="${data_analysis_image_keys[1]}" -v v2="${data_analysis_image_refs[1]}" \
    -v k3="${data_analysis_image_keys[2]}" -v v3="${data_analysis_image_refs[2]}" \
    -v k4="${data_analysis_image_keys[3]}" -v v4="${data_analysis_image_refs[3]}" '
      BEGIN { found1 = found2 = found3 = found4 = 0 }
      $1 == k1 { print k1 "=" v1; found1 = 1; next }
      $1 == k2 { print k2 "=" v2; found2 = 1; next }
      $1 == k3 { print k3 "=" v3; found3 = 1; next }
      $1 == k4 { print k4 "=" v4; found4 = 1; next }
      { print }
      END {
        if (!found1) print k1 "=" v1
        if (!found2) print k2 "=" v2
        if (!found3) print k3 "=" v3
        if (!found4) print k4 "=" v4
      }
    ' "$release_file" >"$next_release"
elif [[ "$service" == "platform" ]]; then
  awk -F= -v k1="$image_key" -v v1="$image_ref" -v k2="$file_gateway_image_key" -v v2="$file_gateway_image_ref" \
    -v k3="$docker_socket_proxy_image_key" -v v3="$docker_socket_proxy_image_ref" '
    BEGIN { found1 = found2 = found3 = 0 }
    $1 == k1 { print k1 "=" v1; found1 = 1; next }
    $1 == k2 { print k2 "=" v2; found2 = 1; next }
    $1 == k3 && v3 != "" { print k3 "=" v3; found3 = 1; next }
    { print }
    END {
      if (!found1) print k1 "=" v1
      if (!found2) print k2 "=" v2
      if (v3 != "" && !found3) print k3 "=" v3
    }
  ' "$release_file" >"$next_release"
else
  awk -F= -v key="$image_key" -v value="$image_ref" '
    BEGIN { found = 0 }
    $1 == key {
      print key "=" value
      found = 1
      next
    }
    { print }
    END {
      if (!found) print key "=" value
    }
  ' "$release_file" >"$next_release"
fi
# 故障恢复时可能由 root 调用 deploy-service。必须保留 .release.env 归属部署账号，供后续 CI 部署更新。
if [[ "$service" == project ]]; then
  project_build="$(project_background_build_id "$image_ref")" || exit 1
  env_set "$next_release" PROJECT_BACKGROUND_BUILD_ID "$project_build" || exit 1
fi
if [[ "$(id -u)" -eq 0 ]]; then
  chown --reference="$release_file" "$next_release"
fi
chmod 600 "$next_release"
mv "$next_release" "$release_file"

if [[ "$service" == "data-analysis" ]]; then
  for data_analysis_image in "${data_analysis_image_refs[@]}"; do
    echo "拉取不可变数据看板镜像：$data_analysis_image"
    if ! docker pull "$data_analysis_image"; then
      mv -f "$previous_release" "$release_file"
      rm -f "$previous_release"
      echo "镜像拉取失败，发布配置已恢复" >&2
      exit 1
    fi
  done
elif [[ "$service" == "platform" ]]; then
  for platform_image in "${platform_image_refs[@]}"; do
    echo "拉取不可变平台镜像：$platform_image"
    if ! docker pull "$platform_image"; then
      mv -f "$previous_release" "$release_file"
      rm -f "$previous_release"
      echo "镜像拉取失败，发布配置已恢复" >&2
      exit 1
    fi
  done
else
  echo "拉取不可变镜像：$image_ref"
  if ! docker pull "$image_ref"; then
    mv -f "$previous_release" "$release_file"
    rm -f "$previous_release"
    echo "镜像拉取失败，发布配置已恢复" >&2
    exit 1
  fi
fi
if [[ "$service" == "frontend" ]]; then
  compose_config_ok=false
  frontend_compose config --quiet && compose_config_ok=true
else
  compose_config_ok=false
  compose config --quiet && compose_config_ok=true
fi
if [[ "$compose_config_ok" != "true" ]]; then
  mv -f "$previous_release" "$release_file"
  rm -f "$previous_release"
  echo "Compose 校验失败，发布配置已恢复" >&2
  exit 1
fi

# .release.env 是 Agent 的单文件 bind mount。原子提交后必须重建容器并比较
# 宿主/容器内摘要；仅 docker restart 不会重新绑定已经被替换的 inode。
if [[ "$service" != platform ]] && ! refresh_subsystem_provisioner_config; then
  mv -f "$previous_release" "$release_file"
  refresh_subsystem_provisioner_config || true
  echo 'Agent 配置刷新失败，发布配置已恢复' >&2
  exit 1
fi

# 首次上线时子系统镜像会先于浏览器接入发布。此时 OIDC 与机器凭据尚未生成，不能启动
# 子系统 API，但必须保留不可变 digest，供生产 Agent 在页面接入时迁移并启动。
if [[ "$service" == "contract" ]] && ! contract_runtime_ready; then
  if [[ "$fail_if_runtime_not_ready" == "true" ]]; then
    mv -f "$previous_release" "$release_file"
    refresh_subsystem_provisioner_config || true
    echo "合同管理运行配置未完成，拒绝将仅暂存报告为发布成功" >&2
    echo "请先在基础平台重新发布 contract_management/prod，补齐 CRM 与项目集成机器凭据后再运行 CI/CD" >&2
    exit 1
  fi
  rm -f "$previous_release"
  echo "合同镜像已安全暂存：$image_ref"
  echo "运行凭据尚未生成，请登录基础平台的“应用接入”页面完成 contract_management/prod 接入。"
  exit 0
fi
if [[ "$service" == "project" ]] && ! project_runtime_ready; then
  if [[ "$fail_if_runtime_not_ready" == "true" ]]; then
    mv -f "$previous_release" "$release_file"
    refresh_subsystem_provisioner_config || true
    echo "项目管理运行配置未完成，拒绝将仅暂存报告为发布成功" >&2
    echo "请先登录基础平台的“应用接入”页面完成 project_management/prod 接入，再重新运行 CI/CD" >&2
    exit 1
  fi
  rm -f "$previous_release"
  echo "项目管理镜像已安全暂存：$image_ref"
  echo "运行凭据尚未生成，请登录基础平台的“应用接入”页面完成 project_management/prod 接入。"
  exit 0
fi
if [[ "$service" == "reimbursement" ]] && ! reimbursement_runtime_ready; then
  if [[ "$fail_if_runtime_not_ready" == "true" ]]; then
    mv -f "$previous_release" "$release_file"
    refresh_subsystem_provisioner_config || true
    echo "报销运行配置未完成，拒绝将仅暂存报告为发布成功" >&2
    echo "请先登录基础平台的“应用接入”页面完成 reimbursement/prod 接入，再重新发布" >&2
    exit 1
  fi
  rm -f "$previous_release"
  echo "报销镜像已安全暂存：$image_ref"
  echo "运行凭据尚未生成，请登录基础平台的“应用接入”页面完成 reimbursement/prod 接入。"
  exit 0
fi
if [[ "$service" == "settlement" ]] && ! settlement_runtime_ready; then
  if [[ "$fail_if_runtime_not_ready" == "true" ]]; then
    mv -f "$previous_release" "$release_file"
    refresh_subsystem_provisioner_config || true
    echo "结算运行配置未完成，拒绝将仅暂存报告为发布成功" >&2
    echo "请先登录基础平台的“应用接入”页面完成 settlement/prod 接入，再重新运行 CI/CD" >&2
    exit 1
  fi
  rm -f "$previous_release"
  echo "结算镜像已安全暂存：$image_ref"
  echo "运行凭据尚未生成，请登录基础平台的“应用接入”页面完成 settlement/prod 接入。"
  exit 0
fi
if [[ "$service" == "data-analysis" ]] && ! data_analysis_runtime_ready; then
  rm -f "$previous_release"
  echo "数据看板镜像已安全暂存：${data_analysis_image_refs[*]}"
  echo "运行凭据尚未生成，请完成 data_analysis/prod 接入并补齐 dashboard 数据库凭据。"
  exit 0
fi

echo "开始发布 $service"
# 服务标识允许使用 data-analysis 这类连字符名称，但 Bash 函数名只能使用
# 标识符字符；统一转换后再动态调用，避免执行 deploy_data-analysis 这样的外部命令。
deploy_function="deploy_${service//-/_}"
if "$deploy_function"; then
  rm -f "$previous_release" "$deploy_dir/runtime/recovery-status.json" 2>/dev/null || true
  echo "$service 发布成功：$image_ref"
  exit 0
fi

echo "发布失败，恢复上一镜像；已执行的数据库迁移不会反向回滚" >&2
if [[ "$service" == project ]] && ! project_background_rollback; then
  echo '新后台未能证明排空，禁止先恢复镜像指针；保留当前进程、候选及操作状态。' >&2
  record_recovery_result failed '新后台未能证明排空，未恢复镜像指针'
  exit 1
fi
# 应用镜像可回退，数据库迁移不可自动逆转；迁移必须保持前后版本兼容或由人工执行恢复方案。
mv -f "$previous_release" "$release_file"
rm -f "$previous_release"
if [[ "$service" != platform ]]; then
  if ! refresh_subsystem_provisioner_config; then
    recovery_config_failed=true
    echo '错误：发布回滚后 Agent 配置一致性核验失败，请暂停接入操作并人工处理' >&2
  fi
fi
if [[ "$service" == "data-analysis" ]]; then
  # 必须在 rollback_runtime 重建容器之前恢复：否则旧镜像会带着新哈希 fail-closed 启动。
  if ! restore_data_analysis_role_config_hash "$data_analysis_runtime_file"; then
    recovery_config_failed=true
    echo '错误：数据看板授权目录哈希恢复失败，请人工核对 runtime/data-analysis.env 后再重建容器' >&2
  fi
fi
if rollback_runtime; then
  if [[ "$recovery_config_failed" == true ]]; then
    record_recovery_result failed '上一镜像已重建，但 Agent/配置一致性核验失败'
  elif [[ "$rollback_skipped" == true ]]; then
    record_recovery_result not_applicable '首次安装或无可回滚的不可变镜像，未执行运行态回滚'
  else
    record_recovery_result recovered '上一镜像已恢复并通过镜像和健康核验'
  fi
else
  record_recovery_result failed '上一镜像恢复未通过镜像/健康/配置核验，请保留现场人工处理'
fi
exit 1
