#!/usr/bin/env bash
set -Eeuo pipefail
installer="$(cd "$(dirname "$0")/../bin" && pwd)/install-assets.sh"
fixture="$(mktemp -d)"
trap 'rm -rf -- "$fixture"' EXIT
mkdir -p "$fixture/source/bin" "$fixture/source/subsystems.d" "$fixture/tools"
printf 'fixture: true\n' > "$fixture/source/subsystems.d/fixture.yaml"
chmod 750 "$fixture/source/subsystems.d"
chmod 600 "$fixture/source/subsystems.d/fixture.yaml"
# macOS 单元夹具；锁本身由 Linux 部署测试覆盖。
if ! command -v flock >/dev/null; then
  printf '#!/bin/sh\nexit 0\n' > "$fixture/tools/flock"
  chmod +x "$fixture/tools/flock"
  export PATH="$fixture/tools:$PATH"
fi
cat > "$fixture/tools/docker" <<'EOF'
#!/bin/sh
case "${1:-}" in
  info) exit 0 ;;
  ps)
    if [ "${FAKE_DOCKER_CONTROL_PLANE_RUNNING:-false}" = true ]; then
      printf '%s\n' fake-control-plane-container
    fi
    exit 0
    ;;
esac
exit 1
EOF
chmod +x "$fixture/tools/docker"
export PATH="$fixture/tools:$PATH"
printf 'services: {}\n' > "$fixture/source/docker-compose.yml"
printf '#!/bin/bash\ntrue\n' > "$fixture/source/bin/deploy.sh"
tar -czf "$fixture/assets.tar.gz" -C "$fixture/source" .
(cd "$fixture" && sha256sum assets.tar.gz > assets.tar.gz.sha256)
bash "$installer" "$fixture/assets.tar.gz" "$fixture/target"

# 新入口引用后台helper时，资产必须完整；不能安装后才发现缺少排空工具。
printf 'source "${start_enabled_helper_dir}/project-background.sh"\n' > "$fixture/source/bin/start-enabled.sh"
tar -czf "$fixture/missing-background.tar.gz" -C "$fixture/source" .
(cd "$fixture" && sha256sum missing-background.tar.gz > missing-background.tar.gz.sha256)
if bash "$installer" "$fixture/missing-background.tar.gz" "$fixture/incomplete-background-target" >"$fixture/background-missing.log" 2>&1; then
  echo 'installer accepted an entrypoint without its required project background helper' >&2; exit 1
fi
grep -Fq 'project-background.sh' "$fixture/background-missing.log" || { echo 'missing background dependency was not diagnosed' >&2; exit 1; }
rm "$fixture/source/bin/start-enabled.sh"
cmp "$fixture/source/docker-compose.yml" "$fixture/target/docker-compose.yml"
[[ -d "$fixture/target/runtime-approvals" && ! -L "$fixture/target/runtime-approvals" ]]
[[ -z "$(find "$fixture/target/runtime-approvals" -mindepth 1 -print -quit)" ]] || { echo 'installer fabricated an approval' >&2; exit 1; }
[[ "$(stat -c '%a' "$fixture/target/runtime-approvals" 2>/dev/null || stat -f '%Lp' "$fixture/target/runtime-approvals")" == 700 ]]
[[ "$(find "$fixture/target/subsystems.d" -type d -perm 755 | wc -l | tr -d ' ')" == 1 ]]
[[ "$(find "$fixture/target/subsystems.d" -type f -perm 644 | wc -l | tr -d ' ')" == 1 ]]
printf '# operator module selection\nservices: {}\n' > "$fixture/target/docker-compose.yml"
cp "$fixture/target/docker-compose.yml" "$fixture/selected.yml"
printf 'PRIVATE_FIXTURE=unchanged\n' > "$fixture/target/.env"
cp "$fixture/target/.env" "$fixture/env.expected"
bash "$installer" "$fixture/assets.tar.gz" "$fixture/target"
cmp "$fixture/selected.yml" "$fixture/target/docker-compose.yml"
cmp "$fixture/source/docker-compose.yml" "$fixture/target/docker-compose.yml.dist"
cmp "$fixture/env.expected" "$fixture/target/.env"

