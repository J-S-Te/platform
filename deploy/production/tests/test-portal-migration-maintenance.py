#!/usr/bin/env python3
"""执行真实 CRM/Portal 发布尾段，验证 Portal 停写迁移与前向失败保护。"""
import json
import os
from pathlib import Path
import re
import subprocess
import tempfile
import unittest

SOURCE = (Path(__file__).resolve().parents[1] / "bin/deploy-customer-opportunity.sh").read_text()
PORTAL = ["portal-api", "portal-report-worker", "portal-invite-compensation-worker"]


# 用途：提取发布器真实函数，修复前新增函数缺失时留空以复现旧行为。
# 参数：name 为待执行的函数名称。
# 返回值：函数定义文本，或不存在时为空文本。
def function(name):
    match = re.search(r"^" + name + r"\(\) \{\n.*?^\}", SOURCE, re.M | re.S)
    return match.group() if match else ""


FAKE = r'''#!/usr/bin/env python3
import json, os, pathlib, sys
root = pathlib.Path(os.environ["FIXTURE"])
args = sys.argv[1:]
state = json.loads((root / "state").read_text())
with (root / "events").open("a") as log:
    log.write(" ".join(args) + "\n")
fail = os.environ.get("FAIL_POINT", "")
if args[0] == "compose":
    command = args[1]
    services = [arg for arg in args[2:] if arg.startswith(("portal-", "customer-"))]
    point = command + ":" + ",".join(services)
    if command == "run" and services == ["portal-migrate"]:
        if any(value == "old" for key, value in state.items() if key.startswith("portal-")):
            (root / "unsafe").touch()
        (root / "ddl").touch()
    if point == fail:
        if command == "up":
            for service in services:
                if service in state:
                    state[service] = "new"
            (root / "state").write_text(json.dumps(state))
        sys.exit(23)
    if command == "stop":
        for service in services:
            state[service] = "stopped"
    elif command == "up":
        for service in services:
            if service not in state:
                continue
            version = (root / "pointer").read_text()
            if service.startswith("portal-") and (root / "ddl").exists() and version == "old":
                (root / "unsafe").touch()
            if service == "portal-api" and state["portal-report-worker"] != "new":
                (root / "api-before-worker").touch()
            state[service] = version
    elif command == "ps":
        if "-q" in args:
            print(services[0])
        else:
            print("\n".join(key for key, value in state.items() if value != "stopped"))
elif args[0] == "workers-ready":
    if fail == "health:portal-report-worker":
        sys.exit(23)
elif args[0] == "docker":
    service = args[2]
    template = " ".join(args[3:])
    if "Config.Image" in template:
        print("wrong" if fail == "image:" + service else
              "portal-new" if service.startswith("portal-") else "crm-new")
    elif "RestartCount" in template:
        print("0")
    elif "State.Running" in template:
        print("true")
    else:
        print("unhealthy" if fail == "health:" + service else "healthy")
(root / "state").write_text(json.dumps(state))
'''


