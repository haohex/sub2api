import json
import os
from pathlib import Path
import tarfile
import tempfile
import unittest
from unittest.mock import patch
import zipfile

import build


class BuildTests(unittest.TestCase):
    def test_full_bundle_platforms_archives_and_image_context(self):
        self.check_bundle(False)

    def test_legacy_simple_flag_cannot_disable_full_artifacts(self):
        self.check_bundle(True)

    def check_bundle(self, simple):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            (root / 'backend/cmd/server').mkdir(parents=True)
            (root / 'backend/resources').mkdir()
            (root / 'deploy').mkdir()
            (root / 'frontend').mkdir()
            (root / 'LICENSE').write_text('license')
            (root / 'deploy/docker-entrypoint.sh').write_text('#!/bin/sh\n')
            (root / 'Dockerfile.goreleaser').write_text('FROM scratch\nCOPY sub2api /app/sub2api\n')
            output = root / 'bundle'
            calls = []

            def run(*args, **kwargs):
                calls.append(args)
                if args[0] == 'go':
                    binary = Path(args[args.index('-o') + 1])
                    binary.write_text(kwargs['env']['GOOS'] + '/' + kwargs['env']['GOARCH'])
                if args[0] == 'docker':
                    context = Path(args[-1])
                    self.assertIn('COPY bin/${TARGETARCH}/sub2api', (context / 'Dockerfile').read_text())
                    self.assertEqual((context / 'bin/amd64/sub2api').read_text(), 'linux/amd64')
                    self.assertTrue((context / 'backend/resources').is_dir())
                    self.assertEqual((context / 'bin/arm64/sub2api').read_text(), 'linux/arm64')
                    (output / 'image.tar').write_bytes(b'oci')

            original = Path.cwd()
            try:
                os.chdir(root)
                with patch.dict(os.environ, RELEASE_TAG='v0.2.4-hao.10', RELEASE_SHA='a' * 40,
                                BUNDLE_DIR=str(output), SIMPLE_RELEASE=str(simple).lower()):
                    with patch.object(build, 'run', side_effect=run), patch.object(
                            build.subprocess, 'check_output', return_value='2026-09-12T00:00:00Z'):
                        build.main()
            finally:
                os.chdir(original)
            manifest = json.loads((output / 'bundle.json').read_text())
            self.assertFalse(manifest['simple'])
            self.assertEqual(len(manifest['files']), 7)
            self.assertEqual(len([c for c in calls if c[0] == 'go']), 5)
            with tarfile.open(output / 'sub2api_0.2.4-hao.10_linux_amd64.tar.gz') as archive:
                self.assertEqual(archive.extractfile('sub2api').read(), b'linux/amd64')
                self.assertIn('deploy/docker-entrypoint.sh', archive.getnames())
            with zipfile.ZipFile(output / 'sub2api_0.2.4-hao.10_windows_amd64.zip') as archive:
                self.assertEqual(archive.read('sub2api.exe'), b'windows/amd64')


if __name__ == '__main__':
    unittest.main()
