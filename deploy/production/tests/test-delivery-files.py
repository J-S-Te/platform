#!/usr/bin/env python3
"""文件系统检查点证明使用真实字节，并拒绝不完整状态。"""
import importlib.util
import io
from pathlib import Path
import tarfile
import tempfile
import unittest

spec=importlib.util.spec_from_file_location('delivery_files',Path(__file__).resolve().parents[1]/'bin/delivery-files.py')
module=importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


class FilesTests(unittest.TestCase):
    def setUp(self):
        temp=tempfile.TemporaryDirectory(prefix='delivery-files-')
        self.addCleanup(temp.cleanup)
        self.root=Path(temp.name).resolve()
        self.target=self.root/'target'
        self.target.mkdir()
        self.archive=self.root/'assets.tar.gz'

    def assets(self, entries):
        with tarfile.open(self.archive,'w:gz') as bundle:
            root=tarfile.TarInfo('./');root.type=tarfile.DIRTYPE;bundle.addfile(root)
            for name, raw in entries.items():
                info=tarfile.TarInfo('./'+name);info.size=len(raw);bundle.addfile(info,io.BytesIO(raw))

    def write(self,name,raw=b'current'):
        path=self.target/name;path.parent.mkdir(parents=True,exist_ok=True);path.write_bytes(raw)
        return path

    def test_root_tar_member_and_immutable_assets_are_checked(self):
        self.assets({'bin/tool.py':b'current','subsystems.d/app.yaml':b'profile','docker-compose.yml':b'new template'})
        self.write('bin/tool.py');self.write('subsystems.d/app.yaml',b'profile');self.write('docker-compose.yml',b'operator module selection')
        self.assertTrue(module.assets_match(self.archive,self.target))
        self.write('bin/tool.py',b'other release')
        self.assertFalse(module.assets_match(self.archive,self.target))

    def test_unfinished_asset_transaction_cannot_be_proved_complete(self):
        self.assets({'bin/tool.py':b'current'});self.write('bin/tool.py')
        (self.target/'runtime/.assets-install-transaction').mkdir(parents=True)
        self.assertFalse(module.assets_match(self.archive,self.target))

    def test_approval_projection_checks_real_runtime_directory(self):
        self.assets({'runtime-license-contract_management-prod.json':b'approved'})
        self.write('runtime-approvals/runtime-license-contract_management-prod.json',b'approved')
        self.assertTrue(module.assets_match(self.archive,self.target))
        self.write('runtime-approvals/runtime-license-contract_management-prod.json',b'changed')
        self.assertFalse(module.assets_match(self.archive,self.target))

    def test_symbolic_asset_or_parent_rejected(self):
        self.assets({'bin/tool.py':b'current'})
        elsewhere=self.root/'other';elsewhere.mkdir();(elsewhere/'tool.py').write_bytes(b'current')
        (self.target/'bin').symlink_to(elsewhere,target_is_directory=True)
        with self.assertRaises(ValueError): module.assets_match(self.archive,self.target)

    def test_duplicate_and_traversal_archives_rejected(self):
        self.assets({'../outside':b'x'})
        with self.assertRaises(ValueError): module.assets_match(self.archive,self.target)
        self.assertFalse((self.root/'outside').exists())

    def test_config_requires_both_nonempty_private_files(self):
        self.write('.env',b'APP_PUBLIC_BASE_URL=http://example.test\n').chmod(0o600)
        release=self.write('.release.env',b'PLATFORM_IMAGE=expected\n');release.chmod(0o600)
        self.assertTrue(module.config_match(self.target))
        release.chmod(0o644);self.assertFalse(module.config_match(self.target))
        release.chmod(0o600);release.write_bytes(b'');self.assertFalse(module.config_match(self.target))

    def test_same_filename_does_not_prove_package_content(self):
        left=self.root/'package.tar.gz';left.write_bytes(b'original')
        right=self.write('packages/package.tar.gz',b'original')
        self.assertTrue(module.same_file(left,right))
        right.write_bytes(b'changed');self.assertFalse(module.same_file(left,right))


if __name__=='__main__': unittest.main()
