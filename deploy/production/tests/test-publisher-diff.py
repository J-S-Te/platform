#!/usr/bin/env python3
import contextlib
import importlib.util
import io
import tempfile
import unittest
from pathlib import Path

HERE = Path(__file__).resolve()
spec = importlib.util.spec_from_file_location("publisher", HERE.parent.parent / "bin" / "publish-runtime-approvals.py")
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


class PublisherDiffTest(unittest.TestCase):
    def test_diff_reports_changes(self):
        with tempfile.TemporaryDirectory() as root:
            before, after = Path(root) / "before", Path(root) / "after"
            before.mkdir(); after.mkdir()
            (before / "a").write_text("a", encoding="utf-8")
            (before / "b").write_text("b", encoding="utf-8")
            (after / "a").write_text("a2", encoding="utf-8")
            (after / "c").write_text("c", encoding="utf-8")
            stream = io.StringIO()
            with contextlib.redirect_stdout(stream):
                module.print_bundle_diff(before, after)
            text = stream.getvalue()
            self.assertIn("+ c", text)
            self.assertIn("- b", text)
            self.assertIn("~ a", text)


if __name__ == "__main__":
    unittest.main()
