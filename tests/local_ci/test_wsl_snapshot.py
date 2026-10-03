"""Native Linux snapshot and output-containment tests, also run by WSL CI."""
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location('wsl_snapshot', ROOT / 'tools/wsl_ci_snapshot.py')
snapshot = importlib.util.module_from_spec(spec)
spec.loader.exec_module(snapshot)


class SnapshotTests(unittest.TestCase):
    def test_path_escape(self):
        with tempfile.TemporaryDirectory() as temporary:
            with self.assertRaises(RuntimeError):
                snapshot.bounded(Path(temporary).parent / 'escape', Path(temporary))

    @unittest.skipIf(os.name == 'nt', 'Native Linux clone exercised by WSL')
    def test_clone_preserves_exact_candidate_and_manifest(self):
        self.exercise()

    @unittest.skipIf(os.name == 'nt', 'Native Linux clone exercised by WSL')
    def test_manifest_digest_tamper_refused(self):
        self.exercise(tamper=True)

    @unittest.skipIf(os.name == 'nt', 'Native Linux CRLF overlay fixture')
    def test_crlf_overlay_has_same_tree_and_clean_index(self):
        self.exercise(crlf=True)

    @unittest.skipIf(os.name == 'nt', 'Native Linux modified source fixture')
    def test_modified_source_cannot_claim_exact_candidate(self):
        self.exercise(modified=True)

    @unittest.skipIf(os.name == 'nt', 'Native Linux link fixture')
    def test_dangling_output_refused(self):
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / 'report'
            outside = Path(temporary).parent / ('missing-' + path.parent.name)
            path.symlink_to(outside)
            with self.assertRaises(RuntimeError):
                snapshot.bounded(path, Path(temporary))
            self.assertFalse(outside.exists())

    def exercise(self, tamper=False, crlf=False, modified=False):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary) / 'repository'
            root.mkdir()
            env = {key:value for key,value in os.environ.items() if not key.startswith('GIT_')}
            env.update(GIT_CONFIG_GLOBAL=os.devnull, GIT_CONFIG_NOSYSTEM='1')
            def git(*args):
                return subprocess.check_output(['git', '-c', 'core.hooksPath=/dev/null', *args], cwd=root, env=env, text=True).strip()
            git('init', '-b', 'main')
            git('config', 'user.name', 'Fixture')
            git('config', 'user.email', 'fixture@example.invalid')
            (root / '.gitignore').write_text('.local-ci/\n')
            (root / 'tools').mkdir()
            runner = '''import json, pathlib, subprocess
p=pathlib.Path('.local-ci/results/run');p.mkdir(parents=True)
sha=subprocess.check_output(['git','rev-parse','HEAD'],text=True).strip()
dirty=bool(subprocess.check_output(['git','status','--porcelain'],text=True).strip())
(p/'summary.json').write_text(json.dumps(dict(git_sha=sha,result='PASS',complete=True,mode='linux',dirty=dirty)))
(p/'windows.log').write_text('sanitized fixture log')
(p/'linux.log').write_text('sanitized fixture log')
'''
            (root / 'tools/local_ci.py').write_text(runner)
            git('add', '.')
            git('commit', '-m', 'fixture')
            sha = git('rev-parse', 'HEAD')
            source = root / '.local-ci/source'
            source.mkdir(parents=True)
            files = {}
            for relative in ('.gitignore', 'tools/local_ci.py'):
                target = source / relative
                target.parent.mkdir(parents=True, exist_ok=True)
                data = (root / relative).read_bytes()
                if crlf:
                    data = data.replace(b'\n', b'\r\n')
                if modified and relative == 'tools/local_ci.py':
                    data += b'\n# source differs from the candidate\n'
                target.write_bytes(data)
                files[relative] = hashlib.sha256(data).hexdigest()
            manifest = dict(files=files,source_digest=hashlib.sha256(json.dumps(files,sort_keys=True).encode()).hexdigest())
            if tamper:
                manifest['source_digest']='incorrect'
            (source / '.havok-ci-manifest.json').write_text(json.dumps(manifest))
            destination = root / '.local-ci/results/outer'
            destination.mkdir(parents=True)
            with patch('sys.argv', ['snapshot', str(root/'.git'), str(source), sha, str(destination)]):
                if tamper or modified:
                    with self.assertRaises(RuntimeError):
                        snapshot.main()
                else:
                    self.assertEqual(snapshot.main(), 0)
                    report=json.loads((destination/'wsl-summary.json').read_text())
                    self.assertEqual(report['source_sha'],sha)
                    self.assertEqual(report['parent_source_digest'],manifest['source_digest'])
            self.assertEqual(git('rev-parse','HEAD'),sha)
            self.assertFalse(git('status','--porcelain'))
