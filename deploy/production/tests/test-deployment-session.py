#!/usr/bin/env python3
"""回归部署进度命令行，不使用 Docker、服务器、密钥或数据库。"""
from concurrent.futures import ThreadPoolExecutor
import copy
import importlib.util
import json
import os
from pathlib import Path
import stat
import subprocess
import sys
import tempfile
from types import SimpleNamespace
import unittest
from unittest import mock


HELPER = Path(__file__).resolve().parents[1] / "bin/deployment-session.py"
SPEC = importlib.util.spec_from_file_location("uip_deployment_session", HELPER)
MODULE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(MODULE)


def save(path, value):
    path.write_text(json.dumps(value, ensure_ascii=False))
    path.chmod(0o600)


class DeploymentSessionTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="uip-session-test-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name).resolve()
        self.target = self.root / "target"
        self.media = self.root / "media"
        self.media.mkdir()
        self.state = Path(str(self.target) + ".installation-state")
        self.history = Path(str(self.target) + ".installation-history")
        self.input = self.root / "identity.json"
        self.identity = {"version": 1, "target": str(self.target), "media": str(self.media),
                         "delivery_id": "sha256:" + "a" * 64, "mode": "install",
                         "components": ["common", "contract", "frontend", "platform"],
                         "options": {"skip_host_deps": False, "skip_configure": False,
                                     "apply_compose": False, "legacy_license_mode": "",
                                     "customer": "", "license_file_path": "", "license_file_sha256": ""}}
        save(self.input, self.identity)
        self.order = "media-check,assets,verify"

    def cli(self, *args, code=0):
        result = subprocess.run([sys.executable, "-B", str(HELPER), *map(str, args)],
                                stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, timeout=15)
        self.assertEqual(result.returncode, code, result.stderr)
        return result

    def begin(self, resume=False, code=0, stages=None, new_session=False):
        args = ["begin", "--state-dir", self.state, "--identity-file", self.input,
                "--stages", self.order if stages is None else stages]
        if resume:
            args.append("--resume")
        if new_session:
            args.append("--new-session")
        return self.cli(*args, code=code).stdout.strip()

    def update(self, session, stage, status, reason=None, code=0):
        args = ["update", "--state-dir", self.state, "--session", session,
                "--stage", stage, "--status", status]
        if reason is not None:
            args.extend(["--reason-code", reason])
        return self.cli(*args, code=code)

    def status(self):
        return json.loads(self.cli("status", "--state-dir", self.state, "--format", "json").stdout)

    def finish(self, session, stage, skipped=False):
        self.update(session, stage, "RUNNING")
        self.update(session, stage, "SKIPPED" if skipped else "DONE")

    def test_new_private_state_and_stdout_only_session_id(self):
        session = self.begin()
        self.assertRegex(session, r"^[a-f0-9]{32}$")
        self.assertFalse(self.target.exists())
        self.assertEqual(stat.S_IMODE(self.state.stat().st_mode), 0o700)
        for name in ("session.json", ".lock"):
            self.assertEqual(stat.S_IMODE((self.state / name).stat().st_mode), 0o600)
        value = self.status()
        self.assertEqual(value["identity"], self.identity)
        self.assertEqual(value["overall_status"], "PENDING")
        self.assertEqual(value["current_stage"], "media-check")
        self.assertEqual(value["next_action"], "RUN_STAGE")
        self.begin(code=4)
        self.assertEqual(self.status()["session_id"], session)

    def test_order_and_session_cannot_be_bypassed(self):
        session = self.begin()
        self.update("b" * 32, "media-check", "RUNNING", code=5)
        self.update(session, "assets", "RUNNING", code=5)
        self.update(session, "media-check", "DONE", code=5)
        self.update(session, "not-in-plan", "RUNNING", code=5)
        self.update(session, "media-check", "RUNNING")
        self.update(session, "media-check", "RUNNING")
        self.update(session, "media-check", "DONE")
        self.update(session, "media-check", "DONE")
        value = self.status()
        self.assertEqual(value["cursor"], 1)
        self.assertEqual(value["stages"][0]["attempts"], 1)
        self.assertEqual(len(value["history"]), 3)
        self.update(session, "media-check", "RUNNING", code=5)

    def test_root_reason_codes_and_completed_query(self):
        session = self.begin()
        self.update(session, "media-check", "RUNNING", "NONE")
        self.update(session, "media-check", "DONE", "NONE")
        for reason in ("COMMAND_FAILED", "WAITING_AUTHORIZATION", "WAITING_PLATFORM_ADOPTION",
                       "WAITING_CONTROLLED_UPDATE", "WAITING_MIGRATION", "RUNTIME_PROOF_FAILED",
                       "ASSETS_RECOVERY_REQUIRED", "COMPOSE_REVIEW_REQUIRED"):
            self.update(session, "assets", "RUNNING", "NONE")
            self.update(session, "assets", "WAITING", reason)
            self.assertEqual(self.status()["stages"][1]["reason_code"], reason)
            human = self.cli("status", "--state-dir", self.state).stdout
            self.assertIn("当前等待原因：" + reason, human)
            if reason == "WAITING_AUTHORIZATION":
                for location in ("厂商电脑", "基础平台管理端", "服务器"):
                    self.assertIn(location, human)
            self.begin(resume=True)
            self.finish(session, "media-check")
        self.finish(session, "assets")
        self.finish(session, "verify")
        result = self.cli("status", "--state-dir", self.state)
        self.assertIn("阶段记录已完成", result.stdout)
        self.assertEqual(self.status()["current_stage"], "")
        self.assertEqual(self.status()["completed_stages"], self.order.split(","))
        self.cli("status", "--state-dir", self.state, "--for", "json", code=2)

    def test_stage_plan_upper_bound(self):
        order = ["phase_" + str(index) for index in range(128)]
        self.begin(stages=",".join(order))
        self.assertEqual(self.status()["stage_order"], order)

    def test_optional_external_license_hash_legacy_default_and_changed_content(self):
        legacy = copy.deepcopy(self.identity)
        del legacy["options"]["license_file_sha256"]
        save(self.input, legacy)
        session = self.begin()
        save(self.input, self.identity)
        self.assertEqual(self.begin(resume=True), session)
        self.assertNotIn("license_file_sha256", self.status()["identity"]["options"])
        changed = copy.deepcopy(self.identity)
        changed["options"]["license_file_path"] = str(self.root / "commercial-license.jws")
        changed["options"]["license_file_sha256"] = "a" * 64
        save(self.input, changed)
        self.begin(resume=True, code=4)
        for bad in ("SHA256:" + "a" * 64, "a" * 63, "g" * 64, True, None):
            changed["options"]["license_file_sha256"] = bad
            save(self.input, changed)
            self.begin(resume=True, code=2)

    def test_same_external_license_path_changed_digest_cannot_resume(self):
        self.identity["options"].update(license_file_path=str(self.root / "commercial-license.jws"),
                                        license_file_sha256="a" * 64)
        save(self.input, self.identity)
        session = self.begin()
        self.assertEqual(self.begin(resume=True), session)
        before = (self.state / "session.json").read_bytes()
        self.identity["options"]["license_file_sha256"] = "b" * 64
        save(self.input, self.identity)
        self.begin(resume=True, code=4)
        self.assertEqual((self.state / "session.json").read_bytes(), before)

    def test_status_is_read_only_when_existing_directory_lacks_lock(self):
        session = self.begin()
        lock = self.state / ".lock"
        lock.unlink()
        before = {path.name: path.read_bytes() for path in self.state.iterdir()}
        self.cli("status", "--state-dir", self.state, code=2)
        self.update(session, "media-check", "RUNNING", code=2)
        self.begin(resume=True, code=2)
        self.assertFalse(lock.exists())
        self.assertEqual({path.name: path.read_bytes() for path in self.state.iterdir()}, before)

    def test_absent_state_status_still_checks_direct_parent(self):
        self.root.chmod(0o770)
        self.cli("status", "--state-dir", self.state, code=2)
        self.assertFalse(self.state.exists())
        self.root.chmod(0o700)
        self.cli("status", "--state-dir", self.state, code=3)

    def test_identity_private_directory_directly_under_sticky_tmp_is_safe(self):
        with tempfile.TemporaryDirectory(prefix="uip-session-shallow-", dir="/tmp") as name:
            private = Path(name).resolve()
            private.chmod(0o700)
            identity = copy.deepcopy(self.identity)
            identity.update(target=str(private / "target"), media=str(private / "media"))
            (private / "media").mkdir(mode=0o700)
            input_path = private / "identity.json"
            save(input_path, identity)
            state = private / "target.installation-state"
            # 身份文件的直接父目录必须私有，不能只依靠带粘滞位的祖父目录；Linux 通常直接在 /tmp 下创建 mktemp -d 目录。
            self.assertEqual(MODULE.read_identity(str(input_path)), identity)
            result = self.cli("begin", "--state-dir", state, "--identity-file", input_path,
                              "--stages", self.order)
            self.assertRegex(result.stdout.strip(), r"^[a-f0-9]{32}$")
            self.cli("status", "--state-dir", state)

    def test_state_directly_under_sticky_tmp_still_rejected_without_creation(self):
        sticky = Path("/tmp").resolve()
        target = sticky / (self.root.name + "-untrusted-parent")
        state = Path(str(target) + ".installation-state")
        identity = copy.deepcopy(self.identity)
        identity["target"] = str(target)
        save(self.input, identity)
        self.cli("begin", "--state-dir", state, "--identity-file", self.input,
                 "--stages", self.order, code=2)
        self.cli("status", "--state-dir", state, code=2)
        self.assertFalse(state.exists())

    def complete(self):
        session = self.begin()
        for stage in self.order.split(","):
            self.finish(session, stage)
        return session

    def next_identity(self):
        value = copy.deepcopy(self.identity)
        value.update(mode="upgrade", delivery_id="sha256:" + "b" * 64)
        value["options"]["apply_compose"] = True
        save(self.input, value)
        return value

    def test_explicit_new_session_archives_complete_batch_and_binds_new_identity(self):
        session = self.complete()
        old = self.status()
        original = json.loads((self.state / "session.json").read_text())
        next_identity = self.next_identity()
        self.begin(code=4)
        self.begin(resume=True, code=4)
        next_session = self.begin(new_session=True, stages="packages,platform,controlled")
        self.assertNotEqual(next_session, session)
        archived = self.history / (session + ".json")
        self.assertEqual(json.loads(archived.read_text()), original)
        self.assertEqual(stat.S_IMODE(self.history.stat().st_mode), 0o700)
        self.assertEqual(stat.S_IMODE(archived.stat().st_mode), 0o600)
        self.assertEqual(archived.stat().st_nlink, 1)
        active = self.status()
        self.assertEqual(active["session_id"], next_session)
        self.assertEqual(active["identity"], next_identity)
        self.assertEqual(active["generation"], 1)
        self.assertEqual(active["overall_status"], "PENDING")
        self.assertEqual(active["current_stage"], "packages")
        self.assertEqual(old["overall_status"], "COMPLETED")
        self.begin(new_session=True, stages="packages,platform,controlled", code=4)

    def test_new_session_requires_existing_complete_batch_and_cannot_mix_resume(self):
        self.begin(new_session=True, code=3)
        self.assertFalse(self.state.exists())
        session = self.begin()
        self.begin(resume=True, new_session=True, code=2)
        for status in ("PENDING", "RUNNING", "WAITING", "FAILED"):
            if status == "RUNNING":
                self.update(session, "media-check", status)
            elif status in ("WAITING", "FAILED"):
                if status == "FAILED":
                    self.begin(resume=True)
                    self.update(session, "media-check", "RUNNING")
                self.update(session, "media-check", status)
            before = (self.state / "session.json").read_bytes()
            self.begin(new_session=True, code=4)
            self.assertEqual((self.state / "session.json").read_bytes(), before)
            self.assertFalse(self.history.exists())

    def test_new_session_archive_collision_never_overwrites_existing_record(self):
        session = self.complete()
        old = json.loads((self.state / "session.json").read_text())
        self.history.mkdir(mode=0o700)
        other = copy.deepcopy(old)
        other["session_id"] = "b" * 32 if session != "b" * 32 else "c" * 32
        other["checksum"] = MODULE.checksum({key: value for key, value in other.items() if key != "checksum"})
        archived = self.history / (session + ".json")
        save(archived, other)
        before = archived.read_bytes()
        self.next_identity()
        self.begin(new_session=True, code=4)
        self.assertEqual(archived.read_bytes(), before)
        self.assertEqual(json.loads((self.state / "session.json").read_text()), old)

    def test_new_session_archive_then_interrupted_new_commit_is_retryable(self):
        session = self.complete()
        before = (self.state / "session.json").read_bytes()
        self.next_identity()
        with mock.patch.object(MODULE.os, "replace", side_effect=OSError("synthetic interruption after immutable archive")):
            with self.assertRaises(OSError):
                MODULE.begin(str(self.state), str(self.input), self.order, False, True)
        archived = self.history / (session + ".json")
        self.assertEqual((self.state / "session.json").read_bytes(), before)
        self.assertEqual(archived.read_bytes(), before)
        before_archive = archived.stat()
        next_session = self.begin(new_session=True)
        self.assertNotEqual(next_session, session)
        self.assertEqual(archived.stat().st_ino, before_archive.st_ino)
        self.assertEqual(archived.read_bytes(), before)

    def test_new_session_recovers_only_atomic_archive_link_residue(self):
        session = self.complete()
        before = (self.state / "session.json").read_bytes()
        self.history.mkdir(mode=0o700)
        residue = self.history / (".session-" + "d" * 32 + ".tmp")
        residue.write_bytes(before)
        residue.chmod(0o600)
        archived = self.history / (session + ".json")
        os.link(residue, archived)
        self.assertEqual(archived.stat().st_nlink, 2)
        partial = self.history / (".session-" + "e" * 32 + ".tmp")
        partial.write_text("partial crash bytes must never be promoted")
        partial.chmod(0o600)
        self.next_identity()
        self.begin(new_session=True)
        self.assertFalse(residue.exists())
        self.assertEqual(archived.stat().st_nlink, 1)
        self.assertEqual(archived.read_bytes(), before)
        self.assertTrue(partial.exists())

    def test_new_session_rejects_unsafe_history_and_preserves_current_record(self):
        session = self.complete()
        before = (self.state / "session.json").read_bytes()
        self.next_identity()
        self.history.mkdir(mode=0o755)
        self.begin(new_session=True, code=2)
        self.history.chmod(0o700)
        unknown = self.history / "unknown"
        save(unknown, {})
        self.begin(new_session=True, code=2)
        unknown.unlink()
        external = self.root / "outside.json"
        external.write_bytes(before)
        external.chmod(0o600)
        archived = self.history / (session + ".json")
        os.link(external, archived)
        self.begin(new_session=True, code=2)
        self.assertTrue(external.exists())
        archived.unlink()
        archived.symlink_to(external)
        self.begin(new_session=True, code=2)
        archived.unlink()
        self.history.rmdir()
        self.history.symlink_to(self.media, target_is_directory=True)
        self.begin(new_session=True, code=2)
        self.assertEqual((self.state / "session.json").read_bytes(), before)
        self.assertEqual(list(self.media.iterdir()), [])

    def test_concurrent_new_session_keeps_exactly_one_old_archive_and_one_new_batch(self):
        session = self.complete()
        self.next_identity()
        command = [sys.executable, "-B", str(HELPER), "begin", "--state-dir", str(self.state),
                   "--identity-file", str(self.input), "--stages", self.order, "--new-session"]
        with ThreadPoolExecutor(max_workers=8) as pool:
            results = list(pool.map(lambda _: subprocess.run(command, capture_output=True, text=True, timeout=15), range(8)))
        successes = [result for result in results if result.returncode == 0]
        self.assertEqual(len(successes), 1)
        self.assertTrue(all(result.returncode in (0, 4) for result in results))
        self.assertEqual(self.status()["session_id"], successes[0].stdout.strip())
        self.assertEqual([path.name for path in self.history.iterdir()], [session + ".json"])

    def test_human_stage_labels_keep_json_stage_keys(self):
        keys = ["host-deps", "assets", "packages", "configure", "contract-import", "platform", "frontend",
                "project-prepare", "project-stage", "license-prepare", "license", "verify", "controlled"]
        self.begin(stages=",".join(keys))
        value = self.status()
        self.assertEqual(value["stage_order"], keys)
        human = self.cli("status", "--state-dir", self.state).stdout
        for text in ("host-deps（宿主依赖）", "assets（部署资产）", "packages（镜像包）", "configure（运行配置）",
                     "contract-import（镜像导入）", "platform（平台发布）", "frontend（前端发布）",
                     "project-prepare（候选准备）", "project-stage（受控暂存）", "license-prepare（授权安装准备）",
                     "license（授权处理）", "verify（基础验收）", "controlled（业务及授权验收）"):
            self.assertIn(text, human)

    def test_failed_resume_reverifies_previous_done_and_preserves_history(self):
        session = self.begin()
        self.finish(session, "media-check")
        self.update(session, "assets", "RUNNING")
        self.update(session, "assets", "FAILED", "ASSETS_REJECTED")
        failure = self.status()["last_failure"]
        self.update(session, "assets", "RUNNING", code=5)
        self.assertEqual(self.begin(resume=True), session)
        resumed = self.status()
        self.assertEqual(resumed["generation"], 2)
        self.assertEqual(resumed["stages"][0]["status"], "DONE")
        self.assertEqual(resumed["stages"][0]["generation"], 1)
        self.assertEqual(resumed["historical_completed_stages"], ["media-check"])
        self.assertEqual(resumed["last_failure"], failure)
        self.update(session, "assets", "RUNNING", code=5)
        self.update(session, "media-check", "RUNNING")
        self.assertEqual(self.status()["stages"][0]["previous_status"], "DONE")
        self.update(session, "media-check", "DONE")
        self.finish(session, "assets")
        self.finish(session, "verify")
        final = self.status()
        self.assertEqual(final["overall_status"], "COMPLETED")
        self.assertEqual([item["attempts"] for item in final["stages"]], [2, 2, 1])
        self.assertEqual(final["last_failure"], failure)
        self.assertEqual(final["next_action"], "VERIFY_REAL_INSTALLATION")

    def test_done_reverification_failure_blocks_without_erasing_later_history(self):
        session = self.begin()
        for stage in self.order.split(","):
            self.finish(session, stage)
        before = self.status()
        self.begin(resume=True)
        self.update(session, "media-check", "RUNNING")
        self.update(session, "media-check", "FAILED", "MEDIA_CHANGED")
        failed = self.status()
        self.assertEqual(failed["overall_status"], "FAILED")
        self.assertEqual(failed["stages"][1]["status"], "DONE")
        self.assertEqual(failed["stages"][2]["status"], "DONE")
        self.assertEqual(failed["history"][:len(before["history"])], before["history"])
        self.update(session, "assets", "RUNNING", code=5)

    def test_running_waiting_and_skipped_safe_recovery(self):
        session = self.begin()
        self.update(session, "media-check", "RUNNING")
        self.begin(resume=True)
        self.finish(session, "media-check")
        self.update(session, "assets", "RUNNING")
        self.update(session, "assets", "WAITING", "COMPOSE_REVIEW_REQUIRED")
        self.assertEqual(self.status()["overall_status"], "WAITING")
        self.update(session, "assets", "DONE", code=5)
        self.begin(resume=True)
        self.finish(session, "media-check")
        self.finish(session, "assets", skipped=True)
        self.finish(session, "verify")
        self.assertEqual(self.status()["overall_status"], "COMPLETED")
        self.begin(code=4)

    def test_identity_and_stage_changes_rejected_without_progress_mutation(self):
        self.begin()
        before = (self.state / "session.json").read_bytes()
        changes = [lambda x: x.update(target=str(self.root / "other")),
                   lambda x: x.update(media=str(self.root / "other-media")),
                   lambda x: x.update(delivery_id="sha256:" + "b" * 64),
                   lambda x: x.update(mode="upgrade"),
                   lambda x: x.update(components=["common", "frontend", "platform"])]
        for key in self.identity["options"]:
            def changed(x, key=key):
                x["options"][key] = {"skip_host_deps": True, "skip_configure": True,
                    "apply_compose": True, "legacy_license_mode": "bridge", "customer": "customer-b",
                    "license_file_path": str(self.root / "license.jws"),
                    "license_file_sha256": "b" * 64}[key]
            changes.append(changed)
        for change in changes:
            identity = copy.deepcopy(self.identity)
            change(identity)
            save(self.input, identity)
            self.begin(resume=True, code=4)
            self.assertEqual((self.state / "session.json").read_bytes(), before)
        save(self.input, self.identity)
        self.begin(resume=True, code=4, stages="media-check,verify")

    def test_invalid_identity_never_captures_credentials_or_raw_inputs(self):
        mutations = [lambda x: x.update(extra="do-not-echo-this-secret"),
                     lambda x: x.update(version=True),
                     lambda x: x.update(components=["contract", "common"]),
                     lambda x: x.update(components=["common", "common"]),
                     lambda x: x.update(components=["unreviewed"]),
                     lambda x: x.update(delivery_id="a" * 64),
                     lambda x: x["options"].update(client_secret="do-not-echo-this-secret"),
                     lambda x: x["options"].update(skip_host_deps="true"),
                     lambda x: x["options"].update(license_file_path="relative.jws")]
        for change in mutations:
            value = copy.deepcopy(self.identity)
            change(value)
            save(self.input, value)
            result = self.cli("begin", "--state-dir", self.state, "--identity-file", self.input,
                              "--stages", self.order, code=2)
            self.assertNotIn("do-not-echo-this-secret", result.stderr)
            self.assertFalse(self.state.exists())

    def test_stage_and_reason_names_are_finite_safe_tokens(self):
        for stages in ("", "one,one", "one,../two", "one,UPPER", ",one", "one,two\n", ",".join("s"+str(n) for n in range(129))):
            self.begin(stages=stages, code=2)
        session = self.begin()
        for reason in ("stderr contains secret", "eyJ.payload.signature", "BAD\nTOKEN", "x" * 200):
            self.update(session, "media-check", "FAILED", reason, code=2)
        self.update(session, "media-check", "RUNNING", "NOT_ALLOWED", code=2)
        self.assertEqual(self.status()["stages"][0]["attempts"], 0)

    def test_missing_state_and_human_status_are_explicit(self):
        self.cli("status", "--state-dir", self.state, code=3)
        self.begin(resume=True, code=3)
        session = self.begin()
        self.update(session, "media-check", "RUNNING")
        self.update(session, "media-check", "FAILED", "MEDIA_INVALID")
        human = self.cli("status", "--state-dir", self.state).stdout
        for text in ("已完成", "当前阶段", "待处理", "下一步", "命令位置", "--resume", "不证明", "MEDIA_INVALID"):
            self.assertIn(text, human)
        result = self.cli("status", "--state-dir", self.state, "--unsafe-do-not-echo", code=2)
        self.assertNotIn("unsafe-do-not-echo", result.stderr)

    def test_state_integrity_and_replay_reject_corruption_and_tampering(self):
        self.begin()
        path = self.state / "session.json"
        original = path.read_bytes()
        for raw in (b"{", b'{"version":1,"version":1}', original.replace(b'"cursor":0', b'"cursor":1')):
            path.write_bytes(raw)
            self.cli("status", "--state-dir", self.state, code=2)
        value = json.loads(original)
        value["stages"][0]["status"] = "DONE"
        value.pop("checksum")
        value["checksum"] = MODULE.checksum(value)
        save(path, value)
        self.cli("status", "--state-dir", self.state, code=2)
        path.write_bytes(original)
        self.assertEqual(self.status()["cursor"], 0)

    def test_duplicate_identity_json_and_special_files_rejected(self):
        self.input.write_text('{"version":1,"version":1}')
        self.begin(code=2)
        self.input.unlink()
        os.mkfifo(self.input, 0o600)
        self.begin(code=2)
        self.assertFalse(self.state.exists())

    def test_symlinks_hardlinks_and_unsafe_permissions_rejected(self):
        self.input.chmod(0o644)
        self.begin(code=2)
        self.input.chmod(0o600)
        session = self.begin()
        path = self.state / "session.json"
        path.chmod(0o644)
        self.cli("status", "--state-dir", self.state, code=2)
        path.chmod(0o600)
        linked = self.root / "linked.json"
        os.link(path, linked)
        self.cli("status", "--state-dir", self.state, code=2)
        linked.unlink()
        original = path.read_bytes()
        path.unlink()
        linked.write_bytes(original)
        linked.chmod(0o600)
        path.symlink_to(linked)
        self.cli("status", "--state-dir", self.state, code=2)
        path.unlink()
        path.write_bytes(original)
        path.chmod(0o600)
        self.state.chmod(0o755)
        self.update(session, "media-check", "RUNNING", code=2)
        self.state.chmod(0o700)
        self.root.chmod(0o775)
        self.cli("status", "--state-dir", self.state, code=2)
        self.root.chmod(0o700)

    def test_parent_and_target_symlink_and_foreign_owner_rejected(self):
        link = self.root / "alias"
        link.symlink_to(self.root, target_is_directory=True)
        self.cli("begin", "--state-dir", link / self.state.name, "--identity-file", self.input,
                 "--stages", self.order, code=2)
        self.target.symlink_to(self.media, target_is_directory=True)
        self.begin(code=2)
        info = SimpleNamespace(st_mode=stat.S_IFREG | 0o600, st_uid=os.geteuid()+10000, st_nlink=1)
        with self.assertRaises(MODULE.SessionError):
            MODULE.check_file(info)

    def test_atomic_fault_and_power_loss_residue_never_promote_partial_json(self):
        session = self.begin()
        path = self.state / "session.json"
        before = path.read_bytes()
        with mock.patch.object(MODULE.os, "replace", side_effect=OSError("synthetic crash before rename")):
            with self.assertRaises(OSError):
                MODULE.update(str(self.state), session, "media-check", "RUNNING", "")
        self.assertEqual(path.read_bytes(), before)
        residue = self.state / (".session-" + "c" * 32 + ".tmp")
        residue.write_text('{"incomplete":')
        residue.chmod(0o600)
        self.assertEqual(self.status()["cursor"], 0)
        self.begin(resume=True)
        self.finish(session, "media-check")
        self.assertTrue(residue.exists())
        self.assertEqual(self.status()["stages"][0]["attempts"], 1)

    def test_unknown_state_files_and_unsafe_residue_fail_closed(self):
        self.begin()
        unknown = self.state / "secrets.env"
        unknown.write_text("do-not-print-this-content")
        unknown.chmod(0o600)
        result = self.cli("status", "--state-dir", self.state, code=2)
        self.assertNotIn("do-not-print-this-content", result.stderr)
        unknown.unlink()
        residue = self.state / (".session-" + "d" * 32 + ".tmp")
        residue.symlink_to(self.input)
        self.cli("status", "--state-dir", self.state, code=2)

    def test_concurrent_short_operations_do_not_lose_history_or_attempts(self):
        session = self.begin()
        with ThreadPoolExecutor(max_workers=6) as executor:
            ids = list(executor.map(lambda _: self.begin(resume=True), range(12)))
        self.assertEqual(ids, [session]*12)
        value = self.status()
        self.assertEqual(value["generation"], 13)
        self.assertEqual(len(value["history"]), 13)
        with ThreadPoolExecutor(max_workers=6) as executor:
            list(executor.map(lambda _: self.update(session, "media-check", "RUNNING"), range(12)))
            list(executor.map(lambda _: self.update(session, "media-check", "DONE"), range(12)))
        value = self.status()
        self.assertEqual(value["stages"][0]["attempts"], 1)
        self.assertEqual(len(value["history"]), 15)
        self.assertEqual(value["cursor"], 1)


if __name__ == "__main__":
    unittest.main()
