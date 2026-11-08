#!/usr/bin/env python3
"""部署前运行配置门禁：runtime/*.env 重复键 + 生产 worker 标志必须为字面量 false。

背景（两次现场真实故障）：

  A. `runtime/contract.env` 里 `CONTRACT_IMPORT_HISTORY_ENABLED`、
     `CONTRACT_IMPORT_CRM_PROVISION_ENABLED` 各出现两次（值甚至相同）。受控更新时
     `updateProductionSubsystemEnvironment` 对**受管键**逐行改写，遇到第二个同名行直接
     返回 `duplicate managed production environment key ...`，接入因此报
     `validate runtime environment group` 失败——而这在发布前完全看不出来。

  B. 编排里 `project-migrate` 服务把 `PROJECT_RUN_WORKER_WITH_API` 写成了 `"true"`
     （`project-api` 是 false，迁移服务漏改）。迁移进程因此会在生产里额外启动后台
     worker，生产校验直接失败：`PROJECT_RUN_WORKER_WITH_API must be false in production`。

因此本脚本把这两条从“隐含约定”变成**部署成功前的显式门禁**：

  检查 A（重复键）：扫描 `<deploy-dir>/runtime/*.env`。按 `KEY=` 解析（与 provisioner 的
    `environmentLineKey` 同口径：容忍 `export ` 前缀，只认合法大写键名），忽略注释与空行；
    同一文件内同名键出现两次及以上即失败——**值相同也失败**，因为 provisioner 只看行数，
    不看值。失败信息给出 `文件:键:行号` 与保留/删除建议。

  检查 B（worker 标志）：解析 `<deploy-dir>/docker-compose.yml`、
    `compose.production.yaml`、`compose.yaml` 中 `PROJECT_RUN_WORKER_WITH_API` 的每一处
    字面值。要求**全部为字面量 false**（`project-migrate`、`project-api`、Worker、锚点
    一视同仁）。出现 `"true"`、空值、`${...}` 插值（尤其 `:-true` 默认值）都失败，并给出
    `文件:行号` 与服务名。

安全与副作用：只读 `<deploy-dir>` 下的文本文件，不启动容器、不执行 compose、不写任何文件；
输出只给键名与行号，不打印任何键值（runtime env 里可能有凭据）。`--dry-run` 只打印计划，
连文件内容都不解析。

用法：
  python3 check-runtime-env-hygiene.py [--deploy-dir <dir>] [--apps <编码,...>] [--json]
                                      [--dry-run]

退出码：0 通过（含“无 runtime 目录/无 compose 文件”的显式跳过）；
        1 发现重复键或 worker 标志不是 false；2 参数错误。
"""
from __future__ import annotations

import argparse
import json
import re
import sys
from pathlib import Path

SCHEMA = "uip.runtime-env-hygiene/v1"
WORKER_KEY = "PROJECT_RUN_WORKER_WITH_API"
WORKER_REQUIRED = "false"
RUNTIME_DIR_NAME = "runtime"
RUNTIME_GLOB = "*.env"
# 生产编排文件按优先级列出；只检查实际存在的那几个，避免在只带 docker-compose.yml 的
# 部署目录里误报“compose.production.yaml 缺失”。
COMPOSE_FILE_NAMES = ("docker-compose.yml", "compose.production.yaml", "compose.yaml")
# 子系统应用编码 -> 该子系统的 runtime 环境文件（--apps 过滤用）。
APPLICATIONS = {
    "contract_management": ("contract.env",),
    "customer_and_opportunity": ("customer.env",),
    "customer_portal": ("portal.env", "portal-report-worker.env"),
    "project_management": ("project.env",),
    "settlement": ("settlement.env",),
    "data_analysis": ("data-analysis.env",),
}
# 与 provisioner 的 validEnvironmentKey 同口径：首字符 _ 或大写字母，其余 _ 大写字母或数字。
ENV_KEY_PATTERN = re.compile(r"[A-Z_][A-Z0-9_]*\Z")
SERVICE_HEADER_PATTERN = re.compile(r"^ {2}([A-Za-z0-9][A-Za-z0-9_.-]*):\s*(?:#.*)?$")
WORKER_LINE_PATTERN = re.compile(
    r"(?<![A-Za-z0-9_.-])" + re.escape(WORKER_KEY) + r"\s*([:=])\s*(.*)\Z")


class GateError(Exception):
    """不可继续的配置错误（消息可直接展示，且不含任何键值）。"""


