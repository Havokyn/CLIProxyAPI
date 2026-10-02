[CmdletBinding()]
param(
    [Parameter(Mandatory)][string]$FleetPath,
    [string]$OutputPath = '.local/fleet-inventory.json',
    [string]$LocalEndpoint = 'http://127.0.0.1:8317',
    [string]$LocalKeyFile,
    [string]$ManagementKeyFile,
    [string]$Model = 'gpt-6-sol',
    [switch]$SmokeTest,
    [switch]$DryRun
)
$ErrorActionPreference = 'Stop'
$arguments = @{ FleetPath = $FleetPath; LocalEndpoint = $LocalEndpoint; LocalKeyFile = $LocalKeyFile; ManagementKeyFile = $ManagementKeyFile; Model = $Model; SmokeTest = $SmokeTest; DryRun = $DryRun }
$inventory = & "$PSScriptRoot/verify-fleet.ps1" @arguments
if ($DryRun) { $inventory; Write-Output 'Plan: write fresh inventory to requested local output. No writes.'; return }
$parent = Split-Path -Parent ([IO.Path]::GetFullPath($OutputPath))
$null = New-Item -ItemType Directory -Force -Path $parent
$inventory | ConvertTo-Json -Depth 12 | Set-Content -LiteralPath $OutputPath -Encoding utf8
Write-Output 'Inventory written. Contains status/identity labels only; keep operational metadata local.'
