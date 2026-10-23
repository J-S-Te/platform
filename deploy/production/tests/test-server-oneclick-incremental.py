#!/usr/bin/env python3
"""使用安全夹具验证实际服务器入口、检查器、安装器、锁及会话。"""
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import platform
import shutil
import subprocess
import sys
import tarfile
import tempfile
import unittest

sys.dont_write_bytecode = True
ROOT = Path(__file__).resolve().parents[4]
PRODUCTION = ROOT / "platform/deploy/production"
ENTRY = ROOT / "服务器一键离线部署.sh"
spec = importlib.util.spec_from_file_location("incremental_delivery_fixtures", Path(__file__).with_name("test-delivery-inspect.py"))
FIXTURE = importlib.util.module_from_spec(spec)
spec.loader.exec_module(FIXTURE)
FIXTURE.VERSION = VERSION = "test-v1"
COMPOSE = "services:\n  # BEGIN SUBSYSTEM: common\n  platform: {}\n  # END SUBSYSTEM: common\n"

# 回执绑定真实交付包 SHA；真实工作流和会话模块仍执行全部内部阶段检查，这些替身绝不调用 Docker。
DEPLOY = r'''#!/usr/bin/env bash
set -Eeuo pipefail
events="${INCREMENTAL_EVENTS:?}"
log() { printf '%s\n' "$*" >> "$events"; }
package_component() {
  case "$(basename -- "$1")" in
    common-infrastructure-*) printf common ;;
    platform-backend-*) printf platform ;;
    frontend-*) printf frontend ;;
    customer-opportunity-backend-*) printf customer-opportunity ;;
    customer-portal-backend-*) printf customer-portal ;;
    contract-backend-*) printf contract ;;
    *) return 99 ;;
  esac
}
package_sha() { sha256sum -- "$1" | awk '{print $1}'; }
import_proof() {
  [[ -f "runtime/.fixture-import-$1" && "$(cat "runtime/.fixture-import-$1")" == "$(package_sha "$2")" ]]
}
import_action() {
  [[ -f "$2" && "$(package_component "$2")" == "$1" ]] || return 99
  log "import:$(basename -- "$2")"
  package_sha "$2" > "runtime/.fixture-import-$1"
}
release_proof() {
  [[ -f "runtime/.fixture-publish-$1" && "$(cat "runtime/.fixture-publish-$1")" == "$(package_sha "$2")" ]]
}
release_action() {
  import_proof "$1" "$2" || return 99
  log "deploy:$1"
  package_sha "$2" > "runtime/.fixture-publish-$1"
}
prepare() {
  import_proof "$1" "$2" || return 99
  if [[ -f "runtime/.fixture-candidate-$1" ]]; then
    [[ "$(cat "runtime/.fixture-candidate-$1")" == "$(package_sha "$2")" ]] || return 98
  else
    log "prepare:$1"
    package_sha "$2" > "runtime/.fixture-candidate-$1"
  fi
}
case "${1:-}" in
  configure)
    [[ $# == 1 && ! -e .env && ! -e .release.env ]] || exit 99
    log configure
    printf 'APP_PUBLIC_BASE_URL=http://192.0.2.10\nKEYCLOAK_PUBLIC_URL=http://192.0.2.10:18090\n' > .env
    printf 'PLATFORM_IMAGE=fixture-registry/platform@sha256:%064d\n' 0 > .release.env
    chmod 0600 .env .release.env ;;
  reload-control-plane)
    [[ $# == 1 ]] || exit 99
    log reload-control-plane
    rm -f runtime/.control-plane-reload-required ;;
  verify) [[ $# == 1 ]] || exit 99; log verify ;;
  status) [[ $# -le 2 ]] || exit 99; log "status:${2:-all}" ;;
  import) [[ $# == 2 ]] || exit 99; import_action "$(package_component "$2")" "$2" ;;
  delivery-import-proof) [[ $# == 3 ]] || exit 99; import_proof "$2" "$3" ;;
  delivery-proof) [[ $# == 3 || $# == 4 ]] || exit 99; release_proof "$2" "$3" ;;
  delivery-prepare) [[ $# == 3 ]] || exit 99; prepare "$2" "$3" ;;
  delivery-enabled) [[ $# == 2 ]] || exit 99; printf 'ENABLED\n' ;;
  delivery-final-proof)
    [[ $# == 3 ]] || exit 99
    import_proof "$2" "$3" && [[ -f "runtime/.fixture-candidate-$2" && "${INCREMENTAL_ADOPTED:-0}" == 1 ]] ;;
  delivery-license-proof)
    [[ $# == 1 && "${INCREMENTAL_LICENSE:-0}" == 1 ]] || exit 1
    printf '{"status":"ENFORCED","applications":["customer_and_opportunity","customer_portal","contract_management"]}\n' ;;
  install)
    [[ $# == 1 && -n "${UIP_INSTALL_SESSION:-}" ]] || exit 99
    source "$(dirname -- "$0")/deployment-workflow.sh"
    log install
    common="packages/common-infrastructure-linux-amd64.tar.gz"
    platform="packages/platform-backend-test-v1-linux-amd64.tar.gz"
    frontend="packages/frontend-test-v1-linux-amd64.tar.gz"
    workflow_import common-import import_proof import_action common "$common"
    workflow_import platform-import import_proof import_action platform "$platform"
    workflow_publish platform release_proof release_action platform "$platform" "$common"
    workflow_import frontend-import import_proof import_action frontend "$frontend"
    workflow_publish frontend release_proof release_action frontend "$frontend"
    workflow_start license-prepare; workflow_done license-prepare
    for component in customer-opportunity customer-portal contract; do
      path="packages/${component}-backend-test-v1-linux-amd64.tar.gz"
      [[ -f "$path" ]] || continue
      workflow_import "$component-import" import_proof import_action "$component" "$path"
      workflow_run "$component-prepare" prepare "$component" "$path"
    done
    workflow_start license; workflow_done license
    # Exercise outer post-install paired-control-plane convergence.
    touch runtime/.control-plane-reload-required ;;
  *) printf 'unexpected deploy invocation: %s\n' "$*" >&2; exit 99 ;;
esac
'''