def default_deploy_dir():
    return Path(__file__).resolve().parents[1]


def split_apps(value):
    return [item for item in re.split(r"[\s,]+", (value or "").strip()) if item]


def runtime_env_files(deploy_dir, apps, emit):
    """列出待检查的 runtime env 文件；目录不存在时返回 None（调用方按跳过处理）。"""
    runtime_dir = Path(deploy_dir) / RUNTIME_DIR_NAME
    if not runtime_dir.is_dir():
        emit("[运行配置门禁] 部署目录没有 %s/ 目录（尚未 configure 或不在部署目录）："
             "跳过重复键检查，不视为失败" % RUNTIME_DIR_NAME)
        return None
    wanted = None
    if apps:
        wanted = set()
        for code in apps:
            wanted.update(APPLICATIONS[code])
    files = []
    for path in sorted(runtime_dir.glob(RUNTIME_GLOB)):
        if not path.is_file() or path.is_symlink():
            # provisioner 只接受常规文件（symlink 会报 production environment file is
            # unavailable，属另一类故障）；这里不猜测内容，明确跳过并提示。
            emit("[运行配置门禁] 跳过 %s/%s：不是常规文件（符号链接），"
                 "provisioner 也不会接受它" % (RUNTIME_DIR_NAME, path.name))
            continue
        if wanted is not None and path.name not in wanted:
            continue
        files.append(path)
    if not files:
        emit("[运行配置门禁] %s/ 下没有可检查的 %s 文件%s"
             % (RUNTIME_DIR_NAME, RUNTIME_GLOB,
                "（--apps 指定的子系统环境文件不存在）" if wanted is not None else ""))
    return files


def unquote(value):
    value = value.strip()
    if len(value) >= 2 and value[0] == value[-1] and value[0] in ("'", '"'):
        return value[1:-1]
    return value


def environment_line_key(line):
    """与 provisioner environmentLineKey 同口径：返回 (键, 值原文) 或 (None, None)。"""
    trimmed = line.strip()
    if not trimmed or trimmed.startswith("#"):
        return None, None
    if trimmed.startswith("export "):
        trimmed = trimmed[len("export "):].strip()
    key, separator, value = trimmed.partition("=")
    key = key.strip()
    if not separator or not ENV_KEY_PATTERN.fullmatch(key):
        return None, None
    return key, value


def duplicate_keys(path):
    """返回该文件内的重复键记录：[(键, [(行号, 归一值), ...]), ...]。"""
    occurrences = {}
    text = path.read_text(encoding="utf-8", errors="replace")
    for lineno, line in enumerate(text.splitlines(), 1):
        key, value = environment_line_key(line)
        if key is None:
            continue
        occurrences.setdefault(key, []).append((lineno, unquote(value)))
    duplicates = []
    for key in sorted(occurrences):
        entries = occurrences[key]
        if len(entries) > 1:
            duplicates.append((key, entries))
    return duplicates


def compose_files(deploy_dir, emit):
    found = []
    for name in COMPOSE_FILE_NAMES:
        path = Path(deploy_dir) / name
        if path.is_file() and not path.is_symlink():
            found.append(path)
    if not found:
        emit("[运行配置门禁] 部署目录没有可解析的编排文件（%s）：跳过生产 worker 标志检查，"
             "不视为失败" % "、".join(COMPOSE_FILE_NAMES))
    return found


def classify_worker_value(value):
    """把一处 worker 标志的字面值归类为 (失败码, 说明)；通过时返回 (None, None)。"""
    if value == WORKER_REQUIRED:
        return None, None
    if value.lower() == "true":
        return ("worker-flag-true",
                "值为 true：生产校验要求该值必须为 false")
    if value == "":
        return ("worker-flag-empty",
                "值为空：生产校验要求该值必须为 false")
    if value.startswith("${"):
        inner = value[2:-1] if value.endswith("}") else value[2:]
        default = ""
        if ":-" in inner:
            default = inner.split(":-", 1)[1]
        elif "-" in inner:
            default = inner.split("-", 1)[1]
        if default.strip().lower() == "true":
            return ("worker-flag-default-true",
                    "插值默认值为 true（%s）：环境变量缺失时就是 true，生产校验会失败；"
                    "必须写成字面量 false" % value)
        return ("worker-flag-interpolated",
                "写成了插值（%s）：取值随运行环境变化，无法保证生产里是 false；"
                "必须写成字面量 false" % value)
    return ("worker-flag-not-false",
            "值为 %s：生产校验要求该值必须为 false" % value)


