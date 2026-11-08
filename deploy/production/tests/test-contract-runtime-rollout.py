#!/usr/bin/env python3
"""执行真实发布函数，隔离验证合同 API/Worker 的维护窗口及失败保护。"""
import os
from pathlib import Path
import re
import subprocess
import tempfile
import unittest

SCRIPT = (Path(__file__).resolve().parents[1] / "bin/deploy-service.sh").read_text()


# 用途：提取脚本中的真实函数供隔离执行。
# 参数：name 为发布函数名。
# 返回值：函数定义；不存在时返回空字符串，兼容修复前的回归复现。
def function(name):
    match = re.search(r"^" + name + r"\(\) \{\n.*?^\}", SCRIPT, re.M | re.S)
    return match.group() if match else ""


FAKE = r'''#!/usr/bin/env python3
import json, os, pathlib, sys
root = pathlib.Path(os.environ["FIXTURE"])
args = sys.argv[1:]
state = json.loads((root / "state").read_text())
with (root / "events").open("a") as log:
    log.write(" ".join(args) + "\n")
point = ""
if args[0] == "compose":
    command = args[1]
    services = [arg for arg in args[2:] if arg.startswith("contract-")]
    if command in ("stop", "up"):
        point = command + ":" + ",".join(services)
    elif command == "run":
        point = "migrate"
    if point and point == os.environ.get("FAIL_POINT"):
        sys.exit(23)
    if command == "stop":
        for service in services:
            state[service] = "stopped"
    if command == "up":
        for service in services:
            if service == "contract-api" and state["contract-worker"] == "old":
                (root / "mixed").touch()
            if service in state:
                state[service] = os.environ["IMAGE"]
    if command == "ps":
        print(services[0])
elif args[0] == "docker":
    service = args[-1] if args[-1].startswith("contract-") else args[2]
    if "Image" in " ".join(args):
        print("wrong" if os.environ.get("FAIL_POINT") == "image:" + service else state[service])
    else:
        healthy = os.environ.get("FAIL_POINT") != "health:" + service
        print("running healthy 0" if healthy else "running unhealthy 0")
(root / "state").write_text(json.dumps(state))
'''


