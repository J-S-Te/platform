#!/usr/bin/env bash
set -Eeuo pipefail
ROOT="$(cd -- "$(dirname -- "$0")/.." && pwd)"
SCRIPT="$ROOT/bin/truncate-container-logs.sh"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/truncate-logs-test.XXXXXX")"
trap 'rm -rf -- "$WORK"' EXIT
pass=0; fail=0
check() { if eval "$2"; then echo "PASS: $1"; pass=$((pass+1)); else echo "FAIL: $1"; fail=$((fail+1)); fi; }

mkdir -p "$WORK/containers/aaa" "$WORK/containers/bbb"
printf 'x%.0s' {1..2048} >"$WORK/containers/aaa/aaa-json.log"
printf 'y%.0s' {1..1024} >"$WORK/containers/bbb/bbb-json.log"

out="$(DOCKER_ROOT="$WORK" bash "$SCRIPT" 2>&1)" || true
check "dry-run reports logs" "grep -q 'aaa-json.log' <<<\"\$out\""
check "dry-run does not truncate" "[[ \$(wc -c < \"$WORK/containers/aaa/aaa-json.log\" | tr -d \" \") -eq 2048 ]]"
check "dry-run message" "grep -q 'dry-run: nothing was truncated' <<<\"\$out\""

out="$(DOCKER_ROOT="$WORK" bash "$SCRIPT" --apply 2>&1)" || true
check "apply truncates logs" "[[ \$(wc -c < \"$WORK/containers/aaa/aaa-json.log\" | tr -d \" \") -eq 0 ]]"
check "apply truncates second log" "[[ \$(wc -c < \"$WORK/containers/bbb/bbb-json.log\" | tr -d \" \") -eq 0 ]]"
check "apply message" "grep -q 'applied: truncated container logs' <<<\"\$out\""

check "missing root fails" "! DOCKER_ROOT=\"$WORK/none\" bash \"$SCRIPT\" >/dev/null 2>&1"
check "unknown arg fails" "! DOCKER_ROOT=\"$WORK\" bash \"$SCRIPT\" --bogus >/dev/null 2>&1"

echo "tests: pass=$pass fail=$fail"
[[ "$fail" -eq 0 ]]
