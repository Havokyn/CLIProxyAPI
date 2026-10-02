$ErrorActionPreference = 'Stop'
$pwsh = Get-Command pwsh -ErrorAction Stop
if ($PSVersionTable.PSVersion.Major -lt 7) {
    throw 'Run this check with PowerShell 7 (pwsh).'
}
$repo = (Resolve-Path (Join-Path $PSScriptRoot '../..')).Path
$scanRoots = @(
    (Join-Path $repo 'ops/havok-fleet'),
    (Join-Path $repo 'scripts/launchers/windows')
)
$files = foreach ($root in $scanRoots) {
    Get-ChildItem -LiteralPath $root -Filter '*.ps1' -File -Recurse
}
foreach ($file in $files) {
    $tokens = $null
    $errors = $null
    [System.Management.Automation.Language.Parser]::ParseFile($file.FullName, [ref]$tokens, [ref]$errors) | Out-Null
    if ($errors.Count -gt 0) {
        throw "PowerShell syntax check failed for $($file.Name)."
    }
}

$endpoint = 'https://proxy.invalid'
$model = 'fleet-dry-run-model'
$shell = $pwsh.Source
foreach ($name in @('claude', 'codex', 'pi')) {
    $launcher = Join-Path $repo "scripts/launchers/windows/$name-cliproxy.ps1"
    $output = & $shell -NoProfile -File $launcher -DryRun -Endpoint $endpoint -Model $model 2>&1
    if ($LASTEXITCODE -ne 0) {
        throw "DryRun failed for $name launcher."
    }
    $text = $output -join "`n"
    if ($text -notmatch '^Plan:' -or $text -notmatch 'No credentials read') {
        throw "DryRun output was incomplete for $name launcher."
    }
    if ($text -match [regex]::Escape($endpoint) -or $text -match [regex]::Escape($model) -or
        $text -match '(?i)(bearer\s+\S{16,}|api[_-]?key\s*[:=]\s*\S{8,})') {
        throw "DryRun exposed configuration or credential-shaped output for $name launcher."
    }
}

$sshCalled = $false
function ssh { $script:sshCalled = $true; throw 'SSH must not run in this check.' }
$remoteBootstrap = Join-Path $repo 'ops/havok-fleet/bootstrap-remote-cliproxy-agent.ps1'
$peerBootstrap = Join-Path $repo 'ops/havok-fleet/bootstrap-tailscale-peer.ps1'
& $remoteBootstrap -SshTarget 'fleet-test-alias' -Endpoint $endpoint -DryRun | Out-Null
& $peerBootstrap -SshTarget 'fleet-test-alias' -Hostname 'fleet-test-peer' -DryRun | Out-Null
if ($sshCalled) { throw 'Fleet bootstrap DryRun attempted SSH.' }
foreach ($target in @($remoteBootstrap, $peerBootstrap)) {
    $sshCalled = $false
    try {
        if ($target -eq $remoteBootstrap) {
            & $target -SshTarget 'invalid alias' -Endpoint $endpoint -DryRun | Out-Null
        } else {
            & $target -SshTarget 'invalid alias' -Hostname 'fleet-test-peer' -DryRun | Out-Null
        }
        throw 'Invalid SSH target was accepted.'
    } catch {
        if ($_.Exception.Message -eq 'Invalid SSH target was accepted.') { throw }
    }
    if ($sshCalled) { throw 'Invalid target reached SSH.' }
}

