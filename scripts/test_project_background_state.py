#!/usr/bin/env python3
"""project_background_state.decide 的回归测试。

运行：python3 scripts/test_project_background_state.py
"""
import importlib.util
import json
import tempfile
from pathlib import Path

SCRIPT = Path(__file__).with_name("project_background_state.py")


def load():
    spec = importlib.util.spec_from_file_location("project_background_state", SCRIPT)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def test_archives_incomplete_stage_when_container_gone():
    module = load()
    assert module.decide({"stage": "activating", "engine": "legacy"}, 0, 0) == "archive"
    assert module.decide({"stage": "prepared"}, 0, 0) == "archive"
    assert module.decide({"stage": "teardown-drained"}, 0, 0) == "archive"


def test_keeps_state_only_while_a_process_runs():
    module = load()
    assert module.decide({"stage": "activating"}, 1, 1) == "keep"
    assert module.decide({"stage": "activating"}, 1, 0) == "keep"
    # 容器停了但残留：进程已不存在，旧操作不可能仍在途，可以归档。
    assert module.decide({"stage": "activating"}, 0, 1) == "archive"


def test_keeps_settled_stages_and_unknown_state():
    module = load()
    for stage in ("stopped", "idle", "verified", ""):
        assert module.decide({"stage": stage}, 0, 0) == "keep"
    assert module.decide(None, 0, 0) == "keep"


def test_cli_reads_state_file():
    module = load()
    with tempfile.TemporaryDirectory() as directory:
        path = Path(directory) / "operation.json"
        path.write_text(json.dumps({"stage": "activating"}))
        with open(path, encoding="utf-8") as _:
            pass
        assert module.decide(json.loads(path.read_text()), 0, 0) == "archive"


if __name__ == "__main__":
    failures = 0
    for name, func in sorted(globals().items()):
        if name.startswith("test_") and callable(func):
            try:
                func()
                print("ok   %s" % name)
            except AssertionError as exc:
                failures += 1
                print("FAIL %s: %s" % (name, exc))
    raise SystemExit(1 if failures else 0)
