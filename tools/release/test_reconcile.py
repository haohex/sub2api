import base64
import copy
import hashlib
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from reconcile import GitHub, GitHubError, Reconciler, body_for, state_of, version_key

HEAD, MERGE, TREE, OTHER = 'a' * 40, 'b' * 40, 'c' * 40, 'd' * 40
DIGEST = 'sha256:' + 'e' * 64


def pr(number=5, merged=False, tree=TREE):
    return {'number': number, 'state': 'closed' if merged else 'open', 'draft': False,
            'merged': merged, 'merged_at': '2026-09-12T10:00:00Z' if merged else None,
            'merge_commit_sha': MERGE, 'base': {'ref': 'main'},
            'head': {'sha': HEAD, 'repo': {'full_name': 'owner/repo'}}}


def release(number=10, ready=False, stable=False, **extra):
    state = dict(pr=5, sha=HEAD, tree=TREE, simple=False, ready=ready)
    state.update(extra)
    return {'id': number, 'tag_name': f'v0.2.4-hao.{number}', 'body': body_for(state),
            'target_commitish': HEAD, 'draft': not ready, 'prerelease': not stable,
            'published_at': '2026-09-12T09:00:00Z'}


class FakeGitHub:
    repo = 'owner/repo'

    def __init__(self, prs=None, releases=None):
        self.prs = prs or []
        self.releases = releases or []
        self.tags = []
        self.tree = TREE
        self.artifacts = []
        self.assets = []
        self.calls = []
        self.version = '0.2.4-hao.8'
        self.tag_sha = HEAD

    def pages(self, path, field=None):
        if path == 'releases':
            return self.releases
        if path.startswith('pulls?'):
            return self.prs
        if path == 'tags':
            return self.tags
        if path.startswith('actions/'):
            return self.artifacts
        if path.endswith('/assets'):
            return self.assets
        raise AssertionError(path)

    def api(self, path, method='GET', data=None, missing=False, token_env='GH_TOKEN'):
        self.calls.append((path, method, copy.deepcopy(data)))
        if path == '':
            return {'default_branch': 'main'}
        if path.startswith('pulls/'):
            return next(p for p in self.prs if str(p['number']) == path.split('/')[1])
        if path.startswith('commits/'):
            return {'commit': {'tree': {'sha': self.tree}}}
        if path.startswith('git/ref/'):
            return {'object': {'type': 'commit', 'sha': self.tag_sha}}
        if path == 'git/refs' and method == 'POST':
            self.tags.append({'name': data['ref'].removeprefix('refs/tags/')})
            self.tag_token = token_env
            return {'object': {'type': 'commit', 'sha': data['sha']}}
        if path.startswith('contents/'):
            if method == 'PUT':
                self.version = base64.b64decode(data['content']).decode().strip()
            return {'content': base64.b64encode(self.version.encode()).decode(), 'sha': OTHER}
        if path == 'releases' and method == 'POST':
            created = dict(data, id=100, published_at=None)
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
                         'SIMPLE_RELEASE': 'false', 'DOCKERHUB_USERNAME': '',
                         'TELEGRAM_BOT_TOKEN': '', 'TELEGRAM_CHAT_ID': ''})
        env.start()
        self.addCleanup(env.stop)

    def plan(self, gh):
        Reconciler(gh).plan()
        return dict(line.split('=', 1) for line in self.output.read_text().splitlines())

    def test_version_order_is_numeric(self):
        self.assertGreater(version_key('v0.2.4-hao.10'), version_key('v0.2.4-hao.9'))
        self.assertIsNone(version_key('v0.2.4;bad'))

    @patch('reconcile.urlopen')
    def test_repository_endpoint_has_no_trailing_slash(self, urlopen):
        urlopen.return_value.__enter__.return_value.read.return_value = b'{"default_branch":"main"}'
        with patch.dict(os.environ, GITHUB_REPOSITORY='owner/repo', GH_TOKEN='test-token'):
            self.assertEqual(GitHub().api('')['default_branch'], 'main')
        self.assertEqual(urlopen.call_args.args[0].full_url, 'https://api.github.com/repos/owner/repo')

    def test_allocate_accounts_for_drafts_and_remote_tags(self):
        gh = FakeGitHub([pr()], [release(11, abandoned='expired')])
        gh.tags = [{'name': 'v0.2.4-hao.12'}]
        result = self.plan(gh)
        self.assertEqual(result['tag'], 'v0.2.4-hao.13')
        self.assertEqual(result['build'], 'true')
        self.assertTrue(gh.releases[-1]['draft'])
        self.assertTrue(gh.releases[-1]['prerelease'])
        self.assertEqual(gh.tag_token, 'TAG_TOKEN')

    def test_native_tag_denial_never_falls_back_to_old_candidate_workflows(self):
        gh = FakeGitHub()
        reconciler = Reconciler(gh)
        def api(path, method='GET', data=None, **kwargs):
            if path == 'git/refs':
                self.assertEqual(kwargs.get('token_env'), 'TAG_TOKEN')
                raise GitHubError(403, method, path)
            return [{'name': 'workflows', 'sha': TREE if HEAD in path else OTHER}]
        with patch.object(gh, 'api', side_effect=api), patch.dict(os.environ, TRUSTED_SHA=MERGE):
            with self.assertRaisesRegex(RuntimeError, 'candidate workflows differ'):
                reconciler.create_tag('v0.2.4-hao.10', HEAD)

    def test_squash_same_tree_reuses_candidate(self):
        gh = FakeGitHub([pr(merged=True)], [release(ready=True)])
        self.assertEqual(self.plan(gh)['work'], 'false')

    def test_changed_merge_tree_allocates_new_immutable_version(self):
        gh = FakeGitHub([pr(merged=True)], [release(ready=True)])
        gh.tree = OTHER
        result = self.plan(gh)
        self.assertEqual(result['sha'], MERGE)
        self.assertEqual(result['tag'], 'v0.2.4-hao.11')
        self.assertEqual(state_of(gh.releases[0])['sha'], HEAD)

    def test_frozen_bundle_resumes_without_rebuild(self):
        gh = FakeGitHub([pr()], [release(bundle_run=123)])
        gh.artifacts = [{'name': 'v0.2.4-hao.10', 'expired': False}]
        result = self.plan(gh)
        self.assertEqual(result['build'], 'false')
        self.assertEqual(result['bundle_run'], '123')

    def test_expired_bundle_allocates_new_version(self):
        gh = FakeGitHub([pr()], [release(bundle_run=123)])
        result = self.plan(gh)
        self.assertEqual(result['tag'], 'v0.2.4-hao.11')
        self.assertTrue(state_of(gh.releases[0])['abandoned'])

    def test_closed_unmerged_and_historical_merged_prs_are_ignored(self):
        closed = pr()
        closed['state'] = 'closed'
        historic = pr(6, merged=True)
        historic['merged_at'] = '2026-09-11T00:00:00Z'
        self.assertEqual(self.plan(FakeGitHub([closed, historic]))['work'], 'false')

    def test_new_commit_invalidates_old_candidate(self):
        updated = pr()
        updated['head']['sha'] = MERGE
        gh = FakeGitHub([updated], [release(ready=True)])
        gh.tree = OTHER
        self.assertEqual(self.plan(gh)['sha'], MERGE)

    def test_failed_pr_does_not_starve_other_pr(self):
        gh = FakeGitHub([pr(), pr(6)], [release(attempted_at='2026-09-12T09:00:00Z')])
        self.plan(gh)
        self.assertEqual(state_of(gh.releases[-1])['pr'], 6)

    def test_external_pr_only_builds_after_merge(self):
        external = pr()
        external['head']['repo']['full_name'] = 'external/repo'
        self.assertEqual(self.plan(FakeGitHub([external]))['work'], 'false')
        external.update(merged=True, state='closed', merged_at='2026-09-12T10:00:00Z')
        self.assertEqual(self.plan(FakeGitHub([external]))['sha'], MERGE)

    def test_open_pr_never_promotes(self):
        gh = FakeGitHub([pr()], [release(ready=True, image_digest=DIGEST)])
        Reconciler(gh).promote()
        self.assertTrue(gh.releases[0]['prerelease'])

    def test_stale_candidate_and_moved_tag_never_promote(self):
        gh = FakeGitHub([pr(merged=True)], [release(ready=True, image_digest=DIGEST)])
        gh.tree = OTHER
        Reconciler(gh).promote()
        self.assertTrue(gh.releases[0]['prerelease'])
        gh.tree, gh.tag_sha = TREE, OTHER
        with self.assertRaisesRegex(RuntimeError, 'Tag moved'):
            Reconciler(gh).promote()

    @patch.object(Reconciler, 'copy_image')
    def test_stable_metadata_does_not_skip_incomplete_channel_sync(self, copy_image):
        item = release(ready=True, stable=True, image_digest=DIGEST,
                       merged_sha=MERGE, merged_at='2026-09-12T10:00:00Z')
        gh = FakeGitHub([pr(merged=True)], [item])
        Reconciler(gh).promote()
        self.assertEqual(copy_image.call_count, 3)
        self.assertEqual(gh.version, '0.2.4-hao.10')
        self.assertTrue(any(data == {'make_latest': 'true'} for _, _, data in gh.calls))

    @patch.object(Reconciler, 'copy_image')
    def test_delayed_old_merge_does_not_overwrite_newer_merge(self, copy_image):
        older = release(20, ready=True, stable=True, image_digest='sha256:' + '1' * 64,
                        merged_at='2026-09-12T09:00:00Z')
        newer = release(19, ready=True, stable=True, image_digest=DIGEST,
                        merged_at='2026-09-12T10:00:00Z')
        gh = FakeGitHub([pr(merged=True)], [older, newer])
        Reconciler(gh).promote()
        self.assertEqual(gh.version, '0.2.4-hao.19')
        for call in copy_image.call_args_list:
            self.assertIn(DIGEST, call.args[0])

    @patch.object(Reconciler, 'copy_image', side_effect=RuntimeError('network'))
    def test_channel_failure_does_not_sync_version(self, copy_image):
        gh = FakeGitHub([pr(merged=True)], [release(ready=True, image_digest=DIGEST)])
        with self.assertRaisesRegex(RuntimeError, 'network'):
            Reconciler(gh).promote()
        self.assertEqual(gh.version, '0.2.4-hao.8')

    def test_bundle_validation_rejects_path_escape_before_upload(self):
        gh = FakeGitHub([pr()], [release()])
        manifest = {'tag': 'v0.2.4-hao.10', 'sha': HEAD, 'simple': False,
                    'files': {'image.tar': 'f' * 64, 'checksums.txt': 'f' * 64, '../escape': 'f' * 64}}
        (Path(self.temp.name) / 'bundle.json').write_text(json.dumps(manifest))
        with patch.dict(os.environ, RELEASE_TAG='v0.2.4-hao.10', BUNDLE_DIR=self.temp.name):
            with self.assertRaises((RuntimeError, FileNotFoundError)):
                Reconciler(gh).publish()
        self.assertFalse(any(method == 'PATCH' for _, method, _ in gh.calls))

    def bundle(self):
        directory = Path(self.temp.name)
        files = {'image.tar': b'opaque OCI image', 'checksums.txt': b''}
        for name, contents in files.items():
            (directory / name).write_bytes(contents)
        (directory / 'bundle.json').write_text(json.dumps({
            'tag': 'v0.2.4-hao.10', 'sha': HEAD, 'simple': True,
            'files': {n: hashlib.sha256(c).hexdigest() for n, c in files.items()}}))
        env = patch.dict(os.environ, RELEASE_TAG='v0.2.4-hao.10',
                         BUNDLE_DIR=str(directory), GITHUB_RUN_ID='123')
        env.start()
        self.addCleanup(env.stop)

    @patch('reconcile.subprocess.run')
    @patch.object(Reconciler, 'inspect_image', return_value=DIGEST)
    @patch.object(Reconciler, 'inspect_reference')
    @patch.object(Reconciler, 'copy_image')
    def test_upload_interruption_resumes_frozen_bundle_without_image_overwrite(
            self, copy_image, inspect_ref, inspect_image, upload):
        self.bundle()
        gh = FakeGitHub([pr()], [release(simple=True)])
        inspect_ref.side_effect = [DIGEST, None]
        upload.side_effect = RuntimeError('upload interrupted')
        with self.assertRaisesRegex(RuntimeError, 'upload interrupted'):
            Reconciler(gh).publish()
        state = state_of(gh.releases[0])
        self.assertEqual(state['image_digest'], DIGEST)
        self.assertEqual(state['bundle_run'], 123)
        self.assertFalse(state['ready'])
        inspect_ref.side_effect = [DIGEST, DIGEST]
        upload.side_effect = None
        Reconciler(gh).publish()
        self.assertEqual(copy_image.call_count, 1)
        self.assertTrue(state_of(gh.releases[0])['ready'])
        self.assertTrue(gh.releases[0]['prerelease'])
        self.assertFalse(gh.releases[0]['draft'])

    @patch.object(Reconciler, 'inspect_reference', side_effect=[DIGEST, 'sha256:' + 'f' * 64])
    @patch.object(Reconciler, 'copy_image')
    def test_existing_version_image_is_never_overwritten(self, copy_image, inspect_ref):
        self.bundle()
        gh = FakeGitHub([pr()], [release(simple=True)])
        with self.assertRaisesRegex(RuntimeError, 'refusing overwrite'):
            Reconciler(gh).publish()
        copy_image.assert_not_called()

    @patch.object(Reconciler, 'copy_image')
    def test_newer_merge_with_lower_reserved_version_is_renumbered(self, copy_image):
        candidate = release(10, ready=True, image_digest=DIGEST)
        previous = release(11, ready=True, stable=True, image_digest=DIGEST,
                           merged_at='2026-09-12T09:00:00Z')
        gh = FakeGitHub([pr(merged=True)], [candidate, previous])
        Reconciler(gh).promote()
        self.assertTrue(state_of(candidate)['abandoned'])
        self.assertTrue(candidate['prerelease'])

    @patch.object(Reconciler, 'copy_image')
    def test_older_merge_finishing_late_remains_prerelease(self, copy_image):
        old_pr = pr(merged=True)
        old_pr['merged_at'] = '2026-09-12T09:00:00Z'
        candidate = release(12, ready=True, image_digest=DIGEST)
        newer = release(11, ready=True, stable=True, image_digest=DIGEST,
                        merged_at='2026-09-12T10:00:00Z')
        gh = FakeGitHub([old_pr], [candidate, newer])
        Reconciler(gh).promote()
        self.assertTrue(candidate['prerelease'])
        self.assertEqual(gh.version, '0.2.4-hao.11')


if __name__ == '__main__':
    unittest.main()
