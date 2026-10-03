"""Offline repository verification using only the Python standard library."""
from __future__ import annotations
import argparse
from datetime import datetime, timezone
import importlib.util
import json
import os
from pathlib import Path
import platform
import re
import shutil
import subprocess
import sys
import time
import tempfile
import hashlib
import stat

ROOT = Path(__file__).resolve().parents[1]
FORK = re.compile(r'^(?:https://github\.com/|git@github\.com:|ssh://git@github\.com/)Havokyn/CLIProxyAPI(?:\.git)?$', re.I)


def git(*args):
    command = ['git', '-c', f'safe.directory={ROOT.as_posix()}', '-c', 'core.autocrlf=true', *args]
    try:
        return subprocess.run(command, cwd=ROOT, env=dict(os.environ, GIT_OPTIONAL_LOCKS='0'),
                              capture_output=True, text=True, encoding='utf-8', errors='replace')
    except OSError:
        return subprocess.CompletedProcess(command, 1, stdout='', stderr='Cannot start Git')


def sanitize(text):
    spec = importlib.util.spec_from_file_location('ci_scanner', ROOT / 'ops/havok-fleet/check-secret-safety.py')
    scanner = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(scanner)
    output = []
    private_block = False
    for line in text.splitlines():
        if scanner.PRIVATE_KEY.search(line):
            private_block = True
        if private_block:
            output.append('[private-key diagnostic redacted]')
            if 'END ' in line and 'PRIVATE KEY' in line:
                private_block = False
            continue
        if scanner.scan_text(line):
            output.append('[credential-shaped diagnostic redacted]')
        else:
            output.append(re.sub(r'(https?://)[^\s/@]+:[^\s/@]+@', r'\1[redacted]@', line))
    return '\n'.join(output) + '\n'


def sanity():
    for args in [('diff', '--check'), ('diff', '--cached', '--check')]:
        if sys.platform == 'linux' and '--cached' not in args:
            # The Windows gate checks the entire tree. On shared DrvFS, avoid
            # rereading every upstream Go blob solely due to cross-OS stat data.
            args += ('--', 'tools', 'ops/havok-fleet', 'scripts/launchers',
                     'tests/fleet', 'tests/local_ci', 'docs/havok-fleet.md', '.gitattributes', '.gitignore')
        result = git(*args)
        if result.returncode:
            raise ValueError('Whitespace errors: git ' + ' '.join(args))
    urls = git('remote', 'get-url', '--all', '--push', 'origin')
    fetch = git('remote', 'get-url', '--all', 'origin')
    if urls.returncode or fetch.returncode or not urls.stdout.strip() or not all(
            FORK.fullmatch(url) for url in (urls.stdout + fetch.stdout).splitlines()):
        raise ValueError('origin fetch/push must identify Havokyn/CLIProxyAPI')
    if 'upstream' in git('remote').stdout.splitlines():
        push = git('remote', 'get-url', '--all', '--push', 'upstream')
        if push.returncode or any(url not in ('DISABLED', 'no_push') for url in push.stdout.splitlines()):
            raise ValueError('upstream push must be disabled: git remote set-url --push upstream DISABLED')
    print('Repository, origin fork, upstream push safety and whitespace checks passed.')


def wsl_exec(*arguments):
    # Bypass WSL's default shell so backslashes and positional arguments survive.
    return ['wsl.exe', '-d', 'Ubuntu-24.04', '--exec', *arguments]


def linux_static():
    files = list((ROOT / 'scripts/launchers/posix').iterdir()) + [ROOT / 'tools/havok-ci-linux.sh', ROOT / 'tools/git-hooks/pre-push']
    for path in files:
        if b'\r' in path.read_bytes():
            raise ValueError(f'{path.relative_to(ROOT)}: CR line endings in POSIX source')
        result = subprocess.run(['sh', '-n', str(path)], capture_output=True)
        if result.returncode:
            raise ValueError(f'{path.relative_to(ROOT)}: POSIX syntax error')
    for path in (ROOT / 'ops/havok-fleet').glob('*.py'):
        if b'\r' in path.read_bytes():
            raise ValueError(f'{path.relative_to(ROOT)}: CR line endings in Linux bootstrap source')
        compile(path.read_text(), str(path), 'exec')
    print('POSIX syntax, LF line endings and Linux bootstrap compilation passed.')


