[CmdletBinding()]
param(
    [Parameter(Mandatory)][string]$FleetPath,
    [string]$LocalEndpoint = 'http://127.0.0.1:8317',
    [string]$LocalKeyFile,
    [string]$ManagementKeyFile,
    [string]$Model = 'gpt-6-sol',
    [switch]$SmokeTest,
    [switch]$DryRun
)
. "$PSScriptRoot/common.ps1"
Assert-FleetEndpoint $LocalEndpoint
$fleet = Get-Content -LiteralPath $FleetPath -Raw | ConvertFrom-Json
if ($fleet.schema_version -ne 1 -or -not $fleet.machines) { throw 'Expected a version 1 fleet configuration with machines.' }
if (@($fleet.PSObject.Properties.Name | Where-Object { $_ -notin @('schema_version', 'machines') })) { throw 'Unexpected fleet config fields.' }
foreach ($machine in $fleet.machines) {
    Assert-FleetTarget $machine.ssh_target
    Assert-FleetEndpoint $machine.endpoint
    if ($machine.label -notmatch '^[A-Za-z0-9][A-Za-z0-9._-]*$' -or $machine.credential_label -notmatch '^[A-Za-z0-9][A-Za-z0-9._-]*$') { throw 'Invalid machine or credential label.' }
    $unknown = @($machine.PSObject.Properties.Name | Where-Object { $_ -notin @('label', 'ssh_target', 'endpoint', 'credential_label') })
    if ($unknown) { throw 'Unexpected machine config fields; use only label, ssh_target, endpoint, credential_label.' }
}
if ($DryRun) { Write-Output 'Plan: probe local listener/UI/models/diagnostic and remote Tailscale/models/runtimes/launchers. Smoke only if explicitly requested. No connections or writes.'; return }

function Get-SafeHttpStatus([string]$Path, [string]$KeyFile, [switch]$CountModels, [switch]$RoutingDiagnostic) {
    $headers = @{}
    try {
        if ($KeyFile) {
            $key = (Get-Content -LiteralPath $KeyFile -Raw).Trim()
            if (-not $key -or $key -match '\s') { return @{ state = 'key_invalid'; http_status = $null } }
            $headers.Authorization = 'Bearer ' + $key
        }
        $response = Invoke-WebRequest -Uri ($LocalEndpoint.TrimEnd('/') + $Path) -Headers $headers -MaximumRedirection 0 -TimeoutSec 20 -SkipHttpErrorCheck
        $count = $null
        if ($CountModels -and $response.StatusCode -eq 200) { $doc = $response.Content | ConvertFrom-Json; if ($null -ne $doc.data) { $count = @($doc.data).Count } }
        $status = @{ state = 'observed'; http_status = [int]$response.StatusCode; model_count = $count }
        if ($RoutingDiagnostic -and $response.StatusCode -eq 200) {
            $doc = $response.Content | ConvertFrom-Json
            $status.reset_aware_enabled = [bool]$doc.enabled -and $doc.strategy -eq 'reset-aware'
        }
        return $status
    } catch { return @{ state = 'unavailable'; http_status = $null; model_count = $null } }
    finally { $headers.Clear(); $key = $null }
}
$listenerState = 'unknown'
$processState = 'unknown'
if (Get-Command Get-NetTCPConnection -ErrorAction SilentlyContinue) {
    $connections = @(Get-NetTCPConnection -LocalPort ([uri]$LocalEndpoint).Port -State Listen -ErrorAction SilentlyContinue)
    $listenerState = if ($connections) { 'listening' } else { 'down' }
    $matching = @($connections | ForEach-Object { Get-Process -Id $_.OwningProcess -ErrorAction SilentlyContinue } | Where-Object ProcessName -Like 'cli-proxy-api*')
    $processState = if ($matching) { 'running' } else { 'not_observed' }
}
$local = [ordered]@{ listener = $listenerState; process = $processState; models = (Get-SafeHttpStatus '/v1/models' $LocalKeyFile -CountModels); management_ui = (Get-SafeHttpStatus '/management.html' '') }
$local.reset_aware = if ($ManagementKeyFile) { Get-SafeHttpStatus '/v8/management/routing/reset-aware' $ManagementKeyFile -RoutingDiagnostic } else { @{ state = 'not_checked_missing_management_key'; http_status = $null } }
$machines = @()
foreach ($machine in $fleet.machines) {
    try {
        $payload = New-FleetPayload 'probe' $machine.endpoint $Model $false $SmokeTest.IsPresent
        $status = Invoke-FleetPython $machine.ssh_target $payload
    } catch { $status = @{ probe_state = 'ssh_or_probe_failed'; e2e_state = 'not_tested' } }
    $machines += [ordered]@{ label = $machine.label; ssh_target = $machine.ssh_target; credential_label = $machine.credential_label; status = $status }
}
$healthy = $local.models.http_status -eq 200 -and $null -ne $local.models.model_count -and $local.management_ui.http_status -eq 200
if ($ManagementKeyFile) { $healthy = $healthy -and $local.reset_aware.reset_aware_enabled }
foreach ($machine in $machines) {
    $status = $machine.status
    $healthy = $healthy -and $status.tailscale.online -and $status.credential_state -eq 'configured' -and $status.proxy.valid_response -and $status.proxy.http_status -eq 200
    foreach ($runtime in @('pi', 'claude', 'codex')) {
        if ($status.runtimes.$runtime -and -not $status.launchers.$runtime) { $healthy = $false }
    }
    if ($SmokeTest -and $status.e2e_state -ne 'verified') { $healthy = $false }
}
[pscustomobject]@{ schema_version = 1; generated_at_utc = (Get-Date).ToUniversalTime().ToString('o'); health_state = $(if ($healthy) { 'healthy' } else { 'degraded' }); local = $local; machines = $machines }
