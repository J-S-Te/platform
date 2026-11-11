#!/usr/bin/env python3
"""离线部署后的许可运行态门禁：容器健康 + 许可状态卷 + 许可实例一致 + 受控部署核验。

背景（现场真实故障）：`project_management` 的 API 容器缺少
`/var/lib/commercial-license` 卷挂载后，容器启动即报
`commercial license configuration failed`，随后每分钟重启；平台页面只显示
「部署 Agent 执行失败」，操作员重复点击「重试」7 次都无效——因为重试既不会补上
缺失的许可状态卷，也不会纠正与平台库不一致的许可实例。

因此本脚本把“许可运行态”从隐含前提变成**部署成功前的显式门禁**，四项全过才算通过：

  1. 每个已登记商业应用的 API 容器存在、`running`、健康（无 healthcheck 时看运行状态）；
  2. 该容器存在挂载目标 `/var/lib/commercial-license` 的**命名卷**；
  3. 容器 env `COMMERCIAL_LICENSE_INSTANCE_ID` 与平台库
     `license_deployment.instance_id` 一致；
  4. `license-install verify-deployment --application <code>` 全部通过（受控部署已确认）。

已显式停用（`deploy.sh disable <子系统>`，容器停在 exited/created）的应用在**自动发现**时按
停用跳过并给出提示；`--apps` 显式指定（continue/verify <CLI参数> 走的路径）仍然严格判定，
因此“该应用必须完成”的场景不会因为跳过而放行。

安全约束：脚本只读运行态，不重启容器、不改固定记录、不导入/激活许可；只读查询使用
容器内 `MYSQL_PWD` 传递数据库口令，绝不把口令放进 argv；输出只给许可实例的**前缀**，
不打印任何密钥、口令、凭据或许可原文。

冒烟与反例：`--dry-run` 只打印计划、不执行任何 docker 命令（零副作用）；`--fixture`
用一份 JSON 事实集替代实时探测，可在没有生产环境的情况下验证“缺卷 / 实例不一致”等
反例是否被正确诊断（见 tests/test-license-runtime.py）。
"""
from __future__ import annotations

import argparse
import json
import re
import subprocess
import sys
from pathlib import Path

SCHEMA = "uip.license-runtime-check/v1"
LICENSE_STATE_TARGET = "/var/lib/commercial-license"
LICENSE_ENABLED = "COMMERCIAL_LICENSE_ENABLED"
LICENSE_INSTANCE = "COMMERCIAL_LICENSE_INSTANCE_ID"
LICENSE_STATE_PATH = "COMMERCIAL_LICENSE_STATE_PATH"
DEFAULT_COMPOSE_PROJECT = "basic-platform-production"
INSTANCE_PATTERN = re.compile(r"[A-Za-z0-9][A-Za-z0-9_.:@-]{0,127}\Z")
DATABASE_PATTERN = re.compile(r"[A-Za-z_][A-Za-z0-9_]*\Z")
OUTPUT_LIMIT = 4000

# 应用编码 -> (API 服务名候选, 中文名)。服务名候选同时覆盖两套编排命名：
# 生产 docker-compose.yml 用 `data-analysis-api`，本地 compose.local.yaml 用
# `dashboard-api`、平台 API 用 `api`、平台库用 `mysql`。多候选按顺序取第一个命中的，
# 避免把本地编排误判成“容器缺失”。
APPLICATIONS = {
    "contract_management": (("contract-api",), "合同管理系统"),
    "customer_and_opportunity": (("customer-api",), "客户与商机管理系统"),
    "customer_portal": (("portal-api",), "客户自助门户"),
    "project_management": (("project-api",), "项目服务内容管理系统"),
    "reimbursement": (("reimbursement-api",), "报销管理系统"),
    "settlement": (("settlement-api",), "结算与开票管理系统"),
    "data_analysis": (("data-analysis-api", "dashboard-api"), "数据看板与统计分析系统"),
}
PLATFORM_API_SERVICES = ("platform-api", "api")
PLATFORM_MYSQL_SERVICES = ("platform-mysql", "mysql")
# 干净停止的容器状态：只见于显式 stop/disable 或从未启动，不是许可运行态故障的表现。
STOPPED_STATUSES = ("exited", "created", "dead")
ONE_OFF_LABEL = "com.docker.compose.oneoff"