def native_windows_temp():
    import ctypes
    candidates = [os.environ.get('HAVOK_CI_TEMP'), tempfile.gettempdir(), r'O:\Temp']
    for candidate in filter(None, candidates):
        path = Path(candidate).resolve()
        filesystem = ctypes.create_unicode_buffer(64)
        valid = ctypes.windll.kernel32.GetVolumeInformationW(
            str(Path(path.anchor)), None, 0, None, None, None, filesystem, len(filesystem))
        if valid and filesystem.value == 'NTFS' and path.is_dir() and shutil.disk_usage(path).free >= 2 * 1024**3:
            return path
        if candidate == os.environ.get('HAVOK_CI_TEMP'):
            break
    raise ValueError('Windows tests need an NTFS temporary directory with 2 GiB free. Set HAVOK_CI_TEMP; the build cache remains on the repository drive.')


def assert_no_reparse(path, boundary):
    current = Path(os.path.abspath(path))
    boundary = Path(os.path.abspath(boundary))
    if not current.is_relative_to(boundary):
        raise ValueError('Path escaped its expected boundary')
    while current != boundary.parent:
        try:
            metadata = current.lstat()
        except FileNotFoundError:
            metadata = None
        if metadata and (stat.S_ISLNK(metadata.st_mode) or
                         getattr(metadata, 'st_file_attributes', 0) & 0x400):
            raise ValueError('Symlink/junction/reparse point in generated or source path; operation refused')
        if current == boundary:
            return
        current = current.parent
    raise ValueError('Path escaped its expected boundary')


def prepare_source(destination):
    """Mirror tracked and nonignored working source; never traverse runtime backups."""
    expected = ROOT / '.local-ci/source'
    if destination != expected:
        raise ValueError('Invalid verification source destination')
    assert_no_reparse(destination, ROOT)
    manifest_path = destination / '.havok-ci-manifest.json'
    previous = {'repository': ROOT.as_posix(), 'files': {}}
    if destination.exists():
        if not manifest_path.is_file():
            raise ValueError('Existing source snapshot has no ownership manifest; refusing to replace it')
        previous = json.loads(manifest_path.read_text())
        if previous['repository'] != ROOT.as_posix():
            raise ValueError('Source snapshot belongs to another repository')
        for path in destination.rglob('*'):
            assert_no_reparse(path, ROOT)
            if path.is_file() and path != manifest_path and path.relative_to(destination).as_posix() not in previous['files']:
                raise ValueError('Unexpected file in generated source snapshot; refusing to modify it')
    else:
        destination.mkdir(parents=True)
    listed = git('ls-files', '--cached', '--others', '--exclude-standard', '-z')
    if listed.returncode:
        raise ValueError('Cannot enumerate source-controlled/nonignored working files')
    files = {}
    for relative in sorted(set(filter(None, listed.stdout.split('\0')))):
        path = ROOT / relative
        target = destination / relative
        assert_no_reparse(path, ROOT)
        assert_no_reparse(target, ROOT)
        if not target.resolve().is_relative_to(destination.resolve()):
            raise ValueError('Source snapshot cannot follow symlinks or escaping paths')
        if not path.is_file():
            continue
        data = path.read_bytes()
        digest = hashlib.sha256(data).hexdigest()
        files[relative] = digest
        if not target.is_file() or hashlib.sha256(target.read_bytes()).hexdigest() != digest:
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_bytes(data)
    for relative in previous['files'].keys() - files.keys():
        target = destination / relative
        assert_no_reparse(target, ROOT)
        if not target.resolve().is_relative_to(destination.resolve()):
            raise ValueError('Invalid previous snapshot path; cleanup refused')
        if target.is_file():
            target.unlink()  # Only exact generated files from our ownership manifest.
    manifest = dict(repository=ROOT.as_posix(), files=files,
                    source_digest=hashlib.sha256(json.dumps(files, sort_keys=True).encode()).hexdigest())
    manifest_path.write_text(json.dumps(manifest, indent=2) + '\n', encoding='utf-8')
    print(f'Isolated source snapshot: {len(files)} files; ignored runtime/backup directories excluded.')


