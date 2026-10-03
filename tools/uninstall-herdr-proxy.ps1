[CmdletBinding()]
param([string]$StateDirectory = (Join-Path $env:LOCALAPPDATA 'CLIProxyAPI/herdr-proxy'))
$ErrorActionPreference = 'Stop'
$manifestPath = Join-Path $StateDirectory 'install.json'
$manifest = Get-Content -LiteralPath $manifestPath -Raw | ConvertFrom-Json
# Validate every file before any deletion; never remove changed user work.
foreach ($file in $manifest.files) {
    if ((Get-FileHash -LiteralPath $file.path -Algorithm SHA256).Hash -ne $file.hash) { throw 'Installed file changed; uninstall refused.' }
}
if ((Get-FileHash -LiteralPath $manifest.piConfig -Algorithm SHA256).Hash -ne $manifest.piHash) { throw 'Pi config changed; uninstall refused to overwrite it.' }
[IO.File]::WriteAllText($manifest.piConfig, [IO.File]::ReadAllText((Join-Path $StateDirectory 'pi-models.before.json')))
foreach ($file in $manifest.files) { Remove-Item -LiteralPath $file.path }
Remove-Item -LiteralPath $manifestPath
if ($manifest.shimDirectory -and (Test-Path -LiteralPath $manifest.shimDirectory) -and -not @(Get-ChildItem -LiteralPath $manifest.shimDirectory -Force).Count) { Remove-Item -LiteralPath $manifest.shimDirectory }
Write-Output 'Owned proxy command/shims removed; original Pi config restored. PATH and normal Herdr unchanged. Stop any remaining proxy sessions explicitly.'
