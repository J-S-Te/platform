#!/usr/bin/env bash
# 整理已签名的离线交付材料，供客户接收；介质内部保持扁平结构。
# 本脚本仅创建面向客户的外层目录，
# 分别存放镜像、公钥和清单。
set -Eeuo pipefail
umask 022
script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"

usage() {
  cat <<'USAGE'
用法：./整理客户交付目录.sh \
  --customer <客户公司名> \
  --media <已签名介质目录> \
  --archive <总归档 .tar> \
  [--public-keys <公钥/trust 目录>]  （默认 Key/公钥） \
  [--verifier <linux/amd64 release-verifier>]  （默认 outs/release-verifier-linux-amd64） \
  [--previous-delivery <上一批客户交付目录>]  可选；用于披露 trust/verifier/manifest 差异 \
  [--output-parent <父目录>]

输出：
  <父目录>/<客户公司名>/镜像/
  <父目录>/<客户公司名>/公钥/
  <父目录>/<客户公司名>/交付清单/
  <父目录>/<客户公司名>/工具/        （仅提供 --verifier 时）
  <父目录>/<客户公司名>/交付说明.md
USAGE
}
die() { printf '[错误] %s\n' "$*" >&2; exit 1; }
reject_zip_apple() {
  local root="$1" label="$2" hit
  hit="$(find "$root" -maxdepth 3 \( -name '*.zip' -o -name '__MACOSX' -o -name '._*' \) -print -quit 2>/dev/null || true)"
  [[ -z "$hit" ]] || die "${label} 包含 zip/Finder 元数据，禁止交付：$hit"
}

customer=""
media=""
archive=""
public_keys=""
verifier=""
previous_delivery=""
output_parent=""
while (($#)); do
  case "$1" in
    --customer) customer="${2:?--customer 缺少参数}"; shift 2 ;;
    --media) media="${2:?--media 缺少参数}"; shift 2 ;;
    --archive) archive="${2:?--archive 缺少参数}"; shift 2 ;;
    --public-keys) public_keys="${2:?--public-keys 缺少参数}"; shift 2 ;;
    --verifier) verifier="${2:?--verifier 缺少参数}"; shift 2 ;;
    --previous-delivery) previous_delivery="${2:?--previous-delivery 缺少参数}"; shift 2 ;;
    --output-parent) output_parent="${2:?--output-parent 缺少参数}"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) usage >&2; die "未知参数：$1" ;;
  esac
done

