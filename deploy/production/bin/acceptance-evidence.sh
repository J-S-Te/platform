#!/usr/bin/env bash
# Read-only acceptance evidence collector. It deliberately never reads config
# values, container environments, database rows, application logs or Secrets.
set -Eeuo pipefail
umask 077

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
default_deploy_dir="$(cd -- "$script_dir/.." && pwd)"
mode=""
output_dir=""
deploy_dir="$default_deploy_dir"
package_dir=""
backup_dir=""
failures=0
warnings=0

usage() {
  cat <<'EOF'
用法：
  acceptance-evidence.sh baseline --output DIR [--deploy-dir DIR]
  acceptance-evidence.sh build-gate --output DIR [--deploy-dir DIR]
  acceptance-evidence.sh delivery-gate --output DIR --package-dir DIR
  acceptance-evidence.sh postdeploy --output DIR [--deploy-dir DIR]
  acceptance-evidence.sh recovery-verify --output DIR --backup DIR [--deploy-dir DIR]

说明：
  baseline         修改服务器前的只读资源盘点；Docker 尚未安装时也可执行。
  build-gate       联网构建前的软件、架构与 Docker 能力门禁。
  delivery-gate    对隔离交付目录中的 SHA256SUMS 和文件类型做只读校验。
  postdeploy       执行 doctor/verify 并采集脱敏容器、镜像、配置摘要证据。
  recovery-verify  只校验灾备批次，不执行任何恢复。

输出目录必须不存在，脚本会以 0700 创建。证据不得放进待交付包或公开目录。
EOF
}

[[ $# -ge 1 ]] || { usage >&2; exit 64; }
mode="$1"
shift
while (($#)); do
  case "$1" in
    --output) output_dir="${2:-}"; shift 2 ;;
    --deploy-dir) deploy_dir="${2:-}"; shift 2 ;;
    --package-dir) package_dir="${2:-}"; shift 2 ;;
    --backup) backup_dir="${2:-}"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) usage >&2; exit 64 ;;
  esac
