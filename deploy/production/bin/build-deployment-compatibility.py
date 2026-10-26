#!/usr/bin/env python3
"""Generate the machine-readable deployment compatibility declaration.

Compatibility with the previous program is only accepted from a separate
evidence file whose migration-set hash matches the current migrations.  Without
such evidence the declaration deliberately stays "unknown".
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import stat
import sys

SCHEMA = "uip.deployment-compatibility/v1"
EVIDENCE_SCHEMA = "uip.deployment-compatibility-evidence/v1"


def reject(message):
    print("deployment compatibility: " + message, file=sys.stderr)
    raise SystemExit(2)


def migration_files(directory):
    files = sorted(Path(directory).glob("[0-9][0-9][0-9][0-9][0-9][0-9]_*.sql"))
    if not files:
        reject("no migrations found in " + str(directory))
    versions = []
    for path in files:
        match = re.match(r"(\d{6})_", path.name)
        if not match:
            reject("invalid migration name: " + path.name)
        versions.append(int(match.group(1)))
    return files, max(versions)


def migration_hash(files):
    digest = hashlib.sha256()
    for path in files:
        digest.update(path.name.encode("utf-8"))
        digest.update(b"\0")
        digest.update(path.read_bytes())
        digest.update(b"\0")
    return digest.hexdigest()


def evidence(path, expected_hash):
    if not path:
        return "unknown", "forward-only", ""
    target = Path(path)
    try:
        info = target.lstat()
    except OSError:
        reject("evidence file is missing")
    if not stat.S_ISREG(info.st_mode) or stat.S_ISLNK(info.st_mode) or info.st_size > 64 * 1024:
        reject("evidence file must be a bounded regular file")
    try:
        value = json.loads(target.read_text(encoding="utf-8"))
    except (OSError, ValueError):
        reject("evidence file is not valid JSON")
    required = {"schema", "migration_set_sha256", "previous_program_after_migration",
                "recovery_type", "verified_at", "verifier", "evidence"}
    if not isinstance(value, dict) or set(value) != required or value.get("schema") != EVIDENCE_SCHEMA:
        reject("evidence schema or fields are invalid")
    if value.get("migration_set_sha256") != expected_hash:
        return "unknown", "forward-only", ""
    previous = value.get("previous_program_after_migration")
    recovery = value.get("recovery_type")
    if previous not in ("compatible", "incompatible") or recovery not in ("backward", "forward-only"):
        reject("evidence compatibility/recovery values are invalid")
    if not isinstance(value.get("verified_at"), str) or not value["verified_at"]:
        reject("evidence verified_at is required")
    if not isinstance(value.get("verifier"), str) or not value["verifier"]:
        reject("evidence verifier is required")
    if not isinstance(value.get("evidence"), str) or not value["evidence"]:
        reject("evidence reference is required")
    return previous, recovery, value["evidence"]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--migrations", required=True)
    parser.add_argument("--release", required=True)
    parser.add_argument("--output", required=True)
    parser.add_argument("--assets-contract", default="1")
    parser.add_argument("--config-contract", default="1")
    parser.add_argument("--previous-evidence", default="")
    args = parser.parse_args()
    files, latest = migration_files(args.migrations)
    digest = migration_hash(files)
    previous, recovery, note = evidence(args.previous_evidence, digest)
    value = {
        "schema": SCHEMA,
        "release": args.release,
        "assets_contract": args.assets_contract,
        "config_contract": args.config_contract,
        "database": {
            "current_min": 0,
            "migrate_to": latest,
            "migration_set_sha256": digest,
            "previous_program_after_migration": previous,
        },
        "recovery": {
            "type": recovery,
            "requires_backup": True,
        },
    }
    if note:
        value["database"]["compatibility_evidence"] = note
    output = Path(args.output)
    if output.exists() or output.is_symlink():
        reject("output already exists")
    output.parent.mkdir(parents=True, exist_ok=True)
    temporary = output.with_name("." + output.name + ".tmp")
    try:
        temporary.write_text(json.dumps(value, ensure_ascii=False, indent=2, sort_keys=True) + "\n", encoding="utf-8")
        os.chmod(temporary, 0o644)
        os.replace(temporary, output)
    finally:
        if temporary.exists():
            temporary.unlink()
    print(json.dumps({"output": str(output), "migrate_to": latest, "migration_set_sha256": digest,
                      "previous_program_after_migration": previous, "recovery_type": recovery}, ensure_ascii=False))


if __name__ == "__main__":
    main()
