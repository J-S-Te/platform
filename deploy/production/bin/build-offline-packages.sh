#!/usr/bin/env bash
# =============================================================================
# 离线镜像与部署资产打包脚本（构建工作站使用）
#
#   运行位置：仓库内，platform/deploy/production/bin/。
#             它依赖同级的 offline-base.Dockerfile、上一级的部署资产目录，
#             以及工作区中各子系统源码；交付目录顶层的“参考副本”不能就地运行。
#   前置条件：Docker Engine（含 buildx）；能拉取公共基础镜像。
#   典型用法：./bin/build-offline-packages.sh --component all
#   产出：    <输出目录>/<组件>-<版本>-linux-amd64.tar.gz   各组件镜像包
#             <输出目录>/host-dependencies-*.tar.gz       Ubuntu/amd64 宿主依赖
#             <输出目录>/deployment-assets-<版本>.tar.gz   部署脚本与配置资产
#             <输出目录>/SHA256SUMS                        覆盖输出目录全部 *.tar.gz
#   打包后的分发与部署步骤见同目录 OFFLINE_DEPLOYMENT.md。
# =============================================================================
set -Eeuo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
deploy_dir="$(cd -- "$script_dir/.." && pwd)"
workspace_root="$(cd -- "$deploy_dir/../../.." && pwd)"
platform="linux/amd64"
version="${OFFLINE_RELEASE_VERSION:-$(date -u +%Y%m%dT%H%M%SZ)}"
output_root="${OFFLINE_OUTPUT_DIR:-$workspace_root/.artifacts/offline/$version}"
output_explicit=false
component="all"
license_file=''
license_snapshot=''
license_digest=''
runtime_approval_dir=''
runtime_approval_approved=false
reuse_approved_crm_image=false
runtime_approval_digest=''
installation_scenario=''
installation_customer=''
installation_digest=''
managed_proxy_upstream='ghcr.io/tecnativa/docker-socket-proxy:v0.5.0@sha256:1f5038b54f06c3e18422902cf00ba21803d1c97805aae032e5e6673d532d3459'
managed_proxy_transport='tecnativa/docker-socket-proxy:v0.5.0'
managed_proxy_native_id=''
runtime_approval_files=(
  license-evidence.json
  license-migration-evidence.json
  runtime-license-contract_management-prod.json
  runtime-license-customer_and_opportunity-prod.json
  runtime-license-customer_portal-prod.json
  runtime-license-project_management-prod.json
  runtime-license-settlement-prod.json
  runtime-license-data_analysis-prod.json
)
build_goproxy="${OFFLINE_GOPROXY:-https://goproxy.cn|https://proxy.golang.org|direct}"
build_gosumdb="${OFFLINE_GOSUMDB:-sum.golang.google.cn}"
build_npm_registry="${OFFLINE_NPM_CONFIG_REGISTRY:-https://registry.npmjs.org}"
pull_retries="${OFFLINE_PULL_RETRIES:-3}"
pull_retry_delay="${OFFLINE_PULL_RETRY_DELAY_SECONDS:-5}"

usage() {
  cat <<'EOF'
用法：build-offline-packages.sh [--component 组件名|all] [--version 版本] [--output 输出目录]
  [--license-file 客户许可证.jws --runtime-approval-dir 批准目录 --runtime-approval-approved]
  [--reuse-approved-crm-image] 复用批准目录绑定的本机CRM镜像，避免重建改变ID
  [--installation-scenario fresh|migrate|platform-only|expand --customer-id 客户标识]

可用组件：
  common                公共基础设施（含独立文件上传网关镜像）
  platform              基础平台后端
  frontend              统一前端
  customer-opportunity  客户与商机管理系统后端
  customer-portal       客户自助门户后端
  contract              合同管理系统后端
  project               项目服务内容管理系统后端
  settlement            结算与开票管理系统后端
  data-analysis         数据看板与统计分析系统后端
  host-deps             Ubuntu/amd64 宿主机离线依赖（jq/curl/tar 等）
  assets                服务器部署脚本和配置资产
  all                   构建全部组件（默认）

说明：--component 默认为 all。指定单个组件时只构建该组件（assets 只生成部署资产）；
每次运行都会基于输出目录中现有的 *.tar.gz 重写总校验文件 SHA256SUMS。
--license-file 使用平台内置厂商公钥验证签名，随 assets/all 部署资产交付。
增量构建同一客户版本时每次传入同一许可证；不同客户或续签请使用新输出目录。
许可证采用固定起止日期，部署或重启不重新计算授权期限。厂商私钥不得放入部署包。
客户许可证必须同时提供已人工审核的运行组件批准目录及 --runtime-approval-approved。
该开关表示厂商已完成审核，不会将工具生成的候选文件自动改写为批准结果。
仅交付固定 prod 运行批准 JSON 和 license-evidence.json，未知文件与私钥不打包。
四场景交付需明确指定 --installation-scenario，禁止按包数量猜测存量。
--customer-id 可从签名许可证读取；无许可证时必须填写，并提供已批准目录。
批准目录的 license-migration-evidence.json 描述升级前实际旧镜像，而非目标镜像。

可选环境变量（只用于构建依赖下载，不写入 package.env）：
  OFFLINE_GOPROXY                   Go module 代理链
  OFFLINE_GOSUMDB                   Go checksum database
  OFFLINE_NPM_CONFIG_REGISTRY       npm registry
  OFFLINE_PULL_RETRIES              公共镜像拉取次数，默认 3，范围 1-10
  OFFLINE_PULL_RETRY_DELAY_SECONDS  拉取重试间隔秒数，默认 5，范围 1-60
EOF
}

while (($#)); do
  case "$1" in
    --component) component="${2:?缺少组件名称}"; shift 2 ;;
    --version) version="${2:?缺少版本号}"; shift 2 ;;
    --output) output_root="${2:?缺少输出目录}"; output_explicit=true; shift 2 ;;
    --license-file) license_file="${2:?缺少许可证文件}"; shift 2 ;;
    --runtime-approval-dir) runtime_approval_dir="${2:?缺少批准目录}"; shift 2 ;;
    --runtime-approval-approved) runtime_approval_approved=true; shift ;;
    --reuse-approved-crm-image) reuse_approved_crm_image=true; shift ;;
    --installation-scenario) installation_scenario="${2:?缺少安装场景}"; shift 2 ;;
    --customer-id) installation_customer="${2:?缺少客户标识}"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) usage >&2; exit 2 ;;
  esac
done

if [[ "$output_explicit" == false && -z "${OFFLINE_OUTPUT_DIR:-}" ]]; then
  output_root="$workspace_root/.artifacts/offline/$version"
fi

