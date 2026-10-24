#!/usr/bin/env bash
# 验证服务器一键入口中显式启用的发布完整性门禁；门禁必须先于任何介质脚本或依赖安装执行。
set -Eeuo pipefail

[[ "$(uname -s)" == Linux ]] || { echo 'SKIP: release-integrity gate test requires Linux'; exit 0; }
[[ "$(id -u)" == 0 ]] || { echo 'SKIP: release-integrity gate test requires root'; exit 0; }

root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../../../.." && pwd)"
entry="$root/服务器一键离线部署.sh"
[[ -f "$entry" && ! -L "$entry" ]]

tmp="$(mktemp -d "${TMPDIR:-/tmp}/uip-release-gate-test.XXXXXX")"
cleanup() { rm -rf -- "$tmp"; }
trap cleanup EXIT

media="$tmp/media"
trust="$tmp/trust"
target="$tmp/target"
mkdir -p "$media" "$trust" "$target"

cat > "$tmp/verifier-ok" <<'SH'
#!/usr/bin/env bash
printf 'called\n' >> "$GATE_LOG"
printf '{"status":"VERIFIED"}\n'
SH
cat > "$tmp/verifier-reject" <<'SH'
#!/usr/bin/env bash
printf '{"status":"FAILED","code":"RELEASE_SIGNATURE_INVALID","message":"bad signature"}\n'
exit 1
SH
chmod 700 "$tmp/verifier-ok" "$tmp/verifier-reject"
export GATE_LOG="$tmp/gate.log"

run_gate() { # verifier out_file
  local verifier="$1" out="$2"
  set +e
  bash "$entry" --media "$media" --target "$target" --check-only \
    --require-release-integrity \
    --release-verifier "$verifier" \
    --release-trust-dir "$trust" >"$out" 2>&1
  local status=$?
  set -e
  printf '%s\n' "$status"
}

status="$(run_gate "$tmp/verifier-ok" "$tmp/ok.log")"
[[ "$status" == 1 ]] || { cat "$tmp/ok.log"; echo "[FAIL] expected later preflight failure after gate"; exit 1; }
grep -Fq '发布完整性验签通过' "$tmp/ok.log" || { cat "$tmp/ok.log"; echo '[FAIL] gate success line missing'; exit 1; }
[[ -f "$GATE_LOG" ]] || { echo '[FAIL] trusted verifier was not called'; exit 1; }

status="$(run_gate "$tmp/verifier-reject" "$tmp/reject.log")"
[[ "$status" == 1 ]] || { cat "$tmp/reject.log"; echo '[FAIL] rejected verifier must fail closed'; exit 1; }
grep -Fq 'RELEASE_SIGNATURE_INVALID' "$tmp/reject.log" || { cat "$tmp/reject.log"; echo '[FAIL] rejection code missing'; exit 1; }
! grep -Fq '缺少命令：python3' "$tmp/reject.log" || { cat "$tmp/reject.log"; echo '[FAIL] media preflight ran before signature rejection'; exit 1; }

cp "$tmp/verifier-reject" "$media/verifier"
status="$(run_gate "$media/verifier" "$tmp/inside.log")"
[[ "$status" == 1 ]] || { cat "$tmp/inside.log"; echo '[FAIL] verifier under media must be rejected'; exit 1; }
grep -Fq 'release-verifier 不能来自待验证介质' "$tmp/inside.log" || { cat "$tmp/inside.log"; echo '[FAIL] inside-media verifier diagnostic missing'; exit 1; }

touch "$tmp/archive.tar"
set +e
bash "$entry" --media "$tmp/archive.tar" --target "$target" --check-only \
  --require-release-integrity --release-verifier "$tmp/verifier-ok" \
  --release-trust-dir "$trust" >"$tmp/tar.log" 2>&1
status=$?
set -e
[[ "$status" == 1 ]] || { cat "$tmp/tar.log"; echo '[FAIL] archive media must be rejected for this gate'; exit 1; }
grep -Fq '必须是已解压目录' "$tmp/tar.log" || { cat "$tmp/tar.log"; echo '[FAIL] archive diagnostic missing'; exit 1; }


# workflow_assets_install 在门禁通过后执行快照写入器，此处直接验证逐字节复制和冲突拒绝。
die() { printf '%s\n' "$*" >&2; exit 1; }
snapshot_fn="$(awk '/^install_release_integrity_snapshot\(\) \{/{flag=1} flag{print} flag && /^}$/{exit}' "$entry")"
[[ -n "$snapshot_fn" ]] || { echo '[FAIL] snapshot function not found'; exit 1; }
eval "$snapshot_fn"
mkdir -p "$media/attestations"
printf 'manifest\n' > "$media/release-manifest.json"
printf 'signature\n' > "$media/release-manifest.sigstore.json"
printf 'statement\n' > "$media/attestations/platform.intoto.json"
require_release_integrity=true
media_dir="$media"
target_dir="$target"
install_release_integrity_snapshot
snapshot_dir="$target/runtime/release-integrity"
cmp "$media/release-manifest.json" "$snapshot_dir/release-manifest.json"
cmp "$media/release-manifest.sigstore.json" "$snapshot_dir/release-manifest.sigstore.json"
cmp "$media/attestations/platform.intoto.json" "$snapshot_dir/attestations/platform.intoto.json"
printf 'changed\n' > "$media/release-manifest.json"
if ( install_release_integrity_snapshot ) >/dev/null 2>&1; then
  echo '[FAIL] changed installed snapshot must be refused'; exit 1
fi
printf 'manifest\n' > "$media/release-manifest.json"
install_release_integrity_snapshot

echo 'release-integrity gate regression: PASS'