class PortalMigrationMaintenanceTest(unittest.TestCase):
    # 用途：隔离执行真实发布尾段、镜像验证、回滚和退出恢复逻辑。
    # 参数：failure 为注入失败的命令或验证边界。
    # 返回值：进程结果、边界事件、容器状态和不安全事件标记。
    def rollout(self, failure=""):
        with tempfile.TemporaryDirectory(prefix="portal-maintenance-") as directory:
            root = Path(directory)
            (root / "fake").write_text(FAKE)
            (root / "fake").chmod(0o755)
            (root / "state").write_text(json.dumps(
                {service: "old" for service in PORTAL + ["customer-api", "customer-worker"]}))
            (root / "events").touch()
            (root / "pointer").write_text("new")
            helpers = "\n".join(function(name) for name in (
                "stop_portal_runtime", "verify_service_image",
                "rollback_runtime", "cleanup"))
            harness = helpers + r'''
compose() { "$FIXTURE/fake" compose "$@"; }
docker() { "$FIXTURE/fake" docker "$@"; }
sleep() { :; }
wait_for_workers() { "$FIXTURE/fake" workers-ready; }
require_backup_space() { :; }
require_mysql_ready() { :; }
diagnose_mysql_container() { :; }
backup_database_if_initialized() {
  echo "backup:$1" >> "$FIXTURE/events"
  [[ "$FAIL_POINT" != "backup:$1" ]]
}
restore_release() {
  echo restored >> "$FIXTURE/events"
  echo -n old > "$FIXTURE/pointer"
  release_updated=false
}
restore_customer_runtime() { :; }
restore_portal_runtime() { :; }
env_value_from() { echo old; }
wait_for_health() { [[ "$FAIL_POINT" != api-health ]]; }
verify_portal_proxy_runtime() { [[ "$FAIL_POINT" != proxy ]]; }
port_value() { echo 18084; }
deploy_dir="$FIXTURE"
previous_release="$FIXTURE/previous"
next_release="$FIXTURE/next"
previous_customer_runtime="$FIXTURE/customer-previous"
previous_portal_runtime="$FIXTURE/portal-previous"
touch "$previous_release"
release_file="$FIXTURE/release"
release_updated=true
release_committed=false
portal_migration_started=false
customer_runtime_updated=false
portal_runtime_updated=false
customer_worker_services=(customer-worker portal-report-worker portal-invite-compensation-worker)
portal_selected_runtime=$'portal-api\nportal-report-worker\nportal-invite-compensation-worker'
crm_image_ref=crm-new
portal_image_ref=portal-new
acr_enterprise_or_new_personal=old
acr_legacy_personal=old
offline_local_registry=old
trap cleanup EXIT INT TERM
''' + SOURCE[SOURCE.index('echo "启动 CRM/Portal 数据库与 Temporal"'):]
            process = subprocess.run(
                ["bash", "-Eeuo", "pipefail", "-c", harness], text=True,
                capture_output=True, env=dict(os.environ, FIXTURE=directory, FAIL_POINT=failure))
            return (process, (root / "events").read_text().splitlines(),
                    json.loads((root / "state").read_text()), (root / "unsafe").exists(),
                    (root / "api-before-worker").exists())

    # 用途：确保备份未完成时不停止现有 Portal 写入者，也不开始迁移。
    # 参数：无。
    # 返回值：无；备份失败破坏服务或执行迁移时断言失败。
    def test_backup_failure_keeps_portal_running(self):
        for failure in ("backup:customer-mysql", "backup:portal-mysql"):
            with self.subTest(failure=failure):
                result, events, state, unsafe, early_api = self.rollout(failure)
                self.assertNotEqual(result.returncode, 0)
                self.assertTrue(all(state[service] == "old" for service in PORTAL))
                self.assertFalse(any(event.startswith("compose stop") for event in events))
                self.assertNotIn("compose run --no-deps portal-migrate", events)

    # 用途：任一Portal写入者停止失败必须阻止DDL，不能仅依赖API停服。
    # 参数：无。
    # 返回值：无；停止失败仍执行迁移时断言失败。
    def test_stop_failure_never_starts_migration(self):
        failures = ("stop:portal-api",
                    "stop:portal-report-worker,portal-invite-compensation-worker")
        for failure in failures:
            with self.subTest(failure=failure):
                result, events, state, unsafe, early_api = self.rollout(failure)
                self.assertNotEqual(result.returncode, 0)
                self.assertNotIn("compose run --no-deps portal-migrate", events)

    # 用途：证明迁移前停止所有选中 Portal 写入者，新 Worker 核验完成后才开放 API。
    # 参数：无。
    # 返回值：无；DDL混写、API提前开放或验证顺序错误时断言失败。
    def test_stops_writers_before_ddl_and_workers_before_api(self):
        result, events, state, unsafe, early_api = self.rollout()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse(unsafe, "DDL执行期间旧Portal写入者仍在运行")
        self.assertFalse(early_api, "Portal API先于新报告Worker启动")
        stop_api = events.index("compose stop portal-api")
        migration = events.index("compose run --no-deps portal-migrate")
        stop_workers = events.index("compose stop portal-report-worker portal-invite-compensation-worker")
        api = events.index("compose up -d --force-recreate --no-deps --wait --wait-timeout 120 portal-api")
        worker_verified = next(index for index, event in enumerate(events)
                               if event.startswith("docker inspect portal-report-worker")
                               and "Config.Image" in event)
        self.assertLess(events.index("backup:portal-mysql"), stop_api)
        self.assertLess(stop_api, stop_workers)
        self.assertLess(stop_workers, migration)
        self.assertLess(worker_verified, api)

    # 用途：部分DDL或后续替换失败时保持Portal停写，禁止恢复旧指针和旧代码。
    # 参数：无。
    # 返回值：无；失败后重启旧Writer或留下运行Writer时断言失败。
    def test_post_migration_failure_holds_portal_for_forward_repair(self):
        failures = ("run:portal-migrate", "up:customer-api", "image:customer-api", "api-health",
                    "run:customer-api",
                    "up:customer-worker,portal-report-worker,portal-invite-compensation-worker",
                    "image:portal-report-worker", "health:portal-report-worker",
                    "up:portal-api", "image:portal-api", "proxy")
        for failure in failures:
            with self.subTest(failure=failure):
                result, events, state, unsafe, early_api = self.rollout(failure)
                self.assertNotEqual(result.returncode, 0)
                self.assertFalse(unsafe)
                self.assertTrue(all(state[service] == "stopped" for service in PORTAL),
                                (failure, state, result.stderr))
                self.assertNotIn("restored", events)
                self.assertIn("前向", result.stderr)


if __name__ == "__main__":
    unittest.main()
