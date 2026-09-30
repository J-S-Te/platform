#!/usr/bin/env bash
# Build a release-bound Ubuntu/amd64 offline APT repository for the host tools
# required before deployment assets or image packages can be consumed.
set -Eeuo pipefail

usage() {
  cat <<'EOF'
用法：build-host-dependencies.sh --version 发布版本 --output 输出目录

必须在与离线目标机相同 Ubuntu VERSION_ID、同为 amd64 的联网构建机运行。
产物包括宿主机命令及其 APT 依赖，不包括 Docker Engine 或 Compose plugin。
EOF
}

version=''
output_root=''
while (($#)); do
  case "$1" in
    --version) version="${2:?缺少版本号}"; shift 2 ;;
    --output) output_root="${2:?缺少输出目录}"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) usage >&2; exit 2 ;;
  esac
done

[[ "$version" =~ ^[A-Za-z0-9._-]+$ ]] || { echo '版本号格式不正确' >&2; exit 2; }
[[ -n "$output_root" ]] || { usage >&2; exit 2; }
[[ -r /etc/os-release ]] || { echo '无法读取 /etc/os-release' >&2; exit 1; }

for command_name in apt-cache apt-get dpkg dpkg-deb gzip install mktemp sha256sum sort tar; do
  command -v "$command_name" >/dev/null 2>&1 || {
    echo "构建宿主依赖包缺少命令：$command_name" >&2
    exit 1
  }
done

# /etc/os-release is owned by the operating system and is the authoritative
# compatibility boundary for the APT repository emitted here.
# shellcheck disable=SC1091
source /etc/os-release
os_id="${ID:-}"
os_version_id="${VERSION_ID:-}"
os_codename="${VERSION_CODENAME:-${UBUNTU_CODENAME:-}}"
architecture="$(dpkg --print-architecture)"
[[ "$os_id" == ubuntu && -n "$os_version_id" && -n "$os_codename" ]] || {
  echo '宿主依赖离线包目前只支持 Ubuntu，且必须能识别 VERSION_ID/VERSION_CODENAME' >&2
  exit 1
}
[[ "$architecture" == amd64 && "$(uname -m)" == x86_64 ]] || {
  echo "宿主依赖包必须在 Ubuntu linux/amd64 构建机生成；当前：$(uname -m)/$architecture" >&2
  exit 1
}
[[ "$os_version_id" =~ ^[0-9]+\.[0-9]+$ && "$os_codename" =~ ^[a-z0-9-]+$ ]] || {
  echo 'Ubuntu 版本元数据格式不安全' >&2
  exit 1
}

# Include every package that provides a command used by configure, import,
# lifecycle, backup and recovery paths. The six explicitly requested packages
# remain first-class requirements; the remainder prevents the next missing
# host command from surfacing after jq is installed.
requested_packages=(
  jq curl ca-certificates tar gzip coreutils
  bash diffutils findutils gawk grep iproute2 openssl sed util-linux
)

for package in "${requested_packages[@]}"; do
  candidate="$(apt-cache policy "$package" | awk '/Candidate:/ {print $2; exit}')"
  [[ -n "$candidate" && "$candidate" != '(none)' ]] || {
    echo "APT 没有可下载候选版本：${package}；请先更新与目标 Ubuntu 版本匹配的软件源" >&2
    exit 1
  }
done

mkdir -p "$output_root"
output_root="$(cd -- "$output_root" && pwd)"
stage="$(mktemp -d "$output_root/.host-dependencies-stage.XXXXXX")"
archive_temporary="$(mktemp "$output_root/.host-dependencies.archive.XXXXXX")"
sidecar_temporary="$(mktemp "$output_root/.host-dependencies.sidecar.XXXXXX")"
cleanup() {
  local status=$?
  trap - EXIT
  rm -rf -- "$stage"
  rm -f -- "$archive_temporary" "$sidecar_temporary"
  exit "$status"
}
trap cleanup EXIT
mkdir -p "$stage/debs"

