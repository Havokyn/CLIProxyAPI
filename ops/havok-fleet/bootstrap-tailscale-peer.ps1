[CmdletBinding()]
param(
    [Parameter(Mandatory)][string]$SshTarget,
    [Parameter(Mandatory)][ValidatePattern('^[A-Za-z0-9][A-Za-z0-9-]{0,62}$')][string]$Hostname,
    [switch]$DryRun
)
. "$PSScriptRoot/common.ps1"
Assert-FleetTarget $SshTarget
if ($DryRun) { Write-Output 'Plan: detect OS/install; install Tailscale only if missing; set hostname; request manual login if needed; report online/DNS/IP. No SSH or writes.'; return }
$payload = @{ source = (Get-Content "$PSScriptRoot/tailscale-peer.py" -Raw); hostname = $Hostname }
$result = Invoke-FleetPython $SshTarget $payload
if ($result.authorization_url) { Write-Output $result.authorization_url; return }
$result | ConvertTo-Json -Depth 5
if (-not $result.online) { throw 'Tailscale is not online. Resolve authentication/connectivity and rerun.' }