done
case "$mode" in baseline|build-gate|delivery-gate|postdeploy|recovery-verify) ;; *) usage >&2; exit 64 ;; esac
[[ -n "$output_dir" && "$output_dir" == /* && "$output_dir" != / ]] || {
  echo '--output 必须是安全的绝对路径' >&2; exit 64; }
[[ ! -e "$output_dir" && ! -L "$output_dir" ]] || {
  echo "拒绝覆盖已有证据目录：$output_dir" >&2; exit 73; }
install -d -m 700 "$output_dir"

summary="$output_dir/SUMMARY.tsv"
report="$output_dir/ACCEPTANCE_REPORT.md"
printf 'status\tcheck\tevidence\n' >"$summary"

record() {
  local status="$1" check="$2" evidence="$3"
  printf '%s\t%s\t%s\n' "$status" "$check" "$evidence" >>"$summary"
  case "$status" in FAIL) failures=$((failures + 1));; WARN) warnings=$((warnings + 1));; esac
}

safe_name() {
  [[ "$1" =~ ^[A-Za-z0-9._-]+$ ]] || { echo "不安全的证据文件名：$1" >&2; exit 64; }
}

capture() {
  local name="$1"; shift
  safe_name "$name"
  "$@" >"$output_dir/$name" 2>&1
}

capture_optional() {
  local name="$1" check="$2"; shift 2
  if capture "$name" "$@"; then record PASS "$check" "$name"; else record WARN "$check" "$name"; fi
}

capture_required() {
  local name="$1" check="$2"; shift 2
  if capture "$name" "$@"; then record PASS "$check" "$name"; else record FAIL "$check" "$name"; fi
}

command_capture() {
  local command_name="$1" name="$2" check="$3" requirement="$4"; shift 4
  if ! command -v "$command_name" >/dev/null 2>&1; then
    printf 'command unavailable: %s\n' "$command_name" >"$output_dir/$name"
    if [[ "$requirement" == required ]]; then record FAIL "$check" "$name"; else record WARN "$check" "$name"; fi
    return
  fi
  if [[ "$requirement" == required ]]; then capture_required "$name" "$check" "$@"; else capture_optional "$name" "$check" "$@"; fi
}

write_metadata() {
  {
    printf 'COLLECTOR_VERSION=1\n'
    printf 'MODE=%s\n' "$mode"
    printf 'COLLECTED_AT_UTC=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    printf 'HOSTNAME_SHA256=%s\n' "$(hostname | sha256sum | awk '{print $1}')"
    printf 'DEPLOY_DIR=%s\n' "$deploy_dir"
  } >"$output_dir/METADATA"
  chmod 600 "$output_dir/METADATA"
}

baseline() {
  capture_required uname.txt kernel-and-architecture uname -a
  if [[ -r /etc/os-release ]]; then
    capture_required os-release.txt operating-system awk -F= '$1 ~ /^(NAME|VERSION|VERSION_ID|ID)$/ {print}' /etc/os-release
  else
    printf 'unavailable\n' >"$output_dir/os-release.txt"; record WARN operating-system os-release.txt
  fi
  command_capture lscpu cpu.txt cpu-inventory optional lscpu
  command_capture free memory.txt memory-inventory optional free -h
  command_capture df filesystems.txt disk-and-inode-inventory required df -hT
  command_capture df inodes.txt disk-and-inode-inventory required df -hi
  command_capture ss listening-tcp.txt listening-ports optional ss -lntH
  command_capture ip addresses.txt network-addresses optional ip -brief address
  command_capture ip routes.txt network-routes optional ip route show
  command_capture timedatectl time-sync.txt time-and-sync optional timedatectl show --property=Timezone --property=NTPSynchronized --property=TimeUSec
  if command -v docker >/dev/null 2>&1; then
    capture_optional docker-version.txt docker-version docker version
    capture_optional containers-before.txt existing-containers docker ps -a --no-trunc --format 'table {{.Names}}\t{{.Image}}\t{{.Status}}\t{{.Ports}}'
    capture_optional volumes-before.txt existing-volumes docker volume ls --format 'table {{.Name}}\t{{.Driver}}'
    capture_optional networks-before.txt existing-networks docker network ls --format 'table {{.Name}}\t{{.Driver}}\t{{.Scope}}'
  else
    printf 'Docker not installed at baseline.\n' >"$output_dir/docker-version.txt"
    record WARN docker-version docker-version.txt
  fi
}

build_gate() {
  local missing_commands=0
  baseline
  if [[ "$(uname -s)" == Linux && "$(uname -m)" =~ ^(x86_64|amd64)$ ]]; then
    printf 'linux/amd64\n' >"$output_dir/build-platform.txt"; record PASS build-platform build-platform.txt
  else
    printf 'unsupported build host: %s/%s\n' "$(uname -s)" "$(uname -m)" >"$output_dir/build-platform.txt"
    record FAIL build-platform build-platform.txt
  fi
  for command_name in docker gzip tar sha256sum mktemp jq awk sed grep find stat; do
    if command -v "$command_name" >/dev/null 2>&1; then
      printf '%s\tPASS\n' "$command_name" >>"$output_dir/required-commands.tsv"
    else
      printf '%s\tMISSING\n' "$command_name" >>"$output_dir/required-commands.tsv"
      missing_commands=$((missing_commands + 1))
    fi
  done
  if ((missing_commands == 0)); then
    record PASS required-build-commands required-commands.tsv
  else
    record FAIL required-build-commands required-commands.tsv
  fi
  if docker buildx version >"$output_dir/docker-buildx.txt" 2>&1; then record PASS docker-buildx docker-buildx.txt; else record FAIL docker-buildx docker-buildx.txt; fi
  if docker compose version >"$output_dir/docker-compose.txt" 2>&1; then record PASS docker-compose docker-compose.txt; else record FAIL docker-compose docker-compose.txt; fi
}

delivery_gate() {
  [[ -n "$package_dir" ]] || { echo 'delivery-gate 需要 --package-dir' >&2; exit 64; }
  package_dir="$(cd -- "$package_dir" 2>/dev/null && pwd)" || { echo '交付目录不存在' >&2; exit 66; }
  if find "$package_dir" -mindepth 1 -maxdepth 1 \( -type l -o \( ! -type f ! -type d \) \) -print -quit | grep -q .; then
    find "$package_dir" -mindepth 1 -maxdepth 1 \( -type l -o \( ! -type f ! -type d \) \) -print >"$output_dir/unsafe-file-types.txt"
    record FAIL safe-delivery-file-types unsafe-file-types.txt
  else
    printf 'no top-level symbolic link or special file\n' >"$output_dir/unsafe-file-types.txt"
    record PASS safe-delivery-file-types unsafe-file-types.txt
  fi
  while IFS= read -r -d '' delivery_file; do
    relative="${delivery_file#"$package_dir"/}"
    size="$(wc -c <"$delivery_file" | tr -d '[:space:]')"
    printf '%s\t%s bytes\n' "$relative" "$size"
  done < <(find "$package_dir" -maxdepth 2 -type f -print0) >"$output_dir/delivery-files.tsv"
  LC_ALL=C sort -o "$output_dir/delivery-files.tsv" "$output_dir/delivery-files.tsv"
  record PASS delivery-inventory delivery-files.tsv
  if [[ -f "$package_dir/SHA256SUMS" && ! -L "$package_dir/SHA256SUMS" ]]; then
    capture_required sha256-check.txt delivery-sha256 bash -c 'cd -- "$1" && sha256sum --check SHA256SUMS' _ "$package_dir"
  else
    printf 'SHA256SUMS missing or unsafe\n' >"$output_dir/sha256-check.txt"; record FAIL delivery-sha256 sha256-check.txt
  fi
  if find "$package_dir" -maxdepth 2 -type f \( -name '.env' -o -name '*.pem' -o -name '*.key' -o -name '*.sql' -o -name '*.sql.gz' -o -name '*.db' \) -print -quit | grep -q .; then
    find "$package_dir" -maxdepth 2 -type f \( -name '.env' -o -name '*.pem' -o -name '*.key' -o -name '*.sql' -o -name '*.sql.gz' -o -name '*.db' \) -print >"$output_dir/forbidden-artifacts.txt"
    record FAIL forbidden-artifacts forbidden-artifacts.txt
  else
    printf 'no obvious configuration, key or database artifact\n' >"$output_dir/forbidden-artifacts.txt"
    record PASS forbidden-artifacts forbidden-artifacts.txt
  fi
}

config_fingerprints() {
  local file mode
  for file in .env .release.env docker-compose.yml; do
    if [[ -f "$deploy_dir/$file" && ! -L "$deploy_dir/$file" ]]; then
      mode="$(stat -c '%a' "$deploy_dir/$file" 2>/dev/null || stat -f '%Lp' "$deploy_dir/$file")"
      printf '%s\tmode=%s\tsha256=%s\n' "$file" "$mode" "$(sha256sum "$deploy_dir/$file" | awk '{print $1}')"
    else
      printf '%s\tMISSING_OR_UNSAFE\n' "$file"
      return 1
    fi
  done
}

postdeploy() {
  [[ -x "$deploy_dir/bin/deploy.sh" ]] || { echo '部署入口不存在或不可执行' >&2; exit 66; }
  capture_required config-fingerprints.tsv configuration-fingerprints config_fingerprints
  capture_required compose-services.txt compose-service-scope docker compose --project-directory "$deploy_dir" --file "$deploy_dir/docker-compose.yml" --env-file "$deploy_dir/.env" --env-file "$deploy_dir/.release.env" config --services
  capture_required compose-images.txt compose-image-scope docker compose --project-directory "$deploy_dir" --file "$deploy_dir/docker-compose.yml" --env-file "$deploy_dir/.env" --env-file "$deploy_dir/.release.env" config --images
  capture_required doctor.txt deployment-doctor "$deploy_dir/bin/deploy.sh" doctor
  capture_required verify.txt platform-readiness "$deploy_dir/bin/deploy.sh" verify
  capture_required deployment-status.txt container-health-and-restarts "$deploy_dir/bin/deploy.sh" status
  capture_optional docker-containers.txt docker-container-summary docker ps -a --no-trunc --format 'table {{.Names}}\t{{.Image}}\t{{.Status}}\t{{.Ports}}'
}

recovery_verify() {
  [[ -n "$backup_dir" ]] || { echo 'recovery-verify 需要 --backup' >&2; exit 64; }
  [[ -x "$deploy_dir/bin/backup-all.sh" ]] || { echo '灾备校验入口不存在或不可执行' >&2; exit 66; }
  capture_required recovery-verify.txt disaster-recovery-media "$deploy_dir/bin/backup-all.sh" --verify-only --backup "$backup_dir"
}

write_metadata
case "$mode" in
  baseline) baseline ;;
  build-gate) build_gate ;;
  delivery-gate) delivery_gate ;;
  postdeploy) postdeploy ;;
  recovery-verify) recovery_verify ;;
esac

cat >"$report" <<EOF
# 统一身份认证平台验收报告（待人工确认）

- 证据模式：$mode
- UTC 采集时间：$(date -u +%Y-%m-%dT%H:%M:%SZ)
- 自动门禁失败：$failures
- 自动门禁警告：$warnings
- 证据目录：$(basename -- "$output_dir")

## 真实性边界

本目录只包含只读技术证据，不包含密码、Token、Secret、私钥、容器环境变量、数据库业务数据或原始应用日志。技术健康不能替代登录、授权、业务读写、文件上传下载和异步任务的人工验收。

## 人工验收结论

- 执行人：待填写
- 执行窗口：待填写
- 源码版本/发布版本：待填写
- 验收结果：待执行（PASS / FAIL / BLOCKED）
- 阻塞和影响：待填写
- RTO 实测值：未执行
- RPO 实测值：未执行
- 宿主机重启：未执行
- 长期稳定性：未执行

逐项验收记录请使用 ACCEPTANCE_CHECKLIST.md；不得仅依据本文件自动生成的 PASS 宣布业务验收通过。
EOF

find "$output_dir" -type f -exec chmod 0600 {} +
printf 'evidence=%s failures=%d warnings=%d\n' "$output_dir" "$failures" "$warnings"
((failures == 0))