# 许可证交付必须显式执行，并与可变运行凭据隔离。
mkdir -p "$fixture/source/license"
printf 'signed-license-transport-fixture\n' > "$fixture/source/license/commercial-license.jws"
printf '{"version":1,"scenario":"fresh","customer_id":"fixture-customer"}\n' > "$fixture/source/license-installation.json"
printf '{"version":1,"installation_boundary":true,"services":[]}\n' > "$fixture/source/license-migration-evidence.json"
tar -czf "$fixture/licensed.tar.gz" -C "$fixture/source" .
(cd "$fixture" && sha256sum licensed.tar.gz > licensed.tar.gz.sha256)
bash "$installer" "$fixture/licensed.tar.gz" "$fixture/licensed-target"
cmp "$fixture/source/license/commercial-license.jws" "$fixture/licensed-target/license/commercial-license.jws"
cmp "$fixture/source/license-installation.json" "$fixture/licensed-target/license-installation.json"
cmp "$fixture/source/license-migration-evidence.json" "$fixture/licensed-target/license-migration-evidence.json"
cmp "$fixture/source/license-migration-evidence.json" "$fixture/licensed-target/runtime-approvals/license-migration-evidence.json"
[[ "$(stat -c '%a' "$fixture/licensed-target/runtime-approvals/license-migration-evidence.json" 2>/dev/null || stat -f '%Lp' "$fixture/licensed-target/runtime-approvals/license-migration-evidence.json")" == 600 ]]
bash "$installer" "$fixture/licensed.tar.gz" "$fixture/licensed-target"

# 通用资产覆盖包不能替换另一份规范批准文件。
cp "$fixture/licensed-target/runtime-approvals/license-migration-evidence.json" "$fixture/approval.expected"
printf '{"version":1,"installation_boundary":true,"services":[],"project":"changed-fixture"}\n' > "$fixture/licensed-target/runtime-approvals/license-migration-evidence.json"
if bash "$installer" "$fixture/licensed.tar.gz" "$fixture/licensed-target" >/dev/null 2>&1; then
  echo 'installer overwrote a different canonical approval' >&2; exit 1
fi
grep -q '"project":"changed-fixture"' "$fixture/licensed-target/runtime-approvals/license-migration-evidence.json"
cp "$fixture/approval.expected" "$fixture/licensed-target/runtime-approvals/license-migration-evidence.json"
printf 'private-runtime\n' > "$fixture/licensed-target/runtime-approvals/.secret.env"
if bash "$installer" "$fixture/licensed.tar.gz" "$fixture/licensed-target" >/dev/null 2>&1; then
  echo 'installer accepted an unknown hidden file in approvals' >&2; exit 1
fi
rm "$fixture/licensed-target/runtime-approvals/.secret.env"

