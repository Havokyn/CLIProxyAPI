# Raw arguments intentionally preserve Herdr's native CLI syntax.
$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot 'herdr-proxy-common.ps1')
$arguments = @($args)
if ($arguments.Count -and $arguments[0] -in @('--help', '-h')) {
    Write-Output @'
herdr-proxy: isolated CLIProxy-backed Herdr 0.9.3
  herdr-proxy status
  herdr-proxy doctor
  herdr-proxy --dry-run [-- <Herdr arguments>]
  herdr-proxy [-- <Herdr arguments>]
Starts a fresh separate named proxy session. Native --session names are used
as labels under havok-proxy- with a unique suffix. Remote commands are refused
until remote proxy support exists.
Normal herdr remains direct. No direct login fallback is permitted.
'@
    exit 0
}
try {
    $settings = Read-HerdrProxySettings
    $mode = 'launch'
    if ($arguments.Count -and $arguments[0] -in @('status', 'doctor', '--dry-run')) {
        $mode = $arguments[0]
        $arguments = @($arguments | Select-Object -Skip 1)
    }
    if ($arguments.Count -and $arguments[0] -eq '--') { $arguments = @($arguments | Select-Object -Skip 1) }
    $status = Get-HerdrProxyPreflight $settings
    Show-HerdrProxyStatus $status
    if ($status.failures.Count) { throw 'Unsafe proxy preflight.' }
    if ($mode -in @('status', 'doctor')) {
        if ($mode -eq 'doctor') { Write-Output 'Doctor PASS: protected key source; authenticated catalog; runtime paths; proxy configs; private shims. No direct fallback.' }
        exit 0
    }
    # A new server owns the inherited environment. Never attach to a direct
    # persistent server, including one launched with a proxy-looking name.
    $session = 'havok-proxy-' + [guid]::NewGuid().ToString('N').Substring(0, 12)
    $nativeArguments = [Collections.Generic.List[string]]::new()
    for ($i = 0; $i -lt $arguments.Count; $i++) {
        if ($arguments[$i] -match '^--(remote|machine)(=|$)' -or $arguments[$i] -eq '--handoff') { throw 'Remote/handoff operations require a separately configured proxy host.' }
        if ($arguments[$i] -match '^--session(=|$)') {
            $label = if ($arguments[$i].StartsWith('--session=')) { $arguments[$i].Substring(10) } else { $i++; if ($i -ge $arguments.Count) { throw 'Session name missing.' }; $arguments[$i] }
            if ($label -notmatch '^[a-zA-Z0-9][a-zA-Z0-9_-]{0,31}$') { throw 'Invalid proxy session label.' }
            $session = 'havok-proxy-' + $label + '-' + [guid]::NewGuid().ToString('N').Substring(0, 8)
        } else { $nativeArguments.Add($arguments[$i]) }
    }
    $arguments = @($nativeArguments)
    if ($arguments.Count -and ($arguments[0] -in @('session', 'update', 'channel', 'machine', 'config') -or ($arguments[0] -eq 'server' -and $arguments.Count -gt 1))) { throw 'Use normal herdr explicitly for installation/session administration.' }
    Write-Output ('Proxy session    ' + $session)
    if ($mode -eq '--dry-run') {
        foreach ($runtime in @('claude', 'pi', 'codex')) { Write-Output ("$runtime : " + $settings[$runtime] + ' <- ' + (Join-Path $settings.shimDirectory "$runtime.cmd")) }
        Write-Output ('Herdr executable ' + $settings.herdr)
        Write-Output 'Herdr command    --session <fresh proxy session> followed by supplied native arguments (not echoed).'
        Write-Output 'Dry run PASS: no Herdr launch, credential changes, or inference.'
        exit 0
    }
    $credentialNames = @('CLIPROXY_API_KEY', 'OPENAI_API_KEY', 'ANTHROPIC_API_KEY', 'ANTHROPIC_AUTH_TOKEN', 'CLAUDE_CODE_OAUTH_TOKEN')
    $names = @('PATH', 'HERDR_PROXY_MODE', 'CLIPROXY_MODE', 'HERDR_CONFIG_PATH', 'HERDR_PROXY_SETTINGS', 'HERDR_PROXY_SHELL_ENGINE', 'HERDR_PROXY_PATH') + $credentialNames
    $saved = @{}
    foreach ($name in $names) { $saved[$name] = [Environment]::GetEnvironmentVariable($name, 'Process') }
    try {
        foreach ($name in $credentialNames) { [Environment]::SetEnvironmentVariable($name, $null, 'Process') }
        $env:PATH = $settings.shimDirectory + ';' + $env:PATH
        $env:HERDR_PROXY_PATH = $env:PATH
        $env:HERDR_PROXY_MODE = '1'; $env:CLIPROXY_MODE = '1'
        $env:HERDR_PROXY_SHELL_ENGINE = $settings.shellEngine
        $env:HERDR_CONFIG_PATH = $settings.herdrConfig
        foreach ($runtime in @('claude', 'pi', 'codex')) {
            $resolved = Get-Command $runtime -CommandType Application,ExternalScript | Select-Object -First 1
            if (-not $resolved.Source.StartsWith($settings.shimDirectory + '\', [StringComparison]::OrdinalIgnoreCase)) { throw 'PATH shim interception failed.' }
        }
        Write-Output 'Starting Herdr...'
        & $settings.herdr --session $session @arguments
        $code = $LASTEXITCODE
    } finally { foreach ($name in $names) { [Environment]::SetEnvironmentVariable($name, $saved[$name], 'Process') } }
    exit $code
} catch {
    Write-Output 'CLIProxy unavailable or proxy configuration unsafe.'
    Write-Output 'Proxy-mode Herdr was NOT started.'
    Write-Output 'Use normal `herdr` explicitly if direct mode is desired.'
    # Never echo exception text: HTTP exceptions may include secret headers.
    exit 1
}
