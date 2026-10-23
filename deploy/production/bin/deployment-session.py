#!/usr/bin/env python3
"""私有且可恢复的部署进度记录，仅作提示，不作为授权依据。

本工具不执行部署、存储、授权或 Docker 命令。调用方每次尝试时
都须验证真实安装事实，包括此前已完成的工作。
"""
import argparse
from contextlib import contextmanager
from datetime import datetime, timezone
import fcntl
import hashlib
import json
import os
from pathlib import Path
import re
import secrets
import stat
import sys
import time


MAX_JSON = 2 * 1024 * 1024
MAX_EVENTS = 4096
STAGE = re.compile(r"[a-z][a-z0-9_-]{0,63}\Z")
TOKEN = re.compile(r"[A-Z][A-Z0-9_]{0,63}\Z")
SESSION = re.compile(r"[a-f0-9]{32}\Z")
DIGEST = re.compile(r"sha256:[a-f0-9]{64}\Z")
CUSTOMER = re.compile(r"[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}\Z")
COMPONENTS = frozenset({"common", "platform", "frontend", "customer-opportunity",
                        "customer-portal", "contract", "project", "settlement", "data-analysis"})
TERMINAL = frozenset({"DONE", "SKIPPED"})
UPDATE_STATUSES = frozenset({"RUNNING", "DONE", "FAILED", "WAITING", "SKIPPED"})
TEMP_NAME = re.compile(r"\.session-[a-f0-9]{32}\.tmp\Z")
ARCHIVE_NAME = re.compile(r"[a-f0-9]{32}\.json\Z")
STATE_NAME = "session.json"
LOCK_NAME = ".lock"


class SessionError(ValueError):
    def __init__(self, code, message, exit_code=2):
        super().__init__(message)
        self.code, self.exit_code = code, exit_code


def reject(code, message, exit_code=2):
    raise SessionError(code, message, exit_code)


def now():
    return datetime.now(timezone.utc).isoformat(timespec="microseconds").replace("+00:00", "Z")


def encoded(value):
    return json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":"), allow_nan=False).encode("utf-8")


def checksum(value):
    return "sha256:" + hashlib.sha256(encoded(value)).hexdigest()


def unique_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            reject("INVALID_JSON", "JSON 含重复字段")
        result[key] = value
    return result


def decode(raw):
    def invalid_constant(_):
        reject("INVALID_JSON", "JSON 数值无效")
    try:
        return json.loads(raw.decode("utf-8"), object_pairs_hook=unique_object, parse_constant=invalid_constant)
    except (UnicodeError, json.JSONDecodeError, RecursionError):
        reject("INVALID_JSON", "JSON 内容损坏或格式无效")


def exact(value, keys, code="INVALID_STATE"):
    if not isinstance(value, dict) or set(value) != set(keys):
        reject(code, "会话记录或输入字段不完整，含未知字段，或类型无效")


def canonical_path(value, optional=False):
    if optional and value == "":
        return value
    if not isinstance(value, str) or not value or len(value) > 4096 or any(ord(char) < 32 or ord(char) == 127 for char in value):
        reject("UNSAFE_PATH", "路径必须是规范的非根绝对路径")
    path = Path(value)
    try:
        if not path.is_absolute() or value == "/" or str(path) != value or str(path.resolve()) != value:
            reject("UNSAFE_PATH", "路径必须规范且不能包含符号链接")
    except (OSError, RuntimeError):
        reject("UNSAFE_PATH", "路径无法安全解析")
    return value