class GateError(Exception):
    """不可继续的探测/配置错误（消息可直接展示，且不含任何凭据）。"""


class Container:
    """一个容器的只读事实集；env 仅在进程内用于比对，不进入任何输出。"""

    __slots__ = ("identifier", "name", "service", "application", "oneoff", "status",
                 "health", "restart_count", "exit_code", "mounts", "env")

    def __init__(self, *, identifier, name, service, application, oneoff, status,
                 health, restart_count, exit_code, mounts, env):
        self.identifier = identifier
        self.name = name
        self.service = service
        self.application = application
        self.oneoff = oneoff
        self.status = status
        self.health = health
        self.restart_count = restart_count
        self.exit_code = exit_code
        self.mounts = mounts
        self.env = env

    @classmethod
    def from_inspect(cls, item):
        if not isinstance(item, dict):
            raise GateError("docker inspect 返回了非对象条目")
        config = item.get("Config") or {}
        labels = config.get("Labels") or {}
        state = item.get("State") or {}
        health = (state.get("Health") or {}).get("Status") or "none"
        mounts = []
        for mount in item.get("Mounts") or []:
            if not isinstance(mount, dict):
                continue
            mounts.append({
                "type": mount.get("Type") or "",
                # 命名卷的真实名字在 Name；绑定挂载没有 Name 时才回落到宿主机 Source。
                "name": mount.get("Name") or mount.get("Source") or "",
                "destination": mount.get("Destination") or "",
            })
        env = {}
        for entry in config.get("Env") or []:
            if not isinstance(entry, str) or "=" not in entry:
                continue
            key, value = entry.split("=", 1)
            env[key] = value
        return cls(
            identifier=item.get("Id") or "",
            name=(item.get("Name") or "").lstrip("/"),
            service=labels.get("com.docker.compose.service") or "",
            application=labels.get("com.basic-platform.application_code") or "",
            oneoff=str(labels.get(ONE_OFF_LABEL) or "").strip().lower() in ("true", "1"),
            status=state.get("Status") or "unknown",
            health=health,
            restart_count=state.get("RestartCount", 0),
            exit_code=state.get("ExitCode", 0),
            mounts=mounts,
            env=env,
        )


def instance_summary(value):
    """把许可实例压成可比较、可展示但不可复用的摘要（前缀 + 长度）。"""
    if not value:
        return None
    return {"prefix": value[:8], "length": len(value)}


