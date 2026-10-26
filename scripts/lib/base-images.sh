# Dockerfile 基础镜像动态收集、串行拉取与按用途预准备（仅函数定义，加载零副作用）。

collect_dockerfile_base_images() {
    # 动态收集各子系统 Dockerfile 的 FROM 基础镜像，替代手工清单：基础镜像升级后
    # 手工清单必然漂移（合同子系统升到 golang:1.27.1 而清单仍是 1.26.4，重建时
    # 现场拉取遇上 Docker Hub 不可达即部署失败）。多阶段引用（FROM runtime-base）
    # 与 scratch 不含 .:/ 分隔符，据此过滤，只保留真实镜像引用。
    local dockerfile
    local images=()
    local line
    while IFS= read -r dockerfile; do
        while IFS= read -r line; do
            [[ -n "$line" ]] && images+=("$line")
        done < <(grep -E "^FROM " "$dockerfile" | awk '{print $2}' | grep -E '[.:/]' || true)
    done < <(find "$workspace_root" -maxdepth 3 -name "Dockerfile" -type f 2>/dev/null)
    printf '%s\n' "${images[@]}" | sort -u
}

pull_image_with_retry() {
    local image="$1" max_attempts="${2:-5}" attempt delay
    if docker image inspect "$image" >/dev/null 2>&1; then
        log "基础镜像已存在，跳过拉取：$image"
        return 0
    fi
    for ((attempt = 1; attempt <= max_attempts; attempt++)); do
        log "串行拉取基础镜像（${attempt}/${max_attempts}）：$image"
        if docker pull "$image"; then return 0; fi
        if ((attempt < max_attempts)); then
            delay=$((attempt * 5))
            log "基础镜像拉取失败，${delay} 秒后重试：$image"
            sleep "$delay"
        fi
    done
    fail "基础镜像拉取失败：${image}。Docker Hub 认证端点不可达；请检查 Docker Desktop 的 Proxies/镜像加速器配置后重试。"
}

prepare_base_images() {
    local image
    local images=()
    while IFS= read -r image; do
        [[ -n "$image" ]] && images+=("$image")
    done < <(collect_dockerfile_base_images)
    # 编排在 compose image: 字段直接引用、不经任何 Dockerfile 构建的基础镜像。
    images+=("mysql:8.4" "quay.io/keycloak/keycloak:26.2" "temporalio/auto-setup:1.29.7")
    log "检查并串行准备基础镜像"
    for image in "${images[@]}"; do pull_image_with_retry "$image"; done
}

prepare_go_backend_base_images() {
    local target_name="${1:-Go 后端}"
    local image
    log "检查并串行准备${target_name}所需基础镜像"
    while IFS= read -r image; do
        [[ -n "$image" ]] || continue
        case "$image" in
            golang:*|alpine:*) pull_image_with_retry "$image" ;;
        esac
    done < <(collect_dockerfile_base_images)
}

prepare_frontend_base_images() {
    local image
    log "检查并串行准备统一前端所需基础镜像"
    while IFS= read -r image; do
        [[ -n "$image" ]] || continue
        case "$image" in
            node:*|nginx:*) pull_image_with_retry "$image" ;;
        esac
    done < <(collect_dockerfile_base_images)
}
