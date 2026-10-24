#!/usr/bin/env python3
"""使用投影后的 Docker 事实回归只读旧安装清单导出。"""
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import stat
import subprocess
import sys
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[4]
EXPORTER = ROOT / "platform/deploy/production/bin/export-license-inventory.py"
WRAPPER = ROOT / "服务器一键离线部署.sh"
spec = importlib.util.spec_from_file_location("license_inventory", EXPORTER)
inventory = importlib.util.module_from_spec(spec)
spec.loader.exec_module(inventory)


class DockerFacts:
    """Docker 边界夹具，仅支持 ps 及两项投影后的检查。"""

    def __init__(self):
        self.ids = ["1" * 64, "2" * 64, "3" * 64]
        self.containers = {
            cid: dict(container_id=cid, service=service, project="uip",
                      image_digest="sha256:" + digest * 64, running=running,
                      one_off="False", started_at="2026-10-12T03:00:00Z")
            for cid, service, digest, running in zip(
                self.ids,
                ["customer-api", "customer-worker", "customer-mysql"],
                ["a", "a", "b"], [True, False, False])
        }
        self.images = {
            "sha256:" + "a" * 64: dict(id="sha256:" + "a" * 64,
                                       version=None, protocol=None,
                                       architecture="amd64", os="linux"),
            "sha256:" + "b" * 64: dict(id="sha256:" + "b" * 64,
                                       version="8.4", protocol="",
                                       architecture="amd64", os="linux"),
        }
        self.calls = []
        self.ps_count = 0
        self.inspect_count = {}
        self.on_recheck = None
        self.on_second_ps = None
        self.container_raw = {}
        self.image_raw = {}

    def __call__(self, args):
        self.calls.append(list(args))
        if args[:1] == ["ps"]:
            if args != ["ps", "--all", "--no-trunc", "--filter",
                        "label=com.docker.compose.project=uip", "--format", "{{.ID}}"]:
                raise AssertionError("inventory must enumerate the complete project")
            self.ps_count += 1
            if self.ps_count == 2 and self.on_second_ps:
                replacement = self.on_second_ps()
                if replacement is not None:
                    return replacement
            return "\n".join(reversed(self.ids)) + "\n"
        if args[:3] == ["container", "inspect", "--format"]:
            if args[3] != inventory.CONTAINER_FORMAT:
                raise AssertionError("container inspection must use the approved projection")
            cid = args[4]
            self.inspect_count[cid] = self.inspect_count.get(cid, 0) + 1
            if self.inspect_count[cid] == 2 and self.on_recheck:
                self.on_recheck(cid)
            return self.container_raw.get(cid, json.dumps(self.containers[cid]))
        if args[:3] == ["image", "inspect", "--format"]:
            if args[3] != inventory.IMAGE_FORMAT:
                raise AssertionError("image inspection must use the approved projection")
            digest = args[4]
            return self.image_raw.get(digest, json.dumps(self.images[digest]))
        raise AssertionError("unexpected or mutating Docker command: " + repr(args))


