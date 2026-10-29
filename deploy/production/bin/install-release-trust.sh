#!/usr/bin/env bash
# Offline bootstrap for the release-verifier and its fixed trust policy.
# The files may travel in the same offline package for convenience, but the
# expected SHA-256 values should be confirmed through an independent channel.
set -Eeuo pipefail
umask 077

usage() {
  cat <<'USAGE'
用法（root）：
  sudo ./安装发布信任.sh \
    [--trust-policy <公钥/trust-policy.json>] \
    [--verifier-source <工具/release-verifier-linux-amd64>] \
    [--trust-dir /etc/unified-identity-platform/release-trust] \
    [--verifier-path /usr/local/bin/release-verifier] \
    [--expected-trust-sha256 <sha256>] \
    [--expected-verifier-sha256 <sha256>] \
    [--force]
说明：expected 摘要必须来自独立可信渠道；--force 仅在核对本批摘要并备份旧 trust/verifier 后使用，
      会覆盖目标机已有的不同文件，不能用来跳过摘要核对。
USAGE
}
die() { printf '[错误] %s\n' "$*" >&2; exit 1; }

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
base_dir="$(cd -- "$script_dir/.." && pwd)"
trust_policy=""
verifier_source=""
trust_dir="/etc/unified-identity-platform/release-trust"
verifier_path="/usr/local/bin/release-verifier"
expected_trust=""
expected_verifier=""
force=false
while (($#)); do
  case "$1" in
    --trust-policy) trust_policy="${2:?缺少参数}"; shift 2 ;;
    --verifier-source) verifier_source="${2:?缺少参数}"; shift 2 ;;
    --trust-dir) trust_dir="${2:?缺少参数}"; shift 2 ;;
    --verifier-path) verifier_path="${2:?缺少参数}"; shift 2 ;;
    --expected-trust-sha256) expected_trust="${2:?缺少参数}"; shift 2 ;;
    --expected-verifier-sha256) expected_verifier="${2:?缺少参数}"; shift 2 ;;
    --force) force=true; shift ;;
    -h|--help) usage; exit 0 ;;
    *) usage >&2; die "未知参数：$1" ;;
  esac
done

[[ "${EUID:-$(id -u)}" -eq 0 ]] || die '安装 trust/verifier 需要 root，请使用 sudo'
[[ -n "$trust_policy" ]] || trust_policy="$base_dir/公钥/trust-policy.json"
[[ -n "$verifier_source" ]] || verifier_source="$base_dir/工具/release-verifier-linux-amd64"
[[ "$trust_policy" == /* ]] || trust_policy="$(cd -- "$(dirname -- "$trust_policy")" && pwd)/$(basename -- "$trust_policy")"
[[ "$verifier_source" == /* ]] || verifier_source="$(cd -- "$(dirname -- "$verifier_source")" && pwd)/$(basename -- "$verifier_source")"
[[ -f "$trust_policy" && ! -L "$trust_policy" ]] || die 'trust policy 缺失或不安全'
[[ -f "$verifier_source" && ! -L "$verifier_source" ]] || die 'verifier 缺失或不安全'
[[ "$trust_dir" == /* && "$verifier_path" == /* ]] || die '目标路径必须是绝对路径'

if grep -q 'PRIVATE KEY' "$trust_policy" 2>/dev/null; then
  die 'trust policy 包含私钥，拒绝安装'
fi
grep -Eq '"schema"[[:space:]]*:[[:space:]]*"urn:uip:release-trust:v1"' "$trust_policy" ||
  die 'trust policy schema 不正确'
grep -Eq '"policy"[[:space:]]*:[[:space:]]*"vendor-key-offline-v1"' "$trust_policy" ||
  die 'trust policy policy 不正确'

trust_actual="$(sha256sum "$trust_policy")"; trust_actual="${trust_actual%% *}"
verifier_actual="$(sha256sum "$verifier_source")"; verifier_actual="${verifier_actual%% *}"
if [[ -n "$expected_trust" && "$trust_actual" != "$expected_trust" ]]; then
  die 'trust policy 摘要与独立核对值不一致'
fi
if [[ -n "$expected_verifier" && "$verifier_actual" != "$expected_verifier" ]]; then
  die 'verifier 摘要与独立核对值不一致'
fi
if [[ -z "$expected_trust$expected_verifier" ]]; then
  printf '%s\n' '[警告] 未提供独立核对的 SHA-256；本次只能防止传输损坏，不能证明公钥来源可信。' >&2
fi

install -d -m 700 "$trust_dir"
install -d -m 755 "$(dirname -- "$verifier_path")"
destination_trust="$trust_dir/trust-policy.json"
if [[ -e "$destination_trust" || -L "$destination_trust" ]]; then
  [[ -f "$destination_trust" && ! -L "$destination_trust" ]] || die '现有 trust 路径不安全'
  existing_trust="$(sha256sum "$destination_trust")"; existing_trust="${existing_trust%% *}"
  if [[ "$existing_trust" == "$trust_actual" ]]; then
    :
  elif [[ "$force" == true ]]; then
    install -m 600 "$trust_policy" "$destination_trust"
  else
    die '已存在不同的 trust policy；核对交付说明中的本批摘要并备份后，再使用 --force 覆盖'
  fi
else
  install -m 600 "$trust_policy" "$destination_trust"
fi
chmod 700 "$trust_dir"
chmod 600 "$destination_trust"

if [[ -e "$verifier_path" || -L "$verifier_path" ]]; then
  [[ -f "$verifier_path" && ! -L "$verifier_path" ]] || die '现有 verifier 路径不安全'
  existing_verifier="$(sha256sum "$verifier_path")"; existing_verifier="${existing_verifier%% *}"
  if [[ "$existing_verifier" == "$verifier_actual" ]]; then
    :
  elif [[ "$force" == true ]]; then
    install -m 755 "$verifier_source" "$verifier_path"
  else
    die '已存在不同的 verifier；核对交付说明中的本批摘要并备份后，再使用 --force 覆盖'
  fi
else
  install -m 755 "$verifier_source" "$verifier_path"
fi
chmod 755 "$verifier_path"

printf '[信息] trust policy 已安装：%s\n' "$destination_trust"
printf '[信息] trust policy SHA-256：%s\n' "$trust_actual"
printf '[信息] verifier 已安装：%s\n' "$verifier_path"
printf '[信息] verifier SHA-256：%s\n' "$verifier_actual"
printf '%s\n' '[提示] 请把上述摘要与独立可信渠道公布的摘要核对；不要仅因文件在同一交付包就信任。'
