#!/usr/bin/env python3
"""Temporal 共享边界门禁（D4）：共享实例的库、备份、停售保留与文档口径必须对齐。

背景
----
Temporal 与 `contract-mysql` 共用一个 MySQL 命名实例（`MYSQL_SEEDS=contract-mysql`、
`DBNAME=temporal`、消费方 `TEMPORAL_NAMESPACE=default`）。这是登记过的共享例外：不排期
独立实例，但边界必须可验证，否则一次“顺手”的编排调整就会让 Temporal 静默连到别的库、
让备份漏掉在途 workflow，或让合同停售把共享技术底座一起删掉。

本门禁用标准库逐行解析（不引入 pyyaml/ruby，不发请求、不启动容器）：

1. `docker-compose.yml` 的 `temporal` 服务：`MYSQL_SEEDS == contract-mysql`、
   `DBNAME == temporal`，且没有指向别的库的 namespace 覆盖（缺省或 `default`）；
2. `bin/backup-all.sh`：数据库备份清单含 `contract-mysql`，且导出参数是
   `--all-databases`（`temporal`/`temporal_visibility` 与合同库同批次）；
3. `bin/compose-scope.sh`：C11 注释锚点仍在（停售不删除/不改名共享库），
   锚点上下文必须同时提到 `contract-mysql` 与“不随合同停售而删除或改名”；
4. README「Temporal 共享边界」章节存在，并登记 payload / task queue 前缀口径。

负例验证时可设置 `DEPLOY_PRODUCTION_ROOT` 指向 /tmp 副本（CI 不设置该变量）。
"""
from __future__ import annotations

import os
import re
import unittest
from pathlib import Path


PRODUCTION = Path(
    os.environ.get("DEPLOY_PRODUCTION_ROOT", Path(__file__).resolve().parents[1])
).resolve()
COMPOSE = PRODUCTION / "docker-compose.yml"
BACKUP = PRODUCTION / "bin" / "backup-all.sh"
COMPOSE_SCOPE = PRODUCTION / "bin" / "compose-scope.sh"
README = PRODUCTION / "README.md"

TEMPORAL_SERVICE = "temporal"
EXPECTED_MYSQL_SEEDS = "contract-mysql"
EXPECTED_DBNAME = "temporal"
ALLOWED_NAMESPACES = ("", "default")
TEMPORAL_SECTION = "Temporal 共享边界"


def read(path: Path) -> str:
    if not path.is_file():
        raise AssertionError("缺少部署资产：%s" % path)
    return path.read_text(encoding="utf-8")


def service_environment(text: str, service: str):
    """取某个 compose 服务的 environment 键值（只解析 `KEY: value` 的标量行）。"""
    lines = text.splitlines()
    start = None
    for index, line in enumerate(lines):
        if re.match(r"^ {2}%s:\s*(?:#.*)?$" % re.escape(service), line):
            start = index
            break
    if start is None:
        raise AssertionError("docker-compose.yml 缺少服务 `%s`（本门禁依赖共享 Temporal）" % service)

    block = []
    index = start + 1
    while index < len(lines):
        line = lines[index]
        if re.match(r"^ {2}[A-Za-z0-9]", line) and not line.startswith("   "):
            break
        block.append(line)
        index += 1

    environment = {}
    in_environment = False
    for line in block:
        if re.match(r"^ {4}environment:\s*(?:#.*)?$", line):
            in_environment = True
            continue
        if line.strip().startswith("#") or not line.strip():
            continue
        indent = len(line) - len(line.lstrip())
        if in_environment and indent <= 4:
            in_environment = False
        if not in_environment:
            continue
        match = re.match(r"^\s{6}([A-Za-z_][A-Za-z0-9_]*):\s*(.*?)\s*$", line)
        if not match:
            continue
        key, value = match.group(1), match.group(2)
        if value.startswith('"') and value.endswith('"') and len(value) >= 2:
            value = value[1:-1]
        elif value.startswith("'") and value.endswith("'") and len(value) >= 2:
            value = value[1:-1]
        environment[key] = value
    if not environment:
        raise AssertionError("docker-compose.yml 的 `%s` 服务未解析出 environment" % service)
    return environment


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


def backup_database_services(text: str):
    """bin/backup-all.sh 的 `services=(...)` → 数据库服务名列表。"""
    match = re.search(r"services=\(([^)]*)\)", text)
    if not match:
        raise AssertionError("bin/backup-all.sh 未解析出 `services=(...)` 数据库备份清单")
    return [name for name in match.group(1).split() if name]