# 已审核发布的生命周期字段属于生产 Agent schema；通用未知字段和凭据仍必须按失败关闭原则拒绝。
cat > "$fixture/source/runtime-license-project_management-prod.json" <<'JSON'
{"version":1,"application":"project_management","environment":"prod","components":[],"release_generation":"project-r2","retire_service_ids":["project-legacy-worker"]}
JSON
tar -czf "$fixture/lifecycle.tar.gz" -C "$fixture/source" .
(cd "$fixture" && sha256sum lifecycle.tar.gz > lifecycle.tar.gz.sha256)
bash "$installer" "$fixture/lifecycle.tar.gz" "$fixture/lifecycle-target"
cmp "$fixture/source/runtime-license-project_management-prod.json" "$fixture/lifecycle-target/runtime-approvals/runtime-license-project_management-prod.json"
for invalid in '{"unknown_lifecycle":true}' '{"release_generation":3}' '{"retire_service_ids":["project-old","project-old"]}' '{"retire_service_ids":["../escape"]}' '{"client_secret":"never-deliver"}'; do
  jq --argjson extra "$invalid" '. + $extra' "$fixture/lifecycle-target/runtime-approvals/runtime-license-project_management-prod.json" \
    > "$fixture/source/runtime-license-project_management-prod.json"
  tar -czf "$fixture/lifecycle-invalid.tar.gz" -C "$fixture/source" .
  (cd "$fixture" && sha256sum lifecycle-invalid.tar.gz > lifecycle-invalid.tar.gz.sha256)
  if bash "$installer" "$fixture/lifecycle-invalid.tar.gz" "$fixture/lifecycle-invalid-target" >"$fixture/lifecycle-invalid.log" 2>&1; then
    echo 'installer accepted invalid release lifecycle metadata or credentials' >&2; exit 1
  fi
  grep -q '批准文件 JSON 格式无效或包含凭据字段' "$fixture/lifecycle-invalid.log"
done
rm "$fixture/source/runtime-license-project_management-prod.json"
mkdir -p "$fixture/symlink-target" "$fixture/outside-approvals"
ln -s "$fixture/outside-approvals" "$fixture/symlink-target/runtime-approvals"
if bash "$installer" "$fixture/licensed.tar.gz" "$fixture/symlink-target" >/dev/null 2>&1; then
  echo 'installer followed an approvals directory symlink' >&2; exit 1
fi
[[ -z "$(find "$fixture/outside-approvals" -mindepth 1 -print -quit)" ]]

# 仅为操作人选定的旧 Agent 添加一个只读挂载。
operator="$fixture/operator-target"
bash "$installer" "$fixture/assets.tar.gz" "$operator"
cat > "$operator/docker-compose.yml" <<'EOF'
# custom deployment selection
services:
  subsystem-provisioner:
    image: operator-approved-image
    volumes:
      - ./runtime:/app/operator-runtime
    networks: [application]
  preserved-custom-service:
    image: untouched-custom-image
EOF
bash "$installer" "$fixture/assets.tar.gz" "$operator"
[[ "$(grep -c 'runtime-approvals.*runtime-approvals:ro' "$operator/docker-compose.yml")" == 1 ]]
grep -q 'untouched-custom-image' "$operator/docker-compose.yml"
grep -q './runtime:/app/operator-runtime' "$operator/docker-compose.yml"
cp "$operator/docker-compose.yml" "$fixture/operator.expected"
bash "$installer" "$fixture/assets.tar.gz" "$operator"
cmp "$fixture/operator.expected" "$operator/docker-compose.yml"
printf 'other-customer\n' > "$fixture/source/license/commercial-license.jws"
tar -czf "$fixture/other-license.tar.gz" -C "$fixture/source" .
(cd "$fixture" && sha256sum other-license.tar.gz > other-license.tar.gz.sha256)
if bash "$installer" "$fixture/other-license.tar.gz" "$fixture/licensed-target" >/dev/null 2>&1; then
  echo 'asset update replaced existing license identity' >&2; exit 1
fi
grep -q '^signed-license-transport-fixture$' "$fixture/licensed-target/license/commercial-license.jws"
printf 'never-ship-private-key\n' > "$fixture/source/license/vendor-private.pem"
tar -czf "$fixture/license-secret.tar.gz" -C "$fixture/source" .
(cd "$fixture" && sha256sum license-secret.tar.gz > license-secret.tar.gz.sha256)
if bash "$installer" "$fixture/license-secret.tar.gz" "$fixture/secret-target" >/dev/null 2>&1; then
  echo 'license directory allowed private key' >&2; exit 1
fi
rm "$fixture/source/license/commercial-license.jws" "$fixture/source/license/vendor-private.pem"
rmdir "$fixture/source/license"

