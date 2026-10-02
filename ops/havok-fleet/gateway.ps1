[CmdletBinding()]
param(
    [Parameter(Mandatory)][ValidateSet('start', 'status', 'stop')][string]$Action,
    [Parameter(Mandatory)][string]$Executable,
    [string]$ConfigPath,
    [string]$StatePath = '.local/gateway-process.json',
    [ValidateRange(1, 65535)][int]$Port = 8317,
    [switch]$DryRun
)
$ErrorActionPreference = 'Stop'
if ($DryRun) { Write-Output "Plan: gateway $Action using explicit executable and local PID record. No changes."; return }
$exePath = (Resolve-Path -LiteralPath $Executable).Path
$state = if (Test-Path -LiteralPath $StatePath) { Get-Content -LiteralPath $StatePath -Raw | ConvertFrom-Json } else { $null }
$process = if ($state) { Get-Process -Id $state.pid -ErrorAction SilentlyContinue } else { $null }
$owned = $process -and $process.Path -eq $exePath -and $process.StartTime.ToUniversalTime().ToString('o') -eq $state.started_at
if ($Action -eq 'status') { [pscustomobject]@{ owned_process_running = [bool]$owned; listener = [bool](Get-NetTCPConnection -LocalPort $Port -State Listen -ErrorAction SilentlyContinue) }; return }
if ($Action -eq 'stop') {
    if ($owned) { Stop-Process -Id $process.Id; Remove-Item -LiteralPath $StatePath; Write-Output 'Owned gateway process stopped.' }
    else { throw 'No matching owned process; nothing stopped.' }
    return
}
if ($owned) { Write-Output 'Owned gateway already running.'; return }
if (Get-NetTCPConnection -LocalPort $Port -State Listen -ErrorAction SilentlyContinue) { throw 'Port already occupied; no process changed.' }
if (-not $ConfigPath) { throw 'ConfigPath is required to start.' }
$config = (Resolve-Path -LiteralPath $ConfigPath).Path
# Conservative guard: operators must explicitly bind the gateway to loopback.
$configText = Get-Content -LiteralPath $config -Raw
if ($configText -notmatch '(?m)^host:\s*["'']?(127\.0\.0\.1|::1)["'']?\s*(?:#.*)?$') { throw 'Config must explicitly set host to a loopback address.' }
$child = Start-Process -FilePath $exePath -ArgumentList @('--config', ('"' + $config + '"')) -WorkingDirectory (Split-Path -Parent $config) -WindowStyle Hidden -PassThru
$parent = Split-Path -Parent ([IO.Path]::GetFullPath($StatePath))
$null = New-Item -ItemType Directory -Force -Path $parent
@{ pid = $child.Id; started_at = $child.StartTime.ToUniversalTime().ToString('o') } | ConvertTo-Json | Set-Content -LiteralPath $StatePath
Write-Output 'Gateway process started; run verify-fleet.ps1 to check readiness.'