def validate_identity(value):
    exact(value, {"version", "target", "media", "delivery_id", "mode", "components", "options"}, "INVALID_IDENTITY")
    if type(value["version"]) is not int or value["version"] != 1:
        reject("INVALID_IDENTITY", "安装身份版本无效")
    canonical_path(value["target"])
    canonical_path(value["media"])
    if not isinstance(value["delivery_id"], str) or not DIGEST.fullmatch(value["delivery_id"]):
        reject("INVALID_IDENTITY", "交付摘要必须为 SHA256SUMS 的 sha256 摘要")
    if value["mode"] not in ("install", "upgrade", "add-subsystems"):
        reject("INVALID_IDENTITY", "部署模式无效")
    components = value["components"]
    if not isinstance(components, list) or not components or len(components) > len(COMPONENTS) or any(not isinstance(item, str) or item not in COMPONENTS for item in components) or components != sorted(set(components)):
        reject("INVALID_IDENTITY", "组件列表必须已排序、唯一且全部属于支持的交付组件")
    options = value["options"]
    option_keys = {"skip_host_deps", "skip_configure", "apply_compose", "legacy_license_mode", "customer", "license_file_path"}
    if not isinstance(options, dict) or set(options) not in (option_keys, option_keys | {"license_file_sha256"}):
        reject("INVALID_IDENTITY", "安装选项字段不完整或含未知字段")
    if any(type(options[key]) is not bool for key in ("skip_host_deps", "skip_configure", "apply_compose")) or options["legacy_license_mode"] not in ("", "bridge", "upgrade"):
        reject("INVALID_IDENTITY", "安装选项类型或旧授权模式无效")
    if not isinstance(options["customer"], str) or (options["customer"] and not CUSTOMER.fullmatch(options["customer"])):
        reject("INVALID_IDENTITY", "客户标识无效")
    canonical_path(options["license_file_path"], optional=True)
    license_hash = options.get("license_file_sha256", "")
    if not isinstance(license_hash, str) or (license_hash and not re.fullmatch(r"[a-f0-9]{64}", license_hash)):
        reject("INVALID_IDENTITY", "外部许可证文件摘要必须为空或 64 位小写 SHA256")
    return value


def comparable_identity(value):
    # 旧的版本 1 日志缺少可选外部许可证摘要。保留其字节级校验和，仅在比较时规范化此默认值。
    return dict(value, options=dict(value["options"], license_file_sha256=value["options"].get("license_file_sha256", "")))


def validate_stages(value):
    if not isinstance(value, list) or not 1 <= len(value) <= 128 or any(not isinstance(item, str) or not STAGE.fullmatch(item) for item in value) or len(set(value)) != len(value):
        reject("INVALID_STAGES", "阶段列表必须为 1 至 128 个唯一的受限阶段名")
    return value


def check_directory(info, leaf=False, direct_parent=False):
    if not stat.S_ISDIR(info.st_mode) or info.st_uid not in (0, os.geteuid()):
        reject("UNSAFE_DIRECTORY", "目录类型或属主不安全")
    permissions = stat.S_IMODE(info.st_mode)
    if leaf:
        if permissions != 0o700:
            reject("UNSAFE_PERMISSIONS", "会话目录权限必须为 0700")
    elif permissions & 0o022:
        # 标准的 root 所有且带粘滞位的临时祖先目录仅是路径遍历锚点，不能作为会话或身份文件可直接写入的父目录。
        if direct_parent or info.st_uid != 0 or not permissions & stat.S_ISVTX:
            reject("UNSAFE_PERMISSIONS", "父目录可由组或其他用户写入")


