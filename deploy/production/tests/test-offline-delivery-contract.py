#!/usr/bin/env python3
"""在冻结的目录树中使用 Docker 桩验证实际离线构建器和安装器。"""
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import shutil
import subprocess
import tarfile
import tempfile
import unittest


REPO = Path(__file__).resolve().parents[4]
PRODUCTION = REPO / "platform/deploy/production"
UPSTREAM = "ghcr.io/tecnativa/docker-socket-proxy:v0.5.0@sha256:1f5038b54f06c3e18422902cf00ba21803d1c97805aae032e5e6673d532d3459"
TRANSPORT = "tecnativa/docker-socket-proxy:v0.5.0"
DOCKER = r'''#!/usr/bin/env python3
import hashlib, io, json, os, pathlib, sys, tarfile
args=sys.argv[1:]
with open(os.environ['PROBE_DOCKER_LOG'],'a') as f: f.write(json.dumps(args)+'\n')
if args[:2]==['buildx','version']: print('buildx test-fixed'); sys.exit(0)
if args[:2]==['version','--format']: print('test-fixed'); sys.exit(0)
if args[:2] in (['context','inspect'],['buildx','inspect']): sys.exit(0)
if args[:1]==['info']:
    if '--format' in args: print(os.environ['PROBE_DOCKER_ROOT'])
    sys.exit(0)
if args[:2]==['buildx','build'] or args[:1]==['tag']: sys.exit(0)
if args[:2]==['image','inspect']:
    if os.environ.get('PROBE_MISSING_UPSTREAM')=='1' and args[2].startswith('ghcr.io/tecnativa/'):
        sys.exit(1)
    fmt=args[-1]
    if fmt=='{{.Id}}' and os.environ.get('PROBE_RETAG_MISMATCH')=='1' and args[2]=='tecnativa/docker-socket-proxy:v0.5.0':
        print('sha256:'+'b'*64); sys.exit(0)
    print('linux/amd64' if fmt=='{{.Os}}/{{.Architecture}}' else
          'amd64/linux' if fmt=='{{.Architecture}}/{{.Os}}' else 'sha256:'+'f'*64)
    sys.exit(0)
if args[:1]==['pull']: sys.exit(39)
if args[:2]==['save','--output']:
    blobs={}; descriptors=[]
    for tag in args[3:]:
        config=json.dumps({'architecture':'amd64','os':'linux','config':{'Cmd':[tag]}}).encode()
        config_digest=hashlib.sha256(config).hexdigest(); blobs[config_digest]=config
        manifest=json.dumps({'schemaVersion':2,'mediaType':'application/vnd.oci.image.manifest.v1+json',
            'config':{'mediaType':'application/vnd.oci.image.config.v1+json','digest':'sha256:'+config_digest,'size':len(config)},'layers':[]}).encode()
        manifest_digest=hashlib.sha256(manifest).hexdigest(); blobs[manifest_digest]=manifest
        descriptors.append({'mediaType':'application/vnd.oci.image.manifest.v1+json','digest':'sha256:'+manifest_digest,
            'size':len(manifest),'annotations':{'org.opencontainers.image.ref.name':tag}})
    with tarfile.open(args[2],'w') as archive:
        entries={'index.json':json.dumps({'schemaVersion':2,'manifests':descriptors}).encode(),
                 'oci-layout':b'{"imageLayoutVersion":"1.0.0"}'}
        entries.update({'blobs/sha256/'+key:value for key,value in blobs.items()})
        for name,raw in entries.items():
            info=tarfile.TarInfo(name); info.size=len(raw); archive.addfile(info,io.BytesIO(raw))
    sys.exit(0)
sys.exit('Unexpected Docker stub invocation: '+repr(args))
'''


