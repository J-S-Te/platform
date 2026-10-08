#!/usr/bin/env python3
"""Recover immutable local contract secrets from the still-running trusted API.

Never rotate a database account or encryption key to repair a lost runtime file.
Only four persistence/session keys are recovered; managed OAuth settings are kept.
"""
import base64
import json
import os
from pathlib import Path
import subprocess
import tempfile


KEYS = (
    "CONTRACT_MYSQL_PASSWORD", "CONTRACT_MYSQL_ROOT_PASSWORD",
    "OIDC_SESSION_ENCRYPTION_KEY_BASE64", "SIGNING_PHONE_ENCRYPTION_KEY_BASE64",
)


def checked_run(args, data=None):
    result = subprocess.run(args, input=data, capture_output=True, text=True, timeout=30)
    if result.returncode:
        # Docker/DB errors can contain connection strings: never surface raw output.
        raise RuntimeError("受控恢复校验失败；未修改配置或数据库")
    return result.stdout


def inspect_container(name, project, service, working_dir):
    rows = json.loads(checked_run(["docker", "inspect", name]))
    if len(rows) != 1:
        raise RuntimeError("目标容器不唯一")
    container = rows[0]
    labels = container.get("Config", {}).get("Labels", {})
    if (labels.get("com.docker.compose.project") != project
            or labels.get("com.docker.compose.service") != service
            or Path(labels.get("com.docker.compose.project.working_dir", "")).resolve() != working_dir.resolve()
            or not container.get("State", {}).get("Running")):
        raise RuntimeError("容器不属于当前本地编排或没有运行")
    return container


def validated_values(container):
    env = dict(line.split("=", 1) for line in container["Config"]["Env"] if "=" in line)
    values = {key: env.get(key, "") for key in KEYS}
    for key, value in values.items():
        if not value or "REPLACE_WITH_" in value or any(c in value for c in "\r\n\x00"):
            raise RuntimeError("旧运行时缺少有效的持久化密钥")
        if key.endswith("_BASE64"):
            try:
                valid = len(base64.b64decode(value, validate=True)) == 32
            except ValueError:
                valid = False
            if not valid:
                raise RuntimeError("旧运行时加密密钥格式无效")
    # Do not adopt a database password unrelated to the API's actual connection.
    if not env.get("MYSQL_DSN", "").startswith("contract:" + values["CONTRACT_MYSQL_PASSWORD"] + "@tcp(contract-mysql:3306)/"):
        raise RuntimeError("旧 API 数据库连接与运行时凭据不一致")
    return values


def restore_file(path, values):
    original = path.read_text()
    lines, seen = [], set()
    for line in original.splitlines():
        key = line.split("=", 1)[0]
        if key in values:
            if key not in seen:
                lines.append(key + "=" + values[key])
                seen.add(key)
        else:
            lines.append(line)
    lines.extend(key + "=" + values[key] for key in KEYS if key not in seen)
    with tempfile.NamedTemporaryFile(dir=path.parent, prefix=".contract-runtime-backup-", delete=False) as backup:
        os.chmod(backup.name, 0o600)
        backup.write(original.encode())
    temporary = None
    try:
        with tempfile.NamedTemporaryFile(dir=path.parent, prefix=".contract-runtime-", delete=False) as target:
            temporary = target.name
            os.chmod(target.name, 0o600)
            target.write(("\n".join(lines) + "\n").encode())
            target.flush()
            os.fsync(target.fileno())
        os.replace(temporary, path)
    finally:
        if temporary and os.path.exists(temporary):
            os.unlink(temporary)


def main():
    platform = Path(__file__).resolve().parents[1]
    path = platform.parent / "contract_management" / ".env.local"
    if not path.is_file() or path.is_symlink():
        raise RuntimeError("请先保留现有普通运行时文件；不处理符号链接")
    project = "basic-platform-local"
    api = inspect_container(project + "-contract-api-1", project, "contract-api", platform)
    if api.get("State", {}).get("Health", {}).get("Status") != "healthy":
        raise RuntimeError("旧合同 API 未通过健康检查，不能据此恢复")
    database = inspect_container(project + "-contract-mysql-1", project, "contract-mysql", platform)
    if not any(m.get("Type") == "volume" and m.get("Name") == project + "-contract-mysql-data"
               and m.get("Destination") == "/var/lib/mysql" for m in database.get("Mounts", [])):
        raise RuntimeError("数据库没有挂载当前编排的保留数据卷")
    values = validated_values(api)
    # Feed credentials over stdin, not shell interpolation, argv, or log output.
    for user, key in (("contract", KEYS[0]), ("root", KEYS[1])):
        output = checked_run(["docker", "exec", "-i", project + "-contract-mysql-1", "sh", "-c",
                              'IFS= read -r MYSQL_PWD; export MYSQL_PWD; exec mysql -h127.0.0.1 -u "$1" -Nse "SELECT 1"',
                              "sh", user], values[key] + "\n")
        if output.strip() != "1":
            raise RuntimeError("保留数据库认证校验失败")
    restore_file(path, values)
    print("原有合同数据库凭据及加密/会话密钥已验证并恢复；未改数据库账号、OAuth 凭据或业务数据。")


if __name__ == "__main__":
    try:
        main()
    except (RuntimeError, ValueError, OSError, subprocess.TimeoutExpired) as error:
        print(str(error) if isinstance(error, RuntimeError) else "受控恢复失败；未输出敏感配置")
        raise SystemExit(1)
