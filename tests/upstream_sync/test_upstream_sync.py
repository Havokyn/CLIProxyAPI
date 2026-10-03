"""Deterministic offline fixtures. No live remotes, hooks, GitHub, or credentials."""
import contextlib
import importlib.util
import io
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

SOURCE = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location('upstream_sync', SOURCE / 'tools/upstream_sync.py')
sync = importlib.util.module_from_spec(spec)
spec.loader.exec_module(sync)


class SyncTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name) / 'repo'
        self.root.mkdir()
        self.environment = patch.dict(os.environ, dict(GIT_CONFIG_GLOBAL=os.devnull, GIT_CONFIG_NOSYSTEM='1'))
        self.environment.start()
        self.addCleanup(self.environment.stop)
        self.run_git('init', '-b', 'main')
        self.run_git('config', 'user.name', 'Fixture')
        self.run_git('config', 'user.email', 'fixture@example.invalid')
        self.run_git('config', 'core.autocrlf', 'false')
        self.run_git('config', 'commit.gpgsign', 'false')
        self.run_git('config', 'core.hooksPath', str(self.root / 'no-hooks'))
        (self.root / '.gitignore').write_text('.local-ci/\n.worktrees/\n')
        for name in ('tools/havok-ci.ps1', 'tools/local_ci.py', 'ops/havok-fleet/check-secret-safety.py'):
            path = self.root / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text('# fixture\n')
        self.commit('base', 'shared.txt', 'base\n')
        self.base = sync.value(self.root, 'rev-parse', 'HEAD')
        self.run_git('remote', 'add', 'origin', 'https://github.com/Havokyn/CLIProxyAPI.git')
        self.run_git('remote', 'add', 'upstream', 'https://github.com/router-for-me/CLIProxyAPI.git')
        self.run_git('remote', 'set-url', '--push', 'upstream', 'DISABLED')
        self.run_git('update-ref', 'refs/remotes/origin/main', self.base)
        self.run_git('update-ref', 'refs/remotes/upstream/main', self.base)
        self.root_patch = patch.object(sync, 'ROOT', self.root)
        self.root_patch.start()
        self.addCleanup(self.root_patch.stop)

    def run_git(self, *args):
        return sync.git(self.root, *args)

    def commit(self, subject, name, text):
        path = self.root / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(text)
        self.run_git('add', '.')
        self.run_git('commit', '-m', subject)

    def upstream(self, conflict=False):
        self.run_git('switch', '-c', 'fixture-upstream', self.base)
        self.commit('upstream fix', 'shared.txt' if conflict else 'upstream.txt', 'upstream\n')
        sha = sync.value(self.root, 'rev-parse', 'HEAD')
        self.run_git('update-ref', 'refs/remotes/upstream/main', sha)
        self.run_git('switch', 'main')
        return sha

    def test_clean_repo_and_no_upstream_changes(self):
        sync.clean(self.root)
        sync.identity(self.root)
        data = sync.status(self.root, fetch=False)
        self.assertEqual((data['ahead'], data['behind'], data['status']), (0, 0, 'CURRENT'))
        self.assertIs(sync.prepare(self.root, data), data)
        self.assertFalse((self.root / '.worktrees').exists())

    def test_dirty_repo_refused(self):
        (self.root / 'untracked').write_text('work')
        with self.assertRaisesRegex(sync.Refusal, 'dirty'):
            sync.prerequisites(self.root)
        self.assertTrue((self.root / 'untracked').exists())

    def test_wrong_origin_refused(self):
        self.run_git('remote', 'set-url', 'origin', 'https://github.com/router-for-me/CLIProxyAPI.git')
        with self.assertRaisesRegex(sync.Refusal, 'origin'):
            sync.identity(self.root)

    def test_push_enabled_refusal_and_explicit_fix(self):
        self.run_git('config', '--unset-all', 'remote.upstream.pushurl')
        with self.assertRaisesRegex(sync.Refusal, 'DISABLED'):
            sync.identity(self.root)
        sync.identity(self.root, fix=True)
        self.assertEqual(sync.value(self.root, 'remote', 'get-url', '--push', 'upstream'), 'DISABLED')

    def test_multiple_push_urls_replaced(self):
        self.run_git('config', '--add', 'remote.upstream.pushurl', 'https://github.com/router-for-me/CLIProxyAPI.git')
        with self.assertRaises(sync.Refusal):
            sync.identity(self.root)
        sync.identity(self.root, fix=True)
        self.assertEqual(sync.value(self.root, 'remote', 'get-url', '--all', '--push', 'upstream'), 'DISABLED')

    def test_behind(self):
        self.upstream()
        data = sync.status(self.root, fetch=False)
        self.assertEqual((data['ahead'], data['behind']), (0, 1))
        self.assertEqual(len(data['upstream_commits']), 1)

    def test_diverged_overlap_and_branch_name(self):
        sha = self.upstream(conflict=True)
        self.commit('Havok fix', 'shared.txt', 'Havok\n')
        self.run_git('update-ref', 'refs/remotes/origin/main', 'HEAD')
        data = sync.status(self.root, fetch=False)
        self.assertEqual((data['ahead'], data['behind']), (1, 1))
        self.assertEqual(data['overlap'][0]['file'], 'shared.txt')
        self.assertTrue(sync.BRANCH.fullmatch(data['recommended_branch']))
        self.assertTrue(data['recommended_branch'].endswith(sha[:8]))

    def test_prepare_isolated_merge_and_report(self):
        self.upstream()
        data = sync.prepare(self.root, sync.status(self.root, fetch=False))
        self.assertEqual(data['state'], 'MERGE_PENDING')
        self.assertEqual(sync.value(self.root, 'rev-parse', 'main'), self.base)
        self.assertEqual(sync.value(Path(data['worktree']), 'rev-parse', 'HEAD'), self.base)
        self.assertTrue((Path(data['report']) / 'summary.md').is_file())
        recorded = json.loads((Path(data['report']) / 'status.json').read_text())
        self.assertEqual(recorded['upstream_sha'], data['upstream_sha'])

    def test_conflict_detection_preserves_main(self):
        self.upstream(conflict=True)
        self.commit('Havok', 'shared.txt', 'Havok\n')
        self.run_git('update-ref', 'refs/remotes/origin/main', 'HEAD')
        head = sync.value(self.root, 'rev-parse', 'HEAD')
        data = sync.prepare(self.root, sync.status(self.root, fetch=False))
        self.assertEqual(data['state'], 'CONFLICTS')
        self.assertEqual(data['conflicting_files'], ['shared.txt'])
        self.assertEqual(sync.value(self.root, 'rev-parse', 'main'), head)
        self.assertIn('upstream_intent', data['overlap'][0])

    def test_dry_run_json_does_not_change_branches(self):
        self.upstream()
        before = sync.value(self.root, 'show-ref')
        real_git = sync.git
        def offline_git(root, *args, **kwargs):
            if args[0] == 'fetch':
                return subprocess.CompletedProcess(args, 0, '', '')
            return real_git(root, *args, **kwargs)
        with patch.object(sync, 'BASELINE', self.base), patch.object(sync, 'git', offline_git), \
                patch('sys.argv', ['sync', 'dry-run', '--json']), contextlib.redirect_stdout(io.StringIO()) as output:
            self.assertEqual(sync.main(), 0)
        self.assertEqual(json.loads(output.getvalue())['behind'], 1)
        self.assertEqual(sync.value(self.root, 'show-ref'), before)
        self.assertFalse((self.root / '.worktrees').exists())

    def test_destination_validation(self):
        for url in ('https://github.com/Havokyn/CLIProxyAPI.git', 'git@github.com:Havokyn/CLIProxyAPI.git'):
            self.assertTrue(sync.matches_url(url, sync.FORK))
        for url in ('https://github.com/router-for-me/CLIProxyAPI.git',
                    'https://github.com/Havokyn/CLIProxyAPI.evil', 'https://github.com.evil/Havokyn/CLIProxyAPI.git'):
            self.assertFalse(sync.matches_url(url, sync.FORK))

    def test_generated_path_escape_refused(self):
        with self.assertRaises(sync.Refusal):
            sync.contained(self.root.parent / 'escape', self.root)

    def test_dangling_output_symlink_refused(self):
        path = self.root / 'output.txt'
        external = self.root.parent / 'must-not-create.txt'
        try:
            path.symlink_to(external)
        except OSError:
            self.skipTest('Symlink creation not permitted on this host')
        with self.assertRaises(sync.Refusal):
            sync.write_text(path, 'test')
        self.assertFalse(external.exists())
        path.unlink()

    def test_report_sanitization(self):
        directory = self.root / '.local-ci/upstream-sync/test'
        sync.write_report(directory, dict(decisions=['contact fixture@example.invalid'], gates={}))
        for path in directory.iterdir():
            self.assertNotIn('fixture@example.invalid', path.read_text())

    def test_no_destructive_or_force_operations(self):
        text = (SOURCE / 'tools/upstream_sync.py').read_text()
        for forbidden in ('--force', '--hard', "'clean'", "'rebase'", "'stash'", "'--ours'", "'--theirs'"):
            self.assertNotIn(forbidden, text)
        self.assertIn("'push', '--no-follow-tags', 'origin'", text)
        self.assertIn("'--repo', FORK", text)

    def test_actions_not_required_and_gate_binding(self):
        text = (SOURCE / 'tools/upstream_sync.py').read_text()
        self.assertIn("enabled != 'false'", text)
        self.assertNotIn('required_status_checks', text)
        self.assertIn("check.get('sha') != sha", text)
        self.assertIn("ci.get('dirty')", text)

    def test_fetch_ignores_dangerous_configured_refspec(self):
        remote = Path(self.temp.name) / 'remote'
        subprocess.run(['git', 'clone', '--bare', str(self.root), str(remote)], check=True, capture_output=True)
        self.upstream()
        self.run_git('push', str(remote), 'fixture-upstream:main')
        self.run_git('config', 'remote.upstream.fetch', '+refs/heads/main:refs/heads/main')
        self.run_git('remote', 'set-url', 'upstream', str(remote))
        self.run_git('switch', '-c', 'feature')
        before = sync.value(self.root, 'rev-parse', 'main')
        sync.fetch_main(self.root, 'upstream')
        self.assertEqual(sync.value(self.root, 'rev-parse', 'main'), before)
        self.assertNotEqual(sync.value(self.root, 'rev-parse', 'upstream/main'), before)

    def test_candidate_scan_includes_unscoped_files(self):
        self.commit('test-only credential fixture', 'sdk/unscoped.txt', 'api_' + 'key=' + 'abcdefghijk\n')
        # Use the real scanner while keeping fixture repositories offline.
        import shutil
        shutil.copyfile(SOURCE / 'ops/havok-fleet/check-secret-safety.py', self.root / 'ops/havok-fleet/check-secret-safety.py')
        self.run_git('add', '.')
        self.run_git('commit', '-m', 'scanner')
        findings, unresolved = sync.scan_candidate(self.root, dict(havok_sha=self.base))
        self.assertTrue(any(item['file'] == 'sdk/unscoped.txt' for item in unresolved))
        self.assertTrue(findings)

    def test_verify_selects_windows_report_and_binds_all_gates(self):
        data = dict(havok_sha=self.base, upstream_sha=self.base, review_pass=True,
                    reviewed_tree=sync.value(self.root, 'rev-parse', 'HEAD^{tree}'), branch='sync/upstream-20261003-12345678')
        directory = self.root / '.local-ci/upstream-sync/verify'
        sha = sync.value(self.root, 'rev-parse', 'HEAD')
        def fake_command(args, cwd, **kwargs):
            mode = args[-1][1:].lower()
            for stamp, report_mode in ((mode + '-1', mode), (mode + '-2', 'linux')):
                out = self.root / '.local-ci/results' / stamp
                out.mkdir(parents=True)
                ci = dict(mode=report_mode, result='PASS', complete=True, dirty=False, git_sha=sha,
                          checks=[dict(name=name, result='PASS') for name in
                                  ('Go Build', 'Reset-Aware Tests', 'Secret Safety', 'Claude Failover Tests')])
                (out / 'summary.json').write_text(json.dumps(ci))
                (out / 'cli-proxy-api.exe').write_bytes(b'fixture binary')
            return subprocess.CompletedProcess(args, 0, '', '')
        with patch.object(sync, 'prerequisites'), patch.object(sync, 'scan_candidate', return_value=([], [])), \
                patch.object(sync, 'command', side_effect=fake_command), patch.object(sync.shutil, 'which', return_value='pwsh'):
            # candidate() and diff use real Git; avoid intercepting their subprocesses.
            with patch.object(sync, 'candidate', return_value=(sha, data['reviewed_tree'])), patch.object(sync, 'git'):
                result = sync.verify(self.root, directory, data, self.root)
        self.assertEqual(result['state'], 'PR_READY')
        self.assertEqual(result['gates']['fast']['sha'], sha)
        self.assertEqual(result['gates']['claude_regressions']['result'], 'PASS')
        self.assertIn('fast-1', result['gates']['fast']['evidence'])

    def test_pr_rejects_stale_candidate_before_push(self):
        with patch.object(sync, 'prerequisites'), patch.object(sync, 'candidate', return_value=('new', 'new-tree')), \
                patch.object(sync, 'command') as command:
            with self.assertRaisesRegex(sync.Refusal, 'changed'):
                sync.create_pr(self.root, self.root, dict(candidate_sha='old'), self.root)
        command.assert_not_called()

    def test_pr_rejects_missing_full_gate_before_push(self):
        data = dict(candidate_sha='sha', candidate_tree='tree', reviewed_tree='tree', review_pass=True, gates={})
        with patch.object(sync, 'prerequisites'), patch.object(sync, 'candidate', return_value=('sha', 'tree')), \
                patch.object(sync, 'command') as command:
            with self.assertRaisesRegex(sync.Refusal, 'PASS'):
                sync.create_pr(self.root, self.root, data, self.root)
        command.assert_not_called()


if __name__ == '__main__':
    unittest.main()
