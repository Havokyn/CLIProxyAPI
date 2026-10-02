$ErrorActionPreference = 'Stop'

function Assert-FleetTarget([string]$SshTarget) {
    if ($SshTarget -notmatch '^[A-Za-z0-9][A-Za-z0-9._-]*$') {
        throw 'SSH target must be a configured alias (letters, digits, dots, underscores, hyphens).'
    }
}

function Assert-FleetEndpoint([string]$Endpoint) {
    if ($Endpoint -notmatch '^https?://[A-Za-z0-9.\-:\[\]]+/?$') { throw 'Endpoint must be a plain origin without embedded whitespace or configuration syntax.' }
    $uri = $null
    if (-not [uri]::TryCreate($Endpoint, [UriKind]::Absolute, [ref]$uri) -or
        $uri.UserInfo -or $uri.Query -or $uri.Fragment -or $uri.AbsolutePath -ne '/' -or
        ($uri.Scheme -ne 'https' -and -not ($uri.Scheme -eq 'http' -and $uri.IsLoopback))) {
        throw 'Endpoint must be an HTTPS origin or a loopback HTTP origin, without credentials or a path.'
    }
}

function Invoke-FleetPython([string]$SshTarget, [hashtable]$Payload) {
    Assert-FleetTarget $SshTarget
    # Only source/templates and nonsecret settings travel to the machine.
    $command = 'python3 -c ''import json,sys; bundle=json.load(sys.stdin); exec(compile(bundle["source"],"fleet-agent","exec"))'''
    $output = ($Payload | ConvertTo-Json -Depth 12 -Compress) | & ssh -o BatchMode=yes -o ConnectTimeout=15 $SshTarget $command 2>$null
    if ($LASTEXITCODE -ne 0) { throw 'Remote operation failed; check SSH, Python 3, and remote prerequisites. Remote output suppressed.' }
    try { return ($output -join "`n") | ConvertFrom-Json } catch { throw 'Remote operation returned invalid status JSON.' }
}

function New-FleetPayload([string]$Mode, [string]$Endpoint, [string]$Model, [bool]$Herdr, [bool]$SmokeTest) {
    Assert-FleetEndpoint $Endpoint
    if ($Model -notmatch '^[A-Za-z0-9][A-Za-z0-9._-]*$') { throw 'Invalid model identifier.' }
    $files = @{}
    foreach ($name in @('pi-cliproxy', 'claude-cliproxy', 'codex-cliproxy', 'cliproxy-env.sh')) {
        $files[$name] = (Get-Content -LiteralPath (Join-Path $PSScriptRoot "../../scripts/launchers/posix/$name") -Raw).Replace("`r`n", "`n")
    }
    return @{ source = (Get-Content -LiteralPath (Join-Path $PSScriptRoot 'remote-agent.py') -Raw); mode = $Mode; endpoint = $Endpoint.TrimEnd('/'); model = $Model; herdr = $Herdr; smoke_test = $SmokeTest; files = $files }
}
