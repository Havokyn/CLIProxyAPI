param()
$ErrorActionPreference = 'Stop'
$repo = Split-Path $PSScriptRoot -Parent
$files = @('tools', 'ops/havok-fleet', 'scripts/launchers/windows', 'tests/fleet', 'tests/local_ci') | ForEach-Object {
    Get-ChildItem -LiteralPath (Join-Path $repo $_) -Filter '*.ps1' -File -Recurse
}
foreach ($file in $files) {
    $tokens = $null; $errors = $null
    [System.Management.Automation.Language.Parser]::ParseFile($file.FullName, [ref]$tokens, [ref]$errors) | Out-Null
    foreach ($error in $errors) {
        Write-Output "$($file.Name):$($error.Extent.StartLineNumber): PowerShell parse error"
    }
    if ($errors.Count) { exit 1 }
}
Write-Output 'PowerShell syntax passed.'
