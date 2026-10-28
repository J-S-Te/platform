#!/usr/bin/env bash
# 共享兼容单元和全发布输入指纹的唯一事实来源。调用方应在构建工作站执行，绝不在部署环境执行。

offline_platform_build_input_paths() {
  find platform/cmd platform/internal platform/migrations -type f -print0
  printf '%s\0' platform/Dockerfile platform/.dockerignore platform/go.mod platform/go.sum \
    platform/docker-entrypoint.sh platform/scripts/sync-contract-catalog.sh \
    platform/scripts/sync-settlement-catalog.sh platform/scripts/sync-project-catalog.sh
  offline_license_build_input_paths platform
}

offline_license_build_input_paths() {
  local component="$1"
  if [[ -d "$component/third_party/license-core" ]]; then
    find "$component/third_party/license-core" -type f -print0
    printf '%s\0' "$component/scripts/license-core-sync.sh" "$component/scripts/license-core.sha256"
  fi
}

offline_frontend_build_input_paths() {
  find frontend/src frontend/public frontend/nginx -type f -print0
  printf '%s\0' frontend/Dockerfile frontend/.dockerignore frontend/package.json frontend/package-lock.json \
    frontend/index.html frontend/login.html frontend/vite.config.js
}

offline_build_input_paths() {
  local mode="$1" component
  offline_platform_build_input_paths
  offline_frontend_build_input_paths
  case "$mode" in
    shared)
      find platform/deploy/production/bin platform/deploy/production/monitoring \
        platform/deploy/production/mysql-init platform/deploy/production/nginx \
        platform/deploy/production/subsystem-templates platform/deploy/production/subsystems.d \
        -type f ! -path '*/__pycache__/*' ! -path '*/.mimosa/*' ! -name '*.pyc' -print0
      printf '%s\0' platform/deploy/production/.env.example \
        platform/deploy/production/.release.env.example platform/deploy/production/docker-compose.yml
      ;;
    full)
      for component in customer_and_opportunity contract_management project_management Settlement data_analysis; do
        find "$component/cmd" "$component/internal" "$component/migrations" -type f -print0
        printf '%s\0' "$component/Dockerfile" "$component/.dockerignore" "$component/go.mod" "$component/go.sum"
        offline_license_build_input_paths "$component"
      done
      find contract_management/authz project_management/authz Settlement/authz -type f -print0
      printf '%s\0' contract_management/docker-entrypoint.sh
      # 门户将已审核字体资产复制到镜像内并验证。
      if [[ -d customer_and_opportunity/assets/fonts ]]; then
        find customer_and_opportunity/assets/fonts -type f -print0
      fi
      find platform/deploy/production -type f \
        ! -path '*/runtime/*' ! -path '*/backups/*' ! -path '*/packages/*' ! -path '*/manifests/*' \
        ! -path '*/__pycache__/*' ! -path '*/.mimosa/*' ! -name '*.pyc' \
        ! -name '.env' ! -name '.release.env' ! -name '*.pem' ! -name '*.key' \
        ! -name '*.bak' ! -name '*.backup' ! -name '*.dump' ! -name 'dump*.sql' -print0
      ;;
    *) echo '未知离线构建输入范围' >&2; return 2 ;;
  esac
}

offline_build_input_fingerprint() (
  local workspace="$1" mode="$2" input
  cd -- "$workspace" || exit 1
  offline_build_input_paths "$mode" | LC_ALL=C sort -zu |
    while IFS= read -r -d '' input; do
      [[ -f "$input" && ! -L "$input" ]] || continue
      printf '%s  %s\0' "$(sha256sum "$input" | awk '{print tolower($1)}')" "$input"
    done | sha256sum | awk '{print tolower($1)}'
)

# offline_build_input_index 为发布构建输入写入确定性的逐文件快照，记录实际工作区字节，包括未提交改动，
# 而非可能无法代表本次构建的 Git 提交。
offline_build_input_index() (
  local workspace="$1" mode="$2" output="$3" input size digest temporary
  cd -- "$workspace" || exit 1
  [[ -n "$output" && "$output" == /* ]] || { echo '源码索引输出必须是绝对路径' >&2; exit 1; }
  [[ ! -L "$output" ]] || { echo '源码索引输出不能是符号链接' >&2; exit 1; }
  temporary="$(mktemp "$(dirname -- "$output")/.source-input-index.XXXXXX")" || exit 1
  trap 'rm -f -- "$temporary"' EXIT
  offline_build_input_paths "$mode" | LC_ALL=C sort -zu |
    while IFS= read -r -d '' input; do
      [[ -f "$input" && ! -L "$input" ]] || continue
      [[ "$input" != *$'\t'* && "$input" != *$'\n'* ]] || {
        echo "源码路径包含禁止字符：$input" >&2; exit 1;
      }
      size="$(wc -c < "$input" | tr -d ' ')"
      digest="$(sha256sum "$input" | awk '{print tolower($1)}')"
      [[ "$size" =~ ^[0-9]+$ && "$digest" =~ ^[0-9a-f]{64}$ ]] || {
        echo "无法生成源码索引：$input" >&2; exit 1;
      }
      printf '%s\t%s\t%s\n' "$digest" "$size" "$input"
    done > "$temporary"
  chmod 600 "$temporary"
  mv -f -- "$temporary" "$output"
  trap - EXIT
)

# Keep formal preflight and actual common builds on the same image list.
offline_common_base_images() {
  printf '%s\n' mysql:8.4 quay.io/keycloak/keycloak:26.2 temporalio/auto-setup:1.29.7 \
    metabase/metabase:v0.53.7 prom/prometheus:v3.5.0 prom/node-exporter:v1.9.1 \
    registry:2.8.3 tecnativa/docker-socket-proxy:v0.5.0
}