def worker_occurrences(path):
    """解析一处编排文件里的 worker 标志：返回 [(服务/锚点名, 行号, 值), ...]。"""
    occurrences = []
    owner = None
    in_services = False
    text = path.read_text(encoding="utf-8", errors="replace")
    for lineno, raw in enumerate(text.splitlines(), 1):
        line = raw.rstrip()
        stripped = line.strip()
        if not stripped or stripped.startswith("#"):
            continue
        indent = len(line) - len(line.lstrip(" \t"))
        if indent == 0:
            # 顶层键：`services:` 开启服务块；`x-...` 锚点块按自身名字归属。
            top_key = stripped.split(":", 1)[0].strip()
            in_services = top_key == "services"
            owner = None if in_services else top_key or None
            continue
        if in_services and indent == 2:
            match = SERVICE_HEADER_PATTERN.match(line)
            if match:
                owner = match.group(1)
                continue
        match = WORKER_LINE_PATTERN.search(line)
        if not match:
            continue
        value = match.group(2).strip()
        # 只把“空白 + #”当行尾注释；值里可能出现的 # 属于值本身。
        if " #" in value:
            value = value.split(" #", 1)[0].rstrip()
        occurrences.append((owner or "未命名块", lineno, unquote(value)))
    return occurrences


def collect_failures(deploy_dir, files, composites, emit):
    """执行检查 A/B，返回 (证据, 失败列表)；发现的失败不中断，全部汇总后判定。"""
    evidence = {
        "schema": SCHEMA,
        "ok": False,
        "deploy_dir": str(deploy_dir),
        "runtime_dir": RUNTIME_DIR_NAME if files is not None else None,
        "runtime_files": [path.name for path in (files or [])],
        "compose_files": [path.name for path in composites],
        "duplicate_keys": [],
        "worker_flags": [],
        "failures": [],
    }
    failures = []

    def fail(code, subject, message):
        failures.append({"code": code, "subject": subject, "message": message})
        emit("[运行配置门禁失败] %s：%s" % (code, message))

    # 检查 A：同一 runtime 文件内的重复键。provisioner 遇到受管键重复直接失败，
    # 与两行值是否相同无关，因此这里也不比较值。
    for path in files or []:
        try:
            duplicates = duplicate_keys(path)
        except OSError as error:
            fail("runtime-env-unreadable", path.name,
                 "无法读取 %s/%s：%s；无法证明运行配置干净，按失败处理"
                 % (RUNTIME_DIR_NAME, path.name, error.strerror or error))
            continue
        for key, entries in duplicates:
            lines = [lineno for lineno, _ in entries]
            values = {value for _, value in entries}
            same_value = len(values) == 1
            record = {
                "file": "%s/%s" % (RUNTIME_DIR_NAME, path.name),
                "key": key,
                "lines": lines,
                "same_value": same_value,
            }
            evidence["duplicate_keys"].append(record)
            fail("runtime-env-duplicate-key", "%s:%s" % (record["file"], key),
                 "%s 中键 %s 重复出现 %d 次（第 %s 行，值%s）：provisioner 的 "
                 "updateProductionSubsystemEnvironment 遇到重复的受管键会直接失败"
                 "（即使值相同），接入报 validate runtime environment group；"
                 "请只保留一行（删掉其余重复行）再发布"
                 % (record["file"], key, len(lines),
                    "、".join(str(item) for item in lines),
                    "相同" if same_value else "不同"))

    # 检查 B：编排里每一处 worker 标志都必须是字面量 false。
    for path in composites:
        try:
            occurrences = worker_occurrences(path)
        except OSError as error:
            fail("compose-unreadable", path.name,
                 "无法读取编排文件 %s：%s；无法证明生产 worker 标志为 false，按失败处理"
                 % (path.name, error.strerror or error))
            continue
        if not occurrences:
            emit("[运行配置门禁] %s 未声明 %s（沿用镜像默认）：不视为失败，"
                 "但生产发布前请确认迁移服务显式写死 false" % (path.name, WORKER_KEY))
            continue
        for service, lineno, value in occurrences:
            code, reason = classify_worker_value(value)
            record = {
                "file": path.name,
                "line": lineno,
                "service": service,
                "value": value,
                "ok": code is None,
            }
            evidence["worker_flags"].append(record)
            if code is None:
                continue
            fail(code, "%s:%s" % (path.name, lineno),
                 "%s（%s:%d）的 %s %s；请改成 %s=%s（project-api、Worker 与 "
                 "project-migrate 必须一致），否则生产校验报 "
                 "PROJECT_RUN_WORKER_WITH_API must be false in production"
                 % (service, path.name, lineno, WORKER_KEY, reason,
                    WORKER_KEY, WORKER_REQUIRED))

    evidence["failures"] = failures
    evidence["ok"] = not failures
    return evidence, failures