def open_directory(path, create=False, strict_parent=False):
    canonical_path(path)
    parts = Path(path).parts[1:]
    descriptor = os.open("/", os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        check_directory(os.fstat(descriptor))
        for index, name in enumerate(parts):
            leaf = index == len(parts) - 1
            try:
                next_fd = os.open(name, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=descriptor)
            except FileNotFoundError:
                if leaf:
                    check_directory(os.fstat(descriptor), direct_parent=strict_parent)
                if not create or not leaf:
                    raise
                try:
                    os.mkdir(name, 0o700, dir_fd=descriptor)
                    os.fsync(descriptor)
                except FileExistsError:
                    pass
                next_fd = os.open(name, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=descriptor)
            try:
                check_directory(os.fstat(next_fd), leaf=leaf if create else False,
                                direct_parent=strict_parent and index == len(parts) - 2)
            except BaseException:
                os.close(next_fd)
                raise
            os.close(descriptor)
            descriptor = next_fd
        return descriptor
    except BaseException:
        os.close(descriptor)
        raise


def check_file(info, allowed_links=(1,)):
    if not stat.S_ISREG(info.st_mode) or info.st_uid not in (0, os.geteuid()) or info.st_nlink not in allowed_links:
        reject("UNSAFE_FILE", "会话文件必须为可信属主的独立普通文件")
    if stat.S_IMODE(info.st_mode) != 0o600:
        reject("UNSAFE_PERMISSIONS", "会话和输入 JSON 文件权限必须为 0600")


def read_file(directory, name, limit=MAX_JSON):
    descriptor = os.open(name, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=directory)
    try:
        info = os.fstat(descriptor)
        check_file(info)
        if info.st_size > limit:
            reject("INPUT_TOO_LARGE", "会话或输入文件超过大小上限")
        chunks, count = [], 0
        while True:
            chunk = os.read(descriptor, min(65536, limit + 1 - count))
            if not chunk:
                break
            chunks.append(chunk)
            count += len(chunk)
            if count > limit:
                reject("INPUT_TOO_LARGE", "会话或输入文件超过大小上限")
        return b"".join(chunks)
    finally:
        os.close(descriptor)


def read_identity(path):
    canonical_path(path)
    parent = open_directory(str(Path(path).parent))
    try:
        check_directory(os.fstat(parent), direct_parent=True)
        return validate_identity(decode(read_file(parent, Path(path).name, 65536)))
    finally:
        os.close(parent)


@contextmanager
def locked_directory(path, create=False):
    try:
        descriptor = open_directory(path, create=create, strict_parent=True)
    except FileNotFoundError:
        reject("NO_SESSION", "尚无已提交的安装会话", 3)
    lock = None
    try:
        check_directory(os.fstat(descriptor), leaf=True)
        flags = os.O_RDWR | os.O_NOFOLLOW | os.O_NONBLOCK | (os.O_CREAT if create else 0)
        try:
            lock = os.open(LOCK_NAME, flags, 0o600, dir_fd=descriptor)
        except FileNotFoundError:
            reject("MISSING_LOCK", "已有会话目录缺少可信短锁，拒绝写入或恢复")
        check_file(os.fstat(lock))
        deadline = time.monotonic() + 5
        while True:
            try:
                fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
                break
            except BlockingIOError:
                if time.monotonic() >= deadline:
                    reject("SESSION_BUSY", "会话记录正被另一短操作占用，请重试", 5)
                time.sleep(0.02)
        for name in os.listdir(descriptor):
            if name not in (LOCK_NAME, STATE_NAME) and not TEMP_NAME.fullmatch(name):
                reject("UNKNOWN_STATE_FILE", "会话目录包含未登记文件")
            info = os.stat(name, dir_fd=descriptor, follow_symlinks=False)
            check_file(info)
        yield descriptor
    finally:
        if lock is not None:
            os.close(lock)
        os.close(descriptor)


def atomic_write(directory, value):
    raw = encoded(value) + b"\n"
    if len(raw) > MAX_JSON:
        reject("STATE_LIMIT", "会话历史超过大小上限，请保留记录并检查重复调用")
    name = ".session-" + secrets.token_hex(16) + ".tmp"
    descriptor = None
    try:
        descriptor = os.open(name, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600, dir_fd=directory)
        os.fchmod(descriptor, 0o600)
        with os.fdopen(descriptor, "wb") as stream:
            descriptor = None
            stream.write(raw)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(name, STATE_NAME, src_dir_fd=directory, dst_dir_fd=directory)
        os.fsync(directory)
    finally:
        if descriptor is not None:
            os.close(descriptor)
        try:
            os.unlink(name, dir_fd=directory)
        except FileNotFoundError:
            pass


def valid_time(value):
    if not isinstance(value, str) or len(value) != 27:
        reject("INVALID_STATE", "会话时间格式无效")
    try:
        datetime.strptime(value, "%Y-%m-%dT%H:%M:%S.%fZ")
    except ValueError:
        reject("INVALID_STATE", "会话时间格式无效")


def stage_record(key):
    return {"key": key, "status": "PENDING", "attempts": 0, "generation": 0,
            "previous_status": None, "previous_generation": None,
            "started_at": None, "finished_at": None, "reason_code": ""}


def model(order):
    return {"generation": 1, "cursor": 0,
            "stages": [stage_record(key) for key in order], "last_failure": None}


def overall(state):
    if state["cursor"] == len(state["stages"]):
        return "COMPLETED"
    current = state["stages"][state["cursor"]]
    return current["status"] if current["generation"] == state["generation"] else "PENDING"


def advisory(state):
    status = overall(state)
    key = state["stages"][state["cursor"]]["key"] if status != "COMPLETED" else ""
    actions = {"COMPLETED": "VERIFY_REAL_INSTALLATION", "PENDING": "RUN_STAGE",
               "RUNNING": "WAIT_FOR_RUNNER_OR_RESUME", "FAILED": "RESUME_AND_REVERIFY",
               "WAITING": "OPERATOR_ACTION_AND_RESUME"}
    return {"next_stage": key, "action": actions[status]}


def apply_event(state, event):
    exact(event, {"sequence", "at", "action", "stage", "status", "reason_code", "generation", "attempt"})
    valid_time(event["at"])
    if type(event["generation"]) is not int or type(event["attempt"]) is not int or not isinstance(event["reason_code"], str) or (event["reason_code"] and not TOKEN.fullmatch(event["reason_code"])):
        reject("INVALID_STATE", "会话事件格式无效")
    if event["action"] in ("BEGIN", "RESUME"):
        expected = 1 if event["action"] == "BEGIN" else state["generation"] + 1
        if event["generation"] != expected or event["attempt"] != 0 or any(event[key] != "" for key in ("stage", "status", "reason_code")):
            reject("INVALID_STATE", "会话恢复事件无效")
        state["generation"], state["cursor"] = expected, 0
        return
    if event["action"] != "UPDATE" or event["generation"] != state["generation"] or event["status"] not in UPDATE_STATUSES or state["cursor"] >= len(state["stages"]):
        reject("INVALID_TRANSITION", "会话阶段转移无效", 5)
    current = state["stages"][state["cursor"]]
    if event["stage"] != current["key"]:
        reject("INVALID_TRANSITION", "必须按固定顺序执行当前阶段，不允许跳步", 5)
    same_generation = current["generation"] == state["generation"]
    status = event["status"]
    if status == "RUNNING":
        if same_generation and current["status"] != "PENDING":
            reject("RESUME_REQUIRED", "失败、等待或中断阶段必须使用原参数 --resume 后重试", 5)
        if event["attempt"] != current["attempts"] + 1 or event["reason_code"]:
            reject("INVALID_STATE", "会话阶段尝试次数无效")
        current.update(previous_status=current["status"] if current["attempts"] else None,
                       previous_generation=current["generation"] if current["attempts"] else None,
                       status="RUNNING", attempts=event["attempt"], generation=state["generation"],
                       started_at=event["at"], finished_at=None, reason_code="")
        return
    if not same_generation or current["status"] != "RUNNING" or event["attempt"] != current["attempts"]:
        reject("INVALID_TRANSITION", "阶段必须先标记 RUNNING，再记录真实执行结果", 5)
    if status in ("FAILED", "WAITING", "SKIPPED") and not event["reason_code"]:
        reject("INVALID_STATE", "失败、等待或跳过阶段必须记录固定原因码")
    if status == "DONE" and event["reason_code"]:
        reject("INVALID_STATE", "完成阶段不接受自由原因内容")
    current.update(status=status, finished_at=event["at"], reason_code=event["reason_code"])
    if status == "FAILED":
        state["last_failure"] = {"stage": current["key"], "reason_code": event["reason_code"],
                                 "at": event["at"], "attempt": current["attempts"],
                                 "generation": state["generation"]}
    if status in TERMINAL:
        state["cursor"] += 1


def validate_state(value, state_dir):
    exact(value, {"version", "session_id", "identity", "identity_digest", "created_at", "updated_at",
                  "stage_order", "generation", "cursor", "stages", "last_failure", "history", "advisory", "checksum"})
    if type(value["version"]) is not int or value["version"] != 1 or not isinstance(value["session_id"], str) or not SESSION.fullmatch(value["session_id"]):
        reject("INVALID_STATE", "会话版本或标识无效")
    validate_identity(value["identity"])
    validate_binding(state_dir, value["identity"])
    if value["identity_digest"] != checksum(value["identity"]):
        reject("STATE_TAMPERED", "会话安装身份摘要不一致")
    without_checksum = {key: item for key, item in value.items() if key != "checksum"}
    if value["checksum"] != checksum(without_checksum):
        reject("STATE_TAMPERED", "会话记录校验不一致，拒绝据此恢复")
    validate_stages(value["stage_order"])
    valid_time(value["created_at"])
    valid_time(value["updated_at"])
    history = value["history"]
    if not isinstance(history, list) or not 1 <= len(history) <= MAX_EVENTS:
        reject("INVALID_STATE", "会话事件历史无效或超过上限")
    replay = model(value["stage_order"])
    for sequence, event in enumerate(history, 1):
        if not isinstance(event, dict) or type(event.get("sequence")) is not int or event["sequence"] != sequence or (event.get("action") == "BEGIN") != (sequence == 1):
            reject("INVALID_STATE", "会话事件顺序无效")
        try:
            apply_event(replay, event)
        except SessionError:
            reject("INVALID_STATE", "会话历史无法重放，拒绝恢复")
    if history[0]["at"] != value["created_at"] or history[-1]["at"] != value["updated_at"]:
        reject("INVALID_STATE", "会话时间与事件不一致")
    for key in ("generation", "cursor", "stages", "last_failure"):
        if encoded(replay[key]) != encoded(value[key]):
            reject("STATE_TAMPERED", "会话进度与历史不一致")
    if value["advisory"] != advisory(replay):
        reject("STATE_TAMPERED", "会话下一步建议与真实阶段记录不一致")
    return value


def validate_binding(state_dir, identity):
    if state_dir != identity["target"] + ".installation-state":
        reject("IDENTITY_CONFLICT", "会话目录必须是目标目录的 .installation-state 同级目录", 4)
    media, location = Path(identity["media"]), Path(state_dir)
    if media == location or media in location.parents or location in media.parents:
        reject("IDENTITY_CONFLICT", "会话目录与只读交付介质不能互相嵌套", 4)


def load_state(directory, state_dir):
    try:
        raw = read_file(directory, STATE_NAME)
    except FileNotFoundError:
        reject("NO_SESSION", "尚无已提交的安装会话，请使用原一键安装入口开始", 3)
    return validate_state(decode(raw), state_dir)


def publish(directory, value, event):
    if len(value["history"]) >= MAX_EVENTS:
        reject("STATE_LIMIT", "会话事件超过上限，请保留记录并检查重复重试")
    event["sequence"] = len(value["history"]) + 1
    apply_event(value, event)
    value["history"].append(event)
    value["updated_at"], value["advisory"] = event["at"], advisory(value)
    value.pop("checksum", None)
    value["checksum"] = checksum(value)
    atomic_write(directory, value)


def create_session(directory, identity, order):
    stamp = now()
    value = {"version": 1, "session_id": secrets.token_hex(16), "identity": identity,
             "identity_digest": checksum(identity), "created_at": stamp, "updated_at": stamp,
             "stage_order": order, "history": [], **model(order)}
    event = {"at": stamp, "action": "BEGIN", "stage": "", "status": "", "reason_code": "", "generation": 1, "attempt": 0}
    publish(directory, value, event)
    return value["session_id"]


def validate_archive_directory(directory):
    records = {}
    for name in os.listdir(directory):
        if not TEMP_NAME.fullmatch(name) and not ARCHIVE_NAME.fullmatch(name):
            reject("UNSAFE_ARCHIVE", "安装历史目录包含未登记文件")
        info = os.stat(name, dir_fd=directory, follow_symlinks=False)
        check_file(info, allowed_links=(1, 2))
        records[name] = info
    recovered = set()
    for name, info in records.items():
        inode = (info.st_dev, info.st_ino)
        if info.st_nlink != 2 or inode in recovered:
            continue
        names = [candidate for candidate, other in records.items() if (other.st_dev, other.st_ino) == inode]
        temporary = [candidate for candidate in names if TEMP_NAME.fullmatch(candidate)]
        archived = [candidate for candidate in names if ARCHIVE_NAME.fullmatch(candidate)]
        # 原子且不覆盖的发布将已完整 fsync 的临时文件硬链接到不可变最终名称。只允许解除这个确切的崩溃残留链接，绝不提升任意硬链接或临时文件。
        if len(names) != 2 or len(temporary) != 1 or len(archived) != 1:
            reject("UNSAFE_ARCHIVE", "历史文件存在未经登记的硬链接")
        os.unlink(temporary[0], dir_fd=directory)
        os.fsync(directory)
        recovered.add(inode)
    for name in os.listdir(directory):
        check_file(os.stat(name, dir_fd=directory, follow_symlinks=False))


def archive_completed(state_dir, value, next_identity):
    archive_path = value["identity"]["target"] + ".installation-history"
    media, archive = Path(next_identity["media"]), Path(archive_path)
    if media == archive or media in archive.parents or archive in media.parents:
        reject("IDENTITY_CONFLICT", "安装历史目录与交付介质不能互相嵌套", 4)
    directory = open_directory(archive_path, create=True, strict_parent=True)
    try:
        check_directory(os.fstat(directory), leaf=True)
        validate_archive_directory(directory)
        final = value["session_id"] + ".json"

        def existing_archive():
            try:
                stored = validate_state(decode(read_file(directory, final)), state_dir)
            except FileNotFoundError:
                return False
            if stored["session_id"] != value["session_id"] or overall(stored) != "COMPLETED" or encoded(stored) != encoded(value):
                reject("ARCHIVE_CONFLICT", "已有历史记录与待归档完整会话不一致，拒绝覆盖", 4)
            return True

        if existing_archive():
            return
        temporary = ".session-" + secrets.token_hex(16) + ".tmp"
        descriptor = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600, dir_fd=directory)
        try:
            os.fchmod(descriptor, 0o600)
            with os.fdopen(descriptor, "wb") as stream:
                descriptor = None
                stream.write(encoded(value) + b"\n")
                stream.flush()
                os.fsync(stream.fileno())
            try:
                os.link(temporary, final, src_dir_fd=directory, dst_dir_fd=directory, follow_symlinks=False)
            except FileExistsError:
                if not existing_archive():
                    raise
            os.fsync(directory)
        finally:
            if descriptor is not None:
                os.close(descriptor)
            os.unlink(temporary, dir_fd=directory)
            os.fsync(directory)
    finally:
        os.close(directory)


