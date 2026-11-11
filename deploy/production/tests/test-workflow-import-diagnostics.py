#!/usr/bin/env python3
"""导入复用探测与最终失败的最小隔离回归，不调用 Docker。"""
from pathlib import Path
import shlex
import subprocess
import unittest

WORKFLOW = Path(__file__).resolve().parents[1] / "bin/deployment-workflow.sh"


class ImportDiagnosticTests(unittest.TestCase):
    # 用途：运行真实 workflow_import，以内存标志模拟包尚未导入。
    # 参数：final_success 决定实际导入后证明是否成立。
    # 返回值：子进程结果；不执行安装或外部服务调用。
    def probe(self, final_success):
        script = "source " + shlex.quote(str(WORKFLOW)) + "\n"
        script += """
workflow_start() { :; }
workflow_done() { printf 'stage-done\\n'; }
imported=false
proof() {
  if [[ "$imported" == true && "$FINAL_SUCCESS" == true ]]; then return 0; fi
  printf 'proof-evidence-mismatch\\n' >&2
  return 1
}
action() { imported=true; printf 'import-performed\\n'; }
workflow_import fixture-import proof action
"""
        return subprocess.run(["bash", "-c", "FINAL_SUCCESS=" +
                               ("true" if final_success else "false") + "\n" + script],
                              capture_output=True, text=True, timeout=5)

    # 用途：验证首次缓存未命中后成功导入不遗留最终失败文案。
    # 参数：无。
    # 返回值：无；诊断或阶段结果不符时失败。
    def test_initial_probe_failure_is_recoverable(self):
        result = self.probe(True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("import-performed", result.stdout)
        self.assertIn("stage-done", result.stdout)
        self.assertNotIn("proof-evidence-mismatch", result.stderr)
        self.assertIn("重新导入", result.stdout)

    # 用途：验证实际导入后证明失败仍明确失败，不把阶段标为完成。
    # 参数：无。
    # 返回值：无；吞错或错误完成时失败。
    def test_final_proof_failure_remains_visible(self):
        result = self.probe(False)
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(result.stderr.count("proof-evidence-mismatch"), 1)
        self.assertNotIn("stage-done", result.stdout)


if __name__ == "__main__":
    unittest.main()