def report(options, emit):
    if options.dry_run:
        emit("[运行配置门禁] 预演（--dry-run）：只打印计划，不读取/写入任何文件")
        emit("[运行配置门禁] 部署目录=%s" % options.deploy_dir)
        target = "、".join(options.apps) if options.apps else "全部 runtime/*.env"
        emit("[运行配置门禁] 待检查运行环境文件=%s" % target)
        emit("[运行配置门禁] 计划检查：")
        emit("  1) %s/*.env 内同名的 KEY= 不得重复（值相同也算重复），失败给出 文件:键:行号"
             % RUNTIME_DIR_NAME)
        emit("  2) %s 中每处 %s 必须是字面量 %s，失败给出 文件:行号:服务名"
             % ("/".join(COMPOSE_FILE_NAMES), WORKER_KEY, WORKER_REQUIRED))
        return 0

    files = runtime_env_files(options.deploy_dir, options.apps, emit)
    composites = compose_files(options.deploy_dir, emit)
    evidence, failures = collect_failures(options.deploy_dir, files, composites, emit)

    if not failures:
        runtime_note = (
            "runtime/*.env 无重复键（%d 个文件）" % len(files) if files is not None
            else "无 runtime/ 目录，重复键检查跳过")
        compose_note = (
            "%d 个编排文件中的 %s 全部为字面量 %s"
            % (len(composites), WORKER_KEY, WORKER_REQUIRED) if composites
            else "无编排文件，worker 标志检查跳过")
        emit("[运行配置门禁] 通过：%s；%s" % (runtime_note, compose_note))
    else:
        emit("[运行配置门禁] 未通过：共 %d 项问题（逐条见上）。先修配置再发布。" % len(failures))
        emit("处置：runtime/*.env 保留每个键唯一一行；编排里 %s 全部写死 %s"
             % (WORKER_KEY, WORKER_REQUIRED))
        emit("说明：这两类问题分别对应 provisioner 的 validate runtime environment group "
             "与生产校验 PROJECT_RUN_WORKER_WITH_API must be false in production。")
    if options.json:
        json.dump(evidence, sys.stdout, ensure_ascii=False, indent=2, sort_keys=True)
        sys.stdout.write("\n")
    return 0 if not failures else 1


def parse_arguments(argv=None):
    parser = argparse.ArgumentParser(
        description="部署前运行配置门禁：runtime/*.env 重复键 + 生产 worker 标志必须为 false")
    parser.add_argument("--deploy-dir", default=str(default_deploy_dir()),
                        help="部署目录（含 runtime/ 与编排文件的目录；默认取本脚本的上一级）")
    parser.add_argument("--apps", default="",
                        help="只检查指定子系统应用编码（逗号或空格分隔：%s），默认检查全部 "
                             "runtime/*.env" % "、".join(APPLICATIONS))
    parser.add_argument("--json", action="store_true",
                        help="以 JSON 输出完整证据（人类日志转到 stderr）")
    parser.add_argument("--dry-run", action="store_true",
                        help="只打印计划，不读取/写入任何文件")
    options = parser.parse_args(argv)

    deploy_dir = Path(options.deploy_dir).expanduser()
    if not deploy_dir.is_dir():
        parser.error("部署目录不存在：%s" % deploy_dir)
    options.deploy_dir = deploy_dir.resolve()
    options.apps = split_apps(options.apps)
    unknown = [code for code in options.apps if code not in APPLICATIONS]
    if unknown:
        parser.error("不支持的应用编码：%s（支持：%s）"
                     % ("、".join(unknown), "、".join(APPLICATIONS)))
    return options


def main(argv=None):
    options = parse_arguments(argv)
    if options.json:
        def emit(message):
            print(message, file=sys.stderr)
    else:
        def emit(message):
            print(message)
    try:
        return report(options, emit)
    except GateError as error:
        print("[运行配置门禁失败] %s" % error, file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
