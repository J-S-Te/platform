"""仅临时目录、合成Docker与重载测试；不读取真实配置或连接业务环境。"""
import importlib.util
import json
import fcntl
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

HELPER = Path(__file__).resolve().parents[1] / "deploy/production/bin/prepare-reimbursement-infrastructure.py"
spec = importlib.util.spec_from_file_location("reimbursement_infrastructure", HELPER)
infra = importlib.util.module_from_spec(spec)
spec.loader.exec_module(infra)


class InfrastructureTests(unittest.TestCase):
    # 用途：准备只含合成内容的私有临时部署树及继承描述符9。
    # 参数：无。
    # 返回值：无。
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name).resolve()
        self.root.chmod(0o700)
        (self.root / "runtime").mkdir(mode=0o700)
        (self.root / "bin").mkdir(mode=0o700)
        for name, raw in {".env": b"# synthetic\nCOMPOSE_PROJECT_NAME=fixture\nUNCHANGED=yes\n",
                          ".release.env": b"REIMBURSEMENT_IMAGE=fixture@sha256:" + b"a" * 64 + b"\n",
                          "docker-compose.yml": b"services: {}\n",
                          "bin/provisioner-config-refresh.sh": b"# fixture only\n"}.items():
            path = self.root / name
            path.write_bytes(raw)
            path.chmod(0o600)
        self.original = (self.root / ".env").read_bytes()
        self.old_fd9 = None
        try:
            self.old_fd9 = os.dup(9)
        except OSError:
            pass
        fd = os.open(self.root / "runtime/.deploy.lock", os.O_CREAT | os.O_RDWR, 0o600)
        if fd != 9:
            os.dup2(fd, 9)
            os.close(fd)
        self.calls = []
        self.container = b""
        self.volumes = b""
        self.reloads = []

    # 用途：关闭本轮描述符并删除本轮临时树，保留原进程已有描述符。
    # 参数：无。
    # 返回值：无。
    def tearDown(self):
        os.close(9)
        if self.old_fd9 is not None:
            os.dup2(self.old_fd9, 9)
            os.close(self.old_fd9)
        self.temp.cleanup()

    # 用途：模拟受控Docker元数据与Compose验证，不调用真实Docker。
    # 参数：command为实际构造命令，kwargs为超时等执行选项。
    # 返回值：预先声明的合成字节。
    def docker(self, command, **kwargs):
        self.calls.append(command)
        if "volume" in command:
            return self.volumes
        if "ps" in command:
            return self.container
        if "--format" in command and "json" in command:
            return json.dumps({"services": {"reimbursement-mysql": {"volumes": [
                {"type": "volume", "source": "db", "target": "/var/lib/mysql"}]}},
                "volumes": {"db": {"name": "fixture_exact_database_volume"}}}).encode()
        if "--quiet" in command:
            return b""
        raise AssertionError("unexpected fixture command")

    # 用途：以合成Linux持锁事实执行真实文件事务。
    # 参数：reload为可选的本轮合成重载行为。
    # 返回值：初始化回执。
    def prepare(self, reload=None):
        info = f"lock: 1: FLOCK ADVISORY WRITE {os.getpid()} {os.major(os.fstat(9).st_dev):02x}:{os.minor(os.fstat(9).st_dev):02x}:{os.fstat(9).st_ino} 0 EOF\n"
        with patch.object(infra, "lock_description", return_value=info), \
                patch.object(infra, "run_private", side_effect=self.docker), \
                patch.object(infra, "reload_control_plane", side_effect=reload or (lambda *args: self.reloads.append(1))):
            return infra.prepare(self.root, 9)

    # 用途：验证两键新建只追加配置、完整备份及一次重载，并验证幂等零写。
    # 参数：无。
    # 返回值：无。
    def test_initialize_and_idempotence(self):
        result = self.prepare()
        self.assertEqual(result["status"], "PREPARED")
        current = (self.root / ".env").read_bytes()
        self.assertTrue(current.startswith(self.original))
        values = infra.target_values(current)
        self.assertEqual(set(map(len, values.values())), {64})
        self.assertNotEqual(*values.values())
        backup = self.root / result["backup"]
        self.assertEqual((backup / ".env").read_bytes(), self.original)
        self.assertEqual((backup / ".env").stat().st_mode & 0o777, 0o600)
        self.assertEqual(self.reloads, [1])
        self.assertFalse((self.root / "runtime/.control-plane-reload-required").exists())
        before = (self.root / ".env").stat()
        self.assertEqual(self.prepare()["status"], "UNCHANGED")
        self.assertEqual((self.root / ".env").stat().st_ino, before.st_ino)
        self.assertEqual(self.reloads, [1])

    # 用途：拒绝占位、部分、重复或空键，不轮换已有口令。
    # 参数：无。
    # 返回值：无。
    def test_bad_key_states_are_rejected(self):
        for raw in [b"REIMBURSEMENT_MYSQL_PASSWORD=\n", b'REIMBURSEMENT_MYSQL_PASSWORD="PENDING_x" # comment\n',
                    b"REIMBURSEMENT_MYSQL_PASSWORD: existing\n", b"REIMBURSEMENT_MYSQL_PASSWORD=PENDING_x\n",
                    b"REIMBURSEMENT_MYSQL_PASSWORD=valid\n", b"REIMBURSEMENT_MYSQL_PASSWORD=a\nREIMBURSEMENT_MYSQL_PASSWORD=b\n",
                    b"REIMBURSEMENT_MYSQL_PASSWORD=valid\nREIMBURSEMENT_MYSQL_ROOT_PASSWORD=REPLACE_WITH_x\n"]:
            (self.root / ".env").write_bytes(self.original + raw)
            with self.assertRaises(infra.InfrastructureError):
                self.prepare()
            self.assertEqual((self.root / ".env").read_bytes(), self.original + raw)

    # 用途：既有停止容器或实际数据库卷均阻断首次生成。
    # 参数：无。
    # 返回值：无。
    def test_existing_database_is_rejected(self):
        for container, volumes in [(b"stopped-container\n", b""), (b"", b"fixture_exact_database_volume\n")]:
            self.container, self.volumes = container, volumes
            with self.assertRaises(infra.InfrastructureError):
                self.prepare()
            self.assertEqual((self.root / ".env").read_bytes(), self.original)

    # 用途：拒绝未持锁事实、符号链接和未完成事务。
    # 参数：无。
    # 返回值：无。
    def test_lock_and_marker_fail_closed(self):
        with patch.object(infra, "lock_description", return_value="pos: 0\n"):
            with self.assertRaises(infra.InfrastructureError):
                infra.prepare(self.root, 9)
        marker = self.root / "runtime/.runtime-approval-publish"
        marker.write_text("fixture")
        marker.chmod(0o600)
        with self.assertRaises(infra.InfrastructureError):
            self.prepare()
        marker.unlink()
        (self.root / ".env").chmod(0o644)
        with self.assertRaises(infra.InfrastructureError):
            self.prepare()

    # 用途：重载失败必须恢复原配置并重载旧视图。
    # 参数：无。
    # 返回值：无。
    def test_reload_failure_restores_original(self):
        original_reload = infra.reload_control_plane
        calls = []
        # 用途：模拟第一次重载失败及恢复旧视图成功。
        # 参数：root为本轮临时部署根。
        # 返回值：无。
        def reload(root):
            calls.append(1)
            if len(calls) == 1:
                raise infra.InfrastructureError("fixture reload failure")
        self.addCleanup(setattr, infra, "reload_control_plane", original_reload)
        info = f"lock: 1: FLOCK ADVISORY WRITE {os.getpid()} {os.major(os.fstat(9).st_dev):02x}:{os.minor(os.fstat(9).st_dev):02x}:{os.fstat(9).st_ino} 0 EOF\n"
        with patch.object(infra, "lock_description", return_value=info), patch.object(infra, "run_private", side_effect=self.docker), patch.object(infra, "reload_control_plane", side_effect=reload):
            with self.assertRaises(infra.InfrastructureError):
                infra.prepare(self.root, 9)
        self.assertEqual((self.root / ".env").read_bytes(), self.original)
        self.assertEqual(calls, [1, 1])
        self.assertFalse((self.root / "runtime/.control-plane-reload-required").exists())

    # 用途：备份失败或备份后原配置漂移均不发布候选。
    # 参数：无。
    # 返回值：无。
    def test_backup_failure_and_cas(self):
        with patch.object(infra, "backup_environment", side_effect=infra.InfrastructureError("fixture backup")):
            with self.assertRaises(infra.InfrastructureError):
                self.prepare()
        self.assertEqual((self.root / ".env").read_bytes(), self.original)
        backup = infra.backup_environment
        external = self.original + b"EXTERNAL=preserved\n"
        # 用途：模拟不遵守共享锁的外部写入，验证CAS拒绝覆盖。
        # 参数：root为临时根，original为锁内原内容。
        # 返回值：实际临时备份目录。
        def changed_after_backup(root, original):
            result = backup(root, original)
            (root / ".env").write_bytes(external)
            return result
        with patch.object(infra, "backup_environment", side_effect=changed_after_backup):
            with self.assertRaises(infra.InfrastructureError):
                self.prepare()
        self.assertEqual((self.root / ".env").read_bytes(), external)
        self.assertFalse((self.root / "runtime/.control-plane-reload-required").exists())

    # 用途：候选Compose失败只保留备份，不改当前配置或启动重载。
    # 参数：无。
    # 返回值：无。
    def test_candidate_validation_fails_before_write(self):
        with patch.object(infra, "validate_candidate", side_effect=infra.InfrastructureError("fixture invalid compose")):
            with self.assertRaises(infra.InfrastructureError):
                self.prepare()
        self.assertEqual((self.root / ".env").read_bytes(), self.original)
        self.assertEqual(self.reloads, [])
        self.assertFalse((self.root / "runtime/.control-plane-reload-required").exists())

    # 用途：正向重载后的外部内容漂移不得被恢复覆盖。
    # 参数：无。
    # 返回值：无。
    def test_success_reload_drift_preserves_external(self):
        external = b"EXTERNAL_DURING_RELOAD=preserved\n"
        # 用途：模拟重载窗口中不遵守共享锁的写入。
        # 参数：root为临时根。
        # 返回值：无。
        def drift(root):
            (root / ".env").write_bytes(external)
        with self.assertRaises(infra.InfrastructureError):
            self.prepare(reload=drift)
        self.assertEqual((self.root / ".env").read_bytes(), external)
        self.assertTrue((self.root / "runtime/.control-plane-reload-required").exists())

    # 用途：恢复旧视图重载后再次验证配置，漂移时保留恢复门禁。
    # 参数：无。
    # 返回值：无。
    def test_recovery_reload_drift_keeps_marker(self):
        calls = []
        external = b"EXTERNAL_DURING_RECOVERY=preserved\n"
        # 用途：先失败再模拟恢复重载窗口的外部变更。
        # 参数：root为临时根。
        # 返回值：无。
        def drift(root):
            calls.append(1)
            if len(calls) == 1:
                raise infra.InfrastructureError("fixture initial reload")
            (root / ".env").write_bytes(external)
        with self.assertRaises(infra.InfrastructureError):
            self.prepare(reload=drift)
        self.assertEqual((self.root / ".env").read_bytes(), external)
        self.assertEqual(calls, [1, 1])
        self.assertTrue((self.root / "runtime/.control-plane-reload-required").exists())

    # 用途：同内容不同inode及marker漂移仍视为外部改动，不移除恢复门禁。
    # 参数：无。
    # 返回值：无。
    def test_same_content_inode_and_marker_drift(self):
        # 用途：以同字节文件替换候选inode，验证身份CAS。
        # 参数：root为临时根。
        # 返回值：无。
        def drift(root):
            original = (root / ".env").read_bytes()
            replacement = root / "replacement"
            replacement.write_bytes(original)
            replacement.chmod(0o600)
            os.replace(replacement, root / ".env")
        with self.assertRaises(infra.InfrastructureError):
            self.prepare(reload=drift)
        self.assertTrue((self.root / "runtime/.control-plane-reload-required").exists())

    # 用途：Docker元数据查询失败不得写入候选或生成业务容器。
    # 参数：无。
    # 返回值：无。
    def test_docker_failure_and_symlink(self):
        with patch.object(self, "docker", side_effect=infra.InfrastructureError("fixture docker failure")):
            with self.assertRaises(infra.InfrastructureError):
                self.prepare()
        self.assertEqual((self.root / ".env").read_bytes(), self.original)
        path = self.root / ".env"
        path.unlink()
        path.symlink_to(self.root / ".release.env")
        with self.assertRaises(infra.InfrastructureError):
            self.prepare()

    # 用途：在Linux原生CI验证未持锁FD拒绝、真实flock继承事实接受。
    # 参数：无。
    # 返回值：无；本机没有Linux fdinfo时明确跳过。
    @unittest.skipUnless(Path("/proc/self/fdinfo").is_dir(), "requires Linux fdinfo; production CI verifies actual FLOCK")
    def test_linux_actual_lock_facts(self):
        with self.assertRaises(infra.InfrastructureError):
            infra.require_lock(self.root, 9)
        fcntl.flock(9, fcntl.LOCK_EX | fcntl.LOCK_NB)
        try:
            infra.require_lock(self.root, 9)
        finally:
            fcntl.flock(9, fcntl.LOCK_UN)


if __name__ == "__main__":
    unittest.main()
