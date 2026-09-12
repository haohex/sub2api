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
MARKER = re.compile(r'<!-- sub2api-release-v1\n(.*?)\n-->', re.S)


def version_key(tag):
    match = TAG.fullmatch(tag)
    return tuple(map(int, match.groups())) if match else None


def digest_file(path):
    with path.open('rb') as handle:
        return hashlib.file_digest(handle, 'sha256').hexdigest()


def state_of(release):
    match = MARKER.search(release.get('body') or '')
    if not match:
        return None
    state = json.loads(match[1])
    if (not version_key(release['tag_name']) or not SHA.fullmatch(state['sha'])
            or not SHA.fullmatch(state['tree']) or not isinstance(state['pr'], int)
            or state['pr'] <= 0 or not isinstance(state['simple'], bool)):
        raise ValueError('Invalid managed release metadata')
    return state


def body_for(state):
    # Never interpolate PR text into shell commands or executable files.
    return (f'PR #{state["pr"]} · commit `{state["sha"]}`\n\n'
            f'构建模式：{"linux/amd64 镜像" if state["simple"] else "完整安装包与多架构镜像"}。'
            '\n候选版本仅供测试；正式版在合并校验完成后自动提升。\n\n'
            '<!-- sub2api-release-v1\n' + json.dumps(state, sort_keys=True) + '\n-->')