class Runner:
    def __init__(self, options):
        self.options = options
        self.started = time.monotonic()
        assert_no_reparse(ROOT / '.local-ci', ROOT)
        assert_no_reparse(ROOT / '.local-ci/results', ROOT)
        assert_no_reparse(ROOT / '.local-ci/go-cache', ROOT)
        assert_no_reparse(ROOT / '.local-ci/go-tmp', ROOT)
        stamp = datetime.now(timezone.utc).strftime('%Y%m%dT%H%M%S.%fZ')
        self.directory = ROOT / '.local-ci/results' / stamp
        self.directory.mkdir(parents=True)
        self.checks = []
        self.env = os.environ.copy()
        # Hooks export repository-local Git variables. Fixture repositories must
        # never inherit an absolute index/Git directory belonging to this checkout.
        for name in ('GIT_DIR', 'GIT_WORK_TREE', 'GIT_INDEX_FILE', 'GIT_COMMON_DIR',
                     'GIT_PREFIX', 'GIT_OBJECT_DIRECTORY', 'GIT_ALTERNATE_OBJECT_DIRECTORIES',
                     'GIT_CONFIG_PARAMETERS'):
            self.env.pop(name, None)
        # Go's VCS stamping invokes Git itself; scope the E: ownership allowance
        # to test/build children as well, without changing user/global config.
        config_count = int(self.env.get('GIT_CONFIG_COUNT', '0'))
        self.env[f'GIT_CONFIG_KEY_{config_count}'] = 'safe.directory'
        self.env[f'GIT_CONFIG_VALUE_{config_count}'] = ROOT.as_posix()
        self.env['GIT_CONFIG_COUNT'] = str(config_count + 1)
        self.env['GIT_OPTIONAL_LOCKS'] = '0'
        self.env.update(GOCACHE=str(ROOT / '.local-ci/go-cache'), GOPROXY='off', GOSUMDB='off',
                        GOTOOLCHAIN='local', GOFLAGS='-mod=readonly', PYTHONDONTWRITEBYTECODE='1')
        self.env['GOTMPDIR'] = str(ROOT / '.local-ci/go-tmp')
        Path(self.env['GOTMPDIR']).mkdir(parents=True, exist_ok=True)
        self.linux_temp = None
        self.windows_temp = None
        self.source_lock = None
        if options.linux:
            # DrvFS without metadata cannot enforce 0600/0700. Test credentials and
            # fake homes belong on Ubuntu's native filesystem, never /mnt/e.
            self.linux_temp = tempfile.TemporaryDirectory(prefix='havok-ci-', dir='/tmp')
            temp = Path(self.linux_temp.name)
        elif os.name == 'nt':
            self.windows_temp = tempfile.TemporaryDirectory(prefix='havok-ci-', dir=native_windows_temp())
            temp = Path(self.windows_temp.name)
            self.env['GOTMPDIR'] = str(temp / 'go-tmp')
            Path(self.env['GOTMPDIR']).mkdir()
        else:
            temp = self.directory / 'tmp'
            temp.mkdir()
        self.env.update(TMP=str(temp), TEMP=str(temp), TMPDIR=str(temp))
        for name in ('windows.log', 'linux.log', 'go-tests.log', 'build.log'):
            (self.directory / name).touch()

    def check(self, name, command, logfile='windows.log', skip=False, cwd=ROOT):
        started = time.monotonic()
        status, code = 'SKIPPED', None
        display = subprocess.list2cmdline([str(arg) for arg in command])
        if not skip:
            try:
                result = subprocess.run(command, cwd=cwd, env=self.env, capture_output=True,
                                        text=True, encoding='utf-8', errors='replace')
                code = result.returncode
                output = sanitize(result.stdout + result.stderr)
            except OSError as error:
                code, output = 1, f'Cannot start command: {type(error).__name__}\n'
            status = 'PASS' if code == 0 else 'FAIL'
            with (self.directory / logfile).open('a', encoding='utf-8') as log:
                log.write(f'\nDirectory: {cwd}\n$ {display}\n{output}\nExit: {code}\n')
            if code:
                print(f'  Failed command: {display}\n  Details: {self.directory / logfile}')
                locations = re.findall(r'[\w./\\-]+\.(?:go|py|ps1|sh):\d+(?::\d+)?', output)
                if locations:
                    print('  Locations: ' + ', '.join(dict.fromkeys(locations))[:300])
        self.checks.append(dict(name=name, result=status, exit_code=code, command=display,
                                duration_seconds=round(time.monotonic() - started, 3), log=logfile))
        print(f'{name:24} {status}', flush=True)
        return status == 'PASS'

    def finish(self):
        failed = [c['name'] for c in self.checks if c['result'] == 'FAIL']
        incomplete = any(c['result'] == 'SKIPPED' and c['name'] != 'Full Go Suite' for c in self.checks)
        final = 'FAIL' if failed else 'INCOMPLETE' if incomplete else 'PASS'
        report = dict(timestamp=datetime.now(timezone.utc).isoformat(), git_sha=git('rev-parse', 'HEAD').stdout.strip(),
                      branch=git('branch', '--show-current').stdout.strip(), dirty=bool(git('status', '--porcelain').stdout),
                      mode='linux' if self.options.linux else 'full' if self.options.full else 'fast' if self.options.fast else 'default',
                      operating_system=platform.platform(), wsl_distro='Ubuntu-24.04' if not self.options.no_wsl else None,
                      go_cache=self.env['GOCACHE'], checks=self.checks, duration_seconds=round(time.monotonic()-self.started, 3),
                      result=final, complete=not incomplete, exclusions=[c['name'] for c in self.checks if c['result']=='SKIPPED'])
        report['temporary_directory'] = self.env['TEMP']
        report['go_source'] = str(ROOT / '.local-ci/source') if self.source_lock else None
        if self.source_lock and (ROOT / '.local-ci/source/.havok-ci-manifest.json').is_file():
            manifest = (ROOT / '.local-ci/source/.havok-ci-manifest.json').read_text()
            (self.directory / 'source-manifest.json').write_text(manifest)
            report['source_digest'] = json.loads(manifest)['source_digest']
        (self.directory / 'summary.json').write_text(json.dumps(report, indent=2) + '\n', encoding='utf-8')
        markdown = '# HAVOK CI\n\n' + '\n'.join(f"- {c['name']}: {c['result']} ({c['duration_seconds']}s)" for c in self.checks)
        markdown += f'\n\nRESULT: {final}\n\nSHA: {report["git_sha"]}\n\nMode: {report["mode"]}; complete: {report["complete"]}\n'
        (self.directory / 'summary.md').write_text(markdown, encoding='utf-8')
        print(f'\nRESULT: {final}' + (' (partial: explicit exclusions)' if incomplete else ''))
        if failed:
            print('Failed: ' + ', '.join(failed))
        print(f'Duration: {int(report["duration_seconds"])//60:02}:{int(report["duration_seconds"])%60:02}\nReport: {self.directory}')
        if self.options.json_report:
            print(json.dumps(report, indent=2))
        if self.linux_temp:
            self.linux_temp.cleanup()
        if self.windows_temp:
            self.windows_temp.cleanup()
        if self.source_lock:
            self.source_lock.close()
            (ROOT / '.local-ci/source.lock').unlink()
        return 1 if failed else 2 if incomplete else 0


