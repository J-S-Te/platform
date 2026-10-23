#!/usr/bin/env python3
"""独立验证 OCI 身份命令行，仅使用临时的元数据归档。

不使用 Docker、网络、实际业务归档、凭据，也不导入被测实现。
夹具遵循 OCI image-spec v1.1.0；预期摘要直接取夹具字节的 SHA256，
不从被测工具获取。
"""
import copy
import hashlib
import io
import json
from pathlib import Path
import subprocess
import sys
import tarfile
import tempfile
import unittest

HELPER = Path(__file__).resolve().parents[1] / 'bin/offline-image-identities.py'
INDEX = 'application/vnd.oci.image.index.v1+json'
MANIFEST = 'application/vnd.oci.image.manifest.v1+json'
CONFIG = 'application/vnd.oci.image.config.v1+json'
TAG = 'uip-package/portal-backend:test-20261014'


def encoded(value):
    return json.dumps(value, sort_keys=True, separators=(',', ':')).encode()


def digest(raw):
    return 'sha256:' + hashlib.sha256(raw).hexdigest()


def blob_path(value):
    return 'blobs/sha256/' + value.split(':', 1)[1]


class MetadataFixture:
    def __init__(self):
        self.members = {'oci-layout': encoded({'imageLayoutVersion': '1.0.0'})}
        self.roots = []

    def blob(self, raw, media_type):
        value = digest(raw)
        self.members[blob_path(value)] = raw
        return {'mediaType': media_type, 'digest': value, 'size': len(raw)}

    def image(self, label='one', os='linux', arch='amd64', variant='', config_raw=None):
        config = {'architecture': arch, 'os': os, 'config': {'Labels': {'fixture': label}},
                  'rootfs': {'type': 'layers', 'diff_ids': []}, 'history': []}
        if variant:
            config['variant'] = variant
        config_descriptor = self.blob(config_raw if config_raw is not None else encoded(config), CONFIG)
        manifest = {'schemaVersion': 2, 'mediaType': MANIFEST, 'config': config_descriptor, 'layers': []}
        descriptor = self.blob(encoded(manifest), MANIFEST)
        descriptor['platform'] = {'os': os, 'architecture': arch}
        if variant:
            descriptor['platform']['variant'] = variant
        return descriptor, config_descriptor['digest']

    def index(self, descriptors):
        return self.blob(encoded({'schemaVersion': 2, 'mediaType': INDEX, 'manifests': descriptors}), INDEX)

    def tag(self, descriptor, tag=TAG, bare=True):
        tagged = copy.deepcopy(descriptor)
        tagged['annotations'] = {'io.containerd.image.name': tag}
        if bare:
            tagged['annotations']['org.opencontainers.image.ref.name'] = tag.rsplit(':', 1)[1]
        self.roots.append(tagged)
        return tagged

    def root_raw(self):
        return encoded({'schemaVersion': 2, 'mediaType': INDEX, 'manifests': self.roots})

    def replace_blob(self, descriptor, raw, rehash=True):
        if rehash:
            replacement = self.blob(raw, descriptor['mediaType'])
            descriptor.update(replacement)
        else:
            self.members[blob_path(descriptor['digest'])] = raw

    def archive(self, path, root_raw=None, extra=(), override=None):
        members = dict(self.members)
        members['index.json'] = self.root_raw() if root_raw is None else root_raw
        if override:
            members.update(override)
        with tarfile.open(path, 'w', format=tarfile.USTAR_FORMAT) as archive:
            for name, raw in members.items():
                info = tarfile.TarInfo(name)
                info.size = len(raw)
                info.mode = 0o644
                archive.addfile(info, io.BytesIO(raw))
            for info, raw in extra:
                archive.addfile(info, io.BytesIO(raw) if raw is not None else None)
        return path


