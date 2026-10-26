#!/usr/bin/env bash
set -Eeuo pipefail

bin_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/uip-recovery-status.XXXXXX")"
trap 'rm -rf -- "$test_root"' EXIT

extract_function() {
  local name="$1" file="$2"
  awk -v signature="${name}() {" '
    $0 == signature {inside=1}
    inside {print}
    inside && $0 == "}" {exit}
  ' "$file"
}

eval "$(extract_function record_recovery_result "$bin_dir/deploy-service.sh")"
deploy_dir="$test_root/deploy"
service=platform

record_recovery_result recovered '上一镜像已恢复并通过镜像和健康核验'
python3 - "$deploy_dir/runtime/recovery-status.json" <<'PY'
import json, stat, sys
path = sys.argv[1]
value = json.load(open(path, encoding="utf-8"))
assert value["schema"] == "uip.recovery-status/v1", value
assert value["status"] == "recovered", value
assert value["service"] == "platform", value
assert value["verified"] is True, value
assert "secret" not in json.dumps(value).lower(), value
assert value["migration"] == "unknown", value
assert stat.S_IMODE(__import__("os").stat(path).st_mode) == 0o600, oct(__import__("os").stat(path).st_mode)
PY

migration_state=completed
record_recovery_result recovered '迁移后恢复，迁移已完成'
python3 - "$deploy_dir/runtime/recovery-status.json" <<'PY'
import json, sys
value = json.load(open(sys.argv[1], encoding="utf-8"))
assert value["migration"] == "completed", value
PY

record_recovery_result failed '上一镜像恢复未通过镜像/健康/配置核验'
python3 - "$deploy_dir/runtime/recovery-status.json" <<'PY'
import json, sys
value = json.load(open(sys.argv[1], encoding="utf-8"))
assert value["status"] == "failed", value
assert value["verified"] is False, value
PY

record_recovery_result not_applicable '首次安装或无可回滚的不可变镜像'
python3 - "$deploy_dir/runtime/recovery-status.json" <<'PY'
import json, sys
value = json.load(open(sys.argv[1], encoding="utf-8"))
assert value["status"] == "not_applicable", value
assert value["verified"] is False, value
PY

printf 'recovery status evidence: PASS\n'
