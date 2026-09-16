#!/usr/bin/env python3
"""Reconcile PR releases. Only run from the trusted default branch, never PR code.

Draft Release bodies hold durable state; Actions artifacts freeze build outputs before
any upload. A repository-wide workflow lock serializes allocation and channel updates.
"""
import base64
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
from urllib.error import HTTPError
from urllib.parse import quote
from urllib.request import Request, urlopen

TAG = re.compile(r'^v(\d+)\.(\d+)\.(\d+)-hao\.(\d+)$')
SHA = re.compile(r'^[a-f0-9]{40}$')
MARKER = re.compile(r'<!-- sub2api-release-v1\r?\n(.*?)\r?\n-->', re.S)


class GitHubError(RuntimeError):
    def __init__(self, status, method, path):
        self.status = status
        super().__init__(f'GitHub {method} {path}: HTTP {status}')


def version_key(tag):
    match = TAG.fullmatch(tag)
    return tuple(map(int, match.groups())) if match else None


def source_base(version, branch):
    match = re.fullmatch(r'(\d+\.\d+\.\d+)(?:-[0-9A-Za-z.-]+)?', version.strip().removeprefix('v'))
    if not match:
        raise RuntimeError('Cannot determine release base version')
    base = match[1]
    if branch.startswith('sync/kln-release/'):
        tag = branch.removeprefix('sync/kln-release/')
        upstream = re.fullmatch(r'v(\d+\.\d+\.\d+)-klno\.\d+', tag)
        if not upstream or upstream[1] != base:
            raise RuntimeError('Merged VERSION differs from the KlN release base version')
    return base


def expected_files(tag):
    return {'image.tar', 'checksums.txt'} | {
        f'sub2api_{tag[1:]}_{platform}.{extension}' for platform, extension in [
            ('linux_amd64', 'tar.gz'), ('linux_arm64', 'tar.gz'),
            ('darwin_amd64', 'tar.gz'), ('darwin_arm64', 'tar.gz'), ('windows_amd64', 'zip')]}


def digest_file(path):
    with path.open('rb') as handle:
        return hashlib.file_digest(handle, 'sha256').hexdigest()


def state_of(release):
    match = MARKER.search(release.get('body') or '')
    if not match:
        if '<!-- sub2api-release-v1' in (release.get('body') or ''):
            raise ValueError('Invalid managed release metadata marker')
        return None
    state = json.loads(match[1])
    if not isinstance(state, dict):
        raise ValueError('Invalid managed release metadata')
    tag = state.get('tag') or release['tag_name']
    if not isinstance(tag, str):
        raise ValueError('Invalid canonical release tag')
    if release['draft'] and tag.startswith('untagged-'):
        # Migrate drafts created before the canonical tag was persisted. The
        # generated title is only a hint; managed() must verify the real tag SHA.
        name = release.get('name', '')
        tag = 'v' + name.removeprefix('Sub2API ') if name.startswith('Sub2API ') else ''
    if (not version_key(tag) or not SHA.fullmatch(state['sha'])
            or not SHA.fullmatch(state['tree']) or not isinstance(state['pr'], int)
            or state['pr'] <= 0 or not isinstance(state['simple'], bool)):
        raise ValueError('Invalid managed release metadata')
    if release['tag_name'] != tag and not (release['draft'] and release['tag_name'].startswith('untagged-')):
        raise ValueError('Release tag differs from its frozen identity')
    state['tag'] = tag
    return state


def body_for(state):
    # Never interpolate PR text into shell commands or executable files.
    return (f'PR #{state["pr"]} · commit `{state["sha"]}`\n\n'
            f'构建模式：{"linux/amd64 镜像" if state["simple"] else "完整安装包与多架构镜像"}。'
            '\n合并后发布；完整产物与镜像验证通过后正式发布。\n\n'
            '<!-- sub2api-release-v1\n' + json.dumps(state, sort_keys=True) + '\n-->')


