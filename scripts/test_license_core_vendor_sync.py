"""验证受控核心同步不会留下旧vendor副本，不读取任何运行密钥。"""
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest


class LicenseVendorSyncTest(unittest.TestCase):
    # 用途：验证权威、mirror及vendor能通过同一个白名单同步入口保持一致。
    # 参数：self为独立临时目录测试上下文。
    # 返回值：无；同步后仍漂移时测试失败。
    def test_sync_updates_existing_vendor_without_expanding_allowlist(self):
        script = Path(__file__).with_name('license-core-sync.sh')
        production = ['catalog/catalog.go', 'clock.go', 'diff.go', 'license.go',
                      'recovery.go', 'runtime/lock_unix.go', 'runtime/snapshot.go',
                      'runtime/state.go']
        with tempfile.TemporaryDirectory(prefix='license-vendor-sync-') as directory:
            base = Path(directory)
            target, source = base / 'platform', base / 'authority'
            (target / 'scripts').mkdir(parents=True)
            shutil.copy2(script, target / 'scripts/license-core-sync.sh')
            for name in production + ['go.mod']:
                path = source / name
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text('authority fixture\n')
            vendor = target / 'vendor/github.com/J-S-Te/license-core'
            for name in production:
                path = vendor / name
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text('old vendor fixture\n')
            synced = subprocess.run(['sh', str(target / 'scripts/license-core-sync.sh'),
                                     '--sync', str(source)], capture_output=True, text=True)
            self.assertEqual(synced.returncode, 0, synced.stderr)
            checked = subprocess.run(['sh', str(target / 'scripts/license-core-sync.sh'),
                                      '--check', str(source)], capture_output=True, text=True)
            self.assertEqual(checked.returncode, 0, checked.stderr)
            self.assertEqual(sorted(str(path.relative_to(vendor)) for path in vendor.rglob('*')
                                    if path.is_file()), sorted(production))


if __name__ == '__main__':
    unittest.main()
