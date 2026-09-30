#!/usr/bin/env bash
# Install a verified, OS-bound local APT repository without network access.
set -Eeuo pipefail

usage() {
  cat >&2 <<'EOF'
用法：sudo ./install-host-dependencies.sh host-dependencies-*.tar.gz

要求：目标机为归档声明的同一 Ubuntu VERSION_ID、linux/amd64；Docker 不在本包范围。
最小引导命令 bash、tar、gzip、sha256sum、dpkg、apt-get 必须由 Ubuntu 基础系统提供。
EOF
}

(($# == 1)) || { usage; exit 2; }
((EUID == 0)) || { echo '安装宿主依赖必须使用 root' >&2; exit 1; }
archive="$1"
[[ -f "$archive" && ! -L "$archive" && -f "$archive.sha256" && ! -L "$archive.sha256" ]] || {
  echo '缺少普通文件形式的宿主依赖包或 SHA256 伴随文件' >&2
  exit 1
}
for command_name in apt-get dpkg dpkg-deb gzip mktemp sha256sum tar; do
  command -v "$command_name" >/dev/null 2>&1 || {
    echo "Ubuntu 最小引导环境缺少命令：$command_name" >&2
    exit 1
  }
done

archive="$(cd "$(dirname -- "$archive")" && pwd)/$(basename -- "$archive")"
expected="$(awk -v name="$(basename -- "$archive")" '
  NF != 2 || NR != 1 || length($1) != 64 || tolower($1) ~ /[^0-9a-f]/ || $2 != name {bad=1}
  {digest=tolower($1)}
  END {if (bad || NR != 1) exit 1; print digest}
' "$archive.sha256")" || { echo '宿主依赖包 SHA256 伴随文件格式无效' >&2; exit 1; }
actual="$(sha256sum "$archive" | awk '{print tolower($1)}')"
[[ "$actual" == "$expected" ]] || { echo '宿主依赖包 SHA256 校验失败' >&2; exit 1; }
gzip -t "$archive"

listing="$(tar -tzf "$archive")"
printf '%s\n' "$listing" | awk '
  {sub(/^\.\//, ""); if ($0 == "" || $0 == ".") next;
   if ($0 ~ /^\// || $0 ~ /(^|\/)\.\.($|\/)/) exit 1;
   if ($0 !~ /^(host-dependencies\.env|package-manifest\.tsv|Packages|Packages\.gz|SHA256SUMS|debs\/?|debs\/[A-Za-z0-9.+_:%~-]+\.deb)$/) exit 1;
   if (++seen[$0] != 1) exit 1}
' || { echo '宿主依赖包包含重复条目或不安全路径' >&2; exit 1; }
tar -tvzf "$archive" | awk 'substr($1,1,1) != "-" && substr($1,1,1) != "d" {exit 1}' || {
  echo '宿主依赖包禁止包含链接或特殊文件' >&2
  exit 1
}

stage="$(mktemp -d /var/tmp/uip-host-dependencies.XXXXXX)"
cleanup() {
  local status=$?
  trap - EXIT
  rm -rf -- "$stage"
  exit "$status"
}
trap cleanup EXIT
tar -xzf "$archive" --no-same-owner --no-same-permissions -C "$stage"
(cd "$stage" && sha256sum --check SHA256SUMS)

manifest_value() {
  local key="$1"
  awk -F= -v key="$key" '$1 == key {sub(/^[^=]*=/, ""); print; found++} END {exit found == 1 ? 0 : 1}' \
    "$stage/host-dependencies.env"
}
package_format="$(manifest_value PACKAGE_FORMAT)"
package_os_id="$(manifest_value OS_ID)"
package_os_version="$(manifest_value OS_VERSION_ID)"
package_codename="$(manifest_value OS_CODENAME)"
package_architecture="$(manifest_value ARCHITECTURE)"
requested_csv="$(manifest_value REQUESTED_PACKAGES)"
[[ "$package_format" == 1 && "$package_os_id" == ubuntu && "$package_architecture" == amd64 ]] || {
  echo '宿主依赖包元数据无效' >&2
  exit 1
}
[[ "$requested_csv" =~ ^[a-z0-9][a-z0-9+.,-]*$ ]] || {
  echo '宿主依赖包请求列表格式无效' >&2
  exit 1
}

# shellcheck disable=SC1091
source /etc/os-release
host_os_id="${ID:-}"
host_os_version="${VERSION_ID:-}"
host_codename="${VERSION_CODENAME:-${UBUNTU_CODENAME:-}}"
host_architecture="$(dpkg --print-architecture)"
if [[ "$host_os_id" != "$package_os_id" || "$host_os_version" != "$package_os_version" ||
      "$host_codename" != "$package_codename" || "$host_architecture" != "$package_architecture" ]]; then
  echo '宿主依赖包与目标操作系统不匹配，拒绝安装：' >&2
  echo "  依赖包：$package_os_id $package_os_version $package_codename $package_architecture" >&2
  echo "  目标机：$host_os_id $host_os_version $host_codename $host_architecture" >&2
  exit 1
fi

while IFS=$'\t' read -r package_name package_version package_arch deb_name deb_sha; do
  [[ "$package_name" =~ ^[a-z0-9][a-z0-9+.-]*$ && -n "$package_version" ]] || {
    echo '宿主依赖包清单包含非法包名或版本' >&2; exit 1;
  }
  [[ "$package_arch" == amd64 || "$package_arch" == all ]] || {
    echo "宿主依赖架构无效：$package_name ($package_arch)" >&2; exit 1;
  }
  deb="$stage/debs/$deb_name"
  [[ -f "$deb" && ! -L "$deb" ]] || { echo "清单中的 .deb 不存在：$deb_name" >&2; exit 1; }
  [[ "$(dpkg-deb -f "$deb" Package)" == "$package_name" ]] || { echo "包名不一致：$deb_name" >&2; exit 1; }
  [[ "$(dpkg-deb -f "$deb" Version)" == "$package_version" ]] || { echo "包版本不一致：$deb_name" >&2; exit 1; }
  [[ "$(sha256sum "$deb" | awk '{print tolower($1)}')" == "$deb_sha" ]] || { echo "包摘要不一致：$deb_name" >&2; exit 1; }
done < "$stage/package-manifest.tsv"

printf 'deb [trusted=yes] file:%s ./\n' "$stage" > "$stage/sources.list"
mkdir -p "$stage/apt-lists/partial" "$stage/apt-cache/archives/partial"
apt_options=(
  -o "Dir::Etc::sourcelist=$stage/sources.list"
  -o 'Dir::Etc::sourceparts=-'
  -o "Dir::State::lists=$stage/apt-lists"
  -o "Dir::Cache::archives=$stage/apt-cache/archives"
  -o 'APT::Get::List-Cleanup=0'
  -o 'Acquire::Languages=none'
)
apt-get "${apt_options[@]}" update
IFS=, read -r -a requested_packages <<< "$requested_csv"
DEBIAN_FRONTEND=noninteractive apt-get "${apt_options[@]}" \
  --no-install-recommends install -y "${requested_packages[@]}"

required_commands=(
  jq curl update-ca-certificates tar gzip sha256sum realpath
  bash cmp find awk grep ss openssl sed flock
)
for command_name in "${required_commands[@]}"; do
  command -v "$command_name" >/dev/null 2>&1 || {
    echo "宿主依赖安装后仍缺少命令：$command_name" >&2
    exit 1
  }
done

echo "宿主依赖安装完成：$package_os_id $package_os_version $package_codename $package_architecture"
printf '已验证命令：%s\n' "${required_commands[*]}"
