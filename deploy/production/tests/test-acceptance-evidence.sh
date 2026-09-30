#!/usr/bin/env bash
set -Eeuo pipefail

production_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
collector="$production_dir/bin/acceptance-evidence.sh"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/acceptance-evidence-test.XXXXXX")"
trap 'rm -rf -- "$test_root"' EXIT

delivery="$test_root/delivery"
mkdir -p "$delivery"
printf 'package payload\n' >"$delivery/component-linux-amd64.tar.gz"
(cd -- "$delivery" && sha256sum component-linux-amd64.tar.gz >SHA256SUMS)

"$collector" delivery-gate --output "$test_root/good-evidence" --package-dir "$delivery"
grep -F $'PASS\tdelivery-sha256\t' "$test_root/good-evidence/SUMMARY.tsv" >/dev/null
grep -F '自动门禁失败：0' "$test_root/good-evidence/ACCEPTANCE_REPORT.md" >/dev/null
[[ "$(stat -c '%a' "$test_root/good-evidence/ACCEPTANCE_REPORT.md" 2>/dev/null || stat -f '%Lp' "$test_root/good-evidence/ACCEPTANCE_REPORT.md")" == 600 ]]

if "$collector" delivery-gate --output "$test_root/reused" --package-dir "$delivery" >/dev/null 2>&1; then
  if "$collector" delivery-gate --output "$test_root/reused" --package-dir "$delivery" >/dev/null 2>&1; then
    echo 'collector overwrote an existing evidence directory' >&2
    exit 1
  fi
else
  echo 'collector unexpectedly failed the first reusable-path test' >&2
  exit 1
fi

printf 'tampered\n' >>"$delivery/component-linux-amd64.tar.gz"
if "$collector" delivery-gate --output "$test_root/bad-evidence" --package-dir "$delivery" >/dev/null 2>&1; then
  echo 'collector accepted a damaged delivery package' >&2
  exit 1
fi
grep -F $'FAIL\tdelivery-sha256\t' "$test_root/bad-evidence/SUMMARY.tsv" >/dev/null

rm -f "$delivery/component-linux-amd64.tar.gz"
printf 'PRIVATE KEY\n' >"$delivery/production.key"
sha256sum "$delivery/production.key" | sed 's#  .*/#  #' >"$delivery/SHA256SUMS"
if "$collector" delivery-gate --output "$test_root/secret-evidence" --package-dir "$delivery" >/dev/null 2>&1; then
  echo 'collector accepted an obvious private-key artifact' >&2
  exit 1
fi
grep -F $'FAIL\tforbidden-artifacts\t' "$test_root/secret-evidence/SUMMARY.tsv" >/dev/null
if grep -R -F 'PRIVATE KEY' "$test_root/secret-evidence" >/dev/null; then
  echo 'collector copied sensitive file contents into evidence' >&2
  exit 1
fi

if grep -En 'docker (rm|rmi|volume rm|network rm|system prune)|iptables|nft |restore-mysql|restore-file-gateway' "$collector" >/dev/null; then
  echo 'read-only collector contains a destructive or firewall mutation command' >&2
  exit 1
fi

echo 'acceptance evidence collector tests passed'
