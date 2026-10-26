#!/usr/bin/env python3
"""Bind the deployment compatibility declaration to the current release assets."""
import hashlib
import json
from pathlib import Path
import re
import subprocess
import sys
import tempfile

ROOT = Path(__file__).resolve().parents[3]
PRODUCTION = ROOT / "deploy/production"
declaration = json.loads((PRODUCTION / "deployment-compatibility.json").read_text(encoding="utf-8"))
assert declaration["schema"] == "uip.deployment-compatibility/v1"
assert declaration["release"] and "REPLACE" not in declaration["release"].upper()
assert declaration["assets_contract"] and declaration["config_contract"]
env_template = (PRODUCTION / ".env.example").read_text(encoding="utf-8")
for key in ("SUBSYSTEM_PRODUCTION_ALLOWED_TENANT_ID", "FILE_GATEWAY_HOST_ROOT",
            "IAM_IMPORT_FILE_GATEWAY_CLIENT_ID", "PUBLIC_HTTP_PORT", "OIDC_ISSUER"):
    assert key + "=" in env_template, "config contract key missing from .env.example: " + key
assert declaration["database"]["previous_program_after_migration"] in ("compatible", "incompatible", "unknown")
assert declaration["recovery"]["type"] in ("backward", "forward-only", "unknown")
assert isinstance(declaration["recovery"]["requires_backup"], bool)

migrations = sorted(
    int(match.group(1))
    for path in (ROOT / "migrations").glob("[0-9][0-9][0-9][0-9][0-9][0-9]_*.sql")
    if (match := re.match(r"(\d{6})_", path.name))
)
assert migrations, "no platform migrations found"
assert declaration["database"]["migrate_to"] == migrations[-1], (
    "deployment-compatibility.json migrate_to must equal the latest platform migration"
)
migration_hash = hashlib.sha256()
for path in sorted((ROOT / "migrations").glob("*.sql")):
    migration_hash.update(path.name.encode())
    migration_hash.update(b"\0")
    migration_hash.update(path.read_bytes())
    migration_hash.update(b"\0")
assert declaration["database"].get("migration_set_sha256") == migration_hash.hexdigest(), (
    "deployment-compatibility.json migration_set_sha256 is stale"
)

install_assets = (PRODUCTION / "bin/install-assets.sh").read_text(encoding="utf-8")
assert "deployment-compatibility" in install_assets
assert "deployment-plan.py" in (PRODUCTION / "bin/build-offline-packages.sh").read_text(encoding="utf-8")
assert "deployment-compatibility.json" in (PRODUCTION / "bin/build-offline-packages.sh").read_text(encoding="utf-8")
assert "deployment-compatibility.json" in (PRODUCTION / "bin/delivery-inspect.py").read_text(encoding="utf-8")
# 构建器必须自行生成声明，不能依赖操作员手写 compatible。
with tempfile.TemporaryDirectory() as directory:
    root = Path(directory)
    output = root / "compat.json"
    subprocess.run([sys.executable, str(PRODUCTION / "bin/build-deployment-compatibility.py"),
                    "--migrations", str(ROOT / "migrations"), "--release", "qa", "--output", str(output)],
                   check=True, capture_output=True)
    generated = json.loads(output.read_text(encoding="utf-8"))
    assert generated["database"]["migrate_to"] == migrations[-1]
    assert generated["database"]["migration_set_sha256"] == migration_hash.hexdigest()
    assert generated["database"]["previous_program_after_migration"] == "unknown"
    evidence = root / "evidence.json"
    evidence.write_text(json.dumps({
        "schema": "uip.deployment-compatibility-evidence/v1",
        "migration_set_sha256": migration_hash.hexdigest(),
        "previous_program_after_migration": "compatible",
        "recovery_type": "backward",
        "verified_at": "2026-10-25T00:00:00Z",
        "verifier": "release-manager",
        "evidence": "qa-evidence-001",
    }), encoding="utf-8")
    proven = root / "proven.json"
    subprocess.run([sys.executable, str(PRODUCTION / "bin/build-deployment-compatibility.py"),
                    "--migrations", str(ROOT / "migrations"), "--release", "qa", "--output", str(proven),
                    "--previous-evidence", str(evidence)], check=True, capture_output=True)
    value = json.loads(proven.read_text(encoding="utf-8"))
    assert value["database"]["previous_program_after_migration"] == "compatible"
    assert value["recovery"]["type"] == "backward"

print("deployment compatibility declaration is bound to current migrations and assets: PASS")