def begin(state_dir, identity_file, stages, resume, new_session=False):
    if resume and new_session:
        reject("INVALID_ARGUMENTS", "--resume 与 --new-session 不能同时使用")
    canonical_path(state_dir)
    identity = read_identity(identity_file)
    validate_binding(state_dir, identity)
    order = validate_stages(stages.split(","))
    with locked_directory(state_dir, create=not resume and not new_session) as directory:
        try:
            value = load_state(directory, state_dir)
        except SessionError as error:
            if error.code != "NO_SESSION" or resume or new_session:
                raise
            return create_session(directory, identity, order)
        if new_session:
            if overall(value) != "COMPLETED":
                reject("INCOMPLETE_SESSION", "原安装会话尚未完成，必须先以原参数 --resume 收口", 4)
            archive_completed(state_dir, value, identity)
            return create_session(directory, identity, order)
        if comparable_identity(value["identity"]) != comparable_identity(identity) or value["stage_order"] != order:
            reject("IDENTITY_CONFLICT", "目标、介质摘要、模式、组件、选项或阶段列表与原会话不一致", 4)
        if not resume:
            reject("RESUME_REQUIRED", "已有安装会话；先查看状态，并以原参数显式 --resume，不能覆盖旧批次", 4)
        event = {"at": now(), "action": "RESUME", "stage": "", "status": "", "reason_code": "", "generation": value["generation"] + 1, "attempt": 0}
        publish(directory, value, event)
        return value["session_id"]


