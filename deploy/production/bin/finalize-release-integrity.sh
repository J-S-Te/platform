#!/usr/bin/env bash
# 构建端收尾：生成证明并由构建角色签名，生成发布清单并由发布角色签名，再用独立安装的验证器检查结果目录。
set -Eeuo pipefail
umask 077

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
platform_root="$(cd -- "$script_dir/../../.." && pwd)"
media=""
spec=""
profiles=""
build_key=""
release_key=""
trust_dir=""
verifier=""
work_dir=""

usage() {
  cat <<'USAGE'
用法：finalize-release-integrity.sh \
  --media <交付目录> --spec <release-spec.json> --profiles-dir <六份运行 Profile 目录> \
  --build-key <构建签名私钥> --release-key <发布签名私钥> \
  --trust-dir <释放信任目录> --verifier <release-verifier 绝对路径> [--work-dir <空目录>]

该步骤在 SHA256SUMS 生成后、总归档前执行；会向介质写入 release-manifest.json、
release-manifest.sigstore.json、attestations/ 和 profiles/。私钥绝不写入介质。
USAGE
}

die() { printf '[错误] %s\n' "$*" >&2; exit 1; }
while (($#)); do
  case "$1" in
    --media) media="${2:?--media 缺少参数}"; shift 2 ;;
    --spec) spec="${2:?--spec 缺少参数}"; shift 2 ;;
    --profiles-dir) profiles="${2:?--profiles-dir 缺少参数}"; shift 2 ;;
    --build-key) build_key="${2:?--build-key 缺少参数}"; shift 2 ;;
    --release-key) release_key="${2:?--release-key 缺少参数}"; shift 2 ;;
    --trust-dir) trust_dir="${2:?--trust-dir 缺少参数}"; shift 2 ;;
    --verifier) verifier="${2:?--verifier 缺少参数}"; shift 2 ;;
    --work-dir) work_dir="${2:?--work-dir 缺少参数}"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) usage >&2; die "未知参数：$1" ;;
  esac
done

[[ -n "$media" && -n "$spec" && -n "$profiles" && -n "$build_key" && -n "$release_key" && -n "$trust_dir" && -n "$verifier" ]] ||
  die 'finalize 参数不完整'
[[ "$media" == /* && "$spec" == /* && "$profiles" == /* && "$build_key" == /* && "$release_key" == /* && "$trust_dir" == /* && "$verifier" == /* ]] ||
  die 'finalize 参数必须是绝对路径'
[[ -d "$media" && ! -L "$media" ]] || die '介质目录不安全'
[[ -f "$spec" && ! -L "$spec" ]] || die 'release spec 缺失或不安全'
[[ -d "$profiles" && ! -L "$profiles" ]] || die 'profiles 目录不安全'
[[ -f "$build_key" && ! -L "$build_key" ]] || die '构建私钥缺失或不安全'
[[ -f "$release_key" && ! -L "$release_key" ]] || die '发布私钥缺失或不安全'
[[ -d "$trust_dir" && ! -L "$trust_dir" ]] || die '信任目录缺失或不安全'
[[ -f "$verifier" && -x "$verifier" && ! -L "$verifier" ]] || die 'release-verifier 缺失或不可执行'
command -v go >/dev/null 2>&1 || die 'finalize 需要 go 工具链'
[[ -f "$media/SHA256SUMS" && ! -L "$media/SHA256SUMS" ]] || die 'finalize 必须在 SHA256SUMS 生成后执行'
for existing in release-manifest.json release-manifest.sigstore.json attestations profiles; do
  [[ ! -e "$media/$existing" && ! -L "$media/$existing" ]] || die "介质已包含发布完整性产物，拒绝重复 finalize：$existing"
done

temporary_work=false
if [[ -z "$work_dir" ]]; then
  work_dir="$(mktemp -d "${TMPDIR:-/tmp}/uip-release-finalize.XXXXXX")"
  temporary_work=true
else
  [[ "$work_dir" == /* && -d "$work_dir" && ! -L "$work_dir" ]] || die 'work-dir 必须是已存在的绝对目录'
fi
cleanup() {
  local status=$?
  if [[ "$temporary_work" == true ]]; then rm -rf -- "$work_dir"; fi
  exit "$status"
}
trap cleanup EXIT

(
  cd -- "$platform_root"
  go build -trimpath -o "$work_dir/release-manifest" ./cmd/release-manifest
  go build -trimpath -o "$work_dir/release-sign" ./cmd/release-sign
) || die '无法构建发布完整性 finalize 工具'

"$work_dir/release-manifest" attestations \
  --media "$media" --spec "$spec" --profiles-dir "$profiles" \
  --plan-output "$work_dir/attestation-signing-plan.json" ||
  die '生成 attestations 失败'

shopt -s nullglob
statements=("$media"/attestations/*.intoto.json)
((${#statements[@]} > 0)) || die '没有生成任何 attestations'
for statement in "${statements[@]}"; do
  [[ -f "$statement" && ! -L "$statement" ]] || die 'attestation 不是安全普通文件'
  "$work_dir/release-sign" dsse --key "$build_key" --input "$statement" \
    --output "${statement%.json}.sigstore.json" --key-id "${UIP_RELEASE_BUILD_KEY_ID:-build}" ||
    die "构建角色签名失败：$(basename -- "$statement")"
done

"$work_dir/release-manifest" manifest \
  --media "$media" --spec "$spec" --profiles-dir "$profiles" \
  --plan-output "$work_dir/release-signing-plan.json" ||
  die '生成 release manifest 失败'

"$work_dir/release-sign" blob --key "$release_key" \
  --input "$media/release-manifest.json" \
  --output "$media/release-manifest.sigstore.json" ||
  die '发布角色签名失败'

"$verifier" verify --media "$media" --trust-dir "$trust_dir" --output json > "$work_dir/verification.json" ||
  die 'finalize 后离线验签失败'
python3 - "$work_dir/verification.json" <<'PY'
import json, sys
with open(sys.argv[1], encoding="utf-8") as handle:
    result = json.load(handle)
if result.get("status") != "VERIFIED":
    raise SystemExit("release verification status is not VERIFIED")
PY

printf '[信息] release finalize 完成并验签通过：%s\n' "$media"
printf '[信息] manifest：%s\n' "$media/release-manifest.json"
printf '[信息] 构建证明：%s/attestations\n' "$media"
