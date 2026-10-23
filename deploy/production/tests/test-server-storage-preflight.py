#!/usr/bin/env python3
"""使用严格的本地命令行替身验证服务器只读存储预检。"""
import hashlib
import json
import os
from pathlib import Path
import shlex
import shutil
import subprocess
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[4]
WRAPPER = ROOT / "服务器一键离线部署.sh"
SOURCE = WRAPPER.read_text()
BASH = os.environ.get("TEST_STORAGE_BASH", shutil.which("bash") or "bash")
GIB_KIB = 1024 * 1024
DRIVER_FORMAT = '{{range .DriverStatus}}{{if eq (index . 0) "driver-type"}}{{index . 1}}{{end}}{{end}}'


def function(name):
    return name + "() {" + SOURCE.split(name + "() {", 1)[1].split("\n}", 1)[0] + "\n}"


def associative_arrays_available():
    result = subprocess.run([BASH, "-c", "set -e; ((BASH_VERSINFO[0] > 4 || (BASH_VERSINFO[0] == 4 && BASH_VERSINFO[1] >= 3))); declare -A probe=(); probe[a]=1; [[ ${probe[a]} == 1 ]]"],
                            capture_output=True, text=True)
    return result.returncode == 0


CLI = r'''#!/usr/bin/env python3
import json, os, pathlib, sys
name=pathlib.Path(sys.argv[0]).name; args=sys.argv[1:]
facts=json.loads(pathlib.Path(os.environ['STORAGE_FACTS']).read_text())
with open(os.environ['STORAGE_CALLS'],'a') as log: log.write(json.dumps([name]+args)+'\n')
if name=='docker' and args==['info','--format','{{range .DriverStatus}}{{if eq (index . 0) "driver-type"}}{{index . 1}}{{end}}{{end}}']:
    print(facts['status']); sys.exit(facts.get('docker_exit',0))
if name=='systemctl' and len(args)==6 and args[0]=='show' and args[2:]==['-p','ExecStart','--value','--no-pager']:
    if args[1] not in ('docker.service','containerd.service'): sys.exit(91)
    print(facts['docker_exec' if args[1]=='docker.service' else 'containerd_exec'])
    sys.exit(facts.get('systemctl_exit',0))
if name=='containerd' and args[-2:]==['config','dump']:
    print(facts['dump']); sys.exit(facts.get('containerd_exit',0))
if name=='df' and len(args)==2 and args[0] in ('-Pk','-Pi'):
    entry=facts['filesystems'].get(args[1])
    if entry is None: sys.exit(92)
    if args[0]=='-Pk':
        print('Filesystem 1024-blocks Used Available Capacity Mounted on')
        print(entry['device'],999999999,1,entry['available'],'1%',entry['mount'])
    else:
        print('Filesystem Inodes IUsed IFree IUse% Mounted on')
        print(entry['device'],999999999,1,entry['inodes'],'1%',entry['mount'])
    sys.exit(0)
if name=='du' and args[:1]==['-sk']:
    print(facts.get('selected_media_kib',1024),args[1]); sys.exit(0)
sys.exit('Unexpected or mutating command refused by storage fixture: '+repr([name]+args))
'''