class GitHub:
    def __init__(self):
        self.repo = os.environ['GITHUB_REPOSITORY']
        self.root = f'https://api.github.com/repos/{self.repo}'

    def api(self, path, method='GET', data=None, missing=False, token_env='GH_TOKEN'):
        request = Request(self.root + ('/' + path if path else ''), method=method,
                          data=None if data is None else json.dumps(data).encode(), headers={
                              'Authorization': 'Bearer ' + os.environ[token_env],
                              'Accept': 'application/vnd.github+json',
                              'Content-Type': 'application/json',
                              'X-GitHub-Api-Version': '2022-11-28'})
        try:
            with urlopen(request, timeout=60) as response:
                content = response.read()
                return json.loads(content) if content else None
        except HTTPError as error:
            if missing and error.code == 404:
                return None
            raise GitHubError(error.code, method, path) from error

    def pages(self, path, field=None):
        result = []
        for page in range(1, 1001):
            data = self.api(path + ('&' if '?' in path else '?') + f'per_page=100&page={page}')
            items = data[field] if field else data
            result.extend(items)
            if len(items) < 100:
                return result
        raise RuntimeError('Pagination limit reached; refusing incomplete release history')

    def upload(self, release_id, path):
        url = f'https://uploads.github.com/repos/{self.repo}/releases/{release_id}/assets?name={quote(path.name)}'
        with path.open('rb') as content:
            request = Request(url, method='POST', data=content, headers={
                'Authorization': 'Bearer ' + os.environ['GH_TOKEN'],
                'Content-Type': 'application/octet-stream',
                'Content-Length': str(path.stat().st_size),
                'Accept': 'application/vnd.github+json'})
            try:
                with urlopen(request, timeout=300) as response:
                    return json.loads(response.read())
            except HTTPError as error:
                raise GitHubError(error.code, 'POST', f'releases/{release_id}/assets') from error


