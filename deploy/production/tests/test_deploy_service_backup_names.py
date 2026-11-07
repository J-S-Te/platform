#!/usr/bin/env python3
"""发布脚本备份命名的静态守卫。

背景（2026-11-07 测试服务器真实事故）：`deploy_data_analysis()` 曾用 `local ... service`
遮蔽全局 `$service`，导致 `backup_database_if_initialized` 的默认 backup_name 为空，
产出 `-<release>.sql.gz` 备份名；`sha256sum` 把以 `-` 开头的文件名当成选项，报
`invalid option -- '2'`，备份摘要校验失败并阻断整次发布。

守卫规则：deploy-service.sh 里每个 backup_database_if_initialized 调用必须显式给出
第三个参数（备份名），不得依赖全局 $service 的隐式默认；这样任何遮蔽都不会再把
备份名变空。
"""

from __future__ import annotations

import re
import unittest
from pathlib import Path

SCRIPT = Path(__file__).resolve().parents[1] / "bin" / "deploy-service.sh"
CALL = re.compile(r"backup_database_if_initialized\s+(\S+)\s+(\S+)(\s+\S+)?\s*(?:\|\||$)")


class BackupNameGuardTest(unittest.TestCase):
    def test_every_backup_call_names_the_backup_explicitly(self) -> None:
        text = SCRIPT.read_text(encoding="utf-8")
        missing = []
        for line_number, line in enumerate(text.splitlines(), start=1):
            match = CALL.search(line)
            if not match or match.group(3) is None:
                if match:
                    missing.append((line_number, line.strip()))
        self.assertEqual(
            missing,
            [],
            "backup_database_if_initialized 必须显式传第三个参数（备份名），"
            f"否则空 backup_name 会生成以 '-' 开头的文件名并让 sha256sum 失败: {missing}",
        )

    def test_backup_name_default_is_documented_as_defensive_only(self) -> None:
        text = SCRIPT.read_text(encoding="utf-8")
        self.assertIn("backup_name=\"${3:-$service}\"", text)
        # 默认值仅作防御；所有调用点都必须显式传参（上一用例已强制）。
        self.assertIn("local mysql_service=\"$1\" database=\"$2\" backup_name=\"${3:-$service}\"", text)


if __name__ == "__main__":
    unittest.main()