class LiveProbe:
    """真实只读探测：docker ps/inspect 与 docker exec。不启动、不重建、不写入。"""

    transport = "docker"

    def __init__(self, timeout=180):
        self.timeout = timeout

    def _run(self, argv, *, check=True):
        try:
            completed = subprocess.run(argv, capture_output=True, text=True, timeout=self.timeout)
        except FileNotFoundError as error:
            raise GateError("找不到 docker 命令，无法核验容器许可运行态；请在部署主机上以 root 执行") from error
        except subprocess.TimeoutExpired as error:
            raise GateError("docker 命令超时（%s 秒）：%s" % (self.timeout, " ".join(argv[:4]))) from error
        if check and completed.returncode != 0:
            detail = (completed.stderr or completed.stdout or "").strip().splitlines()
            reason = detail[-1] if detail else "无输出"
            raise GateError("docker 命令失败：%s（%s）" % (" ".join(argv[:4]), reason))
        return completed

    def containers(self, project):
        listing = self._run([
            "docker", "ps", "-aq", "--filter", "label=com.docker.compose.project=%s" % project,
        ])
        identifiers = [line.strip() for line in listing.stdout.splitlines() if line.strip()]
        if not identifiers:
            return []
        raw = self._run(["docker", "inspect", *identifiers])
        try:
            items = json.loads(raw.stdout)
        except ValueError as error:
            raise GateError("docker inspect 输出不是合法 JSON") from error
        return [Container.from_inspect(item) for item in items]

    def platform_instance(self, mysql, platform_api):
        database = (mysql.env.get("MYSQL_DATABASE") or "").strip()
        if not database and platform_api is not None:
            database = (platform_api.env.get("MYSQL_DATABASE") or "").strip()
        if not DATABASE_PATTERN.fullmatch(database or ""):
            raise GateError("无法确定平台数据库名（%s 未提供合法 MYSQL_DATABASE），拒绝猜测" % mysql.name)
        sql = "SELECT instance_id FROM %s.license_deployment WHERE id = 1" % database
        # 口令只在容器内以 MYSQL_PWD 传递：不进 argv、不进日志（同 deploy-service.sh 既有做法）。
        result = self._run([
            "docker", "exec", mysql.identifier, "sh", "-c",
            'exec env MYSQL_PWD="$MYSQL_ROOT_PASSWORD" mysql -uroot -Nse "$1"', "_", sql,
        ], check=False)
        if result.returncode != 0:
            detail = (result.stderr or "").strip().splitlines()
            raise GateError("读取平台许可实例失败（%s）：%s" % (
                mysql.name, detail[-1] if detail else "查询未返回结果"))
        rows = [line.strip() for line in result.stdout.splitlines() if line.strip()]
        if not rows:
            raise GateError("平台库没有许可部署记录（license_deployment 为空）：请先导入并激活许可")
        return rows[0]

    def verify_deployment(self, platform_api, application, timeout):
        result = self._run([
            "docker", "exec", platform_api.identifier, "./license-install", "verify-deployment",
            "--application", application, "--timeout", timeout,
        ], check=False)
        return result.returncode, (result.stdout or "") + (result.stderr or "")


class FixtureProbe:
    """用 JSON 事实集替代实时探测（反例回归 / 无生产环境时的验证）。

    事实集结构（只读，不接触 Docker）：
      {
        "containers": [
          {"name": "...", "service": "project-api", "application": "project_management",
           "status": "running", "health": "healthy", "restart_count": 0, "exit_code": 0,
           "mounts": [{"type": "volume", "name": "...", "destination": "/var/lib/commercial-license"}],
           "env": {"COMMERCIAL_LICENSE_ENABLED": "true",
                   "COMMERCIAL_LICENSE_INSTANCE_ID": "..."}},
          {"name": "...", "service": "platform-mysql",
           "env": {"MYSQL_DATABASE": "basic_platform"}}
        ],
        "platform_instance_id": "...",
        "license_deployment_missing": false,
        "verify_deployment": {"project_management": {"exit_code": 0, "output": ""}}
      }
    """

    transport = "fixture"

    def __init__(self, document):
        if not isinstance(document, dict) or not isinstance(document.get("containers"), list):
            raise GateError("事实集必须是含 containers 列表的 JSON 对象")
        self.document = document
        self._cache = None

    def containers(self, project):
        if self._cache is not None:
            return self._cache
        facts = []
        for index, item in enumerate(self.document["containers"]):
            if not isinstance(item, dict):
                raise GateError("事实集第 %d 个容器不是对象" % (index + 1))
            mounts = []
            for mount in item.get("mounts") or []:
                if not isinstance(mount, dict):
                    raise GateError("事实集容器 %s 的 mounts 含非对象条目" % item.get("name", index))
                mounts.append({
                    "type": str(mount.get("type") or ""),
                    "name": str(mount.get("name") or ""),
                    "destination": str(mount.get("destination") or ""),
                })
            env = item.get("env") or {}
            if not isinstance(env, dict):
                raise GateError("事实集容器 %s 的 env 不是对象" % item.get("name", index))
            facts.append(Container(
                identifier=str(item.get("id") or "fixture-%d" % (index + 1)),
                name=str(item.get("name") or "fixture-%d" % (index + 1)),
                service=str(item.get("service") or ""),
                application=str(item.get("application") or ""),
                oneoff=bool(item.get("oneoff")),
                status=str(item.get("status") or "running"),
                health=str(item.get("health") or "none"),
                restart_count=item.get("restart_count", 0),
                exit_code=item.get("exit_code", 0),
                mounts=mounts,
                env={str(key): str(value) for key, value in env.items()},
            ))
        self._cache = facts
        return facts

    def platform_instance(self, mysql, platform_api):
        if self.document.get("license_deployment_missing"):
            raise GateError("平台库没有许可部署记录（license_deployment 为空）：请先导入并激活许可")
        value = self.document.get("platform_instance_id")
        if not isinstance(value, str) or not value:
            raise GateError("事实集缺少 platform_instance_id")
        return value

    def verify_deployment(self, platform_api, application, timeout):
        results = self.document.get("verify_deployment") or {}
        if not isinstance(results, dict):
            raise GateError("事实集 verify_deployment 不是对象")
        entry = results.get(application)
        if entry is None:
            raise GateError("事实集缺少 %s 的受控部署核验结果" % application)
        if not isinstance(entry, dict):
            raise GateError("事实集 %s 的受控部署核验结果不是对象" % application)
        return int(entry.get("exit_code", 0)), str(entry.get("output") or "")


