#!/usr/bin/env python3
"""deploy-service.sh 须自行写入一次性 IAM 导入凭据，不依赖 deploy.sh 的函数。

平台发布在独立的 deploy-service.sh 进程内调用 provision_iam_import_client。
deploy.sh 虽定义了 env_set，但子脚本不会继承 Shell 函数；若缺少本地定义，
首次平台发布会在客户端密钥已签发后中断。
本测试提取脚本实际定义的辅助函数，
验证 .env 文件的原子写入。
"""
import os
import re
import stat
import subprocess
import tempfile
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
SCRIPT = (ROOT / "bin" / "deploy-service.sh").read_text()


def function(name):
    """用途：提取发布脚本的真实函数供隔离执行。
    参数：name 为待执行的 Shell 函数名。
    返回值：函数源码；缺失时断言失败。
    """
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

# 真实执行发布函数与 Compose dotenv 解码，仅替代会写真实数据库的签发操作。
# 固定默认 client ID、误读引号或未阻断解析错误，均会导致下列行为断言失败。
iam_import_functions = "\n".join(
    function(name) for name in (
        "env_value", "dsn_env_value", "env_set",
        "iam_import_client_configured", "provision_iam_import_client",
    )
)
fake_provision_compose = r'''# 用途：隔离数据库签发，仅返回测试用凭据并记录调用边界。
# 参数：与生产 compose 命令一致，用于核验客户端和租户传递。
# 返回值：合成凭据片段；缺少客户端参数时失败。
compose() {
  printf '%s\n' call >> "$deploy_dir/compose.calls"
  printf '%s\n' "$@" > "$deploy_dir/compose.args"
  local argument client_id=
  for argument in "$@"; do
    case "$argument" in
      IAM_IMPORT_PROVISION_CLIENT_ID=*) client_id="${argument#*=}" ;;
    esac
  done
  [[ -n "$client_id" ]] || return 80
  printf '%s\n' \
    'IAM_IMPORT_FILE_GATEWAY_BASE_URL=http://file-gateway:8086' \
    'IAM_IMPORT_FILE_GATEWAY_TOKEN_URL=http://platform-api:8080/oauth2/token' \
    "IAM_IMPORT_FILE_GATEWAY_CLIENT_ID=$client_id" \
    'IAM_IMPORT_FILE_GATEWAY_CLIENT_SECRET=generated-test-secret-with-=-and-$-chars' \
    'IAM_IMPORT_FILE_GATEWAY_SCOPE=platform:file:upload platform:file:bind'
}
'''


def check_iam_import_case(label, client, secret, action, tenant="tenant-1"):
    """用途：验证真实初始化函数的保留、首次配置和失败关闭行为。
    参数：label 标识用例；client、secret 为原始 dotenv 行或缺失；
        action 为 preserve、initialize 或 reject；tenant 为租户 dotenv 值。
    返回值：无；副作用、返回码或幂等行为不符时断言失败。
    """
    with tempfile.TemporaryDirectory(prefix="deploy-service-iam-import-") as directory:
        deploy_dir = Path(directory)
        env_file = deploy_dir / ".env"
        lines = ["# preserve this comment", f"SUBSYSTEM_PRODUCTION_ALLOWED_TENANT_ID={tenant}"]
        if client is not None:
            lines.append(f"IAM_IMPORT_FILE_GATEWAY_CLIENT_ID={client}")
        if secret is not None:
            lines.append(f"IAM_IMPORT_FILE_GATEWAY_CLIENT_SECRET={secret}")
        original = ("\n".join(lines) + "\nUNRELATED=keep\n").encode()
        env_file.write_bytes(original)
        env_file.chmod(0o600)
        (deploy_dir / ".release.env").write_text("")
        harness = (
            'deploy_dir="$1"\nruntime_file="$deploy_dir/.env"\n'
            'release_file="$deploy_dir/.release.env"\n'
            + fake_provision_compose + iam_import_functions + "\n"
            + "provision_iam_import_client || exit 20\n"
            + 'cp "$runtime_file" "$deploy_dir/after-first.env"\n'
            + "provision_iam_import_client || exit 21\n"
        )
        process = subprocess.run(
            ["bash", "-Eeuo", "pipefail", "-c", harness, "bash", str(deploy_dir)],
            capture_output=True, text=True,
            env={key: value for key, value in os.environ.items() if key not in {
                "IAM_IMPORT_FILE_GATEWAY_CLIENT_ID", "IAM_IMPORT_FILE_GATEWAY_CLIENT_SECRET",
                "SUBSYSTEM_PRODUCTION_ALLOWED_TENANT_ID", "UNSET_IMPORT_TEST",
            }},
        )
        calls = deploy_dir / "compose.calls"
        if action == "reject":
            assert process.returncode != 0, f"{label}: must fail closed"
            assert not calls.exists(), f"{label}: must not issue a credential"
            assert env_file.read_bytes() == original, f"{label}: changed rejected config"
            if secret and "SYNTHETIC_REVIEW_ONLY_" in secret:
                output = process.stdout + process.stderr
                assert "SYNTHETIC_REVIEW_ONLY_" not in output, f"{label}: leaked secret marker"
                assert secret not in output, f"{label}: leaked secret"
                assert f"IAM_IMPORT_FILE_GATEWAY_CLIENT_SECRET={secret}" not in output, label
            return
        assert process.returncode == 0, f"{label}: {process.stdout}{process.stderr}"
        assert env_file.read_bytes() == (deploy_dir / "after-first.env").read_bytes(), label
        assert stat.S_IMODE(env_file.stat().st_mode) == 0o600, label
        if action == "preserve":
            assert not calls.exists(), f"{label}: must not issue a credential"
            assert env_file.read_bytes() == original, f"{label}: changed existing credentials"
            return
        assert calls.read_text().splitlines() == ["call"], f"{label}: initialized twice"
        args = (deploy_dir / "compose.args").read_text().splitlines()
        expected_client = "customer-import-client" if client == "customer-import-client" else (
            "platform-iam-import-file-gateway"
        )
        assert f"IAM_IMPORT_PROVISION_CLIENT_ID={expected_client}" in args, label
        assert "IAM_IMPORT_PROVISION_TENANT_ID=tenant-1" in args, label
        assert args[-2:] == ["platform-migrate", "./provision-iam-import-client"], label
        content = env_file.read_text()
        assert "IAM_IMPORT_FILE_GATEWAY_CLIENT_SECRET=generated-test-secret" in content, label
        assert "UNRELATED=keep\n" in content, label
        assert not list(deploy_dir.glob(".env-update.*")), label



