#!/usr/bin/env bash
set -Eeuo pipefail

test_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
deploy_dir="$(cd -- "$test_dir/.." && pwd -P)"
builder="$deploy_dir/bin/build-host-dependencies.sh"
installer="$deploy_dir/bin/install-host-dependencies.sh"
aggregate_builder="$deploy_dir/bin/build-offline-packages.sh"

fail() { echo "FAIL: $*" >&2; exit 1; }
[[ -x "$builder" && -x "$installer" ]] || fail 'host dependency entrypoints are not executable'
bash -n "$builder" "$installer" "$aggregate_builder"

for package in jq curl ca-certificates tar gzip coreutils; do
  grep -Eq "(^|[[:space:]])${package}([[:space:]]|$)" "$builder" || {
    fail "required host package is not included: $package"
  }
done
grep -q 'host-deps|assets|all' "$aggregate_builder" || fail 'host-deps component is not registered'
grep -q 'if want host-deps' "$aggregate_builder" || fail 'all build does not invoke host dependency builder'
grep -q 'install-host-dependencies.sh' "$aggregate_builder" || fail 'host dependency installer is not published'
grep -q 'Dir::Etc::sourcelist' "$installer" || fail 'installer does not isolate APT sources'
grep -q 'Dir::Etc::sourceparts=-' "$installer" || fail 'installer may consult host APT source parts'
grep -q -- "--sort=name" "$builder" || fail 'host dependency archive does not normalize member order'
grep -q -- "--mtime='@0'" "$builder" || fail 'host dependency archive does not normalize timestamps'
grep -q -- '--numeric-owner' "$builder" || fail 'host dependency archive does not normalize ownership'
grep -q -- 'gzip -n' "$builder" || fail 'host dependency archive gzip header is not deterministic'

if (($# == 1)); then
  archive="$1"
  [[ -f "$archive" && -f "$archive.sha256" ]] || fail 'test archive or sidecar missing'
  (cd "$(dirname -- "$archive")" && sha256sum --check "$(basename -- "$archive").sha256")
  gzip -t "$archive"
  tar -tzf "$archive" | awk '
    {sub(/^\.\//, ""); if ($0 == "" || $0 == ".") next}
    $0 ~ /^\// || $0 ~ /(^|\/)\.\.($|\/)/ {exit 1}
    $0 !~ /^(host-dependencies\.env|package-manifest\.tsv|Packages|Packages\.gz|SHA256SUMS|debs\/?|debs\/[A-Za-z0-9.+_:%~-]+\.deb)$/ {exit 1}
  ' || fail 'host dependency archive structure is unsafe'
  metadata="$(tar -xOf "$archive" host-dependencies.env)"
  grep -q '^OS_ID=ubuntu$' <<< "$metadata" || fail 'archive is not Ubuntu-bound'
  grep -q '^ARCHITECTURE=amd64$' <<< "$metadata" || fail 'archive is not amd64-bound'
  requested="$(awk -F= '$1 == "REQUESTED_PACKAGES" {sub(/^[^=]*=/, ""); print}' <<< "$metadata")"
  for package in jq curl ca-certificates tar gzip coreutils; do
    case ",$requested," in
      *",$package,"*) ;;
      *) fail "archive metadata omits required package: $package" ;;
    esac
  done
fi

echo 'host dependency packaging, APT isolation and archive metadata tests passed'