mapfile -t resolved_packages < <(
  apt-cache depends --recurse --important \
    --no-recommends --no-suggests --no-conflicts --no-breaks \
    --no-replaces --no-enhances "${requested_packages[@]}" \
    | awk '/^[^[:space:]<]/ && $1 ~ /^[a-z0-9][a-z0-9+.-]*$/ {print $1}' \
    | LC_ALL=C sort -u
)
((${#resolved_packages[@]} > 0)) || { echo '无法解析宿主依赖闭包' >&2; exit 1; }

(
  cd "$stage/debs"
  apt-get download "${resolved_packages[@]}"
)
compgen -G "$stage/debs/*.deb" >/dev/null || { echo 'APT 未下载任何 .deb' >&2; exit 1; }

requested_csv="$(IFS=,; printf '%s' "${requested_packages[*]}")"
cat > "$stage/host-dependencies.env" <<EOF
PACKAGE_FORMAT=1
OS_ID=$os_id
OS_VERSION_ID=$os_version_id
OS_CODENAME=$os_codename
ARCHITECTURE=$architecture
RELEASE_VERSION=$version
REQUESTED_PACKAGES=$requested_csv
EOF

: > "$stage/package-manifest.tsv"
: > "$stage/Packages"
while IFS= read -r -d '' deb; do
  filename="$(basename -- "$deb")"
  package_name="$(dpkg-deb -f "$deb" Package)"
  package_version="$(dpkg-deb -f "$deb" Version)"
  package_architecture="$(dpkg-deb -f "$deb" Architecture)"
  [[ "$package_architecture" == amd64 || "$package_architecture" == all ]] || {
    echo "依赖包架构不正确：$filename ($package_architecture)" >&2
    exit 1
  }
  printf '%s\t%s\t%s\t%s\t%s\n' \
    "$package_name" "$package_version" "$package_architecture" "$filename" \
    "$(sha256sum "$deb" | awk '{print tolower($1)}')" \
    >> "$stage/package-manifest.tsv"
  dpkg-deb -f "$deb" >> "$stage/Packages"
  printf 'Filename: debs/%s\nSize: %s\nSHA256: %s\n\n' \
    "$filename" "$(stat -c '%s' "$deb")" \
    "$(sha256sum "$deb" | awk '{print tolower($1)}')" \
    >> "$stage/Packages"
done < <(find "$stage/debs" -maxdepth 1 -type f -name '*.deb' -print0 | LC_ALL=C sort -z)

gzip -n -9 -c "$stage/Packages" > "$stage/Packages.gz"
(
  cd "$stage"
  sha256sum host-dependencies.env package-manifest.tsv Packages Packages.gz debs/*.deb > SHA256SUMS
)

archive="$output_root/host-dependencies-${os_id}-${os_version_id}-${os_codename}-${version}-linux-amd64.tar.gz"
# apt-get stamps downloaded .deb files with the download time. Normalize every
# archive header so rebuilding an unchanged repository produces identical
# bytes; a same-version conflict then means package content actually changed,
# not merely that the build ran at a different time.
COPYFILE_DISABLE=1 LC_ALL=C tar \
  --format=gnu \
  --sort=name \
  --mtime='@0' \
  --owner=0 \
  --group=0 \
  --numeric-owner \
  --mode='u+rwX,go+rX,go-w' \
  --no-xattrs \
  -C "$stage" \
  -cf - \
  host-dependencies.env package-manifest.tsv Packages Packages.gz SHA256SUMS debs \
  | gzip -n -9 > "$archive_temporary"
gzip -t "$archive_temporary"
tar -tvzf "$archive_temporary" | awk 'substr($1,1,1) != "-" && substr($1,1,1) != "d" {exit 1}' || {
  echo '宿主依赖归档包含链接或特殊文件' >&2
  exit 1
}
archive_digest="$(sha256sum "$archive_temporary" | awk '{print tolower($1)}')"
printf '%s  %s\n' "$archive_digest" "$(basename -- "$archive")" > "$sidecar_temporary"

if [[ -e "$archive" || -e "$archive.sha256" ]]; then
  [[ -f "$archive" && ! -L "$archive" && -f "$archive.sha256" && ! -L "$archive.sha256" ]] || {
    echo "既有宿主依赖产物不完整或不安全：$archive" >&2
    exit 1
  }
  existing_digest="$(sha256sum "$archive" | awk '{print tolower($1)}')"
  [[ "$existing_digest" == "$archive_digest" ]] || {
    echo "同名宿主依赖包内容不同：${archive}；请使用新的发布版本" >&2
    exit 1
  }
  rm -f -- "$archive_temporary" "$sidecar_temporary"
  archive_temporary=''
  sidecar_temporary=''
  echo "复用内容相同的宿主依赖包：$archive"
  exit 0
fi

mv -- "$archive_temporary" "$archive"
archive_temporary=''
mv -- "$sidecar_temporary" "$archive.sha256"
sidecar_temporary=''
chmod 640 "$archive" "$archive.sha256"
echo "已生成宿主依赖离线包：$archive"
