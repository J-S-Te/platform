#!/usr/bin/env python3
"""部署资产入库完整性门禁（DEP-01）：被引用并随包发布的文件必须真的在 git 索引里。

背景
----
`monitoring/prometheus/prometheus.yml` 的 `rule_files` 和权威编排
`docker-compose.yml` 的只读挂载都引用部署目录内的文件。这些文件曾经“存在于开发
机器的磁盘上、也被构建脚本打包”，但从未 `git add`：`git ls-files --error-unmatch`
报错，而 `bin/build-offline-packages.sh` 与 compose 照样引用它们。换一台机器
checkout 后，构建/部署会在 prometheus 启动时才失败（容器内找不到规则文件直接退出），
或者更糟：交付目录里少一个告警文件而无人发现。

本用例把这条发布阻断固化成断言：

1. prometheus.yml 的每个 `rule_files` 条目都对应部署目录内真实存在且已被 git 跟踪的文件；
2. `monitoring/prometheus/*.yml` 全部被跟踪（整个目录随资产包发布，新增规则文件
   不能只躺在工作树里）；
3. docker-compose.yml 里 `./` 相对挂载、部署根绝对挂载与 env_file 引用的部署文件，
   只要在当前工作树真实存在，就必须被跟踪；
4. 声明的目录挂载（如 subsystem-templates/、subsystems.d/）内的每个文件都必须被跟踪。

刻意不做的事
------------
- 不要求 `runtime/`、`data/`、`backups/` 入库：它们是部署期生成目录，本来就不在
  版本库里；判据是“引用的路径在当前工作树里存在才要求入库”。
- 不断言 bin/、tests/ 下的全部文件入库：本任务期间存在他人未提交的新脚本/测试，
  它们不属于 DEP-01 的“被引用资产”范围，也不得因为本门禁被迫 git add。
- 不校验告警 YAML 的语义与指标名白名单：那由
  tests/test-compose-orchestration-consistency.py 负责，本用例只回答“文件在不在索引里”。

依赖与执行
----------
只用标准库与 git 命令，不需要 pyyaml/ruby。在没有 .git 的源码快照里会明确 skip
（构建脚本同样只在 git 工作树内执行本门禁，见 bin/build-offline-packages.sh）。
"""
from __future__ import annotations

import re
import subprocess
import unittest
from pathlib import Path

PRODUCTION = Path(__file__).resolve().parents[1]
DEPLOY_ROOT_RELATIVE = "deploy/production"
PROMETHEUS_DIR = PRODUCTION / "monitoring" / "prometheus"
PROMETHEUS_CONFIG = PROMETHEUS_DIR / "prometheus.yml"
COMPOSE = PRODUCTION / "docker-compose.yml"

# 历史部署根（参考副本/旧文档里出现过）与当前部署根：都折叠成部署目录内相对路径。
DEPLOY_ROOT_PREFIXES = ("/opt/unified-identity-platform/", "/opt/basic-platform/")
RULE_FILE_PREFIX = "/etc/prometheus/"
# 部署期生成、不入库的目录前缀：即使被 compose 引用也不要求跟踪。
RUNTIME_PREFIXES = ("runtime/", "data/", "backups/", "monitoring/textfile/")


def run_git(*args: str) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        ["git", "-C", str(PRODUCTION), *args], capture_output=True, text=True, check=False
    )


def ls_files_error_unmatch(repo_root: Path, repo_relative: str) -> subprocess.CompletedProcess[str]:
    """在仓库根执行 `git ls-files --error-unmatch`。

    注意：paths 相对 `git -C` 的工作目录解析，因此必须在仓库根执行并传仓库根相对路径，
    否则子目录里的调用会把 deploy/production/... 再拼一次而误报未跟踪。
    """
    return subprocess.run(
        ["git", "-C", str(repo_root), "ls-files", "--error-unmatch", "--", repo_relative],
        capture_output=True,
        text=True,
        check=False,
    )


def git_toplevel() -> Path | None:
    result = run_git("rev-parse", "--show-toplevel")
    if result.returncode != 0:
        return None
    return Path(result.stdout.strip())


def expand_defaults(value: str) -> str | None:
    """把 `${VAR:-default}` 折叠成 default；仍含变量时返回 None（无法静态判定）。"""
    expanded = re.sub(r"\$\{[^}]*:-([^}]*)\}", r"\1", value)
    if "${" in expanded:
        return None
    return expanded


def deploy_relative(host_path: str) -> str | None:
    """把挂载/引用路径归一成部署目录内的相对路径。"""
    candidate = expand_defaults(host_path.strip())
    if candidate is None:
        return None
    for prefix in DEPLOY_ROOT_PREFIXES:
        if candidate.startswith(prefix):
            candidate = candidate[len(prefix):]
            break
    else:
        if candidate.startswith("./"):
            candidate = candidate[2:]
        elif candidate.startswith("/"):
            # 绝对路径但不在已知部署根下（如容器内路径）→ 与部署目录无关。
            return None
    candidate = candidate.rstrip("/")
    if not candidate or candidate in {".", ".."}:
        return None
    if candidate.startswith(RUNTIME_PREFIXES):
        return None
    return candidate