class GitHub:
    def __init__(self):
        self.repo = os.environ['GITHUB_REPOSITORY']
        self.root = f'https://api.github.com/repos/{self.repo}'

    def api(self, path, method='GET', data=None, missing=False):
        request = Request(self.root + ('/' + path if path else ''), method=method,
                          data=None if data is None else json.dumps(data).encode(), headers={
                              'Authorization': 'Bearer ' + os.environ['GH_TOKEN'],
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
            raise RuntimeError(f'GitHub {method} {path}: HTTP {error.code}') from error

    def pages(self, path, field=None):
        result = []
        for page in range(1, 1001):
            data = self.api(path + ('&' if '?' in path else '?') + f'per_page=100&page={page}')
            items = data[field] if field else data
            result.extend(items)
            if len(items) < 100:
                return result
        raise RuntimeError('Pagination limit reached; refusing incomplete release history')


class Reconciler:
    def __init__(self, gh):
        self.gh = gh
        self.branch = gh.api('')['default_branch']
        self.image = 'ghcr.io/' + gh.repo.lower()
        self.releases = gh.pages('releases')

    def save(self, release, state, **fields):
        updated = self.gh.api(f'releases/{release["id"]}', 'PATCH',
                              dict(body=body_for(state), **fields))
        release.update(updated)

    def pr(self, number):
        return self.gh.api(f'pulls/{number}')

    def commit(self, sha):
        return self.gh.api('commits/' + sha)

    def eligible(self, pr):
        return (pr['base']['ref'] == self.branch and not pr['draft']
                and (pr['merged'] or (pr['head']['repo'] is not None
                     and pr['head']['repo']['full_name'] == self.gh.repo)))

    def source(self, pr):
        return pr['merge_commit_sha'] if pr['merged'] else pr['head']['sha']

    def same_source(self, state, pr):
        if not self.eligible(pr) or (pr['state'] == 'closed' and not pr['merged']):
            return False
        source = self.source(pr)
        return state['sha'] == source or state['tree'] == self.commit(source)['commit']['tree']['sha']

    def managed(self):
        return [(release, state_of(release)) for release in self.releases if state_of(release)]

    def plan(self):
        since = datetime.fromisoformat(os.environ['AUTOMATION_SINCE'].replace('Z', '+00:00'))
        # Include merged PRs even if an event was suppressed or no candidate existed.
        prs = self.gh.pages(f'pulls?state=all&base={quote(self.branch)}&sort=updated&direction=desc')
        candidates = []
        known = self.managed()
        for summary in prs:
            if summary['state'] == 'closed' and (not summary.get('merged_at') or
                    datetime.fromisoformat(summary['merged_at'].replace('Z', '+00:00')) < since):
                continue
            pr = self.pr(summary['number'])
            if not self.eligible(pr):
                continue
            source = self.source(pr)
            commit = self.commit(source)
            tree = commit['commit']['tree']['sha']
            matches = [(r, s) for r, s in known if s['pr'] == pr['number']
                       and not s.get('abandoned') and (s['sha'] == source or s['tree'] == tree)]
            if any(s.get('ready') for _, s in matches):
                continue
            if matches:
                release, state = max(matches, key=lambda item: version_key(item[0]['tag_name']))
                if state.get('bundle_run'):
                    artifacts = self.gh.pages(f'actions/runs/{state["bundle_run"]}/artifacts', 'artifacts')
                    if not any(a['name'] == release['tag_name'] and not a['expired'] for a in artifacts):
                        state['abandoned'] = 'Build artifact expired; allocate a new immutable version'
                        self.save(release, state)
                        matches = []
            if not matches:
                state = {'pr': pr['number'], 'sha': source, 'tree': tree,
                         'simple': os.environ.get('SIMPLE_RELEASE') == 'true'}
                release = None
            candidates.append((release, state))
        if not candidates:
            self.output(work='false', build='false')
            return
        # One build per run; round robin prevents a broken PR starving others.
        release, state = min(candidates, key=lambda pair: pair[1].get('attempted_at', ''))
        if release is None:
            content = self.gh.api(f'contents/backend/cmd/server/VERSION?ref={state["sha"]}')
            base = base64.b64decode(content['content']).decode().strip().removeprefix('v')
            match = re.match(r'^(\d+\.\d+\.\d+)(?:-|$)', base)
            if not match:
                raise RuntimeError('Cannot determine release base version')
            prefix = 'v' + match[1] + '-hao.'
            tags = [r['tag_name'] for r in self.releases] + [t['name'] for t in self.gh.pages('tags')]
            number = max([int(t[len(prefix):]) for t in tags
                          if t.startswith(prefix) and t[len(prefix):].isdigit()] or [0]) + 1
            tag = prefix + str(number)
            release = self.gh.api('releases', 'POST', {
                'tag_name': tag, 'target_commitish': state['sha'], 'draft': True,
                'prerelease': True, 'make_latest': 'false', 'name': f'Sub2API {tag[1:]}',
                'body': body_for(state)})
        state['attempted_at'] = datetime.now(timezone.utc).isoformat()
        self.save(release, state)
        self.output(work='true', build=str(not state.get('bundle_run')).lower(),
                    tag=release['tag_name'], sha=state['sha'],
                    simple=str(state['simple']).lower(), bundle_run=str(state.get('bundle_run', '')))

    @staticmethod
    def output(**values):
        with open(os.environ['GITHUB_OUTPUT'], 'a') as output:
            for key, value in values.items():
                output.write(f'{key}={value}\n')

    def selected(self):
        tag = os.environ['RELEASE_TAG']
        return next(r for r in self.releases if r['tag_name'] == tag)

    def publish(self):
        release = self.selected()
        state = state_of(release)
        if state.get('ready'):
            return
        bundle = Path(os.environ['BUNDLE_DIR'])
        manifest = json.loads((bundle / 'bundle.json').read_text())
        if (manifest['tag'] != release['tag_name'] or manifest['sha'] != state['sha']
                or manifest['simple'] != state['simple']):
            raise RuntimeError('Bundle does not match the allocated release')
        files = manifest['files']
        expected = {'image.tar', 'checksums.txt'}
        if not state['simple']:
            version = release['tag_name'][1:]
            expected.update(f'sub2api_{version}_{platform}.{extension}' for platform, extension in [
                ('linux_amd64', 'tar.gz'), ('linux_arm64', 'tar.gz'),
                ('darwin_amd64', 'tar.gz'), ('darwin_arm64', 'tar.gz'), ('windows_amd64', 'zip')])
        if set(files) != expected:
            raise RuntimeError('Incomplete bundle')
        for name, digest in files.items():
            if not re.fullmatch(r'[a-zA-Z0-9_.-]+', name) or not re.fullmatch(r'[a-f0-9]{64}', digest):
                raise RuntimeError('Invalid bundle filename/digest')
            path = bundle / name
            if path.is_symlink() or digest_file(path) != digest:
                raise RuntimeError('Bundle integrity failure: ' + name)
        if state.get('files') and state['files'] != files:
            raise RuntimeError('Refusing to overwrite an immutable build')
        self.verify_tag(release, state, missing=True)
        # Persist the exact successful, fully tested build before publishing anything.
        state.update(files=files, bundle_run=state.get('bundle_run') or int(os.environ['GITHUB_RUN_ID']))
        self.save(release, state)
        target = self.image + ':' + release['tag_name'][1:]
        archive = 'oci-archive:' + str(bundle / 'image.tar')
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
                subprocess.run(['gh', 'release', 'upload', release['tag_name'], str(bundle / name),
                                '--repo', self.gh.repo], check=True)
        # A late PR update may leave this as a superseded prerelease; never promote it blindly.
        state['ready'] = True
        self.save(release, state, draft=False, prerelease=True, make_latest='false')

    def verify_tag(self, release, state, missing=False):
        ref = self.gh.api('git/ref/tags/' + quote(release['tag_name'], safe=''), missing=missing)
        if ref:
            obj = ref['object']
            while obj['type'] == 'tag':
                obj = self.gh.api('git/tags/' + obj['sha'])['object']
            if obj['sha'] != state['sha']:
                raise RuntimeError('Tag moved; refusing publication')

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
        # First establish which ready candidates actually represent merged code.
        for release, state in self.managed():
            if not state.get('ready') or state.get('abandoned'):
                continue
            try:
                pr = self.pr(state['pr'])
                if not pr['merged'] or not self.same_source(state, pr):
                    continue
                self.verify_tag(release, state)
                if release['prerelease']:
                    stable_peers = [(r, state_of(r)) for r in self.releases
                                    if not r['draft'] and not r['prerelease']
                                    and version_key(r['tag_name'])
                                    and version_key(r['tag_name'])[:3] == version_key(release['tag_name'])[:3]]
                    if any(s and s.get('merged_at', '') > pr['merged_at'] for _, s in stable_peers):
                        continue  # An older merge completing late stays a historical prerelease.
                    if any(version_key(r['tag_name']) > version_key(release['tag_name']) for r, _ in stable_peers):
                        state['abandoned'] = 'Newer merged code requires a new monotonically increasing version'
                        self.save(release, state)
                        continue
                    state['merged_sha'] = pr['merge_commit_sha']
                    state['merged_at'] = pr['merged_at']
                    # Stable aliases are reconciled separately, even after a partial failure.
                    self.save(release, state, prerelease=False, make_latest='false')
            except Exception as error:
                errors.append(str(error))
        invalid = set()
        # An invalid stable release must never be chosen as a channel source.
        for release, state in self.managed():
            if release['draft'] or release['prerelease']:
                continue
            try:
                pr = self.pr(state['pr'])
                if not pr['merged'] or not self.same_source(state, pr) or not state.get('ready'):
                    raise RuntimeError('Stable release no longer matches its merged source')
                self.verify_tag(release, state)
            except Exception as error:
                invalid.add(release['id'])
                errors.append(str(error))
        # Fail closed before changing any mutable channel if provenance verification failed.
        if invalid:
            raise RuntimeError('\n'.join(errors))
        stable = [r for r in self.releases if not r['draft'] and not r['prerelease']
                  and version_key(r['tag_name'])]
        if not stable:
            if errors:
                raise RuntimeError('\n'.join(errors))
            return
        # Elect the highest version for each mutable channel, including legacy releases.
        def channel_order(release):
            key = version_key(release['tag_name'])
            state = state_of(release)
            # Late completion of an older merged PR must not revert newer merged code.
            merged_at = state.get('merged_at') if state else None
            return (key[:3], merged_at or release['published_at'], key[3])

        channels = {}
        for release in stable:
            key = version_key(release['tag_name'])
            for alias in ('latest', str(key[0]), f'{key[0]}.{key[1]}'):
                if alias not in channels or channel_order(release) > channel_order(channels[alias]):
                    channels[alias] = release
        for alias, release in channels.items():
            state = state_of(release)
            if not state or not state.get('image_digest'):
                continue  # Never guess provenance of pre-existing releases.
            try:
                source = 'docker://' + self.image + '@' + state['image_digest']
                # Reconcile idempotently from the frozen digest, not a mutable version tag.
                self.copy_image(source, self.image + ':' + alias)
                hub = os.environ.get('DOCKERHUB_USERNAME')
                if hub and not state['simple']:
                    self.ensure_version_image(source, hub + '/sub2api:' + release['tag_name'][1:],
                                              state['image_digest'])
                    self.copy_image(source, hub + '/sub2api:' + alias)
                if alias == 'latest':
                    self.sync_version(release['tag_name'][1:])
                    self.gh.api(f'releases/{release["id"]}', 'PATCH', {'make_latest': 'true'})
            except Exception as error:
                errors.append(f'{alias}: {error}')
        if errors:
            raise RuntimeError('\n'.join(errors))

        latest = channels['latest']
        state = state_of(latest)
        if state:
            self.notify(latest, state)

    def notify(self, release, state):
        token = os.environ.get('TELEGRAM_BOT_TOKEN')
        chat = os.environ.get('TELEGRAM_CHAT_ID')
        if not token or not chat or state['simple'] or state.get('notified'):
            return
        message = (f'Sub2API {release["tag_name"]} 已正式发布（PR #{state["pr"]}）\n'
                   f'https://github.com/{self.gh.repo}/releases/tag/{release["tag_name"]}\n'
                   f'docker pull {self.image}:{release["tag_name"][1:]}')
        request = Request(f'https://api.telegram.org/bot{token}/sendMessage',
                          data=json.dumps({'chat_id': chat, 'text': message}).encode(),
                          headers={'Content-Type': 'application/json'}, method='POST')
        try:
            with urlopen(request, timeout=30) as response:
                if not json.loads(response.read()).get('ok'):
                    raise RuntimeError('Telegram rejected the notification')
        except Exception:
            # Do not include the request URL: it contains the bot credential.
            raise RuntimeError('Formal release notification failed; will retry') from None
        state['notified'] = True
        self.save(release, state)

    def sync_version(self, version):
        path = 'contents/backend/cmd/server/VERSION'
        current = self.gh.api(path + '?ref=' + quote(self.branch, safe=''))
        old = base64.b64decode(current['content']).decode().strip()
        if old == version:
            return
        if version_key('v' + old) and version_key('v' + old) > version_key('v' + version):
            raise RuntimeError('Refusing VERSION downgrade')
        self.gh.api(path, 'PUT', {
            'message': f'chore(发布): 同步正式版本 {version} [skip ci]',
            'content': base64.b64encode((version + '\n').encode()).decode(),
            'sha': current['sha'], 'branch': self.branch})


if __name__ == '__main__':
    getattr(Reconciler(GitHub()), sys.argv[1])()