def default_deploy_dir():
    return Path(__file__).resolve().parents[1]


def compose_project_from_env(deploy_dir):
    """按部署目录既有约定取 Compose 项目名：.env 的 COMPOSE_PROJECT_NAME，否则生产默认值。"""
    runtime = Path(deploy_dir) / ".env"
    try:
        if runtime.is_file() and not runtime.is_symlink():
            for line in runtime.read_text(encoding="utf-8", errors="replace").splitlines():
                stripped = line.strip()
                if not stripped or stripped.startswith("#") or "=" not in stripped:
                    continue
                key, value = stripped.split("=", 1)
                if key.strip() == "COMPOSE_PROJECT_NAME":
                    value = value.strip().strip('"').strip("'")
                    if value:
                        return value
    except OSError:
        pass
    return DEFAULT_COMPOSE_PROJECT


def split_apps(value):
    return [item for item in re.split(r"[\s,]+", (value or "").strip()) if item]


def pick_container(facts, services):
    """按服务名候选挑唯一的长期容器；多个候选优先仍在运行的，仍有歧义则报错。"""
    candidates = [item for item in facts if not item.oneoff and item.service in services]
    if not candidates:
        return None
    if len(candidates) == 1:
        return candidates[0]
    running = [item for item in candidates if item.status == "running"]
    if len(running) == 1:
        return running[0]
    names = "、".join(sorted(item.name for item in (running or candidates)))
    raise GateError("服务 %s 匹配到多个容器（%s），无法确定核验对象；请人工核对后重试"
                    % ("/".join(services), names))


def application_containers(facts, application):
    return [item for item in facts
            if not item.oneoff and item.application == application]


def api_container_for(facts, application):
    services = APPLICATIONS[application][0]
    candidates = [item for item in application_containers(facts, application)
                  if item.service in services]
    if not candidates:
        # 编排里 API 服务名可能随版本微调：只要标签指向该应用且服务名以 -api 结尾即可。
        candidates = [item for item in application_containers(facts, application)
                      if item.service.endswith("-api")]
    if not candidates:
        return None
    if len(candidates) == 1:
        return candidates[0]
    running = [item for item in candidates if item.status == "running"]
    if len(running) == 1:
        return running[0]
    names = "、".join(sorted(item.name for item in (running or candidates)))
    raise GateError("%s 匹配到多个 API 容器（%s），无法确定核验对象" % (application, names))


def discover_applications(facts):
    """已登记商业应用 = 当前 Compose 项目里带 application_code 标签的长期容器所属应用。"""
    found = {item.application for item in facts
             if not item.oneoff and item.application in APPLICATIONS}
    return [code for code in APPLICATIONS if code in found]


def license_state_mount(facts):
    for mount in facts.mounts:
        if mount["destination"] == LICENSE_STATE_TARGET:
            return mount
    return None


