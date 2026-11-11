#!/usr/bin/env python3
"""客户外层安装编排器；只执行已经独立验签的批次原部署工具。"""
import argparse
import contextlib
import fcntl
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import platform
import re
import shutil
import signal
import stat
import subprocess
import sys
import tarfile
import tempfile

DIGEST = re.compile(r"[a-f0-9]{64}")
BOOTSTRAP = "服务器一键离线部署.sh"
DEFAULT_CACHE = "/var/lib/uip-customer-install"


class InstallError(Exception):
    """安装门禁或外部依赖阻断，调用方必须停止。"""


# 用途：拒绝非法文件路径与任何已有符号链接分量。
# 参数：path 是待操作路径。
# 返回值：规范绝对 Path；不安全路径抛出 InstallError。
def safe_path(path):
    value = Path(os.path.abspath(path))
    if ".." in Path(path).parts:
        raise InstallError("路径不能包含 ..")
    for item in (value, *value.parents):
        if item.is_symlink():
            raise InstallError("路径包含符号链接：" + str(item))
    return value


# 用途：以非跟随链接的方式读取普通文件。
# 参数：path 是文件位置，limit 是允许读取的字节上限。
# 返回值：文件字节；非普通文件或超限时阻断。
def read_file(path, limit=8 * 1024 * 1024):
    path = safe_path(path)
    descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(descriptor, "rb") as stream:
        if not stat.S_ISREG(os.fstat(stream.fileno()).st_mode):
            raise InstallError("不是普通文件：" + str(path))
        data = stream.read(limit + 1)
    if len(data) > limit:
        raise InstallError("元数据文件过大：" + str(path))
    return data