[[ "$version" =~ ^[A-Za-z0-9._-]+$ ]] || { echo "版本号格式不正确" >&2; exit 2; }
case "$installation_scenario" in ''|fresh|migrate|platform-only|expand) ;; *) echo '安装场景无效' >&2; exit 2 ;; esac
[[ -n "$installation_scenario" || -z "$installation_customer" ]] || { echo '--customer-id 必须与安装场景同时提供' >&2; exit 2; }
[[ "$reuse_approved_crm_image" != true || ( -n "$runtime_approval_dir" && "$runtime_approval_approved" == true ) ]] || { echo '复用CRM镜像需要已审核运行批准' >&2; exit 2; }
[[ -z "$installation_scenario" || -n "$runtime_approval_dir" ]] || { echo '四场景安装必须提供已批准目录' >&2; exit 2; }
case "$component" in common|platform|frontend|customer-opportunity|customer-portal|contract|project|settlement|data-analysis|host-deps|assets|all) ;; *) usage >&2; exit 2 ;; esac
for build_setting in "$build_goproxy" "$build_gosumdb" "$build_npm_registry"; do
  [[ -n "$build_setting" && "$build_setting" != *$'\n'* && "$build_setting" != *$'\r'* ]] || {
    echo '依赖下载地址不能为空或包含换行符' >&2
    exit 2
  }