def update(state_dir, session, stage, status, reason):
    canonical_path(state_dir)
    if not SESSION.fullmatch(session) or not STAGE.fullmatch(stage) or status not in UPDATE_STATUSES or (reason and not TOKEN.fullmatch(reason)):
        reject("INVALID_ARGUMENTS", "会话、阶段、状态或固定原因码格式无效")
    if reason == "NONE":
        reason = ""
    if status in ("RUNNING", "DONE") and reason:
        reject("INVALID_ARGUMENTS", "RUNNING 和 DONE 不接受原因内容")
    if not reason:
        reason = {"FAILED": "STAGE_FAILED", "WAITING": "OPERATOR_ACTION_REQUIRED", "SKIPPED": "EXPLICITLY_SKIPPED"}.get(status, "")
    try:
        with locked_directory(state_dir) as directory:
            value = load_state(directory, state_dir)
            if session != value["session_id"]:
                reject("SESSION_MISMATCH", "会话标识不匹配", 5)
            matching = [item for item in value["stages"] if item["key"] == stage]
            if not matching:
                reject("INVALID_TRANSITION", "阶段不属于本次固定安装计划", 5)
            previous = matching[0]
            if previous["generation"] == value["generation"] and previous["status"] == status and previous["reason_code"] == reason:
                return
            attempt = previous["attempts"] + (1 if status == "RUNNING" else 0)
            event = {"at": now(), "action": "UPDATE", "stage": stage, "status": status,
                     "reason_code": reason, "generation": value["generation"], "attempt": attempt}
            publish(directory, value, event)
    except FileNotFoundError:
        reject("NO_SESSION", "尚无安装会话", 3)