# 任一控制面进程运行时替换资产，都必须留下持久化的失败关闭标记；在 platform-api 和 Agent 成对重载消除此标记之前，拒绝后续覆盖安装。
running_target="$fixture/running-target"
bash "$installer" "$fixture/assets.tar.gz" "$running_target"
printf 'COMPOSE_PROJECT_NAME=fixture-production\n' > "$running_target/.env"
FAKE_DOCKER_CONTROL_PLANE_RUNNING=true \
  bash "$installer" "$fixture/assets.tar.gz" "$running_target"
reload_marker="$running_target/runtime/.control-plane-reload-required"
[[ -f "$reload_marker" && ! -L "$reload_marker" ]] || {
  echo 'running control-plane asset update did not create the reload marker' >&2; exit 1
}
grep -Eq '^ASSETS_ARCHIVE_SHA256=[a-f0-9]{64}$' "$reload_marker" || {
  echo 'reload marker is not bound to the installed asset archive' >&2; exit 1
}
if bash "$installer" "$fixture/assets.tar.gz" "$running_target" >"$fixture/reload-marker.log" 2>&1; then
  echo 'installer accepted another overlay while control-plane reload is pending' >&2; exit 1
fi
grep -q 'reload-control-plane' "$fixture/reload-marker.log" || {
  echo 'pending reload rejection does not provide the recovery command' >&2; exit 1
}
# 新增已审核文档不能将契约扩展为接受任意 Markdown。
printf 'unreviewed document\n' > "$fixture/source/UNKNOWN_DEPLOYMENT.md"
tar -czf "$fixture/unknown-document.tar.gz" -C "$fixture/source" .
(cd "$fixture" && sha256sum unknown-document.tar.gz > unknown-document.tar.gz.sha256)
if bash "$installer" "$fixture/unknown-document.tar.gz" "$fixture/unknown-target" >/dev/null 2>&1; then
  echo 'archive containing an unreviewed document must be rejected' >&2; exit 1
fi
[[ ! -e "$fixture/unknown-target" ]] || { echo 'rejected document archive changed the target' >&2; exit 1; }
rm "$fixture/source/UNKNOWN_DEPLOYMENT.md"
printf 'not-deployment-state\n' > "$fixture/source/.env"
tar -czf "$fixture/bad.tar.gz" -C "$fixture/source" .
(cd "$fixture" && sha256sum bad.tar.gz > bad.tar.gz.sha256)
if bash "$installer" "$fixture/bad.tar.gz" "$fixture/target" >/dev/null 2>&1; then
  echo 'archive containing runtime secrets must be rejected' >&2; exit 1
fi

# 其他文件名的伴随文件绝不能为当前归档提供授权。
cp "$fixture/assets.tar.gz.sha256" "$fixture/wrong-sidecar.tar.gz.sha256"
cp "$fixture/assets.tar.gz" "$fixture/wrong-sidecar.tar.gz"
if bash "$installer" "$fixture/wrong-sidecar.tar.gz" "$fixture/wrong-target" >/dev/null 2>&1; then
  echo 'sidecar bound to another archive name was accepted' >&2; exit 1
fi

# 模拟覆盖安装中断。再次安装必须拒绝混合状态；显式恢复应还原确切备份的顶层资产。
backup="$fixture/target/backups/install-assets.recovery-test"
transaction="$fixture/target/runtime/.assets-install-transaction"
mkdir -p "$backup" "$transaction"
cp -a "$fixture/target/bin" "$backup/bin"
printf '%s\n' "$backup" > "$transaction/backup"
printf 'bin\n' > "$transaction/touched"
printf 'bin\n' > "$transaction/existing"
printf 'applying\n' > "$transaction/state"
printf '#!/bin/bash\nexit 77\n' > "$fixture/target/bin/deploy.sh"
if bash "$installer" "$fixture/assets.tar.gz" "$fixture/target" >/dev/null 2>&1; then
  echo 'installer accepted an unfinished prior transaction' >&2; exit 1