# 用途：流式计算文件摘要，避免把镜像归档载入内存。
# 参数：path 是普通文件路径。
# 返回值：小写 SHA-256 摘要；不安全文件阻断。
def sha256(path):
    path = safe_path(path)
    descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(descriptor, "rb") as stream:
        if not stat.S_ISREG(os.fstat(stream.fileno()).st_mode):
            raise InstallError("不是普通文件：" + str(path))
        digest = hashlib.sha256()
        for block in iter(lambda: stream.read(8 * 1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


# 用途：拒绝歧义 JSON 字段。
# 参数：pairs 是解析器读出的键值对。
# 返回值：唯一键字典；重复键阻断。
def unique_pairs(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise InstallError("JSON 存在重复字段：" + key)
        result[key] = value
    return result


# 用途：解析受限 JSON 数据。
# 参数：data 是 UTF-8 字节或文本。
# 返回值：JSON 对象；非法数据统一阻断。
def parse_json(data):
    try:
        return json.loads(data, object_pairs_hook=unique_pairs)
    except (ValueError, UnicodeError) as exc:
        raise InstallError("JSON 材料无效") from exc


# 用途：约束外层批次配置，禁止配置注入命令或任意路径。
# 参数：config 是批次配置对象。
# 返回值：原配置；缺项或类型不符时阻断。
def validate_config(config):
    fields = {"schema", "customer", "release_id", "target_platform", "archive",
              "archive_sha256", "archive_size", "archive_root", "manifest_sha256",
              "delivery_id", "trust_sha256", "verifier_sha256", "installer_sha256"}
    if not isinstance(config, dict) or set(config) != fields:
        raise InstallError("客户安装配置字段无效")
    if config["schema"] != "urn:uip:customer-install:v1":
        raise InstallError("客户安装配置版本不支持")
    for key in fields - {"archive_size"}:
        if not isinstance(config[key], str) or not config[key] or len(config[key]) > 256:
            raise InstallError("客户安装配置类型无效：" + key)
    for key in fields:
        if key.endswith("sha256") or key == "delivery_id":
            if not DIGEST.fullmatch(config[key]):
                raise InstallError("客户安装摘要无效：" + key)
    for key in ("archive", "archive_root", "release_id"):
        if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._-]{0,180}", config[key]):
            raise InstallError("批次文件名无效：" + key)
    if (type(config["archive_size"]) is not int or config["archive_size"] <= 0
            or config["target_platform"] != "linux/amd64"):
        raise InstallError("归档大小或目标平台无效")
    return config


# 用途：检查 tar 的全部成员并读取少量身份材料，禁止穿越和链接。
# 参数：archive 是未压缩总归档路径。
# 返回值：顶层目录、manifest、SHA256SUMS 摘要及成员数量。
def archive_info(archive):
    seen, roots, metadata, total = set(), set(), {}, 0
    archive = safe_path(archive)
    with tarfile.open(archive, "r:") as source:
        for member in source:
            name = checked_member(member, seen)
            parts = PurePosixPath(name).parts
            roots.add(parts[0])
            total += member.size
            if len(seen) > 100000 or total > archive.stat().st_size:
                raise InstallError("归档成员数量或大小异常")
            if len(parts) == 1 and not member.isdir():
                raise InstallError("归档必须有单一顶层目录")
            if len(parts) == 2 and parts[1] in (
                    "release-manifest.json", "SHA256SUMS", BOOTSTRAP):
                if not member.isfile() or member.size > 8 * 1024 * 1024:
                    raise InstallError("归档身份材料无效")
                metadata[parts[1]] = source.extractfile(member).read()
    if len(roots) != 1 or set(metadata) != {"release-manifest.json", "SHA256SUMS", BOOTSTRAP}:
        raise InstallError("归档缺少唯一批次或原部署入口")
    return {"root": roots.pop(), "manifest": parse_json(metadata["release-manifest.json"]),
            "manifest_sha256": hashlib.sha256(metadata["release-manifest.json"]).hexdigest(),
            "delivery_id": hashlib.sha256(metadata["SHA256SUMS"]).hexdigest(),
            "members": len(seen)}


# 用途：在扫描和实际解包时分别检查成员，避免二次打开后的路径变化绕过检查。
# 参数：member 是当前 tar 成员，seen 是本遍已处理名称集合。
# 返回值：安全规范成员名称；危险类型或重复成员阻断。
def checked_member(member, seen):
    name = member.name.removeprefix("./").rstrip("/")
    parts = PurePosixPath(name).parts
    if (not parts or name.startswith("/") or ".." in parts or "\\" in name
            or str(PurePosixPath(name)) != name or name in seen
            or not (member.isdir() or member.isfile()) or member.linkname):
        raise InstallError("归档包含不安全或重复成员：" + member.name)
    if any(ord(char) < 32 for char in name) or len(name) > 1024:
        raise InstallError("归档成员名称无效")
    seen.add(name)
    return name


# 用途：校验外层材料与固定批次绑定；不把摘要检查当作密码学验签。
# 参数：root 是客户目录，config 是固定批次配置。
# 返回值：检查报告；任意文件漂移时阻断。
def inspect_delivery(root, config):
    validate_config(config)
    root = safe_path(root)
    files = {"公钥/trust-policy.json": "trust_sha256",
             "工具/release-verifier-linux-amd64": "verifier_sha256",
             "工具/安装发布信任.sh": "installer_sha256",
             "交付清单/release-manifest.json": "manifest_sha256",
             "交付清单/SHA256SUMS": "delivery_id"}
    for name, key in files.items():
        if sha256(root / name) != config[key]:
            raise InstallError("材料摘要不匹配：" + name)
    archive = root / "镜像" / config["archive"]
    if sha256(archive) != config["archive_sha256"] or archive.stat().st_size != config["archive_size"]:
        raise InstallError("镜像归档摘要或大小不匹配，禁止安装")
    sidecar = read_file(Path(str(archive) + ".sha256"), 4096).decode().strip().split()
    if sidecar != [config["archive_sha256"], config["archive"]]:
        raise InstallError("镜像归档 sidecar 与固定批次不符")
    facts = archive_info(archive)
    for key in ("manifest_sha256", "delivery_id"):
        if facts[key] != config[key]:
            raise InstallError("归档内外发布材料不一致：" + key)
    manifest = facts["manifest"]
    if (facts["root"] != config["archive_root"]
            or manifest.get("release_id") != config["release_id"]
            or manifest.get("target_platform") != config["target_platform"]):
        raise InstallError("归档批次或平台与固定配置不符")
    return {"status": "CHECKED_NOT_VERIFIED", "release_id": config["release_id"],
            "customer": config["customer"], "archive_sha256": config["archive_sha256"],
            "members": facts["members"]}


# 用途：只向全新目录手动解包安全的普通文件，保留原内容且移除特殊权限。
# 参数：archive 是已校验总归档，destination 是不存在的缓存子目录。
# 返回值：无；拒绝覆盖、链接或异常 tar。
def extract_archive(archive, destination):
    archive_info(archive)
    destination = safe_path(destination)
    if destination.exists():
        raise InstallError("解包目录已存在，拒绝覆盖")
    # 不完整解包永远不使用正式缓存名，断电后新运行可以重新提取。
    staging = Path(tempfile.mkdtemp(prefix=".customer-unpack-", dir=destination.parent))
    try:
        with tarfile.open(archive, "r:") as source:
            seen = set()
            for member in source:
                output = staging / checked_member(member, seen)
                if member.isdir():
                    output.mkdir(parents=True, exist_ok=True, mode=0o700)
                    continue
                output.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
                with output.open("xb") as stream, source.extractfile(member) as content:
                    shutil.copyfileobj(content, stream, 8 * 1024 * 1024)
                output.chmod(0o755 if member.mode & 0o111 else 0o644)
        if destination.exists():
            raise InstallError("解包目标发生变化，拒绝覆盖")
        os.rename(staging, destination)
    except BaseException:
        shutil.rmtree(staging)
        raise


# 用途：调用介质外独立验证器，严格核对成功退出和顶层状态。
# 参数：media 是解包介质，verifier 是已受信程序，trust 是宿主信任目录。
# 返回值：验签 JSON；失败或歧义响应阻断。
def verify_media(media, verifier, trust):
    result = subprocess.run([str(verifier), "verify", "--media", str(media),
                             "--trust-dir", str(trust), "--output", "json"],
                            capture_output=True, text=True)
    if result.returncode != 0:
        raise InstallError("独立验签失败，尚未执行介质内脚本")
    report = parse_json(result.stdout)
    if not isinstance(report, dict) or report.get("status") != "VERIFIED":
        raise InstallError("独立验签未返回顶层 VERIFIED，禁止部署")
    return report


HOST_COMMANDS = ("jq", "curl", "openssl", "ss", "cmp", "realpath", "tar",
                 "gzip", "sha256sum", "bash", "python3", "flock", "install")


# 用途：首装或续跑前自动补齐随包宿主依赖，避免原部署器预检因缺命令阻断。
# 参数：media 是已独立验签并解包的介质目录。
# 返回值：无；依赖齐全直接返回，缺命令时按 Ubuntu 版本唯一匹配并安装，无法匹配则阻断。
def ensure_host_dependencies(media):
    media = safe_path(media)
    missing = [name for name in HOST_COMMANDS if shutil.which(name) is None]
    if not missing:
        return
    try:
        release = platform.freedesktop_os_release()
    except OSError as exc:
        raise InstallError("缺少宿主命令：%s；无法读取系统版本，请管理员准备后重试"
                           % "、".join(missing)) from exc
    os_id = release.get("ID", "")
    version = release.get("VERSION_ID", "")
    codename = release.get("VERSION_CODENAME") or release.get("UBUNTU_CODENAME") or ""
    matches = []
    if os_id == "ubuntu" and version and codename:
        matches = sorted(media.glob("host-dependencies-%s-%s-%s-*-linux-amd64.tar.gz"
                                    % (os_id, version, codename)))
    if len(matches) != 1:
        raise InstallError("缺少宿主命令：%s；介质中没有唯一匹配 %s %s %s 的宿主依赖包，"
                           "请管理员准备后重试" % ("、".join(missing), os_id or "unknown",
                                                    version or "unknown", codename or "unknown"))
    installer = media / "install-host-dependencies.sh"
    if installer.is_symlink() or not installer.is_file():
        raise InstallError("介质缺少宿主依赖安装入口，拒绝执行")
    print("[信息] 缺少宿主命令：%s；正在使用已验签介质安装随包宿主依赖。" % "、".join(missing),
          flush=True)
    result = subprocess.run(["bash", str(installer), str(matches[0])])
    if result.returncode != 0:
        raise InstallError("随包宿主依赖安装失败（退出码 %s）" % result.returncode)
    still = [name for name in HOST_COMMANDS if shutil.which(name) is None]
    if still:
        raise InstallError("宿主依赖安装后仍缺少命令：" + "、".join(still))


# 用途：把部署器记录的摘要归一为裸 SHA-256，兼容 "sha256:" 前缀格式。
# 参数：value 是会话身份或批次配置中的摘要。
# 返回值：小写裸摘要；非字符串返回空字符串。
def normalized_digest(value):
    if not isinstance(value, str):
        return ""
    text = value.strip().lower()
    return text.removeprefix("sha256:")


# 用途：比较部署器记录的路径与会话期望路径（两者都可能经过 realpath）。
# 参数：value 是记录值，expected 是本次期望路径。
# 返回值：一致返回 True；非字符串或不同返回 False。
def same_path(value, expected):
    if not isinstance(value, str) or not value:
        return False
    return os.path.realpath(value) == os.path.realpath(str(expected))


# 用途：核对安装会话身份，并区分实际完成与人工待办。
# 参数：session 是原部署器状态，config 是批次配置，target/media 是固定安装路径。
# 返回值：完成返回 0，等待返回 2；身份或状态异常阻断。
def session_result(session, config, target, media):
    identity = session.get("identity", {})
    # 部署器身份使用 realpath 且 delivery_id 带 "sha256:" 前缀；这里按同一语义比较，
    # 不能直接比较客户目录中的裸摘要字符串。
    if (not same_path(identity.get("target"), target)
            or not same_path(identity.get("media"), media)
            or identity.get("mode") != "install"
            or normalized_digest(identity.get("delivery_id")) != normalized_digest(config["delivery_id"])):
        raise InstallError("已有安装会话属于其他介质、目标或模式，拒绝自动接管")
    status = session.get("overall_status")
    if status == "COMPLETED":
        return 0
    if status in ("WAITING", "PENDING", "FAILED", "RUNNING"):
        return 2
    raise InstallError("安装会话状态未知，拒绝按成功处理")


# 用途：建立或核验仅当前管理员可写的专属缓存目录。
# 参数：path 是缓存目录。
# 返回值：安全绝对路径；权限或所有者异常阻断。
def private_directory(path):
    path = safe_path(path)
    path.mkdir(mode=0o700, parents=True, exist_ok=True)
    info = path.stat()
    if not stat.S_ISDIR(info.st_mode) or info.st_uid != os.geteuid() or info.st_mode & 0o077:
        raise InstallError("安装缓存必须归当前管理员所有且权限为 0700：" + str(path))
    for parent in path.parents:
        info = parent.stat()
        if info.st_uid not in (0, os.geteuid()) or (info.st_mode & 0o022 and not info.st_mode & stat.S_ISVTX):
            raise InstallError("缓存父路径可被其他用户改写：" + str(parent))
    return path


# 用途：避免两个入口同时编排安装或更换信任。
# 参数：cache 是管理员专属目录。
# 返回值：持锁上下文；锁冲突阻断，退出后自动释放。
@contextlib.contextmanager
def installation_lock(cache):
    descriptor = os.open(cache / "install.lock", os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600)
    try:
        try:
            fcntl.flock(descriptor, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError as exc:
            raise InstallError("已有一键安装入口在运行，请等待原任务结束") from exc
        yield
    finally:
        os.close(descriptor)


# 用途：从真实终端获取必要的人工作业确认，不接受管道默认同意。
# 参数：message 是提示，expected 是必须输入的确认文字。
# 返回值：无；取消、无终端或文字不符时阻断。
def confirm(message, expected):
    try:
        with open("/dev/tty", "r+") as terminal:
            terminal.write(message + "\n请输入“" + expected + "”继续，其他输入取消：")
            terminal.flush()
            answer = terminal.readline().strip()
    except OSError as exc:
        raise InstallError("此步骤需要现场终端确认，请在交互终端运行") from exc
    if answer != expected:
        raise InstallError("操作已取消，未授权继续")


# 用途：在独立进程组运行原部署器，取消时转发信号并等候退出。
# 参数：command 是参数数组；不经 shell 拼接或记录凭据。
# 返回值：无；失败或取消保留原会话并抛出错误。
def run_attended(command):
    child = subprocess.Popen(command, start_new_session=True)
    try:
        code = child.wait()
    except KeyboardInterrupt:
        # 取消清理期间不再接受第二次中断，避免部署器存活时提前释放外层锁。
        handlers = {number: signal.signal(number, signal.SIG_IGN)
                    for number in (signal.SIGINT, signal.SIGTERM)}
        try:
            try:
                os.killpg(child.pid, signal.SIGINT)
            except ProcessLookupError:
                pass
            try:
                child.wait(timeout=30)
            except subprocess.TimeoutExpired:
                try:
                    os.killpg(child.pid, signal.SIGTERM)
                except ProcessLookupError:
                    pass
                print("[信息] 正在等待原部署器退出；保留锁及原安装记录，请勿启动其他安装。", flush=True)
                child.wait()
        finally:
            for number, handler in handlers.items():
                signal.signal(number, handler)
        raise InstallError("安装已取消；已完成的变更和会话保留，原命令可重验后续跑")
    if code != 0:
        raise InstallError("原部署器停止（退出码 %s）；处理上方原因后重运行本入口，不自动清理数据" % code)


# 用途：仅在独立核对确认后安装本批信任，禁止自动覆盖冲突文件。
# 参数：root/config 是外层批次，work 是管理员缓存；check_only 禁止安装信任。
# 返回值：宿主 verifier 和 trust 路径；缺失或冲突阻断。
def ensure_trust(root, config, work, check_only):
    verifier = safe_path("/usr/local/bin/release-verifier")
    trust = safe_path("/etc/unified-identity-platform/release-trust")
    destinations = {verifier: config["verifier_sha256"],
                    trust / "trust-policy.json": config["trust_sha256"]}
    for path, expected in destinations.items():
        safe_path(path)
        if path.exists() and sha256(path) != expected:
            raise InstallError("已有不同的可信配置，须管理员独立审核并备份；不会自动覆盖：" + str(path))
    if all(path.exists() for path in destinations):
        for path in destinations:
            protected_trust_path(path)
        if not os.access(verifier, os.X_OK):
            raise InstallError("已有受信验证器不可执行")
        return verifier, trust
    if check_only:
        raise InstallError("尚未安装本批信任；--check-only 不安装信任，请交互运行一键入口完成独立核对")
    confirm("首次安装发布信任。请向厂商通过独立可信渠道核对以下摘要；同包清单不能证明来源。\n"
            "trust-policy SHA-256：" + config["trust_sha256"] + "\n"
            "verifier SHA-256：" + config["verifier_sha256"], "已独立核对")
    snapshots = (("公钥/trust-policy.json", "trust.json", "trust_sha256"),
                 ("工具/release-verifier-linux-amd64", "verifier", "verifier_sha256"),
                 ("工具/安装发布信任.sh", "install-trust.sh", "installer_sha256"))
    for source, name, key in snapshots:
        data = read_file(root / source, 64 * 1024 * 1024)
        if hashlib.sha256(data).hexdigest() != config[key]:
            raise InstallError("信任材料在检查后发生变化，禁止执行")
        (work / name).write_bytes(data)
        (work / name).chmod(0o700)
    run_attended(["bash", str(work / "install-trust.sh"), "--trust-policy", str(work / "trust.json"),
                  "--verifier-source", str(work / "verifier"), "--trust-dir", str(trust),
                  "--verifier-path", str(verifier), "--expected-trust-sha256", config["trust_sha256"],
                  "--expected-verifier-sha256", config["verifier_sha256"]])
    if any(sha256(path) != expected for path, expected in destinations.items()):
        raise InstallError("安装后的信任摘要不一致")
    for path in destinations:
        protected_trust_path(path)
    return verifier, trust


# 用途：确认宿主可信工具及其父路径不能由普通用户替换。
# 参数：path 是已安装的 verifier 或信任策略。
# 返回值：无；所有者、权限或链接不安全时阻断。
def protected_trust_path(path):
    path = safe_path(path)
    for item in (path, *path.parents):
        info = item.stat()
        if info.st_uid != 0 or info.st_mode & 0o022:
            raise InstallError("宿主信任路径必须归 root 所有且不能被其他用户写入：" + str(item))


# 用途：读取原部署器的受控会话状态，避免自行猜测成功。
# 参数：bootstrap 是已验签脚本，target 是安装目录。
# 返回值：原会话 JSON；损坏或工具失败时阻断。
def read_session(bootstrap, target):
    result = subprocess.run(["bash", str(bootstrap), "--target", str(target),
                             "--status", "--format", "json"], capture_output=True, text=True)
    if result.returncode != 0:
        raise InstallError("无法读取原安装会话，请保留现场并联系管理员")
    value = parse_json(result.stdout)
    if not isinstance(value, dict):
        raise InstallError("原安装会话响应无效")
    return value


# 用途：编排本批首装与恢复；每次执行前重新校验和独立验签。
# 参数：root/config 是客户目录事实，args 是用户选择的路径与只读开关。
# 返回值：成功 0，真实人工待办 2；其他异常由顶层阻断。
def install(root, config, args):
    if platform.system() != "Linux" or platform.machine() != "x86_64":
        raise InstallError("正式安装只支持 Linux/amd64；本机可使用 --inspect 检查外层材料")
    if os.geteuid() != 0:
        raise InstallError("请使用 sudo bash 一键安装.sh；不会自动提权")
    if sys.version_info < (3, 10):
        raise InstallError("需要 Python 3.10 或以上版本")
    for command in ("bash", "sha256sum", "tar", "flock", "install", "docker"):
        if not shutil.which(command):
            raise InstallError("缺少基础命令：" + command + "；请管理员准备后重试，不自动联网安装")
    target, cache = safe_path(args.target), safe_path(args.cache_dir)
    paths = (target, cache, root)
    if any(path == Path("/") for path in paths) or any(
            left == right or left in right.parents or right in left.parents
            for left, right in ((target, cache), (target, root), (cache, root))):
        raise InstallError("安装目录、客户目录和缓存必须互不包含，且不能是根目录")
    cache = private_directory(cache)
    with installation_lock(cache):
        key = hashlib.sha256((config["archive_sha256"] + str(target)).encode()).hexdigest()
        work = private_directory(cache / key)
        verifier, trust = ensure_trust(root, config, work, args.check_only)
        print("[2/4] 本批宿主信任已核对，准备解包缓存。", flush=True)
        unpacked = work / "unpacked"
        if not unpacked.exists():
            extract_archive(root / "镜像" / config["archive"], unpacked)
        media = safe_path(unpacked / config["archive_root"])
        print("[3/4] 正在使用宿主可信验证器独立验签。", flush=True)
        verify_media(media, verifier, trust)
        if (sha256(media / "release-manifest.json") != config["manifest_sha256"]
                or sha256(media / "SHA256SUMS") != config["delivery_id"]):
            raise InstallError("已验签缓存与当前批次不符")
        return deploy_verified(root, config, args, target, work, media, verifier, trust)


# 用途：调用已验签的原首装入口，并保留其唯一真实恢复状态。
# 参数：root/config/args 是外层事实；target/work/media 和 verifier/trust 是安全路径。
# 返回值：完成 0，等待人工 2；异常不覆盖旧配置、不升级已有目标。
def deploy_verified(root, config, args, target, work, media, verifier, trust):
    bootstrap = media / BOOTSTRAP
    binding = {"target": str(target), "archive_sha256": config["archive_sha256"],
               "media": str(media), "delivery_id": config["delivery_id"]}
    receipt = work / "installation.json"
    state = safe_path(str(target) + ".installation-state")
    if receipt.exists() and parse_json(read_file(receipt)) != binding:
        raise InstallError("入口安装记录不匹配，禁止自动接管")
    resume = state.exists()
    session = None
    if resume:
        if not receipt.exists():
            raise InstallError("目标已有非本入口管理的会话，须管理员核对后使用原恢复流程")
        session = read_session(bootstrap, target)
        session_result(session, config, target, media)
    elif target.exists() and (not target.is_dir() or any(target.iterdir())):
        raise InstallError("目标已有文件但没有匹配安装会话，拒绝覆盖或自动升级")
    command = ["bash", str(bootstrap), "--media", str(media), "--target", str(target),
               "--install", "--require-release-integrity", "--release-verifier", str(verifier),
               "--release-trust-dir", str(trust)]
    if resume:
        command.append("--resume")
    # 正式安装与续跑都需要宿主命令；--check-only 保持只读，不安装宿主依赖。
    if not args.check_only:
        ensure_host_dependencies(media)
    # 旧批次的 --install --resume --check-only 仍调用首装计划，会把原配置判成冲突。
    # 正式 --resume 本身重验原介质、主机、资产及会话，不调用不适用的首装预检。
    if not resume:
        print("[4/4] 独立验签通过；开始原部署器首装预检。", flush=True)
        run_attended(command + ["--check-only"])
    if args.check_only:
        if resume:
            run_attended(["bash", str(bootstrap), "--target", str(target), "--status"])
            print("[只读] 独立验签及原会话身份核验通过；不是新的首装预检。"
                  "未执行部署，实际恢复检查由无 --check-only 的原续跑入口执行。", flush=True)
        else:
            print("[信息] 首装预检通过；未执行部署。", flush=True)
        return 0
    if not receipt.exists():
        with receipt.open("x") as stream:
            json.dump(binding, stream, ensure_ascii=False, sort_keys=True)
        receipt.chmod(0o600)
    print("[信息] " + ("继续同批安装会话。" if resume else "开始首次部署；必要配置由原向导询问。"), flush=True)
    run_attended(command)
    session = read_session(bootstrap, target)
    result = session_result(session, config, target, media)
    # 先让原续跑完成其真实资产和会话门禁，再对原接口明确就绪的采用收口。
    if result == 2 and finish_ready_adoptions(target, session):
        run_attended(command if resume else command + ["--resume"])
        session = read_session(bootstrap, target)
        result = session_result(session, config, target, media)
    if result == 0:
        print("[完成] 原安装会话与验收阶段已完成。", flush=True)
    else:
        print("[待处理] 安装会话尚未完成：" + str(session.get("current_stage", "未知阶段")), flush=True)
        print("请按下方原部署器提示处理。许可证：在基础平台‘许可证’导出实例申请，交正式签发方签发后导入；"
              "应用接入：在基础平台完成对应应用的受控接入。不能以‘北京’目录名代替 customer_id。", flush=True)
        run_attended(["bash", str(bootstrap), "--target", str(target), "--status"])
        print("完成必要处理后，再运行同一个一键安装入口；自动重验并续跑，不重建镜像。", flush=True)
    return result


# 用途：对平台已经真实完成采用的组件调用原收口接口，其他阶段保留人工接入。
# 参数：target 是安装位置，session 是原受控会话；只处理最终收口阶段。
# 返回值：实际收口返回 True，无就绪组件返回 False；未知状态或验收失败阻断。
def finish_ready_adoptions(target, session):
    if session.get("current_stage") != "controlled" or session.get("overall_status") != "WAITING":
        return False
    allowed = {"customer-opportunity", "customer-portal", "contract", "project", "reimbursement", "settlement", "data-analysis"}
    deployer = safe_path(target / "bin/deploy.sh")
    changed = False
    for component in session.get("identity", {}).get("components", []):
        if component not in allowed:
            continue
        result = subprocess.run(["bash", str(deployer), "status", component],
                                capture_output=True, text=True)
        phases = re.findall(r"^当前阶段：([A-Z_]+)$", result.stdout, re.MULTILINE)
        if result.returncode != 0 or len(phases) != 1:
            raise InstallError("无法安全读取应用接入阶段：" + component + "；请在平台核对后重试")
        if phases[0] == "READY_TO_CONTINUE":
            run_attended(["bash", str(deployer), "continue", component])
            run_attended(["bash", str(deployer), "verify", component])
            changed = True
    return changed


# 用途：提供面向客户的单命令接口及无写入材料检查入口。
# 参数：argv 是入口转发的命令行参数。
# 返回值：退出码；错误不输出凭据或完整运行配置。
def main(argv=None):
    parser = argparse.ArgumentParser(description="客户一键安装：校验 → 独立验签 → 原部署器 → 同批续跑")
    parser.add_argument("--target", default="/opt/unified-identity-platform", help="安装位置（高级选项）")
    parser.add_argument("--cache-dir", default=DEFAULT_CACHE, help="管理员专属缓存（高级选项）")
    mode = parser.add_mutually_exclusive_group()
    mode.add_argument("--inspect", action="store_true", help="只读检查外层与归档，不执行验签或安装")
    mode.add_argument("--check-only", action="store_true", help="验签并首装预检；已有会话只核验身份及状态")
    args = parser.parse_args(argv)
    root = safe_path(Path(__file__).absolute().parent.parent)
    config = parse_json(read_file(root / "工具/customer-install.json"))
    if not args.inspect:
        print("[1/4] 正在核对客户目录和完整镜像归档；不会重新构建镜像。", flush=True)
    report = inspect_delivery(root, config)
    if args.inspect:
        print(json.dumps(report, ensure_ascii=False))
        return 0
    return install(root, config, args)


if __name__ == "__main__":
    try:
        signal.signal(signal.SIGTERM, signal.default_int_handler)
        sys.exit(main())
    except (InstallError, OSError, tarfile.TarError, KeyboardInterrupt) as error:
        print("[停止] " + (str(error) or "操作已取消；保留实际状态，处理后重运行本入口"), file=sys.stderr)
        sys.exit(1)