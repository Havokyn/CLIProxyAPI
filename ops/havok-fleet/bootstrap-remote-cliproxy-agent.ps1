[CmdletBinding()]
param(
    [Parameter(Mandatory)][string]$SshTarget,
    [Parameter(Mandatory)][string]$Endpoint,
    [string]$Model = 'gpt-6-sol',
    [switch]$EnableHerdrBindings,
    [switch]$DryRun
)
. "$PSScriptRoot/common.ps1"
Assert-FleetTarget $SshTarget
$payload = New-FleetPayload 'install' $Endpoint $Model $EnableHerdrBindings.IsPresent $false
if ($DryRun) {
    Write-Output 'Plan: verify Tailscale and protected remote key; merge opt-in Pi provider; install launchers and isolated Codex profile if installed; optionally add Herdr bindings; verify models. No SSH or writes.'
    return
}
$result = Invoke-FleetPython $SshTarget $payload
$result | ConvertTo-Json -Depth 8
if ($result.bootstrap -ne 'configured') {
    throw 'Bootstrap incomplete. If key_missing: manually create ~/.config/cliproxy (700) and enter a unique key into client.key (600) on the remote machine, then rerun. No key was transferred.'
}
