#!/usr/bin/env python3
"""Build a release bundle without registry credentials or repository write access."""
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import tarfile
import tempfile
import zipfile


def run(*args, **kwargs):
    subprocess.run(args, check=True, **kwargs)


def main():
    tag, sha = os.environ['RELEASE_TAG'], os.environ['RELEASE_SHA']
    version = tag.removeprefix('v')
    simple = os.environ.get('SIMPLE_RELEASE') == 'true'
    out = Path(os.environ['BUNDLE_DIR']).resolve()
    out.mkdir(parents=True, exist_ok=True)
    Path('backend/cmd/server/VERSION').write_text(version + '\n')
    run('pnpm', 'install', '--frozen-lockfile', cwd='frontend')
    run('pnpm', 'run', 'build', cwd='frontend')
    date = subprocess.check_output(['git', 'show', '-s', '--format=%cI', sha], text=True).strip()
    targets = [('linux', 'amd64')] if simple else [
        ('linux', 'amd64'), ('linux', 'arm64'), ('darwin', 'amd64'),
        ('darwin', 'arm64'), ('windows', 'amd64')]
    with tempfile.TemporaryDirectory() as temp:
        context = Path(temp)
        for goos, arch in targets:
            binary = context / 'bin' / arch / ('sub2api.exe' if goos == 'windows' else 'sub2api')
            # Linux files are retained for the image; other OS files use a separate path.
            if goos != 'linux':
                binary = context / goos / arch / binary.name
            binary.parent.mkdir(parents=True, exist_ok=True)
            env = dict(os.environ, CGO_ENABLED='0', GOOS=goos, GOARCH=arch)
            run('go', 'build', '-trimpath', '-tags=embed', '-ldflags',
                f'-s -w -X main.Commit={sha} -X main.Date={date} -X main.BuildType=release',
                '-o', str(binary), './cmd/server', cwd='backend', env=env)
            if simple:
                continue
            files = [(binary, binary.name)] + [(p, str(p)) for p in
                     [*Path('.').glob('LICENSE*'), *Path('.').glob('README*'), Path('deploy')]]
            name = f'sub2api_{version}_{goos}_{arch}'
            if goos == 'windows':
                with zipfile.ZipFile(out / (name + '.zip'), 'w', zipfile.ZIP_DEFLATED) as archive:
                    for path, dest in files:
                        if path.is_dir():
                            for child in path.rglob('*'):
                                if child.is_file():
                                    archive.write(child, str(child))
                        else:
                            archive.write(path, dest)
            else:
                with tarfile.open(out / (name + '.tar.gz'), 'w:gz') as archive:
                    for path, dest in files:
                        archive.add(path, arcname=dest)
        shutil.copytree('deploy', context / 'deploy')
        shutil.copytree('backend/resources', context / 'backend/resources')
        dockerfile = Path('Dockerfile.goreleaser').read_text()
        if 'COPY sub2api /app/sub2api' not in dockerfile:
            raise RuntimeError('Dockerfile.goreleaser binary COPY contract changed')
        dockerfile = dockerfile.replace('COPY sub2api /app/sub2api',
                                       'ARG TARGETARCH\nCOPY bin/${TARGETARCH}/sub2api /app/sub2api')
        (context / 'Dockerfile').write_text(dockerfile)
        platforms = 'linux/amd64' if simple else 'linux/amd64,linux/arm64'
        run('docker', 'buildx', 'build', '--platform', platforms, '--provenance=false',
            '--label', f'org.opencontainers.image.revision={sha}',
            '--label', f'org.opencontainers.image.version={version}',
            '--output', f'type=oci,dest={out / "image.tar"}', str(context))
    hashes = {}
    for path in out.iterdir():
        if path.is_file():
            with path.open('rb') as handle:
                hashes[path.name] = hashlib.file_digest(handle, 'sha256').hexdigest()
    (out / 'checksums.txt').write_text(''.join(f'{h}  {n}\n' for n, h in sorted(hashes.items()) if n != 'image.tar'))
    hashes['checksums.txt'] = hashlib.sha256((out / 'checksums.txt').read_bytes()).hexdigest()
    (out / 'bundle.json').write_text(json.dumps({'tag': tag, 'sha': sha, 'simple': simple, 'files': hashes}))


if __name__ == '__main__':
    main()
