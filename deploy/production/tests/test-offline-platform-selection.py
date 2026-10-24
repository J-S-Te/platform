#!/usr/bin/env python3
"""使用严格的多平台 Docker 夹具验证实际打包器。

仅写入临时文件。模拟客户端不使用 Docker 套接字、网络、运行中的容器
或源码检出目录。实际归档元数据解析器仍验证每个输出镜像的
架构及配置摘要。
"""
import hashlib
import io
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tarfile
import tempfile
import unittest


PRODUCTION = Path(__file__).resolve().parents[1]
UPSTREAM = ("ghcr.io/tecnativa/docker-socket-proxy:v0.5.0@sha256:"
            "1f5038b54f06c3e18422902cf00ba21803d1c97805aae032e5e6673d532d3459")
TRANSPORT = "tecnativa/docker-socket-proxy:v0.5.0"
VERSION = "qa-platform-selection"


DOCKER = r'''
import hashlib, io, json, os, pathlib, sys, tarfile
a = sys.argv[1:]
log = pathlib.Path(os.environ['UIP_SELECTION_LOG'])
with log.open('a') as f:
    f.write(json.dumps(a) + '\n')
store = pathlib.Path(os.environ['UIP_SELECTION_STORE'])
state = json.loads(store.read_text()) if store.exists() else {'images': {}, 'pulled': [], 'saved': False}
upstream = os.environ['UIP_SELECTION_UPSTREAM']
transport = 'tecnativa/docker-socket-proxy:v0.5.0'
def persist(): store.write_text(json.dumps(state, sort_keys=True))
def flag(name, default='1'): return os.environ.get('UIP_SELECTION_' + name, default) == '1'
def raw(value): return json.dumps(value, sort_keys=True, separators=(',', ':')).encode()
def digest(value): return hashlib.sha256(value).hexdigest()
def config(ref):
    if ref not in state['images']:
        state['images'][ref] = {'architecture': 'amd64', 'os': 'linux', 'config': {'Cmd': [ref]}}
        persist()
    return state['images'][ref]
def fail(message): sys.exit('fixture: ' + message)
def option(args, name):
    if name not in args or args.index(name) + 1 >= len(args): fail('missing ' + name)
    index = args.index(name)
    value = args[index + 1]
    return value, args[:index] + args[index + 2:]
if a == ['buildx', 'version']:
    print('github.com/docker/buildx qa-platform-selection'); sys.exit(0)
if a[:2] == ['version', '--format'] and len(a) == 3:
    fields = {
        '{{.Client.APIVersion}}': os.environ.get('UIP_SELECTION_CLIENT_API', '1.54'),
        '{{.Server.APIVersion}}': os.environ.get('UIP_SELECTION_SERVER_API', '1.54'),
        '{{.Client.Version}}': '29.0.0-fixture',
        '{{.Server.Version}}': '29.0.0-fixture',
    }
    if a[2] not in fields: fail('unknown version format ' + a[2])
    print(fields[a[2]]); sys.exit(0)
if a in (['image', 'inspect', '--help'], ['save', '--help'], ['image', 'save', '--help']):
    kind = 'INSPECT_HELP' if a[:2] == ['image', 'inspect'] else 'SAVE_HELP'
    print('Usage: docker fixture\nOptions:\n  --format string')
    if flag(kind): print('  --platform string    Select platform')
    sys.exit(0)
if a[:2] == ['image', 'inspect']:
    args = a[2:]
    explicit = '--platform' in args
    if explicit:
        if not flag('INSPECT_HELP'): fail('inspect CLI has no platform option')
        platform, args = option(args, '--platform')
        if platform != 'linux/amd64': fail('unexpected inspect platform')
    fmt, args = option(args, '--format')
    if len(args) != 1: fail('unexpected inspect arguments ' + repr(args))
    ref = args[0]
    if explicit and flag('INSPECT_ERROR', '0'): fail('selected-platform inspection failed')
    if ref == 'mysql:8.4' and not flag('TARGET_PRESENT') and ref not in state['pulled']:
        if explicit or os.environ.get('UIP_SELECTION_DEFAULT_ARCH', 'arm64') != 'arm64':
            fail('requested platform is not cached')
    value = dict(config(ref))
    if not explicit:
        value['architecture'] = os.environ.get('UIP_SELECTION_DEFAULT_ARCH', 'arm64')
    if state['saved'] and flag('PROXY_DRIFT_AFTER_SAVE', '0') and ref == transport:
        value['config'] = {'Cmd': ['unexpected replacement after export']}
    formats = {
        '{{.Os}}/{{.Architecture}}': value['os'] + '/' + value['architecture'],
        '{{.Architecture}}/{{.Os}}': value['architecture'] + '/' + value['os'],
        '{{.Id}}': 'sha256:' + digest(raw(value)),
    }
    if fmt not in formats: fail('unexpected image format ' + fmt)
    print(formats[fmt]); sys.exit(0)
if a[:2] == ['pull', '--platform'] and len(a) == 4:
    if a[2] != 'linux/amd64': fail('unexpected pull platform')
    if flag('PULL_ERROR', '0'): fail('simulated pull failure')
    ref = a[3]
    state['pulled'].append(ref)
    value = config(ref)
    value['architecture'] = os.environ.get('UIP_SELECTION_PULL_ARCH', 'amd64')
    persist(); sys.exit(0)
if a[:2] == ['buildx', 'build']:
    args = a[2:]
    platform, args = option(args, '--platform')
    ref, args = option(args, '--tag')
    if platform != 'linux/amd64' or '--load' not in args or '--pull=false' not in args:
        fail('unbounded or wrong-platform build')
    state['images'][ref] = {'architecture': os.environ.get('UIP_SELECTION_BUILD_ARCH', 'amd64'),
                            'os': 'linux', 'config': {'Cmd': [ref]}}
    persist(); sys.exit(0)
if len(a) == 3 and a[0] == 'tag':
    state['images'][a[2]] = config(a[1])
    if flag('PROXY_DRIFT_ON_TAG', '0') and a[2] == transport:
        state['images'][a[2]] = {'architecture': 'amd64', 'os': 'linux',
                                'config': {'Cmd': ['unexpected replacement at retag']}}
    persist(); sys.exit(0)
if a and a[0] == 'save':
    args = a[1:]
    explicit = '--platform' in args
    if explicit:
        if not flag('SAVE_HELP'): fail('save CLI has no platform option')
        platform, args = option(args, '--platform')
        if platform != 'linux/amd64': fail('unexpected export platform')
    path, refs = option(args, '--output')
    if not refs or any(ref.startswith('-') for ref in refs): fail('unexpected export references')
    if flag('SAVE_ERROR', '0'): fail('selected-platform export failed')
    entries = {}; descriptors = []
    def blob(value):
        data = raw(value); identifier = digest(data)
        entries['blobs/sha256/' + identifier] = data
        return {'digest': 'sha256:' + identifier, 'size': len(data)}
    for ref in refs:
        value = config(ref)
        # Old engines export their single stored architecture. Modern engines
        # without an explicit selector may export both locally cached variants.
        variants = [value]
        if not explicit and os.environ.get('UIP_SELECTION_DEFAULT_ARCH', 'arm64') == 'arm64':
            variants += [dict(value, architecture='arm64')]
        manifests = []
        for variant in variants:
            cd = blob(variant); cd['mediaType'] = 'application/vnd.oci.image.config.v1+json'
            md = blob({'schemaVersion': 2, 'mediaType': 'application/vnd.oci.image.manifest.v1+json',
                       'config': cd, 'layers': []})
            md.update(mediaType='application/vnd.oci.image.manifest.v1+json',
                      platform={'os': variant['os'], 'architecture': variant['architecture']})
            manifests.append(md)
        if len(manifests) == 1:
            descriptor = manifests[0]
        else:
            descriptor = blob({'schemaVersion': 2, 'mediaType': 'application/vnd.oci.image.index.v1+json',
                               'manifests': manifests})
            descriptor['mediaType'] = 'application/vnd.oci.image.index.v1+json'
        descriptor['annotations'] = {'org.opencontainers.image.ref.name': ref}
        descriptors.append(descriptor)
    entries['index.json'] = raw({'schemaVersion': 2, 'manifests': descriptors})
    entries['oci-layout'] = b'{"imageLayoutVersion":"1.0.0"}'
    with tarfile.open(path, 'w') as archive:
        for name, value in entries.items():
            member = tarfile.TarInfo(name); member.size = len(value)
            archive.addfile(member, io.BytesIO(value))
    state['saved'] = True; persist(); sys.exit(0)
fail('unexpected Docker invocation ' + repr(a))
'''


