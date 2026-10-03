# Havok upstream synchronization

This customized fork has production routing and operations behavior. **Do not use
GitHub's Sync fork button.** Do not replace or rebase published Havok main, force
push, discard dirty work, or push to upstream.

`origin/main` is Havok production. `upstream/main` is fetch-only with its push URL
set to `DISABLED`. Develop on `feat/*` or `fix/*`. Reconcile on isolated
`sync/upstream-YYYYMMDD-<sha>` branches, using a real merge to retain ancestry.

## Check

```powershell
.\tools\upstream-status.ps1
.\tools\upstream-status.ps1 -Json
```

Status verifies exact GitHub fetch/push identities, fetches both remotes, and reports
local main, production main, upstream SHA, merge base, ahead/behind, commit lists,
changed-file intersection and overlap risk. It never modifies branches, merges,
pushes, or changes configuration. File intersection is a review aid; independent
files can still change interacting behavior.

## Prepare

```powershell
.\tools\sync-upstream.ps1 -DryRun
.\tools\sync-upstream.ps1 -Prepare
# Only if explicitly repairing an enabled upstream push URL:
.\tools\sync-upstream.ps1 -Prepare -FixUpstreamPush
```

The default mode is dry run. Both modes require a clean checkout, exact fork
identity, disabled upstream pushes, local main equal to freshly fetched
origin/main, local CI tools, the preserved Claude baseline, and no unrelated active
integration. Historical worktrees without an active operation are preserved.
No automatic stash or destructive Git operation is used.

Prepare creates a worktree under `.worktrees/` from production main and merges the
pinned upstream SHA with `--no-ff --no-commit`. It deliberately leaves the merge
uncommitted for semantic review. Reports live under the gitignored
`.local-ci/upstream-sync/<timestamp>/` directory. A conflict returns exit 3 and
records file, both histories' intents, category, and recommended resolution.
Never select ours/theirs for an entire conflict set.

Inspect each upstream-only commit and every overlapping file. Preserve upstream
fixes and Havok quota freshness, longest reset window priority, floors, reserve,
manual exclusions, cooldown, healthy stickiness, exhausted-session failover,
provider isolation, reservation cleanup and no ambiguous replay. Adapt custom
behavior to upstream refactors. Never restore removed APIs merely to apply a patch.

Review routing/auth/session, executor, config, API, translators, plugin interfaces,
example config and docs. Commit the reviewed merge and any reconciliation changes
in the isolated worktree. Use process-scoped Git ownership allowances if needed:

```powershell
git -c safe.directory=E:/CLIProxyAPI/.worktrees/<name> -C <worktree> diff --check
git -c safe.directory=E:/CLIProxyAPI/.worktrees/<name> -C <worktree> commit
```

Record `decisions`, `remaining_risks`, `review_pass: true`, and `reviewed_tree`
(the exact `git rev-parse HEAD^{tree}` result) in that report's `status.json`.
Then run from the original repository:

```powershell
.\tools\sync-upstream.ps1 -Verify -Report <report-directory>
```

Verify runs **Fast**, then **Full**, each without exclusions, and checks their
actual complete, clean, exact-SHA reports. Both include build, secret scan and
reset-aware regressions. Full covers all Go packages. A named deterministic
Claude/session/executor gate inside the locked CI snapshot proves cold weekly exclusion, exhausted-affinity
failover, healthy stickiness, capacity rejection/refresh, namespace invalidation
and no ambiguous replay. Reports bind gates, candidate SHA/tree and binary hash.
Any candidate change invalidates previous evidence. A failed gate stops the process.
WSL checks use a disposable native Linux clone of the exact candidate, overlaid
from the hash-verified locked source manifest. This avoids Windows worktree pointer
incompatibility without modifying the original repository. Linux evidence records
the same SHA and source digest and is copied into the Windows CI report.
On large-cluster filesystems, set a process-scoped `HAVOK_CI_CACHE` to a dedicated
NTFS cache directory if generated cache files exhaust disk space. No global Go or
Git configuration is changed.

## Internal PR

```powershell
.\tools\sync-upstream.ps1 -CreatePR -Report <report-directory>
```

CreatePR rechecks safety, ancestry, current production main, exact candidate and
local CI evidence. It requires GitHub Actions to be disabled. It pushes only the
sync ref to the verified Havok origin with a regular push and creates the PR with
explicit `--repo Havokyn/CLIProxyAPI --base main`. It never deploys or merges main.
Install repository-local destination protection with
`.\tools\install-git-hooks.ps1`; this refuses replacement of unrelated hooks.
Upstream push URL and the destination-checking pre-push hook provide independent
guards. No global Git configuration is changed.

## Deployment, verification, merge

After semantic review and all local gates PASS, back up and hash the currently
running binary. Deploy the exact verified candidate reversibly using the existing
operator launch mechanism. Preserve configuration, credentials, OAuth, Tailscale,
Herdr, machine keys and Service Grants. If verification fails, restore the binary.

Verify port 8317, authenticated `/v1/models`, documented model count, management UI,
reset-aware diagnostics, Tailscale HTTP access, herdr-proxy doctor and both pools.
Use only one tiny provider-bound Claude request plus continuation and one tiny
Codex session plus continuation. Check current healthy rank-1 placement and
sticky continuation using sanitized identifiers. Never print credentials/emails.
Record live evidence in the local report and update the internal PR body.

Merge using a normal merge commit, preserving the upstream merge ancestry. Fetch
origin and fast-forward local main only while clean. Fetch upstream again and
check `git rev-list --count main..upstream/main`. If upstream advanced during
validation, report the new gap rather than claiming currentness.

Remove only the owned, clean, merged temporary worktree. Preserve the report and
rollback binary. Keep historical worktrees and branches unless separately reviewed
for cleanup. Main protection should block force pushes and deletion without cloud
status checks. PR-only rules can obstruct direct local maintenance and are an
operator choice, not a required automated gate.

## Tooling tests

```powershell
python -m unittest discover -s tests/upstream_sync -v
```

Tests use disposable local Git fixtures, not production failure experiments.
The local CI runners also execute these tests. Reports and Git errors avoid raw
remote diagnostics, credentials and account emails. Local reports are operator
evidence, not tamper-proof attestations; review must remain independent.
