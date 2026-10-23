#!/usr/bin/env python3
"""使用严格的 Docker/Registry 替身验证真实打包、导入及发布证明。"""

import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import shlex
import shutil
import subprocess
import sys
import tarfile
import tempfile
import unittest


sys.dont_write_bytecode = True
ROOT = Path(__file__).resolve().parents[1]
BIN = ROOT / "bin"
SPEC = importlib.util.spec_from_file_location("reviewed_runtime_test_inspector", BIN / "delivery-inspect.py")
INSPECTOR = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(INSPECTOR)
OCI = "application/vnd.oci.image.manifest.v1+json"
CONFIG = "application/vnd.oci.image.config.v1+json"
APPLICATIONS = {
    "customer-opportunity": ("customer_and_opportunity", "customer", "CUSTOMER_CRM_IMAGE"),
    "customer-portal": ("customer_portal", "portal", "CUSTOMER_PORTAL_IMAGE"),
    "contract": ("contract_management", "contract", "CONTRACT_IMAGE"),
    "project": ("project_management", "project", "PROJECT_IMAGE"),
    "settlement": ("settlement", "settlement", "SETTLEMENT_IMAGE"),
    "data-analysis": ("data_analysis", "data-analysis", "DATA_ANALYSIS_IMAGE"),
}
BI_KEYS = ["DATA_ANALYSIS_DASHBOARD_API_IMAGE", "DATA_ANALYSIS_AGGREGATION_WORKER_IMAGE",
           "DATA_ANALYSIS_ALERT_WORKER_IMAGE", "DATA_ANALYSIS_MIGRATE_IMAGE"]


def raw(value):
    return json.dumps(value, separators=(",", ":")).encode()


def digest(value):
    return "sha256:" + hashlib.sha256(value).hexdigest()


def archive_bytes(members, compressed=False):
    data = io.BytesIO()
    with tarfile.open(fileobj=data, mode="w:gz" if compressed else "w:") as archive:
        for name, content in members:
            item = tarfile.TarInfo(name)
            item.size = len(content)
            item.mode = 0o600
            archive.addfile(item, io.BytesIO(content))
    return data.getvalue()


