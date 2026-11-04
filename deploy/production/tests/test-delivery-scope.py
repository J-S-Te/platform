#!/usr/bin/env python3
import json
from pathlib import Path
import re
import subprocess
import sys
import tarfile
import tempfile

ROOT = Path(__file__).resolve().parents[1]
PROJECT = ROOT / "bin" / "project-compose-assets.py"
CHECK = ROOT / "bin" / "check-delivery-scope.py"
COMPOSE = ROOT / "docker-compose.yml"


def run(command):
    result = subprocess.run(command, capture_output=True, text=True)
    if result.returncode != 0:
        raise AssertionError(result.stdout + result.stderr)
    return result.stdout


def make_media(root: Path, components, portal_compose, portal_env):
    (root / "build-evidence.json").write_text(json.dumps({
        "packages": [{"component": item} for item in components],
    }))
    compose = COMPOSE.read_text()
    if not portal_compose:
        projected = root / "compose-projected.yml"
        run([sys.executable, str(PROJECT), "--input", str(COMPOSE), "--output", str(projected), "--disabled", "portal"])
        compose = projected.read_text()
        # uip_20261102_r1 缺陷回归：被裁剪块的 BEGIN/END 标记行不能被再次注释，
        # 否则受控升级按 `^\s*#\s*(BEGIN|END)\s+SUBSYSTEM` 推导状态时整块不可见。
        assert re.search(r"(?m)^  # BEGIN SUBSYSTEM: portal ", compose), "portal marker must stay parseable"
        assert re.search(r"(?m)^  # END SUBSYSTEM: portal ", compose), "portal END marker must stay parseable"
        assert not re.search(r"(?m)^[ \t]*#[ \t]+#", compose), "subsystem markers must not be double-commented"
    archive = root / "deployment-assets-test.tar.gz"
    with tarfile.open(archive, "w:gz") as bundle:
        for name, payload in [("docker-compose.yml", compose)]:
            data = payload.encode()
            info = tarfile.TarInfo(name)
            info.size = len(data)
            bundle.addfile(info, __import__("io").BytesIO(data))
        if portal_env:
            for name in ("subsystem-templates/portal.env.example", "subsystem-templates/portal-report-worker.env.example"):
                data = b"PORTAL=fixture\n"
                info = tarfile.TarInfo(name)
                info.size = len(data)
                bundle.addfile(info, __import__("io").BytesIO(data))
    return archive


def main():
    with tempfile.TemporaryDirectory(prefix="uip-scope-test-") as temporary:
        root = Path(temporary)
        make_media(root, ["common", "platform", "frontend", "contract"], False, False)
        spec = root / "release-spec.json"
        spec.write_text(json.dumps({"schema": "urn:uip:release-spec:v1", "applications": [{"code": "contract_management"}]}))
        run([sys.executable, str(CHECK), "--media", str(root), "--selected-components", "contract", "--spec", str(spec)])
        try:
            run([sys.executable, str(CHECK), "--media", str(root), "--selected-components", "contract,customer-portal"])
        except AssertionError:
            pass
        else:
            raise AssertionError("portal selected but assets lacked portal did not fail")
    with tempfile.TemporaryDirectory(prefix="uip-scope-test-portal-") as temporary:
        root = Path(temporary)
        make_media(root, ["common", "platform", "frontend", "contract", "customer-portal"], True, True)
        run([sys.executable, str(CHECK), "--media", str(root), "--selected-components", "contract,customer-portal"])
        try:
            run([sys.executable, str(CHECK), "--media", str(root), "--selected-components", "contract"])
        except AssertionError:
            pass
        else:
            raise AssertionError("portal assets without selected portal did not fail")
    # uip_20261102_r1 第二层缺陷回归：整批禁用了实际交付的子系统块时，投影必须拒绝发布，
    # 否则受控升级会在"交付编排模板"校验处判 customer-opportunity 未启用并中止。
    with tempfile.TemporaryDirectory(prefix="uip-projection-gate-") as temporary:
        output = Path(temporary) / "compose.yml"
        result = subprocess.run(
            [sys.executable, str(PROJECT), "--input", str(COMPOSE), "--output", str(output),
             "--disabled", "contract,project,customer,portal,settlement,data-analysis",
             "--require-enabled", "contract,project,customer,data-analysis"],
            capture_output=True, text=True)
        assert result.returncode != 0, "projection must reject subsystem blocks disabled but shipped in this batch"
    print("PASS: delivery scope projection and consistency checks")


if __name__ == "__main__":
    main()
