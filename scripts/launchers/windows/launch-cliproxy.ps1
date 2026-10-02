[CmdletBinding()]
param(
    [Parameter(Mandatory)][ValidateSet('codex', 'claude', 'pi')][string]$Client,
    [string]$Endpoint = 'http://127.0.0.1:8317',
    [string]$Model = 'gpt-6-sol',
    [string]$KeyFile = (Join-Path $HOME '.config/cliproxy/client.key'),
    [string]$CredentialsJson,
    [string]$Executable,
    [string]$ProfileHome = (Join-Path $HOME '.codex-cliproxy'),
    [string]$PiConfigPath = (Join-Path $HOME '.pi/agent/models.json'),
    [string]$PromptFile,
    [switch]$DryRun,
    [Parameter(ValueFromRemainingArguments)][string[]]$ClientArgs
)

$ErrorActionPreference = 'Stop'
. "$PSScriptRoot/../../../ops/havok-fleet/common.ps1"
Assert-FleetEndpoint $Endpoint
if ($Model -and $Model -notmatch '^[A-Za-z0-9][A-Za-z0-9._-]*$') { throw 'Invalid model identifier.' }
$Endpoint = $Endpoint.TrimEnd('/')

switch ($Client) {
    'codex' { $exe = 'codex'; $envName = 'CLIPROXY_API_KEY'; $codexProfile = 'cliproxy' }
    'claude' { $exe = 'claude'; $envName = 'ANTHROPIC_API_KEY' }
    'pi' { $exe = 'pi'; $envName = 'OPENAI_API_KEY' }
}

if ($DryRun) {
    Write-Output 'Plan: read protected key; configure opt-in provider/profile; launch runtime. No credentials read, files written, or runtime started.'
    exit 0
}

try {
if ($CredentialsJson) {
    $credentialData = Get-Content -LiteralPath $CredentialsJson -Raw | ConvertFrom-Json
    $key = [string]$credentialData.client_api_key
} else {
    $key = [System.IO.File]::ReadAllText((Resolve-Path -LiteralPath $KeyFile)).Trim()
}
} catch { throw 'Unable to read protected client credential source.' }
if ([string]::IsNullOrWhiteSpace($key) -or $key -match '\s') { throw 'Client key is missing or malformed.' }

function Write-ClientConfig([string]$Path, [string]$Content) {
    $Path = [IO.Path]::GetFullPath($Path)
    $directory = [IO.DirectoryInfo](Split-Path -Parent $Path)
    while ($directory) {
        if ($directory.Exists -and ($directory.Attributes -band [IO.FileAttributes]::ReparsePoint)) { throw 'Linked config parent refused.' }
        $directory = $directory.Parent
    }
    if ((Test-Path -LiteralPath $Path) -and (Get-Item -LiteralPath $Path).LinkType) { throw 'Linked config destination refused.' }
    $parent = Split-Path -Parent $Path
    $null = New-Item -ItemType Directory -Force -Path $parent
    if ((Test-Path -LiteralPath $Path) -and [IO.File]::ReadAllText($Path) -eq $Content) { return }
    $temporary = Join-Path $parent ([IO.Path]::GetRandomFileName())
    try {
        [IO.File]::WriteAllText($temporary, $Content)
        if (Test-Path -LiteralPath $Path) { [IO.File]::Replace($temporary, $Path, [NullString]::Value) }
        else { Move-Item -LiteralPath $temporary -Destination $Path }
    }
    finally { if (Test-Path -LiteralPath $temporary) { Remove-Item -LiteralPath $temporary } }
}

if ($Executable) { $exe = $Executable }
$command = Get-Command $exe -ErrorAction Stop | Select-Object -First 1
$environmentNames = @('CLIPROXY_API_KEY', 'CODEX_HOME', 'ANTHROPIC_BASE_URL', 'ANTHROPIC_AUTH_TOKEN', 'ANTHROPIC_API_KEY', 'OPENAI_API_KEY', 'OPENAI_BASE_URL')
$saved = @{}
foreach ($name in $environmentNames) { $saved[$name] = [Environment]::GetEnvironmentVariable($name, 'Process') }
try {

Set-Item -Path "Env:$envName" -Value $key
switch ($Client) {
    'claude' { $env:ANTHROPIC_BASE_URL = $Endpoint; $env:ANTHROPIC_AUTH_TOKEN = $key; Remove-Item Env:ANTHROPIC_API_KEY -ErrorAction SilentlyContinue }
    'pi' {
        try { $data = if (Test-Path -LiteralPath $PiConfigPath) { Get-Content -LiteralPath $PiConfigPath -Raw | ConvertFrom-Json -AsHashtable } else { @{} } }
        catch { throw 'Existing Pi models configuration is invalid; left unchanged.' }
        if (-not $data.ContainsKey('providers')) { $data.providers = @{} }
        if (-not $data.providers.ContainsKey('cliproxy')) { $data.providers.cliproxy = @{} }
        $provider = $data.providers.cliproxy
        $provider.baseUrl = "$Endpoint/v1"
        $provider.api = 'openai-responses'
        $provider.apiKey = '${CLIPROXY_API_KEY}'
        if (-not @($provider.models | Where-Object id -EQ $Model)) { $provider.models = @($provider.models | Where-Object { $null -ne $_ }) + @(@{ id = $Model; name = "$Model via CLIProxy"; reasoning = $true }) }
        Write-ClientConfig $PiConfigPath ($data | ConvertTo-Json -Depth 30)
        $env:CLIPROXY_API_KEY = $key
    }
    'codex' {
        $provider = @"
[model_providers.cliproxy]
name = "CLIProxyAPI"
base_url = "$Endpoint/v1"
env_key = "CLIPROXY_API_KEY"
wire_api = "responses"
requires_openai_auth = false
"@
        $config = $provider + "`n[profiles.cliproxy]`nmodel_provider = `"cliproxy`"`nmodel = `"$Model`"`n"
        Write-ClientConfig (Join-Path $ProfileHome 'config.toml') $config
        Write-ClientConfig (Join-Path $ProfileHome 'cliproxy.config.toml') ("model_provider = `"cliproxy`"`nmodel = `"$Model`"`n" + $provider)
        $env:CODEX_HOME = $ProfileHome
    }
}
if ($Client -eq 'codex') {
    & $command.Source --no-daemon --profile $codexProfile --model $Model @ClientArgs
} elseif ($Client -eq 'pi') {
    & $command.Source --model "cliproxy/$Model" @ClientArgs
} else {
    $arguments = @()
    if ($Model) { $arguments += @('--model', $Model) }
    if ($PromptFile) { $arguments += @('--append-system-prompt-file', $PromptFile) }
    & $command.Source @arguments @ClientArgs
}
$code = $LASTEXITCODE
} finally {
    foreach ($name in $environmentNames) { [Environment]::SetEnvironmentVariable($name, $saved[$name], 'Process') }
    $key = $null
    $credentialData = $null
}
exit $code