def truncate(text):
    text = (text or "").strip()
    if len(text) <= OUTPUT_LIMIT:
        return text
    return text[:OUTPUT_LIMIT] + "…（输出已截断）"


def run_checks(options, probe, emit):
    """执行四项检查，返回 (证据字典, 是否通过)。发现的失败不中断，全部汇总后判定。"""
    evidence = {
        "schema": SCHEMA,
        "ok": False,
        "deploy_dir": str(options.deploy_dir),
        "compose_project": options.compose_project,
        "license_state_target": LICENSE_STATE_TARGET,
        "transport": getattr(probe, "transport", "docker"),
        "platform": {},
        "applications": [],
        "failures": [],
    }
    failures = []

    def fail(code, application, message):
        failures.append({"code": code, "application": application, "message": message})
        # 失败码先给出，便于现场日志被 grep 到同一类根因；具体处置写在消息里。
        emit("[许可门禁失败] %s：%s" % (code, message))

    facts = probe.containers(options.compose_project)
    applications = list(options.apps) or discover_applications(facts)

    platform_api = pick_container(facts, PLATFORM_API_SERVICES)
    mysql = pick_container(facts, PLATFORM_MYSQL_SERVICES)
    platform_env = (platform_api.env.get("APP_ENV") or "") if platform_api else ""
    audit_env = (platform_api.env.get("AUDIT_ENVIRONMENT_CODE") or "") if platform_api else ""
    production = platform_env == "production" and audit_env == "prod"
    evidence["platform"] = {
        "api_container": platform_api.name if platform_api else None,
        "mysql_container": mysql.name if mysql else None,
        "environment": platform_env or None,
        "audit_environment": audit_env or None,
        "production": production,
    }
    emit("[许可门禁] 部署目录=%s Compose 项目=%s 平台环境=%s/%s 许可状态卷目标=%s"
         % (options.deploy_dir, options.compose_project, platform_env or "未知",
            audit_env or "未知", LICENSE_STATE_TARGET))

    if platform_api is None:
        fail("platform-api-missing", None,
             "无法核验受控部署：基础平台 API 容器未运行（服务 %s）；请先完成平台发布再执行本门禁"
             % "/".join(PLATFORM_API_SERVICES))

    platform_instance = None
    if mysql is None:
        fail("platform-mysql-missing", None,
             "无法读取平台许可实例：平台数据库容器未运行（服务 %s）；请先恢复平台数据库"
             % "/".join(PLATFORM_MYSQL_SERVICES))
    else:
        try:
            platform_instance = probe.platform_instance(mysql, platform_api)
        except GateError as error:
            fail("license-instance-unreadable", None, "平台许可实例不可核验：%s" % error)
        else:
            if not INSTANCE_PATTERN.fullmatch(platform_instance):
                fail("license-instance-invalid", None,
                     "平台库 license_deployment.instance_id 不是合法实例标识，拒绝继续比对")
                platform_instance = None
    evidence["platform"]["license_instance"] = instance_summary(platform_instance)

    for application in applications:
        name = APPLICATIONS[application][1]
        record = {
            "application": application,
            "name": name,
            "container": None,
            "status": None,
            "health": None,
            "restart_count": None,
            "exit_code": None,
            "license_state_volume": None,
            "license_enabled": None,
            "instance": None,
            "instance_matches": None,
            "verify_deployment": None,
            "skipped": None,
        }
        evidence["applications"].append(record)
        try:
            container = api_container_for(facts, application)
        except GateError as error:
            fail("container-ambiguous", application, str(error))
            continue
        if container is None:
            fail("container-missing", application,
                 "%s 的 API 容器不存在（服务 %s）：该应用已登记但运行态缺失；请重新执行接入/受控更新"
                 % (application, "/".join(APPLICATIONS[application][0])))
            continue

        record.update({
            "container": container.name,
            "status": container.status,
            "health": container.health,
            "restart_count": container.restart_count,
            "exit_code": container.exit_code,
            "license_enabled": (container.env.get(LICENSE_ENABLED) or "").strip().lower() or None,
        })

        # 显式停用（`deploy.sh disable <子系统>`）只停止容器、保留卷与凭据，容器会停在
        # exited/created；这不是本次故障形态（缺卷/实例不一致在 restart: unless-stopped 下
        # 表现为 restarting 或 running+unhealthy，不会自己停下来）。因此“自动发现”的应用
        # 遇到干净的停止状态时记录为跳过，避免把刻意停用的子系统误判成部署失败；
        # 而 `--apps`（continue/verify <CLI参数> 走的显式路径）仍然严格判定。
        if not options.apps and container.status in STOPPED_STATUSES:
            record["skipped"] = "容器已停止（可能已显式停用）；如需按运行态故障复核请用 --apps 指定该应用"
            emit("[许可门禁] %s：API 容器处于 %s，按已停用跳过（如需复核：--apps %s）"
                 % (application, container.status, application))
            continue

        # 检查 1：容器存在且健康。重启循环的容器在采样瞬间常显示 running/starting，
        # 因此把“仍在启动且已重启”也判为未收敛，而不是等它下一次崩溃。
        if container.status != "running":
            fail("container-not-running", application,
                 "%s 的 API 容器未运行：容器=%s status=%s health=%s 重启次数=%s 退出码=%s；"
                 "请先查看该容器日志修复启动失败"
                 % (application, container.name, container.status, container.health,
                    container.restart_count, container.exit_code))
        elif container.health == "unhealthy":
            fail("container-unhealthy", application,
                 "%s 的 API 容器健康检查失败（unhealthy）：容器=%s 重启次数=%s；"
                 "商业许可配置失败会表现为这种持续重启"
                 % (application, container.name, container.restart_count))
        elif container.health == "starting":
            if isinstance(container.restart_count, int) and container.restart_count > 0:
                fail("container-restart-loop", application,
                     "%s 的 API 容器尚未收敛且已重启 %s 次（health=starting）：容器可能陷入重启循环；"
                     "请查看 docker logs 确认许可运行态"
                     % (application, container.restart_count))
            else:
                fail("container-not-converged", application,
                     "%s 的 API 容器仍在启动（health=starting）：请等健康收敛后再宣布部署成功"
                     % application)

        # 检查 2：许可状态卷。这是本次现场故障的直接根因：缺卷 -> 启动即失败 -> 每分钟重启。
        if record["license_enabled"] == "true":
            mount = license_state_mount(container)
            if mount is None:
                fail("license-state-volume-missing", application,
                     "%s 的 API 容器缺少许可状态卷，容器会陷入重启循环；"
                     "请重新执行受控更新以按固定记录重建（应用：%s，容器：%s）"
                     % (application, name, container.name))
            elif mount["type"] != "volume":
                fail("license-state-not-volume", application,
                     "%s 的 API 容器许可状态目录不是命名卷（type=%s 目标=%s）："
                     "请按编排重新受控更新，不要用绑定挂载或临时容器替代"
                     % (application, mount["type"] or "未知", LICENSE_STATE_TARGET))
            else:
                record["license_state_volume"] = mount["name"]
            # COMMERCIAL_LICENSE_STATE_PATH 平台写入的是状态目录**内的**状态文件
            # （如 /var/lib/commercial-license/project-api-<id>.json），因此只要求它落在
            # 被挂载的状态目录之下；落在挂载点之外就会写进容器可写层，重启即丢。
            state_path = (container.env.get(LICENSE_STATE_PATH) or "").strip()
            if state_path and state_path != LICENSE_STATE_TARGET \
                    and not state_path.startswith(LICENSE_STATE_TARGET.rstrip("/") + "/"):
                fail("license-state-path-outside-volume", application,
                     "%s 的 API 容器 %s=%s 不在许可状态卷目标 %s 之内：容器会把许可写进可写层，"
                     "重启后丢失并陷入重启循环；请重新执行受控更新"
                     % (application, LICENSE_STATE_PATH, state_path, LICENSE_STATE_TARGET))
        else:
            fail("license-runtime-disabled", application,
                 "%s 的 API 容器未启用商业许可运行态（%s 不是 true）：禁止用关闭许可绕过门禁；"
                 "请重新执行接入/受控更新以写入平台签发的许可运行态"
                 % (application, LICENSE_ENABLED))

        # 检查 3：许可实例与平台库一致。
        app_instance = (container.env.get(LICENSE_INSTANCE) or "").strip()
        record["instance"] = instance_summary(app_instance)
        if not app_instance or not INSTANCE_PATTERN.fullmatch(app_instance):
            fail("license-instance-missing", application,
                 "%s 的 API 容器缺少合法的 %s（平台受控接入未写入许可运行态）："
                 "请重新执行接入/受控更新" % (application, LICENSE_INSTANCE))
        elif platform_instance:
            if app_instance == platform_instance:
                record["instance_matches"] = True
            else:
                record["instance_matches"] = False
                fail("license-instance-mismatch", application,
                     "%s 的 API 容器许可实例与平台不一致（容器前缀=%s 平台前缀=%s）："
                     "容器会持续拒绝许可并在重启循环中失败；"
                     "请按当前平台实例重新执行接入/受控更新，不要手工改固定记录"
                     % (application, app_instance[:8], platform_instance[:8]))

        # 检查 4：受控部署核验。license-install 自己拒绝在非 production/prod 环境运行，
        # 因此非生产安装显式记为“跳过（环境不支持）”而不是伪造通过。
        if not production:
            record["verify_deployment"] = (
                "skipped: 平台环境 %s/%s 不是 production/prod，license-install 拒绝在该环境核验受控部署"
                % (platform_env or "未知", audit_env or "未知"))
            emit("[许可门禁] %s：受控部署核验跳过（非生产环境 %s/%s）"
                 % (application, platform_env or "未知", audit_env or "未知"))
        elif platform_api is None:
            record["verify_deployment"] = "skipped: 基础平台 API 容器未运行"
        else:
            try:
                code, output = probe.verify_deployment(platform_api, application, options.verify_timeout)
            except GateError as error:
                record["verify_deployment"] = "failed"
                fail("verify-deployment-unavailable", application,
                     "%s 的受控部署核验无法执行：%s" % (application, error))
            else:
                if code == 0:
                    record["verify_deployment"] = "passed"
                else:
                    record["verify_deployment"] = "failed"
                    fail("verify-deployment-failed", application,
                         "%s 的受控部署核验未通过（exit=%s）：%s"
                         % (application, code, truncate(output) or "无输出"))

        emit("[许可门禁] 应用 %s（%s）：容器=%s 状态=%s 健康=%s 重启=%s 许可状态卷=%s "
             "许可实例=%s 受控部署核验=%s"
             % (application, name, container.name, container.status, container.health,
                container.restart_count,
                record["license_state_volume"] or "缺失",
                "一致（前缀 %s）" % app_instance[:8] if record["instance_matches"] is True
                else ("不一致" if record["instance_matches"] is False else "未知"),
                record["verify_deployment"]))

    evidence["ok"] = not failures
    evidence["failures"] = failures
    return evidence, not failures