class Reconciler:
    def __init__(self, gh):
        self.gh = gh
        self.branch = gh.api('')['default_branch']
        self.image = 'ghcr.io/' + gh.repo.lower()
        self.releases = gh.pages('releases')
        self.quarantined = set()

    def save(self, release, state, **fields):
        tag = state.get('tag') or release['tag_name']
        if not version_key(tag):
            raise ValueError('Cannot save a release without a canonical tag')
        state['tag'] = tag
        updated = self.gh.api(f'releases/{release["id"]}', 'PATCH',
                              dict(body=body_for(state), tag_name=tag, **fields))
        if updated['tag_name'] != tag and not (updated['draft'] and updated['tag_name'].startswith('untagged-')):
            raise ValueError('GitHub returned an unexpected published tag')
        release.update(updated)
        # Use our frozen identity even if GitHub exposes its internal draft alias.
        release['tag_name'] = tag

    def pr(self, number):
        return self.gh.api(f'pulls/{number}')

    def commit(self, sha):
        return self.gh.api('commits/' + sha)

    def eligible(self, pr):
        return (pr['base']['ref'] == self.branch and pr['merged']
                and bool(pr.get('merged_at')) and bool(SHA.fullmatch(pr['merge_commit_sha'])))

    def source(self, pr):
        if not self.eligible(pr):
            raise RuntimeError('Only merged PRs targeting the default branch may release')
        return pr['merge_commit_sha']

    def same_source(self, state, pr):
        if not self.eligible(pr):
            return False
        return (state['sha'] == self.source(pr)
                and state['tree'] == self.commit(state['sha'])['commit']['tree']['sha'])

    def managed(self):
        result = []
        for release in self.releases:
            try:
                state = state_of(release)
                if not state:
                    continue
                if release['tag_name'] != state['tag']:
                    self.verify_tag(dict(release, tag_name=state['tag']), state,
                                    missing=state.get('schema') == 2 and release['draft'])
                    release['tag_name'] = state['tag']
                result.append((release, state))
            except (ValueError, KeyError, TypeError, RuntimeError) as error:
                if not release['draft']:
                    raise RuntimeError(f'Invalid published release {release["id"]}: {error}') from error
                self.quarantined.add(release['id'])
                print(f'::warning::Quarantined release {release["id"]}: {error}')
        return result

    def active(self):
        # v1 pre-merge candidates are historical records, never resumed by v2.
        return [(r, s) for r, s in self.managed() if s.get('schema') == 2]

    def base_version(self, pr, sha):
        content = self.gh.api(f'contents/backend/cmd/server/VERSION?ref={sha}')
        version = base64.b64decode(content['content']).decode().strip().removeprefix('v')
        return source_base(version, pr['head'].get('ref', ''))

    def recover(self, tag):
        if not version_key(tag):
            raise RuntimeError('Invalid recovery tag')
        release = next((r for r in self.releases if r['tag_name'] == tag), None)
        if release is None:
            raise RuntimeError('Recovery requires an existing release and tag')
        state = state_of(release)
        if state:
            if state.get('schema') != 2:
                raise RuntimeError('Cannot adopt a managed historical release')
            return release, state
        if self.gh.pages(f'releases/{release["id"]}/assets'):
            raise RuntimeError('Recovery only adopts empty releases; existing assets must not be replaced')
        sha = self.tag_commit(tag)
        prs = self.gh.pages(f'commits/{sha}/pulls')
        matches = [self.pr(p['number']) for p in prs]
        matches = [p for p in matches if self.eligible(p) and p['merge_commit_sha'] == sha]
        if len(matches) != 1:
            raise RuntimeError('Recovery tag must identify exactly one merged PR result')
        pr = matches[0]
        if self.base_version(pr, sha) != '.'.join(map(str, version_key(tag)[:3])):
            raise RuntimeError('Recovery tag base version differs from source')
        state = dict(schema=2, tag=tag, pr=pr['number'], sha=sha,
                     tree=self.commit(sha)['commit']['tree']['sha'], simple=False,
                     merged_at=pr['merged_at'], recovery=True, ready=False)
        self.save(release, state)
        return release, state

    def resume(self, release, state):
        if state.get('ready'):
            self.output(work='false', build='false')
            return
        state['attempted_at'] = datetime.now(timezone.utc).isoformat()
        self.save(release, state)
        run = state.get('bundle_run') or state.get('build_run')
        reusable = False
        if run:
            artifacts = self.gh.pages(f'actions/runs/{run}/artifacts', 'artifacts')
            reusable = any(a['name'] == state['tag'] and not a['expired'] for a in artifacts)
            if not reusable and state.get('files'):
                # Once any publication can have happened, rebuilding may change bytes.
                raise RuntimeError('Frozen artifact unavailable; restore the original bundle, do not rebuild or renumber')
        if reusable:
            state['bundle_run'] = run
        else:
            state.pop('bundle_run', None)
            state['build_run'] = int(os.environ['GITHUB_RUN_ID'])
        state['attempted_at'] = datetime.now(timezone.utc).isoformat()
        self.save(release, state)
        self.output(work='true', build=str(not reusable).lower(), tag=state['tag'],
                    sha=state['sha'], bundle_run=str(run if reusable else ''))

    def bootstrap_recovery(self):
        config = json.loads(Path(__file__).with_name('merged-only.json').read_text())
        for entry in config.get('recover_releases', []):
            if entry['repo'] != self.gh.repo:
                continue
            release = next((r for r in self.releases if r['tag_name'] == entry['tag']), None)
            if release is None or state_of(release):
                continue
            if self.gh.pages(f'releases/{release["id"]}/assets'):
                continue  # A maintainer has already supplied assets; never adopt implicitly.
            if self.tag_commit(entry['tag']) != entry['sha']:
                raise RuntimeError('Bootstrap recovery tag differs from the audited source')
            pr = self.pr(entry['pr'])
            if not self.eligible(pr) or pr['merge_commit_sha'] != entry['sha']:
                raise RuntimeError('Bootstrap recovery PR differs from the audited source')
            self.recover(entry['tag'])

    def plan(self):
        recovery = os.environ.get('RECOVER_TAG', '')
        if recovery:
            self.resume(*self.recover(recovery))
            return
        self.bootstrap_recovery()
        since = datetime.fromisoformat(os.environ['AUTOMATION_SINCE'].replace('Z', '+00:00'))
        known = self.active()
        candidates = []
        # Recovery records remain eligible even before the new activation boundary.
        for release, state in known:
            if state.get('recovery') and not state.get('ready'):
                candidates.append((release, state))
        prs = self.gh.pages(f'pulls?state=closed&base={quote(self.branch)}&sort=updated&direction=desc')
        for summary in prs:
            if not summary.get('merged_at') or datetime.fromisoformat(
                    summary['merged_at'].replace('Z', '+00:00')) < since:
                continue
            pr = self.pr(summary['number'])
            if not self.eligible(pr):
                continue
            matches = [(r, s) for r, s in known if s['pr'] == pr['number']]
            if matches:
                if any(not self.same_source(state, pr) for _, state in matches):
                    raise RuntimeError('Merged PR release identity changed')
                # Historical parser failures may have allocated multiple versions.
                # Keep every reservation and frozen bundle; never allocate another.
                for match in matches:
                    if not match[1].get('ready') and match not in candidates:
                        candidates.append(match)
                continue
            sha = self.source(pr)
            candidates.append((None, dict(schema=2, pr=pr['number'], sha=sha,
                                         tree=self.commit(sha)['commit']['tree']['sha'],
                                         simple=False, merged_at=pr['merged_at'])))
        if not candidates:
            self.output(work='false', build='false')
            return
        # Allocate new merges chronologically; retries rotate so failures do not starve others.
        release, state = min(candidates, key=lambda pair: (
            pair[1].get('attempted_at', ''), pair[1]['merged_at'], pair[1]['pr']))
        if release is None:
            base = self.base_version(self.pr(state['pr']), state['sha'])
            prefix = 'v' + base + '-hao.'
            tags = [s['tag'] for _, s in self.managed()]
            tags += [r['tag_name'] for r in self.releases] + [t['name'] for t in self.gh.pages('tags')]
            number = max([int(t[len(prefix):]) for t in tags
                          if t.startswith(prefix) and t[len(prefix):].isdigit()] or [0]) + 1
            tag = state['tag'] = prefix + str(number)
            # Reserve in a draft first. No public tag is created before checks/build succeed.
            release = self.gh.api('releases', 'POST', {
                'tag_name': tag, 'target_commitish': state['sha'], 'draft': True,
                'prerelease': False, 'make_latest': 'false', 'name': f'Sub2API {tag[1:]}',
                'body': body_for(state)})
        self.resume(release, state)

    def create_tag(self, tag, sha):
        payload = {'ref': 'refs/tags/' + tag, 'sha': sha}
        try:
            # Suppress tag-push workflows from OLD candidate commits. Creating a
            # release with a PAT before this ref exists could wake the legacy publisher.
            self.gh.api('git/refs', 'POST', payload, token_env='TAG_TOKEN')
        except GitHubError as error:
            if error.status != 403:
                raise
            trusted = os.environ['TRUSTED_SHA']
            def workflows(commit):
                entries = self.gh.api(f'contents/.github?ref={commit}')
                return next(item['sha'] for item in entries if item['name'] == 'workflows')
            if workflows(sha) != workflows(trusted):
                raise RuntimeError('Native tag token denied; candidate workflows differ from trusted main. '
                                   'Update the PR from main before retrying; do not use a PAT to trigger old workflows.')
            self.gh.api('git/refs', 'POST', payload)

    @staticmethod
    def output(**values):
        with open(os.environ['GITHUB_OUTPUT'], 'a') as output:
            for key, value in values.items():
                output.write(f'{key}={value}\n')

    def selected(self):
        tag = os.environ['RELEASE_TAG']
        return next(r for r, _ in self.managed() if r['tag_name'] == tag)

    def publish(self):
        release = self.selected()
        state = state_of(release)
        if state.get('ready'):
            return
        if state.get('schema') != 2 or state['simple']:
            raise RuntimeError('Only full merged-PR releases may publish')
        bundle = Path(os.environ['BUNDLE_DIR'])
        manifest = json.loads((bundle / 'bundle.json').read_text())
        if (manifest['tag'] != release['tag_name'] or manifest['sha'] != state['sha']
                or manifest['simple'] != state['simple']):
            raise RuntimeError('Bundle does not match the allocated release')
        files = manifest['files']
        if set(files) != expected_files(state['tag']):
            raise RuntimeError('Incomplete bundle')
        for name, digest in files.items():
            if not re.fullmatch(r'[a-zA-Z0-9_.-]+', name) or not re.fullmatch(r'[a-f0-9]{64}', digest):
                raise RuntimeError('Invalid bundle filename/digest')
            path = bundle / name
            if path.is_symlink() or digest_file(path) != digest:
                raise RuntimeError('Bundle integrity failure: ' + name)
        checksums = ''.join(f'{digest}  {name}\n' for name, digest in sorted(files.items())
                            if name not in {'image.tar', 'checksums.txt'})
        if (bundle / 'checksums.txt').read_text() != checksums:
            raise RuntimeError('Checksum manifest does not describe the binary archives')
        if state.get('files') and state['files'] != files:
            raise RuntimeError('Refusing to overwrite an immutable build')
        if not self.same_source(state, self.pr(state['pr'])):
            raise RuntimeError('Bundle source is not the merged PR result')
        self.verify_tag(release, state, missing=True)
        # Persist the exact successful, fully tested build before publishing anything.
        state.update(files=files, bundle_run=state.get('bundle_run') or int(os.environ['GITHUB_RUN_ID']))
        self.save(release, state)
        target = self.image + ':' + release['tag_name'][1:]
        archive = 'oci-archive:' + str(bundle / 'image.tar')
        self.verify_platforms(archive)
        expected_digest = self.inspect_reference(archive)
        if state.get('image_digest') and state['image_digest'] != expected_digest:
            raise RuntimeError('Frozen image digest differs from the bundle')
        # Freeze the expected OCI digest before the first registry write as well.
        state['image_digest'] = expected_digest
        self.save(release, state)
        self.ensure_version_image(archive, target, expected_digest)
        assets = {a['name']: a for a in self.gh.pages(f'releases/{release["id"]}/assets')}
        for name, digest in files.items():
            if name == 'image.tar':
                continue
            if name in assets:
                if assets[name].get('digest') != 'sha256:' + digest:
                    raise RuntimeError('Existing asset differs: ' + name)
            else:
                self.gh.upload(release['id'], bundle / name)
        if self.tag_commit(state['tag'], missing=True) is None:
            self.create_tag(state['tag'], state['sha'])
        self.verify_tag(release, state)
        state['ready'] = True
        self.save(release, state)

    def tag_commit(self, tag, missing=False):
        ref = self.gh.api('git/ref/tags/' + quote(tag, safe=''), missing=missing)
        if ref is None:
            if missing:
                return None
            raise RuntimeError('Release tag is missing')
        obj = ref['object']
        while obj['type'] == 'tag':
            obj = self.gh.api('git/tags/' + obj['sha'])['object']
        if obj['type'] != 'commit' or not SHA.fullmatch(obj['sha']):
            raise RuntimeError('Release tag does not point at a commit')
        return obj['sha']

    def verify_tag(self, release, state, missing=False):
        sha = self.tag_commit(state.get('tag') or release['tag_name'], missing=missing)
        if sha is not None and sha != state['sha']:
            raise RuntimeError('Tag moved; refusing publication')

    @staticmethod
    def verify_platforms(reference):
        result = subprocess.run(['skopeo', 'inspect', '--raw', reference],
                                capture_output=True, text=True, check=True)
        manifest = json.loads(result.stdout)
        platforms = {(m.get('platform', {}).get('os'), m.get('platform', {}).get('architecture'))
                     for m in manifest.get('manifests', [])}
        if platforms != {('linux', 'amd64'), ('linux', 'arm64')}:
            raise RuntimeError('Release image must contain linux/amd64 and linux/arm64')

    @staticmethod
    def inspect_image(image):
        return Reconciler.inspect_reference('docker://' + image)

    @staticmethod
    def inspect_reference(reference, missing=False):
        result = subprocess.run(['skopeo', 'inspect', '--format', '{{.Digest}}', reference],
                                capture_output=True, text=True)
        if result.returncode:
            # Authentication/network failures must not be mistaken for a missing tag.
            if missing and ('manifest unknown' in result.stderr.lower() or
                            'name unknown' in result.stderr.lower()):
                return None
            raise RuntimeError('Unable to inspect image reference: ' + reference)
        digest = result.stdout.strip()
        if not re.fullmatch(r'sha256:[a-f0-9]{64}', digest):
            raise RuntimeError('Invalid registry digest')
        return digest

    @staticmethod
    def copy_image(source, target):
        subprocess.run(['skopeo', 'copy', '--all', '--preserve-digests', '--retry-times', '3',
                        source, 'docker://' + target], check=True)

    def ensure_version_image(self, source, target, digest):
        existing = self.inspect_reference('docker://' + target, missing=True)
        if existing is not None and existing != digest:
            raise RuntimeError('Version image tag moved; refusing overwrite')
        if existing is None:
            self.copy_image(source, target)
        if self.inspect_image(target) != digest:
            raise RuntimeError('Published image digest differs from the frozen bundle')

    def promote(self):
        errors = []
        ready = []
        hub = os.environ.get('DOCKERHUB_USERNAME', '')
        for release, state in self.active():
            if not state.get('ready'):
                continue
            try:
                if not self.same_source(state, self.pr(state['pr'])):
                    raise RuntimeError('Release no longer matches its merged source')
                self.verify_tag(release, state)
                source = 'docker://' + self.image + '@' + state['image_digest']
                if set(state.get('files', {})) != expected_files(state['tag']):
                    raise RuntimeError('Incomplete frozen release manifest')
                self.verify_platforms(source)
                self.ensure_version_image(source, self.image + ':' + state['tag'][1:], state['image_digest'])
                assets = {a['name']: a for a in self.gh.pages(f'releases/{release["id"]}/assets')}
                for name, digest in state['files'].items():
                    if name != 'image.tar' and assets.get(name, {}).get('digest') != 'sha256:' + digest:
                        raise RuntimeError('Published asset missing or changed: ' + name)
                # Every version gets its Docker Hub tag, including delayed historical builds.
                if hub:
                    self.ensure_version_image(source, hub + '/sub2api:' + state['tag'][1:],
                                              state['image_digest'])
                ready.append((release, state))
            except Exception as error:
                errors.append(str(error))
        if errors:
            raise RuntimeError('\n'.join(errors))
        # Include historical releases as guards; never guess their image provenance.
        peers = [r for r in self.releases if not r['draft'] and not r['prerelease']
                 and version_key(r['tag_name'])]
        peers += [r for r, _ in ready if r not in peers]
        if not peers:
            return

        def channel_order(release):
            key = version_key(release['tag_name'])
            state = state_of(release)
            merged_at = state.get('merged_at') if state else None
            return (key[:3], merged_at or release['published_at'], key[3])

        channels = {}
        for release in peers:
            key = version_key(release['tag_name'])
            for alias in ('latest', str(key[0]), f'{key[0]}.{key[1]}'):
                if alias not in channels or channel_order(release) > channel_order(channels[alias]):
                    channels[alias] = release
        active_ids = {r['id'] for r, _ in ready}
        for alias, release in channels.items():
            if release['id'] not in active_ids:
                continue
            state = state_of(release)
            source = 'docker://' + self.image + '@' + state['image_digest']
            self.copy_image(source, self.image + ':' + alias)
            if hub:
                self.copy_image(source, hub + '/sub2api:' + alias)
        # Finalize only after all registry steps succeed. Partial success is retried.
        for release, state in ready:
            if not state.get('complete') or release['draft'] or release['prerelease']:
                state['complete'] = True
                self.save(release, state, draft=False, prerelease=False, make_latest='false')
        latest = channels.get('latest')
        if latest and latest['id'] in active_ids:
            self.gh.api(f'releases/{latest["id"]}', 'PATCH', {'make_latest': 'true'})


if __name__ == '__main__':
    getattr(Reconciler(GitHub()), sys.argv[1])()
