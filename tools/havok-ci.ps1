param(
    [switch]$Fast, [switch]$Full, [switch]$NoBuild, [switch]$NoWSL,
    [switch]$InstallHook, [switch]$JsonReport
)
$ErrorActionPreference = 'Stop'
if ($Fast -and $Full) { throw 'Choose either -Fast or -Full.' }
$repo = Split-Path $PSScriptRoot -Parent
if ($InstallHook) { & (Join-Path $PSScriptRoot 'install-git-hooks.ps1') }
$python = Get-Command python -ErrorAction Stop
$arguments = @((Join-Path $PSScriptRoot 'local_ci.py'))
if ($Fast) { $arguments += '--fast' }
if ($Full) { $arguments += '--full' }
if ($NoBuild) { $arguments += '--no-build' }
if ($NoWSL) { $arguments += '--no-wsl' }
if ($JsonReport) { $arguments += '--json-report' }
& $python.Source @arguments
exit $LASTEXITCODE