class StorageFixture(unittest.TestCase):
    def setUp(self):
        preferred = "/private/tmp" if Path("/private/tmp").is_dir() else None
        self.temp = tempfile.TemporaryDirectory(prefix="server-storage-preflight-", dir=preferred)
        self.addCleanup(self.temp.cleanup)
        self.path = Path(self.temp.name).resolve()
        self.docker = self.path / "docker-root"
        self.containerd = self.path / "containerd-root"
        self.target = self.path / "target-parent"
        self.tools = self.path / "tools"
        for path in (self.docker, self.containerd, self.target, self.tools):
            path.mkdir()
        for name in ("docker", "systemctl", "containerd", "df", "du"):
            executable = self.tools / name
            executable.write_text(CLI)
            executable.chmod(0o755)
        self.calls = self.path / "calls.jsonl"
        self.facts_file = self.path / "facts.json"
        self.facts = dict(status="", docker_exec="{ path=/usr/bin/dockerd ; argv[]=/usr/bin/dockerd -H fd:// --containerd=/run/containerd/containerd.sock ; }",
                          containerd_exec="{ path=/usr/bin/containerd ; argv[]=/usr/bin/containerd ; }",
                          dump=self.dump(), filesystems={})
        self.env = dict(os.environ, PATH=str(self.tools)+os.pathsep+os.environ["PATH"],
                        STORAGE_FACTS=str(self.facts_file), STORAGE_CALLS=str(self.calls),
                        PYTHONDONTWRITEBYTECODE="1")
        self.base_code = "set -Eeuo pipefail\ndie() { echo \"$*\" >&2; exit 7; }; info() { echo \"$*\"; };\n"
        self.base_code += function("containerd_config_value")+"\n"+function("detect_docker_image_storage")+"\n"
        self.base_code += "docker_root="+shlex.quote(str(self.docker))+"\n"

    def dump(self, root=None, quote="'", address="/run/containerd/containerd.sock"):
        return ("version = 3\nroot = "+quote+str(root or self.containerd)+quote+"\nstate = '/run/containerd'\n"
                "[grpc]\naddress = "+quote+address+quote+"\n")

    def dump_top_level_address(self, root=None, quote="'", address="/run/containerd/containerd.sock"):
        # containerd 2.0–2.2 的配置 v3 将 gRPC 套接字作为顶层键，仅将 uid/gid 保留在 [grpc] 下。
        return ("version = 3\nroot = "+quote+str(root or self.containerd)+quote+"\nstate = '/run/containerd'\n"
                "address = "+quote+address+quote+"\n[grpc]\n  uid = 0\n  gid = 0\n")

    def run_code(self, code, *, succeeds=True):
        self.facts_file.write_text(json.dumps(self.facts))
        self.calls.write_text("")
        result = subprocess.run([BASH, "-c", self.base_code+code], env=self.env,
                                capture_output=True, text=True)
        self.assertEqual(result.returncode == 0, succeeds, result.stdout+result.stderr)
        return result

    def detect(self, *, succeeds=True):
        return self.run_code('detect_docker_image_storage\nprintf "STORE=%s\\nROOT=%s\\n" "$image_store" "$containerd_root"\n', succeeds=succeeds)

    def commands(self):
        return [json.loads(line) for line in self.calls.read_text().splitlines()]

    def external(self):
        self.facts["status"] = "io.containerd.snapshotter.v1"

