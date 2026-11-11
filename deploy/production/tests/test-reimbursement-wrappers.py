#!/usr/bin/env python3
"""只执行根包装器的纯选择/映射/基线校验函数，不打包镜像或部署服务。"""
import io
import importlib.util
import json
from pathlib import Path
import subprocess
import tarfile
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[4]
BUILDER = ROOT / "一键制作离线镜像包.sh"
INSTALLER = ROOT / "服务器一键离线部署.sh"


# 用途：从实际包装器抽取独立纯函数，避免执行其主打包或安装流程。
# 参数：path为真实源码文件，name为固定测试函数名。
# 返回值：完整函数源码；声明不存在时断言失败。
def shell_function(path, name):
    source = path.read_text()
    start = source.index(name + "() {\n")
    end = source.index("\n}\n", start) + 3
    return source[start:end]


class ReimbursementWrappers(unittest.TestCase):
    # 用途：在临时shell中运行抽取的无部署函数并捕获结果。
    # 参数：functions为实际函数源码，command为固定测试命令。
    # 返回值：进程回执；不调用Docker、sudo或任何包装器主程序。
    def run_shell(self, functions, command):
        body = "set -e\ndie() { printf '%s\\n' \"$*\" >&2; exit 7; }\ninfo() { :; }\n"
        return subprocess.run(["bash", "-c", body + functions + "\n" + command],
                              capture_output=True, text=True, timeout=10)

    # 用途：核验完整、仅平台及混合显式选择的报销归一语义。
    # 参数：无。
    # 返回值：无；漏选、重复或混用all/none被接受时失败。
    def test_builder_all_none_and_explicit_selection(self):
        functions = shell_function(BUILDER, "normalize_components")
        line = next(x for x in BUILDER.read_text().splitlines() if x.startswith("optional_components="))
        prefix = line + "\nportal_directory_mode=crm\n"
        for raw, expected in (("none", ""), ("reimbursement,reimbursement,project", "project reimbursement")):
            result = self.run_shell(functions, prefix + f"normalize_components '{raw}'")
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(result.stdout.strip(), expected)
        result = self.run_shell(functions, prefix + "normalize_components all")
        self.assertIn("reimbursement", result.stdout.split())
        for raw in ("all reimbursement", "none reimbursement", "reimbursement-unknown"):
            self.assertNotEqual(self.run_shell(functions, prefix + f"normalize_components '{raw}'").returncode, 0)

    # 用途：核验服务器入口的包名、应用码与升级标记来自真实映射。
    # 参数：无。
    # 返回值：无；报销识别成unknown或其他系统时失败。
    def test_installer_package_application_and_upgrade_mapping(self):
        for name, argument, expected in (
            ("package_component_of", "reimbursement-backend-qa-linux-amd64.tar.gz", "reimbursement"),
            ("package_pattern_of", "reimbursement", "reimbursement-backend-*-linux-amd64.tar.gz"),
            ("subsystem_application_of", "reimbursement", "reimbursement"),
            ("upgrade_subsystem_block_key", "reimbursement", "reimbursement"),
        ):
            result = self.run_shell(shell_function(INSTALLER, name), f"{name} '{argument}'")
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(result.stdout.strip(), expected)

    # 用途：通过根公开只读入口核验报销介质及包名与manifest不符时的拒绝。
    # 参数：无；OCI内容、摘要和目标均为临时夹具。
    # 返回值：无；不会执行实际构建、镜像导入或部署。
    def test_public_readonly_entry_accepts_reimbursement_and_rejects_forged_component(self):
        path = Path(__file__).with_name("test-delivery-inspect.py")
        spec = importlib.util.spec_from_file_location("reimbursement_media_fixture", path)
        fixture = importlib.util.module_from_spec(spec); spec.loader.exec_module(fixture)
        fixture.COMPONENT_PREFIXES["reimbursement"] = "reimbursement-backend"
        fixture.PROFILES["reimbursement"] = "reimbursement-prod.yaml"
        holder = fixture.DeliveryTests(); holder.setUp(); self.addCleanup(holder.doCleanups)
        old = holder.names.pop("contract")
        (holder.media / old).unlink(); (holder.media / (old + ".sha256")).unlink()
        holder.facts.pop("contract")
        holder.add_package("reimbursement"); holder.finish()
        command = ["bash", str(INSTALLER), "--media", str(holder.media), "--check-delivery", "--format", "json"]
        result = subprocess.run(command, capture_output=True, text=True, timeout=30)
        self.assertEqual(result.returncode, 0, result.stderr)
        report = json.loads(result.stdout)
        self.assertEqual(report["applications"], ["reimbursement"])
        self.assertEqual(report["consumer_count"], 2)
        self.assertEqual(report["status"], "NEEDS_INPUT")
        package = holder.media / holder.names["reimbursement"]
        entries = []
        with tarfile.open(package, "r:gz") as source:
            for member in source.getmembers():
                raw = source.extractfile(member).read()
                if member.name == "package.env":
                    raw = raw.replace(b"COMPONENT=reimbursement", b"COMPONENT=contract")
                entries.append((member.name, raw))
        package.write_bytes(fixture.tar_bytes(entries, compressed=True)); holder.finish()
        result = subprocess.run(command, capture_output=True, text=True, timeout=30)
        self.assertEqual(result.returncode, 2, result.stdout + result.stderr)
        self.assertEqual(json.loads(result.stdout)["status"], "INVALID")

    # 用途：验证新增报销增量交付不能复用缺少报销部署能力的旧资产。
    # 参数：无。
    # 返回值：无；旧资产放行或审核新资产被拒绝时失败。
    def test_extension_requires_reimbursement_assets(self):
        functions = shell_function(BUILDER, "validate_extension_asset_compatibility")
        with tempfile.TemporaryDirectory(prefix="reimbursement-wrapper-") as directory:
            archive = Path(directory) / "assets.tar.gz"
            with tarfile.open(archive, "w:gz") as target:
                data = b"version: 1\n"
                entry = tarfile.TarInfo("unrelated.yaml"); entry.size = len(data)
                target.addfile(entry, io.BytesIO(data))
            command = f"selected_optional_components=reimbursement\nvalidate_extension_asset_compatibility '{archive}'"
            self.assertNotEqual(self.run_shell(functions, command).returncode, 0)
            with tarfile.open(archive, "w:gz") as target:
                for name in ("subsystems.d/reimbursement-prod.yaml", "subsystem-templates/reimbursement.env.example"):
                    path = ROOT / "platform/deploy/production" / name
                    data = path.read_bytes(); entry = tarfile.TarInfo(name); entry.size = len(data)
                    target.addfile(entry, io.BytesIO(data))
            result = self.run_shell(functions, command)
            self.assertEqual(result.returncode, 0, result.stderr)


if __name__ == "__main__":
    unittest.main()