BACKEND = r'''#!/usr/bin/env python3
import hashlib,json,os,pathlib,sys
arguments=sys.argv[1:]
state=json.loads(pathlib.Path(os.environ['DOCKER_STATE']).read_text())
name=pathlib.Path(sys.argv[0]).name
with open(os.environ['DOCKER_CALL_LOG'],'a') as log:
    log.write(json.dumps([name]+arguments)+'\n')
def stop():
    print('unexpected or unavailable fixture operation',file=sys.stderr)
    raise SystemExit(92)
def output(value):
    print(value)
    raise SystemExit(0)
if name=='curl':
    url=arguments[-1]
    if '--head' in arguments:
        if url not in state['registry']: stop()
        raise SystemExit(0)
    if url not in state['registry']: stop()
    sys.stdout.write(state['registry'][url]); raise SystemExit(0)
if arguments[:2]==['image','inspect']:
    image=state['images'].get(arguments[2])
    if image is None: stop()
    form=arguments[arguments.index('--format')+1]
    if form=='{{.Architecture}}/{{.Os}} {{.Id}}': output(image.get('platform','amd64/linux')+' '+image['id'])
    if form=='{{.Id}}': output(image['id'])
    stop()
if arguments[:2]==['container','inspect']:
    candidate=state.get('candidate')
    if candidate is None or arguments[2]!=candidate['Name']: raise SystemExit(1)
    if '--format' in arguments:
        if arguments[-1]!='{{json .}}': stop()
        output(json.dumps(candidate))
    raise SystemExit(0)
if arguments[0]=='inspect':
    form=arguments[arguments.index('--format')+1] if '--format' in arguments else arguments[arguments.index('-f')+1]
    name=arguments[1] if arguments[1] not in ('--format','-f') else arguments[-1]
    service=state['services'].get(name.removeprefix('cid-'))
    if service is None: stop()
    values={'{{.State.Status}}':service.get('status','running'), '{{.State.Running}}':'true',
            '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}':service.get('health','healthy'),
            '{{.RestartCount}}':str(service.get('restarts',0)), '{{.Image}}':service['native']}
    if form not in values: stop()
    output(values[form])
if arguments[0]=='exec':
    program=arguments[-1]
    target=pathlib.Path(os.environ['RUNTIME_TARGET'])
    if 'SUBSYSTEM_PRODUCTION_PROFILES_DIR' in program:
        values=[hashlib.sha256(p.read_bytes()).hexdigest()+'  '+p.name for p in sorted((target/'subsystems.d').glob('*.yaml'))]
        if state.get('stale_profiles'): values[0]='0'*64+'  stale.yaml'
        output('\n'.join(values))
    if 'SUBSYSTEM_PRODUCTION_RELEASE_ENV_PATH' in program:
        values=[hashlib.sha256((target/p).read_bytes()).hexdigest()+'  /fixture/'+p for p in ('.release.env','docker-compose.yml')]
        if state.get('stale_agent'): values[0]='0'*64+'  /fixture/.release.env'
        output('\n'.join(values))
    stop()
if arguments[0]=='compose':
    index=1
    while index<len(arguments) and arguments[index] in ('--project-directory','--file','--env-file'):
        index+=2
    command=arguments[index:]
    if command==['config','--format','json']: output(json.dumps({'services':state['compose_services']}))
    if command==['config','--services']: output('\n'.join(state['compose_services']))
    if command[:2]==['ps','-q']:
        output('cid-'+command[2] if command[2] in state['services'] else '')
    if command[:1]==['run'] and 'verify-deployment' in command:
        if not state.get('controlled_ready',True): raise SystemExit(23)
        output('controlled READY/current-manifest')
    stop()
stop()
'''


class RuntimeTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="delivery-runtime-")
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name).resolve()
        self.target = self.root / "deployment"
        self.media = self.root / "media"
        self.tools = self.root / "tools"
        for path in (self.target / "manifests", self.target / "runtime", self.target / "subsystems.d", self.media, self.tools):
            path.mkdir(parents=True)
        for path in (ROOT / "subsystems.d").glob("*.yaml"):
            shutil.copy2(path, self.target / "subsystems.d" / path.name)
        (self.target / "docker-compose.yml").write_text("services: {}\n")
        (self.target / ".env").write_text("COMPOSE_PROJECT_NAME=runtime-fixture\nPUBLIC_ACCESS_MODE=ip\nPUBLIC_PLATFORM_HOST=192.0.2.10\nPUBLIC_SSO_HOST=192.0.2.10\nPUBLIC_HTTP_PORT=8081\nPLATFORM_API_PORT=18080\nKEYCLOAK_HTTP_PORT=18090\n")
        self.state_path = self.root / "docker-state.json"
        self.log = self.root / "calls.log"
        self.state = {"images": {}, "registry": {}, "services": {}, "compose_services": {}, "candidate": None}
        self.address = "127.0.0.1:18250"
        self.packages = {}
        self.records = {}
        self.refs = {}
        self.release = {}
        for component in ("common", "platform", "frontend", *APPLICATIONS):
            self.package(component)
        for name in ("docker", "curl"):
            path = self.tools / name
            path.write_text(BACKEND)
            path.chmod(0o755)
        stat_command = self.tools / "stat"
        stat_command.write_text("#!/usr/bin/env python3\nimport os,stat,sys\nassert sys.argv[1:3]==['-c','%a']\nprint(format(stat.S_IMODE(os.stat(sys.argv[3]).st_mode),'o'))\n")
        stat_command.chmod(0o755)
        for service in ("platform-api", "platform-worker", "subsystem-provisioner"):
            self.service(service, self.refs["platform"])
        self.service("frontend", self.refs["frontend"])
        self.service("file-gateway", self.refs["gateway"])
        self.service("docker-socket-proxy", self.refs["proxy"])
        for service in ("platform-mysql", "keycloak-db", "contract-mysql", "file-gateway-mysql"):
            self.service(service, "mysql:8.4")
        self.service("keycloak", "quay.io/keycloak/keycloak:26.2")
        self.service("temporal", "temporalio/auto-setup:1.29.7")
        for component, (application, runtime, key) in APPLICATIONS.items():
            profile = self.target / "subsystems.d" / (application + "-prod.yaml" if component != "data-analysis" else "data-analysis-prod.yaml")
            line = next(line for line in profile.read_text().splitlines() if line.startswith("  runtime_services: "))
            services = line.partition("[")[2].partition("]")[0].split(", ")
            for service in services:
                reference = self.refs[component]
                if component == "data-analysis":
                    reference = {"data-analysis-metabase": "metabase/metabase:v0.53.7", "data-analysis-api": self.refs[BI_KEYS[0]],
                                 "data-analysis-aggregation-worker": self.refs[BI_KEYS[1]], "data-analysis-alert-worker": self.refs[BI_KEYS[2]]}[service]
                self.service(service, reference)
            values = ["PLATFORM_APPLICATION_CODE", "PLATFORM_ENVIRONMENT_CODE", "PLATFORM_AUDIT_CLIENT_ID", "PLATFORM_AUDIT_CLIENT_SECRET",
                      "FILE_GATEWAY_BASE_URL", "FILE_GATEWAY_APPLICATION_ID", "FILE_GATEWAY_CLIENT_ID", "FILE_GATEWAY_CLIENT_SECRET",
                      "PLATFORM_AUTHORIZATION_CONTEXT_URL", "PLATFORM_AUTHORIZATION_CATALOG_APPLICATION_ID", "PLATFORM_AUTHORIZATION_CATALOG_CLIENT_ID",
                      "PLATFORM_AUTHORIZATION_CATALOG_CLIENT_SECRET", "OIDC_SESSION_ENCRYPTION_KEY_BASE64", "OIDC_ISSUER", "OIDC_CLIENT_ID", "OIDC_CLIENT_SECRET",
                      "OIDC_REDIRECT_URI", "OIDC_TENANT_ID", "PLATFORM_OWNER_DIRECTORY_CLIENT_ID", "PLATFORM_OWNER_DIRECTORY_CLIENT_SECRET"]
            (self.target / "runtime" / (runtime + ".env")).write_text("".join(key + "=fixture-value\n" for key in values))
        self.state["compose_services"]["data-analysis-migrate"] = {"image": self.refs[BI_KEYS[3]]}
        build_id = "customer-presale-" + self.refs["customer-opportunity"].rsplit("@sha256:", 1)[1][:16]
        self.release["CUSTOMER_PRESALE_TEMPORAL_WORKER_BUILD_ID"] = build_id
        self.state["compose_services"]["customer-presale-worker"]["environment"] = {"TEMPORAL_WORKER_BUILD_ID": build_id}
        self.save_release()
        self.save()

    def package(self, component, oci=False):
        version = "fixture-v1"
        tags = INSPECTOR.expected_tags(component, version)
        members, configs, manifest = [], [], []
        oci_entries = []
        entries = []
        for tag in tags:
            config = raw({"architecture": "amd64", "os": "linux", "rootfs": {"type": "layers", "diff_ids": []},
                          "config": {"Labels": {"org.opencontainers.image.version": version,
                                                 "com.basic-platform.license.protocol": "1", "fixture.tag": tag}}})
            config_digest = digest(config)
            configs.append(config_digest)
            image_manifest = raw({"schemaVersion": 2, "mediaType": OCI,
                                  "config": {"mediaType": CONFIG, "digest": config_digest, "size": len(config)}, "layers": []})
            image_digest = digest(image_manifest)
            native = image_digest if oci else config_digest
            self.state["images"][tag] = {"id": native}
            self.state["images"][native] = {"id": native}
            if oci:
                members += [("blobs/sha256/" + config_digest[7:], config), ("blobs/sha256/" + image_digest[7:], image_manifest)]
                oci_entries.append({"mediaType": OCI, "digest": image_digest, "size": len(image_manifest),
                                    "platform": {"architecture": "amd64", "os": "linux"},
                                    "annotations": {"org.opencontainers.image.ref.name": tag}})
            else:
                members.append((config_digest[7:] + ".json", config))
                manifest.append({"Config": config_digest[7:] + ".json", "RepoTags": [tag], "Layers": []})
            if component == "common":
                repository = "uip/file-gateway" if tag.startswith("uip-package/file-gateway:") else "uip/docker-socket-proxy" if tag.startswith("tecnativa/docker-socket-proxy:") else None
            elif component == "data-analysis":
                repository = "uip/" + tag.rsplit("/", 1)[1].split(":", 1)[0]
            else:
                repository = "uip/" + component
            if repository:
                reference = self.address + "/" + repository + "@" + image_digest
                self.state["registry"]["http://" + self.address + "/v2/" + repository + "/manifests/" + image_digest] = image_manifest.decode()
                self.state["registry"]["http://" + self.address + "/v2/" + repository + "/blobs/" + config_digest] = config.decode()
                self.state["images"][reference] = {"id": native}
                entries.append(reference)
                if component == "common":
                    self.refs["gateway" if "file-gateway" in repository else "proxy"] = reference
        if oci:
            members += [("oci-layout", raw({"imageLayoutVersion": "1.0.0"})), ("index.json", raw({"schemaVersion": 2, "manifests": oci_entries}))]
        else:
            members.append(("manifest.json", raw(manifest)))
        images = archive_bytes(members)
        metadata = {"PACKAGE_FORMAT": "2", "COMPONENT": component, "VERSION": version, "PLATFORM": "linux/amd64",
                    "IMAGES": ",".join(tags), "IMAGE_CONFIG_DIGESTS": ",".join(configs)}
        if component == "common":
            metadata.update(DOCKER_SOCKET_PROXY_UPSTREAM_REF=INSPECTOR.PROXY, DOCKER_SOCKET_PROXY_CONFIG_DIGEST=configs[7])
        payload = archive_bytes([("package.env", "".join(key + "=" + value + "\n" for key, value in metadata.items()).encode()),
                                 ("SHA256SUMS", (hashlib.sha256(images).hexdigest() + "  images.tar\n").encode()), ("images.tar", images)], compressed=True)
        filename = ("common-infrastructure" if component == "common" else INSPECTOR.COMPONENTS[component] + "-" + version) + "-linux-amd64.tar.gz"
        path = self.media / filename
        path.write_bytes(payload)
        Path(str(path) + ".sha256").write_text(hashlib.sha256(payload).hexdigest() + "  " + filename + "\n")
        image_ref = ",".join(tags) if component == "common" else entries[0]
        record = {"COMPONENT": component, "VERSION": version, "IMAGE_REF": image_ref,
                  "SOURCE_IMAGE_IDS": ",".join(configs), "IMAGE_DIGEST": "-" if component == "common" else image_ref.rsplit("@sha256:", 1)[1],
                  "PACKAGE_SHA256": hashlib.sha256(payload).hexdigest()}
        if component == "common":
            record.update(FILE_GATEWAY_IMAGE_REF=self.refs["gateway"], DOCKER_SOCKET_PROXY_IMAGE_REF=self.refs["proxy"])
            self.release.update(FILE_GATEWAY_IMAGE=self.refs["gateway"], DOCKER_SOCKET_PROXY_IMAGE=self.refs["proxy"])
        elif component == "data-analysis":
            record.update(zip(BI_KEYS, entries))
            self.release.update(zip(BI_KEYS, entries))
            self.refs.update(zip(BI_KEYS, entries))
        self.refs[component] = image_ref
        if component != "common":
            key = "PLATFORM_IMAGE" if component == "platform" else "FRONTEND_IMAGE" if component == "frontend" else APPLICATIONS[component][2]
            self.release[key] = image_ref
        self.packages[component] = path
        self.records[component] = record
        self.save_record(component)

    def save_record(self, component):
        record = self.records[component]
        filename = component + "-" + record["VERSION"] + ".imported"
        (self.target / "manifests" / filename).write_text("".join(key + "=" + value + "\n" for key, value in record.items()))
        (self.target / "manifests" / (component + ".latest")).write_text(filename + "\n")

    def service(self, name, image):
        self.state["compose_services"][name] = {"image": image}
        self.state["services"][name] = {"native": self.state["images"][image]["id"]}

    def save(self):
        self.state_path.write_text(json.dumps(self.state))

    def save_release(self):
        (self.target / ".release.env").write_text("".join(key + "=" + value + "\n" for key, value in self.release.items()))
        for path in (self.target / ".env", self.target / ".release.env"):
            path.chmod(0o600)

    def shell(self, body, success=True):
        self.save()
        environment = dict(os.environ, PATH=str(self.tools) + os.pathsep + os.environ["PATH"], DOCKER_STATE=str(self.state_path),
                           DOCKER_CALL_LOG=str(self.log), RUNTIME_TARGET=str(self.target), UIP_VERIFY_STABILITY_SECONDS="1", UIP_HEALTH_WAIT_SECONDS="1")
        prefix = "set -Eeuo pipefail\nsource " + shlex.quote(str(BIN / "deploy.sh")) + "\n"
        for key, value in {"deploy_dir": self.target, "runtime_file": self.target / ".env", "release_file": self.target / ".release.env",
                           "compose_file": self.target / "docker-compose.yml", "manifest_dir": self.target / "manifests",
                           "profiles_dir": self.target / "subsystems.d", "registry_address": self.address}.items():
            prefix += key + "=" + shlex.quote(str(value)) + "\n"
        result = subprocess.run(["bash", "-c", prefix + body], env=environment, text=True, capture_output=True, timeout=45)
        self.assertEqual(result.returncode == 0, success, result.stdout + result.stderr)
        return result

    def invoke(self, function, component, success=True):
        arguments = [function, component, str(self.packages[component])]
        if function == "delivery_release_matches" and component == "platform":
            arguments.append(str(self.packages["common"]))
        return self.shell(" ".join(shlex.quote(argument) for argument in arguments), success)

    def calls(self):
        return [json.loads(line) for line in self.log.read_text().splitlines()] if self.log.exists() else []

    def candidate(self):
        self.state["candidate"] = {"Name": "uip-contract-candidate", "Image": self.state["images"][self.refs["contract"]]["id"],
                                   "Config": {"Image": self.refs["contract"], "Labels": {
                                       "com.basic-platform.discovery": "v1", "com.basic-platform.application_code": "contract_management",
                                       "com.basic-platform.environment": "prod", "com.basic-platform.service_name": "contract-api",
                                       "com.basic-platform.service_role": "business", "com.basic-platform.version": "fixture-v1"}}}

    def test_exact_classic_and_common_imports_are_inspection_only(self):
        self.invoke("delivery_import_matches", "contract")
        self.invoke("delivery_import_matches", "common")
        self.assertTrue(self.calls())
        self.assertTrue(all(call[0] == "curl" or call[1:3] == ["image", "inspect"] for call in self.calls()))

    def test_oci_native_manifest_is_proved_instead_of_confused_with_config(self):
        self.package("contract", oci=True)
        self.invoke("delivery_import_matches", "contract")

    def test_import_rejects_latest_for_another_version(self):
        (self.target / "manifests/contract.latest").write_text("contract-other.imported\n")
        self.invoke("delivery_import_matches", "contract", False)
        self.assertEqual(self.calls(), [])

    def test_import_rejects_corrupt_record_config_and_package_hash(self):
        for key in ("SOURCE_IMAGE_IDS", "PACKAGE_SHA256"):
            with self.subTest(key=key):
                original = self.records["contract"][key]
                self.records["contract"][key] = "0" * 64
                self.save_record("contract")
                self.invoke("delivery_import_matches", "contract", False)
                self.records["contract"][key] = original

    def test_import_rejects_duplicate_record_field_and_symlink(self):
        path = self.target / "manifests/contract-fixture-v1.imported"
        path.write_text(path.read_text() + "VERSION=fixture-v1\n")
        self.invoke("delivery_import_matches", "contract", False)
        path.unlink()
        path.symlink_to(self.target / ".env")
        self.invoke("delivery_import_matches", "contract", False)

    def test_import_rejects_missing_registry_and_forged_registry_bytes(self):
        key = next(key for key in self.state["registry"] if "/uip/contract/manifests/" in key)
        original = self.state["registry"].pop(key)
        self.invoke("delivery_import_matches", "contract", False)
        self.state["registry"][key] = original + " "
        self.invoke("delivery_import_matches", "contract", False)

    def test_import_rejects_wrong_native_id_or_architecture(self):
        self.state["images"][self.refs["contract"]]["id"] = "sha256:" + "a" * 64
        self.invoke("delivery_import_matches", "contract", False)
        self.state["images"][self.refs["contract"]]["platform"] = "arm64/linux"
        self.invoke("delivery_import_matches", "contract", False)

    def test_import_rejects_external_registry_without_network_attempt(self):
        self.records["contract"]["IMAGE_REF"] = self.refs["contract"].replace(self.address, "example.invalid:5000")
        self.save_record("contract")
        self.invoke("delivery_import_matches", "contract", False)
        self.assertFalse(any(call[0] == "curl" for call in self.calls()))

    def test_common_requires_proxy_record_and_matching_local_infrastructure(self):
        del self.records["common"]["DOCKER_SOCKET_PROXY_IMAGE_REF"]
        self.save_record("common")
        self.invoke("delivery_import_matches", "common", False)
        self.records["common"]["DOCKER_SOCKET_PROXY_IMAGE_REF"] = self.refs["proxy"]
        self.save_record("common")
        self.state["images"]["mysql:8.4"]["id"] = "sha256:" + "b" * 64
        self.invoke("delivery_import_matches", "common", False)

    def test_platform_release_matches_exact_tuple_and_real_runtime(self):
        self.invoke("delivery_release_matches", "platform")
        self.assertFalse(any(call[1:2] in (["network"], ["up"], ["pull"]) for call in self.calls()))

    def test_platform_requires_explicit_common_and_refuses_stale_agent(self):
        self.shell("delivery_release_matches platform " + shlex.quote(str(self.packages["platform"])), False)
        self.state["stale_agent"] = True
        self.invoke("delivery_release_matches", "platform", False)

    def test_platform_refuses_proxy_pointer_drift(self):
        self.release["DOCKER_SOCKET_PROXY_IMAGE"] = self.refs["gateway"]
        self.save_release()
        self.invoke("delivery_release_matches", "platform", False)

    def test_contract_release_requires_all_runtime_and_managed_ready(self):
        self.invoke("delivery_release_matches", "contract")
        saved = self.state["services"].pop("contract-worker")
        self.invoke("delivery_release_matches", "contract", False)
        self.state["services"]["contract-worker"] = saved
        self.state["controlled_ready"] = False
        self.invoke("delivery_release_matches", "contract", False)

    def test_contract_release_refuses_old_container_and_compose_override(self):
        other = "sha256:" + "c" * 64
        self.state["images"][other] = {"id": other}
        self.state["services"]["contract-worker"]["native"] = other
        self.invoke("delivery_release_matches", "contract", False)
        self.state["services"]["contract-worker"]["native"] = self.state["images"][self.refs["contract"]]["id"]
        self.state["compose_services"]["contract-worker"]["image"] = self.refs["project"]
        self.invoke("delivery_release_matches", "contract", False)

    def test_crm_transfer_worker_and_build_id_are_required(self):
        self.invoke("delivery_release_matches", "customer-opportunity")
        saved = self.state["services"].pop("customer-contract-transfer-worker")
        self.invoke("delivery_release_matches", "customer-opportunity", False)
        self.state["services"]["customer-contract-transfer-worker"] = saved
        self.release["CUSTOMER_PRESALE_TEMPORAL_WORKER_BUILD_ID"] = "customer-presale-old"
        self.save_release()
        self.invoke("delivery_release_matches", "customer-opportunity", False)

    def test_bi_release_binds_all_four_images(self):
        self.invoke("delivery_release_matches", "data-analysis")
        self.release[BI_KEYS[3]] = self.refs[BI_KEYS[0]]
        self.save_release()
        self.invoke("delivery_release_matches", "data-analysis", False)

    def test_candidate_ready_waits_without_continue_or_adoption(self):
        self.candidate()
        result = self.shell("subsystem_phase() { printf 'READY_TO_CONTINUE\\n'; }; delivery_prepare_resume contract " + shlex.quote(str(self.packages["contract"])))
        self.assertIn("等待操作员明确执行 continue", result.stdout)
        self.assertIsNotNone(json.loads(self.state_path.read_text())["candidate"])
        self.assertFalse(any(call[1:2] in (["rm"], ["create"]) for call in self.calls()))

    def test_candidate_wrong_image_or_version_is_never_replaced(self):
        self.candidate()
        self.state["candidate"]["Config"]["Labels"]["com.basic-platform.version"] = "old"
        self.shell("subsystem_phase() { printf 'WAITING_FOR_PLATFORM_ADOPTION\\n'; }; delivery_prepare_resume contract " + shlex.quote(str(self.packages["contract"])), False)
        self.assertFalse(any(call[1:2] in (["rm"], ["create"]) for call in self.calls()))

    def test_existing_candidate_waits_and_missing_candidate_alone_dispatches_prepare(self):
        self.candidate()
        self.shell("subsystem_phase() { printf 'WAITING_FOR_PLATFORM_ADOPTION\\n'; }; prepare_subsystem() { return 93; }; delivery_prepare_resume contract " + shlex.quote(str(self.packages["contract"])))
        self.state["candidate"] = None
        fixture_candidate = self.root / "new-candidate.json"
        self.candidate()
        fixture_candidate.write_text(json.dumps(self.state["candidate"]))
        self.state["candidate"] = None
        dispatcher = self.root / "dispatch.py"
        dispatcher.write_text("import json,os,pathlib\np=pathlib.Path(os.environ['DOCKER_STATE'])\ns=json.loads(p.read_text())\nassert s['candidate'] is None\ns['candidate']=json.loads(pathlib.Path(" + repr(str(fixture_candidate)) + ").read_text())\np.write_text(json.dumps(s))\n")
        result = self.shell("subsystem_phase() { printf 'IMPORTED\\n'; }; prepare_subsystem() { python3 " + shlex.quote(str(dispatcher)) + "; }; delivery_prepare_resume contract " + shlex.quote(str(self.packages["contract"])))
        self.assertIn("本次目标候选已准备", result.stdout)

    def test_verified_phase_is_not_allowed_to_hide_another_release(self):
        self.release["CONTRACT_IMAGE"] = self.refs["project"]
        self.save_release()
        self.shell("subsystem_phase() { printf 'VERIFIED\\n'; }; delivery_prepare_resume contract " + shlex.quote(str(self.packages["contract"])), False)


if __name__ == "__main__":
    unittest.main(verbosity=2)
