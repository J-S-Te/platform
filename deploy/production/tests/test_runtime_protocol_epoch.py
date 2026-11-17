#!/usr/bin/env python3
"""隔离验证归档/指标协议换代的排空、指针和回滚边界，不操作Docker。"""
import re
import shlex
import subprocess
import tempfile
import unittest
from pathlib import Path

SOURCE = (Path(__file__).resolve().parents[1] / "bin/deploy-service.sh").read_text()


# 用途：提取真实发布函数并在隔离Bash中验证命令顺序。
# 参数：body 为合成不可变镜像和Docker/Compose边界的测试步骤。
# 返回值：真实函数执行结果；不存在协议防护时立即失败。
def run_protocol(body):
    names = ["epoch", "data_analysis", "preflight", "quiesce", "rollback_allowed"]
    functions = []
    for suffix in names:
        match = re.search(r"^runtime_protocol_" + suffix + r"\(\) \{\n.*?^\}", SOURCE, re.M | re.S)
        if not match:
            raise AssertionError("缺少协议边界函数：" + suffix)
        functions.append(match.group())
    fixture = r'''
set -euo pipefail
service=settlement
image_key=SETTLEMENT_IMAGE
image_ref=new
old_ref=old
runtime_write_started=false
release_image_value() { printf '%s\n' "$old_ref"; }
rollback_ref_is_immutable() { test -n "$1"; }
scope_runtime_services() { printf '%s\n' contract-api contract-worker; }
docker() {
  if [[ "$1" == image ]]; then
    [[ "$3" == old ]] && printf '0\n' || printf '1\n'
  else printf 'false\n'; fi
}
compose() {
  if [[ "$1" == stop ]]; then
    printf 'STOPPED %s\n' "$*"
    return "${stop_result:-0}"
  fi
  if [[ "$1" == ps ]]; then printf 'container-a\ncontainer-b\n'; fi
}
'''
    return subprocess.run(["bash", "-c", fixture + "\n" + "\n".join(functions) + "\n" + body],
                          text=True, capture_output=True, timeout=10)