def comment_block_containing(text: str, needle: str):
    """返回包含 needle 的那一段连续行（用于核对 C11 注释锚点的上下文）。"""
    lines = text.splitlines()
    for index, line in enumerate(lines):
        if needle not in line:
            continue
        start = index
        while start > 0 and lines[start - 1].lstrip().startswith("#"):
            start -= 1
        end = index
        while end + 1 < len(lines) and lines[end + 1].lstrip().startswith("#"):
            end += 1
        return "\n".join(lines[start:end + 1])
    return ""


class TemporalSharedBoundaryTest(unittest.TestCase):
    """共享 Temporal 的库指向、备份覆盖、停售保留与文档口径必须一致。"""

    @classmethod
    def setUpClass(cls):
        cls.compose = read(COMPOSE)
        cls.backup = read(BACKUP)
        cls.compose_scope = read(COMPOSE_SCOPE)
        cls.readme = read(README)
        cls.environment = service_environment(cls.compose, TEMPORAL_SERVICE)

    def test_temporal_uses_contract_mysql_instance(self):
        """temporal 必须连 contract-mysql 实例的 temporal 库。"""
        self.assertEqual(
            self.environment.get("MYSQL_SEEDS"),
            EXPECTED_MYSQL_SEEDS,
            "temporal 的 MYSQL_SEEDS 必须是 contract-mysql（共享技术底座），实际 %r"
            % self.environment.get("MYSQL_SEEDS"),
        )
        self.assertEqual(
            self.environment.get("DBNAME"),
            EXPECTED_DBNAME,
            "temporal 的 DBNAME 必须是 temporal（共享库名，不得按子系统改名），实际 %r"
            % self.environment.get("DBNAME"),
        )

    def test_temporal_namespace_is_default_or_absent(self):
        """temporal 不得注入非 default 的 namespace 覆盖。"""
        overrides = {
            key: value
            for key, value in self.environment.items()
            if key in ("NAMESPACE", "TEMPORAL_NAMESPACE")
            or key.endswith("_NAMESPACE")
        }
        problems = [
            "%s=%r" % (key, value)
            for key, value in sorted(overrides.items())
            if value not in ALLOWED_NAMESPACES
        ]
        self.assertEqual(
            problems,
            [],
            "temporal 服务出现了非 default 的 namespace 覆盖：%s（共享实例只允许缺省/default）"
            % problems,
        )

    def test_backup_covers_contract_mysql_and_all_databases(self):
        """备份清单必须含 contract-mysql，且用 --all-databases 覆盖 temporal 库。"""
        services = backup_database_services(self.backup)
        self.assertIn(
            "contract-mysql",
            services,
            "bin/backup-all.sh 的数据库备份清单缺少 contract-mysql；"
            "Temporal 库驻留在该实例，漏备份会丢掉在途 workflow。实际清单：%s" % services,
        )
        self.assertIn(
            "--all-databases",
            self.backup,
            "bin/backup-all.sh 必须用 --all-databases 导出（temporal/temporal_visibility 与合同库同批次）",
        )

    def test_c11_shared_database_anchor_survives(self):
        """compose-scope.sh 必须保留 C11 注释锚点：停售不删除/不改名共享库。"""
        self.assertIn(
            "C11",
            self.compose_scope,
            "bin/compose-scope.sh 缺少 C11 注释锚点（共享 contract-mysql/Temporal 不随合同停售删除）",
        )
        block = comment_block_containing(self.compose_scope, "C11")
        self.assertTrue(
            block,
            "bin/compose-scope.sh 的 C11 锚点不在注释里，无法作为约束登记处",
        )
        self.assertIn(
            "contract-mysql",
            block,
            "C11 注释锚点上下文必须提到 contract-mysql（共享库所在实例）：%s" % block,
        )
        self.assertIn(
            "不随合同停售而删除或改名",
            block,
            "C11 注释锚点上下文必须写明“不随合同停售而删除或改名”：%s" % block,
        )

    def test_readme_documents_shared_boundary(self):
        """README「Temporal 共享边界」必须登记连接事实、安全边界与升级顺序。"""
        section = "\n".join(section_lines(self.readme, TEMPORAL_SECTION))
        self.assertIn("MYSQL_SEEDS", section, "README 共享边界章节缺少 MYSQL_SEEDS 说明")
        self.assertIn("contract-mysql", section, "README 共享边界章节缺少 contract-mysql 说明")
        self.assertIn("DBNAME", section, "README 共享边界章节缺少 DBNAME 说明")
        self.assertIn("default", section, "README 共享边界章节缺少 namespace 缺省/default 说明")
        self.assertIn("C11", section, "README 共享边界章节缺少 C11（停售不删库）说明")
        self.assertIn("task queue", section, "README 共享边界章节缺少 task queue 前缀约束")
        self.assertIn("payload", section, "README 共享边界章节缺少 payload 安全约束")


if __name__ == "__main__":
    unittest.main()
