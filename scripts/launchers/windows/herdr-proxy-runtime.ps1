$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot 'herdr-proxy-common.ps1')
try {
    if ($env:HERDR_PROXY_MODE -ne '1') { throw 'Proxy marker missing.' }
    $runtime = $args[0]
    if ($runtime -notin @('claude', 'pi', 'codex')) { throw 'Unsupported runtime.' }
    $arguments = @($args | Select-Object -Skip 1)
    # Authentication/profile/provider overrides could bypass the gateway.
    if ($arguments.Count -and $arguments[0] -match '^(login|logout|relogin|auth)$') { throw 'Direct authentication refused.' }
    foreach ($argument in $arguments) {
        if ($argument -match '^(--profile|--provider|--api-key|--base-url|--config)(=|$)' -or ($runtime -eq 'codex' -and $argument -match '^(-p|-c)')) { throw 'Direct authentication or provider override refused.' }
    }
    $settings = Read-HerdrProxySettings
    Assert-HerdrProxyExecutable $settings[$runtime] $settings.shimDirectory
    $preflight = Get-HerdrProxyPreflight $settings
    if ($preflight.failures.Count) { throw 'Proxy preflight no longer safe.' }
    $parameters = @{ Client = $runtime; Endpoint = $settings.endpoint; CredentialsJson = $settings.credentials; Executable = $settings[$runtime]; ClientArgs = $arguments; ProxyMode = $true }
    if ($runtime -eq 'claude') {
        $parameters.Model = ''
        if ($env:SR_OPUS_PROMPT -and (Test-Path -LiteralPath $env:SR_OPUS_PROMPT)) { $parameters.PromptFile = $env:SR_OPUS_PROMPT }
    } else { $parameters.Model = $settings.model }
    $parameters.PiConfigPath = $settings.piConfig
    if ($runtime -eq 'codex') { $env:CODEX_HOME = Split-Path $settings.codexConfig }
    if ($runtime -eq 'pi') { $env:PI_CODING_AGENT_DIR = Split-Path $settings.piConfig }
    & (Join-Path $PSScriptRoot 'launch-cliproxy.ps1') @parameters
    $code = $LASTEXITCODE
    if ($code -ne 0) { Show-HerdrProxyPoolFailure $settings $(if ($runtime -eq 'pi') { 'codex' } else { $runtime }) $(if ($runtime -eq 'claude') { '' } else { $settings.model }) }
    exit $code
} catch {
    Write-Output 'Proxy runtime refused unsafe configuration/authentication override. No direct fallback.'
    exit 1
}