def compose_references() -> dict[str, set[str]]:
    """从权威编排提取本地文件/目录引用，按来源分类返回部署目录内相对路径集合。"""
    text = COMPOSE.read_text(encoding="utf-8")
    raw_mounts: set[str] = set()
    raw_env_files: set[str] = set()
    for line in text.splitlines():
        stripped = line.strip()
        if not stripped.startswith("- "):
            continue
        entry = stripped[2:].strip().strip('"').strip("'")
        path_match = re.match(r"^path:\s*(\S+)", entry)
        if path_match:
            raw_env_files.add(path_match.group(1))
            continue
        # 整个条目就是 "${VAR:-default}"（env_file 的简写形式）。
        whole_variable = re.match(r"^\$\{[^}]*:-([^}]*)\}$", entry)
        if whole_variable:
            raw_env_files.add(whole_variable.group(1))
            continue
        if ":" not in entry:
            continue
        host_path = entry.split(":", 1)[0]
        if host_path.startswith("./") or host_path.startswith("/opt/") or "${" in host_path:
            raw_mounts.add(host_path)

    references: dict[str, set[str]] = {"mount": set(), "env_file": set()}
    for host_path in raw_mounts:
        relative = deploy_relative(host_path)
        if relative is not None:
            references["mount"].add(relative)
    for raw in raw_env_files:
        relative = deploy_relative(raw)
        if relative is not None:
            references["env_file"].add(relative)
    return references


def prometheus_rule_files() -> list[str]:
    """从 prometheus.yml 的 rule_files 块提取容器内规则文件路径（简单列表语法）。"""
    text = PROMETHEUS_CONFIG.read_text(encoding="utf-8")
    rule_files: list[str] = []
    in_block = False
    for line in text.splitlines():
        if not in_block:
            in_block = bool(re.match(r"^rule_files:\s*$", line))
            continue
        if re.match(r"^\S", line):
            break
        match = re.match(r"^\s+-\s+(\S+)\s*$", line)
        if match:
            rule_files.append(match.group(1).strip('"').strip("'"))
    return rule_files


class GitIndexCompletenessTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.repo_root = git_toplevel()
        if cls.repo_root is None:
            raise unittest.SkipTest("当前目录不在 git 工作树内，跳过部署资产入库检查（DEP-01）")

    def assert_tracked(self, relative: str) -> None:
        """断言部署目录内的文件已被 git 跟踪；目录则递归检查其全部文件。"""
        path = PRODUCTION / relative
        if path.is_dir():
            files = sorted(item for item in path.rglob("*") if item.is_file())
            self.assertTrue(files, f"目录挂载 {DEPLOY_ROOT_RELATIVE}/{relative} 下没有任何文件")
            for item in files:
                self.assert_tracked(str(item.relative_to(PRODUCTION)))
            return
        repo_relative = f"{DEPLOY_ROOT_RELATIVE}/{relative}"
        result = ls_files_error_unmatch(self.repo_root, repo_relative)
        self.assertEqual(
            result.returncode,
            0,
            f"{repo_relative} 被引用并随包发布，但没有进入 git 索引（DEP-01）："
            f"执行 git add {repo_relative}；否则换一台机器 checkout 后构建/部署会缺文件。"
            f"git 输出：{result.stderr.strip()!r}",
        )

    def assert_tracked_if_present(self, relative: str) -> None:
        """存在才要求入库：runtime/generated 路径在干净 checkout 里本来就不存在。"""
        if not (PRODUCTION / relative).exists():
            return
        self.assert_tracked(relative)

    def test_prometheus_rule_files_exist_and_are_tracked(self) -> None:
        """prometheus.yml 的 rule_files 必须存在且入库，否则 prometheus 启动即失败。"""
        rule_files = prometheus_rule_files()
        self.assertTrue(rule_files, "prometheus.yml 的 rule_files 不得为空")
        for container_path in rule_files:
            with self.subTest(rule_file=container_path):
                self.assertTrue(
                    container_path.startswith(RULE_FILE_PREFIX),
                    f"rule_files 条目必须位于 {RULE_FILE_PREFIX} 下：{container_path!r}",
                )
                relative = f"monitoring/prometheus/{container_path[len(RULE_FILE_PREFIX):]}"
                self.assertTrue(
                    (PRODUCTION / relative).is_file(),
                    f"prometheus.yml 引用的规则文件不存在：{DEPLOY_ROOT_RELATIVE}/{relative}",
                )
                self.assert_tracked(relative)

    def test_prometheus_directory_files_are_all_tracked(self) -> None:
        """整个 monitoring/prometheus 目录随资产包发布：规则文件不能只留在工作树。"""
        files = sorted(PROMETHEUS_DIR.glob("*.yml"))
        self.assertTrue(files, "monitoring/prometheus 下没有任何 *.yml")
        untracked = [
            file.name
            for file in files
            if ls_files_error_unmatch(
                self.repo_root,
                f"{DEPLOY_ROOT_RELATIVE}/monitoring/prometheus/{file.name}",
            ).returncode
            != 0
        ]
        self.assertEqual(
            untracked,
            [],
            "monitoring/prometheus 下的规则文件未入库（DEP-01）："
            f"{untracked}；执行 git add 对应文件，否则资产包与 git 内容不一致。",
        )

    def test_compose_mounts_are_tracked(self) -> None:
        """compose 的本地挂载（规则文件、SQL 模板、模板/清单目录）必须入库。"""
        mounts = sorted(compose_references()["mount"])
        self.assertTrue(mounts, "docker-compose.yml 中未解析到任何本地部署资产挂载")
        for relative in mounts:
            with self.subTest(mount=relative):
                self.assert_tracked_if_present(relative)

    def test_compose_env_files_are_tracked(self) -> None:
        """compose 的 env_file 引用：工作树里存在（非 runtime/）就必须入库。"""
        env_files = sorted(compose_references()["env_file"])
        self.assertTrue(env_files, "docker-compose.yml 中未解析到任何 env_file 引用")
        for relative in env_files:
            with self.subTest(env_file=relative):
                self.assert_tracked_if_present(relative)


if __name__ == "__main__":
    unittest.main()