def check_runtime_decode_failure(label, payload, exit_code=0, native_error=""):
    """用途：验证解析失败不会输出原生错误、输入 JSON 或部分解码值。
    参数：label 标识用例；payload 为解码输入；exit_code 和 native_error 为 Compose 结果。
    返回值：无；错误细节外泄、返回成功或错误消息不稳定时断言失败。
    """
    fake_docker = r'''# 用途：模拟失败的 Compose 输出，验证真实读取函数的日志边界。
# 参数：忽略命令参数，仅替代不需要 Docker daemon 的配置解析外部进程。
# 返回值：测试指定的 JSON、原生错误和退出码。
docker() {
  printf '%s' "$PROBE_JSON"
  printf '%s' "$PROBE_NATIVE_ERROR" >&2
  return "$PROBE_EXIT_CODE"
}
'''
    harness = (
        'deploy_dir=unused\nruntime_file=unused\nrelease_file=unused\n'
        + fake_docker + function("dsn_env_value") + "\n"
        + "dsn_env_value IAM_IMPORT_FILE_GATEWAY_CLIENT_SECRET\n"
    )
    process = subprocess.run(
        ["bash", "-Eeuo", "pipefail", "-c", harness], capture_output=True, text=True,
        env={**os.environ, "PROBE_JSON": payload, "PROBE_EXIT_CODE": str(exit_code),
             "PROBE_NATIVE_ERROR": native_error},
    )
    assert process.returncode != 0, f"{label}: must propagate parser failure"
    assert process.stdout == "", f"{label}: must not publish partial decoded values"
    assert "SYNTHETIC_REVIEW_ONLY_" not in process.stderr, f"{label}: leaked parser input"
    assert process.stderr == "运行配置解析失败，拒绝读取运行参数\n", label


for label, payload in [
    ("malformed-json", 'SYNTHETIC_REVIEW_ONLY_INVALID_JSON'),
    ("invalid-json-shape", '{"SYNTHETIC_REVIEW_ONLY_KEY": "private"}'),
    ("invalid-value-type", '{"services":{"probe":{"environment":'
     '{"VALUE":["SYNTHETIC_REVIEW_ONLY_VALUE"]}}}}'),
]:
    check_runtime_decode_failure(label, payload)
check_runtime_decode_failure(
    "compose-failure-after-output",
    '{"services":{"probe":{"environment":{"VALUE":"SYNTHETIC_REVIEW_ONLY_VALUE"}}}}',
    exit_code=7, native_error="SYNTHETIC_REVIEW_ONLY_NATIVE_ERROR",
)

preserved_cases = [
    ("custom-client", "customer-import-client", "existing-secret"),
    ("default-client", "platform-iam-import-file-gateway", "existing-secret"),
    ("quoted-client", "'customer-import-client'", "existing-secret"),
    ("equals-dollar-quotes", "customer-import-client", "'existing=secret$VALUE\"quoted\"'"),
    ("double-quoted-secret", "customer-import-client", '\"existing=secret$$dollar\"'),
    ("unquoted-equals", "customer-import-client", "existing=secret=tail"),
]
for label, client, secret in preserved_cases:
    check_iam_import_case(label, client, secret, "preserve")

initialize_cases = [
    ("missing-client", None, "existing-secret"),
    ("empty-client", "", "existing-secret"),
    ("placeholder-client", "REPLACE_WITH_CLIENT", "existing-secret"),
    ("quoted-placeholder-client", "'REPLACE_WITH_CLIENT'", "existing-secret"),
    ("missing-secret", "customer-import-client", None),
    ("empty-secret", "customer-import-client", "''"),
    ("whitespace-secret", "customer-import-client", "'   '"),
    ("placeholder-secret", "customer-import-client", "REPLACE_WITH_SECRET"),
    ("quoted-placeholder-secret", "customer-import-client", "'REPLACE_WITH_SECRET'"),
]
for label, client, secret in initialize_cases:
    check_iam_import_case(label, client, secret, "initialize", tenant="'tenant-1'")

for label, client, secret in [
    ("invalid-interpolation-secret-leak", "customer-import-client",
     '\"SYNTHETIC_REVIEW_ONLY_${bad!value}\"'),
    ("invalid-client", "bad/client", "existing-secret"),
    ("short-client", "ab", "existing-secret"),
    ("broken-dotenv", "customer-import-client", '\"unterminated'),
    ("failed-interpolation", "customer-import-client", "${UNSET_IMPORT_TEST:?missing}"),
]:
    check_iam_import_case(label, client, secret, "reject")
check_iam_import_case("missing-tenant", None, None, "reject", tenant="")

print("deploy-service IAM import credential regression passed (21 provision + 4 parser failure cases)")