def report(options, emit):
    if options.dry_run:
        applications = list(options.apps)
        target = "、".join(applications) if applications else "全部已登记商业应用（按容器标签发现）"
        emit("[许可门禁] 预演（--dry-run）：不执行任何 docker/compose 命令，零副作用")
        emit("[许可门禁] 部署目录=%s Compose 项目=%s 许可状态卷目标=%s"
             % (options.deploy_dir, options.compose_project, LICENSE_STATE_TARGET))
        emit("[许可门禁] 待检查应用=%s" % target)
        emit("[许可门禁] 计划检查：")
        emit("  1) 每个应用的 API 容器存在且 running/健康")
        emit("  2) docker inspect 挂载目标含 %s（命名卷）" % LICENSE_STATE_TARGET)
        emit("  3) 容器 env %s 与平台库 license_deployment.instance_id 比对" % LICENSE_INSTANCE)
        emit("  4) docker exec <基础平台 API 容器> ./license-install verify-deployment --application <code>")
        return 0

    probe = FixtureProbe(load_fixture(options.fixture)) if options.fixture else LiveProbe()
    evidence, ok = run_checks(options, probe, emit)
    if ok:
        records = evidence["applications"]
        skipped = [item for item in records if item.get("skipped")]
        checked = len(records) - len(skipped)
        if not records:
            emit("[许可门禁] 通过：当前没有已登记的商业应用容器，无需检查许可运行态")
        else:
            message = ("[许可门禁] 通过：%d 个已登记商业应用的 API 容器运行健康、许可状态卷齐全、"
                       "许可实例一致（受控部署核验按环境结论见上）" % checked)
            if skipped:
                message += "；%d 个已显式停用的应用按停用跳过（%s）" % (
                    len(skipped), "、".join(item["application"] for item in skipped))
            emit(message)
    else:
        emit("[许可门禁] 未通过：共 %d 项问题（逐条见上）" % len(evidence["failures"]))
        emit("不要重复点击「重试」接入，也不要手工修改固定记录或关闭商业许可开关——"
             "它们都不会补上缺失的许可状态卷，也不会纠正许可实例。")
        emit("正确顺序：导入并激活许可 → 接入/受控更新 → ./bin/deploy.sh license-runtime 复核 → "
             "再宣布部署成功（见 OFFLINE_RUNBOOK.md「许可运行态门禁」）。")
    if options.json:
        json.dump(evidence, sys.stdout, ensure_ascii=False, indent=2, sort_keys=True)
        sys.stdout.write("\n")
    return 0 if ok else 1


