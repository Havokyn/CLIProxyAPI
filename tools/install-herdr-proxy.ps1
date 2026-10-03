[CmdletBinding()]
param(
    [string]$InstallDirectory = (Join-Path $HOME '.local/bin'),
    [string]$StateDirectory = (Join-Path $env:LOCALAPPDATA 'CLIProxyAPI/herdr-proxy'),
    [string]$CredentialsJson = (Join-Path $env:LOCALAPPDATA 'CLIProxyAPI/.local/credentials.json'),
    [string]$HerdrExecutable,
    [string]$ClaudeExecutable,
    [string]$PiExecutable,
    [string]$CodexExecutable,
    [string]$PiConfig = (Join-Path $HOME '.pi/agent/models.json'),
    [string]$CodexConfig = (Join-Path $HOME '.codex/config.toml'),
    [string]$HerdrConfig = (Join-Path $env:APPDATA 'herdr/config.toml'),
    [string]$Endpoint = 'http://127.0.0.1:8317',
    [string]$Model = 'gpt-6-sol'
)
$ErrorActionPreference = 'Stop'
$root = Split-Path $PSScriptRoot -Parent
. (Join-Path $root 'ops/havok-fleet/common.ps1')
Assert-FleetEndpoint $Endpoint
$Endpoint = $Endpoint.TrimEnd('/')
if ($Model -notmatch '^[A-Za-z0-9][A-Za-z0-9._-]*$') { throw 'Invalid model.' }
function Resolve-RealRuntime($Explicit, $Name) {
    if ($Explicit) { return (Resolve-Path -LiteralPath $Explicit).Path }
    return (Get-Command $Name -CommandType Application,ExternalScript -ErrorAction Stop | Select-Object -First 1).Source
}
if (-not $HerdrExecutable) {
    $stable = Join-Path $HOME '.herdr/packages/standalone/releases/0.9.3-x86_64-pc-windows-msvc/herdr.exe'
    $HerdrExecutable = if (Test-Path -LiteralPath $stable) { $stable } else { Resolve-RealRuntime '' 'herdr.exe' }
}
$settings = @{
    root = $root; endpoint = $Endpoint; credentials = [IO.Path]::GetFullPath($CredentialsJson)
    herdr = Resolve-RealRuntime $HerdrExecutable 'herdr.exe'
    claude = Resolve-RealRuntime $ClaudeExecutable 'claude.exe'
    # NPM's PowerShell wrapper preserves literal argv without CMD expanding
    # %ENV% inside supplied prompts after the child key has been installed.
    pi = Resolve-RealRuntime $PiExecutable 'pi.ps1'
    codex = Resolve-RealRuntime $CodexExecutable 'codex.exe'
    model = $Model; piConfig = [IO.Path]::GetFullPath($PiConfig); codexConfig = [IO.Path]::GetFullPath($CodexConfig)
    codexProfile = Join-Path (Split-Path $CodexConfig) 'cliproxy.config.toml'
    shimDirectory = Join-Path ([IO.Path]::GetFullPath($StateDirectory)) 'shims'
    shellEngine = (Get-Command pwsh -CommandType Application | Select-Object -First 1).Source
    shellExecutable = Join-Path ([IO.Path]::GetFullPath($StateDirectory)) 'herdr-proxy-shell.exe'
    herdrConfig = Join-Path ([IO.Path]::GetFullPath($StateDirectory)) 'config.toml'
}
. (Join-Path $root 'scripts/launchers/windows/herdr-proxy-common.ps1')
foreach ($runtime in @('herdr', 'claude', 'pi', 'codex')) { Assert-HerdrProxyExecutable $settings[$runtime] $settings.shimDirectory }
if (-not (Test-Path -LiteralPath $settings.codexProfile)) { throw 'Existing Codex cliproxy profile required; no normal Codex configuration will be overwritten.' }
# Install into an existing user PATH directory. Never mutate PATH.
$pathEntries = @($env:PATH.Split(';') + ([Environment]::GetEnvironmentVariable('PATH', 'User') -split ';')) | ForEach-Object { $_.TrimEnd('\', '/') }
$InstallDirectory = [IO.Path]::GetFullPath($InstallDirectory).TrimEnd('\', '/')
if ($InstallDirectory -notin $pathEntries) { throw 'InstallDirectory must already exist on the user/process PATH.' }
foreach ($path in @($StateDirectory, $InstallDirectory, $PiConfig)) {
    $item = if (Test-Path -LiteralPath $path) { Get-Item -LiteralPath $path -Force } else { [IO.FileInfo][IO.Path]::GetFullPath($path) }
    while ($item) {
        if ($item.Exists -and ($item.Attributes -band [IO.FileAttributes]::ReparsePoint)) { throw 'Linked install/config path refused.' }
        $item = if ($item -is [IO.DirectoryInfo]) { $item.Parent } else { $item.Directory }
    }
}
$manifestPath = Join-Path $StateDirectory 'install.json'
if (Test-Path -LiteralPath $manifestPath) { throw 'Already installed. Uninstall before reinstalling; preserve active proxy sessions.' }
if ((Test-Path -LiteralPath $StateDirectory) -and @(Get-ChildItem -LiteralPath $StateDirectory -Force).Count) { throw 'Nonempty unowned state directory refused.' }
foreach ($extension in @('cmd', 'ps1')) {
    if (Test-Path -LiteralPath (Join-Path $InstallDirectory "herdr-proxy.$extension")) { throw 'Existing command refused; it is not owned by this installer.' }
}
$config = if (Test-Path -LiteralPath $HerdrConfig) { [IO.File]::ReadAllText($HerdrConfig) } else { '' }
if ($config -match '(?m)^\[terminal\]') { throw 'Existing terminal section needs explicit merge; installation refused.' }
$originalPi = [IO.File]::ReadAllText($PiConfig)
$pi = $originalPi | ConvertFrom-Json -AsHashtable
$null = New-Item -ItemType Directory -Force -Path $StateDirectory, $settings.shimDirectory, $InstallDirectory
$pwsh = (Get-Command pwsh -CommandType Application | Select-Object -First 1).Source
$files = [Collections.Generic.List[string]]::new()
function Write-OwnedFile($Path, $Content) {
    if (Test-Path -LiteralPath $Path) { throw 'Unowned destination refused.' }
    $files.Add([IO.Path]::GetFullPath($Path))
    [IO.File]::WriteAllText($Path, $Content)
}
$piChanged = $false
try {
$go = (Get-Command go -CommandType Application -ErrorAction SilentlyContinue | Select-Object -First 1).Source
if (-not $go) { $go = 'C:/Program Files/Go/bin/go.exe' }
if (-not (Test-Path -LiteralPath $go)) { throw 'Go compiler required for private shell bootstrap.' }
$files.Add($settings.shellExecutable)
& $go build -buildvcs=false -o $settings.shellExecutable (Join-Path $root 'scripts/launchers/windows/shell/main.go')
if ($LASTEXITCODE) { throw 'Private shell build failed.' }
$settingsPath = Join-Path $StateDirectory 'settings.json'
Write-OwnedFile $settingsPath ($settings | ConvertTo-Json -Depth 8)
$bridge = Join-Path $root 'scripts/launchers/windows/herdr-proxy-runtime.ps1'
$entry = Join-Path $root 'scripts/launchers/windows/herdr-proxy.ps1'
function Quote-PS($Value) { return "'" + $Value.Replace("'", "''") + "'" }
$recovery = Join-Path $root 'scripts/launchers/windows/proxy-recovery.ps1'
Write-OwnedFile (Join-Path $settings.shimDirectory 'relogin.ps1') ("& " + (Quote-PS $recovery) + " @args`nexit `$LASTEXITCODE`n")
Write-OwnedFile (Join-Path $settings.shimDirectory 'relogin.cmd') ("@echo off`r`n`"$pwsh`" -NoLogo -NoProfile -File `"$recovery`" %*`r`nexit /b %ERRORLEVEL%`r`n")
foreach ($runtime in @('claude', 'pi', 'codex')) {
    Write-OwnedFile (Join-Path $settings.shimDirectory "$runtime.ps1") (('$env:HERDR_PROXY_SETTINGS = ' + (Quote-PS $settingsPath)) + "`n& " + (Quote-PS $bridge) + " $runtime @args`nexit `$LASTEXITCODE`n")
    Write-OwnedFile (Join-Path $settings.shimDirectory "$runtime.cmd") ("@echo off`r`nsetlocal`r`nset `"HERDR_PROXY_SETTINGS=$settingsPath`"`r`n`"$pwsh`" -NoLogo -NoProfile -File `"$bridge`" $runtime %*`r`nexit /b %ERRORLEVEL%`r`n")
}
Write-OwnedFile (Join-Path $InstallDirectory 'herdr-proxy.ps1') (('$env:HERDR_PROXY_SETTINGS = ' + (Quote-PS $settingsPath)) + "`n& " + (Quote-PS $entry) + " @args`nexit `$LASTEXITCODE`n")
Write-OwnedFile (Join-Path $InstallDirectory 'herdr-proxy.cmd') ("@echo off`r`nsetlocal`r`nset `"HERDR_PROXY_SETTINGS=$settingsPath`"`r`n`"$pwsh`" -NoLogo -NoProfile -File `"$entry`" %*`r`nexit /b %ERRORLEVEL%`r`n")
# Preserve normal Herdr config. The private shell bypasses user profiles.
$config += "`n[terminal]`ndefault_shell = '$($settings.shellExecutable.Replace('\', '/'))'`nshell_mode = 'non_login'`n"
Write-OwnedFile $settings.herdrConfig $config
# Only merge the opt-in Pi provider; direct providers remain intact. Keep an
# exact backup for uninstall, and never write an actual key into models.json.
if (-not $pi.providers) { $pi.providers = @{} }
if (-not $pi.providers.cliproxy) { $pi.providers.cliproxy = @{} }
$pi.providers.cliproxy.baseUrl = "$Endpoint/v1"
$pi.providers.cliproxy.api = 'openai-responses'
$pi.providers.cliproxy.apiKey = '${CLIPROXY_API_KEY}'
if (-not @($pi.providers.cliproxy.models | Where-Object id -EQ $Model)) { $pi.providers.cliproxy.models = @($pi.providers.cliproxy.models | Where-Object { $_ }) + @(@{ id = $Model; name = "$Model via CLIProxy"; reasoning = $true }) }
Write-OwnedFile (Join-Path $StateDirectory 'pi-models.before.json') $originalPi
$piChanged = $true
[IO.File]::WriteAllText($PiConfig, ($pi | ConvertTo-Json -Depth 30))
$manifest = @{ files = @($files | ForEach-Object { @{ path = $_; hash = (Get-FileHash -LiteralPath $_ -Algorithm SHA256).Hash } }); piConfig = $PiConfig; piHash = (Get-FileHash -LiteralPath $PiConfig -Algorithm SHA256).Hash; shimDirectory = $settings.shimDirectory }
[IO.File]::WriteAllText($manifestPath, ($manifest | ConvertTo-Json -Depth 8))
} catch {
    if ($piChanged) { [IO.File]::WriteAllText($PiConfig, $originalPi) }
    foreach ($file in $files) { if (Test-Path -LiteralPath $file) { Remove-Item -LiteralPath $file } }
    if (Test-Path -LiteralPath $manifestPath) { Remove-Item -LiteralPath $manifestPath }
    if ((Test-Path -LiteralPath $settings.shimDirectory) -and -not @(Get-ChildItem -LiteralPath $settings.shimDirectory -Force).Count) { Remove-Item -LiteralPath $settings.shimDirectory }
    throw 'Installation failed; owned files removed and original Pi config restored.'
}
Write-Output ('Installed herdr-proxy in ' + $InstallDirectory + '. User/system PATH unchanged. Normal herdr unchanged.')
