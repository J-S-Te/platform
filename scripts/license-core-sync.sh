#!/bin/sh
# Only the standard-library consumer core is distributable to platform builds.
set -eu
platform_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
mirror="$platform_root/third_party/license-core"
manifest="$platform_root/scripts/license-core.sha256"
files='clock.go diff.go go.mod license.go recovery.go runtime/lock_unix.go runtime/snapshot.go runtime/state.go'
production_files='clock.go diff.go license.go recovery.go runtime/lock_unix.go runtime/snapshot.go runtime/state.go'
digest() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d ' ' -f 1
  else shasum -a 256 "$1" | cut -d ' ' -f 1; fi
}
verify() {
  directory=$1
  [ -z "$(find "$directory" -type l -print)" ] || { echo "license core symlink rejected: $directory" >&2; exit 1; }
  expected=$(printf '%s\n' $files | sort)
  actual=$(cd "$directory" && find . -type f | sed 's|^./||' | sort)
  [ "$actual" = "$expected" ] || { echo "license core file allowlist mismatch: $directory" >&2; exit 1; }
  listed=$(awk '{print $2}' "$manifest" | sort)
  [ "$listed" = "$expected" ] || { echo 'invalid license core manifest file list' >&2; exit 1; }
  while read -r hash name; do
    [ "$(digest "$directory/$name")" = "$hash" ] || { echo "license core digest mismatch: $directory/$name" >&2; exit 1; }
  done < "$manifest"
  [ "$(wc -l < "$manifest" | tr -d ' ')" = 8 ] || { echo 'invalid license core manifest' >&2; exit 1; }
}
verify_source_list() {
  [ -z "$(find "$source_root" -type l -print)" ] || { echo 'license core authority symlinks rejected' >&2; exit 1; }
  # coverage is development-only; syncclient/consumer are business runtime
  # adapters not imported by platform. None is shipped by this platform build.
  # Adding a new distributable package still requires reviewing this allowlist.
  actual=$(cd "$source_root" && find . -type f -name '*.go' ! -name '*_test.go' ! -path './coverage/*' ! -path './syncclient/*' ! -path './consumer/*' | sed 's|^./||' | sort)
  expected=$(printf '%s\n' $production_files | sort)
  [ "$actual" = "$expected" ] || { echo 'review the recursive license core source allowlist before syncing' >&2; exit 1; }
}
mode=${1:---check}
source_root=${2:-$platform_root/../license-core}
case "$mode" in
  --sync)
    [ -d "$source_root" ] || { echo "license core source missing: $source_root" >&2; exit 1; }
    # A new production source requires an explicit allowlist review.
    verify_source_list
    mkdir -p "$mirror"
    for file in $files; do mkdir -p "$mirror/$(dirname "$file")"; cp "$source_root/$file" "$mirror/$file"; done
    : > "$manifest"
    for file in $files; do printf '%s  %s\n' "$(digest "$mirror/$file")" "$file" >> "$manifest"; done
    verify "$mirror"
    ;;
  --check)
    verify "$mirror"
    if [ -d "$source_root" ]; then
      verify_source_list
      for file in $files; do
        cmp -s "$source_root/$file" "$mirror/$file" || { echo "license core authority drift: $file (run --sync and review)" >&2; exit 1; }
      done
    fi
    if [ -d "$platform_root/vendor/github.com/J-S-Te/license-core" ]; then
      vendor_root="$platform_root/vendor/github.com/J-S-Te/license-core"
      actual=$(cd "$vendor_root" && find . -type f | sed 's|^./||' | sort)
      expected=$(printf '%s\n' $production_files | sort)
      [ "$actual" = "$expected" ] || { echo 'license core vendor allowlist drift' >&2; exit 1; }
      [ -z "$(find "$vendor_root" -type l -print)" ] || { echo 'license core vendor symlinks rejected' >&2; exit 1; }
      for file in $production_files; do
        cmp -s "$mirror/$file" "$platform_root/vendor/github.com/J-S-Te/license-core/$file" || { echo "license core vendor drift: $file (run go mod vendor)" >&2; exit 1; }
      done
    fi
    ;;
  *) echo 'usage: sh scripts/license-core-sync.sh --check|--sync [authoritative-core-directory]' >&2; exit 2 ;;
esac
