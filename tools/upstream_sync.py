"""Fetch-only status and isolated, locally verified Havok upstream integration.

No main updates, deployment, automatic conflict resolution, or PR merges.
Only standard-library dependencies; GitHub CLI is needed only for PR creation.
"""
from __future__ import annotations
import argparse
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[1]
FORK = 'Havokyn/CLIProxyAPI'
UPSTREAM = 'router-for-me/CLIProxyAPI'
BASELINE = '6278926d661aa74a15b4021693eca29ed91b556a'
BRANCH = re.compile(r'^sync/upstream-\d{8}-[0-9a-f]{8,40}$')


class Refusal(RuntimeError):
    pass


def command(args, cwd, *, allow_failure=False):
    env = os.environ.copy()
    for key in ('GIT_DIR', 'GIT_WORK_TREE', 'GIT_INDEX_FILE', 'GIT_COMMON_DIR',
                'GIT_PREFIX', 'GIT_OBJECT_DIRECTORY', 'GIT_ALTERNATE_OBJECT_DIRECTORIES',
                'GIT_CONFIG_PARAMETERS'):
        env.pop(key, None)
    result = subprocess.run(args, cwd=cwd, env=env, capture_output=True,
                            text=True, encoding='utf-8', errors='replace')
    if result.returncode and not allow_failure:
        # Never relay raw Git/network output, which can contain private URLs.
        raise Refusal(f'{Path(args[0]).name} operation failed (exit {result.returncode}).')
    return result


def git(root, *args, allow_failure=False):
    return command(['git', '-c', f'safe.directory={root.as_posix()}', '-C', str(root), *args],
                   root, allow_failure=allow_failure)


def value(root, *args):
    return git(root, *args).stdout.strip()


def matches_url(url, repository):
    return bool(re.fullmatch(r'(?:https://github\.com/|git@github\.com:|ssh://git@github\.com/)' +
                             re.escape(repository) + r'(?:\.git)?', url, re.I))


def identity(root, fix=False):
    if Path(value(root, 'rev-parse', '--show-toplevel')).resolve() != root.resolve():
        raise Refusal('Run from the repository root.')
    for push in (False, True):
        urls = value(root, 'remote', 'get-url', '--all', *(['--push'] if push else []), 'origin').splitlines()
        if not urls or not all(matches_url(url, FORK) for url in urls):
            raise Refusal('origin fetch/push must identify Havokyn/CLIProxyAPI.')
    urls = value(root, 'remote', 'get-url', '--all', 'upstream').splitlines()
    if not urls or not all(matches_url(url, UPSTREAM) for url in urls):
        raise Refusal('upstream fetch must identify router-for-me/CLIProxyAPI.')
    disabled = value(root, 'remote', 'get-url', '--all', '--push', 'upstream').splitlines() == ['DISABLED']
    if not disabled and fix:
        git(root, 'config', '--local', '--replace-all', 'remote.upstream.pushurl', 'DISABLED')
        disabled = value(root, 'remote', 'get-url', '--all', '--push', 'upstream') == 'DISABLED'
    if not disabled:
        raise Refusal('upstream push must be DISABLED. Use -FixUpstreamPush explicitly to repair it locally.')


def clean(root):
    if value(root, 'status', '--porcelain', '--untracked-files=all'):
        raise Refusal('Working tree is dirty. Commit or move work yourself; no automatic stash.')


def operation_check(root, owned=None):
    for item in git(root, 'worktree', 'list', '--porcelain').stdout.split('\n\n'):
        lines = item.splitlines()
        if not lines:
            continue
        path = Path(lines[0].removeprefix('worktree '))
        branch = next((line.removeprefix('branch refs/heads/') for line in lines if line.startswith('branch ')), '')
        if branch.startswith('sync/upstream-') and (owned is None or path.resolve() != owned.resolve()):
            raise Refusal('Another sync worktree is present. Finish or inspect that operation first.')
        for marker in ('MERGE_HEAD', 'CHERRY_PICK_HEAD', 'REVERT_HEAD', 'rebase-merge', 'rebase-apply'):
            marker_path = Path(value(path, 'rev-parse', '--git-path', marker))
            if not marker_path.is_absolute():
                marker_path = path / marker_path
            if marker_path.exists():
                raise Refusal('An existing Git integration operation is active; inspect it first.')


