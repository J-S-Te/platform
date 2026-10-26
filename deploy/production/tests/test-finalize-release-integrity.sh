#!/usr/bin/env bash
set -Eeuo pipefail

root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../../../.." && pwd)"
platform="$root/platform"
deploy="$platform/deploy/production"
work="$(mktemp -d "${TMPDIR:-/tmp}/uip-finalize-test.XXXXXX")"
trap 'rm -rf -- "$work"' EXIT
media="$work/media"
profiles="$work/profiles"
workspace="$work/workspace"
mkdir -p "$media" "$profiles" "$workspace/platform"

config_digest="sha256:$(printf 'a%.0s' {1..64})"
package_name="platform-backend-test-linux-amd64.tar.gz"
printf 'PACKAGE_FORMAT=2\nCOMPONENT=platform\nVERSION=test\nPLATFORM=linux/amd64\nIMAGES=uip-package/platform:test\nIMAGE_CONFIG_DIGESTS=%s\n' \
  "$config_digest" > "$work/package.env"
tar -czf "$media/$package_name" -C "$work" package.env
package_sha="$(sha256sum "$media/$package_name" | awk '{print $1}')"
package_size="$(wc -c < "$media/$package_name" | tr -d ' ')"
printf '%s  %s\n' "$package_sha" "$package_name" > "$media/$package_name.sha256"
printf 'RELEASE_VERSION=test\nTARGET_PLATFORM=linux/amd64\nDOCKER_CLIENT_VERSION=29\nDOCKER_SERVER_VERSION=29\nBUILDX_VERSION=buildx\n' > "$media/BUILD_INFO.txt"
printf 'FROM alpine@sha256:%s\n' "$(printf 'b%.0s' {1..64})" > "$workspace/platform/Dockerfile"
dockerfile_sha="$(sha256sum "$workspace/platform/Dockerfile" | awk '{print $1}')"
dockerfile_size="$(wc -c < "$workspace/platform/Dockerfile" | tr -d ' ')"
printf '%s\t%s\tplatform/Dockerfile\n' "$dockerfile_sha" "$dockerfile_size" > "$media/source-input-index.tsv"
build_evidence="$media/build-evidence.json"
install -m 755 "$root/清理首次部署产物.sh" "$media/清理首次部署产物.sh"
install -m 644 "$root/操作说明/失败首装清理与重新部署.md" "$media/FIRST_INSTALL_RESET.md"
python3 - "$build_evidence" "$package_name" "$package_sha" "$package_size" "$config_digest" "$dockerfile_sha" <<'PY'
import json, sys
path, package, package_sha, package_size, config, dockerfile_sha = sys.argv[1:]
json.dump({
    "schema": "urn:uip:build-evidence:v1", "version": "test", "target_platform": "linux/amd64",
    "build_type": "https://uip.dev/buildtypes/container-v1", "source_input_sha256": "c" * 64,
    "builder": {"id": "builder://uip/test", "invocation_id": "inv-test"},
    "packages": [{"component": "platform", "archive": package, "sha256": package_sha,
                  "size": int(package_size), "images": [{"tag": "uip-package/platform:test", "config_digest": config}]}],
    "dockerfiles": [{"path": "platform/Dockerfile", "sha256": dockerfile_sha,
                     "from": ["alpine@sha256:" + "b" * 64], "all_base_refs_pinned": True}],
    "formal_signing_allowed": True,
}, open(path, "w", encoding="utf-8"), separators=(",", ":"))
PY
for index in a b c d e f; do printf 'profile: %s\n' "$index" > "$profiles/$index.yaml"; done
python3 - "$media/spec.json" <<'PY'
import json, sys
json.dump({
    "schema": "urn:uip:release-spec:v1", "release_id": "test", "channel": "production", "release_sequence": 1,
    "build_inputs_sha256": "c" * 64,
    "compatibility": {"verifier_policy": "vendor-key-offline-v1", "min_api_release_integrity": "release-integrity-v1",
                      "min_agent_release_integrity": "release-integrity-v1", "catalog_version": 1,
                      "registration_protocols": {"data_analysis": 2}, "shared_control_plane_sha256": "d" * 64},
    "images": [{"name": "platform", "tag": "uip-package/platform:test", "package": "platform-backend-test-linux-amd64.tar.gz", "archive_member": "images.tar"}],
    "applications": [{"code": "data_analysis", "environment": "prod", "components": [{
        "service_id": "data-analysis-api", "role": "api", "conditional": False, "image": "platform",
        "registration_protocol": 2, "catalog_version": 1, "replicas": 1, "command_policy": "locked"}]}],
}, open(sys.argv[1], "w", encoding="utf-8"), separators=(",", ":"))
PY
openssl ecparam -name prime256v1 -genkey -noout -out "$work/build.key"
openssl ec -in "$work/build.key" -pubout -out "$work/build.pub" 2>/dev/null
openssl ecparam -name prime256v1 -genkey -noout -out "$work/release.key"
openssl ec -in "$work/release.key" -pubout -out "$work/release.pub" 2>/dev/null
chmod 600 "$work/build.key" "$work/release.key"
mkdir -p "$work/trust"
python3 - "$work/trust/trust-policy.json" "$work/build.pub" "$work/release.pub" <<'PY'
import json, sys
policy = {
    "schema": "urn:uip:release-trust:v1", "policy": "vendor-key-offline-v1", "trust_epoch": 1,
    "release_keys": [{"id": "test-release", "algorithm": "ecdsa-p256", "public_key_pem": open(sys.argv[3], encoding="utf-8").read()}],
    "build_keys": [{"id": "test-build", "algorithm": "ecdsa-p256", "public_key_pem": open(sys.argv[2], encoding="utf-8").read()}],
    "allowed_builders": ["builder://uip/test"], "allowed_build_types": ["https://uip.dev/buildtypes/container-v1"],
    "min_release_sequence": 1,
}
json.dump(policy, open(sys.argv[1], "w", encoding="utf-8"), separators=(",", ":"))
PY
(
  cd "$media"
  sha256sum "$package_name" "$package_name.sha256" BUILD_INFO.txt source-input-index.tsv build-evidence.json 清理首次部署产物.sh FIRST_INSTALL_RESET.md > SHA256SUMS
)
(cd "$platform" && go build -trimpath -o "$work/release-verifier" ./cmd/release-verifier)
bash "$deploy/bin/finalize-release-integrity.sh" \
  --media "$media" --spec "$media/spec.json" --profiles-dir "$profiles" \
  --build-key "$work/build.key" --release-key "$work/release.key" \
  --trust-dir "$work/trust" --verifier "$work/release-verifier"
