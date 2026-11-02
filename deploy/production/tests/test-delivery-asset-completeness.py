#!/usr/bin/env python3
"""部署资产包必须包含运行时脚本：打包门禁 check-asset-bundle.py 的独立回归。"""
import re
import subprocess
import sys
import tempfile
from pathlib import Path
import unittest

PRODUCTION = Path(__file__).resolve().parents[1]
BUILDER = PRODUCTION / "bin/build-offline-packages.sh"
CHECKER = PRODUCTION / "bin/check-asset-bundle.py"
BIN = PRODUCTION / "bin"


# 用途：解析构建器的部署资产白名单。
# 参数：无。
# 返回值：相对部署目录的文件名集合；无法解析时使测试失败。
def asset_files():
    text = BUILDER.read_text(encoding="utf-8")
    match = re.search(r"local -a asset_files=\((.*?)\n  \)", text, re.S)
    if match is None:
        raise AssertionError("无法解析 build-offline-packages.sh 的 asset_files 白名单")
    return set(match.group(1).split())


class DeliveryAssetCompletenessTests(unittest.TestCase):
    # 用途：以白名单为输入执行打包同款检查。
    # 参数：packaged 是本次拟打包的相对路径集合。
    # 返回值：检查器进程结果。
    def run_checker(self, packaged):
        with tempfile.NamedTemporaryFile("w", suffix="-assets.txt", encoding="utf-8") as manifest:
            manifest.write("\n".join(sorted(packaged)) + "\n")
            manifest.flush()
            return subprocess.run(
                [sys.executable, str(CHECKER), "--manifest", manifest.name,
                 "--source-root", str(PRODUCTION), "--source-bin", str(BIN)],
                capture_output=True, text=True)

    # 用途：验证当前白名单内脚本引用的运行时文件都已随包交付。
    # 参数：无。
    # 返回值：无；漏项时由检查器输出引用链并失败。
    def test_packaged_scripts_reference_only_packaged_helpers(self):
        result = self.run_checker(asset_files())
        self.assertEqual(result.returncode, 0, result.stderr)

    # 用途：验证漏打运行时脚本会被打包门禁拒绝（历史缺陷回归）。
    # 参数：无。
    # 返回值：无；逐项断言非零退出且指出缺失文件。
    def test_missing_runtime_helper_is_rejected(self):
        for removed in ("bin/initialize-audit-hash-key.py", "bin/platform-mysql-command.py"):
            with self.subTest(removed=removed):
                result = self.run_checker(asset_files() - {removed})
                self.assertNotEqual(result.returncode, 0, result.stdout)
                self.assertIn(removed.split("/")[-1], result.stderr)


if __name__ == "__main__":
    unittest.main()
