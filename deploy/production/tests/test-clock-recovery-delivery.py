#!/usr/bin/env python3
"""离线时钟恢复工具的定向构建、归档、安装及输入指纹回归。"""
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import shutil
import struct
import subprocess
import sys
import tarfile
import tempfile
import unittest


PRODUCTION = Path(__file__).resolve().parents[1]
INPUTS = PRODUCTION / "bin/offline-build-inputs.sh"


class ClockRecoveryDeliveryTests(unittest.TestCase):
    # 用途：为每个用例创建独立临时工作目录。
    # 参数：无。
    # 返回值：无；目录不可用时使测试失败。
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="clock-recovery-delivery-")
        self.root = Path(self.temporary.name).resolve()

    # 用途：移除当前用例自行创建的临时夹具。
    # 参数：无。
    # 返回值：无。
    def tearDown(self):
        self.temporary.cleanup()

    # 用途：写入仅隔离外部运行能力的命令替身。
    # 参数：name 为命令名，text 为脚本内容。
    # 返回值：生成的可执行文件路径。
    def write_tool(self, name, text):
        tools = self.root / "tools"
        tools.mkdir(exist_ok=True)
        path = tools / name
        path.write_text("#!/bin/sh\n" + text, encoding="utf-8")
        path.chmod(0o755)
        return path

    # 用途：以现有交付夹具冻结当前公开源码和本次部署资产，避免活动前端输入干扰。
    # 参数：无。
    # 返回值：已核对源、副本摘要一致的独立交付布局。
    def freeze_delivery_inputs(self):
        spec = importlib.util.spec_from_file_location(
            "clock_recovery_frozen_layout", PRODUCTION / "tests/test-offline-delivery-contract.py")
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        fixture = module.DeliveryContract()
        fixture.controlled_delivery_inputs = True
        fixture.setUp()
        self.addCleanup(fixture.tearDown)
        self.assertTrue(fixture.copied_input_hashes)
        return fixture

    # 用途：验证真实资产构建器交付可执行静态工具且安装不调用 Go 或网络。
    # 参数：无。
    # 返回值：无；工具漏打、动态链接或安装权限错误时失败。
    def test_assets_install_static_recovery_tool_without_host_go(self):
        frozen = self.freeze_delivery_inputs()
        production = frozen.production
        self.write_tool("docker", '[ "$1 $2" = "buildx version" ]\n')
        env = os.environ | {
            "PATH": str(self.root / "tools") + os.pathsep + os.environ["PATH"],
            "OFFLINE_GOPROXY": "off",
            "OFFLINE_GOSUMDB": "sum.golang.org",
        }
        output = self.root / "output"
        result = subprocess.run(
            ["bash", str(production / "bin/build-offline-packages.sh"),
             "--component", "assets", "--version", "clock-recovery-test",
             "--output", str(output)], env=env, capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        archive = output / "deployment-assets-clock-recovery-test.tar.gz"
        with tarfile.open(archive) as bundle:
            members = {item.name.removeprefix("./"): item for item in bundle.getmembers()}
            self.assertTrue("bin/clock-recovery" in members, "资产包没有交付时钟恢复工具")
            self.assertIn("CLOCK_RECOVERY.md", members)
            raw = bundle.extractfile(members["bin/clock-recovery"]).read()
            self.assertEqual(raw[:6], b"\x7fELF\x02\x01")
            self.assertEqual(struct.unpack_from("<H", raw, 18)[0], 62)
            offset = struct.unpack_from("<Q", raw, 32)[0]
            width, count = struct.unpack_from("<HH", raw, 54)
            program_types = {struct.unpack_from("<I", raw, offset + width * i)[0]
                             for i in range(count)}
            self.assertTrue(program_types.isdisjoint({2, 3}), "工具仍依赖动态装载器")
            self.assertFalse(any(name.endswith((".pem", ".key", "recovery.jws"))
                                 or "/clock-state" in name for name in members))
        checksum = subprocess.run(
            ["sha256sum", "--check", "SHA256SUMS"], cwd=output,
            capture_output=True, text=True)
        self.assertEqual(checksum.returncode, 0, checksum.stdout + checksum.stderr)
        spec = importlib.util.spec_from_file_location(
            "clock_recovery_delivery_inspector", production / "bin/delivery-inspect.py")
        inspector = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(inspector)
        with archive.open("rb") as stream:
            contents = inspector.asset_contents(stream)
        self.assertIn("CLOCK_RECOVERY.md", contents)

        attempts = self.root / "unexpected-host-tools"
        for name in ("go", "curl", "wget"):
            self.write_tool(name, 'printf "%s\\n" "$0" >> "$HOST_TOOL_ATTEMPTS"\nexit 99\n')
        if shutil.which("flock") is None:
            self.write_tool("flock", "exit 0\n")
        target = self.root / "installed"
        installed = subprocess.run(
            ["bash", str(output / "install-assets.sh"), str(archive), str(target)],
            env=env | {"HOST_TOOL_ATTEMPTS": str(attempts)}, capture_output=True, text=True)
        self.assertEqual(installed.returncode, 0, installed.stdout + installed.stderr)
        binary = target / "bin/clock-recovery"
        self.assertTrue(binary.stat().st_mode & 0o111, "安装后的工具没有执行权限")
        self.assertEqual(hashlib.sha256(binary.read_bytes()).digest(), hashlib.sha256(raw).digest())
        self.assertTrue((target / "CLOCK_RECOVERY.md").is_file())
        self.assertFalse(attempts.exists(), "目标机安装调用了 Go 或网络下载命令")
        proof = subprocess.run(
            [sys.executable, str(production / "bin/delivery-files.py"), "assets",
             "--source", str(archive), "--target", str(target)],
            capture_output=True, text=True)
        self.assertEqual(proof.returncode, 0, proof.stdout + proof.stderr)
        self.record_build_evidence(output, archive, raw, frozen.copied_input_hashes)

    # 用途：保留真实交付构建的输入、归档和二进制摘要，便于主任务复核。
    # 参数：output 为输出目录，archive 为资产包，raw 为工具字节，copied_inputs 为公开源摘要。
    # 返回值：无；可选证据路径不安全或写入失败时使测试失败。
    def record_build_evidence(self, output, archive, raw, copied_inputs):
        info = dict(line.split("=", 1) for line in
                    (output / "BUILD_INFO.txt").read_text().splitlines())
        copied_manifest = json.dumps(copied_inputs, sort_keys=True, ensure_ascii=False)
        evidence = {
            "source_input_sha256": info["SOURCE_INPUT_SHA256"],
            "clock_recovery_sha256": hashlib.sha256(raw).hexdigest(),
            "asset_archive_sha256": hashlib.sha256(archive.read_bytes()).hexdigest(),
            "go_version": info["CLOCK_RECOVERY_GO_VERSION"],
            "target_platform": info["TARGET_PLATFORM"],
            "copied_public_input_sha256": hashlib.sha256(copied_manifest.encode()).hexdigest(),
            "copied_public_input_count": len(copied_inputs),
            "environment": "frozen-public-source-fixture-with-docker-metadata-stub",
        }
        serialized = json.dumps(evidence, ensure_ascii=False, sort_keys=True)
        print("CLOCK_RECOVERY_DELIVERY_EVIDENCE=" + serialized)
        configured = os.environ.get("CLOCK_RECOVERY_DELIVERY_EVIDENCE", "")
        if configured:
            path = Path(configured)
            self.assertTrue(path.is_absolute() and not path.is_symlink())
            path.write_text(serialized + "\n", encoding="utf-8")
            path.with_suffix(".inputs.json").write_text(copied_manifest + "\n", encoding="utf-8")

    # 用途：验证缺少受控构建源码时明确失败，不生成占位工具。
    # 参数：无。
    # 返回值：无；缺少源码却成功或输出工具时失败。
    def test_missing_controlled_sources_fail_without_output(self):
        helper = self.root / "workspace/platform/deploy/production/bin/build-clock-recovery.sh"
        helper.parent.mkdir(parents=True)
        shutil.copyfile(PRODUCTION / "bin/build-clock-recovery.sh", helper)
        output = self.root / "clock-recovery"
        result = subprocess.run(["bash", str(helper), str(output)],
                                capture_output=True, text=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("clock-recovery 构建源码缺失", result.stderr)
        self.assertFalse(output.exists())

    # 用途：阻止未纳入输入指纹的链接源码进入恢复工具。
    # 参数：无。
    # 返回值：无；链接源码被接受或生成工具时失败。
    def test_symlinked_source_is_rejected_before_build(self):
        workspace = self.root / "workspace"
        helper = workspace / "platform/deploy/production/bin/build-clock-recovery.sh"
        helper.parent.mkdir(parents=True)
        shutil.copyfile(PRODUCTION / "bin/build-clock-recovery.sh", helper)
        for relative in ("commercial-license-tool/go.mod",
                         "commercial-license-tool/cmd/clock-recovery/main.go",
                         "commercial-license-tool/internal/clockrecovery/recovery.go",
                         "license-core/go.mod", "license-core/license.go",
                         "license-core/runtime/state.go"):
            source = workspace / relative
            source.parent.mkdir(parents=True, exist_ok=True)
            source.write_text("reviewed-source\n", encoding="utf-8")
        external = self.root / "unbound.go"
        external.write_text("unbound-source\n", encoding="utf-8")
        (workspace / "license-core/extra.go").symlink_to(external)
        output = self.root / "clock-recovery"
        result = subprocess.run(["bash", str(helper), str(output)],
                                capture_output=True, text=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("未纳入指纹的符号链接", result.stderr)
        self.assertFalse(output.exists())

    # 用途：计算受控稀疏夹具的真实构建输入指纹。
    # 参数：workspace 为临时工作区，mode 为 shared 或 full。
    # 返回值：输入指纹；枚举失败时使测试失败。
    def fingerprint(self, workspace, mode):
        result = subprocess.run(
            ["bash", "-Eeuo", "pipefail", "-c",
             'source "$1"; offline_build_input_fingerprint "$2" "$3"',
             "clock-recovery-inputs", str(INPUTS), str(workspace), mode],
            capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        return result.stdout.strip()

    # 用途：阻止恢复工具或共享依赖改变后继续复用旧发布输入证明。
    # 参数：无。
    # 返回值：无；shared/full 任一范围漏掉真实源码时失败。
    def test_shared_and_full_fingerprints_bind_recovery_sources_and_dependency(self):
        workspace = self.root / "workspace"
        for component in ("platform", "customer_and_opportunity", "contract_management",
                          "project_management", "Settlement", "data_analysis"):
            for directory in ("cmd", "internal", "migrations", "authz"):
                (workspace / component / directory).mkdir(parents=True)
        for directory in ("frontend/src", "frontend/public", "frontend/nginx",
                          "platform/deploy/production/bin", "platform/deploy/production/monitoring",
                          "platform/deploy/production/mysql-init", "platform/deploy/production/nginx",
                          "platform/deploy/production/subsystem-templates",
                          "platform/deploy/production/subsystems.d"):
            (workspace / directory).mkdir(parents=True)
        sources = ("commercial-license-tool/cmd/clock-recovery/main.go",
                   "commercial-license-tool/internal/clockrecovery/recovery.go",
                   "commercial-license-tool/go.mod", "commercial-license-tool/go.sum",
                   "license-core/go.mod", "license-core/go.sum", "license-core/runtime/state.go",
                   "platform/deploy/production/CLOCK_RECOVERY.md")
        for name in sources:
            path = workspace / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text("reviewed-source\n", encoding="utf-8")
        for mode in ("shared", "full"):
            baseline = self.fingerprint(workspace, mode)
            for name in sources:
                with self.subTest(mode=mode, source=name):
                    path = workspace / name
                    path.write_text("changed-source\n", encoding="utf-8")
                    self.assertNotEqual(baseline, self.fingerprint(workspace, mode), name)
                    path.write_text("reviewed-source\n", encoding="utf-8")
            (workspace / "commercial-license-tool/vendor-private.pem").write_text("excluded-fixture\n")
            (workspace / "license-core/runtime/clock-state.json").write_text("excluded-fixture\n")
            self.assertEqual(baseline, self.fingerprint(workspace, mode),
                             "私钥或状态夹具进入了源码输入证明")


if __name__ == "__main__":
    unittest.main()
