#!/usr/bin/env python3
"""deploy-service.sh 须自行写入一次性 IAM 导入凭据，不依赖 deploy.sh 的函数。

平台发布在独立的 deploy-service.sh 进程内调用 provision_iam_import_client。
deploy.sh 虽定义了 env_set，但子脚本不会继承 Shell 函数；若缺少本地定义，
首次平台发布会在客户端密钥已签发后中断。
本测试提取脚本实际定义的辅助函数，
验证 .env 文件的原子写入。
"""
import re
import stat
import subprocess
import tempfile
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
SCRIPT = (ROOT / "bin" / "deploy-service.sh").read_text()


def function(name):
    match = re.search(r"^" + re.escape(name) + r"\(\) \{\n.*?^\}", SCRIPT, re.MULTILINE | re.DOTALL)
    assert match, f"deploy-service.sh must define {name} locally"
    return match.group()


env_value = function("env_value")
env_set = function("env_set")

with tempfile.TemporaryDirectory(prefix="deploy-service-env-set-") as directory:
    deploy_dir = Path(directory)
    env_file = deploy_dir / ".env"
    env_file.write_text("# managed comment\nexisting=old\n\nTAIL=1\n")
    env_file.chmod(0o600)

    harness = (
        'deploy_dir="$1"\n'
        + env_value
        + "\n"
        + env_set
        + r'''
env_file="$deploy_dir/.env"
env_set "$env_file" existing 'new=value with spaces \ and $ dollar' || exit 10
env_set "$env_file" added 'a b c' || exit 11
if env_set "$env_file" bad $'line1\nline2'; then exit 12; fi
'''
    )
    process = subprocess.run(
        ["bash", "-Eeuo", "pipefail", "-c", harness, "bash", str(deploy_dir)],
        capture_output=True,
        text=True,
    )
    assert process.returncode == 0, process.stdout + process.stderr

    lines = env_file.read_text().splitlines()
    assert "# managed comment" in lines, lines
    assert "existing=new=value with spaces \\ and $ dollar" in lines, lines
    assert "added=a b c" in lines, lines
    assert "TAIL=1" in lines, lines
    assert "bad=" not in "\n".join(lines), lines
    assert stat.S_IMODE(env_file.stat().st_mode) == 0o600, oct(stat.S_IMODE(env_file.stat().st_mode))
    assert not list(deploy_dir.glob(".env-update.*")), "atomic write must not leave temporary files"

print("deploy-service env_set regression passed")

# 复现首次平台发布的真实调用路径：provision_iam_import_client 必须在独立
# deploy-service.sh 进程内完成凭据写回，并在第二次调用时保持幂等。
iam_import_client_configured = function("iam_import_client_configured")
provision_iam_import_client = function("provision_iam_import_client")

with tempfile.TemporaryDirectory(prefix="deploy-service-iam-import-") as directory:
    deploy_dir = Path(directory)
    env_file = deploy_dir / ".env"
    env_file.write_text(
        "SUBSYSTEM_PRODUCTION_ALLOWED_TENANT_ID=tenant-1\n"
        "IAM_IMPORT_FILE_GATEWAY_CLIENT_ID=REPLACE_WITH_CLIENT\n"
    )
    env_file.chmod(0o600)
    calls = deploy_dir / "compose.calls"
    fake_compose = r'''compose() {
  printf '%s\n' call >> "$deploy_dir/compose.calls"
  cat <<'CREDS'
IAM_IMPORT_FILE_GATEWAY_BASE_URL=http://file-gateway:8086
IAM_IMPORT_FILE_GATEWAY_TOKEN_URL=http://platform-api:8080/oauth2/token
IAM_IMPORT_FILE_GATEWAY_CLIENT_ID=platform-iam-import-file-gateway
IAM_IMPORT_FILE_GATEWAY_CLIENT_SECRET=test-secret-with-=-and-$-chars
IAM_IMPORT_FILE_GATEWAY_SCOPE=platform:file:upload platform:file:bind
CREDS
}
'''
    harness = (
        'deploy_dir="$1"\nruntime_file="$deploy_dir/.env"\n'
        + fake_compose
        + "\n"
        + env_value
        + "\n"
        + env_set
        + "\n"
        + iam_import_client_configured
        + "\n"
        + provision_iam_import_client
        + "\n"
        + "provision_iam_import_client || exit 20\n"
        + "provision_iam_import_client || exit 21\n"
    )
    process = subprocess.run(
        ["bash", "-Eeuo", "pipefail", "-c", harness, "bash", str(deploy_dir)],
        capture_output=True,
        text=True,
    )
    assert process.returncode == 0, process.stdout + process.stderr
    content = env_file.read_text()
    assert "IAM_IMPORT_FILE_GATEWAY_BASE_URL=http://file-gateway:8086" in content, content
    assert "IAM_IMPORT_FILE_GATEWAY_CLIENT_SECRET=test-secret-with-=-and-$-chars" in content, content
    assert "IAM_IMPORT_FILE_GATEWAY_SCOPE=platform:file:upload platform:file:bind" in content, content
    assert calls.read_text().count("call") == 1, "already configured client must not be provisioned twice"
    assert stat.S_IMODE(env_file.stat().st_mode) == 0o600

print("deploy-service IAM import credential regression passed")