$temporary = Join-Path ([IO.Path]::GetTempPath()) ([IO.Path]::GetRandomFileName())
$null = New-Item -ItemType Directory -Path $temporary
try {
    $keyFile = Join-Path $temporary 'client.key'
    $runtime = Join-Path $temporary 'fake-runtime.ps1'
    $driver = Join-Path $temporary 'launch-driver.ps1'
    $capture = Join-Path $temporary 'runtime.json'
    $restoreCapture = Join-Path $temporary 'restore.json'
    $piConfig = Join-Path $temporary 'models.json'
    $profileHome = Join-Path $temporary 'codex-home'
    $keyValue = 'test-only-fleet-key'
    [IO.File]::WriteAllText($keyFile, $keyValue)
    $runtimeScript = @'
$record = @{
    args = @($args)
    cliproxy_key_present = ($env:CLIPROXY_API_KEY -eq $env:FLEET_EXPECTED_KEY)
    openai_key_present = ($env:OPENAI_API_KEY -eq $env:FLEET_EXPECTED_KEY)
    anthropic_key_present = ($env:ANTHROPIC_AUTH_TOKEN -eq $env:FLEET_EXPECTED_KEY)
    anthropic_base_url = $env:ANTHROPIC_BASE_URL
    codex_home = $env:CODEX_HOME
}
[IO.File]::WriteAllText($env:FLEET_TEST_CAPTURE, ($record | ConvertTo-Json -Compress))
'@
    [IO.File]::WriteAllText($runtime, $runtimeScript)
    $driverScript = @'
param([string]$Launcher, [string]$Client, [string]$Endpoint, [string]$Model, [string]$KeyFile, [string]$Executable, [string]$PiConfigPath, [string]$ProfileHome)
$names = @('CLIPROXY_' + 'API_KEY', 'CODEX_HOME', 'ANTHROPIC_BASE_URL', 'ANTHROPIC_AUTH_TOKEN', 'ANTHROPIC_' + 'API_KEY', 'OPENAI_API_KEY', 'OPENAI_BASE_URL')
$original = @{}
foreach ($name in $names) {
    $original[$name] = 'prior-' + $name
    [Environment]::SetEnvironmentVariable($name, $original[$name], 'Process')
}
if ($Client -eq 'pi') {
    & $Launcher -Endpoint $Endpoint -Model $Model -KeyFile $KeyFile -Executable $Executable -PiConfigPath $PiConfigPath
} elseif ($Client -eq 'codex') {
    & $Launcher -Endpoint $Endpoint -Model $Model -KeyFile $KeyFile -Executable $Executable -ProfileHome $ProfileHome
} else {
    & $Launcher -Endpoint $Endpoint -Model $Model -KeyFile $KeyFile -Executable $Executable
}
$launchCode = $LASTEXITCODE
$restored = $true
foreach ($name in $names) {
    if ([Environment]::GetEnvironmentVariable($name, 'Process') -cne $original[$name]) { $restored = $false }
}
[IO.File]::WriteAllText($env:FLEET_RESTORE_CAPTURE, (@{ restored = $restored } | ConvertTo-Json -Compress))
exit $launchCode
'@
    [IO.File]::WriteAllText($driver, $driverScript)
    $directConfig = @{ providers = @{ openai = @{ baseUrl = 'https://direct.invalid/v1'; models = @(@{ id = 'direct-model' }) } } } | ConvertTo-Json -Depth 8
    [IO.File]::WriteAllText($piConfig, $directConfig)
    $env:FLEET_EXPECTED_KEY = $keyValue
    $env:FLEET_TEST_CAPTURE = $capture
    $env:FLEET_RESTORE_CAPTURE = $restoreCapture

    $piLauncher = Join-Path $repo 'scripts/launchers/windows/pi-cliproxy.ps1'
    & $shell -NoProfile -File $driver -Launcher $piLauncher -Client pi -Endpoint $endpoint -Model 'test-pi-model' -KeyFile $keyFile -Executable $runtime -PiConfigPath $piConfig
    if ($LASTEXITCODE -ne 0) { throw 'Pi fake-runtime launch failed.' }
    if (-not (Get-Content -LiteralPath $restoreCapture -Raw | ConvertFrom-Json).restored) { throw 'Pi launcher did not restore process environment.' }
    $runtimeResult = Get-Content -LiteralPath $capture -Raw | ConvertFrom-Json
    if (-not $runtimeResult.openai_key_present -or -not $runtimeResult.cliproxy_key_present -or
        ($runtimeResult.args -join ' ') -notmatch '--model cliproxy/test-pi-model') { throw 'Pi launcher did not pass expected provider settings.' }
    $savedPi = Get-Content -LiteralPath $piConfig -Raw | ConvertFrom-Json -AsHashtable
    if (-not $savedPi.providers.openai -or $savedPi.providers.openai.models[0].id -ne 'direct-model' -or
        $savedPi.providers.cliproxy.apiKey -ne '${CLIPROXY_API_KEY}' -or
        $savedPi.providers.cliproxy.baseUrl -ne "$endpoint/v1") { throw 'Pi configuration merge did not preserve direct providers or CLIProxy settings.' }

    $codexLauncher = Join-Path $repo 'scripts/launchers/windows/codex-cliproxy.ps1'
    & $shell -NoProfile -File $driver -Launcher $codexLauncher -Client codex -Endpoint $endpoint -Model 'test-codex-model' -KeyFile $keyFile -Executable $runtime -ProfileHome $profileHome
    if ($LASTEXITCODE -ne 0) { throw 'Codex fake-runtime launch failed.' }
    if (-not (Get-Content -LiteralPath $restoreCapture -Raw | ConvertFrom-Json).restored) { throw 'Codex launcher did not restore process environment.' }
    $runtimeResult = Get-Content -LiteralPath $capture -Raw | ConvertFrom-Json
    $codexConfig = Get-Content -LiteralPath (Join-Path $profileHome 'config.toml') -Raw
    if (-not $runtimeResult.cliproxy_key_present -or $runtimeResult.codex_home -ne $profileHome -or
        ($runtimeResult.args -join ' ') -notmatch '--profile cliproxy --model test-codex-model' -or
        $codexConfig -notmatch 'model = "test-codex-model"' -or $codexConfig -notmatch [regex]::Escape("$endpoint/v1")) {
        throw 'Codex launcher did not configure the isolated provider profile.'
    }

    $claudeLauncher = Join-Path $repo 'scripts/launchers/windows/claude-cliproxy.ps1'
    & $shell -NoProfile -File $driver -Launcher $claudeLauncher -Client claude -Endpoint $endpoint -Model 'test-claude-model' -KeyFile $keyFile -Executable $runtime
    if ($LASTEXITCODE -ne 0) { throw 'Claude fake-runtime launch failed.' }
    if (-not (Get-Content -LiteralPath $restoreCapture -Raw | ConvertFrom-Json).restored) { throw 'Claude launcher did not restore process environment.' }
    $runtimeResult = Get-Content -LiteralPath $capture -Raw | ConvertFrom-Json
    if (-not $runtimeResult.anthropic_key_present -or $runtimeResult.anthropic_base_url -ne $endpoint) {
        throw 'Claude launcher did not pass expected endpoint and auth environment.'
    }
} finally {
    Remove-Item Env:FLEET_EXPECTED_KEY,Env:FLEET_TEST_CAPTURE,Env:FLEET_RESTORE_CAPTURE -ErrorAction SilentlyContinue
    $resolvedTemporary = [IO.Path]::GetFullPath($temporary)
    $expectedParent = [IO.Path]::GetFullPath([IO.Path]::GetTempPath()).TrimEnd([IO.Path]::DirectorySeparatorChar)
    if ([IO.Path]::GetDirectoryName($resolvedTemporary) -ne $expectedParent) { throw 'Temporary cleanup escaped its parent; cleanup refused.' }
    Remove-Item -LiteralPath $temporary -Recurse -Force
}
Write-Output 'PowerShell syntax, launcher runtime, and fleet DryRun checks passed.'
