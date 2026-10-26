#!/usr/bin/env python3
"""Failure-path regression for the release facade; no Docker or signing keys."""
import importlib.util
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

BIN = Path(__file__).resolve().parents[1] / 'bin'
spec = importlib.util.spec_from_file_location('release_guards', BIN / 'release-workflow.py')
release = importlib.util.module_from_spec(spec)
spec.loader.exec_module(release)


class ReleaseGuards(unittest.TestCase):
    def test_material_command_failure_blocks_build(self):
        for code, stdout in [(1, ''), (64, '{}'), (0, 'not-json'), (0, '[]')]:
            with self.subTest(code=code, stdout=stdout):
                with patch.object(release.subprocess, 'run', return_value=subprocess.CompletedProcess([], code, stdout, 'failed')):
                    self.assertFalse(release.gate_build(['--spec', '/spec.json']))

    def test_manifest_rejects_duplicate_entries(self):
        with tempfile.TemporaryDirectory() as temporary:
            output = Path(temporary)
            (output / 'a').write_text('content')
            line = release.file_digest(output / 'a') + '  a\n'
            (output / 'SHA256SUMS').write_text(line * 2)
            self.assertTrue(release.verify_manifest(output))

    def test_unsafe_version_cannot_select_archive_path(self):
        with tempfile.TemporaryDirectory() as temporary:
            output = Path(temporary) / 'pkg'
            output.mkdir()
            (output / 'a').write_text('content')
            (output / 'SHA256SUMS').write_text(release.file_digest(output / 'a') + '  a\n')
            args = type('Args', (), dict(output=str(output), version='../unsafe', allow_legacy_unsigned=True))()
            with patch.object(release.subprocess, 'run') as runner:
                self.assertEqual(2, release.package(args))
                self.assertEqual(0, runner.call_count)

    def test_signed_package_requires_external_verifier(self):
        with tempfile.TemporaryDirectory() as temporary:
            output = Path(temporary) / 'pkg'
            output.mkdir()
            for name in ['a', 'release-manifest.json', 'release-manifest.sigstore.json']:
                (output / name).write_text('{}')
            (output / 'SHA256SUMS').write_text(release.file_digest(output / 'a') + '  a\n')
            args = type('Args', (), dict(output=str(output), version='v1', allow_legacy_unsigned=False, verifier=None, trust_dir=None))()
            with patch.object(release.subprocess, 'run') as runner:
                self.assertEqual(2, release.package(args))
                self.assertEqual(0, runner.call_count)

    def test_review_operator_and_candidate_binding_required(self):
        with tempfile.TemporaryDirectory() as temporary:
            output = Path(temporary)
            (output / 'release-review.json').write_text(json.dumps({'schema': 'uip.release-review/v2'}))
            self.assertTrue(release.validate_review(output))

    def test_verifier_success_rejection_and_untrusted_location(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            output = root / 'pkg'
            output.mkdir()
            trust = root / 'trust'
            trust.mkdir()
            verifier = root / 'verifier'
            verifier.write_text('#!/bin/sh\n')
            verifier.chmod(0o700)
            args = type('Args', (), dict(verifier=str(verifier), trust_dir=str(trust)))()
            for code, stdout, allowed in [(0, '{"status":"VERIFIED"}', True), (1, '{}', False), (0, '{}', False), (0, '[]', False)]:
                with patch.object(release.subprocess, 'run', return_value=subprocess.CompletedProcess([], code, stdout, '')) as runner:
                    self.assertEqual(not allowed, bool(release.signature_error(output, args)))
                    self.assertIn('verify', runner.call_args.args[0])
            args.trust_dir = str(output)
            self.assertTrue(release.signature_error(output, args))


if __name__ == '__main__':
    unittest.main()