class OfflineImageIdentityTests(unittest.TestCase):
    def setUp(self):
        self.assertTrue(HELPER.is_file(), 'new offline identity CLI helper has not been implemented')
        self.temp = tempfile.TemporaryDirectory(prefix='uip-image-identity-fixture-')
        self.addCleanup(self.temp.cleanup)
        self.archive = Path(self.temp.name) / 'image.tar'

    def call(self, tags, configs):
        return subprocess.run([sys.executable, str(HELPER), '--archive', str(self.archive),
                               '--images', ','.join(tags), '--config-digests', ','.join(configs)],
                              text=True, capture_output=True, timeout=10)

    def success(self, fixture, config, identities, tags=None, configs=None):
        fixture.archive(self.archive)
        result = self.call(tags or [TAG], configs or [config])
        self.assertEqual(result.returncode, 0, result.stderr)
        actual = json.loads(result.stdout)
        expected = {'images': [{'tag': tag, 'config_digest': expected_config,
                                'identities': [{'kind': kind, 'digest': value} for kind, value in ids]}
                               for tag, expected_config, ids in identities]}
        # 身份集合内的顺序无关紧要，但不允许增加额外身份。
        for image in actual['images']:
            image['identities'].sort(key=lambda item: (item['kind'], item['digest']))
        for image in expected['images']:
            image['identities'].sort(key=lambda item: (item['kind'], item['digest']))
        self.assertEqual(actual, expected)
        self.assertFalse(any(item['digest'] == image['config_digest']
                             for image in actual['images'] for item in image['identities']))
        return actual

    def rejected(self, fixture, config, code=1, root_raw=None, extra=(), override=None,
                 tags=None, configs=None):
        fixture.archive(self.archive, root_raw=root_raw, extra=extra, override=override)
        result = self.call(tags or [TAG], configs or [config])
        self.assertEqual(result.returncode, code, result.stdout + result.stderr)
        self.assertFalse(result.stdout.strip(), 'failure must not leak a partial trusted identity result')

    def multiarch(self):
        fixture = MetadataFixture()
        amd, config = fixture.image()
        arm, _ = fixture.image(label='arm', arch='arm64', variant='v8')
        attestation, _ = fixture.image(label='attestation', os='unknown', arch='unknown')
        outer = fixture.index([arm, amd, attestation])
        fixture.tag(outer)
        return fixture, amd, config, outer

    def test_verified_index_and_selected_manifest_not_config_are_identities(self):
        f, amd, config, outer = self.multiarch()
        self.success(f, config, [(TAG, config, [('index', outer['digest']), ('manifest', amd['digest'])])])

    def test_direct_manifest_works_without_outer_index_identity(self):
        f = MetadataFixture()
        manifest, config = f.image()
        manifest.pop('platform')  # OCI 布局标签可依赖已验证的镜像配置。
        f.tag(manifest)
        self.success(f, config, [(TAG, config, [('manifest', manifest['digest'])])])

    def test_nested_index_reports_only_verified_selected_path(self):
        f = MetadataFixture()
        amd, config = f.image()
        arm, _ = f.image(label='arm', arch='arm64')
        inner = f.index([amd, arm])
        outer = f.index([inner])
        f.tag(outer)
        self.success(f, config, [(TAG, config, [('index', outer['digest']), ('index', inner['digest']),
                                              ('manifest', amd['digest'])])])

    def test_batch_tags_resolve_their_own_configs_and_digests(self):
        f, first, config1, outer = self.multiarch()
        second, config2 = f.image(label='two')
        tag2 = 'uip-package/project-backend:test-20261014'
        f.tag(second, tag2)
        self.success(f, config1, [(TAG, config1, [('index', outer['digest']), ('manifest', first['digest'])]),
                                  (tag2, config2, [('manifest', second['digest'])])],
                     tags=[TAG, tag2], configs=[config1, config2])

    def test_equivalent_docker_full_tag_normalization(self):
        for supplied, stored in [('mysql:8.4', 'docker.io/library/mysql:8.4'),
                                 ('docker.io/library/mysql:8.4', 'mysql:8.4'),
                                 (TAG, 'docker.io/' + TAG), ('docker.io/' + TAG, TAG)]:
            with self.subTest(supplied=supplied, stored=stored):
                f = MetadataFixture()
                manifest, config = f.image()
                f.tag(manifest, stored)
                self.success(f, config, [(supplied, config, [('manifest', manifest['digest'])])], tags=[supplied])

    def test_wrong_full_tag_or_bare_alias_alone_cannot_prove_tag(self):
        for annotations in [{'io.containerd.image.name': 'unrelated/repo:test-20261014',
                             'org.opencontainers.image.ref.name': 'test-20261014'},
                            {'org.opencontainers.image.ref.name': 'test-20261014'},
                            {'org.opencontainers.image.ref.name': '最新'},
                            {'io.containerd.image.name': TAG,
                             'org.opencontainers.image.ref.name': 'other/repo:test-20261014'}]:
            with self.subTest(annotations=annotations):
                f = MetadataFixture()
                manifest, config = f.image()
                tagged = f.tag(manifest)
                tagged['annotations'] = annotations
                code = 1 if ('io.containerd.image.name' in annotations and
                             annotations.get('org.opencontainers.image.ref.name', '').startswith('other/')) else 2
                self.rejected(f, config, code=code)

    def test_same_normalized_tag_multiple_descriptors_is_ambiguous_even_same_digest(self):
        f = MetadataFixture()
        manifest, config = f.image()
        f.tag(manifest)
        f.tag(manifest, 'docker.io/' + TAG)
        self.rejected(f, config)

    def test_wrong_platform_variant_or_config_platform_is_rejected(self):
        for os, arch, variant in [('linux', 'arm64', ''), ('windows', 'amd64', ''),
                                  ('linux', 'amd64', 'v1'), ('linux', 'amd64', 'v3')]:
            with self.subTest(os=os, arch=arch, variant=variant):
                f = MetadataFixture()
                manifest, config = f.image(os=os, arch=arch, variant=variant)
                f.tag(f.index([manifest]))
                self.rejected(f, config)
        f = MetadataFixture()
        manifest, config = f.image(arch='arm64')
        manifest['platform'] = {'os': 'linux', 'architecture': 'amd64'}
        f.tag(f.index([manifest]))
        self.rejected(f, config)

    def test_duplicate_amd64_candidates_are_rejected(self):
        for identical in [True, False]:
            with self.subTest(identical=identical):
                f = MetadataFixture()
                manifest, config = f.image()
                other = copy.deepcopy(manifest) if identical else f.image(label='different')[0]
                f.tag(f.index([manifest, other]))
                self.rejected(f, config)

    def test_tampered_content_addressed_index_manifest_and_config_rejected(self):
        for part in ['index', 'manifest', 'config']:
            with self.subTest(part=part):
                f, manifest, config, outer = self.multiarch()
                value = {'index': outer['digest'], 'manifest': manifest['digest'], 'config': config}[part]
                f.members[blob_path(value)] += b' '
                self.rejected(f, config)

    def test_wrong_expected_config_and_batch_partial_success_are_rejected(self):
        f, _, config, _ = self.multiarch()
        self.rejected(f, config, configs=['sha256:' + '0' * 64])
        other, config2 = f.image(label='two')
        tag2 = 'uip-package/project-backend:test-20261014'
        f.tag(other, tag2)
        self.rejected(f, config, tags=[TAG, tag2], configs=[config, 'sha256:' + '0' * 64])

    def test_declared_metadata_size_must_be_exact(self):
        for location in ['root-index-descriptor', 'manifest-child', 'config-descriptor']:
            with self.subTest(location=location):
                f = MetadataFixture()
                manifest, config = f.image()
                if location == 'config-descriptor':
                    body = json.loads(f.members[blob_path(manifest['digest'])])
                    body['config']['size'] += 1
                    f.replace_blob(manifest, encoded(body))
                if location == 'manifest-child':
                    manifest['size'] += 1
                tagged = f.tag(f.index([manifest]))
                if location == 'root-index-descriptor':
                    tagged['size'] += 1
                self.rejected(f, config)

    def test_duplicate_json_keys_in_root_index_selected_index_manifest_and_config_rejected(self):
        f, _, config, _ = self.multiarch()
        root = f.root_raw().replace(b'"schemaVersion":2', b'"schemaVersion":1,"schemaVersion":2', 1)
        self.rejected(f, config, root_raw=root)
        for location in ['index', 'manifest', 'config']:
            with self.subTest(location=location):
                f = MetadataFixture()
                config_raw = (b'{"architecture":"arm64","architecture":"amd64","os":"linux",'
                              b'"rootfs":{"type":"layers","diff_ids":[]},"config":{}}') if location == 'config' else None
                manifest, config = f.image(config_raw=config_raw)
                if location == 'manifest':
                    raw = f.members[blob_path(manifest['digest'])]
                    f.replace_blob(manifest, raw.replace(b'"schemaVersion":2', b'"schemaVersion":1,"schemaVersion":2', 1))
                outer = f.index([manifest])
                if location == 'index':
                    raw = f.members[blob_path(outer['digest'])]
                    f.replace_blob(outer, raw.replace(b'"schemaVersion":2', b'"schemaVersion":1,"schemaVersion":2', 1))
                f.tag(outer)
                self.rejected(f, config)

    def test_selected_index_and_manifest_schema_versions_are_verified(self):
        for which in ['index', 'manifest']:
            with self.subTest(schema=which):
                f = MetadataFixture()
                manifest, config = f.image()
                if which == 'manifest':
                    payload = json.loads(f.members[blob_path(manifest['digest'])])
                    payload['schemaVersion'] = 1
                    f.replace_blob(manifest, encoded(payload))
                outer = f.index([manifest])
                if which == 'index':
                    payload = json.loads(f.members[blob_path(outer['digest'])])
                    payload['schemaVersion'] = 1
                    f.replace_blob(outer, encoded(payload))
                f.tag(outer)
                self.rejected(f, config)

    def test_nonfinite_json_numbers_are_rejected_in_every_metadata_document(self):
        for which in ['root', 'index', 'manifest', 'config', 'layout']:
            for constant in ['NaN', 'Infinity', '-Infinity']:
                with self.subTest(document=which, constant=constant):
                    f = MetadataFixture()
                    config_payload = encoded({'architecture': 'amd64', 'os': 'linux',
                                              'rootfs': {'type': 'layers', 'diff_ids': []}, 'config': {}})
                    inject = lambda raw: ('{"qa.number":' + constant + ',').encode() + raw[1:]
                    manifest, config = f.image(config_raw=inject(config_payload) if which == 'config' else None)
                    if which == 'manifest':
                        f.replace_blob(manifest, inject(f.members[blob_path(manifest['digest'])]))
                    outer = f.index([manifest])
                    if which == 'index':
                        f.replace_blob(outer, inject(f.members[blob_path(outer['digest'])]))
                    f.tag(outer)
                    if which == 'layout':
                        f.members['oci-layout'] = inject(f.members['oci-layout'])
                    self.rejected(f, config, root_raw=inject(f.root_raw()) if which == 'root' else None)

    def test_schema_version_requires_integer_not_float_or_string(self):
        for which in ['root', 'index', 'manifest']:
            for version in [2.0, '2', True]:
                with self.subTest(document=which, schema_version=version):
                    f = MetadataFixture()
                    manifest, config = f.image()
                    if which == 'manifest':
                        body = json.loads(f.members[blob_path(manifest['digest'])])
                        body['schemaVersion'] = version
                        f.replace_blob(manifest, encoded(body))
                    outer = f.index([manifest])
                    if which == 'index':
                        body = json.loads(f.members[blob_path(outer['digest'])])
                        body['schemaVersion'] = version
                        f.replace_blob(outer, encoded(body))
                    f.tag(outer)
                    root = json.loads(f.root_raw())
                    if which == 'root':
                        root['schemaVersion'] = version
                    self.rejected(f, config, root_raw=encoded(root))

    def test_body_media_type_is_optional_but_when_present_must_match_descriptor(self):
        for which in ['manifest', 'index', 'root']:
            with self.subTest(optional_media_type=which):
                f = MetadataFixture()
                manifest, config = f.image()
                if which == 'manifest':
                    body = json.loads(f.members[blob_path(manifest['digest'])])
                    del body['mediaType']
                    f.replace_blob(manifest, encoded(body))
                outer = f.index([manifest])
                if which == 'index':
                    body = json.loads(f.members[blob_path(outer['digest'])])
                    del body['mediaType']
                    f.replace_blob(outer, encoded(body))
                f.tag(outer)
                if which == 'root':
                    body = json.loads(f.root_raw())
                    del body['mediaType']
                    # success() 会写入 root_raw()，因此用夹具内部的闭包覆盖它。
                    f.root_raw = lambda: encoded(body)
                self.success(f, config, [(TAG, config, [('index', outer['digest']),
                                                       ('manifest', manifest['digest'])])])
        for which in ['index', 'manifest']:
            with self.subTest(contradictory_media_type=which):
                f = MetadataFixture()
                manifest, config = f.image()
                if which == 'manifest':
                    body = json.loads(f.members[blob_path(manifest['digest'])])
                    body['mediaType'] = INDEX
                    f.replace_blob(manifest, encoded(body))
                outer = f.index([manifest])
                if which == 'index':
                    body = json.loads(f.members[blob_path(outer['digest'])])
                    body['mediaType'] = MANIFEST
                    f.replace_blob(outer, encoded(body))
                f.tag(outer)
                self.rejected(f, config)

    def test_unsafe_tar_paths_links_and_duplicate_metadata_rejected(self):
        for name in ['../escape.json', '/absolute.json', 'blobs/../escape.json']:
            with self.subTest(name=name):
                f, _, config, _ = self.multiarch()
                info = tarfile.TarInfo(name)
                info.size = 2
                self.rejected(f, config, extra=[(info, b'{}')])
        for kind in [tarfile.SYMTYPE, tarfile.LNKTYPE]:
            with self.subTest(link=kind):
                f, _, config, _ = self.multiarch()
                info = tarfile.TarInfo('metadata-link')
                info.type = kind
                info.linkname = blob_path(config)
                self.rejected(f, config, extra=[(info, None)])
        for which in ['index.json', 'config']:
            with self.subTest(duplicate=which):
                f, _, config, _ = self.multiarch()
                name = 'index.json' if which == 'index.json' else blob_path(config)
                raw = f.root_raw() if which == 'index.json' else f.members[name]
                info = tarfile.TarInfo(name)
                info.size = len(raw)
                self.rejected(f, config, extra=[(info, raw)])

    def test_bounded_metadata_and_nested_index_depth_rejected(self):
        padding = 'x' * (16 * 1024 * 1024)
        for which in ['root', 'index', 'config']:
            with self.subTest(oversized=which):
                f = MetadataFixture()
                oversized_config = encoded({'architecture': 'amd64', 'os': 'linux',
                                            'rootfs': {'type': 'layers', 'diff_ids': []},
                                            'config': {'Labels': {'qa.padding': padding}}})
                manifest, config = f.image(config_raw=oversized_config if which == 'config' else None)
                outer = f.index([manifest])
                if which == 'index':
                    oversized_index = encoded({'schemaVersion': 2, 'mediaType': INDEX,
                                               'manifests': [manifest],
                                               'annotations': {'qa.padding': padding}})
                    f.replace_blob(outer, oversized_index)
                f.tag(outer)
                oversized_root = encoded({'schemaVersion': 2, 'mediaType': INDEX,
                                          'manifests': f.roots,
                                          'annotations': {'qa.padding': padding}})
                self.rejected(f, config, root_raw=oversized_root if which == 'root' else None)
        f = MetadataFixture()
        manifest, config = f.image()
        child = manifest
        for _ in range(9):
            child = f.index([child])
        f.tag(child)
        self.rejected(f, config)

    def test_invalid_schema_layout_media_digest_missing_blob_and_json_rejected(self):
        for failure in ['root-schema', 'layout', 'media', 'digest', 'missing', 'json']:
            with self.subTest(failure=failure):
                f, manifest, config, _ = self.multiarch()
                root_raw = None
                if failure == 'root-schema':
                    root = json.loads(f.root_raw())
                    root['schemaVersion'] = 1
                    root_raw = encoded(root)
                elif failure == 'layout':
                    f.members['oci-layout'] = encoded({'imageLayoutVersion': '9.9.9'})
                elif failure == 'media':
                    f.roots[0]['mediaType'] = 'application/x-unsupported'
                elif failure == 'digest':
                    f.roots[0]['digest'] = 'sha256:../escape'
                elif failure == 'missing':
                    del f.members[blob_path(manifest['digest'])]
                elif failure == 'json':
                    f.members['index.json'] = b'ignored'
                    root_raw = b'not-json'
                self.rejected(f, config, code=2 if failure == 'layout' else 1, root_raw=root_raw)

    def test_classic_archive_is_explicitly_unsupported(self):
        f = MetadataFixture()
        _, config = f.image()
        config_raw = f.members[blob_path(config)]
        f.members = {config.split(':')[1] + '.json': config_raw,
                     'manifest.json': encoded([{'Config': config.split(':')[1] + '.json', 'RepoTags': [TAG], 'Layers': []}])}
        # 经典 docker save 归档中不要添加 OCI index.json。
        with tarfile.open(self.archive, 'w') as archive:
            for name, raw in f.members.items():
                info = tarfile.TarInfo(name)
                info.size = len(raw)
                archive.addfile(info, io.BytesIO(raw))
        result = self.call([TAG], [config])
        self.assertEqual(result.returncode, 2, result.stdout + result.stderr)
        self.assertFalse(result.stdout.strip())

    def test_mixed_classic_and_oci_uses_only_verified_oci_path(self):
        f, manifest, config, outer = self.multiarch()
        f.members[config.split(':')[1] + '.json'] = f.members[blob_path(config)]
        f.members['manifest.json'] = encoded([{'Config': config.split(':')[1] + '.json', 'RepoTags': [TAG], 'Layers': []}])
        self.success(f, config, [(TAG, config, [('index', outer['digest']), ('manifest', manifest['digest'])])])
        f.roots[0]['annotations'] = {'io.containerd.image.name': 'other/repo:other'}
        self.rejected(f, config, code=2)

    def test_invalid_cli_pairings_and_repeated_tag_are_rejected(self):
        f, _, config, _ = self.multiarch()
        self.rejected(f, config, tags=[TAG, TAG], configs=[config, config])
        self.rejected(f, config, tags=[TAG, 'other/repo:v'], configs=[config])
        self.rejected(f, config, configs=['not-a-sha256'])


if __name__ == '__main__':
    unittest.main(verbosity=2)