def main():
    parser = argparse.ArgumentParser()
    mode = parser.add_mutually_exclusive_group()
    mode.add_argument('--fast', action='store_true')
    mode.add_argument('--full', action='store_true')
    parser.add_argument('--no-build', action='store_true')
    parser.add_argument('--no-wsl', action='store_true')
    parser.add_argument('--json-report', action='store_true')
    parser.add_argument('--linux', action='store_true')
    parser.add_argument('--sanity', action='store_true')
    parser.add_argument('--linux-static', action='store_true')
    parser.add_argument('--prepare-source', type=Path)
    options = parser.parse_args()
    if options.sanity or options.linux_static or options.prepare_source:
        try:
            if options.prepare_source:
                prepare_source(options.prepare_source)
            else:
                sanity() if options.sanity else linux_static()
            return 0
        except ValueError as error:
            print(str(error))
            return 1
    try:
        runner = Runner(options)
    except ValueError as error:
        print(str(error))
        return 1
    py = sys.executable
    print('HAVOK CI\n', flush=True)
    runner.check('Repository', [py, __file__, '--sanity'])
    runner.check('Secret Safety', [py, 'ops/havok-fleet/check-secret-safety.py', '--staged'])
    runner.check('Fleet Unit Tests', [py, '-m', 'unittest', 'discover', '-s', 'tests/fleet', '-v'], 'linux.log' if options.linux else 'windows.log')
    runner.check('CI Self Tests', [py, '-m', 'unittest', 'discover', '-s', 'tests/local_ci', '-v'])
    runner.check('Upstream Sync Tests', [py, '-m', 'unittest', 'discover', '-s', 'tests/upstream_sync', '-v'])
    if options.linux:
        runner.check('POSIX / Bootstrap', [py, __file__, '--linux-static'], 'linux.log')
        return runner.finish()
    pwsh = shutil.which('pwsh') or 'pwsh'
    runner.check('PowerShell Syntax', [pwsh, '-NoProfile', '-File', 'tools/check-powershell.ps1'])
    runner.check('PowerShell Launchers', [pwsh, '-NoProfile', '-File', 'tests/fleet/test-powershell-launchers.ps1'])
    runner.check('Fleet DryRun', [pwsh, '-NoProfile', '-File', 'tests/local_ci/test-fleet-dry-runs.ps1'])
    runner.check('Herdr Proxy Runtime', [pwsh, '-NoProfile', '-File', 'tests/fleet/test-herdr-proxy.ps1'])
    source = ROOT / '.local-ci/source'
    can_go = all(c['result'] == 'PASS' for c in runner.checks if c['name'] in ('Repository', 'Secret Safety'))
    if can_go:
        try:
            runner.source_lock = (ROOT / '.local-ci/source.lock').open('x')
        except FileExistsError:
            can_go = False
            print('Another Go verification owns source.lock; refusing concurrent snapshot changes.')
    source_ready = runner.check('Go Source Snapshot', [py, __file__, '--prepare-source', str(source)], skip=not can_go)
    go = shutil.which('go') or r'C:\Program Files\Go\bin\go.exe'
    runner.check('Reset-Aware Tests', [go, 'test', '-buildvcs=false', '-count=1', '-p', '1', './sdk/cliproxy/auth', '-run', 'Test(ResetAware|Quota|CodexQuota)'], 'go-tests.log', not source_ready, source)
    runner.check('Claude Failover Tests', [go, 'test', '-buildvcs=false', '-count=1', '-p', '1', './sdk/cliproxy/auth', './sdk/cliproxy/session', './internal/runtime/executor', '-run', 'Test(Claude|MerklePrefixMatcherInvalidate|AuthManager_ConcurrentSuccess|PublishedAuthSnapshotRace)'], 'go-tests.log', not source_ready, source)
    runner.check('Go Focused Tests', [go, 'test', '-buildvcs=false', '-count=1', '-p', '1', './sdk/cliproxy/auth', './internal/config', './internal/api/handlers/management', './sdk/cliproxy'], 'go-tests.log', not source_ready, source)
    runner.check('Go Build', [go, 'build', '-buildvcs=false', '-o', str(runner.directory / 'cli-proxy-api.exe'), './cmd/server'], 'build.log', options.no_build or not source_ready, source)
    if not options.no_wsl:
        try:
            translated = subprocess.run(wsl_exec('wslpath', '-a', '-u', str(ROOT)), capture_output=True, text=True)
            expected_path = '/mnt/' + ROOT.drive[0].lower() + '/' + ROOT.relative_to(ROOT.anchor).as_posix()
            if translated.returncode or translated.stdout.strip() != expected_path:
                raise OSError('WSL path translation failed')
            linux_path = translated.stdout.strip()
        except OSError:
            linux_path = '/__havok_ci_path_translation_failed__'
        command = wsl_exec('bash', '-c', 'cd -- "$1" && sh tools/havok-ci-linux.sh', 'havok-ci', linux_path)
    else:
        command = wsl_exec('sh', 'tools/havok-ci-linux.sh')
    runner.check('WSL Verification', command, 'linux.log', options.no_wsl)
    runner.check('Full Go Suite', [go, 'test', '-buildvcs=false', '-count=1', '-p', '1', './...'], 'go-tests.log', not options.full or not source_ready, source)
    return runner.finish()


if __name__ == '__main__':
    sys.exit(main())