class StorageDetectionTests(StorageFixture):
    def test_classic_does_not_query_system_daemons(self):
        for status in ("",):
            with self.subTest(status=status):
                self.facts["status"] = status
                result = self.detect()
                self.assertIn("STORE=classic\nROOT=", result.stdout)
                self.assertEqual(self.commands(), [["docker", "info", "--format", DRIVER_FORMAT]])

    def test_external_root_in_single_or_double_quotes(self):
        self.external()
        for quote in ("'", '"'):
            with self.subTest(quote=quote):
                self.facts["dump"] = self.dump(quote=quote)
                result = self.detect()
                self.assertIn("STORE=containerd", result.stdout)
                self.assertIn("ROOT="+str(self.containerd), result.stdout)
                self.assertIn(["containerd", "config", "dump"], self.commands())

    def test_containerd_v3_top_level_address_is_accepted(self):
        self.external()
        for quote in ("'", '"'):
            with self.subTest(quote=quote):
                self.facts["dump"] = self.dump_top_level_address(quote=quote)
                result = self.detect()
                self.assertIn("STORE=containerd", result.stdout)
                self.assertIn("ROOT="+str(self.containerd), result.stdout)
                self.assertIn(["containerd", "config", "dump"], self.commands())

    def test_agreeing_top_level_and_grpc_section_addresses_are_accepted(self):
        self.external()
        self.facts["dump"] = self.dump().replace("[grpc]", "address = '/run/containerd/containerd.sock'\n[grpc]")
        result = self.detect()
        self.assertIn("STORE=containerd", result.stdout)
        self.assertIn("ROOT="+str(self.containerd), result.stdout)

    def test_conflicting_top_level_address_refuses_preflight(self):
        self.external()
        self.facts["dump"] = self.dump().replace("[grpc]", "address = '/run/unrelated/containerd.sock'\n[grpc]")
        self.assertIn("不一致", self.detect(succeeds=False).stderr)

    def test_docker_endpoint_argument_forms(self):
        self.external()
        for endpoint in ("=/run/containerd/containerd.sock", " /run/containerd/containerd.sock", "=unix:///run/containerd/containerd.sock"):
            with self.subTest(endpoint=endpoint):
                self.facts["docker_exec"] = "/usr/bin/dockerd --containerd"+endpoint
                self.detect()

    def test_config_and_root_flags_are_passed_to_effective_config_dump(self):
        self.external()
        config = self.path / "custom.toml"
        config.write_text("reviewed configuration fixture\n")
        for flags in ("--config="+str(config)+" --root="+str(self.containerd),
                      "--config "+str(config)+" --root "+str(self.containerd),
                      "-c "+str(config)+" --root "+str(self.containerd)):
            with self.subTest(flags=flags):
                self.facts["containerd_exec"] = "{ path=/usr/bin/containerd ; argv[]=/usr/bin/containerd "+flags+" ; }"
                self.detect()
                self.assertIn(["containerd", "--config", str(config), "--root", str(self.containerd), "config", "dump"], self.commands())

    def test_address_flag_override_and_unix_endpoint(self):
        self.external()
        endpoint = str(self.path / "custom-containerd.sock")
        for flag in ("--address=", "--address ", "-a "):
            with self.subTest(flag=flag):
                self.facts["docker_exec"] = "/usr/bin/dockerd --containerd=unix://"+endpoint
                self.facts["containerd_exec"] = "/usr/bin/containerd "+flag+endpoint
                self.detect()

    def test_effective_root_must_match_root_override(self):
        self.external()
        other = self.path / "other-root"
        other.mkdir()
        self.facts["containerd_exec"] = "/usr/bin/containerd --root="+str(other)
        result = self.detect(succeeds=False)
        self.assertIn("启动参数与有效配置不一致", result.stderr)

    def test_endpoint_mismatch_refuses_unrelated_system_containerd(self):
        self.external()
        self.facts["dump"] = self.dump(address="/run/unrelated/containerd.sock")
        self.assertIn("endpoint", self.detect(succeeds=False).stderr)

    def test_unknown_snapshotter_fails_closed(self):
        self.facts["status"] = "io.containerd.snapshotter.v2"
        self.assertIn("image store", self.detect(succeeds=False).stderr)

    def test_unexpected_driver_type_output_is_not_assumed_classic(self):
        for status in ("not-json", "[not-json]", '{"error":"unavailable"}'):
            with self.subTest(status=status):
                self.facts["status"] = status
                self.detect(succeeds=False)

    def test_daemon_query_failures_do_not_guess_storage_root(self):
        self.external()
        for failure in ("docker_exit", "systemctl_exit", "containerd_exit"):
            with self.subTest(failure=failure):
                self.facts[failure] = 33
                self.detect(succeeds=False)
                del self.facts[failure]

    def test_duplicate_or_missing_config_values_are_rejected(self):
        self.external()
        variants = [self.dump().replace("state =", "root = '"+str(self.containerd)+"'\nstate ="),
                    self.dump()+"address = '/run/containerd/containerd.sock'\n",
                    self.dump().replace("root = '"+str(self.containerd)+"'\n", ""),
                    self.dump().replace("address = '/run/containerd/containerd.sock'\n", ""),
                    self.dump().replace("root = '"+str(self.containerd)+"'", "root = unquoted-root")]
        for dump in variants:
            with self.subTest(dump=dump):
                self.facts["dump"] = dump
                self.detect(succeeds=False)

    def test_repeated_root_arguments_are_not_ambiguously_reinterpreted(self):
        self.external()
        variants = ("--root="+str(self.containerd)+" --root=/different-active-store",
                    "--config=/etc/containerd/first.toml -c /etc/containerd/second.toml",
                    "--address=/run/containerd/containerd.sock -a /run/different.sock")
        for flags in variants:
            with self.subTest(flags=flags):
                self.facts["containerd_exec"] = "/usr/bin/containerd "+flags
                self.detect(succeeds=False)

    def test_unsafe_or_nonexistent_root_is_rejected(self):
        self.external()
        link = self.path / "linked-root"
        link.symlink_to(self.containerd, target_is_directory=True)
        for root in ("relative", "/", str(self.path / "missing"), str(self.containerd)+"/../containerd-root",
                     str(self.containerd)+"/.", str(link)):
            with self.subTest(root=root):
                self.facts["dump"] = self.dump(root=root)
                self.detect(succeeds=False)

    def test_docker_managed_store_is_beneath_docker_root(self):
        self.external()
        managed = self.docker / "containerd/daemon"
        managed.mkdir(parents=True)
        self.facts["docker_exec"] = "/usr/bin/dockerd -H fd://"
        result = self.detect()
        self.assertIn("ROOT="+str(managed), result.stdout)
        self.assertFalse(any(call[:2] == ["systemctl", "show"] and call[2] == "containerd.service" for call in self.commands()))
        self.assertFalse(any(call[0] == "containerd" for call in self.commands()))

    def test_docker_managed_missing_store_is_not_guessed(self):
        self.external()
        self.facts["docker_exec"] = "/usr/bin/dockerd -H fd://"
        self.detect(succeeds=False)

    def test_native_help_leaves_deployment_and_daemons_untouched(self):
        marker = self.target / "operator-data"
        marker.write_bytes(b"existing deployment marker")
        before = hashlib.sha256(marker.read_bytes()).hexdigest()
        self.calls.write_text("")
        result = subprocess.run([BASH, str(WRAPPER), "--help"], env=self.env, capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("--check-only", result.stdout)
        self.assertEqual(self.calls.read_text(), "")
        self.assertEqual(hashlib.sha256(marker.read_bytes()).hexdigest(), before)


@unittest.skipUnless(associative_arrays_available(), "Bash >= 4.3 is required for the real server budget arrays")
class StorageBudgetTests(StorageFixture):
    def budget(self, *, succeeds=True, mode="install", store="containerd"):
        if store == "containerd":
            self.external()
        fragment = SOURCE[SOURCE.index("declare -A required_kib="):SOURCE.index('info "CPU=${cpu_count}')]
        prefix = "deployment_mode="+shlex.quote(mode)+"\nexisting_parent="+shlex.quote(str(self.target))+"\nmedia_kib="+str(GIB_KIB)+"\n"
        prefix += 'requested_subsystems=(contract); declare -A package_components=([contract]=contract-fixture.tar.gz)\n'
        prefix += "media_dir="+shlex.quote(str(self.path))+"\n"
        return self.run_code(prefix+fragment+'\nfor device in "${!required_kib[@]}"; do printf "BUDGET=%s:%s\\n" "$device" "${required_kib[$device]}"; done\n', succeeds=succeeds)

    def filesystems(self, devices, available=200*GIB_KIB, inodes=200000):
        for path, device in zip((self.target, self.docker, self.containerd), devices):
            self.facts["filesystems"][str(path)] = dict(device=device, available=available, inodes=inodes, mount="/"+device)

    def test_same_device_budgets_are_added_once(self):
        self.filesystems(("same", "same", "same"))
        self.assertIn("BUDGET=same:"+str(31*GIB_KIB), self.budget().stdout)

    def test_classic_only_budgets_docker_and_target(self):
        self.filesystems(("same", "same", "unused-images"))
        result = self.budget(store="classic")
        self.assertIn("BUDGET=same:"+str(26*GIB_KIB), result.stdout)
        self.assertNotIn("BUDGET=unused-images", result.stdout)
        self.assertFalse(any(call[0] in ("systemctl", "containerd") for call in self.commands()))

    def test_separate_image_store_device_is_budgeted_independently(self):
        self.filesystems(("metadata", "metadata", "images"))
        result = self.budget()
        self.assertIn("BUDGET=metadata:"+str(11*GIB_KIB), result.stdout)
        self.assertIn("BUDGET=images:"+str(20*GIB_KIB), result.stdout)

    def test_each_of_three_devices_retains_its_own_budget(self):
        self.filesystems(("target", "metadata", "images"))
        result = self.budget()
        for device, amount in (("target", 6), ("metadata", 5), ("images", 20)):
            self.assertIn("BUDGET="+device+":"+str(amount*GIB_KIB), result.stdout)

    def test_insufficient_external_image_space_refuses_preflight(self):
        self.filesystems(("target", "metadata", "images"))
        self.facts["filesystems"][str(self.containerd)]["available"] = 19*GIB_KIB
        self.assertIn("空间不足", self.budget(succeeds=False).stderr)

    def test_insufficient_external_image_inodes_refuses_preflight(self):
        self.filesystems(("target", "metadata", "images"))
        self.facts["filesystems"][str(self.containerd)]["inodes"] = 9999
        self.assertIn("inode", self.budget(succeeds=False).stderr)

    def test_unparseable_capacity_and_inodes_do_not_pass_preflight(self):
        for field in ("available", "inodes"):
            with self.subTest(field=field):
                self.filesystems(("target", "metadata", "images"))
                self.facts["filesystems"][str(self.containerd)][field] = "unknown"
                self.assertIn("无法读取分区", self.budget(succeeds=False).stderr)

    def test_incremental_budget_only_adds_selected_media(self):
        self.filesystems(("same", "same", "same"))
        self.facts["selected_media_kib"] = 3*GIB_KIB
        self.assertIn("BUDGET=same:"+str(12*GIB_KIB), self.budget(mode="add-subsystems").stdout)


if __name__ == "__main__":
    unittest.main()