def load_fixture(path):
    try:
        text = Path(path).read_text(encoding="utf-8")
    except OSError as error:
        raise GateError("无法读取事实集：%s" % path) from error
    try:
        return json.loads(text)
    except ValueError as error:
        raise GateError("事实集不是合法 JSON：%s" % path) from error


def parse_arguments(argv=None):
    parser = argparse.ArgumentParser(
        description="离线部署许可运行态门禁：容器健康、许可状态卷、许可实例一致、受控部署核验")
    parser.add_argument("--deploy-dir", default=str(default_deploy_dir()),
                        help="部署目录（含 docker-compose.yml/.env 的目录；默认取本脚本的上一级）")
    parser.add_argument("--compose-project", default=None,
                        help="Compose 项目名（默认取部署目录 .env 的 COMPOSE_PROJECT_NAME，"
                             "否则 %s）" % DEFAULT_COMPOSE_PROJECT)
    parser.add_argument("--apps", default="",
                        help="只检查指定应用编码（逗号或空格分隔），默认发现全部已登记商业应用")
    parser.add_argument("--json", action="store_true", help="以 JSON 输出完整证据（人类日志转到 stderr）")
    parser.add_argument("--dry-run", action="store_true", help="只打印计划，不执行任何 docker/compose 命令")
    parser.add_argument("--fixture", default=None,
                        help="用 JSON 事实集替代实时探测（反例回归；不与容器交互）")
    parser.add_argument("--verify-timeout", default="60s",
                        help="verify-deployment 等待确认的超时（默认 60s）")
    options = parser.parse_args(argv)

    deploy_dir = Path(options.deploy_dir).expanduser()
    if not deploy_dir.is_dir():
        parser.error("部署目录不存在：%s" % deploy_dir)
    options.deploy_dir = deploy_dir.resolve()
    if not options.compose_project:
        options.compose_project = compose_project_from_env(options.deploy_dir)
    if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.-]*", options.compose_project):
        parser.error("Compose 项目名不合法：%s" % options.compose_project)
    options.apps = split_apps(options.apps)
    unknown = [code for code in options.apps if code not in APPLICATIONS]
    if unknown:
        parser.error("不支持的应用编码：%s（支持：%s）"
                     % ("、".join(unknown), "、".join(APPLICATIONS)))
    if options.fixture and options.dry_run:
        parser.error("--fixture 与 --dry-run 互斥：预演不读取任何事实集")
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
        print("[许可门禁失败] %s" % error, file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
