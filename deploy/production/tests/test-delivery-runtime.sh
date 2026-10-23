#!/usr/bin/env bash
set -Eeuo pipefail
test_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
exec python3 "$test_dir/test-delivery-runtime.py"
