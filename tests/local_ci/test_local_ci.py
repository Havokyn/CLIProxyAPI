"""Failure propagation, staged-secret and push-protection acceptance tests."""
import contextlib
import importlib.util
import io
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest
from unittest import mock
from types import SimpleNamespace

ROOT = Path(__file__).resolve().parents[2]


def load(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


ci = load('local_ci', ROOT / 'tools/local_ci.py')
scanner = load('scanner', ROOT / 'ops/havok-fleet/check-secret-safety.py')


def fixture_env():
    # Temporary Git repositories must not inherit the caller's hooks, templates,
    # private index, credential configuration, or command-line config overrides.
    env = {key: value for key, value in os.environ.items() if not key.startswith('GIT_')}
    env.update(GIT_CONFIG_GLOBAL=os.devnull, GIT_CONFIG_NOSYSTEM='1')
    return env


class LocalCITests(unittest.TestCase):
    @unittest.skipUnless(os.name == 'nt', 'Windows to WSL argument boundary')
    def test_wsl_preserves_path_and_rejects_missing_directory(self):
        translated = subprocess.run(ci.wsl_exec('wslpath', '-a', '-u', str(ROOT)),
                                    capture_output=True, text=True)
        self.assertEqual(translated.returncode, 0, translated.stderr)
        path = translated.stdout.strip()
        expected = '/mnt/' + ROOT.drive[0].lower() + '/' + ROOT.relative_to(ROOT.anchor).as_posix()
        self.assertEqual(path, expected)
        command = ci.wsl_exec('bash', '-c', 'cd -- "$1" && pwd', 'havok-ci', path)
        valid = subprocess.run(command, capture_output=True, text=True)
        self.assertEqual(valid.returncode, 0, valid.stderr)
        self.assertEqual(valid.stdout.strip(), path)
        command[-1] = '/__havok_ci_missing_directory_fixture__'
        invalid = subprocess.run(command, capture_output=True, text=True)
        self.assertNotEqual(invalid.returncode, 0)
        self.assertEqual(invalid.stdout.strip(), '')

    def test_generated_path_cannot_escape_boundary(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory) / 'repo'
            with self.assertRaises(ValueError):
                ci.assert_no_reparse(root / '..' / 'unrelated', root)

    @unittest.skipUnless(os.name == 'nt', 'Windows junction safety')
    def test_source_snapshot_refuses_junction_without_touching_target(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory) / 'repo'
            target = Path(directory) / 'unrelated'
            (root / '.local-ci').mkdir(parents=True)
            target.mkdir()
            marker = target / 'keep.txt'
            marker.write_text('preserve me')
            (target / '.havok-ci-manifest.json').write_text(json.dumps({'repository': root.as_posix(), 'files': {'keep.txt': 'fake'}}))
            source = root / '.local-ci/source'
            result = subprocess.run(['cmd.exe', '/d', '/c', 'mklink', '/J', str(source), str(target)], capture_output=True)
            self.assertEqual(result.returncode, 0)
            try:
                with mock.patch.object(ci, 'ROOT', root), self.assertRaises(ValueError):
                    ci.prepare_source(source)
                self.assertEqual(marker.read_text(), 'preserve me')
            finally:
                source.rmdir()  # Remove only the junction; preserve its target.

    def test_source_snapshot_excludes_backups_and_tracks_current_source(self):
        with tempfile.TemporaryDirectory() as directory, mock.patch.object(ci, 'ROOT', Path(directory)):
            root = Path(directory)
            (root / 'cmd').mkdir()
            (root / 'cmd/main.go').write_text('package main\n')
            (root / 'bin/backup').mkdir(parents=True)
            (root / 'bin/backup/broken.go').write_text('not valid Go\n')
            listed = SimpleNamespace(returncode=0, stdout='cmd/main.go\0')
            with mock.patch.object(ci, 'git', return_value=listed), contextlib.redirect_stdout(io.StringIO()):
                ci.prepare_source(root / '.local-ci/source')
                self.assertEqual((root / '.local-ci/source/cmd/main.go').read_text(), 'package main\n')
                self.assertFalse((root / '.local-ci/source/bin').exists())
                before = json.loads((root / '.local-ci/source/.havok-ci-manifest.json').read_text())['source_digest']
                (root / 'cmd/main.go').write_text('package changed\n')
                ci.prepare_source(root / '.local-ci/source')
                after = json.loads((root / '.local-ci/source/.havok-ci-manifest.json').read_text())['source_digest']
                self.assertNotEqual(before, after)
                listed.stdout = ''
                ci.prepare_source(root / '.local-ci/source')
                self.assertFalse((root / '.local-ci/source/cmd/main.go').exists())

    def test_failed_command_returns_nonzero_and_preserves_report(self):
        with tempfile.TemporaryDirectory() as directory, mock.patch.object(ci, 'ROOT', Path(directory)), \
                mock.patch.object(ci, 'sanitize', side_effect=lambda text: text), \
                mock.patch.object(ci, 'git', return_value=SimpleNamespace(stdout='fixture\n')), \
                contextlib.redirect_stdout(io.StringIO()):
            options = SimpleNamespace(linux=False, full=False, fast=True, no_wsl=False, json_report=False)
            runner = ci.Runner(options)
            runner.check('Safe Failure Fixture', [sys.executable, '-c', 'raise SystemExit(23)'])
            self.assertEqual(runner.finish(), 1)
            report = json.loads((runner.directory / 'summary.json').read_text())
            self.assertEqual(report['result'], 'FAIL')
            self.assertEqual(report['checks'][0]['exit_code'], 23)

    def test_log_redaction(self):
        value = 'sk-' + 'A9mQ7xL2pV4zN8cR6wK3hT1dF5sB'
        text = ci.sanitize('Authorization: ' + 'Bearer ' + value)
        self.assertNotIn(value, text)
        self.assertIn('redacted', text)
        self.assertNotIn(value, ci.sanitize('runtime error: ' + value))
        marker = '-' * 5
        pem = marker + 'BEGIN PRIVATE KEY' + marker + '\nprivate-body-fixture\n' + marker + 'END PRIVATE KEY' + marker
        self.assertNotIn('private-body-fixture', ci.sanitize(pem))

    def test_explicit_exclusion_is_not_a_successful_gate(self):
        with tempfile.TemporaryDirectory() as directory, mock.patch.object(ci, 'ROOT', Path(directory)), \
                mock.patch.object(ci, 'git', return_value=SimpleNamespace(stdout='fixture\n')), \
                contextlib.redirect_stdout(io.StringIO()):
            runner = ci.Runner(SimpleNamespace(linux=False, full=False, fast=True, no_wsl=True, json_report=False))
            runner.check('WSL Verification', ['fixture'], skip=True)
            self.assertEqual(runner.finish(), 2)
            self.assertEqual(json.loads((runner.directory / 'summary.json').read_text())['result'], 'INCOMPLETE')

    def test_hook_repository_environment_does_not_escape_into_fixtures(self):
        with tempfile.TemporaryDirectory() as directory, mock.patch.object(ci, 'ROOT', Path(directory)), \
                mock.patch.dict(os.environ, {'GIT_DIR': '/fixture/parent.git', 'GIT_INDEX_FILE': '/fixture/index',
                                             'GIT_CONFIG_PARAMETERS': "'core.hooksPath=parent-hooks'"}):
            runner = ci.Runner(SimpleNamespace(linux=False))
            self.assertNotIn('GIT_DIR', runner.env)
            self.assertNotIn('GIT_INDEX_FILE', runner.env)
            self.assertNotIn('GIT_CONFIG_PARAMETERS', runner.env)
            if runner.windows_temp:
                runner.windows_temp.cleanup()

    def test_fork_destination_allowlist(self):
        self.assertTrue(ci.FORK.fullmatch('https://github.com/Havokyn/CLIProxyAPI.git'))
        for url in ('https://github.com/router-for-me/CLIProxyAPI.git',
                    'https://github.com/Havokyn/CLIProxyAPI.git/evil',
                    'https://example.org/Havokyn/CLIProxyAPI.git'):
            self.assertFalse(ci.FORK.fullmatch(url))

    def test_staged_index_scanned_even_after_worktree_is_cleaned(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            subprocess.run(['git', 'init', '-q', str(root)], env=fixture_env(), check=True)
            path = root / 'fixture.py'
            value = 'A9mQ' + '7xL2pV4zN8cR6wK3hT1dF5sB'
            path.write_text('management_' + 'key="' + value + '"')
            subprocess.run(['git', '-c', f'safe.directory={root.as_posix()}', '-C', str(root), 'add', 'fixture.py'], env=fixture_env(), check=True)
            path.write_text('# harmless working copy\n')
            findings = scanner.staged_findings(root)
            self.assertTrue(findings)
            self.assertNotIn(value, repr(findings))
            pem = root / 'production.pem'
            pem.write_text('-' * 5 + 'BEGIN OPENSSH PRIVATE KEY' + '-' * 5)
            subprocess.run(['git', '-c', f'safe.directory={root.as_posix()}', '-C', str(root), 'add', 'production.pem'], env=fixture_env(), check=True)
            self.assertIn(('production.pem', 1, 'private-key'), scanner.staged_findings(root))

    def test_scanner_failure_exit_and_secret_free_diagnostics(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / 'tools').mkdir()
            value = 'A9mQ' + '7xL2pV4zN8cR6wK3hT1dF5sB'
            (root / 'tools/fixture.py').write_text('refresh_' + 'token="' + value + '"')
            result = subprocess.run([sys.executable, str(ROOT / 'ops/havok-fleet/check-secret-safety.py'),
                                     '--root', directory], capture_output=True, text=True)
            self.assertEqual(result.returncode, 1)
            self.assertNotIn(value, result.stdout + result.stderr)
            self.assertIn('fixture.py:1:', result.stdout)

    @unittest.skipUnless(sys.platform == 'linux', 'POSIX hook executable test')
    def test_pre_push_invokes_fast_and_propagates_failure(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            subprocess.run(['git', 'init', '-q', str(root)], env=fixture_env(), check=True)
            binary = root / 'fake-bin'
            binary.mkdir()
            runner = binary / 'powershell.exe'
            runner.write_text('#!/bin/sh\nprintf "%s\\n" "$*" > "$HOOK_CAPTURE"\nexit "$HOOK_EXIT"\n')
            runner.chmod(0o700)
            capture = root / 'capture'
            env = dict(fixture_env(), PATH=str(binary) + ':' + os.environ['PATH'],
                       HOOK_CAPTURE=str(capture), HOOK_EXIT='0')
            command = ['git', '-c', f'safe.directory={root.as_posix()}', '-c',
                       f'core.hooksPath={ROOT / "tools/git-hooks"}', 'hook', 'run', 'pre-push', '--', 'origin',
                       'https://github.com/Havokyn/CLIProxyAPI.git']
            result = subprocess.run(command, cwd=root, env=env, capture_output=True)
            self.assertEqual(result.returncode, 0)
            self.assertIn('-Fast', capture.read_text())
            env['HOOK_EXIT'] = '19'
            self.assertEqual(subprocess.run(command, cwd=root, env=env, capture_output=True).returncode, 19)
            capture.unlink()
            command[-1] = 'https://github.com/router-for-me/CLIProxyAPI.git'
            self.assertNotEqual(subprocess.run(command, cwd=root, env=env, capture_output=True).returncode, 0)
            self.assertFalse(capture.exists())

    @unittest.skipUnless(os.name == 'nt', 'Windows hook installer test')
    def test_hook_install_uninstall_and_existing_hook_refusal(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            subprocess.run(['git', 'init', '-q', str(root)], env=fixture_env(), check=True)
            (root / 'tools').mkdir()
            (root / 'tools/git-hooks').mkdir()
            shutil.copyfile(ROOT / 'tools/git-hooks/pre-push', root / 'tools/git-hooks/pre-push')
            for name in ('install-git-hooks.ps1', 'uninstall-git-hooks.ps1'):
                shutil.copyfile(ROOT / 'tools' / name, root / 'tools' / name)
            def invoke(name):
                return subprocess.run(['powershell.exe', '-NoProfile', '-ExecutionPolicy', 'Bypass',
                                       '-File', str(root / 'tools' / name)], env=fixture_env(), capture_output=True)
            self.assertEqual(invoke('install-git-hooks.ps1').returncode, 0)
            self.assertEqual(invoke('uninstall-git-hooks.ps1').returncode, 0)
            configured = subprocess.run(['git', '-c', f'safe.directory={root.as_posix()}', '-C', str(root),
                                         'config', '--local', '--get', 'core.hooksPath'], env=fixture_env(), capture_output=True)
            self.assertEqual(configured.returncode, 1)
            hook = root / '.git/hooks/pre-push'
            hook.write_text('# existing user hook\n')
            self.assertNotEqual(invoke('install-git-hooks.ps1').returncode, 0)
            self.assertEqual(hook.read_text(), '# existing user hook\n')
            hook.rename(root / '.git/hooks/pre-commit')
            self.assertNotEqual(invoke('install-git-hooks.ps1').returncode, 0)


if __name__ == '__main__':
    unittest.main()