def status_document(state_dir):
    canonical_path(state_dir)
    try:
        with locked_directory(state_dir) as directory:
            value = load_state(directory, state_dir)
    except FileNotFoundError:
        reject("NO_SESSION", "尚无安装会话", 3)
    result = dict(value)
    result.update(overall_status=overall(value), current_stage=value["advisory"]["next_stage"],
                  next_stage=value["advisory"]["next_stage"], next_action=value["advisory"]["action"],
                  completed_stages=[item["key"] for item in value["stages"] if item["generation"] == value["generation"] and item["status"] in TERMINAL],
                  historical_completed_stages=[item["key"] for item in value["stages"] if item["generation"] != value["generation"] and item["status"] in TERMINAL])
    return result


def stage_label(key):
    names = {"host-deps": "宿主依赖", "assets": "部署资产", "packages": "镜像包", "configure": "运行配置",
             "platform": "平台发布", "frontend": "前端发布", "license-prepare": "授权安装准备",
             "license": "授权处理", "verify": "基础验收", "controlled": "业务及授权验收"}
    label = names.get(key)
    if label is None:
        for suffix, translated in (("-import", "镜像导入"), ("-prepare", "候选准备"), ("-stage", "受控暂存")):
            if key.endswith(suffix):
                label = translated
                break
    return key + "（" + label + "）" if label else key


