#!/usr/bin/env python3
import json
from pathlib import Path
import subprocess
import sys
import tarfile
import tempfile

PRODUCTION = Path(__file__).resolve().parents[1]
BUILDER = PRODUCTION / "bin" / "build-offline-packages.sh"


def run_assets(output: Path, selected: str):
    result = subprocess.run([
        "bash", str(BUILDER), "--component", "assets", "--version", "test-component-projection",
        "--output", str(output), "--selected-components", selected,
    ], capture_output=True, text=True)
    if result.returncode != 0:
        raise AssertionError(result.stdout + result.stderr)
    archive = output / "deployment-assets-test-component-projection.tar.gz"
    with tarfile.open(archive, "r:gz") as bundle:
        names = {member.name.removeprefix("./") for member in bundle if member.isfile()}
        compose_member = next(member for member in bundle if member.name.removeprefix("./") == "docker-compose.yml")
        compose = bundle.extractfile(compose_member).read().decode()
    return names, compose


def main():
    with tempfile.TemporaryDirectory(prefix="uip-component-projection-") as temporary:
        root = Path(temporary)
        names, compose = run_assets(root / "contract", "contract")
        assert "subsystem-templates/portal.env.example" not in names
        assert "subsystem-templates/portal-report-worker.env.example" not in names
        assert "subsystems.d/customer_portal-prod.yaml" in names  # six profile contract retained
        assert not __import__("re").search(r"(?m)^  portal-api:", compose)
        assert not __import__("re").search(r"(?m)^  portal-mysql-data:", compose)
        assert __import__("re").search(r"(?m)^  contract-api:", compose)

        names, compose = run_assets(root / "portal", "customer-portal")
        assert "subsystem-templates/portal.env.example" in names
        assert __import__("re").search(r"(?m)^  portal-api:", compose)
        assert __import__("re").search(r"(?m)^  portal-mysql-data:", compose)
    print("PASS: asset component projection keeps profiles and removes unselected runtime Compose/portal templates")


if __name__ == "__main__":
    main()
