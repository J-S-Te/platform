#!/usr/bin/env python3
"""客户外层入口的隔离回归；不连接 Docker、业务服务或正式许可系统。"""
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import subprocess
import sys
import tarfile
import tempfile
import unittest
from types import SimpleNamespace
from unittest import mock

BIN = Path(__file__).resolve().parents[1] / "bin"
ROOT = Path(__file__).resolve().parents[4]


# 用途：装载被测权威模块。
# 参数：name 是独立模块名，filename 是工具文件名。
# 返回值：模块对象；缺失实现导致测试明确失败。
def load(name, filename):
    path = BIN / filename
    if not path.is_file():
        raise AssertionError("客户一键入口尚未实现：" + str(path))
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    sys.modules[name] = module
    spec.loader.exec_module(module)
    return module


class CustomerInstallTests(unittest.TestCase):
    # 用途：构造独立的外层交付与极小归档，不创建运行环境。
    # 参数：无。
    # 返回值：无；夹具由测试结束时自动清理。
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="uip-customer-test-")
        self.addCleanup(self.temp.cleanup)
        self.base = Path(self.temp.name).resolve()
        self.delivery = self.base / "北京"
        for directory in ("镜像", "工具", "公钥", "交付清单"):
            (self.delivery / directory).mkdir(parents=True)
        self.manifest = {"schema": "urn:uip:release-manifest:v1",
                         "release_id": "qa-r1", "target_platform": "linux/amd64"}
        self.files = {
            "release-manifest.json": json.dumps(self.manifest).encode(),
            "release-manifest.sigstore.json": b'{}',
            "SHA256SUMS": b"isolated signed checksum fixture\n",
            "服务器一键离线部署.sh": b"#!/bin/bash\nexit 0\n",
        }
        self.archive = self.delivery / "镜像/qa-r1.tar"
        self.write_archive()
        for name, content in self.files.items():
            (self.delivery / "交付清单" / name).write_bytes(content)
        (self.delivery / "公钥/trust-policy.json").write_text('{"test":true}')
        (self.delivery / "工具/release-verifier-linux-amd64").write_text("fixture verifier")
        (self.delivery / "工具/安装发布信任.sh").write_text("#!/bin/bash\nexit 0\n")
        (self.delivery / "工具/SHA256SUMS").write_text("")
        self.generator = load("customer_generator", "prepare-customer-installer.py")
        self.runtime = load("customer_runtime_test", "customer-install.py")
        self.generator.prepare(self.delivery)
        self.config = json.loads((self.delivery / "工具/customer-install.json").read_text())

    # 用途：写入受控 tar 和独立计算的校验旁文件。
    # 参数：extra 是可选的额外 tar 成员，便于验证拒绝路径。
    # 返回值：无。
    def write_archive(self, extra=None):
        with tarfile.open(self.archive, "w") as output:
            for name, content in self.files.items():
                item = tarfile.TarInfo("qa-r1/" + name)
                item.size, item.mode = len(content), 0o755
                output.addfile(item, io.BytesIO(content))
            if extra:
                output.addfile(extra)
        digest = hashlib.sha256(self.archive.read_bytes()).hexdigest()
        Path(str(self.archive) + ".sha256").write_text(digest + "  qa-r1.tar\n")

    # 用途：验证客户真正可以从任意工作目录只读检查，并防止辅助程序被替换。
    # 参数：无。
    # 返回值：无。
    def test_entry_inspects_without_executing_and_rejects_tampered_runtime(self):
        entry = self.delivery / "一键安装.sh"
        result = subprocess.run(["bash", str(entry), "--inspect"], cwd=self.base,
                                capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(json.loads(result.stdout)["status"], "CHECKED_NOT_VERIFIED")
        self.assertFalse((self.base / "target").exists())
        with (self.delivery / "工具/customer-install.py").open("a") as stream:
            stream.write("\nraise RuntimeError('executed tampered runtime')\n")
        result = subprocess.run(["bash", str(entry), "--inspect"],
                                capture_output=True, text=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn("executed tampered runtime", result.stderr)

    # 用途：验证外层文件损坏不会继续安装或被再次生成掩盖。
    # 参数：无。
    # 返回值：无。
    def test_tampered_archive_and_conflicting_generated_file_are_rejected(self):
        with self.archive.open("ab") as stream:
            stream.write(b"tampered")
        with self.assertRaises(self.runtime.InstallError):
            self.runtime.inspect_delivery(self.delivery, self.config)
        (self.delivery / "一键安装.sh").write_text("unknown existing entry")
        with self.assertRaises(Exception):
            self.generator.prepare(self.delivery)
        self.assertEqual((self.delivery / "一键安装.sh").read_text(), "unknown existing entry")

    # 用途：验证不安全 tar 在提取前被拒绝。
    # 参数：无。
    # 返回值：无。
    def test_archive_rejects_traversal_links_and_duplicate_members(self):
        for name, kind in (("qa-r1/../../outside", tarfile.REGTYPE),
                           ("qa-r1/link", tarfile.SYMTYPE),
                           ("qa-r1/SHA256SUMS", tarfile.REGTYPE)):
            with self.subTest(name=name):
                item = tarfile.TarInfo(name)
                item.type, item.linkname = kind, "/etc/passwd"
                self.write_archive(item)
                with self.assertRaises(self.runtime.InstallError):
                    self.runtime.archive_info(self.archive)
        self.assertFalse((self.base / "outside").exists())

    # 用途：验证正式验签只接受可信验证器顶层 VERIFIED 且退出成功。
    # 参数：无。
    # 返回值：无。
    def test_independent_verifier_status_cannot_be_nested_or_failed(self):
        verifier = self.base / "verifier"
        for output, code, accepted in (({"status": "VERIFIED"}, 0, True),
                ({"nested": {"status": "VERIFIED"}}, 0, False),
                ({"status": "VERIFIED"}, 1, False)):
            verifier.write_text("#!/bin/sh\nprintf '%s\\n' '" + json.dumps(output)
                                + "'\nexit " + str(code) + "\n")
            verifier.chmod(0o755)
            if accepted:
                self.runtime.verify_media(self.base, verifier, self.base)
            else:
                with self.assertRaises(self.runtime.InstallError):
                    self.runtime.verify_media(self.base, verifier, self.base)

    # 用途：验证续跑只接受原目标、原介质和原首装身份。
    # 参数：无。
    # 返回值：无。
    def test_resume_binding_and_waiting_are_not_reported_as_success(self):
        media, target = self.base / "cache/media", self.base / "target"
        session = {"identity": {"target": str(target), "media": str(media),
                   "mode": "install", "delivery_id": self.config["delivery_id"]},
                   "overall_status": "WAITING", "current_stage": "controlled",
                   "stages": [{"key": "controlled", "reason_code": "WAITING_AUTHORIZATION"}]}
        self.assertEqual(self.runtime.session_result(session, self.config, target, media), 2)
        session["identity"]["delivery_id"] = "0" * 64
        with self.assertRaises(self.runtime.InstallError):
            self.runtime.session_result(session, self.config, target, media)

    # 用途：验证真部署器身份（realpath + "sha256:" 前缀）可被接管，介质或摘要不符仍阻断。
    # 参数：无。
    # 返回值：无。
    def test_session_identity_matches_deployer_digest_and_realpath(self):
        media, target = self.base / "cache/media", self.base / "target"
        session = {"identity": {"target": os.path.realpath(target),
                   "media": os.path.realpath(media), "mode": "install",
                   "delivery_id": "sha256:" + self.config["delivery_id"]},
                   "overall_status": "WAITING", "current_stage": "platform"}
        self.assertEqual(self.runtime.session_result(session, self.config, target, media), 2)
        session["identity"]["media"] = os.path.realpath(self.base / "other-media")
        with self.assertRaises(self.runtime.InstallError):
            self.runtime.session_result(session, self.config, target, media)
        session["identity"]["media"] = os.path.realpath(media)
        session["identity"]["delivery_id"] = "sha256:" + "0" * 64
        with self.assertRaises(self.runtime.InstallError):
            self.runtime.session_result(session, self.config, target, media)

    # 用途：验证解包保留签名文件字节并拒绝覆盖已有缓存。
    # 参数：无。
    # 返回值：无。
    def test_safe_extract_does_not_overwrite_or_follow_links(self):
        destination = self.base / "unpacked"
        self.runtime.extract_archive(self.archive, destination)
        self.assertEqual((destination / "qa-r1/release-manifest.json").read_bytes(),
                         self.files["release-manifest.json"])
        with self.assertRaises(self.runtime.InstallError):
            self.runtime.extract_archive(self.archive, destination)

    # 用途：验证真实入口对旧信任不覆盖，对首次信任不默认同意。
    # 参数：无。
    # 返回值：无。
    def test_missing_trust_requires_confirmation_and_conflicts_stop(self):
        work = self.base / "work"
        work.mkdir()
        original = self.runtime.safe_path
        with mock.patch.object(self.runtime, "safe_path", side_effect=lambda p:
                self.base / str(p).lstrip("/") if str(p).startswith(("/usr/", "/etc/")) else original(p)):
            with self.assertRaises(self.runtime.InstallError), mock.patch.object(self.runtime, "confirm") as prompt:
                self.runtime.ensure_trust(self.delivery, self.config, work, True)
            prompt.assert_not_called()
            verifier = self.base / "usr/local/bin/release-verifier"
            verifier.parent.mkdir(parents=True)
            verifier.write_text("existing different trusted program")
            with self.assertRaises(self.runtime.InstallError), mock.patch.object(self.runtime, "confirm") as prompt:
                self.runtime.ensure_trust(self.delivery, self.config, work, False)
            prompt.assert_not_called()
            self.assertEqual(verifier.read_text(), "existing different trusted program")

    # 用途：验证随包宿主依赖按 Ubuntu 版本唯一匹配后自动安装并复核命令。
    # 参数：无。
    # 返回值：无；只在隔离介质上运行安装替身。
    def test_missing_host_dependency_installs_from_verified_media(self):
        media = self.base / "media"
        media.mkdir()
        package = media / "host-dependencies-ubuntu-22.04-jammy-20261023-r1-linux-amd64.tar.gz"
        package.write_bytes(b"offline host dependencies")
        installer = media / "install-host-dependencies.sh"
        installer.write_text("#!/bin/bash\nexit 0\n")
        present = {"jq": False}

        def which(name):
            if name == "jq":
                return "/usr/bin/jq" if present["jq"] else None
            return "/usr/bin/" + name

        calls = []

        def run(command, **kwargs):
            calls.append(command)
            present["jq"] = True
            return SimpleNamespace(returncode=0)

        with mock.patch.object(self.runtime.shutil, "which", side_effect=which), \
                mock.patch.object(self.runtime.subprocess, "run", side_effect=run), \
                mock.patch.object(self.runtime.platform, "freedesktop_os_release", create=True,
                                  return_value={"ID": "ubuntu", "VERSION_ID": "22.04",
                                                "VERSION_CODENAME": "jammy"}):
            self.runtime.ensure_host_dependencies(media)
        self.assertEqual(calls, [["bash", str(installer), str(package)]])

    # 用途：验证宿主依赖齐全时不执行任何安装。
    # 参数：无。
    # 返回值：无。
    def test_present_host_dependencies_do_not_run_installer(self):
        media = self.base / "media"
        media.mkdir()
        with mock.patch.object(self.runtime.shutil, "which", return_value="/usr/bin/tool"), \
                mock.patch.object(self.runtime.subprocess, "run") as run:
            self.runtime.ensure_host_dependencies(media)
        run.assert_not_called()

    # 用途：验证非 Ubuntu 或缺少唯一依赖包时阻断，不执行安装。
    # 参数：无。
    # 返回值：无。
    def test_unmatched_host_dependency_package_is_rejected(self):
        media = self.base / "media"
        media.mkdir()
        (media / "install-host-dependencies.sh").write_text("#!/bin/bash\nexit 0\n")
        with mock.patch.object(self.runtime.shutil, "which",
                               side_effect=lambda name: None if name == "jq" else "/usr/bin/" + name), \
                mock.patch.object(self.runtime.subprocess, "run") as run, \
                mock.patch.object(self.runtime.platform, "freedesktop_os_release", create=True,
                                  return_value={"ID": "kylin", "VERSION_ID": "V10"}):
            with self.assertRaises(self.runtime.InstallError):
                self.runtime.ensure_host_dependencies(media)
        run.assert_not_called()

    # 用途：验证入口只在正式安装/续跑前补依赖，--check-only 保持只读。
    # 参数：无。
    # 返回值：无；使用替身，不启动真实部署。
    def test_deploy_verified_installs_dependencies_only_for_real_runs(self):
        session = {"identity": {}, "overall_status": "COMPLETED", "current_stage": "controlled"}
        for check_only, expected in ((False, ["deps", "preflight", "install"]),
                                     (True, ["preflight"])):
            with self.subTest(check_only=check_only):
                target = self.base / ("target-check" if check_only else "target-install")
                work = self.base / ("work-check" if check_only else "work-install")
                work.mkdir()
                media = self.base / ("media-check" if check_only else "media-install")
                media.mkdir()
                (media / self.runtime.BOOTSTRAP).write_text("#!/bin/bash\nexit 0\n")
                session["identity"] = {"target": str(target), "media": str(media),
                                       "mode": "install", "delivery_id": self.config["delivery_id"]}
                calls = []
                with mock.patch.object(self.runtime, "ensure_host_dependencies",
                                       side_effect=lambda item: calls.append("deps")), \
                        mock.patch.object(self.runtime, "run_attended",
                                          side_effect=lambda command: calls.append(
                                              "preflight" if command[-1] == "--check-only" else "install")), \
                        mock.patch.object(self.runtime, "read_session", return_value=session):
                    result = self.runtime.deploy_verified(self.delivery, self.config,
                                                          SimpleNamespace(check_only=check_only),
                                                          target, work, media,
                                                          self.base / "verifier", self.base / "trust")
                self.assertEqual(result, 0)
                self.assertEqual(calls, expected)

    # 用途：验证已在平台完成接入的组件自动完成原 continue/verify 收口。
    # 参数：无。
    # 返回值：无。
    def test_resume_finishes_only_ready_adoptions_via_original_deployer(self):
        target = self.base / "target"
        (target / "bin").mkdir(parents=True)
        (target / "calls").write_text("")
        (target / "bin/deploy.sh").write_text('''#!/bin/bash
set -eu
cd -- "$(dirname -- "$0")/.."
printf '%s %s\\n' "$1" "$2" >> calls
case "$1:$2" in
  status:contract) printf '当前阶段：READY_TO_CONTINUE\\n' ;;
  status:project) printf '当前阶段：WAITING_FOR_PLATFORM_ADOPTION\\n' ;;
  continue:contract|verify:contract) ;;
  *) exit 91 ;;
esac
''')
        session = {"current_stage": "controlled", "overall_status": "WAITING",
                   "identity": {"components": ["common", "platform", "contract", "project"]}}
        self.runtime.finish_ready_adoptions(target, session)
        self.assertEqual((target / "calls").read_text().splitlines(),
                         ["status contract", "continue contract", "verify contract", "status project"])

    # 用途：验证不同批次会话及未知已有目标不会触发原部署器。
    # 参数：无。
    # 返回值：无。
    def test_existing_unmanaged_target_is_not_overwritten(self):
        target, media, work = self.base / "target", self.base / "media", self.base / "work"
        target.mkdir()
        work.mkdir()
        (target / "business-data").write_text("preserve")
        args = SimpleNamespace(check_only=False)
        with self.assertRaises(self.runtime.InstallError), mock.patch.object(self.runtime, "run_attended") as run:
            self.runtime.deploy_verified(self.delivery, self.config, args, target, work, media,
                                         self.base / "verifier", self.base / "trust")
        run.assert_not_called()
        self.assertEqual((target / "business-data").read_text(), "preserve")

    # 用途：验证生成器能重复执行，并确实加入未来客户目录整理流程。
    # 参数：无。
    # 返回值：无。
    def test_generation_is_idempotent_and_real_packager_ships_entry(self):
        self.generator.prepare(self.delivery)
        source = self.base / "signed-media"
        source.mkdir()
        for name, data in self.files.items():
            (source / name).write_bytes(data)
        for directory in ("profiles", "attestations"):
            (source / directory).mkdir()
        (source / "BUILD_INFO.txt").write_text("qa fixture")
        for name in ("build.pub", "release.pub"):
            (self.delivery / "公钥" / name).write_text("public-only fixture")
        output = self.base / "future"
        output.mkdir()
        result = subprocess.run(["bash", str(ROOT / "整理客户交付目录.sh"),
            "--customer", "另一个客户", "--media", str(source), "--archive", str(self.archive),
            "--public-keys", str(self.delivery / "公钥"),
            "--verifier", str(self.delivery / "工具/release-verifier-linux-amd64"),
            "--output-parent", str(output)], capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        entry = output / "另一个客户/一键安装.sh"
        self.assertTrue(entry.is_file(), "整理工具没有为未来客户生成一键入口")
        result = subprocess.run(["bash", str(entry), "--inspect"], capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr)

    # 用途：验证已有配置的真实首装计划会阻断，而入口恢复不误用该接口。
    # 参数：无。
    # 返回值：无；只在隔离文件系统执行状态与部署协议夹具。
    def test_resume_avoids_fresh_install_preflight_conflict(self):
        plan = load("customer_test_deployment_plan", "deployment-plan.py")
        target, media, work = self.base / "target", self.base / "media", self.base / "work"
        for path in (target, media, work, Path(str(target) + ".installation-state")):
            path.mkdir()
        for name in (".env", ".release.env", "docker-compose.yml"):
            (target / name).write_text("")
        document = plan.build_plan(target, "install", ["platform"],
            runner=lambda *a, **k: (1, "unavailable in isolated test"), docker_available=False)
        self.assertEqual(next(c for c in document["checks"] if c["id"] == "SCOPE-001")["status"], "BLOCKED")
        session = {"identity": {"target": str(target), "media": str(media), "mode": "install",
                   "delivery_id": self.config["delivery_id"], "components": ["platform"]},
                   "overall_status": "WAITING", "current_stage": "controlled"}
        (media / "session.json").write_text(json.dumps(session))
        (media / self.runtime.BOOTSTRAP).write_text('''#!/bin/bash
set -eu
cd -- "$(dirname -- "$0")"
if [[ " $* " == *" --status "* ]]; then
  if [[ " $* " == *" --format json "* ]]; then cat session.json; else printf '仍待现场授权\\n'; fi
elif [[ " $* " == *" --check-only "* ]]; then
  printf 'fresh-install-plan-conflict' >&2; exit 92
elif [[ " $* " == *" --resume "* ]]; then
  printf 'resumed\\n' >> calls
else exit 93; fi
''')
        (work / "installation.json").write_text(json.dumps({"target": str(target),
            "archive_sha256": self.config["archive_sha256"], "media": str(media),
            "delivery_id": self.config["delivery_id"]}))
        with mock.patch.object(self.runtime, "ensure_host_dependencies"):
            result = self.runtime.deploy_verified(self.delivery, self.config,
                SimpleNamespace(check_only=False), target, work, media,
                self.base / "verifier", self.base / "trust")
        self.assertEqual(result, 2)
        self.assertEqual((media / "calls").read_text(), "resumed\n")

    # 用途：验证连续取消不会在子进程尚未退出时逃出清理区。
    # 参数：无。
    # 返回值：无；不创建真实部署子进程或向外部进程发信号。
    def test_second_interrupt_cannot_release_before_child_exit(self):
        import signal
        child = mock.Mock(pid=123456)
        # 第二次中断来自操作者，清理区应屏蔽它而不是提前释放锁。
        def wait(timeout=None):
            """用途：模拟子进程等待期间的再次中断。
            参数：timeout 是清理等待时限。
            返回值：子进程正常退出状态，第一次调用触发取消。
            """
            if timeout is None:
                raise KeyboardInterrupt()
            os.kill(os.getpid(), signal.SIGINT)
            return 0
        child.wait.side_effect = wait
        with mock.patch.object(self.runtime.subprocess, "Popen", return_value=child), \
                mock.patch.object(self.runtime.os, "killpg"):
            with self.assertRaises(self.runtime.InstallError):
                self.runtime.run_attended(["isolated-child"])

    # 用途：验证硬中断只留下临时缓存，重试不会把半份解包当成正式介质。
    # 参数：无。
    # 返回值：无。
    def test_interrupted_extraction_can_be_retried(self):
        destination = self.base / "unpacked"
        probe = self.base / "interrupt.py"
        probe.write_text('''import importlib.util, os, pathlib, sys
spec=importlib.util.spec_from_file_location('runtime',sys.argv[1])
runtime=importlib.util.module_from_spec(spec);spec.loader.exec_module(runtime)
runtime.shutil.copyfileobj=lambda *a,**k: os._exit(73)
runtime.extract_archive(pathlib.Path(sys.argv[2]),pathlib.Path(sys.argv[3]))
''')
        result = subprocess.run([sys.executable, str(probe), str(BIN / "customer-install.py"),
                                 str(self.archive), str(destination)])
        self.assertEqual(result.returncode, 73)
        self.assertFalse(destination.exists())
        self.runtime.extract_archive(self.archive, destination)
        self.assertEqual((destination / "qa-r1/SHA256SUMS").read_bytes(), self.files["SHA256SUMS"])


if __name__ == "__main__":
    unittest.main()