def human_status(value):
    labels = {"PENDING": "待执行", "RUNNING": "执行中", "FAILED": "失败", "WAITING": "等待人工处理", "COMPLETED": "阶段记录已完成"}
    pending = [item["key"] for item in value["stages"][value["cursor"]:]]
    actions = {"VERIFY_REAL_INSTALLATION": "核验真实部署、健康与授权状态；后续升级/增装可显式 --new-session 归档并开启新批次。",
               "RUN_STAGE": "由原一键部署入口按顺序执行当前阶段并核验实际状态。",
               "WAIT_FOR_RUNNER_OR_RESUME": "等待原部署进程；确认已中断后，使用原参数追加 --resume 重验。",
               "RESUME_AND_REVERIFY": "处理固定失败原因后，使用原参数追加 --resume，按顺序真实重验。",
               "OPERATOR_ACTION_AND_RESUME": "按原部署入口提示完成操作，再以原参数追加 --resume。"}
    lines = ["安装会话：" + value["session_id"], "状态：" + labels[value["overall_status"]],
             "已完成（本轮记录）：" + (", ".join(map(stage_label, value["completed_stages"])) or "无"),
             "历史已完成、等待重验：" + (", ".join(map(stage_label, value["historical_completed_stages"])) or "无"),
             "当前阶段：" + (stage_label(value["current_stage"]) if value["current_stage"] else "无"), "待处理：" + (", ".join(map(stage_label, pending)) or "无")]
    if value["last_failure"]:
        lines.append("最近失败：" + stage_label(value["last_failure"]["stage"]) + " / " + value["last_failure"]["reason_code"])
    next_action = actions[value["next_action"]]
    if value["overall_status"] == "WAITING":
        reason = value["stages"][value["cursor"]]["reason_code"]
        lines.append("当前等待原因：" + reason)
        next_action = {
            "WAITING_AUTHORIZATION": "厂商电脑：准备绑定本实例的正式许可证和完整运行批准材料；基础平台管理端：导入正式许可证；服务器：复用原安装参数追加 --resume，核验真实授权和组件 ACK。",
            "WAITING_PLATFORM_ADOPTION": "基础平台管理端：完成当前等待系统的受控接入并确认 READY；服务器：复用原安装参数追加 --resume，重验实际部署。",
            "WAITING_CONTROLLED_UPDATE": "基础平台管理端：执行当前等待系统的受控更新并确认 READY；服务器：复用原安装参数追加 --resume，重验实际部署。",
            "WAITING_MIGRATION": "服务器：按原部署入口处理已冻结的存量迁移，确认实际组件就绪后，以原参数追加 --resume；正式许可证由基础平台管理端导入。",
            "ASSETS_RECOVERY_REQUIRED": "服务器：根据原部署入口恢复经过校验的部署资产，保留原目标与安装身份，然后追加 --resume 重验。",
            "COMPOSE_REVIEW_REQUIRED": "服务器：审核原部署入口列出的 Compose 变更并完成原流程要求的显式确认，再以原参数追加 --resume。",
        }.get(reason, next_action)
    lines.extend(["下一步：" + next_action,
                  "命令位置：原 服务器一键离线部署.sh 所在目录；必须复用原介质、目标及完整选项。",
                  "阶段日志仅用于恢复提示，不证明容器健康、业务验收或商业授权已生效。"])
    return "\n".join(lines)


