param()
$ErrorActionPreference = 'Stop'
$repo = Split-Path $PSScriptRoot -Parent
function Invoke-RepoGit { & git -c "safe.directory=$($repo.Replace('\','/'))" -C $repo @args }
$statePath = Invoke-RepoGit rev-parse --git-path havok-hook-state.json
if (-not [IO.Path]::IsPathRooted($statePath)) { $statePath = Join-Path $repo $statePath }
if (-not (Test-Path -LiteralPath $statePath)) { throw 'No Havok hook installation state; refusing to change hooks.' }
$state = Get-Content -LiteralPath $statePath -Raw | ConvertFrom-Json
$current = Invoke-RepoGit config --local --get core.hooksPath
if ($current -ne $state.installed) { throw 'hooksPath changed since installation; refusing to overwrite it.' }
if ($state.previous) { Invoke-RepoGit config --local core.hooksPath $state.previous }
else { Invoke-RepoGit config --local --unset core.hooksPath }
if ($LASTEXITCODE) { throw 'Cannot restore repository hook configuration.' }
Remove-Item -LiteralPath $statePath
Write-Output 'Removed Havok push protection; restored prior repository hook configuration.'