DOCKER = r'''#!/usr/bin/env bash
set -Eeuo pipefail
case "$*" in
  'info') ;;
  'compose version --short') printf 'v2.30.0\n' ;;
  'info --format {{.DockerRootDir}}') printf '%s\n' "${INCREMENTAL_DOCKER_ROOT:?}" ;;
  'info --format {{range .DriverStatus}}{{if eq (index . 0) "driver-type"}}{{index . 1}}{{end}}{{end}}') printf '' ;;
  'version --format {{.Server.Version}}') printf '29.8.1\n' ;;
  *)
    if [[ $# == 6 && "$1 $2" == 'ps -q' && "$3" == --filter && "$4" == label=com.docker.compose.project=basic-platform-production && "$5" == --filter ]]; then
      case "$6" in label=com.docker.compose.service=platform-api|label=com.docker.compose.service=subsystem-provisioner) exit 0 ;; esac
    fi
    if [[ "$1 ${2:-}" == 'compose --project-directory' && "${*: -2}" == 'config --services' ]]; then
      printf 'platform-api\nfrontend\n'; exit 0
    fi
    printf 'unexpected Docker invocation: %s\n' "$*" >> "${INCREMENTAL_REJECTED:?}"
    exit 99 ;;
esac
'''


def sha(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def script(path, text):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(text)
    path.chmod(0o755)


def image_package(component, version=VERSION):
    previous = FIXTURE.VERSION
    try:
        FIXTURE.VERSION = version
        return FIXTURE.image_package(component, image_version=version)[0]
    finally:
        FIXTURE.VERSION = previous


@unittest.skipUnless(platform.system() == "Linux" and os.geteuid() == 0, "Linux root fixture required")
class IncrementalTests(unittest.TestCase):
    def setUp(self):
        for tool in ("jq", "flock", "sha256sum"):
            self.assertIsNotNone(shutil.which(tool), "real host QA tool required: "+tool)
        self.temp = tempfile.TemporaryDirectory(prefix="server-oneclick-incremental-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name).resolve()
        self.media, self.target, self.assets, self.tools = [self.root / name for name in ("media", "target", "assets", "tools")]
        for path in (self.media, self.target, self.assets, self.tools, self.root / "docker-root"):
            path.mkdir()
        self.events, self.rejected = self.root / "events", self.root / "rejected-docker"
        script(self.assets / "bin/deploy.sh", DEPLOY)
        script(self.assets / "bin/backup-all.sh", "#!/usr/bin/env bash\nexit 99\n")
        for tool in ('deployment-workflow.sh','deployment-session.py','delivery-runtime.sh','delivery-runtime.py',
                     'delivery-inspect.py','delivery-inspect-images.py','delivery-files.py','subsystem-runtime.py'):
            shutil.copy2(PRODUCTION / 'bin' / tool, self.assets / 'bin' / tool)
        (self.assets / "docker-compose.yml").write_text(COMPOSE)
        for filename in FIXTURE.PROFILES.values():
            destination = self.assets / "subsystems.d" / filename
            destination.parent.mkdir(exist_ok=True)
            shutil.copy2(PRODUCTION / "subsystems.d" / filename, destination)
        (self.assets / "subsystem-templates").mkdir()
        (self.assets / "subsystem-templates/customer.env.example").write_text("CUSTOMER_CLIENT_ID=PENDING_ONBOARDING\n")
        shutil.copy2(PRODUCTION / "bin/install-assets.sh", self.media / "install-assets.sh")
        shutil.copy2(ROOT / "系统运维工具.sh", self.media / "系统运维工具.sh")
        (self.media / "BUILD_INFO.txt").write_text("RELEASE_VERSION="+VERSION+"\nTARGET_PLATFORM=linux/amd64\n")
        (self.media / "BASE_DELIVERY_INFO.txt").write_text("BASE_DELIVERY=base-v1\nBASE_SHA256SUMS_SHA256="+"0"*64+"\n")
        self.packages = {}
        for component in ("common", "platform", "frontend", "customer-opportunity", "customer-portal", "contract"):
            name = FIXTURE.COMPONENT_PREFIXES[component]+("" if component == "common" else "-"+VERSION)+"-linux-amd64.tar.gz"
            (self.media / name).write_bytes(image_package(component))
            self.sidecar(self.media / name)
            self.packages[component] = name
        self.refresh_media()
        for name in ("packages", "manifests", "runtime"):
            (self.target / name).mkdir()
        for child in self.assets.iterdir():
            if child.is_dir(): shutil.copytree(child, self.target / child.name)
            else: shutil.copy2(child, self.target / child.name)
        (self.target / ".env").write_text("SERVER_IP=192.0.2.10\n")
        (self.target / ".release.env").write_text("PLATFORM_IMAGE=fixture-registry/platform@sha256:"+"9"*64+"\n")
        for name in (".env", ".release.env"):
            (self.target / name).chmod(0o600)
        for component in ("common", "platform", "frontend"):
            shutil.copy2(self.media / self.packages[component], self.target / "packages" / self.packages[component])
        self.save_compat()
        (self.target / "packages/contract-backend-existing-linux-amd64.tar.gz").write_bytes(image_package("contract", "existing"))
        script(self.tools / "docker", DOCKER)
        # 只检查存在性的前置条件，即使对隔离宿主机也不得产生变更。
        for name in ("curl", "openssl", "update-ca-certificates", "ss", "systemctl", "containerd"):
            script(self.tools / name, "#!/usr/bin/env bash\nprintf 'forbidden host command: %s\\n' "+name+' >> "${INCREMENTAL_REJECTED:?}"\nexit 99\n')
        self.env = dict(os.environ, PATH=str(self.tools)+":"+os.environ["PATH"], TMPDIR=str(self.root),
                        PYTHONDONTWRITEBYTECODE="1", INCREMENTAL_EVENTS=str(self.events),
                        INCREMENTAL_REJECTED=str(self.rejected), INCREMENTAL_DOCKER_ROOT=str(self.root / "docker-root"))

    def tearDown(self):
        self.assertFalse(self.rejected.exists(), self.rejected.read_text() if self.rejected.exists() else "")

    def sidecar(self, path):
        path.with_name(path.name+".sha256").write_text(sha(path)+"  "+path.name+"\n")

    def refresh_manifest(self):
        files = sorted(path for path in self.media.iterdir() if path.name != "SHA256SUMS")
        (self.media / "SHA256SUMS").write_text("".join(sha(path)+"  "+path.name+"\n" for path in files))

    def refresh_media(self):
        archive = self.media / ("deployment-assets-"+VERSION+".tar.gz")
        with tarfile.open(archive, "w:gz") as out:
            for child in sorted(self.assets.iterdir()): out.add(child, arcname=child.name)
        self.sidecar(archive)
        lines = ["FORMAT=1", "PLATFORM_ASSETS_INPUT_SHA256="+"a"*64]
        for component in ("common", "platform", "frontend", "assets"):
            name = archive.name if component == "assets" else self.packages[component]
            lines += [component.upper()+"_PACKAGE="+name, component.upper()+"_PACKAGE_SHA256="+sha(self.media / name)]
        (self.media / "PLATFORM_ASSETS_COMPATIBILITY.env").write_text("\n".join(lines)+"\n")
        self.refresh_manifest()

    def save_compat(self):
        path = self.target / "packages/PLATFORM_ASSETS_COMPATIBILITY.delivery.env"
        shutil.copy2(self.media / "PLATFORM_ASSETS_COMPATIBILITY.env", path)
        path.chmod(0o600)

    def run_entry(self, *extra, target=None, env=None):
        args = ["bash", str(ENTRY), "--media", str(self.media), "--target", str(target or self.target), "--skip-host-deps", "--yes"]
        return subprocess.run([*args, *extra], env={**self.env, **(env or {})}, input="", text=True, capture_output=True, timeout=60)

    def add(self, component="customer-opportunity", *extra, env=None):
        return self.run_entry("--add-subsystems", component, *extra, env=env)

    def ok(self, result):
        self.assertEqual(result.returncode, 0, result.stdout+result.stderr)
        return result.stdout+result.stderr

    def operations(self):
        return self.events.read_text().splitlines() if self.events.exists() else []

    def clear_events(self):
        self.events.write_text("")

    def snapshot(self, target=None):
        return {str(path.relative_to(target or self.target)): (sha(path), path.stat().st_mode & 0o777)
                for path in (target or self.target).rglob("*") if path.is_file()}

    def session(self, target=None):
        result = subprocess.run([sys.executable, str(PRODUCTION / "bin/deployment-session.py"), "status", "--state-dir", str(target or self.target)+".installation-state", "--format", "json"], capture_output=True, text=True, env=self.env)
        self.assertEqual(result.returncode, 0, result.stdout+result.stderr)
        return json.loads(result.stdout)

    def assert_rejected_before_mutation(self, result, message):
        self.assertNotEqual(result.returncode, 0, result.stdout+result.stderr)
        self.assertIn(message, result.stdout+result.stderr)
        self.assertEqual(self.operations(), [])

    def test_auto_mode_checks_existing_target_and_requires_interactive_confirmation(self):
        before = self.snapshot()
        self.assertIn("发现既有部署，选择升级", self.ok(self.run_entry("--check-only")))
        self.assertEqual(self.snapshot(), before)
        self.assertEqual(self.operations(), [])
        self.assert_rejected_before_mutation(self.run_entry(), "自动升级需要交互确认")
        self.assertEqual(self.snapshot(), before)

    def test_legacy_unlisted_sidecars_are_cross_checked_and_bad_digest_is_rejected(self):
        manifest = self.media / "SHA256SUMS"
        manifest.write_text("".join(row for row in manifest.read_text().splitlines(True) if not row.split()[1].endswith(".sha256")))
        self.assertIn("旧版介质 sidecar 已与总清单及实际文件三方核验", self.ok(self.run_entry("--check-only")))
        sidecar = self.media / (self.packages["common"]+".sha256")
        sidecar.write_text("0"*64+"  "+self.packages["common"]+"\n")
        self.assert_rejected_before_mutation(self.run_entry("--check-only"), "单包 SHA256 与交付总清单不一致")

    def test_add_check_only_preserves_every_target_file_and_does_not_install_operations_tool(self):
        before = self.snapshot()
        self.assertIn("增量检查完成", self.ok(self.run_entry("--add-components", "customer-opportunity", "--check-only")))
        self.assertEqual(before, self.snapshot())
        self.assertEqual(self.operations(), [])
        self.assertFalse((self.target / "系统运维工具.sh").exists())
        self.assertFalse(Path(str(self.target)+".installation-state").exists())

    def test_legacy_target_missing_workflow_tools_stops_before_actions(self):
        (self.target / 'bin/delivery-runtime.py').unlink()
        self.assert_rejected_before_mutation(self.add(), '分阶段增装工具')
        self.assertFalse(Path(str(self.target)+'.installation-state').exists())

    def test_explicit_install_existing_target_cannot_create_a_recoverable_fresh_session(self):
        before = self.snapshot()
        self.assert_rejected_before_mutation(self.run_entry('--install'), '目标目录已是已部署环境')
        self.assertEqual(self.snapshot(), before)
        self.assertFalse(Path(str(self.target)+'.installation-state').exists())
        # 被拒绝的全新安装尝试不得残留身份，否则后续 --resume 可能将已有安装误判为未完成的新安装。
        resumed = self.run_entry('--install', '--resume')
        self.assertNotEqual(resumed.returncode, 0)
        self.assertEqual(self.operations(), [])
        self.assertEqual(self.snapshot(), before)
        self.assertFalse(Path(str(self.target)+'.installation-state').exists())

    def test_crm_add_preserves_config_base_assets_and_installed_contract(self):
        before = self.snapshot()
        self.assertIn("基础平台、基础设施、统一前端、已有子系统及数据未重新部署", self.ok(self.add()))
        self.assertEqual(self.operations(), ["reload-control-plane", "verify", "import:"+self.packages["customer-opportunity"], "prepare:customer-opportunity", "verify", "status:customer-opportunity"])
        after = self.snapshot()
        for name, value in before.items(): self.assertEqual(after[name], value, name)
        self.assertEqual((self.target / "系统运维工具.sh").read_bytes(), (ROOT / "系统运维工具.sh").read_bytes())
        self.assertTrue((self.target / "packages" / self.packages["customer-opportunity"]).is_file())
        self.assertEqual(self.session()["overall_status"], "WAITING")

    def test_resume_is_explicit_and_reuses_same_package_candidate_and_control_plane(self):
        self.ok(self.add())
        original = self.session()
        self.clear_events()
        self.assert_rejected_before_mutation(self.add(), "RESUME_REQUIRED")
        self.assertIn("镜像包已存在且内容一致", self.ok(self.add("customer-opportunity", "--resume")))
        self.assertEqual(self.operations(), ["verify", "verify", "status:customer-opportunity"])
        current = self.session()
        self.assertEqual(current["session_id"], original["session_id"])
        self.assertEqual(current["generation"], original["generation"]+1)

    def test_portal_always_imports_and_prepares_crm_before_portal(self):
        self.ok(self.add("customer-portal"))
        self.assertEqual(self.operations(), ["reload-control-plane", "verify", "import:"+self.packages["customer-opportunity"], "prepare:customer-opportunity", "import:"+self.packages["customer-portal"], "prepare:customer-portal", "verify", "status:customer-opportunity", "status:customer-portal"])

    def test_other_installed_business_version_is_rejected_before_control_plane(self):
        (self.target / "packages/customer-opportunity-backend-old-linux-amd64.tar.gz").write_bytes(image_package("customer-opportunity", "old"))
        self.assert_rejected_before_mutation(self.add(), "必须使用受控 upgrade 流程")

    def test_profile_asset_drift_is_rejected_before_control_plane(self):
        path = self.target / "subsystems.d/customer_and_opportunity-prod.yaml"
        path.write_bytes(path.read_bytes()+b"\n# externally revised production profile\n")
        self.assert_rejected_before_mutation(self.add(), "共享资产与目标不一致")

    def test_missing_persisted_compatibility_lock_is_rejected_before_control_plane(self):
        (self.target / "packages/PLATFORM_ASSETS_COMPATIBILITY.delivery.env").unlink()
        self.assert_rejected_before_mutation(self.add(), "缺少已保存的 Platform/资产兼容性锁")

    def test_changed_persisted_compatibility_lock_is_rejected_before_control_plane(self):
        path = self.target / "packages/PLATFORM_ASSETS_COMPATIBILITY.delivery.env"
        path.write_bytes(path.read_bytes().replace(b"a"*64, b"b"*64))
        self.assert_rejected_before_mutation(self.add(), "Platform/资产兼容性锁不一致")

    def test_same_named_different_base_package_is_rejected_before_control_plane(self):
        (self.target / "packages" / self.packages["platform"]).write_bytes(b"changed-existing-platform-package")
        self.assert_rejected_before_mutation(self.add(), "内容不同")

    def test_waiting_session_rejects_different_components_or_new_batch_before_operations(self):
        self.ok(self.add())
        self.clear_events()
        self.assert_rejected_before_mutation(self.add("customer-portal", "--resume"), "IDENTITY_CONFLICT")
        self.assert_rejected_before_mutation(self.add("customer-portal", "--new-session"), "INCOMPLETE_SESSION")

    def test_waiting_session_rejects_changed_delivery_digest_before_operations(self):
        self.ok(self.add())
        self.clear_events()
        info = self.media / "BUILD_INFO.txt"
        info.write_text(info.read_text()+"SOURCE_INPUT_SHA256="+"b"*64+"\n")
        self.refresh_manifest()
        self.assert_rejected_before_mutation(self.add("customer-opportunity", "--resume"), "IDENTITY_CONFLICT")

    def test_only_completed_session_can_open_explicit_next_batch_and_preserves_old_record(self):
        ready = {"INCREMENTAL_ADOPTED": "1", "INCREMENTAL_LICENSE": "1"}
        self.ok(self.add(env=ready))
        original = self.session()
        self.assertEqual(original["overall_status"], "COMPLETED")
        self.clear_events()
        self.ok(self.add("customer-portal", "--new-session", env=ready))
        current = self.session()
        self.assertNotEqual(current["session_id"], original["session_id"])
        self.assertEqual(current["overall_status"], "COMPLETED")
        history = Path(str(self.target)+".installation-history") / (original["session_id"]+".json")
        self.assertEqual(json.loads(history.read_text())["session_id"], original["session_id"])
        self.assertEqual(self.operations(), ["reload-control-plane", "verify", "import:"+self.packages["customer-portal"], "prepare:customer-portal", "verify", "status:customer-opportunity", "status:customer-portal"])

    def test_adopted_but_not_authorized_remains_waiting_until_current_proof_succeeds(self):
        self.ok(self.add(env={"INCREMENTAL_ADOPTED": "1"}))
        waiting = self.session()
        self.assertEqual(waiting["overall_status"], "WAITING")
        self.assertEqual(waiting["stages"][-1]["reason_code"], "WAITING_AUTHORIZATION")
        self.ok(self.add("customer-opportunity", "--resume", env={"INCREMENTAL_ADOPTED": "1", "INCREMENTAL_LICENSE": "1"}))
        self.assertEqual(self.session()["overall_status"], "COMPLETED")

    def test_fresh_install_persists_compatibility_and_finishes_real_inner_session_stages(self):
        fresh = self.root / "fresh-target"
        output = self.ok(self.run_entry("--install", target=fresh))
        path = fresh / "packages/PLATFORM_ASSETS_COMPATIBILITY.delivery.env"
        self.assertEqual(path.read_bytes(), (self.media / "PLATFORM_ASSETS_COMPATIBILITY.env").read_bytes())
        self.assertEqual(path.stat().st_mode & 0o777, 0o600)
        operations = self.operations()
        self.assertEqual(operations.count("reload-control-plane"), 1)
        self.assertLess(operations.index("deploy:platform"), operations.index("reload-control-plane"))
        self.assertEqual(operations[-3:], ["reload-control-plane", "verify", "status:all"])
        self.assertFalse((fresh / "runtime/.control-plane-reload-required").exists())
        state = self.session(fresh)
        self.assertEqual(state["overall_status"], "WAITING")
        self.assertTrue(all(row["status"] == "DONE" for row in state["stages"][:-1]))
        self.assertEqual(state["stages"][-1]["reason_code"], "WAITING_PLATFORM_ADOPTION")
        self.assertIn("业务接入/授权待完成", output)
        configs = {name: (fresh / name).read_bytes() for name in (".env", ".release.env")}
        self.clear_events()
        self.ok(self.run_entry("--install", "--resume", target=fresh, env={"INCREMENTAL_ADOPTED": "1", "INCREMENTAL_LICENSE": "1"}))
        self.assertEqual(self.session(fresh)["overall_status"], "COMPLETED")
        self.assertEqual(self.operations(), ["install", "reload-control-plane", "verify", "status:all"])
        self.assertEqual(configs, {name: (fresh / name).read_bytes() for name in configs})


if __name__ == "__main__":
    unittest.main(verbosity=2)
