param(
    [switch]$DryRun, [switch]$Prepare, [switch]$Verify, [switch]$CreatePR,
    [switch]$FixUpstreamPush, [string]$Report, [switch]$Json
)
$ErrorActionPreference = 'Stop'
$modes = @($DryRun, $Prepare, $Verify, $CreatePR) | Where-Object { $_ }
if ($modes.Count -gt 1) { throw 'Choose one sync mode.' }
$mode = if ($Prepare) { 'prepare' } elseif ($Verify) { 'verify' } elseif ($CreatePR) { 'pr' } else { 'dry-run' }
$arguments = @((Join-Path $PSScriptRoot 'upstream_sync.py'), $mode)
if ($FixUpstreamPush) { $arguments += '--fix-upstream-push' }
if ($Report) { $arguments += @('--report', $Report) }
if ($Json) { $arguments += '--json' }
& python @arguments
exit $LASTEXITCODE