done
normalized_gosumdb="${build_gosumdb#"${build_gosumdb%%[![:space:]]*}"}"
normalized_gosumdb="${normalized_gosumdb%"${normalized_gosumdb##*[![:space:]]}"}"
normalized_gosumdb_lower="$(printf '%s' "$normalized_gosumdb" | LC_ALL=C tr '[:upper:]' '[:lower:]')"
[[ "$normalized_gosumdb_lower" != off ]] || {
  echo 'OFFLINE_GOSUMDB 禁止设为 off；离线发布构建不得关闭 Go 模块校验' >&2
  exit 2
}
[[ "$pull_retries" =~ ^[0-9]+$ ]] && ((pull_retries >= 1 && pull_retries <= 10)) || {
  echo 'OFFLINE_PULL_RETRIES 必须是 1-10 的整数' >&2; exit 2;
}
[[ "$pull_retry_delay" =~ ^[0-9]+$ ]] && ((pull_retry_delay >= 1 && pull_retry_delay <= 60)) || {
  echo 'OFFLINE_PULL_RETRY_DELAY_SECONDS 必须是 1-60 的整数' >&2; exit 2;
}

# 本脚本必须从仓库的 platform/deploy/production/bin/ 运行：它依赖同级的
# offline-base.Dockerfile、上层的部署资产目录，以及工作区中各子系统的源码。
# 交付目录顶层的“离线镜像构建脚本.sh”只是本脚本的中文副本，不能就地运行。
missing_inputs=()
[[ -f "$script_dir/offline-base.Dockerfile" ]] || missing_inputs+=("$script_dir/offline-base.Dockerfile")
[[ -f "$deploy_dir/docker-compose.yml" ]] || missing_inputs+=("$deploy_dir/docker-compose.yml")
[[ -d "$workspace_root/platform" ]] || missing_inputs+=("$workspace_root/platform")
if ((${#missing_inputs[@]} > 0)); then
  {
    echo '错误：当前脚本不在离线打包工作目录中，无法运行。缺少以下路径：'
    printf '  %s\n' "${missing_inputs[@]}"
    echo '请在仓库中执行 platform/deploy/production/bin/build-offline-packages.sh。'
    echo '交付目录顶层的“离线镜像构建脚本.sh”是本脚本的中文副本，仅供查看和审核。'
  } >&2
  exit 1
fi

if [[ -n "$license_file" && -z "$runtime_approval_dir" ]]; then
  echo '客户授权安装包必须提供 --runtime-approval-dir 和 --runtime-approval-approved' >&2
  exit 2
fi
if [[ "$runtime_approval_approved" == true && -z "$runtime_approval_dir" ]] ||
   [[ -n "$runtime_approval_dir" && "$runtime_approval_approved" != true ]]; then
  echo '运行批准目录必须同时指定 --runtime-approval-dir 和 --runtime-approval-approved；候选不自动批准' >&2
  exit 2
fi
if [[ -n "$license_file" || -n "$runtime_approval_dir" || -n "$installation_scenario" ]]; then
  for command_name in jq sha256sum install mktemp; do
    command -v "$command_name" >/dev/null || { echo "缺少所需命令：$command_name" >&2; exit 1; }
  done
  license_stage="$(mktemp -d "${TMPDIR:-/tmp}/offline-license-input.XXXXXX")"
  trap 'rm -rf -- "$license_stage"' EXIT
fi
if [[ -n "$license_file" ]]; then
  [[ -f "$license_file" && ! -L "$license_file" ]] || {
    echo '客户许可证必须是非符号链接的普通文件' >&2
    exit 2
  }
  command -v go >/dev/null || { echo '验签客户许可证需要 Go 构建平台可信验证器' >&2; exit 1; }
  # 验证前在输出目录之外创建快照：被拒绝的许可证不得改变已发布产物，之后替换源文件也不得改变复制到本发布的已验证客户许可证。
  license_snapshot="$license_stage/commercial-license.jws"
  install -m 600 -- "$license_file" "$license_snapshot"
  if ! license_metadata="$(cd "$workspace_root/platform" && GOWORK=off go run ./cmd/license-package verify --file "$license_snapshot")"; then
    echo '客户许可证可信校验失败；未修改输出目录' >&2
    exit 1
  fi
  license_digest="$(printf '%s' "$license_metadata" | jq -er '.digest | select(test("^[a-f0-9]{64}$"))')" || {
    echo '可信验证器未返回有效许可证摘要' >&2
    exit 1
  }
  [[ "$(sha256sum "$license_snapshot" | awk '{print tolower($1)}')" == "$license_digest" ]] || {
    echo '可信验证器摘要与许可证文件不一致' >&2
    exit 1
  }
  if [[ -n "$installation_scenario" ]]; then
    signed_customer="$(printf '%s' "$license_metadata" | jq -er '.customer_id')"
    [[ -z "$installation_customer" || "$installation_customer" == "$signed_customer" ]] || { echo '客户与签名许可证不匹配' >&2; exit 2; }
    installation_customer="$signed_customer"
  fi
fi

if [[ -n "$installation_scenario" ]]; then
  [[ "$installation_customer" =~ ^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$ ]] || { echo '无许可证的安装需有效 --customer-id' >&2; exit 2; }
  jq -n --arg scenario "$installation_scenario" --arg customer "$installation_customer" '{version:1,scenario:$scenario,customer_id:$customer}' > "$license_stage/license-installation.json"
  installation_digest="$(sha256sum "$license_stage/license-installation.json" | awk '{print tolower($1)}')"
fi

if [[ -n "$runtime_approval_dir" ]]; then
  [[ -d "$runtime_approval_dir" && ! -L "$runtime_approval_dir" ]] || {
    echo '运行批准目录必须是非符号链接目录' >&2; exit 2;
  }
  mkdir -p -- "$license_stage/approvals"
  runtime_approval_count=0
  for approval_name in "${runtime_approval_files[@]}" approval-review.json; do
    approval_input="$runtime_approval_dir/$approval_name"
    if [[ ! -e "$approval_input" && ! -L "$approval_input" ]]; then
      [[ "$approval_name" != license-evidence.json ]] || {
        echo '运行批准目录缺少 license-evidence.json' >&2; exit 2;
      }
      continue
    fi
    [[ -f "$approval_input" && ! -L "$approval_input" ]] || {
      echo "运行批准文件不是安全普通文件：$approval_name" >&2; exit 2;
    }
    [[ "$(wc -c < "$approval_input" | tr -d ' ')" -le 2097152 ]] || {
      echo "运行批准文件超过 2MiB：$approval_name" >&2; exit 2;
    }
    install -m 600 -- "$approval_input" "$license_stage/approvals/$approval_name"
    # 打包只传输已审核 JSON。隔离 Agent 对配置、镜像和覆盖范围执行权威验证，绝不猜测或修复。
    jq -e -s 'length == 1 and (.[0] | type == "object")' "$license_stage/approvals/$approval_name" >/dev/null || {
      echo "运行批准文件必须是单个 JSON 对象：$approval_name" >&2; exit 2;
    }
    if [[ "$approval_name" == runtime-license-* ]]; then
      approval_app="${approval_name#runtime-license-}"
      approval_app="${approval_app%-prod.json}"
      jq -e --arg app "$approval_app" '.application == $app and .environment == "prod"' "$license_stage/approvals/$approval_name" >/dev/null || {
        echo "运行批准文件的系统或应用环境与固定文件名不符：$approval_name" >&2; exit 2;
      }
      runtime_approval_count=$((runtime_approval_count + 1))
    fi
  done
  ((runtime_approval_count > 0)) || [[ -n "$installation_scenario" ]] || { echo '运行批准目录没有任何受支持的 prod 组件批准文件' >&2; exit 2; }
  [[ -z "$installation_scenario" || -f "$license_stage/approvals/license-migration-evidence.json" ]] || { echo '四场景交付缺少升级前安装边界批准 license-migration-evidence.json' >&2; exit 2; }
  if [[ -n "$license_file" ]]; then
    printf '%s' "$license_metadata" | jq -e '.environment == "production"' >/dev/null || {
      echo '生产部署资产只接受 production 安装环境许可证（应用环境为 prod）' >&2; exit 2;
    }
    while IFS= read -r approval_app; do
      case "$approval_app" in contract_management|customer_and_opportunity|customer_portal|project_management|settlement|data_analysis) ;; *) echo '许可证包含不支持的运行批准系统' >&2; exit 2 ;; esac
      [[ -f "$license_stage/approvals/runtime-license-${approval_app}-prod.json" ]] || {
        echo "许可证系统缺少对应运行批准：$approval_app" >&2; exit 2;
      }
    done < <(printf '%s' "$license_metadata" | jq -er '.applications[]')
  fi
  runtime_approval_digest="$(
    for approval_name in "${runtime_approval_files[@]}" approval-review.json; do
      [[ ! -f "$license_stage/approvals/$approval_name" ]] || printf '%s  %s\n' "$(sha256sum "$license_stage/approvals/$approval_name" | awk '{print tolower($1)}')" "$approval_name"
    done | sha256sum | awk '{print tolower($1)}'
  )"
fi

# Containerd 可在同一标签下保留两种架构，即使拉取过 amd64，无限定 inspect 仍会选择守护进程原生架构。
# 平台选择必须同时检查 CLI 支持和有效 API；旧 Docker 保留现有严格单平台检查。
offline_inspect_platform_args=()
offline_save_platform_args=()

offline_docker_api_at_least() {
  local actual="$1" minimum_minor="$2" major minor
  [[ "$actual" =~ ^[0-9]{1,3}\.[0-9]{1,3}$ ]] || return 1
  major="${actual%%.*}"
  minor="${actual#*.}"
  ((10#$major > 1 || (10#$major == 1 && 10#$minor >= minimum_minor)))
}

offline_configure_platform_selection() {
  local client_api server_api inspect_help save_help override_api
  offline_inspect_platform_args=()
  offline_save_platform_args=()
  client_api="$(docker version --format '{{.Client.APIVersion}}' 2>/dev/null)" || client_api=''
  server_api="$(docker version --format '{{.Server.APIVersion}}' 2>/dev/null)" || server_api=''
  override_api="${DOCKER_API_VERSION:-$client_api}"
  if offline_docker_api_at_least "$client_api" 49 &&
     offline_docker_api_at_least "$server_api" 49 &&
     offline_docker_api_at_least "$override_api" 49 &&
     inspect_help="$(docker image inspect --help 2>/dev/null)" &&
     [[ "$inspect_help" == *--platform* ]]; then
    offline_inspect_platform_args=(--platform "$platform")
  fi
  if offline_docker_api_at_least "$client_api" 48 &&
     offline_docker_api_at_least "$server_api" 48 &&
     offline_docker_api_at_least "$override_api" 48 &&
     save_help="$(docker save --help 2>/dev/null)" &&
     [[ "$save_help" == *--platform* ]]; then
    offline_save_platform_args=(--platform "$platform")
  fi
}

offline_image_inspect() {
  docker image inspect ${offline_inspect_platform_args[@]+"${offline_inspect_platform_args[@]}"} "$@"
}

offline_save_images() {
  docker save ${offline_save_platform_args[@]+"${offline_save_platform_args[@]}"} "$@"
}

validate_approved_crm_image() {
  approved_crm_id="$(jq -er '
    .version == 1 and .application == "customer_and_opportunity" and .environment == "prod" and
    (.components | length == 9) and all(.components[]; .protocol == 1 and (.image_digest | test("^sha256:[a-f0-9]{64}$"))) |
    if . then true else error("invalid CRM approval") end
  ' "$license_stage/approvals/runtime-license-customer_and_opportunity-prod.json" >/dev/null &&
    jq -e '[.components[].service_id] | sort == (["customer-api","customer-contract-transfer-worker","customer-opportunity-alert-worker","customer-owner-notification-worker","customer-presale-alert-worker","customer-presale-assignment-notification-worker","customer-presale-progress-notification-worker","customer-notification-delivery-worker","customer-presale-worker"] | sort)' \
      "$license_stage/approvals/runtime-license-customer_and_opportunity-prod.json" >/dev/null &&
    jq -er '[.components[].image_digest] | unique | if length == 1 then .[0] else error("multiple CRM images") end' \
      "$license_stage/approvals/runtime-license-customer_and_opportunity-prod.json")" || { echo '批准中必须包含同一镜像的完整CRM九组件' >&2; exit 2; }
  [[ "$(offline_image_inspect "$approved_crm_id" --format '{{.Id}}')" == "$approved_crm_id" &&
     "$(offline_image_inspect "$approved_crm_id" --format '{{.Architecture}}/{{.Os}}')" == amd64/linux ]] || { echo '已批准CRM镜像不在本机或架构错误' >&2; exit 2; }
  approved_crm_version="$(offline_image_inspect "$approved_crm_id" --format '{{index .Config.Labels "org.opencontainers.image.version"}}')"
  [[ "$approved_crm_version" =~ ^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$ && "$(offline_image_inspect "$approved_crm_id" --format '{{index .Config.Labels "com.basic-platform.license.protocol"}}')" == 1 ]] || { echo '已批准CRM镜像缺少版本/授权协议标签' >&2; exit 2; }
  jq -e --arg id "$approved_crm_id" --arg ver "$approved_crm_version" '
    [.services[] | select(.application == "customer_and_opportunity" and .environment == "prod")] |
    length == 9 and all(.[]; .image_digest == $id and .version == $ver and .protocol == "1")
  ' "$license_stage/approvals/license-evidence.json" >/dev/null || { echo 'CRM目标证据与批准镜像版本不一致' >&2; exit 2; }
}

for command_name in docker gzip tar sha256sum mktemp jq; do command -v "$command_name" >/dev/null || { echo "缺少所需命令：$command_name" >&2; exit 1; }; done
docker buildx version >/dev/null
offline_configure_platform_selection
if [[ "$reuse_approved_crm_image" == true ]]; then
  validate_approved_crm_image
fi
source "$script_dir/offline-package-metadata.sh"
mkdir -p "$output_root"
# 后续会在输出目录内切换工作目录生成总校验文件，因此这里先规范为绝对路径，
# 保证 --output 同时支持绝对路径和相对路径。
output_root="$(cd -- "$output_root" && pwd)"

source "$script_dir/offline-build-inputs.sh"
source_input_fingerprint() {
  offline_build_input_fingerprint "$workspace_root" full
}

render_build_info() {
  local source_fingerprint docker_client docker_server buildx_version
  source_fingerprint="$(source_input_fingerprint)"
  docker_client="$(docker version --format '{{.Client.Version}}' 2>/dev/null || printf unavailable)"
  docker_server="$(docker version --format '{{.Server.Version}}' 2>/dev/null || printf unavailable)"
  buildx_version="$(docker buildx version 2>/dev/null | head -n 1 | tr -d '\r\n')"
  printf 'RELEASE_VERSION=%s\n' "$version"
  printf 'TARGET_PLATFORM=%s\n' "$platform"
  printf 'SOURCE_INPUT_SHA256=%s\n' "$source_fingerprint"
  [[ -z "$license_digest" ]] || printf 'LICENSE_SHA256=%s\n' "$license_digest"
  [[ -z "$runtime_approval_digest" ]] || printf 'RUNTIME_APPROVAL_SHA256=%s\n' "$runtime_approval_digest"
  [[ -z "$installation_digest" ]] || printf 'INSTALLATION_PLAN_SHA256=%s\n' "$installation_digest"
  printf 'DOCKER_CLIENT_VERSION=%s\n' "$docker_client"
  printf 'DOCKER_SERVER_VERSION=%s\n' "$docker_server"
  printf 'BUILDX_VERSION=%s\n' "${buildx_version:-unavailable}"
}

check_build_info_baseline() (
  local temporary has_published=false
  temporary="$(mktemp "$output_root/.build-info-check.XXXXXX")"
  trap 'rm -f -- "$temporary"' EXIT
  render_build_info > "$temporary"
  if compgen -G "$output_root/*.tar.gz" >/dev/null ||
     [[ -e "$output_root/install-assets.sh" || -L "$output_root/install-assets.sh" ||
        -e "$output_root/install-host-dependencies.sh" || -L "$output_root/install-host-dependencies.sh" ]]; then
    has_published=true
  fi
  if [[ -e "$output_root/BUILD_INFO.txt" || -L "$output_root/BUILD_INFO.txt" ]]; then
    [[ -f "$output_root/BUILD_INFO.txt" && ! -L "$output_root/BUILD_INFO.txt" ]] || {
      echo "既有构建信息不是安全的普通文件：$output_root/BUILD_INFO.txt" >&2
      exit 1
    }
    if ! cmp -s "$temporary" "$output_root/BUILD_INFO.txt"; then
      if [[ "$has_published" == true ]]; then
        echo '同一输出目录的源码输入或工具链基线已经变化；拒绝混合构建，请使用新的 --version 或空输出目录' >&2
        exit 1
      fi
      chmod 644 "$temporary"
      mv -f -- "$temporary" "$output_root/BUILD_INFO.txt"
      temporary=''
    fi
  else
    [[ "$has_published" == false ]] || {
      echo '输出目录已有发布包但缺少 BUILD_INFO.txt，无法证明同一源码/工具链基线；请使用新的 --version 或空输出目录' >&2
      exit 1
    }
    chmod 644 "$temporary"
    mv -f -- "$temporary" "$output_root/BUILD_INFO.txt"
    temporary=''
  fi
)

write_build_info() (
  local temporary
  temporary="$(mktemp "$output_root/.build-info.XXXXXX")"
  trap 'rm -f -- "$temporary"' EXIT
  render_build_info > "$temporary"
  chmod 644 "$temporary"
  if [[ -f "$output_root/BUILD_INFO.txt" ]]; then
    cmp -s "$temporary" "$output_root/BUILD_INFO.txt" || {
      echo '构建期间源码输入或工具链基线发生变化；拒绝更新发布清单' >&2
      exit 1
    }
  else
    mv -f -- "$temporary" "$output_root/BUILD_INFO.txt"
    temporary=''
  fi
)

# 构建首个组件前检查整个发布的源码和工具链基线，避免同一版本的增量构建混入不同源码树或 Docker 工具链生成的包。
check_build_info_baseline

publish_artifact() {
  local staged_archive="$1" archive="$2" staged_sidecar="$3"
  local staged_digest existing_digest expected_existing
  staged_digest="$(sha256sum "$staged_archive" | awk '{print tolower($1)}')"

  # 发布版本不可变。重复相同构建可复用字节完全相同的产物，但绝不能覆盖已发布的同名不同产物。
  if [[ -e "$archive" || -e "$archive.sha256" ]]; then
    [[ ! -L "$archive" && ! -L "$archive.sha256" ]] || {
      echo "拒绝覆盖符号链接形式的既有产物：${archive}" >&2
      return 1
    }
    if [[ -f "$archive" ]]; then
      existing_digest="$(sha256sum "$archive" | awk '{print tolower($1)}')"
      if [[ "$existing_digest" != "$staged_digest" ]]; then
        echo "同名产物已存在且内容不同：${archive}；请使用新的 --version 或空输出目录" >&2
        return 1
      fi
    fi
    if [[ -f "$archive.sha256" ]]; then
      expected_existing="$(awk -v name="$(basename -- "$archive")" '
        NF != 2 || NR != 1 || length($1) != 64 || tolower($1) ~ /[^0-9a-f]/ || $2 != name {bad=1}
        {digest=tolower($1)}
        END {if (bad || NR != 1) exit 1; print digest}
      ' "$archive.sha256")" || {
        echo "既有伴随校验文件格式无效：$archive.sha256" >&2
        return 1
      }
      [[ "$expected_existing" == "$staged_digest" ]] || {
        echo "既有产物与伴随校验不一致：${archive}" >&2
        return 1
      }
    fi
    # 处理已验证归档已提交、伴随文件尚未重命名到位的短暂中断窗口。
    if [[ -f "$archive" && ! -e "$archive.sha256" ]]; then
      mv -f -- "$staged_sidecar" "$archive.sha256"
      rm -f -- "$staged_archive"
      printf '已补齐同内容产物的伴随校验：%s\n' "$archive.sha256"
      return 0
    fi
    if [[ ! -e "$archive" && -f "$archive.sha256" ]]; then
      mv -f -- "$staged_archive" "$archive"
      rm -f -- "$staged_sidecar"
      printf '已补齐伴随校验对应的产物：%s\n' "$archive"
      return 0
    fi
    rm -f -- "$staged_archive" "$staged_sidecar"
    printf '复用字节一致的既有产物：%s\n' "$archive"
    return 0
  fi

  # 原子重命名前两个文件均已在此目录完整写入并验证。消费者要求两者同时存在，因此中断至多留下等待伴随文件的有效归档；下一次相同运行会修复。
  mv -f -- "$staged_archive" "$archive"
  mv -f -- "$staged_sidecar" "$archive.sha256"
}

published_artifact_is_valid() {
  local archive="$1" expected actual
  [[ -f "$archive" && ! -L "$archive" && -f "$archive.sha256" && ! -L "$archive.sha256" ]] || return 1
  expected="$(awk -v name="$(basename -- "$archive")" '
    NF != 2 || NR != 1 || length($1) != 64 || tolower($1) ~ /[^0-9a-f]/ || $2 != name {bad=1}
    {digest=tolower($1)}
    END {if (bad || NR != 1) exit 1; print digest}
  ' "$archive.sha256")" || return 1
  actual="$(sha256sum "$archive" | awk '{print tolower($1)}')" || return 1
  [[ "$expected" == "$actual" ]]
}

reuse_equivalent_image_package() (
  local staged_archive="$1" archive="$2" package_component="$3" temporary
  published_artifact_is_valid "$archive" || exit 1
  temporary="$(mktemp -d "$output_root/.image-equivalence.XXXXXX")"
  trap 'rm -rf -- "$temporary"' EXIT
  mkdir "$temporary/existing" "$temporary/staged"
  for pair in "existing:$archive" "staged:$staged_archive"; do
    label="${pair%%:*}"
    candidate="${pair#*:}"
    gzip -t "$candidate" || exit 1
    [[ "$(tar -tzf "$candidate" | LC_ALL=C sort | tr '\n' ' ')" == 'SHA256SUMS images.tar package.env ' ]] || exit 1
    tar -tvzf "$candidate" | awk 'substr($1,1,1) != "-" {exit 1}' || exit 1
    tar -xzf "$candidate" -C "$temporary/$label" package.env SHA256SUMS images.tar || exit 1
    (cd "$temporary/$label" && sha256sum --check SHA256SUMS >/dev/null) || exit 1
  done
  cmp -s "$temporary/existing/package.env" "$temporary/staged/package.env" || exit 1
  printf '复用镜像配置摘要一致的既有产物：%s（%s）\n' "$archive" "$package_component"
)

reuse_equivalent_asset_package() (
  local staged_archive="$1" archive="$2" temporary listing label candidate relative
  published_artifact_is_valid "$archive" || exit 1
  temporary="$(mktemp -d "$output_root/.asset-equivalence.XXXXXX")"
  trap 'rm -rf -- "$temporary"' EXIT
  mkdir "$temporary/existing" "$temporary/staged"
  for label in existing staged; do
    if [[ "$label" == existing ]]; then candidate="$archive"; else candidate="$staged_archive"; fi
    gzip -t "$candidate" || exit 1
    tar -tzf "$candidate" | awk '
      {sub(/^\.\//, ""); if ($0 == "" || $0 == ".") next}
      $0 ~ /^\// || $0 ~ /(^|\/)\.\.($|\/)/ {exit 1}
    ' || exit 1
    tar -tvzf "$candidate" | awk 'substr($1,1,1) != "-" && substr($1,1,1) != "d" {exit 1}' || exit 1
    tar -xzf "$candidate" --no-same-owner --no-same-permissions -C "$temporary/$label" || exit 1
  done
  listing="$(cd "$temporary/existing" && find . -type f -print | LC_ALL=C sort)"
  [[ "$listing" == "$(cd "$temporary/staged" && find . -type f -print | LC_ALL=C sort)" ]] || exit 1
  while IFS= read -r relative; do
    [[ -n "$relative" ]] || continue
    cmp -s "$temporary/existing/$relative" "$temporary/staged/$relative" || exit 1
  done <<< "$listing"
  printf '复用文件内容一致的既有部署资产：%s\n' "$archive"
)

package_images() (
  local package_component="$1" package_name="$2"; shift 2
  local image_tar package_dir archive archive_temporary sidecar_temporary tag image_id manifest_file manifest_member archive_digest proxy_config_digest
  package_dir="$(mktemp -d "$output_root/.${package_component}.XXXXXX")"
  archive_temporary=''
  sidecar_temporary=''
  cleanup_image_package() {
    local status=$?
    trap - EXIT
    rm -rf -- "$package_dir"
    [[ -z "$archive_temporary" ]] || rm -f -- "$archive_temporary"
    [[ -z "$sidecar_temporary" ]] || rm -f -- "$sidecar_temporary"
    exit "$status"
  }
  trap cleanup_image_package EXIT
  image_tar="$package_dir/images.tar"
  offline_save_images --output "$image_tar" "$@"
  if [[ "$package_component" == common ]]; then
    [[ -n "$managed_proxy_native_id" &&
       "$(offline_image_inspect "$managed_proxy_transport" --format '{{.Id}}')" == "$managed_proxy_native_id" &&
       "$(offline_image_inspect "$managed_proxy_upstream" --format '{{.Id}}')" == "$managed_proxy_native_id" ]] || {
      echo '公共代理镜像在打包期间发生变化；拒绝发布来源不明确的镜像包' >&2; exit 1;
    }
  fi
  manifest_file="$package_dir/image-manifest.json"
  # 经典 Docker 归档包含 manifest.json，纯 OCI 归档刻意不含。
  # 元数据辅助工具解析 index.json 前不能要求 Docker 成员存在；若存在类似清单但有歧义的成员，必须拒绝，不能静默回退到 OCI 元数据。
  if manifest_member="$(offline_archive_member "$image_tar" manifest.json 2>/dev/null)"; then
    tar -xOf "$image_tar" "$manifest_member" > "$manifest_file" || {
      echo "无法读取 Docker 镜像归档清单：$package_component" >&2
      exit 1
    }
  elif tar -tf "$image_tar" | awk '$0 == "manifest.json" || $0 == "./manifest.json" {found=1} END {exit found ? 0 : 1}'; then
    echo "Docker 镜像归档包含重复或歧义 manifest.json：$package_component" >&2
    exit 1
  else
    : > "$manifest_file"
  fi
  (cd "$package_dir" && sha256sum images.tar > SHA256SUMS)
  {
    printf 'PACKAGE_FORMAT=2\n'
    printf 'COMPONENT=%s\n' "$package_component"
    printf 'VERSION=%s\n' "$version"
    printf 'PLATFORM=%s\n' "$platform"
    printf 'IMAGES='
    local first=true
    for tag in "$@"; do
      [[ "$first" == true ]] || printf ','
      printf '%s' "$tag"
      first=false
    done
    printf '\n'
    printf 'IMAGE_CONFIG_DIGESTS='
    first=true
    for tag in "$@"; do
      image_id="$(offline_saved_image_config_id "$image_tar" "$manifest_file" "$tag")" || {
        echo "无法从 Docker 归档读取镜像配置摘要：$tag" >&2
        exit 1
      }
      [[ "$image_id" =~ ^sha256:[a-f0-9]{64}$ ]] || { echo "镜像 ID 格式不正确：$tag" >&2; exit 1; }
      [[ "$first" == true ]] || printf ','
      printf '%s' "$image_id"
      first=false
    done
    printf '\n'
    if [[ "$package_component" == common ]]; then
      # 记录上方实际检查并直接重新打标签的固定上游；配置另从已保存字节独立读取，单有传输标签不能证明上游摘要来源。
      proxy_config_digest="$(offline_saved_image_config_id "$image_tar" "$manifest_file" "$managed_proxy_transport")" || {
        echo '无法核对固定公共代理的归档配置摘要' >&2; exit 1;
      }
      printf 'DOCKER_SOCKET_PROXY_UPSTREAM_REF=%s\n' "$managed_proxy_upstream"
      printf 'DOCKER_SOCKET_PROXY_CONFIG_DIGEST=%s\n' "$proxy_config_digest"
    fi
  } > "$package_dir/package.env"
  if [[ "$package_component" == common ]]; then
    archive="$output_root/${package_name}-linux-amd64.tar.gz"
  else
    archive="$output_root/${package_name}-${version}-linux-amd64.tar.gz"
  fi
  archive_temporary="$(mktemp "$output_root/.${package_name}.archive.XXXXXX")"
  sidecar_temporary="$(mktemp "$output_root/.${package_name}.sidecar.XXXXXX")"
  COPYFILE_DISABLE=1 tar --no-xattrs -C "$package_dir" -czf "$archive_temporary" package.env SHA256SUMS images.tar
  gzip -t "$archive_temporary"
  [[ "$(tar -tzf "$archive_temporary" | LC_ALL=C sort | tr '\n' ' ')" == 'SHA256SUMS images.tar package.env ' ]] || {
    echo "生成的镜像包条目不完整：$package_component" >&2
    exit 1
  }
  archive_digest="$(sha256sum "$archive_temporary" | awk '{print tolower($1)}')"
  printf '%s  %s\n' "$archive_digest" "$(basename -- "$archive")" > "$sidecar_temporary"
  if reuse_equivalent_image_package "$archive_temporary" "$archive" "$package_component"; then
    rm -f -- "$archive_temporary" "$sidecar_temporary"
    archive_temporary=''
    sidecar_temporary=''
    return 0
  fi
  publish_artifact "$archive_temporary" "$archive" "$sidecar_temporary" || exit 1
  archive_temporary=''
  sidecar_temporary=''
  printf '已生成镜像包：%s\n' "$archive"
)

build_image() {
  local context="$1" dockerfile="$2" tag="$3" target="${4:-}"
  local args=(docker buildx build --platform "$platform" --load --pull=false --file "$dockerfile" --tag "$tag")
  if [[ "$dockerfile" == "$workspace_root/frontend/Dockerfile" ]]; then
    args+=(--build-arg "NPM_CONFIG_REGISTRY=$build_npm_registry")
  else
    args+=(--build-arg "GOPROXY=$build_goproxy" --build-arg "GOSUMDB=$build_gosumdb" --build-arg "APP_VERSION=$version")
  fi
  [[ -z "$target" ]] || args+=(--target "$target")
  args+=("$context")
  "${args[@]}"
  [[ "$(offline_image_inspect "$tag" --format '{{.Architecture}}/{{.Os}}')" == "amd64/linux" ]] || {
    echo "构建出的镜像平台不是 linux/amd64：$tag" >&2; exit 1;
  }
}

want() { [[ "$component" == all || "$component" == "$1" ]]; }

publish_host_dependencies_installer() (
  local installer="$output_root/install-host-dependencies.sh" staged_installer staged_sidecar installer_digest
  staged_installer="$(mktemp "$output_root/.install-host-dependencies.bootstrap.XXXXXX")"
  staged_sidecar="$(mktemp "$output_root/.install-host-dependencies.bootstrap-sidecar.XXXXXX")"
  cleanup_host_bootstrap() {
    local status=$?
    trap - EXIT
    rm -f -- "$staged_installer" "$staged_sidecar"
    exit "$status"
  }
  trap cleanup_host_bootstrap EXIT
  install -m 755 "$script_dir/install-host-dependencies.sh" "$staged_installer"
  bash -n "$staged_installer"
  installer_digest="$(sha256sum "$staged_installer" | awk '{print tolower($1)}')"
  printf '%s  %s\n' "$installer_digest" "$(basename -- "$installer")" > "$staged_sidecar"
  publish_artifact "$staged_installer" "$installer" "$staged_sidecar" || exit 1
  staged_installer=''
  staged_sidecar=''
  printf '已生成宿主依赖安装入口：%s\n' "$installer"
)

if want host-deps; then
  "$script_dir/build-host-dependencies.sh" --version "$version" --output "$output_root"
  publish_host_dependencies_installer
fi

ensure_local_platform_image() {
  local image="$1" image_platform='' attempt=0
  if image_platform="$(offline_image_inspect "$image" --format '{{.Os}}/{{.Architecture}}' 2>/dev/null)"; then
    if [[ "$image_platform" == "linux/amd64" ]]; then
      echo "复用本地镜像（${image_platform}）：${image}"
      return 0
    fi
    echo "本地镜像平台为 ${image_platform:-未知}，需要重新拉取 linux/amd64：$image" >&2
  fi
  for ((attempt=1; attempt<=pull_retries; attempt++)); do
    if docker pull --platform "$platform" "$image"; then
      break
    fi
    if ((attempt == pull_retries)); then
      echo "公共镜像拉取失败，已尝试 ${pull_retries} 次：$image" >&2
      return 1
    fi
    echo "公共镜像拉取失败（${attempt}/${pull_retries}）：${image}；${pull_retry_delay} 秒后重试" >&2
    sleep "$pull_retry_delay"
  done
  image_platform="$(offline_image_inspect "$image" --format '{{.Os}}/{{.Architecture}}')"
  [[ "$image_platform" == "linux/amd64" ]] || {
    echo "拉取后的镜像平台不正确：$image ($image_platform)" >&2
    return 1
  }
}

if want common; then
  common_base_images=(
    mysql:8.4 quay.io/keycloak/keycloak:26.2 temporalio/auto-setup:1.29.7
    metabase/metabase:v0.53.7 prom/prometheus:v3.5.0 prom/node-exporter:v1.9.1
    registry:2.8.3 tecnativa/docker-socket-proxy:v0.5.0
  )
  common_images=()
  common_index=0
  base_evidence="$output_root/base-image-evidence.tsv"
  base_evidence_temporary="$output_root/.base-image-evidence.$$"
  : > "$base_evidence_temporary"
  for tag in "${common_base_images[@]}"; do
    if [[ "$tag" == "$managed_proxy_transport" ]]; then
      ensure_local_platform_image "$managed_proxy_upstream"
      managed_proxy_native_id="$(offline_image_inspect "$managed_proxy_upstream" --format '{{.Id}}')"
      [[ "$managed_proxy_native_id" =~ ^sha256:[a-f0-9]{64}$ ]] || {
        echo '固定上游公共代理没有有效的本机镜像身份' >&2; exit 1;
      }
      # 不要以此镜像为 FROM 重新构建；即使只增加包装层也可能改变镜像身份，不能将其表示为原始上游摘要。
      docker tag "$managed_proxy_upstream" "$managed_proxy_transport"
      [[ "$(offline_image_inspect "$managed_proxy_transport" --format '{{.Id}}')" == "$managed_proxy_native_id" ]] || {
        echo '公共代理传输标签与固定上游实际镜像不一致' >&2; exit 1;
      }
      common_images+=("$tag")
      continue
    fi
    ensure_local_platform_image "$tag"
    base_ref="$(offline_image_inspect "$tag" --format '{{range .RepoDigests}}{{println .}}{{end}}' 2>/dev/null |
      awk -v name="${tag%%:*}" 'index($0, name "@sha256:") == 1 { print; exit }')" || base_ref=''
    [[ -n "$base_ref" ]] || base_ref="$tag"
    printf '%s\t%s\n' 'platform/deploy/production/bin/offline-base.Dockerfile' "$base_ref" >> "$base_evidence_temporary"
    common_index=$((common_index + 1))
    local_tag="uip-package/common-${common_index}:$version"
    # 即使 --pull=false，BuildKit 仍会从仓库解析带摘要的 FROM 引用。构建使用已验证本地标签，并记录 RepoDigest 作为证据，
    # 使镜像站无法提供清单元数据时仍可离线构建。
    docker buildx build --platform "$platform" --load --pull=false \
      --build-arg "BASE_IMAGE=$tag" --file "$script_dir/offline-base.Dockerfile" \
      --tag "$local_tag" "$deploy_dir"
    docker tag "$local_tag" "$tag"
    common_images+=("$tag")
  done
  chmod 600 "$base_evidence_temporary"
  mv -f -- "$base_evidence_temporary" "$base_evidence"
  file_gateway_tag="uip-package/file-gateway:$version"
  build_image "$workspace_root/platform" "$workspace_root/platform/Dockerfile" "$file_gateway_tag" file-gateway-runtime
  common_images+=("$file_gateway_tag")
  package_images common common-infrastructure "${common_images[@]}"
fi

if want platform; then
  tag="uip-package/platform-backend:$version"
  build_image "$workspace_root/platform" "$workspace_root/platform/Dockerfile" "$tag"
  package_images platform platform-backend "$tag"
fi
if want frontend; then
  tag="uip-package/frontend:$version"
  build_image "$workspace_root/frontend" "$workspace_root/frontend/Dockerfile" "$tag"
  package_images frontend frontend "$tag"
fi
if want customer-opportunity; then
  tag="uip-package/customer-opportunity-backend:$version"
  if [[ "$reuse_approved_crm_image" == true ]]; then
    docker tag "$approved_crm_id" "$tag"
    printf '复用已审核CRM镜像：%s\n' "$approved_crm_id"
  else
    build_image "$workspace_root/customer_and_opportunity" "$workspace_root/customer_and_opportunity/Dockerfile" "$tag" crm-runtime
  fi
  package_images customer-opportunity customer-opportunity-backend "$tag"
fi
if want customer-portal; then
  tag="uip-package/customer-portal-backend:$version"
  build_image "$workspace_root/customer_and_opportunity" "$workspace_root/customer_and_opportunity/Dockerfile" "$tag" portal-runtime
  package_images customer-portal customer-portal-backend "$tag"
fi
if want contract; then
  tag="uip-package/contract-backend:$version"
  build_image "$workspace_root/contract_management" "$workspace_root/contract_management/Dockerfile" "$tag"
  package_images contract contract-backend "$tag"
fi
if want project; then
  tag="uip-package/project-backend:$version"
  build_image "$workspace_root/project_management" "$workspace_root/project_management/Dockerfile" "$tag"
  package_images project project-backend "$tag"
fi
if want settlement; then
  tag="uip-package/settlement-backend:$version"
  build_image "$workspace_root/Settlement" "$workspace_root/Settlement/Dockerfile" "$tag"
  package_images settlement settlement-backend "$tag"
fi
if want data-analysis; then
  data_analysis_images=()
  for target in dashboard-api aggregation-worker alert-worker production-migrate; do
    tag="uip-package/data-analysis-${target}:$version"
    build_image "$workspace_root/data_analysis" "$workspace_root/data_analysis/Dockerfile" "$tag" "$target"
    data_analysis_images+=("$tag")
  done
  # 生产 Compose 和 CI/CD 中这些是独立可执行镜像。可放入同一传输包，但绝不能合并为单一运行镜像。
  package_images data-analysis data-analysis-backend "${data_analysis_images[@]}"
fi

package_assets() (
  local assets_archive assets_temporary sidecar_temporary assets_stage relative destination assets_digest
  local -a asset_files=(
    .env.example .gitignore .release.env.example ACCEPTANCE_CHECKLIST.md BACKUP_RECOVERY.md DEPLOYMENT_COMPATIBILITY.md DELIVERY_WORKFLOW.md OFFLINE_DEPLOYMENT.md OFFLINE_RUNBOOK.md README.md docker-compose.yml
    bin/acceptance-evidence.sh bin/apply-public-transport.sh bin/backup-all.sh bin/backup-file-gateway.sh bin/backup-keycloak-mysql.sh bin/portal-report-backup.sh
    bin/build-evidence.py bin/build-host-dependencies.sh bin/build-offline-packages.sh bin/compose-scope.sh bin/deploy-customer-opportunity.sh bin/deploy-service.sh bin/deploy.sh
    bin/drill-file-gateway-restore.sh bin/install-assets.sh bin/lifecycle.sh bin/offline-base.Dockerfile bin/project-background.sh
    bin/install-host-dependencies.sh
    bin/offline-configure.sh bin/offline-package-metadata.sh bin/offline-build-inputs.sh bin/offline-image-identities.py
    bin/export-license-inventory.py bin/frontend-network.py bin/check-frontend-network.sh bin/subsystem-runtime.py
    bin/delivery-inspect.py bin/delivery-inspect-images.py bin/delivery-inspect-v2.py bin/delivery-runtime.py bin/delivery-runtime.sh
    bin/deployment-session.py bin/deployment-workflow.sh bin/delivery-files.py
    bin/provisioner-config-refresh.sh bin/publish-runtime-approvals.py bin/public-transport.sh bin/reload-public-certificate.sh
    bin/restore-file-gateway.sh bin/restore-keycloak-mysql.sh bin/restore-mysql.sh bin/start-enabled.sh
    bin/set-container-timezone.sh bin/test-container-timezone.sh CONTAINER_TIMEZONE.md
    bin/test-auto-install-packages.sh bin/test-compose-scope.sh bin/test-deployment-parity.sh bin/test-doctor.sh bin/test-project-background.py
    bin/test-file-gateway-wiring.sh bin/test-lifecycle.sh bin/test-offline-configure.sh bin/test-offline-import.sh bin/test-offline-registry-network.sh
    bin/test-offline-package-metadata.sh bin/test-offline-transport.sh bin/test-public-transport.sh bin/test-start-enabled.sh
    bin/test-backup-recovery-safety.sh bin/test-control-plane-reload.sh bin/test-release-reliability.sh bin/test-subsystem-state-machine.sh bin/test-verify-readiness.sh
    monitoring/prometheus/file-gateway-alerts.yml monitoring/prometheus/keycloak-alerts.yml monitoring/prometheus/prometheus.yml
    mysql-init/contract-databases.sql mysql-init/data-analysis-metabase.sql nginx/basic-platform.conf.example
    subsystem-templates/contract.env.example subsystem-templates/customer.env.example subsystem-templates/data-analysis.env.example
    subsystem-templates/portal.env.example subsystem-templates/portal-report-worker.env.example subsystem-templates/project.env.example subsystem-templates/settlement.env.example
    subsystems.d/contract_management-prod.yaml subsystems.d/customer_and_opportunity-prod.yaml
    subsystems.d/customer_portal-prod.yaml subsystems.d/data-analysis-prod.yaml subsystems.d/project_management-prod.yaml
    subsystems.d/settlement-prod.yaml tests/preflight-test.sh tests/test-acceptance-evidence.sh tests/test-assets-install.sh tests/test-compose-blocks.py
    tests/test-host-dependencies.sh tests/test-offline-package-builder.sh tests/test-offline-image-identities.py
    tests/test-frontend-modes.sh tests/test-key-init.sh tests/test-metabase-init.sh
  )
  assets_archive="$output_root/deployment-assets-${version}.tar.gz"
  assets_temporary="$(mktemp "$output_root/.deployment-assets.archive.XXXXXX")"
  sidecar_temporary="$(mktemp "$output_root/.deployment-assets.sidecar.XXXXXX")"
  assets_stage="$(mktemp -d "$output_root/.deployment-assets-stage.XXXXXX")"
  cleanup_asset_package() {
    local status=$?
    trap - EXIT
    rm -rf -- "$assets_stage"
    rm -f -- "$assets_temporary" "$sidecar_temporary"
    exit "$status"
  }
  trap cleanup_asset_package EXIT

  # 不要整体归档生产目录。仅将已审核静态文件放入暂存区，使 .env 备份、私钥、数据库转储及未来未知文件均默认排除。
  for relative in "${asset_files[@]}"; do
    [[ -f "$deploy_dir/$relative" && ! -L "$deploy_dir/$relative" ]] || {
      echo "静态部署资产缺失或不是普通文件：$relative" >&2
      exit 1
    }
    destination="$assets_stage/$relative"
    mkdir -p -- "$(dirname -- "$destination")"
    cp -p -- "$deploy_dir/$relative" "$destination"
  done
  if [[ -n "$license_snapshot" ]]; then
    mkdir -p -- "$assets_stage/license"
    install -m 600 -- "$license_snapshot" "$assets_stage/license/commercial-license.jws"
  fi
  if [[ -n "$installation_digest" ]]; then
    install -m 600 -- "$license_stage/license-installation.json" "$assets_stage/license-installation.json"
  fi
  if [[ -n "$runtime_approval_digest" ]]; then
    for approval_name in "${runtime_approval_files[@]}"; do
      [[ ! -f "$license_stage/approvals/$approval_name" ]] || install -m 600 -- "$license_stage/approvals/$approval_name" "$assets_stage/$approval_name"
    done
  fi
  COPYFILE_DISABLE=1 tar --no-xattrs -C "$assets_stage" -czf "$assets_temporary" .
  gzip -t "$assets_temporary"
  assets_digest="$(sha256sum "$assets_temporary" | awk '{print tolower($1)}')"
  printf '%s  %s\n' "$assets_digest" "$(basename -- "$assets_archive")" > "$sidecar_temporary"
  if reuse_equivalent_asset_package "$assets_temporary" "$assets_archive"; then
    rm -f -- "$assets_temporary" "$sidecar_temporary"
    assets_temporary=''
    sidecar_temporary=''
    return 0
  fi
  publish_artifact "$assets_temporary" "$assets_archive" "$sidecar_temporary" || exit 1
  assets_temporary=''
  sidecar_temporary=''
  printf '已生成部署资产包：%s\n' "$assets_archive"
)

publish_bootstrap_installer() (
  local installer="$output_root/install-assets.sh" staged_installer staged_sidecar installer_digest
  staged_installer="$(mktemp "$output_root/.install-assets.bootstrap.XXXXXX")"
  staged_sidecar="$(mktemp "$output_root/.install-assets.bootstrap-sidecar.XXXXXX")"
  cleanup_bootstrap() {
    local status=$?
    trap - EXIT
    rm -f -- "$staged_installer" "$staged_sidecar"
    exit "$status"
  }
  trap cleanup_bootstrap EXIT
  # 发布与资产包内相同的规范安装器；统一部署目录树中已不存在旧 platform/scripts 副本。
  install -m 755 "$script_dir/install-assets.sh" "$staged_installer"
  bash -n "$staged_installer"
  installer_digest="$(sha256sum "$staged_installer" | awk '{print tolower($1)}')"
  printf '%s  %s\n' "$installer_digest" "$(basename -- "$installer")" > "$staged_sidecar"
  publish_artifact "$staged_installer" "$installer" "$staged_sidecar" || exit 1
  staged_installer=''
  staged_sidecar=''
  printf '已生成独立安装入口：%s\n' "$installer"
)

if want assets; then
  package_assets
  publish_bootstrap_installer
fi

# 总校验文件始终覆盖输出目录中当前存在的全部镜像包/资产包、独立安装入口和
# 构建信息，因此
# 增量构建到同一输出目录时不会丢失既有组件的摘要。
if compgen -G "$output_root/*.tar.gz" >/dev/null ||
   [[ -f "$output_root/install-assets.sh" || -f "$output_root/install-host-dependencies.sh" ]]; then
  write_build_info
  if find "$output_root" -maxdepth 1 -type f -name '*-linux-amd64.tar.gz' ! -name 'host-dependencies-*' -print -quit | grep -q .; then
    [[ -f "$script_dir/build-evidence.py" && ! -L "$script_dir/build-evidence.py" ]] ||
      { echo '缺少构建证据采集器 build-evidence.py' >&2; exit 1; }
    source_index="$output_root/source-input-index.tsv"
    offline_build_input_index "$workspace_root" full "$source_index" ||
      { echo '源码索引生成失败；拒绝生成构建证据' >&2; exit 1; }
    base_evidence_args=()
    [[ ! -f "$output_root/base-image-evidence.tsv" ]] ||
      base_evidence_args=(--base-image-evidence "$output_root/base-image-evidence.tsv")
    python3 "$script_dir/build-evidence.py"       --output-root "$output_root"       --workspace "$workspace_root"       --version "$version"       --platform "$platform"       --source-sha256 "$(source_input_fingerprint)"       --source-index "$source_index"       "${base_evidence_args[@]+"${base_evidence_args[@]}"}"       --builder-id "${UIP_BUILD_BUILDER_ID:-}"       --invocation-id "${UIP_BUILD_INVOCATION_ID:-}" ||
      { echo '构建证据采集失败；拒绝生成发布校验文件' >&2; exit 1; }
  fi
  checksums_temporary="$(mktemp "$output_root/.checksums.XXXXXX")"
  (
    cd "$output_root"
    shopt -s nullglob
    checksum_files=(*.tar.gz BUILD_INFO.txt)
    [[ ! -f build-evidence.json ]] || checksum_files+=(build-evidence.json)
    [[ ! -f source-input-index.tsv ]] || checksum_files+=(source-input-index.tsv)
    [[ ! -f base-image-evidence.tsv ]] || checksum_files+=(base-image-evidence.tsv)
    [[ ! -f install-assets.sh ]] || checksum_files+=(install-assets.sh)
    [[ ! -f install-host-dependencies.sh ]] || checksum_files+=(install-host-dependencies.sh)
    sha256sum -- "${checksum_files[@]}" > "$checksums_temporary"
  )
  mv -f "$checksums_temporary" "$output_root/SHA256SUMS"
  printf '总校验文件已更新：%s/SHA256SUMS\n' "$output_root"
else
  printf '警告：%s 中没有 .tar.gz，未生成 SHA256SUMS\n' "$output_root" >&2
fi
printf '离线部署包构建完成：%s\n' "$output_root"