class InventoryFixture(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.base = Path(self.temp.name).resolve()
        self.target = self.base / "deployment"
        self.target.mkdir()
        self.env_secret = "private-env-password-123"
        self.release_secret = "private-release-token-456"
        (self.target / ".env").write_text(
            "COMPOSE_PROJECT_NAME=uip\nMYSQL_ROOT_PASSWORD=" + self.env_secret + "\n")
        (self.target / ".release.env").write_text(
            "CUSTOMER_VERSION=20260928-no-license\nSECRET=" + self.release_secret + "\n")
        (self.target / "docker-compose.yml").write_text(
            "services:\n  customer-api: {}\n  customer-worker: {}\n  customer-mysql: {}\n")
        self.docker = DockerFacts()

    def collect(self):
        return inventory.collect(self.target, self.docker)

    def assert_rejected(self, text=None):
        with self.assertRaises(ValueError) as caught:
            self.collect()
        if text:
            self.assertIn(text, str(caught.exception))


class CompleteInventoryTests(InventoryFixture):
    def test_collects_running_stopped_workers_and_infrastructure_with_old_unlabelled_images(self):
        result = self.collect()
        self.assertEqual(result["project"], "uip")
        self.assertEqual(result["deploy_root"], str(self.target))
        self.assertEqual(result["version"], 1)
        self.assertRegex(result["collected_at"], r"^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ$")
        self.assertEqual([c["container_id"] for c in result["containers"]], self.docker.ids)
        self.assertEqual([c["service"] for c in result["containers"]],
                         ["customer-api", "customer-worker", "customer-mysql"])
        self.assertEqual([c["running"] for c in result["containers"]], [True, False, False])
        self.assertEqual(result["containers"][0]["version"], "")
        self.assertEqual(result["containers"][0]["protocol"], "")
        self.assertEqual(result["containers"][1]["protocol"], "")
        self.assertEqual(result["containers"][2]["version"], "8.4")
        for name, field in [("docker-compose.yml", "compose_digest"),
                            (".release.env", "release_digest")]:
            expected = "sha256:" + hashlib.sha256((self.target / name).read_bytes()).hexdigest()
            self.assertEqual(result[field], expected)
        image_calls = [call for call in self.docker.calls if call[:2] == ["image", "inspect"]]
        self.assertEqual(len(image_calls), 2, "shared image must be inspected once")
        self.assertEqual(self.docker.ps_count, 2)

    def test_output_contains_only_projection_and_no_configuration_credentials(self):
        result = self.collect()
        self.assertEqual(set(result), {"version", "project", "deploy_root", "collected_at",
                                       "compose_digest", "release_digest", "containers"})
        keys = {"container_id", "service", "image_digest", "running", "one_off", "started_at",
                "version", "protocol", "architecture", "os"}
        for container in result["containers"]:
            self.assertEqual(set(container), keys)
        encoded = json.dumps(result)
        for private in (self.env_secret, self.release_secret, "MYSQL_ROOT_PASSWORD", "SECRET"):
            self.assertNotIn(private, encoded)
        for docker_format in (inventory.CONTAINER_FORMAT, inventory.IMAGE_FORMAT):
            self.assertNotIn(".Config.Env", docker_format)
            self.assertNotIn("{{json .}}", docker_format)
            self.assertNotIn(".Mounts", docker_format)

    def test_unexpected_json_fields_cannot_escape_the_export_projection(self):
        self.docker.containers[self.docker.ids[0]]["environment"] = [self.env_secret]
        self.docker.images["sha256:" + "a" * 64]["private_label"] = self.release_secret
        result = self.collect()
        encoded = json.dumps(result)
        self.assertNotIn(self.env_secret, encoded)
        self.assertNotIn(self.release_secret, encoded)
        self.assertNotIn("environment", result["containers"][0])
        self.assertNotIn("private_label", result["containers"][0])

    def test_dotenv_is_parsed_without_executing_unrelated_shell_expressions(self):
        marker = self.base / "must-not-exist"
        (self.target / ".env").write_text(
            "export COMPOSE_PROJECT_NAME='uip'\n"
            "EVIL=$(touch " + str(marker) + ")\n"
            "OTHER=`touch " + str(marker) + "`\n")
        self.assertEqual(self.collect()["project"], "uip")
        self.assertFalse(marker.exists())

    def test_shell_substitution_in_project_name_is_rejected_without_execution(self):
        marker = self.base / "must-not-exist"
        (self.target / ".env").write_text("COMPOSE_PROJECT_NAME=$(touch " + str(marker) + ")\n")
        self.assert_rejected("COMPOSE_PROJECT_NAME")
        self.assertFalse(marker.exists())
        self.assertEqual(self.docker.calls, [])

    def test_one_off_fact_remains_distinct_from_running_state(self):
        for value in ("True", "true"):
            with self.subTest(value=value):
                self.docker = DockerFacts()
                self.docker.containers[self.docker.ids[1]]["one_off"] = value
                result = self.collect()
                self.assertTrue(result["containers"][1]["one_off"])
                self.assertFalse(result["containers"][1]["running"])


class InvalidFactsTests(InventoryFixture):
    def test_empty_duplicate_invalid_or_excessive_container_membership(self):
        cases = [[], ["1" * 64, "1" * 64], ["short-id"], ["g" * 64],
                 [f"{index:064x}" for index in range(129)]]
        for ids in cases:
            with self.subTest(count=len(ids), sample=ids[:2]):
                self.docker = DockerFacts()
                self.docker.ids = ids
                self.assert_rejected("容器范围")

    def test_wrong_project_or_container_id_rejects_installation_binding(self):
        for field, value in [("project", "different"), ("container_id", "4" * 64),
                             ("project", None), ("container_id", None)]:
            with self.subTest(field=field, value=value):
                self.docker = DockerFacts()
                self.docker.containers[self.docker.ids[0]][field] = value
                self.assert_rejected("归属或 ID")

    def test_invalid_services_or_actual_image_ids(self):
        for field, value in [("service", None), ("service", ""), ("service", "../crm"),
                             ("service", "crm service"), ("service", "x" * 129),
                             ("image_digest", "customer:latest"), ("image_digest", None),
                             ("image_digest", "sha256:" + "A" * 64)]:
            with self.subTest(field=field, value=value):
                self.docker = DockerFacts()
                self.docker.containers[self.docker.ids[0]][field] = value
                self.assert_rejected("服务归属或实际镜像")

    def test_image_digest_and_linux_amd64_platform_must_match_actual_image(self):
        for field, value in [("id", "sha256:" + "c" * 64), ("id", None),
                             ("architecture", "arm64"), ("os", "windows")]:
            with self.subTest(field=field, value=value):
                self.docker = DockerFacts()
                self.docker.images["sha256:" + "a" * 64][field] = value
                self.assert_rejected("linux/amd64")

    def test_non_boolean_state_invalid_oneoff_and_invalid_labels_are_rejected(self):
        for field, value in [("running", "true"), ("running", 1),
                             ("started_at", None), ("one_off", "sometimes")]:
            with self.subTest(field=field, value=value):
                self.docker = DockerFacts()
                self.docker.containers[self.docker.ids[0]][field] = value
                self.assert_rejected()
        for field, value in [("version", ["old"]), ("protocol", {"secret": "unexpected"})]:
            with self.subTest(field=field):
                self.docker = DockerFacts()
                self.docker.images["sha256:" + "a" * 64][field] = value
                self.assert_rejected("镜像标签")

    def test_duplicate_json_fields_reject_ambiguous_container_or_image_facts(self):
        cid = self.docker.ids[0]
        original = json.dumps(self.docker.containers[cid])
        self.docker.container_raw[cid] = original[:-1] + ',"running":false}'
        self.assert_rejected("重复 JSON")
        self.docker = DockerFacts()
        digest = "sha256:" + "a" * 64
        original = json.dumps(self.docker.images[digest])
        self.docker.image_raw[digest] = original[:-1] + ',"architecture":"arm64"}'
        self.assert_rejected("重复 JSON")

    def test_valid_json_of_wrong_shape_is_rejected_as_a_fact_error(self):
        for raw in ("null", "[]", '"string"', "1"):
            for kind in ("container", "image"):
                with self.subTest(raw=raw, kind=kind):
                    self.docker = DockerFacts()
                    if kind == "container":
                        self.docker.container_raw[self.docker.ids[0]] = raw
                    else:
                        self.docker.image_raw["sha256:" + "a" * 64] = raw
                    self.assert_rejected()

    def test_missing_duplicate_or_invalid_project_declaration_stops_before_docker(self):
        for value in ["MYSQL_PASSWORD=unused\n", "COMPOSE_PROJECT_NAME=uip\nCOMPOSE_PROJECT_NAME=other\n",
                      "COMPOSE_PROJECT_NAME=../outside\n", "COMPOSE_PROJECT_NAME=\"uip';bad\"\n"]:
            with self.subTest(value=value):
                self.docker = DockerFacts()
                (self.target / ".env").write_text(value)
                self.assert_rejected("COMPOSE_PROJECT_NAME")
                self.assertEqual(self.docker.calls, [])


class ConcurrentInstallationTests(InventoryFixture):
    def test_added_or_removed_project_members_prevent_mixed_inventory(self):
        for second in (["1" * 64, "2" * 64], [*self.docker.ids, "4" * 64]):
            with self.subTest(count=len(second)):
                self.docker = DockerFacts()
                self.docker.on_second_ps = lambda values=second: "\n".join(values)
                self.assert_rejected("容器集合")

    def test_duplicate_membership_on_recheck_cannot_be_hidden_by_set_comparison(self):
        self.docker.on_second_ps = lambda: "\n".join([*self.docker.ids, self.docker.ids[0]])
        self.assert_rejected()

    def test_invalid_state_types_on_recheck_cannot_compare_equal_to_valid_state(self):
        for field, value in [("running", 0), ("one_off", "invalid")]:
            with self.subTest(field=field):
                self.docker = DockerFacts()
                second = self.docker.ids[1]
                self.docker.on_recheck = lambda cid, key=field, new=value: (
                    self.docker.containers[cid].__setitem__(key, new) if cid == second else None)
                self.assert_rejected()

    def test_container_state_service_image_restart_or_oneoff_change_is_rejected(self):
        for field, value in [("running", False), ("service", "different-api"),
                             ("image_digest", "sha256:" + "d" * 64),
                             ("started_at", "2026-10-13T03:00:00Z"),
                             ("one_off", "True"), ("project", "other")]:
            with self.subTest(field=field):
                self.docker = DockerFacts()
                first = self.docker.ids[0]
                self.docker.on_recheck = lambda cid, key=field, new=value: (
                    self.docker.containers[cid].__setitem__(key, new) if cid == first else None)
                self.assert_rejected("容器状态")

    def test_any_configuration_change_prevents_inventory_from_crossing_deployments(self):
        for name in ("docker-compose.yml", ".release.env", ".env"):
            with self.subTest(name=name):
                self.docker = DockerFacts()
                file = self.target / name
                original = file.read_bytes()
                self.docker.on_second_ps = lambda: file.write_bytes(original + b"\n# changed\n") and None
                self.assert_rejected("部署配置")
                file.write_bytes(original)


class InputAndOutputTests(InventoryFixture):
    def test_missing_oversized_or_symlink_configuration_is_rejected(self):
        for name, limit in [("docker-compose.yml", 2097152), (".release.env", 65536), (".env", 1048576)]:
            with self.subTest(name=name):
                file = self.target / name
                original = file.read_bytes()
                file.unlink()
                self.assert_rejected("普通文件")
                outside = self.base / (name.replace(".", "-") + ".outside")
                outside.write_bytes(original)
                file.symlink_to(outside)
                self.assert_rejected("普通文件")
                file.unlink()
                file.write_bytes(b"x" * (limit + 1))
                self.assert_rejected("大小上限")
                file.write_bytes(original)

    def test_deployment_symlink_or_symlink_parent_and_root_are_rejected(self):
        link = self.base / "linked"
        link.symlink_to(self.target, target_is_directory=True)
        with self.assertRaises(ValueError):
            inventory.collect(link, self.docker)
        parent_link = self.base / "linked-parent"
        parent_link.symlink_to(self.base, target_is_directory=True)
        with self.assertRaises(ValueError):
            inventory.collect(parent_link / "deployment", self.docker)
        with self.assertRaises(ValueError):
            inventory.collect("/", self.docker)
        self.assertEqual(self.docker.calls, [])

    def test_output_is_exclusive_private_and_roundtrips_projected_inventory(self):
        result = self.collect()
        output = self.base / "installation.json"
        old_umask = os.umask(0o022)
        try:
            inventory.write_inventory(output, result)
        finally:
            os.umask(old_umask)
        self.assertEqual(stat.S_IMODE(output.stat().st_mode), 0o600)
        self.assertEqual(json.loads(output.read_text()), result)
        original = output.read_bytes()
        with self.assertRaises(FileExistsError):
            inventory.write_inventory(output, {"replacement": True})
        self.assertEqual(output.read_bytes(), original)

    def test_private_output_retains_owner_read_write_under_restrictive_umask(self):
        output = self.base / "private-under-umask.json"
        old_umask = os.umask(0o777)
        try:
            inventory.write_inventory(output, {"version": 1})
        finally:
            os.umask(old_umask)
        self.assertEqual(stat.S_IMODE(output.stat().st_mode), 0o600)

    def test_output_symlink_or_parent_symlink_cannot_overwrite_other_file(self):
        victim = self.base / "victim"
        victim.write_text("keep")
        link = self.base / "output-link.json"
        link.symlink_to(victim)
        with self.assertRaises(FileExistsError):
            inventory.write_inventory(link, {"overwrite": True})
        self.assertEqual(victim.read_text(), "keep")
        linked_parent = self.base / "linked-parent"
        linked_parent.symlink_to(self.target, target_is_directory=True)
        with self.assertRaises(ValueError):
            inventory.write_inventory(linked_parent / "new.json", {})
        self.assertFalse((self.target / "new.json").exists())

    def test_missing_parent_and_oversized_inventory_leave_no_output(self):
        with self.assertRaises(ValueError):
            inventory.write_inventory(self.base / "missing" / "output.json", {})
        output = self.base / "too-large.json"
        with self.assertRaises(ValueError):
            inventory.write_inventory(output, {"large": "x" * 262144})
        self.assertFalse(output.exists())


class CommandLineFixture(InventoryFixture):
    def setUp(self):
        super().setUp()
        self.bin_dir = self.base / "bin"
        self.bin_dir.mkdir()
        self.metadata = self.base / "facts.json"
        self.metadata.write_text(json.dumps({"ids": self.docker.ids,
                                             "containers": self.docker.containers,
                                             "images": self.docker.images}))
        fake = self.bin_dir / "docker"
        fake.write_text("#!" + sys.executable + "\n" +
                        "import json, os, sys\n"
                        "from pathlib import Path\n"
                        "facts=json.loads(Path(os.environ['INVENTORY_FIXTURE']).read_text())\n"
                        "args=sys.argv[1:]\n"
                        "with open(os.environ['INVENTORY_CALL_LOG'], 'a') as log: log.write(json.dumps(args)+'\\n')\n"
                        "if args[0]=='ps': print('\\n'.join(facts['ids']))\n"
                        "elif args[:2]==['container','inspect']: print(json.dumps(facts['containers'][args[-1]]))\n"
                        "elif args[:2]==['image','inspect']: print(json.dumps(facts['images'][args[-1]]))\n"
                        "else: sys.exit(90)\n")
        fake.chmod(0o700)
        self.log = self.base / "docker-calls.jsonl"
        self.env = dict(os.environ, PATH=str(self.bin_dir) + os.pathsep + os.environ.get("PATH", ""),
                        INVENTORY_FIXTURE=str(self.metadata), INVENTORY_CALL_LOG=str(self.log),
                        PYTHONDONTWRITEBYTECODE="1")


class CommandLineTests(CommandLineFixture):
    def test_cli_exports_full_inventory_without_changing_deployment(self):
        output = self.base / "cli-inventory.json"
        before = {p.name: p.read_bytes() for p in self.target.iterdir()}
        result = subprocess.run([sys.executable, str(EXPORTER), "--target", str(self.target),
                                 "--output", str(output)], env=self.env, text=True,
                                capture_output=True, timeout=20)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("尚未批准或冻结迁移", result.stdout)
        self.assertEqual(stat.S_IMODE(output.stat().st_mode), 0o600)
        self.assertEqual(len(json.loads(output.read_text())["containers"]), 3)
        self.assertEqual(before, {p.name: p.read_bytes() for p in self.target.iterdir()})
        calls = [json.loads(line) for line in self.log.read_text().splitlines()]
        self.assertTrue(all(call[:1] == ["ps"] or call[:2] in
                            (["container", "inspect"], ["image", "inspect"]) for call in calls))
        self.assertNotIn(self.env_secret, result.stdout + result.stderr)

    def test_cli_failure_does_not_create_or_replace_output(self):
        (self.target / ".env").write_text("COMPOSE_PROJECT_NAME=$(echo unsafe)\n")
        output = self.base / "not-created.json"
        result = subprocess.run([sys.executable, str(EXPORTER), "--target", str(self.target),
                                 "--output", str(output)], env=self.env, text=True,
                                capture_output=True, timeout=20)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("[错误]", result.stderr)
        self.assertFalse(output.exists())
        self.assertFalse(self.log.exists())

    def test_docker_oversized_projection_is_bounded_without_exposing_it(self):
        self.docker.images["sha256:" + "a" * 64]["version"] = self.env_secret + "x" * 262144
        self.metadata.write_text(json.dumps({"ids": self.docker.ids,
                                             "containers": self.docker.containers,
                                             "images": self.docker.images}))
        output = self.base / "oversized-not-created.json"
        result = subprocess.run([sys.executable, str(EXPORTER), "--target", str(self.target),
                                 "--output", str(output)], env=self.env, text=True,
                                capture_output=True, timeout=20)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("超过上限", result.stderr)
        self.assertNotIn(self.env_secret, result.stderr + result.stdout)
        self.assertFalse(output.exists())

    def test_docker_failure_is_reported_without_stderr_credentials_or_partial_output(self):
        fake = self.bin_dir / "docker"
        fake.write_text("#!/bin/sh\necho '" + self.env_secret + "' >&2\nexit 55\n")
        fake.chmod(0o700)
        output = self.base / "docker-error.json"
        result = subprocess.run([sys.executable, str(EXPORTER), "--target", str(self.target),
                                 "--output", str(output)], env=self.env, text=True,
                                capture_output=True, timeout=20)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("Docker 只读检查失败", result.stderr)
        self.assertNotIn(self.env_secret, result.stderr + result.stdout)
        self.assertFalse(output.exists())


class ServerWrapperTests(CommandLineFixture):
    """通过显式模拟 uname，在任意宿主机上验证 Linux 分派逻辑。"""

    def setUp(self):
        super().setUp()
        uname = self.bin_dir / "uname"
        uname.write_text("#!/bin/sh\nif [ \"$1\" = -s ]; then echo Linux; else echo x86_64; fi\n")
        uname.chmod(0o700)

    def run_wrapper(self, wrapper=WRAPPER, extra=(), output=None):
        if output is None:
            output = self.base / "wrapper-inventory.json"
        return subprocess.run(["bash", str(wrapper), "--target", str(self.target),
                               "--export-license-inventory", str(output), *extra],
                              env=self.env, text=True, capture_output=True, timeout=20)

    def test_export_entry_works_without_media_root_or_installation_actions(self):
        before = {p.name: p.read_bytes() for p in self.target.iterdir()}
        result = self.run_wrapper()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("旧安装完整运行信息已导出", result.stdout)
        self.assertNotIn("校验交付介质", result.stdout)
        self.assertEqual(before, {p.name: p.read_bytes() for p in self.target.iterdir()})
        self.assertEqual(len(json.loads((self.base / "wrapper-inventory.json").read_text())["containers"]), 3)

    def test_export_entry_rejects_all_install_or_activation_options_before_docker(self):
        options = [("--media", "/missing/media"), ("--upgrade",), ("--install",),
                   ("--check-only",), ("--skip-host-deps",), ("--skip-configure",),
                   ("--yes",), ("--apply-compose",), ("--add-subsystems", "customer-opportunity"),
                   ("--add-components", "customer-opportunity"),
                   ("--upgrade-subsystems", "customer-opportunity"), ("--customer", "gays-sq"),
                   ("--license", "/missing/commercial-license.jws"),
                   ("--legacy-license-bridge", "gays-sq"), ("--legacy-license-upgrade", "gays-sq")]
        for extra in options:
            with self.subTest(extra=extra):
                result = self.run_wrapper(extra=extra)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("旧安装导出只能与 --target 配合", result.stderr)
                self.assertFalse(self.log.exists(), "conflict must fail before Docker inspection")
                self.assertFalse((self.base / "wrapper-inventory.json").exists())

    def test_duplicate_export_option_is_rejected_before_docker(self):
        result = self.run_wrapper(extra=("--export-license-inventory", str(self.base / "second.json")))
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("不能重复指定", result.stderr)
        self.assertFalse(self.log.exists())

    def test_standalone_uploaded_wrapper_finds_exporter_beside_it(self):
        standalone = self.base / "upload"
        standalone.mkdir()
        wrapper = standalone / WRAPPER.name
        wrapper.write_bytes(WRAPPER.read_bytes())
        (standalone / EXPORTER.name).write_bytes(EXPORTER.read_bytes())
        result = self.run_wrapper(wrapper)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertTrue((self.base / "wrapper-inventory.json").is_file())

    def test_standalone_wrapper_can_use_installed_target_exporter(self):
        standalone = self.base / "upload"
        standalone.mkdir()
        wrapper = standalone / WRAPPER.name
        wrapper.write_bytes(WRAPPER.read_bytes())
        target_bin = self.target / "bin"
        target_bin.mkdir()
        (target_bin / EXPORTER.name).write_bytes(EXPORTER.read_bytes())
        result = self.run_wrapper(wrapper)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_missing_or_symlink_exporter_does_not_fall_through_to_upgrade(self):
        standalone = self.base / "upload"
        standalone.mkdir()
        wrapper = standalone / WRAPPER.name
        wrapper.write_bytes(WRAPPER.read_bytes())
        for symlink in (False, True):
            with self.subTest(symlink=symlink):
                if symlink:
                    (standalone / EXPORTER.name).symlink_to(EXPORTER)
                result = self.run_wrapper(wrapper)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("新版 export-license-inventory.py", result.stderr)
                self.assertFalse(self.log.exists())
                self.assertFalse((self.base / "wrapper-inventory.json").exists())

    def test_non_linux_host_is_rejected_without_inspecting_docker(self):
        uname = self.bin_dir / "uname"
        uname.write_text("#!/bin/sh\necho Darwin\n")
        result = self.run_wrapper()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("目标 Linux 服务器", result.stderr)
        self.assertFalse(self.log.exists())


if __name__ == "__main__":
    unittest.main()
