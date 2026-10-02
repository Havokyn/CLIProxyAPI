param()
$ErrorActionPreference = 'Stop'
$repo = Split-Path $PSScriptRoot -Parent
$hook = Join-Path $PSScriptRoot 'git-hooks/pre-push'
if (-not (Test-Path -LiteralPath $hook -PathType Leaf)) { throw 'Missing repo-local pre-push hook; installation refused.' }
function Invoke-RepoGit { & git -c "safe.directory=$($repo.Replace('\','/'))" -C $repo @args }
$previous = [string](Invoke-RepoGit config --local --get core.hooksPath)
if ($LASTEXITCODE -notin @(0,1)) { throw 'Cannot read repository hook configuration.' }
$effective = Invoke-RepoGit config --get core.hooksPath
$target = 'tools/git-hooks'
if ($effective -eq $target) { Write-Output 'Havok push protection already installed.'; return }
$hooksDirectory = Invoke-RepoGit rev-parse --git-path hooks
if (-not [IO.Path]::IsPathRooted($hooksDirectory)) { $hooksDirectory = Join-Path $repo $hooksDirectory }
$existingHooks = @(Get-ChildItem -LiteralPath $hooksDirectory -File -ErrorAction SilentlyContinue | Where-Object Name -NotLike '*.sample')
if ($effective -or $existingHooks.Count) {
    throw 'Existing hooks detected. Refusing to replace or bypass them. Review and combine hooks explicitly before installing.'
}
$statePath = Invoke-RepoGit rev-parse --git-path havok-hook-state.json
if (-not [IO.Path]::IsPathRooted($statePath)) { $statePath = Join-Path $repo $statePath }
if (Test-Path -LiteralPath $statePath) { throw 'Previous hook state exists; uninstall or inspect it first.' }
@{ previous = $previous; installed = $target } | ConvertTo-Json | Set-Content -LiteralPath $statePath
Invoke-RepoGit config --local core.hooksPath $target
if ($LASTEXITCODE) { throw 'Cannot install repository hooksPath.' }
Write-Output 'Installed repository-local pre-push: havok-ci -Fast. No global configuration changed.'