fi
bash "$installer" --recover "$fixture/target"
cmp "$backup/bin/deploy.sh" "$fixture/target/bin/deploy.sh"
[[ ! -e "$transaction" ]] || { echo 'recovery transaction marker was not cleared' >&2; exit 1; }

# 即使中断安装留下新 deploy.sh 和旧资产，入口也必须在加载或操作混合目录树之前停止。
mkdir -p "$fixture/gate/bin" "$fixture/gate/runtime/.assets-install-transaction"
cp "$(dirname "$installer")/deploy.sh" "$fixture/gate/bin/deploy.sh"
if bash "$fixture/gate/bin/deploy.sh" help >"$fixture/gate.log" 2>&1; then
  echo 'deploy entry point accepted an unfinished asset transaction' >&2; exit 1
fi
grep -q '未完成的部署资产安装事务' "$fixture/gate.log" || { echo 'deploy mixed-state gate message missing' >&2; exit 1; }

# 使用 Docker CLI 能力替身构建真实部署资产包，不构建镜像，验证显式文件白名单、规范伴随文件、原子发布、不可变版本拒绝和临时文件清理。
builder="$(dirname "$installer")/build-offline-packages.sh"
mkdir -p "$fixture/build-tools" "$fixture/output"
printf '#!/bin/sh\n[ "$1" = buildx ] && [ "$2" = version ]\n' > "$fixture/build-tools/docker"
chmod +x "$fixture/build-tools/docker"
env PATH="$fixture/build-tools:$PATH" bash "$builder" \
  --component assets --version asset-safety-test --output "$fixture/output"
asset_archive="$fixture/output/deployment-assets-asset-safety-test.tar.gz"
(cd "$fixture/output" && sha256sum --check SHA256SUMS && sha256sum --check "$(basename "$asset_archive").sha256")
bootstrap_installer="$fixture/output/install-assets.sh"
[[ -x "$bootstrap_installer" && -f "$bootstrap_installer.sha256" && -f "$fixture/output/BUILD_INFO.txt" ]] || {
  echo 'assets build did not publish the independent installer, sidecar and build information' >&2; exit 1
}
(cd "$fixture/output" && sha256sum --check install-assets.sh.sha256)
cmp "$installer" "$bootstrap_installer"
asset_listing="$fixture/assets.list"
tar -tzf "$asset_archive" > "$asset_listing" || {
  echo '无法读取资产包清单' >&2; exit 1;
}
grep -qx './bin/publish-runtime-approvals.py' "$asset_listing" || {
  echo '资产包缺少运行批准发布器，approval-reload 无法执行' >&2; exit 1
}
if grep -Eq '(^|/)(\.env$|\.release\.env$|.*\.bak$|.*\.pem$|.*\.key$|dump[^/]*\.sql(\.gz)?$)' "$asset_listing"; then
  echo 'sensitive or runtime file entered the explicit asset package' >&2; exit 1
fi

