#!/usr/bin/env python3
"""许可运行态判读口径门禁（D5）：status 端点是唯一判据，readyz 不得混用。

背景
----
应用级“许可是否生效”只有一个判据：`GET /api/v1/licenses/status`（需
`platform:license:read`），看逐应用 `systems[]` 的 `status`/`enforcement` 与顶层
`runtime_enforcement`。`/licenses/enforcement/:application` 只是诊断入口；单服务
`/readyz` 是未认证探针，回答“进程依赖是否就绪”，不是合规结论，容器 healthcheck 更不是。

历史事故形态：把 `/readyz` 的许可字段或容器 healthy 当成“许可已生效”，在子系统本地
许可键与平台签发集不一致时得出“聚合 VALID 就一切正常”的错误结论。这条口径靠人自觉
守不住，因此钉成静态门禁：

1. README「许可运行态判读口径」章节必须存在，且以 `/api/v1/licenses/status` 为唯一判据；
2. 章节必须写明 `platform:license:read`、`runtime_enforcement`、诊断入口与
   `runtime/license-`（已知盲点核对路径）；
3. 章节里每一处提到 `readyz` 的行都必须带否定/禁止措辞（禁止、不得、不代表、不作为……），
   不允许出现“readyz 正常/降级 ⇒ 许可 VALID/失效”这类并列判读。

只用标准库逐行解析，默认读本仓库的 `deploy/production`；负例验证时可设置
`DEPLOY_PRODUCTION_ROOT` 指向 /tmp 副本（CI 不设置该变量）。
"""
from __future__ import annotations

import os
import re
import unittest
from pathlib import Path


PRODUCTION = Path(
    os.environ.get("DEPLOY_PRODUCTION_ROOT", Path(__file__).resolve().parents[1])
).resolve()
README = PRODUCTION / "README.md"

SECTION = "许可运行态判读口径"
STATUS_ENDPOINT = "/api/v1/licenses/status"
PERMISSION = "platform:license:read"
DIAGNOSTIC_ENDPOINT = "/licenses/enforcement/"
# 提到 readyz 的行必须命中其中之一，证明“readyz 不是判据”写在同一条陈述里。
NEGATION_MARKERS = (
    "禁止",
    "不得",
    "不能",
    "不要",
    "不是",
    "不代表",
    "不作为",
    "不作",
    "非判据",
)


def read(path: Path) -> str:
    if not path.is_file():
        raise AssertionError("缺少部署资产：%s" % path)
    return path.read_text(encoding="utf-8")


def section_lines(text: str, prefix: str):
    """按标题前缀取章节正文；遇到同级或更高级标题截断。"""
    lines = text.splitlines()
    start = None
    for index, line in enumerate(lines):
        stripped = line.strip()
        if not stripped.startswith("#"):
            continue
        heading = re.sub(r"^\d+[.、]\s*", "", stripped.lstrip("#").strip())
        if heading.startswith(prefix):
            start = index
            break
    if start is None:
        raise AssertionError("README 缺少章节「%s」（本门禁依赖该章节，不得删改标题）" % prefix)
    level = len(lines[start].strip()) - len(lines[start].strip().lstrip("#"))
    body = []
    for line in lines[start + 1:]:
        stripped = line.strip()
        if stripped.startswith("#"):
            current = len(stripped) - len(stripped.lstrip("#"))
            if current <= level:
                break
        body.append(line)
    return body


class LicenseStatusReadingTest(unittest.TestCase):
    """README 的许可判读口径必须唯一、可执行，且不把 readyz 当判据。"""

    @classmethod
    def setUpClass(cls):
        cls.section = section_lines(read(README), SECTION)
        cls.text = "\n".join(cls.section)

    def test_section_declares_status_endpoint_as_sole_verdict(self):
        self.assertIn(
            STATUS_ENDPOINT,
            self.text,
            "README「%s」必须写明唯一判据 %s" % (SECTION, STATUS_ENDPOINT),
        )
        self.assertIn(
            PERMISSION,
            self.text,
            "README「%s」必须写明调用 %s 需要的权限 %s" % (SECTION, STATUS_ENDPOINT, PERMISSION),
        )
        self.assertIn(
            "runtime_enforcement",
            self.text,
            "README「%s」必须写明顶层 runtime_enforcement 的判读方式" % SECTION,
        )

    def test_diagnostic_endpoint_is_not_a_verdict(self):
        self.assertIn(
            DIAGNOSTIC_ENDPOINT,
            self.text,
            "README「%s」必须提到诊断入口 %s 并说明它不是判据" % (SECTION, DIAGNOSTIC_ENDPOINT),
        )
        lines = [line for line in self.section if DIAGNOSTIC_ENDPOINT in line]
        self.assertTrue(
            any("诊断" in line for line in lines),
            "README 提到 %s 时必须写明它只用于诊断，不得当作许可判据：%s"
            % (DIAGNOSTIC_ENDPOINT, lines),
        )

    def test_known_blind_spot_is_registered(self):
        self.assertIn(
            "runtime/license-",
            self.text,
            "README「%s」必须登记已知盲点的核对路径（runtime/license-*.env 与子系统日志）" % SECTION,
        )

    def test_readyz_is_never_used_as_license_verdict(self):
        problems = []
        for line in self.section:
            stripped = line.strip()
            if "readyz" not in stripped.lower():
                continue
            # 标题行不参与判读（本门禁只看正文陈述）。
            if stripped.startswith("#"):
                continue
            if not any(marker in stripped for marker in NEGATION_MARKERS):
                problems.append(stripped)
        self.assertEqual(
            problems,
            [],
            "README「%s」把 readyz 与许可判定并列、且没有否定措辞；"
            "readyz 是未认证探针、回答进程依赖，不是合规结论。命中行：%s" % (SECTION, problems),
        )


if __name__ == "__main__":
    unittest.main()
