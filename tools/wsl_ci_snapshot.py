"""Check a locked Windows source snapshot in a disposable native Linux clone.

Read the common object store without modifying the original repository. Preserve
the exact candidate commit and overlay only hash-verified manifest-owned source.
"""
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile


def bounded(path, boundary):
    path = Path(os.path.abspath(path))
    boundary = Path(os.path.abspath(boundary))
    if not path.is_relative_to(boundary):
        raise RuntimeError('Snapshot path escaped its boundary')
    current = path
    while True:
        try:
            metadata = current.lstat()
        except FileNotFoundError:
            metadata = None
        if current.is_symlink() or (metadata and getattr(metadata, 'st_file_attributes', 0) & 0x400):
            raise RuntimeError('Snapshot path contains a link/reparse point')
        if current == current.parent:
            return path
        current = current.parent


def main():
    common, source, sha, destination = sys.argv[1:]
    source, destination = Path(source), Path(destination)
    bounded(source / '.havok-ci-manifest.json', source)
    bounded(destination, source.parent / 'results')
    manifest = json.loads((source / '.havok-ci-manifest.json').read_text())
    digest = hashlib.sha256(json.dumps(manifest['files'], sort_keys=True).encode()).hexdigest()
    if digest != manifest['source_digest']:
        raise RuntimeError('Source manifest digest mismatch')
    env = {key:value for key,value in os.environ.items() if not key.startswith('GIT_')}
    env.update(GIT_CONFIG_GLOBAL=os.devnull, GIT_CONFIG_NOSYSTEM='1', PYTHONDONTWRITEBYTECODE='1')
    with tempfile.TemporaryDirectory(prefix='havok-ci-snapshot-', dir='/tmp') as temporary:
        repo = Path(temporary) / 'repo'
        def git(*args):
            result = subprocess.run(['git', '-c', 'core.hooksPath=/dev/null', *args],
                                    cwd=repo if repo.exists() else temporary, env=env,
                                    capture_output=True, text=True)
            if result.returncode:
                raise RuntimeError('Isolated Linux Git operation failed')
            return result.stdout.strip()
        git('clone', '--shared', '--no-checkout', common, str(repo))
        git('checkout', '--detach', sha)
        git('config', 'core.autocrlf', 'true')
        git('remote', 'set-url', 'origin', 'https://github.com/Havokyn/CLIProxyAPI.git')
        git('remote', 'add', 'upstream', 'https://github.com/router-for-me/CLIProxyAPI.git')
        git('remote', 'set-url', '--push', 'upstream', 'DISABLED')
        git('config', 'core.hooksPath', '/dev/null')
        for relative in git('ls-files', '-z').split('\0'):
            if relative and relative not in manifest['files']:
                target = bounded(repo / relative, repo)
                if target.is_file() or target.is_symlink():
                    target.unlink()  # Exact generated clone entries absent from the source manifest.
        for relative, digest in manifest['files'].items():
            if '.git' in Path(relative).parts:
                raise RuntimeError('Reserved Git metadata in source manifest')
            original, target = bounded(source / relative, source), bounded(repo / relative, repo)
            data = original.read_bytes()
            if hashlib.sha256(data).hexdigest() != digest:
                raise RuntimeError('Locked source snapshot changed')
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_bytes(data)
        # Overlaying CRLF onto an LF checkout can leave Git's stat cache dirty
        # even when converted blob hashes match. Normalize only this disposable
        # index and require identical ancestry/tree before accepting it as clean.
        git('add', '--all')
        if git('write-tree') != git('rev-parse', 'HEAD^{tree}'):
            raise RuntimeError('Snapshot content differs from the candidate tree')
        result = subprocess.run([sys.executable, 'tools/local_ci.py', '--linux'], cwd=repo, env=env)
        reports = list((repo / '.local-ci/results').glob('*/summary.json'))
        if len(reports) != 1:
            raise RuntimeError('Linux verification did not produce one report')
        report = json.loads(reports[0].read_text())
        report.update(parent_source_digest=manifest['source_digest'], source_sha=sha)
        for filename in ('windows.log', 'linux.log'):
            shutil.copyfile(reports[0].parent / filename, bounded(destination / ('wsl-' + filename), source.parent / 'results'))
        bounded(destination / 'wsl-summary.json', source.parent / 'results').write_text(json.dumps(report, indent=2) + '\n')
        if (report['git_sha'] != sha or report['result'] != 'PASS' or not report['complete']
                or report['mode'] != 'linux' or report['dirty'] is not False):
            return 1
        return result.returncode


if __name__ == '__main__':
    try:
        sys.exit(main())
    except Exception as error:
        print('Native Linux snapshot verification stopped: ' + type(error).__name__)
        sys.exit(1)
