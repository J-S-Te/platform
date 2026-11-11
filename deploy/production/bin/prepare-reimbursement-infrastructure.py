#!/usr/bin/env python3
"""在既有完整备份及继承发布锁内，仅初始化首次报销数据库的两项口令。"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import secrets
import shlex
import stat
import subprocess
import sys
import tempfile

KEYS = ("REIMBURSEMENT_MYSQL_PASSWORD", "REIMBURSEMENT_MYSQL_ROOT_PASSWORD")
MARKER = ".control-plane-reload-required"
PROTECTED = (MARKER, ".assets-install-transaction", ".runtime-approval-publish")
CAPABILITY = "REIMBURSEMENT_INFRASTRUCTURE_PREPARE_V1"
LIMIT = 4 * 1024 * 1024


class InfrastructureError(Exception):
    """只包含可公开业务原因，不包含运行文件或命令输出。"""


# 用途：读取Linux内核记录的继承描述符锁事实，不尝试加锁。
# 参数：fd为父发布进程继承的描述符。
# 返回值：fdinfo原文；无Linux锁证据时失败关闭。
def lock_description(fd):
    try:
        return Path(f"/proc/self/fdinfo/{fd}").read_text()
    except OSError as error:
        raise InfrastructureError("无法验证继承发布锁的Linux内核事实") from error


# 用途：确认描述符9绑定当前同inode普通独占发布锁，不新抢锁。
# 参数：root为受控部署根，fd为固定继承描述符9。
# 返回值：无；锁事实或身份不匹配时拒绝。
def require_lock(root, fd):
    if fd != 9:
        raise InfrastructureError("仅接受继承的发布锁描述符9")
    try:
        held, path = os.fstat(fd), os.lstat(root / "runtime/.deploy.lock")
    except OSError as error:
        raise InfrastructureError("缺少真实继承发布锁") from error
    if not stat.S_ISREG(held.st_mode) or not stat.S_ISREG(path.st_mode) or (
            held.st_dev, held.st_ino) != (path.st_dev, path.st_ino):
        raise InfrastructureError("继承发布锁不是当前同inode普通文件")
    if held.st_uid not in (0, os.geteuid()) or held.st_mode & 0o022:
        raise InfrastructureError("发布锁属主或权限不安全")
    facts = lock_description(fd).splitlines()
    expected = f"{os.major(held.st_dev):02x}:{os.minor(held.st_dev):02x}:{held.st_ino}"
    locks = [line.split() for line in facts if line.startswith("lock:")]
    if not any(len(row) >= 9 and row[2:5] == ["FLOCK", "ADVISORY", "WRITE"]
               and row[6] == expected and row[7:9] == ["0", "EOF"] for row in locks):
        raise InfrastructureError("继承描述符未持有完整文件FLOCK独占发布锁")


# 用途：拒绝不可信目录或符号链接，限制可写部署边界。
# 参数：path为必须已存在的受控目录。
# 返回值：无；不安全元数据时拒绝。
def directory(path):
    try:
        info = path.lstat()
    except OSError as error:
        raise InfrastructureError("受控目录不存在") from error
    if not stat.S_ISDIR(info.st_mode) or info.st_uid not in (0, os.geteuid()) or info.st_mode & 0o022:
        raise InfrastructureError("受控目录类型、属主或权限不安全")


# 用途：有界读取受控普通文件，拒绝符号链接及不安全权限。
# 参数：path为文件路径，private表示必须0600。
# 返回值：原始字节及fstat元数据。
def read_regular(path, private=False):
    try:
        fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
        with os.fdopen(fd, "rb") as source:
            info = os.fstat(source.fileno())
            if not stat.S_ISREG(info.st_mode) or info.st_uid not in (0, os.geteuid()) or info.st_size > LIMIT:
                raise InfrastructureError("运行文件类型、属主或规模不安全")
            mode = stat.S_IMODE(info.st_mode)
            if (private and mode != 0o600) or mode & 0o022:
                raise InfrastructureError("运行文件权限不安全")
            raw = source.read(LIMIT + 1)
            if len(raw) > LIMIT:
                raise InfrastructureError("运行文件超过读取上限")
            return raw, info
    except OSError as error:
        raise InfrastructureError("无法安全读取受控文件") from error


# 用途：只解析目标两键的存在与非占位状态，不输出任何值。
# 参数：raw为根.env的完整原始字节。
# 返回值：已存在目标键的原文字典；重复或不安全状态拒绝。
def target_values(raw):
    try:
        text = raw.decode("utf-8")
    except UnicodeError as error:
        raise InfrastructureError("根配置编码非法") from error
    result = {}
    for line in text.splitlines():
        match = re.match(r"^\s*(?:export\s+)?(" + "|".join(KEYS) + r")\s*=(.*)$", line)
        if not match:
            if re.match(r"^\s*(?:export\s+)?(?:" + "|".join(KEYS) + r")(?:\W|$)", line):
                raise InfrastructureError("目标数据库键存在不规范声明，拒绝自动生成")
            continue
        key, value = match.groups()
        try:
            value = " ".join(shlex.split(value, comments=True, posix=True)).strip()
        except ValueError as error:
            raise InfrastructureError("目标数据库键引号或声明非法") from error
        if key in result or not value or re.match(r"^(REPLACE|PENDING)(?:_|\b)", value, re.I):
            raise InfrastructureError("数据库配置存在空值、占位值或重复键，拒绝自动生成")
        result[key] = value
    if result and len(result) != 2:
        raise InfrastructureError("数据库两键仅部分存在，拒绝自动生成或轮换")
    return result


# 用途：私有运行固定命令，丢弃错误输出并对输出规模失败关闭。
# 参数：command为无口令参数数组，timeout为执行秒数。
# 返回值：仅供内存校验的stdout，失败只抛脱敏异常。
def run_private(command, timeout=60):
    environment = {key: value for key, value in os.environ.items() if key not in KEYS}
    try:
        result = subprocess.run(command, env=environment, stdout=subprocess.PIPE,
                                stderr=subprocess.DEVNULL, timeout=timeout, check=False)
    except (OSError, subprocess.TimeoutExpired) as error:
        raise InfrastructureError("受控Docker或重载命令不可用") from error
    if result.returncode != 0 or len(result.stdout) > LIMIT:
        raise InfrastructureError("受控Docker或重载命令失败")
    return result.stdout


# 用途：构造固定部署图的Compose命令，不使用外部配置覆盖。
# 参数：root为部署根，arguments为固定子命令参数。
# 返回值：命令参数数组。
def compose(root, *arguments):
    return ["docker", "compose", "--project-directory", str(root), "--file",
            str(root / "docker-compose.yml"), "--env-file", str(root / ".env"),
            "--env-file", str(root / ".release.env"), *arguments]


# 用途：首次生成前确认无既有数据库容器或实际持久卷，拒绝误轮换。
# 参数：root为部署根。
# 返回值：无；Docker失败、异常配置或已有数据均拒绝。
def require_absent_database(root):
    if run_private(compose(root, "ps", "-a", "-q", "reimbursement-mysql")).strip():
        raise InfrastructureError("已有报销数据库容器，拒绝生成新口令")
    try:
        config = json.loads(run_private(compose(root, "config", "--format", "json")))
        mounts = config["services"]["reimbursement-mysql"]["volumes"]
        sources = [item["source"] for item in mounts
                   if item.get("type") == "volume" and item.get("target") == "/var/lib/mysql"]
        if len(sources) != 1:
            raise ValueError("ambiguous database mount")
        name = config["volumes"][sources[0]]["name"]
        if not isinstance(name, str) or not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.-]{0,254}", name):
            raise ValueError("invalid volume name")
    except (KeyError, TypeError, ValueError) as error:
        raise InfrastructureError("无法从Compose确定实际数据库卷名") from error
    volumes = run_private(["docker", "volume", "ls", "--format", "{{.Name}}"]).decode().splitlines()
    if name in volumes:
        raise InfrastructureError("已有报销数据库卷，拒绝生成新口令")


# 用途：在不更改当前.env的前提下校验完整候选Compose配置。
# 参数：root为部署根，candidate为含两键的完整候选字节。
# 返回值：无；失败不修改正式文件。
def validate_candidate(root, candidate):
    fd, name = tempfile.mkstemp(prefix=".reimbursement-infra-check-", dir=root)
    temporary = Path(name)
    try:
        with os.fdopen(fd, "wb") as target:
            os.fchmod(target.fileno(), 0o600)
            target.write(candidate)
        command = compose(root, "config", "--quiet")
        command[command.index(str(root / ".env"))] = str(temporary)
        run_private(command)
    finally:
        temporary.unlink(missing_ok=True)


# 用途：同目录持久化并原子发布，保留原.env身份或独占创建marker。
# 参数：path为目标文件，raw为字节，metadata为原身份，exclusive禁止覆盖。
# 返回值：无；任何写入或持久化失败抛错。
def atomic_file(path, raw, metadata=None, exclusive=False):
    if path.is_symlink():
        raise InfrastructureError("原子写入目标不能为符号链接")
    fd, name = tempfile.mkstemp(prefix=".reimbursement-infra-", dir=path.parent)
    temporary = Path(name)
    try:
        with os.fdopen(fd, "wb") as target:
            target.write(raw)
            target.flush()
            if metadata is not None and (os.fstat(target.fileno()).st_uid,
                                         os.fstat(target.fileno()).st_gid) != (metadata.st_uid, metadata.st_gid):
                os.fchown(target.fileno(), metadata.st_uid, metadata.st_gid)
            os.fchmod(target.fileno(), 0o600)
            os.fsync(target.fileno())
            written_metadata = os.fstat(target.fileno())
        if exclusive:
            os.link(temporary, path)
        else:
            os.replace(temporary, path)
        directory_fd = os.open(path.parent, os.O_RDONLY)
        try:
            os.fsync(directory_fd)
        finally:
            os.close(directory_fd)
    finally:
        temporary.unlink(missing_ok=True)
    return written_metadata


# 用途：完整备份根.env并读回字节和摘要，备份失败不改变原配置。
# 参数：root为部署根，original为原始字节。
# 返回值：本次0700备份目录。
def backup_environment(root, original):
    backups = root / "backups"
    if not backups.exists():
        backups.mkdir(mode=0o700)
    directory(backups)
    destination = Path(tempfile.mkdtemp(prefix="reimbursement-infrastructure-", dir=backups))
    destination.chmod(0o700)
    digest = hashlib.sha256(original).hexdigest()
    atomic_file(destination / ".env", original, exclusive=True)
    atomic_file(destination / ".env.sha256", (digest + "  .env\n").encode(), exclusive=True)
    if read_regular(destination / ".env", True)[0] != original or read_regular(
            destination / ".env.sha256", True)[0] != (digest + "  .env\n").encode():
        raise InfrastructureError("根配置完整备份复核失败")
    return destination


# 用途：在父进程已持锁窗口内复用现有控制面的成对重载。
# 参数：root为部署根。
# 返回值：无；健康、摘要、socket或清单失败均拒绝。
def reload_control_plane(root):
    script = r'''set -Eeuo pipefail
deploy_dir="$1"
runtime_file="$deploy_dir/.env"
release_file="$deploy_dir/.release.env"
compose_file="$deploy_dir/docker-compose.yml"
profiles_dir="$deploy_dir/subsystems.d"
COMPOSE_PROJECT_NAME="$(awk -F= '$1=="COMPOSE_PROJECT_NAME" {n++; v=substr($0,index($0,"=")+1)} END {if(n>1)exit 1;print v}' "$runtime_file")"
COMPOSE_PROJECT_NAME="${COMPOSE_PROJECT_NAME#\"}"
COMPOSE_PROJECT_NAME="${COMPOSE_PROJECT_NAME%\"}"
COMPOSE_PROJECT_NAME="${COMPOSE_PROJECT_NAME#\'}"
COMPOSE_PROJECT_NAME="${COMPOSE_PROJECT_NAME%\'}"
# 用途：仅对当前受控部署图执行既有重载所需Compose子命令。
# 参数：既有成对重载函数传入的固定命令参数。
# 返回值：Docker Compose原始退出码。
compose() {
 docker compose --project-directory "$deploy_dir" --file "$compose_file" \
 --env-file "$runtime_file" --env-file "$release_file" "$@"
}
source "$deploy_dir/bin/provisioner-config-refresh.sh"
refresh_subsystem_control_plane_config
'''
    run_private(["bash", "-c", script, "_", str(root)], timeout=300)


# 用途：验证原文件内容和元数据仍属于锁内快照，阻止并发覆盖。
# 参数：path为受控文件，raw为预期字节，metadata为可选原fstat。
# 返回值：无；任何漂移拒绝。
def unchanged(path, raw, metadata=None):
    current, info = read_regular(path, True)
    identity = ("st_dev", "st_ino", "st_uid", "st_gid", "st_mode", "st_size", "st_mtime_ns")
    if current != raw or (metadata is not None and any(
            getattr(info, key) != getattr(metadata, key) for key in identity)):
        raise InfrastructureError("受控配置发生并发漂移，已拒绝覆盖")


# 用途：事务提交两键并成对重载，失败仅恢复本事务内容。
# 参数：root为部署根，original/candidate为前后字节，metadata为原身份，backup为私有备份。
# 返回值：脱敏准备回执；漂移或恢复不完整保留marker与备份。
def commit(root, original, candidate, metadata, backup):
    path, marker = root / ".env", root / "runtime" / MARKER
    before, after = hashlib.sha256(original).hexdigest(), hashlib.sha256(candidate).hexdigest()
    content = ("FORMAT=1\nREASON=reimbursement_infrastructure_prepare\n"
               f"ENV_BEFORE_SHA256={before}\nENV_AFTER_SHA256={after}\n"
               f"ENV_BACKUP={backup.relative_to(root)}\n").encode()
    unchanged(path, original, metadata)
    marker_metadata = atomic_file(marker, content, exclusive=True)
    published_metadata = None
    try:
        unchanged(path, original, metadata)
        published_metadata = atomic_file(path, candidate, metadata)
        run_private(compose(root, "config", "--quiet"))
        reload_control_plane(root)
        unchanged(path, candidate, published_metadata)
        unchanged(marker, content, marker_metadata)
        marker.unlink()
    except BaseException as error:
        try:
            unchanged(marker, content, marker_metadata)
            if published_metadata is None:
                unchanged(path, original, metadata)
            else:
                unchanged(path, candidate, published_metadata)
            restored_metadata = atomic_file(path, original, metadata)
            reload_control_plane(root)
            unchanged(path, original, restored_metadata)
            unchanged(marker, content, marker_metadata)
            marker.unlink()
        except BaseException as recovery:
            raise InfrastructureError("基础设施准备失败且安全恢复未完成，保留marker及备份") from recovery
        raise InfrastructureError("基础设施准备失败，已恢复原配置并重载") from error
    return {"status": "PREPARED", "keys": list(KEYS), "backup": str(backup.relative_to(root))}


# 用途：在真实继承发布锁及已有完整备份后安全准备首次两键。
# 参数：root为受控部署根，fd为固定继承的独占锁9。
# 返回值：PREPARED或UNCHANGED脱敏回执；拒绝不安全状态。
def prepare(root, fd):
    if not root.is_absolute() or root.resolve() != root or root == Path("/"):
        raise InfrastructureError("部署根必须为无符号链接的规范绝对路径")
    for path in (root, root / "runtime", root / "bin"):
        directory(path)
    require_lock(root, fd)
    for name in PROTECTED:
        if os.path.lexists(root / "runtime" / name):
            raise InfrastructureError("存在未完成受控事务，须先恢复")
    original, metadata = read_regular(root / ".env", True)
    read_regular(root / ".release.env", True)
    read_regular(root / "docker-compose.yml")
    read_regular(root / "bin/provisioner-config-refresh.sh")
    if target_values(original):
        run_private(compose(root, "config", "--quiet"))
        return {"status": "UNCHANGED", "keys": list(KEYS)}
    require_absent_database(root)
    backup = backup_environment(root, original)
    candidate = original + (b"" if not original or original.endswith(b"\n") else b"\n")
    for key in KEYS:
        candidate += f"{key}={secrets.token_hex(32)}\n".encode()
    validate_candidate(root, candidate)
    return commit(root, original, candidate, metadata, backup)


# 用途：只提供固定能力声明及两键准备入口，不接收外部口令。
# 参数：无；从命令行接收部署根及继承锁9。
# 返回值：成功0及脱敏JSON，失败1及固定业务原因。
def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--capabilities", action="store_true")
    parser.add_argument("--deploy-root", type=Path)
    parser.add_argument("--deployment-lock-fd", type=int)
    args = parser.parse_args()
    if args.capabilities:
        if args.deploy_root is not None or args.deployment_lock_fd is not None:
            parser.error("能力查询不接受其他参数")
        print(CAPABILITY)
        return 0
    if args.deploy_root is None or args.deployment_lock_fd != 9:
        parser.error("必须指定部署根及继承发布锁9")
    try:
        print(json.dumps(prepare(args.deploy_root, args.deployment_lock_fd), ensure_ascii=False))
        return 0
    except (InfrastructureError, OSError, ValueError):
        print("报销基础设施准备拒绝：状态不安全或操作失败；请按受控marker与备份核对恢复", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
