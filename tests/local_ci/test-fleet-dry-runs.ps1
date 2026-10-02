$ErrorActionPreference = 'Stop'
$repo = (Resolve-Path (Join-Path $PSScriptRoot '../..')).Path
function ssh { throw 'DryRun attempted SSH.' }
function Invoke-WebRequest { throw 'DryRun attempted HTTP.' }
function Start-Process { throw 'DryRun attempted process start.' }
function Stop-Process { throw 'DryRun attempted process stop.' }
function tailscale { throw 'DryRun attempted Tailscale.' }
. (Join-Path $repo 'ops/havok-fleet/common.ps1')
foreach ($endpoint in @('https://proxy.invalid', 'http://127.0.0.1:8317')) { Assert-FleetEndpoint $endpoint }
foreach ($endpoint in @('http://proxy.invalid', 'https://user:password@proxy.invalid', 'https://proxy.invalid/v1')) {
    $rejected = $false
    try { Assert-FleetEndpoint $endpoint } catch { $rejected = $true }
    if (-not $rejected) { throw 'Invalid endpoint accepted.' }
}
$fleet = Join-Path $repo 'ops/havok-fleet/fleet.example.json'
& (Join-Path $repo 'ops/havok-fleet/verify-fleet.ps1') -FleetPath $fleet -DryRun | Out-Null
$output = Join-Path $env:TEMP ('havok-ci-' + [guid]::NewGuid() + '.json')
& (Join-Path $repo 'ops/havok-fleet/write-fleet-inventory.ps1') -FleetPath $fleet -OutputPath $output -DryRun | Out-Null
if (Test-Path -LiteralPath $output) { throw 'Inventory DryRun wrote a file.' }
foreach ($action in @('start', 'status', 'stop')) {
    & (Join-Path $repo 'ops/havok-fleet/gateway.ps1') -Action $action -Executable 'missing-fixture.exe' -DryRun | Out-Null
}
Write-Output 'Fleet endpoint, inventory and gateway DryRun checks passed without network or process operations.'
