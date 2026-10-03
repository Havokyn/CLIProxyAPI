param([switch]$Json)
$ErrorActionPreference = 'Stop'
& python (Join-Path $PSScriptRoot 'upstream_sync.py') status $(if ($Json) { '--json' })
exit $LASTEXITCODE