class OfflinePlatformSelection(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="uip-offline-platform-selection-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.workspace = self.root / "workspace"
        self.production = self.workspace / "platform/deploy/production"
        (self.production / "bin").mkdir(parents=True)
        # 完整生产脚本保持原样执行；使用小型夹具工作区，避免源指纹扫描到用户的无关改动。
        for name in ("build-offline-packages.sh", "offline-package-metadata.sh", "offline-build-inputs.sh", "build-evidence.py"):
            shutil.copyfile(PRODUCTION / "bin" / name, self.production / "bin" / name)
        for module in ("platform", "customer_and_opportunity", "contract_management",
                       "project_management", "Settlement", "data_analysis"):
            for directory in ("cmd", "internal", "migrations", "authz"):
                (self.workspace / module / directory).mkdir(parents=True, exist_ok=True)
        shutil.copyfile(PRODUCTION.parents[1] / "Dockerfile", self.workspace / "platform/Dockerfile")
        for directory in ("src", "public", "nginx"):
            (self.workspace / "frontend" / directory).mkdir(parents=True)
        (self.production / "bin/offline-base.Dockerfile").write_text("ARG BASE_IMAGE\nFROM ${BASE_IMAGE}\n")
        (self.production / "docker-compose.yml").write_text("services: {}\n")
        self.tools = self.root / "tools"
        self.tools.mkdir()
        docker = self.tools / "docker"
        docker.write_text("#!" + sys.executable + "\n" + DOCKER)
        docker.chmod(0o755)
        self.log = self.root / "docker.jsonl"
        self.output = self.root / "output"
        self.env = dict(os.environ)
        for name in ("DOCKER_API_VERSION", "OFFLINE_RELEASE_VERSION", "OFFLINE_OUTPUT_DIR",
                     "OFFLINE_GOSUMDB", "OFFLINE_GOPROXY", "OFFLINE_NPM_CONFIG_REGISTRY"):
            self.env.pop(name, None)
        self.env.update(PATH=str(self.tools) + os.pathsep + self.env["PATH"],
                        UIP_SELECTION_LOG=str(self.log), UIP_SELECTION_STORE=str(self.root / "images.json"),
                        UIP_SELECTION_UPSTREAM=UPSTREAM, OFFLINE_PULL_RETRIES="2",
                        OFFLINE_PULL_RETRY_DELAY_SECONDS="1")

    def build(self, *, component="common", success=True, settings=None, docker_api=None):
        env = dict(self.env)
        env.update({"UIP_SELECTION_" + key: value for key, value in (settings or {}).items()})
        if docker_api is not None:
            env["DOCKER_API_VERSION"] = docker_api
        result = subprocess.run(["bash", str(self.production / "bin/build-offline-packages.sh"),
                                 "--component", component, "--version", VERSION,
                                 "--output", str(self.output)],
                                env=env, capture_output=True, text=True, timeout=60)
        self.assertEqual(result.returncode == 0, success, result.stdout + result.stderr)
        return result

    def calls(self, operation):
        calls = [json.loads(line) for line in self.log.read_text().splitlines()]
        if operation == "inspect":
            return [call for call in calls if call[:2] == ["image", "inspect"] and "--help" not in call]
        return [call for call in calls if call[0] == operation and "--help" not in call]

    def assert_selectors(self, *, inspect, save):
        for kind, expected in (("inspect", inspect), ("save", save)):
            calls = self.calls(kind)
            self.assertTrue(calls, "missing " + kind + " call")
            for call in calls:
                self.assertEqual("--platform" in call, expected, call)
                if expected:
                    self.assertEqual(call[call.index("--platform") + 1], "linux/amd64", call)

    def assert_common_archive(self):
        package = self.output / "common-infrastructure-linux-amd64.tar.gz"
        self.assertTrue(package.is_file())
        with tarfile.open(package) as outer:
            metadata = dict(line.split("=", 1) for line in outer.extractfile("package.env").read().decode().splitlines())
            images = outer.extractfile("images.tar").read()
        self.assertEqual(metadata["DOCKER_SOCKET_PROXY_UPSTREAM_REF"], UPSTREAM)
        with tarfile.open(fileobj=io.BytesIO(images)) as inner:
            index = json.load(inner.extractfile("index.json"))
            refs = {}
            for descriptor in index["manifests"]:
                manifest = json.load(inner.extractfile("blobs/sha256/" + descriptor["digest"].split(":")[1]))
                self.assertNotIn("manifests", manifest, "export included another platform")
                config_id = manifest["config"]["digest"]
                config_bytes = inner.extractfile("blobs/sha256/" + config_id.split(":")[1]).read()
                self.assertEqual(config_id, "sha256:" + hashlib.sha256(config_bytes).hexdigest())
                value = json.loads(config_bytes)
                self.assertEqual((value["os"], value["architecture"]), ("linux", "amd64"))
                refs[descriptor["annotations"]["org.opencontainers.image.ref.name"]] = config_id
        self.assertEqual(metadata["DOCKER_SOCKET_PROXY_CONFIG_DIGEST"], refs[TRANSPORT])
        self.assertEqual(metadata["IMAGE_CONFIG_DIGESTS"].split(","),
                         [refs[ref] for ref in metadata["IMAGES"].split(",")])

    def test_cached_amd64_is_selected_even_when_default_inspection_returns_arm64(self):
        self.build()
        self.assertEqual(self.calls("pull"), [], "cached amd64 was needlessly pulled")
        self.assert_selectors(inspect=True, save=True)
        self.assert_common_archive()

    def test_missing_amd64_is_pulled_and_reinspected_with_platform(self):
        self.build(settings={"TARGET_PRESENT": "0"})
        self.assertEqual(self.calls("pull"), [["pull", "--platform", "linux/amd64", "mysql:8.4"]])
        mysql_inspects = [call for call in self.calls("inspect") if "mysql:8.4" in call and "{{.Os}}/{{.Architecture}}" in call]
        self.assertEqual(len(mysql_inspects), 2)
        self.assert_selectors(inspect=True, save=True)
        self.assert_common_archive()

    def test_successful_pull_returning_wrong_architecture_is_rejected(self):
        self.build(success=False, settings={"TARGET_PRESENT": "0", "PULL_ARCH": "arm64"})
        self.assertEqual(len(self.calls("pull")), 1)
        self.assertEqual(self.calls("save"), [])
        self.assertFalse(list(self.output.glob("*.tar.gz")))
        self.assertTrue(all("--platform" in call for call in self.calls("inspect")))

    def test_platform_inspection_failure_never_falls_back_to_default(self):
        self.build(success=False, settings={"INSPECT_ERROR": "1", "DEFAULT_ARCH": "amd64"})
        self.assertTrue(all("--platform" in call for call in self.calls("inspect")))
        self.assertEqual(self.calls("save"), [])

    def test_wrong_built_architecture_is_rejected_even_with_correct_default_view(self):
        self.build(component="platform", success=False,
                   settings={"BUILD_ARCH": "arm64", "DEFAULT_ARCH": "amd64"})
        self.assertEqual(self.calls("save"), [])
        self.assertTrue(all("--platform" in call for call in self.calls("inspect")))

    def test_api_148_enables_save_but_keeps_strict_legacy_inspection(self):
        self.build(settings={"CLIENT_API": "1.48", "SERVER_API": "1.48", "DEFAULT_ARCH": "amd64"})
        self.assert_selectors(inspect=False, save=True)
        self.assert_common_archive()

    def test_old_server_cannot_be_overridden_by_new_client(self):
        self.build(settings={"SERVER_API": "1.47", "DEFAULT_ARCH": "amd64"})
        self.assert_selectors(inspect=False, save=False)
        self.assert_common_archive()

    def test_old_client_cannot_use_new_server_platform_options(self):
        self.build(settings={"CLIENT_API": "1.47", "DEFAULT_ARCH": "amd64"})
        self.assert_selectors(inspect=False, save=False)

    def test_explicit_api_override_caps_modern_capabilities(self):
        self.build(docker_api="1.47", settings={"DEFAULT_ARCH": "amd64"})
        self.assert_selectors(inspect=False, save=False)

    def test_high_api_override_does_not_raise_old_server_capability(self):
        self.build(docker_api="1.54", settings={"SERVER_API": "1.47", "DEFAULT_ARCH": "amd64"})
        self.assert_selectors(inspect=False, save=False)

    def test_invalid_api_override_uses_strict_legacy_checks(self):
        self.build(docker_api="not-an-api", settings={"DEFAULT_ARCH": "amd64"})
        self.assert_selectors(inspect=False, save=False)

    def test_unknown_reported_api_uses_strict_legacy_checks(self):
        self.build(settings={"CLIENT_API": "unknown", "DEFAULT_ARCH": "amd64"})
        self.assert_selectors(inspect=False, save=False)

    def test_old_cli_help_prevents_unsupported_options_even_on_new_server(self):
        self.build(settings={"INSPECT_HELP": "0", "SAVE_HELP": "0", "DEFAULT_ARCH": "amd64"})
        self.assert_selectors(inspect=False, save=False)
        self.assert_common_archive()

    def test_inspect_and_save_cli_support_are_detected_independently(self):
        self.build(settings={"SAVE_HELP": "0", "DEFAULT_ARCH": "amd64"})
        self.assert_selectors(inspect=True, save=False)

    def test_legacy_wrong_architecture_remains_rejected(self):
        self.build(success=False, settings={"CLIENT_API": "1.47", "PULL_ARCH": "arm64"})
        self.assertEqual(len(self.calls("pull")), 1)
        self.assertTrue(all("--platform" not in call for call in self.calls("inspect")))
        self.assertEqual(self.calls("save"), [])

    def test_legacy_inspect_cannot_hide_wrong_architecture_in_saved_bytes(self):
        result = self.build(component="platform", success=False,
                            settings={"CLIENT_API": "1.47", "BUILD_ARCH": "arm64",
                                      "DEFAULT_ARCH": "amd64"})
        self.assert_selectors(inspect=False, save=False)
        self.assertIn("无法从 Docker 归档读取镜像配置摘要", result.stderr)
        self.assertFalse(list(self.output.glob("*.tar.gz")))

    def test_selected_platform_save_failure_never_retries_without_selector(self):
        self.build(success=False, settings={"SAVE_ERROR": "1"})
        self.assertEqual(len(self.calls("save")), 1)
        self.assertTrue("--platform" in self.calls("save")[0])
        self.assertFalse(list(self.output.glob("*.tar.gz")))

    def test_proxy_transport_identity_is_checked_immediately_after_tagging(self):
        self.build(success=False, settings={"PROXY_DRIFT_ON_TAG": "1"})
        self.assertEqual(self.calls("save"), [])
        self.assertFalse(list(self.output.glob("*.tar.gz")))

    def test_proxy_transport_identity_drift_during_export_blocks_publication(self):
        self.build(success=False, settings={"PROXY_DRIFT_AFTER_SAVE": "1"})
        self.assertEqual(len(self.calls("save")), 1)
        self.assertTrue(all("--platform" in call for call in self.calls("inspect")))
        self.assertFalse(list(self.output.glob("*.tar.gz")))


if __name__ == "__main__":
    unittest.main(verbosity=2)