# 安装真实构建器生成的原始纯资产归档，刻意不手工构造夹具。构建器和安装器白名单必须维持同一兼容契约，
# 包括权威 OFFLINE_RUNBOOK.md 及其中断安装恢复路径。
real_target="$fixture/real-target"
bash "$bootstrap_installer" "$asset_archive" "$real_target"
cmp "$(dirname "$installer")/publish-runtime-approvals.py" "$real_target/bin/publish-runtime-approvals.py" || {
  echo '安装后的运行批准发布器与资产源文件不一致' >&2; exit 1
}
cmp "$(dirname "$installer")/project-background.sh" "$real_target/bin/project-background.sh"
bash -n "$real_target/bin/project-background.sh"
cmp "$(dirname "$installer")/../OFFLINE_RUNBOOK.md" "$real_target/OFFLINE_RUNBOOK.md"
cmp "$(dirname "$installer")/../CONTAINER_TIMEZONE.md" "$real_target/CONTAINER_TIMEZONE.md"
cmp "$(dirname "$installer")/../ACCEPTANCE_CHECKLIST.md" "$real_target/ACCEPTANCE_CHECKLIST.md"
cmp "$(dirname "$installer")/acceptance-evidence.sh" "$real_target/bin/acceptance-evidence.sh"
cmp "$(dirname "$installer")/../tests/test-acceptance-evidence.sh" "$real_target/tests/test-acceptance-evidence.sh"
cmp "$(dirname "$installer")/../docker-compose.yml" "$real_target/docker-compose.yml"
[[ "$(stat -c '%a' "$real_target/bin/acceptance-evidence.sh" 2>/dev/null || stat -f '%Lp' "$real_target/bin/acceptance-evidence.sh")" == 750 ]] || {
  echo 'installed acceptance evidence script mode is not 0750' >&2; exit 1
}
[[ ! -e "$real_target/runtime/.assets-install-transaction" ]] || {
  echo 'real asset package left an install transaction after success' >&2; exit 1
}
real_backup="$real_target/backups/install-assets.runbook-recovery"
real_transaction="$real_target/runtime/.assets-install-transaction"
mkdir -p "$real_backup" "$real_transaction"
cp -p "$real_target/OFFLINE_RUNBOOK.md" "$real_backup/OFFLINE_RUNBOOK.md"
cp -p "$real_target/CONTAINER_TIMEZONE.md" "$real_backup/CONTAINER_TIMEZONE.md"
printf '%s\n' "$real_backup" > "$real_transaction/backup"
printf 'OFFLINE_RUNBOOK.md\nCONTAINER_TIMEZONE.md\n' > "$real_transaction/touched"
printf 'OFFLINE_RUNBOOK.md\nCONTAINER_TIMEZONE.md\n' > "$real_transaction/existing"
printf 'applying\n' > "$real_transaction/state"
printf 'interrupted replacement\n' > "$real_target/OFFLINE_RUNBOOK.md"
printf 'interrupted timezone documentation replacement\n' > "$real_target/CONTAINER_TIMEZONE.md"
bash "$installer" --recover "$real_target"
cmp "$real_backup/OFFLINE_RUNBOOK.md" "$real_target/OFFLINE_RUNBOOK.md"
cmp "$real_backup/CONTAINER_TIMEZONE.md" "$real_target/CONTAINER_TIMEZONE.md"
[[ ! -e "$real_transaction" ]] || {
  echo 'real package runbook recovery transaction was not cleared' >&2; exit 1
}

asset_digest_before="$(sha256sum "$asset_archive" | awk '{print $1}')"
env PATH="$fixture/build-tools:$PATH" bash "$builder" \
  --component assets --version asset-safety-test --output "$fixture/output"
[[ "$(sha256sum "$asset_archive" | awk '{print $1}')" == "$asset_digest_before" ]] || {
  echo 'equivalent assets rebuild replaced the immutable published package' >&2; exit 1
}

printf 'corrupt existing artifact\n' >> "$asset_archive"
if env PATH="$fixture/build-tools:$PATH" bash "$builder" \
  --component assets --version asset-safety-test --output "$fixture/output" >/dev/null 2>&1; then
  echo 'builder overwrote a different artifact under the same immutable version' >&2; exit 1
fi
if find "$fixture/output" -maxdepth 1 \( -name '.*.archive.*' -o -name '.*.sidecar.*' -o -name '.deployment-assets-stage.*' -o -name '.build-info.*' -o -name '.build-info-check.*' -o -name '.asset-equivalence.*' -o -name '.image-equivalence.*' -o -name '.install-assets.bootstrap.*' \) -print -quit | grep -q .; then
  echo 'failed asset build leaked temporary files' >&2; exit 1
fi

echo 'PASS: real allowlisted asset package install, atomic transaction, strict sidecar binding and explicit recovery'
