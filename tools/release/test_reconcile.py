import base64
import copy
import hashlib
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from reconcile import GitHub, GitHubError, Reconciler, body_for, expected_files, state_of, version_key

HEAD, MERGE, TREE, OTHER = 'a' * 40, 'b' * 40, 'c' * 40, 'd' * 40
DIGEST = 'sha256:' + 'e' * 64


def pr(number=5, merged=True):
    return {'number': number, 'state': 'closed' if merged else 'open', 'draft': False,
            'merged': merged, 'merged_at': '2026-09-12T10:00:00Z' if merged else None,
            'merge_commit_sha': MERGE, 'base': {'ref': 'main'},
            'head': {'sha': HEAD, 'ref': 'feature', 'repo': {'full_name': 'owner/repo'}}}


def release(number=10, ready=False, stable=False, **extra):
    state = dict(schema=2, tag=f'v0.2.4-hao.{number}', pr=5, sha=MERGE, tree=TREE,
                 simple=False, ready=ready, merged_at='2026-09-12T10:00:00Z')
    if ready:
        state['files'] = {name: 'f' * 64 for name in expected_files(state['tag'])}
    state.update(extra)
    return {'id': number, 'tag_name': f'v0.2.4-hao.{number}', 'body': body_for(state),
            'target_commitish': MERGE, 'draft': not stable, 'prerelease': False,
            'published_at': '2026-09-12T09:00:00Z' if stable else None}


class FakeGitHub:
    repo = 'owner/repo'

    def __init__(self, prs=None, releases=None):
        self.prs = prs or []
        self.releases = releases or []
        self.tags, self.artifacts, self.assets, self.calls = [], [], [], []
        self.tree, self.tag_sha = TREE, MERGE
        self.version = '0.2.4-hao.8'
        self.assets = [{'name': name, 'digest': 'sha256:' + digest}
                       for r in self.releases for name, digest in (state_of(r) or {}).get('files', {}).items()
                       if name != 'image.tar']

    def pages(self, path, field=None):
        if path == 'releases':
            return self.releases
        if path.startswith('pulls?') or path.endswith('/pulls'):
            return self.prs
        if path == 'tags':
            return self.tags
        if path.startswith('actions/'):
            return self.artifacts
        if path.endswith('/assets'):
            return self.assets
        raise AssertionError(path)

    def upload(self, release_id, path):
        self.assets.append({'name': path.name, 'digest': 'sha256:' + hashlib.sha256(path.read_bytes()).hexdigest()})

    def api(self, path, method='GET', data=None, missing=False, token_env='GH_TOKEN'):
        self.calls.append((path, method, copy.deepcopy(data)))
        if path == '':
            return {'default_branch': 'main'}
        if path.startswith('pulls/'):
            return next(p for p in self.prs if str(p['number']) == path.split('/')[1])
        if path.startswith('commits/'):
            return {'commit': {'tree': {'sha': self.tree}}}
        if path.startswith('git/ref/'):
            return {'object': {'type': 'commit', 'sha': self.tag_sha}} if self.tag_sha else None
        if path == 'git/refs' and method == 'POST':
            self.tags.append({'name': data['ref'].removeprefix('refs/tags/')})
            self.tag_sha, self.tag_token = data['sha'], token_env
            return {'object': {'type': 'commit', 'sha': data['sha']}}
        if path.startswith('contents/') and method == 'GET':
            return {'content': base64.b64encode(self.version.encode()).decode(), 'sha': OTHER}
        if path == 'releases' and method == 'POST':
            created = dict(data, id=100 + len(self.releases), published_at=None)
            self.releases.append(created)
            return created
        if path.startswith('releases/') and method == 'PATCH':
            item = next(r for r in self.releases if str(r['id']) == path.split('/')[1])
            item.update(data)
            return item
        raise AssertionError((path, method))


class ReconcileTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.output = Path(self.temp.name) / 'output'
        env = patch.dict(os.environ, {'GITHUB_OUTPUT': str(self.output),
                         'AUTOMATION_SINCE': '2026-09-12T00:00:00+00:00',
                         'GITHUB_RUN_ID': '123', 'RECOVER_TAG': '', 'DOCKERHUB_USERNAME': ''})
        env.start()
        self.addCleanup(env.stop)

    def plan(self, gh):
        Reconciler(gh).plan()
        return dict(line.split('=', 1) for line in self.output.read_text().splitlines())

    def test_version_order_is_numeric(self):
        self.assertGreater(version_key('v0.2.4-hao.10'), version_key('v0.2.4-hao.9'))
        self.assertIsNone(version_key('v0.2.4;bad'))

    def test_open_draft_closed_and_historical_prs_never_allocate(self):
        opened = pr(merged=False)
        draft = dict(opened, number=6, draft=True)
        closed = dict(opened, number=7, state='closed')
        historic = dict(pr(8), merged_at='2026-09-11T00:00:00Z')
        gh = FakeGitHub([opened, draft, closed, historic])
        self.assertEqual(self.plan(gh)['work'], 'false')
        self.assertFalse(any(method != 'GET' for _, method, _ in gh.calls))

    def test_allocate_counts_draft_canonical_names_and_tags_without_pushing_tag(self):
        old = release(11, schema=1)
        old['tag_name'] = 'untagged-opaque'
        gh = FakeGitHub([pr()], [old])
        gh.tags = [{'name': 'v0.2.4-hao.10'}]
        result = self.plan(gh)
        self.assertEqual(result['tag'], 'v0.2.4-hao.12')
        self.assertEqual(result['sha'], MERGE)
        self.assertTrue(gh.releases[-1]['draft'])
        self.assertFalse(gh.releases[-1]['prerelease'])
        self.assertFalse(any(path == 'git/refs' for path, _, _ in gh.calls))

    def test_new_base_starts_at_one_and_kln_mismatch_fails(self):
        upstream = pr()
        upstream['head']['ref'] = 'sync/kln-release/v0.2.5-klno.1'
        gh = FakeGitHub([upstream])
        gh.tags = [{'name': 'v0.2.4-hao.99'}]
        with self.assertRaisesRegex(RuntimeError, 'KlN release base'):
            self.plan(gh)
        gh.version = '0.2.5-klno.1'
        self.assertEqual(self.plan(gh)['tag'], 'v0.2.5-hao.1')

    def test_retry_and_duplicate_event_keep_one_version(self):
        gh = FakeGitHub([pr()])
        first = self.plan(gh)
        second = self.plan(gh)
        self.assertEqual(first['tag'], second['tag'])
        self.assertEqual(len(gh.releases), 1)

    def test_same_tree_different_sha_is_not_reused(self):
        gh = FakeGitHub([pr()], [release(sha=HEAD)])
        with self.assertRaisesRegex(RuntimeError, 'identity changed'):
            self.plan(gh)

    def test_v1_candidate_is_not_resumed(self):
        gh = FakeGitHub([pr()], [release(schema=1, sha=HEAD)])
        self.assertEqual(self.plan(gh)['tag'], 'v0.2.4-hao.11')

    def test_ready_release_is_not_allocated_again(self):
        self.assertEqual(self.plan(FakeGitHub([pr()], [release(ready=True)]))['work'], 'false')

    def test_release_metadata_accepts_lf_crlf_and_mixed_newlines(self):
        for opening, closing in [('\n', '\n'), ('\r\n', '\r\n'), ('\r\n', '\n')]:
            with self.subTest(opening=opening, closing=closing):
                item = release(ready=True, stable=True)
                item['body'] = item['body'].replace('sub2api-release-v1\n',
                                                    'sub2api-release-v1' + opening).replace('\n-->', closing + '-->')
                gh = FakeGitHub([pr()], [item])
                self.assertEqual(self.plan(gh)['work'], 'false')
                self.assertFalse(any(method != 'GET' for _, method, _ in gh.calls))

    def test_malformed_managed_marker_is_not_treated_as_unmanaged(self):
        item = release(stable=True)
        item['body'] = item['body'].replace('\n-->', '')
        gh = FakeGitHub([pr()])
        gh.releases = [item]
        with self.assertRaisesRegex(RuntimeError, 'Invalid published release'):
            self.plan(gh)
        self.assertFalse(any(method != 'GET' for _, method, _ in gh.calls))

    def test_duplicate_completed_releases_allow_next_pr_without_rewriting_history(self):
        items = [release(n, ready=True, stable=True) for n in (23, 24, 25)]
        for item in items[:2]:
            item['body'] = item['body'].replace('\n', '\r\n')
        original = copy.deepcopy(items)
        next_pr = dict(pr(18), merged_at='2026-09-13T10:00:00Z')
        gh = FakeGitHub([next_pr, pr()], items)
        self.assertEqual(self.plan(gh)['tag'], 'v0.2.4-hao.26')
        self.assertEqual(state_of(gh.releases[-1])['pr'], 18)
        self.assertEqual(gh.releases[:3], original)
        self.assertEqual(self.plan(gh)['tag'], 'v0.2.4-hao.26')
        self.assertEqual(len(gh.releases), 4)

    def test_duplicate_releases_still_reject_changed_source(self):
        for changed in [dict(sha=HEAD), dict(tree=OTHER)]:
            with self.subTest(changed=changed):
                gh = FakeGitHub([pr()], [release(23, ready=True), release(24, **changed)])
                with self.assertRaisesRegex(RuntimeError, 'identity changed'):
                    self.plan(gh)
                self.assertFalse(any(method != 'GET' for _, method, _ in gh.calls))

    def test_duplicate_unfinished_release_keeps_its_frozen_bundle(self):
        gh = FakeGitHub([pr()], [release(23, ready=True, stable=True),
                                release(24, bundle_run=99, files={'image.tar': 'f' * 64})])
        gh.artifacts = [{'name': 'v0.2.4-hao.24', 'expired': False}]
        result = self.plan(gh)
        self.assertEqual(result['tag'], 'v0.2.4-hao.24')
        self.assertEqual(result['bundle_run'], '99')
        self.assertEqual(result['build'], 'false')
        self.assertEqual(len(gh.releases), 2)
        gh.artifacts = []
        with self.assertRaisesRegex(RuntimeError, 'Frozen artifact unavailable'):
            self.plan(gh)

    @patch.object(Reconciler, 'copy_image')
    def test_crlf_duplicate_releases_use_merge_time_for_latest(self, copy_image):
        older = [release(n, ready=True, stable=True, image_digest=DIGEST) for n in (23, 24, 25)]
        for item in older[:2]:
            item['body'] = item['body'].replace('\n', '\r\n')
            item['published_at'] = '2026-09-14T10:00:00Z'
        newer_pr = dict(pr(6), merged_at='2026-09-13T10:00:00Z')
        newer = release(22, ready=True, stable=True, pr=6, image_digest=DIGEST,
                        merged_at=newer_pr['merged_at'])
        gh = FakeGitHub([pr(), newer_pr], older + [newer])
        with patch.object(Reconciler, 'verify_platforms'), patch.object(Reconciler, 'ensure_version_image'):
            Reconciler(gh).promote()
        self.assertEqual([path for path, _, data in gh.calls if data == {'make_latest': 'true'}],
                         ['releases/22'])

    def test_orphan_successful_build_is_reused_before_any_publish(self):
        gh = FakeGitHub([pr()], [release(build_run=99)])
        gh.artifacts = [{'name': 'v0.2.4-hao.10', 'expired': False}]
        result = self.plan(gh)
        self.assertEqual(result['build'], 'false')
        self.assertEqual(result['bundle_run'], '99')

    def test_expired_frozen_bundle_blocks_without_renumbering(self):
        gh = FakeGitHub([pr()], [release(bundle_run=99, files={'image.tar': 'f' * 64})])
        with self.assertRaisesRegex(RuntimeError, 'Frozen artifact unavailable'):
            self.plan(gh)
        self.assertEqual(len(gh.releases), 1)

    def test_unpublished_expired_build_rebuilds_same_version(self):
        gh = FakeGitHub([pr()], [release(build_run=99)])
        self.assertEqual(self.plan(gh)['tag'], 'v0.2.4-hao.10')
        self.assertEqual(state_of(gh.releases[0])['build_run'], 123)

    def test_allocate_oldest_first_then_rotate_failures(self):
        older = dict(pr(6), merged_at='2026-09-12T08:00:00Z')
        gh = FakeGitHub([pr(), older])
        self.plan(gh)
        self.assertEqual(state_of(gh.releases[-1])['pr'], 6)
        self.plan(gh)
        self.assertEqual(state_of(gh.releases[-1])['pr'], 5)

    def test_external_deleted_head_repo_still_releases_merged_result(self):
        external = pr()
        external['head']['repo'] = None
        self.assertEqual(self.plan(FakeGitHub([external]))['sha'], MERGE)

    def empty_release(self):
        item = release(18, stable=True)
        item['body'] = 'Manually created release'
        return item

    def test_recovery_binds_existing_tag_to_merged_pr_and_resumes_after_boundary(self):
        gh = FakeGitHub([pr()], [self.empty_release()])
        with patch.dict(os.environ, RECOVER_TAG='v0.2.4-hao.18'):
            self.assertEqual(self.plan(gh)['sha'], MERGE)
        state = state_of(gh.releases[0])
        self.assertTrue(state['recovery'])
        with patch.dict(os.environ, AUTOMATION_SINCE='2027-01-01T00:00:00Z'):
            self.assertEqual(self.plan(gh)['tag'], 'v0.2.4-hao.18')
        self.assertEqual(len(gh.releases), 1)
        self.assertFalse(any(path == 'git/refs' for path, _, _ in gh.calls))

    def test_recovery_rejects_existing_assets_or_wrong_commit(self):
        for assets, sha in [([{'name': 'existing'}], MERGE), ([], HEAD), ([], None)]:
            gh = FakeGitHub([pr()], [self.empty_release()])
            gh.assets, gh.tag_sha = assets, sha
            with self.subTest(assets=assets, sha=sha), patch.dict(os.environ, RECOVER_TAG='v0.2.4-hao.18'):
                with self.assertRaises(RuntimeError):
                    self.plan(gh)
                self.assertIsNone(state_of(gh.releases[0]))

    def bundle(self):
        directory = Path(self.temp.name)
        files = {'image.tar': b'opaque OCI image'}
        for platform, ext in [('linux_amd64', 'tar.gz'), ('linux_arm64', 'tar.gz'),
                              ('darwin_amd64', 'tar.gz'), ('darwin_arm64', 'tar.gz'), ('windows_amd64', 'zip')]:
            files[f'sub2api_0.2.4-hao.10_{platform}.{ext}'] = platform.encode()
        files['checksums.txt'] = ''.join(f'{hashlib.sha256(c).hexdigest()}  {n}\n'
                                        for n, c in sorted(files.items()) if n != 'image.tar').encode()
        for name, contents in files.items():
            (directory / name).write_bytes(contents)
        (directory / 'bundle.json').write_text(json.dumps({
            'tag': 'v0.2.4-hao.10', 'sha': MERGE, 'simple': False,
            'files': {n: hashlib.sha256(c).hexdigest() for n, c in files.items()}}))
        env = patch.dict(os.environ, RELEASE_TAG='v0.2.4-hao.10', BUNDLE_DIR=str(directory))
        env.start()
        self.addCleanup(env.stop)

    @patch.object(Reconciler, 'verify_platforms')
    @patch.object(Reconciler, 'inspect_image', return_value=DIGEST)
    @patch.object(Reconciler, 'inspect_reference')
    @patch.object(Reconciler, 'copy_image')
    def test_upload_interruption_reuses_bundle_and_leaves_draft(self, copy_image, inspect_ref, inspect_image, platforms):
        self.bundle()
        gh = FakeGitHub([pr()], [release()])
        inspect_ref.side_effect = [DIGEST, None]
        with patch.object(gh, 'upload', side_effect=RuntimeError('interrupted')):
            with self.assertRaisesRegex(RuntimeError, 'interrupted'):
                Reconciler(gh).publish()
        self.assertEqual(state_of(gh.releases[0])['bundle_run'], 123)
        inspect_ref.side_effect = [DIGEST, DIGEST]
        Reconciler(gh).publish()
        self.assertEqual(copy_image.call_count, 1)
        self.assertEqual(len(gh.assets), 6)
        self.assertTrue(state_of(gh.releases[0])['ready'])
        self.assertTrue(gh.releases[0]['draft'])

    def test_bundle_corruption_or_incomplete_platforms_prevents_upload(self):
        self.bundle()
        gh = FakeGitHub([pr()], [release()])
        (Path(self.temp.name) / 'image.tar').write_bytes(b'changed')
        with self.assertRaisesRegex(RuntimeError, 'integrity failure'):
            Reconciler(gh).publish()
        self.assertFalse(gh.assets)
        manifest = Path(self.temp.name) / 'bundle.json'
        data = json.loads(manifest.read_text())
        data['files'].pop('sub2api_0.2.4-hao.10_linux_arm64.tar.gz')
        manifest.write_text(json.dumps(data))
        with self.assertRaisesRegex(RuntimeError, 'Incomplete bundle'):
            Reconciler(gh).publish()

    @patch('reconcile.subprocess.run')
    def test_single_arch_image_is_rejected(self, run):
        run.return_value.stdout = json.dumps({'manifests': [{'platform': {'os': 'linux', 'architecture': 'amd64'}}]})
        with self.assertRaisesRegex(RuntimeError, 'linux/arm64'):
            Reconciler.verify_platforms('oci-archive:test')

    @patch.object(Reconciler, 'inspect_reference', return_value='sha256:' + 'f' * 64)
    @patch.object(Reconciler, 'copy_image')
    def test_existing_version_image_is_never_overwritten(self, copy_image, inspect_ref):
        with self.assertRaisesRegex(RuntimeError, 'refusing overwrite'):
            Reconciler(FakeGitHub()).ensure_version_image('source', 'target', DIGEST)
        copy_image.assert_not_called()

    @patch.object(Reconciler, 'copy_image')
    def test_stable_retry_updates_channels_without_main_write(self, copy_image):
        gh = FakeGitHub([pr()], [release(ready=True, stable=True, image_digest=DIGEST)])
        with patch.object(Reconciler, 'verify_platforms'), patch.object(Reconciler, 'ensure_version_image'):
            Reconciler(gh).promote()
        self.assertEqual(copy_image.call_count, 3)
        self.assertEqual(gh.version, '0.2.4-hao.8')
        self.assertTrue(any(data == {'make_latest': 'true'} for _, _, data in gh.calls))
        self.assertFalse(any(method == 'PUT' for _, method, _ in gh.calls))

    @patch.object(Reconciler, 'copy_image', side_effect=RuntimeError('network'))
    def test_channel_failure_keeps_release_draft(self, copy_image):
        gh = FakeGitHub([pr()], [release(ready=True, image_digest=DIGEST)])
        with patch.object(Reconciler, 'verify_platforms'), patch.object(Reconciler, 'ensure_version_image'):
            with self.assertRaisesRegex(RuntimeError, 'network'):
                Reconciler(gh).promote()
        self.assertTrue(gh.releases[0]['draft'])

    @patch.object(Reconciler, 'ensure_version_image')
    @patch.object(Reconciler, 'copy_image')
    def test_old_merge_gets_hub_version_but_cannot_roll_back_latest(self, copy_image, version_image):
        old_pr = dict(pr(6), merged_at='2026-09-12T09:00:00Z')
        older = release(10, ready=True, pr=6, image_digest='sha256:' + '1' * 64, merged_at=old_pr['merged_at'])
        newer = release(11, ready=True, stable=True, image_digest=DIGEST)
        gh = FakeGitHub([pr(), old_pr], [older, newer])
        with patch.dict(os.environ, DOCKERHUB_USERNAME='owner'), patch.object(Reconciler, 'verify_platforms'):
            Reconciler(gh).promote()
        self.assertEqual(version_image.call_count, 4)
        self.assertFalse(older['draft'])
        for call in copy_image.call_args_list:
            self.assertIn(DIGEST, call.args[0])
        latest_calls = [path for path, _, data in gh.calls if data == {'make_latest': 'true'}]
        self.assertEqual(latest_calls, ['releases/11'])

    def test_moved_tag_blocks_promotion(self):
        gh = FakeGitHub([pr()], [release(ready=True, image_digest=DIGEST)])
        gh.tag_sha = OTHER
        with self.assertRaisesRegex(RuntimeError, 'Tag moved'):
            Reconciler(gh).promote()

    def test_native_tag_denial_cannot_trigger_old_workflows(self):
        gh = FakeGitHub()
        reconciler = Reconciler(gh)
        def api(path, method='GET', data=None, **kwargs):
            if path == 'git/refs':
                raise GitHubError(403, method, path)
            return [{'name': 'workflows', 'sha': TREE if HEAD in path else OTHER}]
        with patch.object(gh, 'api', side_effect=api), patch.dict(os.environ, TRUSTED_SHA=MERGE):
            with self.assertRaisesRegex(RuntimeError, 'candidate workflows differ'):
                reconciler.create_tag('v0.2.4-hao.10', HEAD)

    def test_legacy_alias_requires_real_tag_and_bad_draft_is_quarantined(self):
        item = release(schema=1)
        item.update(tag_name='untagged-wrong')
        gh = FakeGitHub([], [item])
        gh.tag_sha = OTHER
        reconciler = Reconciler(gh)
        self.assertEqual(reconciler.managed(), [])
        self.assertIn(item['id'], reconciler.quarantined)

    def test_new_draft_alias_without_public_tag_resumes(self):
        item = release()
        item['tag_name'] = 'untagged-new'
        gh = FakeGitHub([pr()], [item])
        gh.tag_sha = None
        self.assertEqual(self.plan(gh)['tag'], 'v0.2.4-hao.10')
        self.assertEqual(len(gh.releases), 1)

    def test_bootstrap_recovery_requires_pinned_sha_and_pr(self):
        gh = FakeGitHub([pr(11)], [self.empty_release()])
        gh.repo = 'haohex/sub2api'
        with self.assertRaisesRegex(RuntimeError, 'audited source'):
            self.plan(gh)
        audited = '0f4136790c2bbb70b44e6ea48d9a5312f7efc702'
        gh.tag_sha = audited
        gh.prs[0]['merge_commit_sha'] = audited
        with patch.dict(os.environ, AUTOMATION_SINCE='2027-01-01T00:00:00Z'):
            result = self.plan(gh)
        self.assertEqual(result['sha'], audited)
        self.assertEqual(result['tag'], 'v0.2.4-hao.18')

    @patch.object(Reconciler, 'verify_platforms')
    @patch.object(Reconciler, 'ensure_version_image')
    @patch.object(Reconciler, 'copy_image')
    def test_deleted_asset_blocks_formal_release(self, copy_image, ensure_image, platforms):
        gh = FakeGitHub([pr()], [release(ready=True, image_digest=DIGEST)])
        gh.assets.pop()
        with self.assertRaisesRegex(RuntimeError, 'asset missing or changed'):
            Reconciler(gh).promote()
        copy_image.assert_not_called()
        self.assertTrue(gh.releases[0]['draft'])

    def test_failed_frozen_retry_rotates_instead_of_starving_other_retry(self):
        item = release(bundle_run=99, files={'image.tar': 'f' * 64}, attempted_at='2026-09-12T08:00:00Z')
        other = release(11, pr=6, attempted_at='2026-09-12T09:00:00Z')
        gh = FakeGitHub([pr(), pr(6)], [item, other])
        with self.assertRaisesRegex(RuntimeError, 'Frozen artifact unavailable'):
            self.plan(gh)
        self.assertEqual(self.plan(gh)['tag'], 'v0.2.4-hao.11')

    @patch.object(Reconciler, 'verify_platforms')
    @patch.object(Reconciler, 'inspect_image', return_value=DIGEST)
    @patch.object(Reconciler, 'inspect_reference', side_effect=[DIGEST, None])
    @patch.object(Reconciler, 'copy_image')
    def test_successful_publish_creates_missing_tag_with_native_token(self, copy_image, inspect_ref, inspect_image, platforms):
        self.bundle()
        gh = FakeGitHub([pr()], [release()])
        gh.tag_sha = None
        Reconciler(gh).publish()
        self.assertEqual(gh.tag_sha, MERGE)
        self.assertEqual(gh.tag_token, 'TAG_TOKEN')
        self.assertTrue(gh.releases[0]['draft'])

    def test_false_checksum_manifest_is_rejected_even_if_it_hashes_correctly(self):
        self.bundle()
        root = Path(self.temp.name)
        (root / 'checksums.txt').write_text('wrong manifest')
        manifest = json.loads((root / 'bundle.json').read_text())
        manifest['files']['checksums.txt'] = hashlib.sha256(b'wrong manifest').hexdigest()
        (root / 'bundle.json').write_text(json.dumps(manifest))
        with self.assertRaisesRegex(RuntimeError, 'Checksum manifest'):
            Reconciler(FakeGitHub([pr()], [release()])).publish()

    @patch('reconcile.urlopen')
    def test_repository_api_and_asset_upload_use_canonical_endpoints(self, urlopen):
        urlopen.return_value.__enter__.return_value.read.return_value = b'{"id":1}'
        path = Path(self.temp.name) / 'checksums.txt'
        path.write_text('checksum')
        with patch.dict(os.environ, GITHUB_REPOSITORY='owner/repo', GH_TOKEN='test-token'):
            GitHub().api('')
            self.assertEqual(urlopen.call_args.args[0].full_url, 'https://api.github.com/repos/owner/repo')
            GitHub().upload(387550494, path)
            self.assertEqual(urlopen.call_args.args[0].full_url,
                             'https://uploads.github.com/repos/owner/repo/releases/387550494/assets?name=checksums.txt')


if __name__ == '__main__':
    unittest.main()
