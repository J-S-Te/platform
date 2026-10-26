# Compose 命令包装、子命令解析与本地镜像映射查询（仅函数定义，加载零副作用）。

compose() {
    # macOS 自带 Bash 3.2 在 `set -u` 下展开空数组会报 unbound variable，
    # 因此不使用可选数组，而是按是否接入 Portal 显式分支。
    # customer_portal/dev 接入成功后，普通 up/down/stop/ps/logs/config 自动纳入
    # portal-api。首次接入前不能强行启动，因为此时浏览器 OIDC Client、租户、
    # 角色目录和六组机器凭据尚不存在；平台与 CRM 需先运行以完成受控接入。
    local compose_files=(--file "$compose_file")
    # 已注册开发消费者保留已审核镜像和独立运行环境；构建只准备新候选，只有受控重试或更新才会接管。
    local local_license_override="${project_root}/runtime/local-license.compose.json"
    # 按实际子命令（而非整段参数字串）判断是否为构建：构建只准备候选镜像，
    # 运行镜像由受控更新写入的固定记录决定，构建阶段不得加载固定记录。
    local compose_subcommand="" compose_arg="" compose_skip_value=false
    for compose_arg in "$@"; do
        if [[ "$compose_skip_value" == true ]]; then
            compose_skip_value=false
            continue
        fi
        case "$compose_arg" in
            --profile|--env-file|--file|-f|--project-name|--project-directory|--ansi|--parallel|--progress)
                compose_skip_value=true
                ;;
            -*) ;;
            *)
                compose_subcommand="$compose_arg"
                break
                ;;
        esac
    done
    case "$compose_subcommand" in
        build) ;;
        *)
            # COMPOSE_SKIP_LICENSE_OVERRIDE=1 仅用于两类不产生运行进程的调用：
            #   只读镜像存在性查询；探测候选创建（--no-start，容器不启动）。
            # 任何会启动或运行服务的路径都必须加载固定记录。
            if [[ "${COMPOSE_SKIP_LICENSE_OVERRIDE:-}" != "1" ]] && [[ -e "$local_license_override" || -L "$local_license_override" ]]; then
                [[ -f "$local_license_override" && ! -L "$local_license_override" ]] || fail "本地授权 Compose 固定文件不安全"
                compose_files+=(--file "$local_license_override")
            fi
            ;;
    esac
    if [[ -n "${PUBLIC_TRANSPORT_COMPOSE_FILE:-}" ]]; then
        compose_files+=(--file "$PUBLIC_TRANSPORT_COMPOSE_FILE")
    fi
    data_analysis_profile_arg=""
    if data_analysis_configured; then
        data_analysis_profile_arg="--profile data-analysis"
    fi
    if portal_configured && project_configured; then
        docker compose \
            --project-name "$compose_project" \
            "${compose_files[@]}" \
            --env-file "$env_file" \
            --env-file "$contract_env_file" \
            --env-file "$customer_env_file" \
            --env-file "$portal_env_file" \
            --env-file "$project_env_file" \
            --env-file "$data_analysis_env_file" \
            --env-file "$settlement_env_file" \
            --profile portal \
            --profile project \
            --profile presale-worker \
            --profile keycloak \
            $data_analysis_profile_arg \
            "$@"
        return
    fi
    if portal_configured; then
        docker compose \
            --project-name "$compose_project" \
            "${compose_files[@]}" \
            --env-file "$env_file" \
            --env-file "$contract_env_file" \
            --env-file "$customer_env_file" \
            --env-file "$portal_env_file" \
            --env-file "$project_env_file" \
            --env-file "$data_analysis_env_file" \
            --env-file "$settlement_env_file" \
            --profile portal \
            --profile presale-worker \
            --profile keycloak \
            $data_analysis_profile_arg \
            "$@"
        return
    fi
    if project_configured; then
        docker compose \
            --project-name "$compose_project" \
            "${compose_files[@]}" \
            --env-file "$env_file" \
            --env-file "$contract_env_file" \
            --env-file "$customer_env_file" \
            --env-file "$portal_env_file" \
            --env-file "$project_env_file" \
            --env-file "$data_analysis_env_file" \
            --env-file "$settlement_env_file" \
            --profile project \
            --profile presale-worker \
            --profile keycloak \
            $data_analysis_profile_arg \
            "$@"
        return
    fi
    docker compose \
        --project-name "$compose_project" \
        "${compose_files[@]}" \
        --env-file "$env_file" \
        --env-file "$contract_env_file" \
        --env-file "$customer_env_file" \
        --env-file "$portal_env_file" \
        --env-file "$project_env_file" \
        --env-file "$data_analysis_env_file" \
        --env-file "$settlement_env_file" \
        --profile presale-worker \
        --profile keycloak \
        $data_analysis_profile_arg \
        "$@"
}

compose_run() {
	sync_local_subsystem_environment_exports
    compose --ansi never "$@"
}

# Compose --wait 可能在冷启动尚可恢复时立即因 unhealthy 返回。这里做一次有界重试；
# 仍失败则输出服务状态与日志，避免只留下无法定位的 dependency failed to start。
compose_up_wait() {
    local description="$1"
    shift
    local attempt recreate_flag=""

    if [[ "$force_build" == true ]]; then
        recreate_flag="--force-recreate"
    fi

    for attempt in 1 2; do
        if compose_run up -d --wait $recreate_flag "$@"; then
            return 0
        fi
        if [[ "$attempt" -eq 1 ]]; then
            log "${description}仍在冷启动，首次健康等待未完成；输出当前状态并在 5 秒后重试"
            compose_run ps "$@" >&2 || true
            sleep 5
            continue
        fi
        log "ERROR: ${description}启动失败，以下为容器状态和最近日志"
        compose_run ps -a "$@" >&2 || true
        compose_run logs --no-color --tail=200 "$@" >&2 || true
        return 1
    done
}

# 只读：列出 Compose 解析出的默认本地镜像标签（不加载授权固定文件），供存在性判断使用。
# 仅查询，不启动、不构建、不写文件。
compose_default_local_images() {
    COMPOSE_SKIP_LICENSE_OVERRIDE=1 compose \
        --profile bootstrap --profile customer --profile portal --profile project \
        --profile presale-worker --profile keycloak --ansi never config --format json 2>/dev/null \
        | python3 -c '
import json
import sys
try:
    model = json.load(sys.stdin)
except Exception:
    sys.exit(1)
for name, spec in sorted((model.get("services") or {}).items()):
    image = spec.get("image") if isinstance(spec, dict) else None
    if isinstance(image, str) and image:
        sys.stdout.write(name + "\t" + image + "\n")
'
}