class DeliveryContract(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="offline-delivery-contract-")
        self.root = Path(self.temp.name).resolve()
        self.workspace = self.root / "workspace"
        self.production = self.workspace / "platform/deploy/production"
        shutil.copytree(PRODUCTION, self.production, ignore=shutil.ignore_patterns(
            "runtime", "backups", "packages", "manifests", ".env", ".release.env",
            "*.pem", "*.key", "*.bak", "*.backup", "*.dump", "dump*.sql", "__pycache__", "*.pyc"))
        for module in ("platform", "customer_and_opportunity", "contract_management", "project_management", "Settlement", "data_analysis"):
            for directory in ("cmd", "internal", "migrations", "authz"):
                (self.workspace / module / directory).mkdir(parents=True, exist_ok=True)
            for name in ("Dockerfile", ".dockerignore", "go.mod", "go.sum", "docker-entrypoint.sh"):
                source = REPO / module / name
                if source.is_file():
                    shutil.copyfile(source, self.workspace / module / name)
        for directory in ("src", "public", "nginx"):
            (self.workspace / "frontend" / directory).mkdir(parents=True, exist_ok=True)
        for name in ("Dockerfile", ".dockerignore", "package.json", "package-lock.json", "index.html", "login.html", "vite.config.js"):
            shutil.copyfile(REPO / "frontend" / name, self.workspace / "frontend" / name)
        for name in ("一键制作离线镜像包.sh", "服务器一键离线部署.sh", "系统运维工具.sh", "清理首次部署产物.sh", "license-activate.sh", "license-upgrade.sh", "forward-recover.sh"):
            shutil.copyfile(REPO / name, self.workspace / name)
        (self.workspace / "操作说明").mkdir()
        shutil.copyfile(REPO / "操作说明/失败首装清理与重新部署.md",
                        self.workspace / "操作说明/失败首装清理与重新部署.md")
        for name in ("third_party/license-core/license.go", "scripts/license-core-sync.sh", "scripts/license-core.sha256"):
            path = self.workspace / "platform" / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text("reviewed fixture input\n")
        self.tools = self.root / "tools"
        self.tools.mkdir()
        (self.tools / "docker").write_text(DOCKER)
        (self.tools / "docker").chmod(0o755)
        if not shutil.which("flock"):
            (self.tools / "flock").write_text("#!/bin/sh\nexit 0\n")
            (self.tools / "flock").chmod(0o755)
        (self.tools / "df").write_text("#!/bin/sh\nprintf 'Filesystem 1024-blocks Used Available Capacity Mounted on\\nfixture 999999999 1 999999998 1%% /\\n'\n")
        (self.tools / "df").chmod(0o755)
        self.log = self.root / "docker.log"
        self.env = dict(os.environ, PATH=str(self.tools)+os.pathsep+os.environ["PATH"],
                        PROBE_DOCKER_LOG=str(self.log), PROBE_DOCKER_ROOT=str(self.root),
                        OFFLINE_PULL_RETRIES="1", OFFLINE_PULL_RETRY_DELAY_SECONDS="1", PYTHONDONTWRITEBYTECODE="1")

    def tearDown(self):
        self.temp.cleanup()

    def command(self, args, *, succeeds=True, env=None):
        result = subprocess.run(args, env=env or self.env, text=True, capture_output=True)
        self.assertEqual(result.returncode == 0, succeeds, result.stdout+result.stderr)
        return result

    def build(self, component, output, env=None):
        return self.command(["bash", str(self.production / "bin/build-offline-packages.sh"),
                             "--component", component, "--version", "reviewed-v1", "--output", str(output)], env=env)

    def test_common_uses_exact_fixed_source_without_proxy_rebuild(self):
        output = self.root / "common-output"
        self.build("common", output)
        archive = output / "common-infrastructure-linux-amd64.tar.gz"
        with tarfile.open(archive) as tar:
            metadata = dict(line.split("=", 1) for line in tar.extractfile("package.env").read().decode().splitlines())
        self.assertEqual(metadata["IMAGES"].split(",")[7], TRANSPORT)
        self.assertEqual(metadata["DOCKER_SOCKET_PROXY_UPSTREAM_REF"], UPSTREAM)
        self.assertEqual(metadata["DOCKER_SOCKET_PROXY_CONFIG_DIGEST"], metadata["IMAGE_CONFIG_DIGESTS"].split(",")[7])
        calls = [json.loads(line) for line in self.log.read_text().splitlines()]
        self.assertIn(["tag", UPSTREAM, TRANSPORT], calls)
        proxy_builds = [call for call in calls if call[:2] == ["buildx", "build"] and
                        any("BASE_IMAGE="+TRANSPORT == item or "BASE_IMAGE="+UPSTREAM == item for item in call)]
        self.assertEqual(proxy_builds, [])

    def test_missing_fixed_source_cannot_fall_back_to_old_transport_tag(self):
        result = self.command(["bash", str(self.production / "bin/build-offline-packages.sh"),
                               "--component", "common", "--version", "missing-source", "--output", str(self.root / "missing-output")],
                              env=dict(self.env, PROBE_MISSING_UPSTREAM="1"), succeeds=False)
        self.assertIn("公共镜像拉取失败", result.stderr)
        calls = [json.loads(line) for line in self.log.read_text().splitlines()]
        self.assertIn(["pull", "--platform", "linux/amd64", UPSTREAM], calls)
        self.assertNotIn(["tag", UPSTREAM, TRANSPORT], calls)

    def test_retagged_proxy_must_have_the_actual_fixed_source_identity(self):
        output = self.root / "mismatched-proxy"
        result = self.command(["bash", str(self.production / "bin/build-offline-packages.sh"),
                               "--component", "common", "--version", "mismatched-source", "--output", str(output)],
                              env=dict(self.env, PROBE_RETAG_MISMATCH="1"), succeeds=False)
        self.assertIn("公共代理传输标签与固定上游实际镜像不一致", result.stderr)
        self.assertFalse((output / "common-infrastructure-linux-amd64.tar.gz").exists())

    def test_real_asset_install_can_load_fresh_inventory_collector(self):
        output = self.root / "asset-output"
        self.build("assets", output)
        target = self.root / "installed"
        self.command(["bash", str(output / "install-assets.sh"), str(output / "deployment-assets-reviewed-v1.tar.gz"), str(target)])
        self.assertEqual((target / "bin/export-license-inventory.py").read_bytes(),
                         (PRODUCTION / "bin/export-license-inventory.py").read_bytes())
        spec = importlib.util.spec_from_file_location("fresh_approval_dependency_probe", REPO / "commercial-license-tool/scripts/export-fresh-approval-input.py")
        module = importlib.util.module_from_spec(spec)
        # 执行已捕获的文件，避免将 __pycache__ 写入仓库。
        exec(compile(Path(spec.origin).read_bytes(), spec.origin, "exec"), module.__dict__)
        collector, _, _ = module.installed_collector(target)
        self.assertTrue(callable(collector.collect))
        self.assertTrue(callable(collector.write_inventory))
        self.assertFalse(list(target.rglob("__pycache__")))

    def fingerprint(self):
        helper = self.production / "bin/offline-build-inputs.sh"
        result = self.command(["bash", "-Eeuo", "pipefail", "-c",
                               'source "$1"; offline_build_input_fingerprint "$2" shared',
                               "fixture", str(helper), str(self.workspace)])
        return result.stdout.strip()

    def test_shared_license_inputs_reject_extend_before_any_build(self):
        baseline = self.root / "baseline"
        self.build("assets", baseline)
        packages = {}
        for component, name in (("common", "common-infrastructure-linux-amd64.tar.gz"),
                                ("platform", "platform-backend-base-v1-linux-amd64.tar.gz"),
                                ("frontend", "frontend-base-v1-linux-amd64.tar.gz")):
            stage = self.root / ("stage-"+component)
            stage.mkdir()
            (stage / "package.env").write_text("PACKAGE_FORMAT=2\nCOMPONENT="+component+"\nVERSION=base-v1\nPLATFORM=linux/amd64\n")
            (stage / "images.tar").write_bytes(b"transport fixture only")
            (stage / "SHA256SUMS").write_text(hashlib.sha256((stage / "images.tar").read_bytes()).hexdigest()+"  images.tar\n")
            archive = baseline / name
            with tarfile.open(archive, "w:gz") as tar:
                for entry in ("package.env", "images.tar", "SHA256SUMS"):
                    tar.add(stage / entry, arcname=entry)
            Path(str(archive)+".sha256").write_text(hashlib.sha256(archive.read_bytes()).hexdigest()+"  "+name+"\n")
            packages[component.upper()] = name
        packages["ASSETS"] = "deployment-assets-reviewed-v1.tar.gz"
        compatibility = "FORMAT=1\nPLATFORM_ASSETS_INPUT_SHA256="+self.fingerprint()+"\n"
        for component, name in packages.items():
            compatibility += component+"_PACKAGE="+name+"\n"+component+"_PACKAGE_SHA256="+hashlib.sha256((baseline / name).read_bytes()).hexdigest()+"\n"
        (baseline / "PLATFORM_ASSETS_COMPATIBILITY.env").write_text(compatibility)
        (baseline / "SHA256SUMS").write_text("".join(hashlib.sha256(path.read_bytes()).hexdigest()+"  "+path.name+"\n"
                                                       for path in sorted(baseline.iterdir()) if path.name != "SHA256SUMS"))
        invocation = ["bash", str(self.workspace / "一键制作离线镜像包.sh"), "--version", "extension-v2", "--output", str(self.root / "extension"),
                      "--extend-from", str(baseline), "--components", "contract", "--skip-host-deps", "--preflight-only", "--no-pause"]
        self.command(invocation)
        for relative in ("third_party/license-core/license.go", "scripts/license-core-sync.sh", "scripts/license-core.sha256"):
            path = self.workspace / "platform" / relative
            original = path.read_bytes()
            path.write_bytes(original+b"changed shared licensing input\n")
            result = self.command(invocation, succeeds=False)
            self.assertIn("当前 Platform/前端/共享部署输入已与基线不同", result.stderr)
            self.assertFalse((self.root / "extension").exists())
            path.write_bytes(original)


if __name__ == "__main__":
    unittest.main()
