#!/usr/bin/env python3
"""只读 DSN 默认值门禁：`.env.example` 与 `docker-compose.yml` 不得带非空 `*_READ_DSN` 默认。

背景
----
数析（data-analysis）的报表/看板按 `CONTRACT_READ_DSN`、`CRM_READ_DSN`、
`PROJECT_READ_DSN`、`SETTLEMENT_READ_DSN` 读取业务库投影，生产要求“显式配置 + 独立
SELECT-only 账号”。这些键一旦在模板里带上非空默认值，会同时坏两件事：

1. 安全边界：默认值本身就是仓库内的连接串（含账号/口令），随资产包一起发布；
2. 部署语义：部署方不填也能“起来”，于是悄悄复用业务写账号或连到错误库，
   而 README 与门禁（`tests/test_integration_wiring.py`）假定这些键默认是空的。

因此本用例只回答一个问题：这两个文件里每个 `*_READ_DSN` 的默认值是否为空。

例外登记
--------
确实存在“必须带非空默认”的键时，集中登记在 `ALLOWED_NON_EMPTY_DEFAULTS`，并写清
理由（为什么默认值不构成凭据泄漏、为什么部署方不必逐次填写）。不登记的键一律失败。

依赖与执行
----------
只用标准库逐行解析，不加载 `.env`、不启动容器、不连接数据库、不输出完整运行配置。
默认读本仓库的 `deploy/production`；负例验证时可设置 `DEPLOY_PRODUCTION_ROOT` 指向
/tmp 副本（CI 不设置该变量）。
"""
from __future__ import annotations

import os
import re
import unittest
from pathlib import Path


PRODUCTION = Path(
    os.environ.get("DEPLOY_PRODUCTION_ROOT", Path(__file__).resolve().parents[1])
).resolve()
ENV_EXAMPLE = PRODUCTION / ".env.example"
COMPOSE = PRODUCTION / "docker-compose.yml"

READ_DSN_SUFFIX = "_READ_DSN"
ENV_LINE = re.compile(r"^\s*(?:export\s+)?([A-Za-z_][A-Za-z0-9_]*)\s*=(.*)$")
# Compose `KEY: value`（environment 映射里的键），键名以 _READ_DSN 结尾。
COMPOSE_LINE = re.compile(
    r"^\s*([A-Za-z_][A-Za-z0-9_]*_READ_DSN)\s*:\s*(.*?)\s*$"
)
# Compose 插值：`${KEY}` / `${KEY:-}` / `${KEY-}` 都是“未设置默认值”。
COMPOSE_EMPTY_INTERPOLATION = re.compile(
    r"^\$\{[A-Za-z_][A-Za-z0-9_]*(?::?-)?\}$|^\$\{[A-Za-z_][A-Za-z0-9_]*:[?].*$"
)

# 集中登记的非空默认值例外：键名 -> 理由。
# 当前为空：四个子系统的只读 DSN 都要求部署方按 `README.md` 显式配置，
# 默认非空会把连接串（含凭据）写进仓库，并在“忘记配置”时静默连错库。
# 新增例外必须同时说明为什么默认值可公开、以及为什么部署方不需要逐次填写。
ALLOWED_NON_EMPTY_DEFAULTS = {}


def read(path: Path) -> str:
    if not path.is_file():
        raise AssertionError("缺少部署资产：%s" % path)
    return path.read_text(encoding="utf-8")


def is_empty_value(value: str) -> bool:
    """判断 `KEY=` 后面的值是否等价于空（含空引号）。"""
    value = value.strip()
    if value in ("", '""', "''"):
        return True
    return False


def env_example_read_dsns(text: str):
    """`.env.example` → {key: 原始默认值}；只保留 `*_READ_DSN` 键。"""
    declared = {}
    for line in text.splitlines():
        stripped = line.strip()
        if not stripped or stripped.startswith("#"):
            continue
        match = ENV_LINE.match(line)
        if not match:
            continue
        key = match.group(1)
        if key.endswith(READ_DSN_SUFFIX):
            declared[key] = match.group(2).strip()
    return declared


def compose_read_dsns(text: str):
    """`docker-compose.yml` → {key: 原始默认值}；只保留 `*_READ_DSN` 键。"""
    declared = {}
    for line in text.splitlines():
        stripped = line.strip()
        if not stripped or stripped.startswith("#"):
            continue
        match = COMPOSE_LINE.match(line)
        if not match:
            continue
        declared[match.group(1)] = match.group(2).strip()
    return declared


def has_non_empty_default(value: str) -> bool:
    if is_empty_value(value):
        return False
    return not COMPOSE_EMPTY_INTERPOLATION.match(value)


def offending(declared, source: str):
    """返回 [(key, value)]：默认值非空且未登记为例外。"""
    failures = []
    for key, value in sorted(declared.items()):
        if key in ALLOWED_NON_EMPTY_DEFAULTS:
            continue
        if has_non_empty_default(value):
            failures.append((key, value, source))
    return failures


class ReadOnlyDsnDefaultsTest(unittest.TestCase):
    """只读 DSN 默认值必须为空，避免凭据入库与静默错连。"""

    @classmethod
    def setUpClass(cls):
        cls.env_example = env_example_read_dsns(read(ENV_EXAMPLE))
        cls.compose = compose_read_dsns(read(COMPOSE))

    def test_env_example_read_dsn_defaults_are_empty(self):
        failures = offending(self.env_example, str(ENV_EXAMPLE))
        detail = "\n".join("%s=%s" % (key, value) for key, value, _ in failures)
        self.assertEqual(
            failures,
            [],
            "`.env.example` 的只读 DSN 默认值必须为空（部署方显式配置），"
            "以下键带非空默认值：\n%s" % detail,
        )

    def test_compose_read_dsn_defaults_are_empty(self):
        failures = offending(self.compose, str(COMPOSE))
        detail = "\n".join("%s: %s" % (key, value) for key, value, _ in failures)
        self.assertEqual(
            failures,
            [],
            "`docker-compose.yml` 的只读 DSN 插值必须没有非空默认值"
            "（应为 `${KEY:-}`），以下键带非空默认值：\n%s" % detail,
        )

    def test_read_dsns_are_discovered(self):
        """防止解析器静默失效：两个文件都必须真实解析出只读 DSN 键。"""
        self.assertTrue(
            self.env_example,
            "`.env.example` 未解析出任何 `*_READ_DSN` 键（解析或文件被整体删改）",
        )
        self.assertTrue(
            self.compose,
            "`docker-compose.yml` 未解析出任何 `*_READ_DSN` 键（解析或注入被删改）",
        )


if __name__ == "__main__":
    unittest.main()