def category(path):
    if path.startswith(('sdk/cliproxy/auth/', 'sdk/cliproxy/session/', 'sdk/cliproxy/service')):
        return 'auth/routing', 'HIGH'
    if 'claude' in path.lower():
        return 'Claude', 'HIGH'
    if 'codex' in path.lower():
        return 'Codex', 'HIGH'
    if path.startswith('internal/translator/'):
        return 'translators', 'MEDIUM'
    if path.startswith(('internal/runtime/executor/', 'internal/config/', 'internal/api/',
                        'sdk/api/handlers/', 'sdk/pluginapi/', 'sdk/pluginabi/')) or path == 'config.example.yaml':
        return 'runtime/config/API', 'HIGH'
    return ('docs', 'LOW') if path.startswith('docs/') else ('other', 'MEDIUM')


def commits(root, revision):
    # Subjects and source paths are sanitized before any report is written.
    return [dict(sha=line.split(' ', 1)[0], subject=line.split(' ', 1)[1])
            for line in value(root, 'log', '--format=%H %s', revision).splitlines() if line]


def safe_text(text):
    text = re.sub(r'\b[^\s<>@]+@[^\s<>@]+\.[^\s<>@]+', '[email-redacted]', text)
    text = re.sub(r'(https?://)[^\s/]+@', r'\1[redacted]@', text)
    text = re.sub(r'\b(?:sk-[\w-]{20,}|ghp_\w{20,}|github_pat_\w{20,}|eyJ[\w.-]{30,})', '[redacted]', text)
    return text


def status(root, fetch=True):
    identity(root)
    if fetch:
        git(root, 'fetch', 'origin')
        git(root, 'fetch', 'upstream')
    havok = value(root, 'rev-parse', 'origin/main')
    upstream = value(root, 'rev-parse', 'upstream/main')
    base = value(root, 'merge-base', havok, upstream)
    ahead, behind = map(int, value(root, 'rev-list', '--left-right', '--count', f'{havok}...{upstream}').split())
    def paths(head):
        return set(filter(None, git(root, 'diff', '--name-only', '-z', base, head).stdout.split('\0')))
    left, right = paths(havok), paths(upstream)
    overlap = []
    for path in sorted(left & right):
        area, risk = category(path)
        overlap.append(dict(file=path, category=area, risk=risk,
                            upstream_intent=git(root, 'log', '--format=%h %s', f'{havok}..{upstream}', '--', path).stdout.strip(),
                            havok_intent=git(root, 'log', '--format=%h %s', f'{upstream}..{havok}', '--', path).stdout.strip(),
                            recommended_resolution='Inspect both patches and retain upstream fixes plus Havok semantics.'))
    return dict(schema_version=1, local_main_sha=value(root, 'rev-parse', 'main'), havok_sha=havok,
                upstream_sha=upstream, merge_base=base, ahead=ahead, behind=behind,
                status='UPDATE AVAILABLE' if behind else 'CURRENT',
                upstream_commits=commits(root, f'{havok}..{upstream}'),
                havok_commits=commits(root, f'{upstream}..{havok}'),
                overlap=overlap, upstream_changed_files=sorted(right), havok_changed_files=sorted(left),
                recommended_branch=f'sync/upstream-{datetime.now(timezone.utc):%Y%m%d}-{upstream[:8]}')