class SafeParser(argparse.ArgumentParser):
    def __init__(self, *args, **kwargs):
        kwargs.setdefault("allow_abbrev", False)
        super().__init__(*args, **kwargs)

    def error(self, _message):
        reject("INVALID_ARGUMENTS", "命令参数不完整或不受支持；使用 --help 查看接口")


def run(argv=None):
    parser = SafeParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True, parser_class=SafeParser)
    start = commands.add_parser("begin")
    start.add_argument("--state-dir", required=True)
    start.add_argument("--identity-file", required=True)
    start.add_argument("--stages", required=True)
    start.add_argument("--resume", action="store_true")
    start.add_argument("--new-session", action="store_true")
    change = commands.add_parser("update")
    change.add_argument("--state-dir", required=True)
    change.add_argument("--session", required=True)
    change.add_argument("--stage", required=True)
    change.add_argument("--status", required=True)
    change.add_argument("--reason-code", default="")
    show = commands.add_parser("status")
    show.add_argument("--state-dir", required=True)
    show.add_argument("--format", choices=("human", "json"), default="human")
    args = parser.parse_args(argv)
    if args.command == "begin":
        print(begin(args.state_dir, args.identity_file, args.stages, args.resume, args.new_session))
    elif args.command == "update":
        update(args.state_dir, args.session, args.stage, args.status, args.reason_code)
    else:
        value = status_document(args.state_dir)
        print(json.dumps(value, ensure_ascii=False, sort_keys=True) if args.format == "json" else human_status(value))
    return 0


def main(argv=None):
    try:
        return run(argv)
    except SessionError as error:
        print("[" + error.code + "] " + str(error), file=sys.stderr)
        return error.exit_code
    except (OSError, OverflowError, RecursionError, TypeError, KeyError):
        print("[STATE_IO_ERROR] 无法安全读写会话记录；请检查文件类型、权限与完整性。", file=sys.stderr)
        return 2


if __name__ == "__main__":
    sys.exit(main())
