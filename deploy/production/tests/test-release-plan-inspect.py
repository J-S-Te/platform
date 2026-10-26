#!/usr/bin/env python3
import importlib.util
import io
import json
import unittest
from pathlib import Path

HERE = Path(__file__).resolve()
spec = importlib.util.spec_from_file_location("delivery_inspect", HERE.parent.parent / "bin" / "delivery-inspect.py")
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


class Media:
    def __init__(self, payload):
        self.payload = payload.encode("utf-8")
        self.snapshots = {"release-plan.json": len(self.payload)}

    def open(self, name):
        return io.BytesIO(self.payload)


class ReleasePlanInspectionTest(unittest.TestCase):
    def test_ready_plan_passes(self):
        inspection = module.Inspection(Media(json.dumps({"schema": "uip.release-materials/v1", "overall_status": "READY"})), "prepare")
        inspection.release_plan()
        self.assertEqual("PASS", inspection.report["checks"][-1]["status"])

    def test_blocked_plan_fails(self):
        inspection = module.Inspection(Media(json.dumps({"schema": "uip.release-materials/v1", "overall_status": "BLOCKED"})), "prepare")
        with self.assertRaises(module.Invalid):
            inspection.release_plan()

    def test_oversized_plan_fails(self):
        inspection = module.Inspection(Media("x" * (1024 * 1024 + 1)), "prepare")
        with self.assertRaises(module.Invalid):
            inspection.release_plan()

    def test_malformed_plan_fails(self):
        inspection = module.Inspection(Media("{not-json"), "prepare")
        with self.assertRaises(module.Invalid):
            inspection.release_plan()

    def test_invalid_schema_fails(self):
        inspection = module.Inspection(Media(json.dumps({"schema": "wrong", "overall_status": "READY"})), "prepare")
        with self.assertRaises(module.Invalid):
            inspection.release_plan()


if __name__ == "__main__":
    unittest.main()