def prerequisites(root, fix=False, owned=None):
    clean(root)
    identity(root, fix)
    operation_check(root, owned)
    for relative in ('tools/havok-ci.ps1', 'tools/local_ci.py', 'ops/havok-fleet/check-secret-safety.py'):
        if not (root / relative).is_file():
            raise Refusal('Required local CI tooling is missing.')
    git(root, 'fetch', 'origin')
    git(root, 'fetch', 'upstream')
    if value(root, 'rev-parse', 'main') != value(root, 'rev-parse', 'origin/main'):
        raise Refusal('Local main differs from origin/main. Reconcile main explicitly before syncing.')
    if git(root, 'merge-base', '--is-ancestor', BASELINE, 'origin/main', allow_failure=True).returncode:
        raise Refusal('Havok main does not contain the required Claude failover baseline.')


def contained(path, boundary):
    path = Path(os.path.abspath(path))
    boundary = Path(os.path.abspath(boundary))
    if not path.is_relative_to(boundary):
        raise Refusal('Generated path escapes its boundary.')
    current = path
    while current != boundary.parent:
        if current.exists() and (current.is_symlink() or getattr(current.lstat(), 'st_file_attributes', 0) & 0x400):
            raise Refusal('Generated path contains a symlink/junction/reparse point.')
        if current == boundary:
            return path
        current = current.parent
    raise Refusal('Invalid path boundary.')


def write_report(directory, data):
    contained(directory, ROOT / '.local-ci/upstream-sync')
    directory.mkdir(parents=True, exist_ok=True)
    text = safe_text(json.dumps(data, indent=2)) + '\n'
    temp = directory / 'status.tmp'
    temp.write_text(text, encoding='utf-8')
    temp.replace(directory / 'status.json')
    summary = '# Havok upstream reconciliation\n\n' + '\n'.join(
        f'- {key}: {data.get(key, "NOT_RUN")}' for key in
        ('state', 'havok_sha', 'upstream_sha', 'merge_base', 'ahead', 'behind', 'branch', 'candidate_sha', 'candidate_tree'))
    summary += '\n\n## Gates\n\n' + json.dumps(data.get('gates', {}), indent=2)
    summary += '\n\n## Overlap / conflict review\n\n' + json.dumps(data.get('overlap', []), indent=2)
    summary += '\n\n## Decisions\n\n' + json.dumps(data.get('decisions', []), indent=2)
    summary += '\n\n## Risks\n\n' + '\n'.join(data.get('remaining_risks', [])) + '\n'
    (directory / 'summary.md').write_text(safe_text(summary), encoding='utf-8')


def prepare(root, data):
    if not data['behind']:
        return data
    branch = data['recommended_branch']
    if git(root, 'show-ref', '--verify', 'refs/heads/' + branch, allow_failure=True).returncode == 0:
        raise Refusal('Sync branch already exists. Resume using its report, not a new preparation.')
    stamp = datetime.now(timezone.utc).strftime('%Y%m%dT%H%M%S.%fZ')
    directory = contained(root / '.local-ci/upstream-sync' / stamp, root / '.local-ci/upstream-sync')
    worktree = contained(root / '.worktrees' / branch.split('/')[-1], root / '.worktrees')
    data.update(state='PREPARING', branch=branch, worktree=str(worktree), repository=str(root),
                report=str(directory), gates={}, decisions=[], conflicting_files=[],
                remaining_risks=['Semantic review, local gates and live verification pending.'])
    write_report(directory, data)
    git(root, 'worktree', 'add', '-b', branch, str(worktree), data['havok_sha'])
    result = git(worktree, 'merge', '--no-ff', '--no-commit', data['upstream_sha'], allow_failure=True)
    conflicts = list(filter(None, git(worktree, 'diff', '--name-only', '--diff-filter=U', '-z').stdout.split('\0')))
    data['conflicting_files'] = conflicts
    data['state'] = 'CONFLICTS' if conflicts else 'MERGE_PENDING' if result.returncode == 0 else 'MERGE_FAILED'
    for path in conflicts:
        if not any(item['file'] == path for item in data['overlap']):
            area, risk = category(path)
            data['overlap'].append(dict(file=path, category=area, risk=risk,
                                        upstream_intent=git(worktree, 'log', '--format=%h %s',
                                                            f"{data['havok_sha']}..{data['upstream_sha']}", '--', path).stdout.strip(),
                                        havok_intent=git(worktree, 'log', '--format=%h %s',
                                                        f"{data['upstream_sha']}..{data['havok_sha']}", '--', path).stdout.strip(),
                                        recommended_resolution='Inspect both histories and resolve semantically; never choose an entire side blindly.'))
    write_report(directory, data)
    return data


