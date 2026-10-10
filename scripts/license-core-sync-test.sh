#!/bin/sh
set -eu
platform_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
test_root=$(mktemp -d)
trap 'rm -rf -- "$test_root"' EXIT HUP INT TERM
mkdir -p "$test_root/platform/scripts" "$test_root/platform/third_party"
cp "$platform_root/scripts/license-core-sync.sh" "$platform_root/scripts/license-core.sha256" "$test_root/platform/scripts/"
cp -R "$platform_root/third_party/license-core" "$test_root/platform/third_party/"
check="$test_root/platform/scripts/license-core-sync.sh"
must_fail() {
  if sh "$check" --check >/dev/null 2>&1; then
    echo "expected drift rejection: $1" >&2
    exit 1
  fi
}
# Standalone CI does not have a sibling authoritative module.
sh "$check" --check
cp "$test_root/platform/third_party/license-core/clock.go" "$test_root/platform/third_party/license-core/issuer.go"
must_fail 'non-core file in consumer snapshot'
rm "$test_root/platform/third_party/license-core/issuer.go"
cp -R "$platform_root/third_party/license-core" "$test_root/license-core"
sh "$check" --check
cp "$test_root/license-core/clock.go" "$test_root/license-core/license.go"
must_fail 'authoritative source differs from snapshot'
cp "$platform_root/third_party/license-core/license.go" "$test_root/license-core/license.go"
cp "$test_root/license-core/clock.go" "$test_root/license-core/unreviewed.go"
must_fail 'unreviewed authoritative source'
rm "$test_root/license-core/unreviewed.go"
cp "$test_root/license-core/clock.go" "$test_root/license-core/runtime/unreviewed.go"
must_fail 'unreviewed nested authoritative source'
rm "$test_root/license-core/runtime/unreviewed.go"
cp "$test_root/license-core/clock.go" "$test_root/license-core/runtime/snapshot.go"
must_fail 'nested authoritative source differs from snapshot'
cp "$platform_root/third_party/license-core/runtime/snapshot.go" "$test_root/license-core/runtime/snapshot.go"
ln -s clock.go "$test_root/platform/third_party/license-core/unreviewed-link.go"
must_fail 'symlink in consumer snapshot'
rm "$test_root/platform/third_party/license-core/unreviewed-link.go"
cp "$test_root/license-core/clock.go" "$test_root/platform/third_party/license-core/license.go"
must_fail 'snapshot content differs from manifest'
cp "$platform_root/third_party/license-core/license.go" "$test_root/platform/third_party/license-core/license.go"
mkdir -p "$test_root/platform/vendor/github.com/J-S-Te"
cp -R "$test_root/license-core" "$test_root/platform/vendor/github.com/J-S-Te/"
rm "$test_root/platform/vendor/github.com/J-S-Te/license-core/go.mod"
sh "$check" --check
cp "$test_root/license-core/clock.go" "$test_root/platform/vendor/github.com/J-S-Te/license-core/runtime/snapshot.go"
must_fail 'nested vendor differs from snapshot'
cp "$test_root/license-core/runtime/snapshot.go" "$test_root/platform/vendor/github.com/J-S-Te/license-core/runtime/snapshot.go"
cp "$test_root/license-core/clock.go" "$test_root/platform/vendor/github.com/J-S-Te/license-core/runtime/runtime_test.go"
must_fail 'non-production file in vendor'
rm "$test_root/platform/vendor/github.com/J-S-Te/license-core/runtime/runtime_test.go"
cp "$test_root/license-core/clock.go" "$test_root/platform/vendor/github.com/J-S-Te/license-core/license.go"
must_fail 'vendor differs from snapshot'
echo 'license core snapshot, authority, standalone and vendor checks passed'