[[ -f "$media/release-manifest.json" && -f "$media/release-manifest.sigstore.json" ]]
[[ -d "$media/attestations" && -f "$media/attestations/platform.intoto.sigstore.json" ]]
[[ -f "$media/profiles/a.yaml" && -f "$media/profiles/f.yaml" ]]
"$work/release-verifier" verify --media "$media" --trust-dir "$work/trust" --output json | grep -q '"status": "VERIFIED"'
python3 - "$media/release-manifest.json" <<'PY'
import json, sys
manifest=json.load(open(sys.argv[1]))
artifacts={item['path']:item for item in manifest['artifacts']}
assert artifacts['清理首次部署产物.sh']['type']=='script'
assert 'FIRST_INSTALL_RESET.md' in artifacts
PY
# Exercise the facade with the real verifier and ephemeral test-only keys.
python3 "$deploy/bin/release-workflow.py" package --output "$media" --version signed-test \
  --verifier "$work/release-verifier" --trust-dir "$work/trust"
[[ -f "$work/uip-offline-delivery-signed-test-linux-amd64.tar" ]]
cp "$media/release-manifest.sigstore.json" "$work/original-signature.json"
printf 'invalid signature JSON\n' > "$media/release-manifest.sigstore.json"
if python3 "$deploy/bin/release-workflow.py" package --output "$media" --version rejected-test \
  --verifier "$work/release-verifier" --trust-dir "$work/trust" > "$work/package-rejected.log" 2>&1; then
  echo 'invalid signature unexpectedly passed package verification' >&2; exit 1
fi
[[ ! -e "$work/uip-offline-delivery-rejected-test-linux-amd64.tar" ]]
cp "$work/original-signature.json" "$media/release-manifest.sigstore.json"
printf '\n# tampered fixture\n' >> "$media/清理首次部署产物.sh"
if "$work/release-verifier" verify --media "$media" --trust-dir "$work/trust" --output json > "$work/tampered.json"; then
  echo 'tampered reset tool unexpectedly passed release verification' >&2
  exit 1
fi
echo 'finalize-release-integrity regression: PASS'