def load_report(root, report):
    if not report:
        candidates = list((root / '.local-ci/upstream-sync').glob('*/status.json'))
        if len(candidates) != 1:
            raise Refusal('Specify -Report pointing to the exact sync report directory.')
        report = candidates[0].parent
    directory = contained(Path(report).resolve(), root / '.local-ci/upstream-sync')
    data = json.loads((directory / 'status.json').read_text(encoding='utf-8'))
    if data.get('repository') != str(root) or data.get('report') != str(directory) or not BRANCH.fullmatch(data.get('branch', '')):
        raise Refusal('Report ownership/branch does not match this repository.')
    worktree = contained(Path(data['worktree']), root / '.worktrees')
    if value(worktree, 'branch', '--show-current') != data['branch']:
        raise Refusal('Worktree branch does not match report.')
    return directory, data, worktree


def candidate(worktree, data):
    clean(worktree)
    identity(worktree)
    for sha in (data['havok_sha'], data['upstream_sha'], BASELINE):
        if git(worktree, 'merge-base', '--is-ancestor', sha, 'HEAD', allow_failure=True).returncode:
            raise Refusal('Candidate ancestry does not preserve Havok/upstream/baseline.')
    sha, tree = value(worktree, 'rev-parse', 'HEAD'), value(worktree, 'rev-parse', 'HEAD^{tree}')
    return sha, tree


def verify(root, directory, data, worktree):
    prerequisites(root, owned=worktree)
    sha, tree = candidate(worktree, data)
    if not data.get('review_pass') or data.get('reviewed_tree') != tree:
        raise Refusal('Set review_pass=true, reviewed_tree=<candidate tree> and decisions after semantic review in status.json.')
    data.update(state='VERIFYING', candidate_sha=sha, candidate_tree=tree, gates={})
    write_report(directory, data)
    runner = shutil.which('pwsh') or shutil.which('powershell')
    if not runner:
        raise Refusal('PowerShell is required for local CI.')
    for mode in ('Fast', 'Full'):
        result = command([runner, '-NoProfile', '-File', str(worktree / 'tools/havok-ci.ps1'), '-' + mode],
                         worktree, allow_failure=True)
        (directory / (mode.lower() + '.log')).write_text(safe_text(result.stdout + result.stderr), encoding='utf-8')
        summaries = sorted((worktree / '.local-ci/results').glob('*/summary.json'))
        ci = json.loads(summaries[-1].read_text()) if summaries else {}
        passed = (result.returncode == 0 and ci.get('result') == 'PASS' and ci.get('complete') is True
                  and ci.get('git_sha') == sha and ci.get('dirty') is False and ci.get('mode') == mode.lower())
        data['gates'][mode.lower()] = dict(result='PASS' if passed else 'FAIL', sha=sha,
                                            evidence=str(summaries[-1]) if summaries else None)
        if passed:
            for name, key in (('Go Build', 'build'), ('Reset-Aware Tests', 'reset_aware'), ('Secret Safety', 'secret_scan')):
                checks = [item for item in ci['checks'] if item['name'] == name]
                data['gates'][key] = dict(result=checks[0]['result'] if checks else 'FAIL', sha=sha)
            build = summaries[-1].parent / 'cli-proxy-api.exe'
            data['build_binary'] = str(build)
            data['build_sha256'] = hashlib.sha256(build.read_bytes()).hexdigest() if build.exists() else None
        write_report(directory, data)
        if not passed:
            raise Refusal(f'{mode} local CI failed/incomplete. Inspect sanitized sync log and CI summary.')
    git(worktree, 'diff', '--check')
    # Full CI includes executor, session and ambiguity tests. A named focused rerun
    # supplies explicit Claude fix proof instead of inferring it from a broad pass.
    go = shutil.which('go') or r'C:\Program Files\Go\bin\go.exe'
    source = worktree / '.local-ci/source'
    result = command([go, 'test', '-buildvcs=false', '-count=1', '-p', '1', './sdk/cliproxy/auth',
                      './sdk/cliproxy/session', './internal/runtime/executor', '-run',
                      'Test(Claude|MerklePrefixMatcherInvalidate|AuthManager_ConcurrentSuccess)'], source,
                     allow_failure=True)
    (directory / 'claude-regressions.log').write_text(safe_text(result.stdout + result.stderr), encoding='utf-8')
    data['gates']['claude_regressions'] = dict(result='PASS' if result.returncode == 0 else 'FAIL', sha=sha)
    end_sha, end_tree = candidate(worktree, data)
    if (end_sha, end_tree) != (sha, tree):
        raise Refusal('Candidate changed during verification. Rerun all gates.')
    data['state'] = 'PR_READY' if result.returncode == 0 else 'REGRESSION_FAILED'
    write_report(directory, data)
    if result.returncode:
        raise Refusal('Claude deterministic regression proof failed.')
    return data


