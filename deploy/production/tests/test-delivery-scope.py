#!/usr/bin/env python3
import json
from pathlib import Path
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
    print("PASS: delivery scope projection and consistency checks")


if __name__ == "__main__":
    main()