[[ -n "$customer" && -n "$media" && -n "$archive" ]] || {
  usage >&2
  die '--customer、--media、--archive 必填'
}
[[ -n "$public_keys" ]] || public_keys="$script_dir/Key/公钥"
[[ -n "$verifier" ]] || verifier="$script_dir/outs/release-verifier-linux-amd64"
[[ "$customer" != "." && "$customer" != ".." && "$customer" != */* && "$customer" != *\\* && "$customer" != *$'\n'* && "$customer" != *$'\r'* ]] ||
  die '客户名称必须是单一路径分量'
[[ ${#customer} -le 80 ]] || die '客户名称过长'
[[ -d "$media" && ! -L "$media" ]] || die '介质目录无效'
[[ -f "$archive" && ! -L "$archive" ]] || die '总归档无效'
[[ -f "$archive.sha256" && ! -L "$archive.sha256" ]] || die '总归档 sidecar 缺失'
[[ -d "$public_keys" && ! -L "$public_keys" ]] || die '公钥目录无效'
[[ -z "$previous_delivery" || ( -d "$previous_delivery" && ! -L "$previous_delivery" ) ]] || die '上一批交付目录无效'
reject_zip_apple "$media" '介质目录'
[[ -z "$verifier" || ( -f "$verifier" && ! -L "$verifier" ) ]] || die 'verifier 无效'

for required in release-manifest.json release-manifest.sigstore.json attestations profiles SHA256SUMS BUILD_INFO.txt; do
  [[ -e "$media/$required" && ! -L "$media/$required" ]] || die "介质缺少已签名交付文件：$required"
done
has_fresh_install_reset_tools=false
if [[ -e "$media/清理首次部署产物.sh" || -L "$media/清理首次部署产物.sh" ||
      -e "$media/FIRST_INSTALL_RESET.md" || -L "$media/FIRST_INSTALL_RESET.md" ]]; then
  # 此处不替代正式验签，但不能把未绑定/被改动的工具描述成已经受该批清单保护。
  for reset_item in 清理首次部署产物.sh FIRST_INSTALL_RESET.md; do
    [[ -f "$media/$reset_item" && ! -L "$media/$reset_item" ]] || die "失败首装工具或说明缺失/不安全：$reset_item"
    reset_expected="$(awk -v name="$reset_item" '
      $2 == name {n++; value=tolower($1); if (NF != 2) bad=1}
      END {if (bad || n != 1 || length(value) != 64 || value ~ /[^a-f0-9]/) exit 1; print value}
    ' "$media/SHA256SUMS")" || die "失败首装工具或说明未唯一绑定到 SHA256SUMS：$reset_item"
    reset_actual="$(sha256sum "$media/$reset_item" | awk '{print tolower($1)}')"
    [[ "$reset_actual" == "$reset_expected" ]] || die "失败首装工具或说明与 SHA256SUMS 不符：$reset_item"
  done
  has_fresh_install_reset_tools=true
fi
for public in trust-policy.json build.pub release.pub; do
  [[ -f "$public_keys/$public" && ! -L "$public_keys/$public" ]] || die "公钥目录缺少 $public"
done
if grep -RIl --include='*.key' --include='*.pem' --include='trust-policy.json' 'PRIVATE KEY' "$public_keys" >/dev/null 2>&1; then
  die '公钥目录包含私钥，拒绝交付'
fi
if [[ -z "$output_parent" ]]; then
  output_parent="$(cd -- "$(dirname -- "$media")" && pwd)"
else
  [[ -d "$output_parent" && ! -L "$output_parent" ]] || die 'output-parent 无效'
  output_parent="$(cd -- "$output_parent" && pwd)"
fi
destination="$output_parent/$customer"
[[ ! -e "$destination" && ! -L "$destination" ]] || die "客户交付目录已存在：$destination"

umask 077
mkdir -p "$destination/镜像" "$destination/公钥" "$destination/交付清单"
if [[ -n "$verifier" ]]; then mkdir -p "$destination/工具"; fi
chmod 755 "$destination" "$destination/镜像" "$destination/公钥" "$destination/交付清单"

# 大归档在同一文件系统内优先使用硬链接，失败时回退为复制。
if ln "$archive" "$destination/镜像/$(basename -- "$archive")" 2>/dev/null; then
  :
else
  cp -p "$archive" "$destination/镜像/$(basename -- "$archive")"
fi
cp -p "$archive.sha256" "$destination/镜像/$(basename -- "$archive.sha256")"
chmod 644 "$destination/镜像"/*

for public in trust-policy.json build.pub release.pub; do
  cp -p "$public_keys/$public" "$destination/公钥/$public"
  chmod 644 "$destination/公钥/$public"
done
(
  cd "$destination/公钥"
  sha256sum trust-policy.json build.pub release.pub > SHA256SUMS
  chmod 644 SHA256SUMS
)
cat > "$destination/公钥/重要说明.txt" <<'TXT'
这里只包含公钥和 trust policy，不包含私钥。
公钥必须通过独立可信通道核对指纹后再安装到目标机，不能把本目录直接当作
--release-trust-dir 使用，也不能因为公钥和镜像在同一个交付包就默认信任它。
TXT
chmod 644 "$destination/公钥/重要说明.txt"

for item in release-manifest.json release-manifest.sigstore.json SHA256SUMS BUILD_INFO.txt; do
  cp -p "$media/$item" "$destination/交付清单/$item"
  chmod 644 "$destination/交付清单/$item"
done
for item in attestations profiles; do
  cp -R -p "$media/$item" "$destination/交付清单/$item"
  chmod -R u+rwX,go+rX "$destination/交付清单/$item"
done
for optional in build-evidence.json source-input-index.tsv base-image-evidence.tsv PLATFORM_ASSETS_COMPATIBILITY.env; do
  if [[ -f "$media/$optional" && ! -L "$media/$optional" ]]; then
    cp -p "$media/$optional" "$destination/交付清单/$optional"
    chmod 644 "$destination/交付清单/$optional"
  fi
done

if [[ -n "$verifier" ]]; then
  chmod 755 "$destination/工具"
  cp -p "$verifier" "$destination/工具/$(basename -- "$verifier")"
  chmod 755 "$destination/工具/$(basename -- "$verifier")"
  trust_installer="$script_dir/platform/deploy/production/bin/install-release-trust.sh"
  [[ -f "$trust_installer" && ! -L "$trust_installer" ]] ||
    { echo '[错误] 缺少离线安装发布信任脚本' >&2; exit 1; }
  cp -p "$trust_installer" "$destination/工具/安装发布信任.sh"
  chmod 755 "$destination/工具/安装发布信任.sh"
  (
    cd "$destination/工具"
    sha256sum "$(basename -- "$verifier")" 安装发布信任.sh > SHA256SUMS
    chmod 644 SHA256SUMS
  )
fi

manifest_digest="$(sha256sum "$media/release-manifest.json" | awk '{print tolower($1)}')"
trust_digest="$(sha256sum "$public_keys/trust-policy.json" | awk '{print tolower($1)}')"
verifier_digest=""
if [[ -n "$verifier" ]]; then
  verifier_digest="$(sha256sum "$verifier" | awk '{print tolower($1)}')"
fi

previous_manifest_digest=""
previous_trust_digest=""
previous_verifier_digest=""
if [[ -n "$previous_delivery" ]]; then
  [[ -d "$previous_delivery" && ! -L "$previous_delivery" ]] || die '上一批交付目录无效'
  previous_manifest="$previous_delivery/交付清单/release-manifest.json"
  previous_trust="$previous_delivery/公钥/trust-policy.json"
  previous_verifier="$(find "$previous_delivery/工具" -maxdepth 1 -type f -name 'release-verifier*' -print -quit 2>/dev/null || true)"
  [[ ! -f "$previous_manifest" || -L "$previous_manifest" ]] || previous_manifest_digest="$(sha256sum "$previous_manifest" | awk '{print tolower($1)}')"
  [[ ! -f "$previous_trust" || -L "$previous_trust" ]] || previous_trust_digest="$(sha256sum "$previous_trust" | awk '{print tolower($1)}')"
  [[ -z "$previous_verifier" || ! -f "$previous_verifier" || -L "$previous_verifier" ]] || previous_verifier_digest="$(sha256sum "$previous_verifier" | awk '{print tolower($1)}')"
fi
cat > "$destination/交付说明.md" <<EOF
# ${customer} 离线交付说明

## 客户从这里开始

请先阅读同目录的 \`开始安装.md\`。上传完整客户目录后，在客户 Linux/amd64 服务器运行：

\`\`\`bash
sudo bash ./一键安装.sh
\`\`\`

入口自动校验、独立验签并调用本批原部署器；必要配置按提示填写。
许可证和应用接入完成后，仍运行同一入口继续。下方保留管理员手动操作与核对信息。

## 目录

- \`镜像/\`：离线交付总归档及 SHA-256 sidecar。
- \`公钥/\`：trust-policy.json、build.pub、release.pub；只有公钥，没有私钥。
- \`交付清单/\`：release manifest、签名、attestations、profiles、构建证据。
- \`工具/\`：目标机 release-verifier 和离线安装发布信任脚本。

## 安装前

1. 先通过独立可信通道核对 \`公钥/\` 的 SHA-256 和 trust policy 指纹；
2. 把 trust policy 安装到目标机 \`/etc/unified-identity-platform/release-trust/\`；
3. 不要把交付包里的公钥目录直接作为 \`--release-trust-dir\`，也不要从介质自动信任公钥。

离线服务器可以直接使用随包工具安装，无需联网：

\`\`\`bash
sudo ./工具/安装发布信任.sh \
  --trust-policy ./公钥/trust-policy.json \
  --verifier-source ./工具/release-verifier-linux-amd64 \
  --trust-dir /etc/unified-identity-platform/release-trust \
  --verifier-path /usr/local/bin/release-verifier \
  --expected-trust-sha256 ${trust_digest} \
  --expected-verifier-sha256 ${verifier_digest}
\`\`\`

安装前仍需把这两个摘要与独立可信渠道核对；同包文件不能自动证明来源。

## 固定摘要

\`release-manifest.json\` 的 SHA-256：

\`\`\`text
${manifest_digest}
\`\`\`

该摘要用于 API/Agent 的 \`SUBSYSTEM_RELEASE_EXPECTED_DIGEST\`，并应通过独立通道核对。

## 与上一批信任材料差异

| 项目 | 上一批 | 本批 |
|---|---|---|
| release manifest SHA-256 | ${previous_manifest_digest:-未提供} | ${manifest_digest} |
| trust-policy.json SHA-256 | ${previous_trust_digest:-未提供} | ${trust_digest} |
| release-verifier SHA-256 | ${previous_verifier_digest:-未提供} | ${verifier_digest:-未提供} |

换批后 verifier/trust 摘要变化是正常的，但不能靠“同包”自动信任。已安装旧 verifier 且摘要不同时，先通过独立可信通道核对本批摘要并备份旧文件；确认后安装脚本才允许使用 \`--force\` 覆盖。不要用 \`--force\` 跳过摘要核对，也不要在摘要不一致时继续安装。

## 交付格式

仅交付 tar 和 \`.sha256\` sidecar。禁止使用 macOS Finder 的“压缩”生成 zip：zip 不在 SHA256SUMS 覆盖内，且会引入 \`__MACOSX/\`、\`._*\`、中文名乱码和可执行位丢失。需要传递目录时使用 tar；解压后核心脚本如丢失 755，可用 \`bash <脚本>\` 调用。

## 安装

解压总归档后，使用解压出的目录执行：

\`\`\`bash
./服务器一键离线部署.sh --media <解压目录> --target /opt/unified-identity-platform \\
  --require-release-integrity \\
  --release-verifier /usr/local/bin/release-verifier \\
  --release-trust-dir /etc/unified-identity-platform/release-trust \\
  --install
\`\`\`

业务系统的 JWS、运行时批准和 ACK 仍需在基础平台部署后单独完成。
EOF
chmod 644 "$destination/交付说明.md"

# 仅描述该批介质实际携带的工具，不给历史签名包注入当前工作区的新脚本。
if [[ "$has_fresh_install_reset_tools" == true ]]; then
  cat >> "$destination/交付说明.md" <<'EOF'

## 失败首装清理后重新部署

总归档解压后的介质顶层包含 `清理首次部署产物.sh` 和 `FIRST_INSTALL_RESET.md`，
两者均已纳入该批介质的 SHA256SUMS；正式签名发行还受 release manifest 保护。
完成原有验签后，在介质目录阅读说明并先盘点：

```bash
sudo bash ./清理首次部署产物.sh \
  --target /opt/unified-identity-platform \
  --plan-file /root/uip-reset-01.json
```

仅在全新机器的失败首装、确认数据可以丢弃时，审核计划后执行：

```bash
sudo bash ./清理首次部署产物.sh \
  --apply /root/uip-reset-01.json \
  --confirm-target /opt/unified-identity-platform \
  --discard-fresh-data
```

该命令永久删除计划中的数据库、业务文件、密钥和安装记录；不适用于升级失败或已有真实业务。
原始介质、镜像缓存、共享 Registry/网络和宿主依赖保留。工具、计划和需要保留的备份
必须放在安装目录之外；需要保持实例身份时走原参数 `--resume`/恢复流程。
EOF
fi

reject_zip_apple "$destination" '客户交付目录'
python3 "$script_dir/platform/deploy/production/bin/prepare-customer-installer.py" \
  --delivery "$destination"
printf '[信息] 客户交付目录已生成：%s\n' "$destination"
find "$destination" -maxdepth 2 -type d -print | sed 's/^/  /'