def create_pr(root, directory, data, worktree):
    prerequisites(root, owned=worktree)
    sha, tree = candidate(worktree, data)
    if data.get('candidate_sha') != sha or data.get('candidate_tree') != tree or data.get('reviewed_tree') != tree or not data.get('review_pass'):
        raise Refusal('Candidate/review changed. Rerun verification.')
    for gate in ('fast', 'full', 'build', 'reset_aware', 'secret_scan', 'claude_regressions'):
        check = data.get('gates', {}).get(gate, {})
        if check.get('result') != 'PASS' or check.get('sha') != sha:
            raise Refusal('All exact-candidate local gates must PASS before pushing.')
    # Read the real reports again, not only the editable sync summary.
    for mode in ('fast', 'full'):
        ci = json.loads(Path(data['gates'][mode]['evidence']).read_text())
        if ci.get('git_sha') != sha or ci.get('dirty') or ci.get('result') != 'PASS' or not ci.get('complete') or ci.get('mode') != mode:
            raise Refusal('Local CI evidence is stale or incomplete.')
    if data['havok_sha'] != value(root, 'rev-parse', 'origin/main'):
        raise Refusal('origin/main advanced. Reconcile and reverify before creating a PR.')
    enabled = command(['gh', 'api', f'repos/{FORK}/actions/permissions', '--jq', '.enabled'], root).stdout.strip()
    if enabled != 'false':
        raise Refusal('GitHub Actions must remain disabled.')
    body = f"Incorporates {len(data['upstream_commits'])} upstream commits through {data['upstream_sha']}.\n\n"
    body += 'Preserves reset-aware Codex/Claude quota routing, healthy affinity, exhausted-account failover, reserve policy, fleet tooling, launchers and secret safety.\n\n'
    body += 'Upstream changes:\n' + '\n'.join(f"- {c['sha'][:8]} {c['subject']}" for c in data['upstream_commits'])
    body += '\n\nReconciliation decisions:\n' + '\n'.join('- ' + str(item) for item in data.get('decisions', []))
    body += f'\n\nCandidate: {sha}\nTree: {tree}\n\nFast PASS; Full PASS; build PASS; reset-aware PASS; Claude deterministic regressions PASS; secret scan PASS.\nNo GitHub Actions required. No upstream push. Live deployment proof is recorded separately before merge.\n'
    path = directory / 'pr-body.md'
    path.write_text(safe_text(body), encoding='utf-8')
    # Explicit destination and refspec; regular push rejects non-fast-forward updates.
    git(worktree, '-c', 'credential.helper=', '-c', 'credential.helper=!gh auth git-credential',
        'push', 'origin', f"HEAD:refs/heads/{data['branch']}")
    existing = command(['gh', 'pr', 'list', '--repo', FORK, '--head', data['branch'], '--base', 'main',
                        '--json', 'url'], root).stdout
    urls = json.loads(existing)
    if urls:
        url = urls[0]['url']
    else:
        url = command(['gh', 'pr', 'create', '--repo', FORK, '--base', 'main', '--head', data['branch'],
                       '--title', f"chore(upstream): sync CLIProxy upstream {data['upstream_sha'][:8]}",
                       '--body-file', str(path)], root).stdout.strip()
    if not url.startswith(f'https://github.com/{FORK}/pull/'):
        raise Refusal('Unexpected PR destination; inspect GitHub state.')
    data.update(pr_url=url, state='PR_CREATED')
    write_report(directory, data)
    return data


