#!/usr/bin/env python3
"""回归基于实际事实的构建证据采集器。"""
from __future__ import annotations

import hashlib
import json
from pathlib import Path
import subprocess
import sys
import tarfile
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[4]
HELPER = ROOT / "platform/deploy/production/bin/build-evidence.py"
DIGEST = "sha256:" + "a" * 64


class BuildEvidenceTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="uip-build-evidence-")
        self.root = Path(self.temp.name)
        self.output = self.root / "output"
        self.workspace = self.root / "workspace"
        self.output.mkdir()
        (self.workspace / "platform").mkdir(parents=True)
        self.dockerfile = self.workspace / "platform/Dockerfile"
        self.dockerfile.write_text("FROM golang:1.26 AS build\nFROM alpine:3.21\n", encoding="utf-8")
        self.write_package()

    def tearDown(self):
        self.temp.cleanup()

    def write_package(self, config_digest: str = DIGEST) -> None:
        archive = self.output / "platform-backend-test-linux-amd64.tar.gz"
        package_env = (
            "PACKAGE_FORMAT=2\n"
            "COMPONENT=platform\n"
            "VERSION=test\n"
            "PLATFORM=linux/amd64\n"
            "IMAGES=uip-package/platform:test\n"
            f"IMAGE_CONFIG_DIGESTS={config_digest}\n"
        ).encode()
        with tarfile.open(archive, "w:gz") as bundle:
            info = tarfile.TarInfo("package.env")
            info.size = len(package_env)
            import io
            bundle.addfile(info, io.BytesIO(package_env))
        digest = hashlib.sha256(archive.read_bytes()).hexdigest()
        archive.with_name(archive.name + ".sha256").write_text(f"{digest}  {archive.name}\n", encoding="utf-8")
        (self.output / "BUILD_INFO.txt").write_text(
            "RELEASE_VERSION=test\nTARGET_PLATFORM=linux/amd64\n"
            "DOCKER_CLIENT_VERSION=29.0\nDOCKER_SERVER_VERSION=29.0\nBUILDX_VERSION=buildx-test\n",
            encoding="utf-8",
        )

    def run_helper(self, builder="builder://uip/build", invocation="inv-1", base_evidence=None):
        raw = self.dockerfile.read_bytes()
        index = self.output / "source-input-index.tsv"
        index.write_text(
            f"{hashlib.sha256(raw).hexdigest()}\t{len(raw)}\tplatform/Dockerfile\n",
            encoding="utf-8",
        )
        args = [sys.executable, str(HELPER),
                "--output-root", str(self.output), "--workspace", str(self.workspace),
                "--version", "test", "--platform", "linux/amd64",
                "--source-sha256", "b" * 64, "--source-index", str(index),
                "--builder-id", builder, "--invocation-id", invocation]
        if base_evidence is not None:
            path = self.output / "base-image-evidence.tsv"
            path.write_text(base_evidence, encoding="utf-8")
            args += ["--base-image-evidence", str(path)]
        return subprocess.run(args, capture_output=True, text=True)

    def test_unpinned_builder_is_not_formally_signable(self):
        result = self.run_helper()
        self.assertEqual(result.returncode, 0, result.stderr)
        evidence = json.loads((self.output / "build-evidence.json").read_text(encoding="utf-8"))
        self.assertFalse(evidence["formal_signing_allowed"])
        self.assertTrue(evidence["unsigned_candidate"])
        self.assertEqual(evidence["source_input_sha256"], "b" * 64)
        self.assertEqual(evidence["packages"][0]["component"], "platform")
        self.assertEqual(evidence["packages"][0]["images"][0]["config_digest"], DIGEST)
        self.assertEqual(evidence["dockerfiles"][0]["from"], ["golang:1.26", "alpine:3.21"])
        self.assertFalse(evidence["dockerfiles"][0]["all_base_refs_pinned"])

    def test_pinned_base_and_builder_is_formally_signable(self):
        self.dockerfile.write_text(
            "FROM alpine@sha256:" + "c" * 64 + " AS base\nFROM base AS runtime\n",
            encoding="utf-8",
        )
        result = self.run_helper()
        self.assertEqual(result.returncode, 0, result.stderr)
        evidence = json.loads((self.output / "build-evidence.json").read_text(encoding="utf-8"))
        self.assertTrue(evidence["formal_signing_allowed"])
        self.assertTrue(all(item["all_base_refs_pinned"] for item in evidence["dockerfiles"]))
        self.assertEqual(evidence["dockerfiles"][0]["from"], ["alpine@sha256:" + "c" * 64])

    def test_arg_base_with_evidence_is_formally_signable(self):
        self.dockerfile.write_text(
            "FROM ${BASE_IMAGE} AS base\nFROM base AS runtime\n",
            encoding="utf-8",
        )
        resolved = "alpine@sha256:" + "d" * 64
        result = self.run_helper(base_evidence=f"platform/Dockerfile\t{resolved}\n")
        self.assertEqual(result.returncode, 0, result.stderr)
        evidence = json.loads((self.output / "build-evidence.json").read_text(encoding="utf-8"))
        self.assertTrue(evidence["formal_signing_allowed"])
        self.assertEqual(evidence["dockerfiles"][0]["from"], [resolved])
        self.assertTrue(evidence["dockerfiles"][0]["all_base_refs_pinned"])

    def test_arg_base_without_evidence_is_not_formally_signable(self):
        self.dockerfile.write_text("FROM ${BASE_IMAGE}\n", encoding="utf-8")
        result = self.run_helper()
        self.assertEqual(result.returncode, 0, result.stderr)
        evidence = json.loads((self.output / "build-evidence.json").read_text(encoding="utf-8"))
        self.assertFalse(evidence["formal_signing_allowed"])
        self.assertFalse(evidence["dockerfiles"][0]["all_base_refs_pinned"])

    def test_missing_sidecar_fails(self):
        (self.output / "platform-backend-test-linux-amd64.tar.gz.sha256").unlink()
        result = self.run_helper()
        self.assertEqual(result.returncode, 1)
        self.assertIn("sidecar", result.stderr)
        self.assertFalse((self.output / "build-evidence.json").exists())

    def test_mismatched_digest_count_fails(self):
        archive = self.output / "platform-backend-test-linux-amd64.tar.gz"
        archive.unlink()
        archive.with_name(archive.name + ".sha256").unlink()
        self.write_package()
        # 用包含两个标签但只有一个摘要的内容替换 package.env。
        with tarfile.open(archive, "w:gz") as bundle:
            import io
            env = b"PACKAGE_FORMAT=2\nCOMPONENT=platform\nVERSION=test\nPLATFORM=linux/amd64\nIMAGES=a,b\nIMAGE_CONFIG_DIGESTS=" + DIGEST.encode() + b"\n"
            info = tarfile.TarInfo("package.env")
            info.size = len(env)
            bundle.addfile(info, io.BytesIO(env))
        digest = hashlib.sha256(archive.read_bytes()).hexdigest()
        archive.with_name(archive.name + ".sha256").write_text(f"{digest}  {archive.name}\n", encoding="utf-8")
        result = self.run_helper()
        self.assertEqual(result.returncode, 1)
        self.assertIn("数量不一致", result.stderr)


if __name__ == "__main__":
    unittest.main()