class ContractRolloutTest(unittest.TestCase):
    # 用途：运行真实合同发布或回滚函数并收集隔离容器边界调用。
    # 参数：failure 为失败边界；rollback 选择回滚；services 为选中运行组件。
    # 返回值：退出码、事件列表及是否出现新 API 与旧 Worker 混跑。
    def run_rollout(self, failure="", rollback=False,
                    services="contract-api\ncontract-worker"):
        with tempfile.TemporaryDirectory(prefix="contract-rollout-") as directory:
            root = Path(directory)
            (root / "fake").write_text(FAKE)
            (root / "fake").chmod(0o755)
            (root / "state").write_text(
                '{"contract-api":"old","contract-worker":"old"}')
            (root / "events").touch()
            names = ("replace_contract_runtime", "deploy_contract", "rollback_runtime",
                     "verify_service_image", "verify_service_stable")
            harness = "\n".join(function(name) for name in names) + r'''
compose() { "$FIXTURE/fake" compose "$@"; }
docker() { "$FIXTURE/fake" docker "$@"; }
scope_runtime_services() { printf '%s\n' "$SERVICES"; }
require_contract_runtime_value() { :; }
contract_env_value() { [[ "$1" != PLATFORM_AUTHORIZATION_CATALOG_SYNC_ENABLED ]] || echo true; }
ensure_temporal_application_network() { :; }
require_mysql_ready() { :; }
backup_database_if_initialized() {
  echo backup >> "$FIXTURE/events"
  [[ "$FAIL_POINT" != backup ]]
}
wait_for_health() { echo api-health >> "$FIXTURE/events"; [[ "$FAIL_POINT" != api-health ]]; }
port_value() { echo 18081; }
audit_contract_number_registry() { echo audit >> "$FIXTURE/events"; }
release_image_value() { echo "$IMAGE"; }
rollback_ref_is_immutable() { return 0; }
service=contract
image_ref="$IMAGE"
if [[ "$ROLLBACK" == true ]]; then command=rollback_runtime; else command=deploy_contract; fi
# 与真实脚本的 if 调用一致，不能让 errexit 掩盖缺失的显式失败传播。
if "$command"; then exit 0; else exit "$?"; fi
'''
            process = subprocess.run(
                ["bash", "-Eeuo", "pipefail", "-c", harness], capture_output=True,
                text=True, env=dict(os.environ, FIXTURE=directory, SERVICES=services,
                                    FAIL_POINT=failure, ROLLBACK=str(rollback).lower(),
                                    IMAGE="registry.example/contract@sha256:" + "a" * 64,
                                    DEPLOY_STABILITY_WINDOW_SECONDS="0"))
            events = (root / "events").read_text().splitlines()
            if not failure:
                self.assertEqual(process.returncode, 0, process.stderr)
            return process.returncode, events, (root / "mixed").exists()

    # 用途：证明备份/迁移完成后按 API 停止、Worker 停止/验证、API 启动顺序发布。
    # 参数：无。
    # 返回值：无；混跑或顺序错误时断言失败。
    def test_worker_is_ready_before_api_starts(self):
        code, events, mixed = self.run_rollout()
        self.assertFalse(mixed, "新 API 曾在旧 Worker 仍运行时启动")
        stop_api = events.index("compose stop contract-api")
        stop_worker = events.index("compose stop contract-worker")
        up_worker = events.index("compose up -d --force-recreate --no-deps --wait --wait-timeout 120 contract-worker")
        up_api = events.index("compose up -d --force-recreate --no-deps --wait --wait-timeout 120 contract-api")
        worker_verify = next(index for index, event in enumerate(events)
                             if "docker inspect" in event and "RestartCount" in event
                             and event.endswith("contract-worker"))
        self.assertLess(events.index("backup"), events.index("compose run --rm --no-deps contract-migrate"))
        self.assertLess(events.index("compose run --rm --no-deps contract-migrate"), stop_api)
        self.assertLess(stop_api, stop_worker)
        self.assertLess(stop_worker, up_worker)
        self.assertLess(up_worker, worker_verify)
        self.assertLess(worker_verify, up_api)
        self.assertIn("api-health", events)
        self.assertIn("audit", events)

    # 用途：阻止备份、迁移、停服或 Worker 启动验证失败后继续开放 API。
    # 参数：无。
    # 返回值：无；任一失败边界继续启动 API 时断言失败。
    def test_failure_never_opens_candidate_api(self):
        for failure in ("backup", "migrate", "stop:contract-api", "stop:contract-worker",
                        "up:contract-worker", "image:contract-worker", "health:contract-worker"):
            with self.subTest(failure=failure):
                code, events, mixed = self.run_rollout(failure)
                self.assertNotEqual(code, 0)
                self.assertFalse(mixed)
                self.assertFalse(any("force-recreate" in event and event.endswith("contract-api")
                                     for event in events))
                if failure in ("backup", "migrate"):
                    self.assertFalse(any(event.startswith("compose stop") for event in events))

    # 用途：回滚同样避免 API 与旧 Worker 混跑，并保留失败保护。
    # 参数：无。
    # 返回值：无；回滚顺序或失败传播不满足约束时断言失败。
    def test_rollback_uses_same_maintenance_window(self):
        code, events, mixed = self.run_rollout(rollback=True)
        self.assertFalse(mixed)
        self.assertIn("compose stop contract-api", events)
        code, events, mixed = self.run_rollout("up:contract-worker", rollback=True)
        self.assertNotEqual(code, 0)
        self.assertFalse(any("force-recreate" in event and event.endswith("contract-api")
                             for event in events))

    # 用途：维护窗口严格保留受控选择的运行组件集合。
    # 参数：无。
    # 返回值：无；未选组件被启动或停止时断言失败。
    def test_selection_does_not_add_unselected_worker(self):
        code, events, mixed = self.run_rollout(services="contract-api")
        self.assertFalse(any(event.startswith("compose") and event.endswith("contract-worker")
                             for event in events))

    # 用途：API 启动或核验失败必须保持发布失败，不能继续编号巡检冒充完成。
    # 参数：无。
    # 返回值：无；API 边界失败未传播时断言失败。
    def test_api_failure_does_not_report_success(self):
        for failure in ("up:contract-api", "image:contract-api", "health:contract-api",
                        "api-health"):
            with self.subTest(failure=failure):
                code, events, mixed = self.run_rollout(failure)
                self.assertNotEqual(code, 0)
                self.assertFalse(mixed)
                self.assertNotIn("audit", events)

    # 用途：选中集合顺序变化及仅 Worker 发布均不能提前开放 API。
    # 参数：无。
    # 返回值：无；顺序变化出现混跑或单 Worker 发布变动 API 时断言失败。
    def test_selection_order_and_worker_only(self):
        code, events, mixed = self.run_rollout(services="contract-worker\ncontract-api")
        self.assertFalse(mixed)
        self.assertLess(events.index("compose stop contract-api"),
                        events.index("compose stop contract-worker"))
        code, events, mixed = self.run_rollout(services="contract-worker")
        self.assertFalse(any(event.startswith("compose") and event.endswith("contract-api")
                             for event in events))


if __name__ == "__main__":
    unittest.main()