class RuntimeProtocolTests(unittest.TestCase):
    # 用途：验证合同跨代与结算沿用相同的禁止旧读写者复活边界。
    # 参数：self 为隔离 Bash 测试上下文。
    # 返回值：无；跨代后允许回旧合同镜像时失败。
    def test_contract_epoch_blocks_old_pointer(self):
        result = run_protocol('service=contract; runtime_protocol_preflight; runtime_protocol_quiesce; '
                              'runtime_write_started=true; if runtime_protocol_rollback_allowed; then exit 99; fi')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('contract-api contract-worker', result.stdout)

    # 用途：验证迁移镜像也必须与数析三个运行镜像同代。
    # 参数：self 为隔离 Bash 测试上下文。
    # 返回值：无；候选混代或任一旧组件降代仍获准时失败。
    def test_data_analysis_checks_all_four_images(self):
        setup = r'''
service=data-analysis
data_analysis_image_keys=(API AGG ALERT MIGRATE)
data_analysis_image_refs=(new-api new-agg new-alert new-migrate)
release_image_value() { printf 'old-%s\n' "$1"; }
docker() {
  if [[ "$1" == image ]]; then
    case "$3" in
      new-*) printf '2\n' ;;
      old-*) printf '1\n' ;;
      *) printf '0\n' ;;
    esac
  else printf 'false\n'; fi
}
'''
        good = run_protocol(setup + 'runtime_protocol_preflight; runtime_protocol_quiesce')
        self.assertEqual(good.returncode, 0, good.stderr)
        self.assertIn('data-analysis-aggregation-worker', good.stdout)
        mixed = run_protocol(setup + 'data_analysis_image_refs[3]=legacy-migrate; runtime_protocol_preflight')
        self.assertNotEqual(mixed.returncode, 0)
        older_worker = run_protocol(setup + r'''
docker() { if [[ "$1" == image ]]; then
  case "$3" in old-AGG) printf '3\n';; new-*) printf '2\n';; *) printf '1\n';; esac
  else printf 'false\n'; fi; }
runtime_protocol_preflight
''')
        self.assertNotEqual(older_worker.returncode, 0)

    # 用途：验证升级先排空旧写者，候选可能写入后禁止旧指针恢复。
    # 参数：self 为测试上下文。
    # 返回值：无；跨代回滚或排空缺失时失败。
    def test_cross_epoch_quiesces_and_blocks_old_pointer(self):
        result = run_protocol('runtime_protocol_preflight; runtime_protocol_quiesce; '
                              'runtime_protocol_rollback_allowed; runtime_write_started=true; '
                              'if runtime_protocol_rollback_allowed; then exit 99; fi')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("STOPPED stop", result.stdout)
        guard = SOURCE.index('if ! runtime_protocol_rollback_allowed; then', SOURCE.index('echo "发布失败'))
        pointer = SOURCE.index('mv -f "$previous_release" "$release_file"', guard)
        self.assertLess(guard, pointer)

    # 用途：验证同代回滚维持现有可用性，不新增停机步骤。
    # 参数：self 为测试上下文。
    # 返回值：无；同代升级被不必要阻断时失败。
    def test_same_epoch_keeps_existing_rollback(self):
        result = run_protocol('old_ref=new; runtime_protocol_preflight; runtime_protocol_quiesce; '
                              'runtime_write_started=true; runtime_protocol_rollback_allowed')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn("STOPPED", result.stdout)

    # 用途：验证明确降代在改变发布指针之前拒绝。
    # 参数：self 为测试上下文。
    # 返回值：无；新协议降为未知旧协议仍可发布时失败。
    def test_downgrade_is_rejected_before_release_pointer(self):
        result = run_protocol('old_ref=new; image_ref=old; runtime_protocol_preflight')
        self.assertNotEqual(result.returncode, 0)
        self.assertLess(SOURCE.index('runtime_protocol_preflight || exit 1'),
                        SOURCE.index('previous_release="$(mktemp'))

    # 用途：验证停止旧写者失败时不得继续换代。
    # 参数：self 为测试上下文。
    # 返回值：无；排空失败被吞掉时失败。
    def test_stop_failure_stops_cutover(self):
        result = run_protocol('runtime_protocol_preflight; stop_result=9; runtime_protocol_quiesce')
        self.assertNotEqual(result.returncode, 0)

    # 用途：执行真实失败分支，确认跨代后指针和控制面均不降回旧版本。
    # 参数：self 为测试上下文；文件只在本测试独占的临时目录。
    # 返回值：无；旧指针被恢复或旧进程被要求启动时失败。
    def test_failure_branch_preserves_pointer_and_does_not_start_old_writer(self):
        with tempfile.TemporaryDirectory(prefix="runtime-epoch-test-") as directory:
            current = Path(directory) / "current"
            previous = Path(directory) / "previous"
            current.write_text("NEW")
            previous.write_text("OLD")
            body = ('runtime_protocol_preflight; runtime_write_started=true; '
                    'release_file=' + shlex.quote(str(current)) + '; previous_release=' + shlex.quote(str(previous)) + '; '
                    'record_recovery_result() { :; }; '
                    'refresh_subsystem_provisioner_config() { echo UNEXPECTED_REFRESH; }; '
                    'rollback_runtime() { echo UNEXPECTED_OLD_START; };\n')
            result = run_protocol(body + SOURCE[SOURCE.index('echo "发布失败'):])
            self.assertNotEqual(result.returncode, 0)
            self.assertEqual(current.read_text(), "NEW")
            self.assertEqual(previous.read_text(), "OLD")
            self.assertNotIn("UNEXPECTED", result.stdout)


if __name__ == "__main__":
    unittest.main()
