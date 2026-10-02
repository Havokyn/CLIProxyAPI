[CmdletBinding()]
param(
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
& (Join-Path $PSScriptRoot 'launch-cliproxy.ps1') -Client codex -Endpoint $Endpoint -Model $Model -KeyFile $KeyFile -CredentialsJson $CredentialsJson -Executable $Executable -ProfileHome $ProfileHome -PiConfigPath $PiConfigPath -PromptFile $PromptFile -DryRun:$DryRun -ClientArgs $ClientArgs
exit $LASTEXITCODE