def display(data, as_json):
    if as_json:
        print(safe_text(json.dumps(data, indent=2)))
        return
    print('HAVOK CLIPROXY UPSTREAM STATUS\n')
    for label, key in (('Local main', 'local_main_sha'), ('Havok main', 'havok_sha'),
                       ('Upstream main', 'upstream_sha'), ('Merge base', 'merge_base'),
                       ('Ahead', 'ahead'), ('Behind', 'behind'), ('Status', 'status')):
        print(f'{label:16} {data[key]}')
    for side in ('upstream', 'havok'):
        print(f'\n{side.title()}-only commits:')
        for item in data[side + '_commits']:
            print(safe_text(f"  {item['sha'][:8]} {item['subject']}"))
    print('\nOverlap:')
    for item in data['overlap']:
        print(safe_text(f"  {item['category']:20} {item['risk']:6} {item['file']}"))
    if not data['overlap']:
        print('  No changed-file overlap; cross-file semantic review still required.')
    print(f"\nRecommended: {data['recommended_branch']}")
    if data.get('report'):
        print(f"State: {data['state']}\nReport: {data['report']}\nWorktree: {data['worktree']}")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('mode', choices=('status', 'dry-run', 'prepare', 'verify', 'pr'))
    parser.add_argument('--json', action='store_true')
    parser.add_argument('--fix-upstream-push', action='store_true')
    parser.add_argument('--report', type=Path)
    args = parser.parse_args()
    try:
        if args.mode == 'status':
            if args.fix_upstream_push:
                raise Refusal('Status never changes remote configuration.')
            data = status(ROOT)
        elif args.mode in ('dry-run', 'prepare'):
            if args.mode == 'dry-run' and args.fix_upstream_push:
                raise Refusal('Dry run does not modify configuration. Use -Prepare -FixUpstreamPush.')
            prerequisites(ROOT, args.fix_upstream_push)
            data = status(ROOT, fetch=False)
            if args.mode == 'prepare':
                data = prepare(ROOT, data)
        else:
            directory, data, worktree = load_report(ROOT, args.report)
            if args.fix_upstream_push:
                raise Refusal('Repair upstream push only during preparation.')
            data = verify(ROOT, directory, data, worktree) if args.mode == 'verify' else create_pr(ROOT, directory, data, worktree)
        display(data, args.json)
        return 3 if data.get('state') in ('CONFLICTS', 'MERGE_FAILED') else 0
    except (Refusal, OSError, ValueError, KeyError) as error:
        message = str(error) if isinstance(error, Refusal) else f'Cannot complete operation: {type(error).__name__}.'
        if args.json:
            print(json.dumps(dict(error=safe_text(message))))
        else:
            print('REFUSED: ' + safe_text(message), file=sys.stderr)
        return 1


if __name__ == '__main__':
    sys.exit(main())
