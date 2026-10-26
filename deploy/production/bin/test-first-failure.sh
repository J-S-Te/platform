#!/usr/bin/env bash
set -Eeuo pipefail

bin_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
root="$(mktemp -d "${TMPDIR:-/tmp}/uip-first-failure.XXXXXX")"
trap 'rm -rf -- "$root"' EXIT

extract_function() {
  local name="$1" file="$2"
  awk -v signature="${name}() {" '
    $0 == signature {inside=1}
    inside {print}
    inside && $0 == "}" {exit}
  ' "$file"
}

eval "$(extract_function record_first_failure "$bin_dir/deployment-workflow.sh")"
UIP_INSTALL_TARGET="$root/target"
UIP_INSTALL_SESSION=0123456789abcdef0123456789abcdef
mkdir -p "${UIP_INSTALL_TARGET}/runtime"

record_first_failure assets COMPATIBILITY_BLOCKED
file="${UIP_INSTALL_TARGET}/runtime/first-failure.json"
python3 - "$file" <<'PY'
import json, os, stat, sys
value = json.load(open(sys.argv[1], encoding="utf-8"))
assert value["schema"] == "uip.first-failure/v1", value
assert value["stage"] == "assets", value
assert value["reason"] == "COMPATIBILITY_BLOCKED", value
assert value["session"] == "0123456789abcdef0123456789abcdef", value
assert stat.S_IMODE(os.stat(sys.argv[1]).st_mode) == 0o600
PY

record_first_failure platform STAGE_FAILED
python3 - "$file" <<'PY'
import json, sys
value = json.load(open(sys.argv[1], encoding="utf-8"))
assert value["stage"] == "assets", value
assert value["reason"] == "COMPATIBILITY_BLOCKED", value
PY

[[ -z "$(find "${UIP_INSTALL_TARGET}/runtime" -maxdepth 1 -name '.first-failure.*' -print -quit)" ]] || {
  echo 'first failure left a temporary file' >&2; exit 1;
}
printf 'first failure preservation: PASS\n'
