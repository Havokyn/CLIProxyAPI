$ErrorActionPreference = 'Stop'
$repo = (Resolve-Path (Join-Path $PSScriptRoot '../..')).Path
$pwsh = (Get-Command pwsh -CommandType Application | Select-Object -First 1).Source
$proxy = Join-Path $repo 'scripts/launchers/windows/herdr-proxy.ps1'
$temporary = Join-Path ([IO.Path]::GetTempPath()) ('herdr-proxy-test-' + [guid]::NewGuid())
$null = New-Item -ItemType Directory -Path $temporary
$server = $null
try {
    $root = Join-Path $temporary 'root'; $null = New-Item -ItemType Directory $root
    $shim = Join-Path $temporary 'shims'; $null = New-Item -ItemType Directory $shim
    $capture = Join-Path $temporary 'capture.json'
    $herdrCapture = Join-Path $temporary 'herdr.json'
    $portFile = Join-Path $temporary 'port.txt'
    $credentials = Join-Path $temporary 'credentials.json'
    $piConfig = Join-Path $temporary 'models.json'
    $codexConfig = Join-Path $temporary 'codex.toml'
    $herdrConfig = Join-Path $temporary 'herdr.toml'
    $settingsPath = Join-Path $temporary 'settings.json'

    [IO.File]::WriteAllText($credentials, (@{ client_api_key = 'test-only-client-key'; management_key = 'test-only-management-key' } | ConvertTo-Json))
    [IO.File]::WriteAllText($herdrConfig, "[terminal]`ndefault_shell = '$($pwsh.Replace('\', '/'))'`nshell_mode = 'non_login'`n")

    $serverScript = Join-Path $temporary 'server.py'
    [IO.File]::WriteAllText($serverScript, @'
import json, sys
from http.server import BaseHTTPRequestHandler, HTTPServer
class H(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.headers.get('Authorization') not in ('Bearer test-only-client-key', 'Bearer test-only-management-key'):
            self.send_response(401); self.end_headers(); return
        if self.path == '/v1/models': body = {'data': [{'id': 'gpt-6-sol'}, {'id': 'claude-sonnet'}]}
        elif self.path == '/v8/management/routing/reset-aware': body = {'strategy':'reset-aware','enabled':True,'preview':False,'candidates':[{'provider':'claude'},{'provider':'claude'},{'provider':'codex'},{'provider':'antigravity'}]}
        else: self.send_response(404); self.end_headers(); return
        raw=json.dumps(body).encode(); self.send_response(200); self.send_header('Content-Type','application/json'); self.send_header('Content-Length',str(len(raw))); self.end_headers(); self.wfile.write(raw)
    def log_message(self, *_): pass
s=HTTPServer(('127.0.0.1',0),H); open(sys.argv[1],'w').write(str(s.server_port)); s.serve_forever()
'@)
    $python = (Get-Command python -CommandType Application | Select-Object -First 1).Source
    $server = Start-Process -WindowStyle Hidden -FilePath $python -ArgumentList "`"$serverScript`"", "`"$portFile`"" -PassThru
    for ($i=0; $i -lt 50 -and -not (Test-Path $portFile); $i++) { Start-Sleep -Milliseconds 100 }
    if (-not (Test-Path $portFile)) { throw 'Fake HTTP server did not start.' }
    $endpoint = 'http://127.0.0.1:' + (Get-Content $portFile -Raw).Trim()
    [IO.File]::WriteAllText($piConfig, (@{ providers = @{ cliproxy = @{ api = 'openai-responses'; baseUrl = "$endpoint/v1"; apiKey = '${CLIPROXY_API_KEY}'; models = @(@{ id = 'gpt-6-sol' }) } } } | ConvertTo-Json -Depth 10))
    [IO.File]::WriteAllText($codexConfig, "model_provider = `"cliproxy`"`n[model_providers.cliproxy]`nbase_url = `"$endpoint/v1`"`nenv_key = `"CLIPROXY_API_KEY`"`nwire_api = `"responses`"`nrequires_openai_auth = false`n")

    $fakeRuntime = Join-Path $temporary 'runtime.ps1'
    [IO.File]::WriteAllText($fakeRuntime, @'
$record = @{ args = @($args); cliproxy_key = $env:CLIPROXY_API_KEY; anthropic_token = $env:ANTHROPIC_AUTH_TOKEN; anthropic_base = $env:ANTHROPIC_BASE_URL; codex_home = $env:CODEX_HOME; path = $env:PATH }
[IO.File]::WriteAllText($env:HERDR_PROXY_TEST_CAPTURE, ($record | ConvertTo-Json -Compress))
exit 0
'@)
    $fakeHerdrPs = Join-Path $temporary 'fake-herdr.ps1'
    [IO.File]::WriteAllText($fakeHerdrPs, @'
if ($args.Count -eq 1 -and $args[0] -eq '--version') { Write-Output 'herdr 0.9.3'; exit 0 }
[IO.File]::WriteAllText($env:HERDR_PROXY_HERDR_CAPTURE, (@{ args = @($args); path = $env:PATH; mode = $env:HERDR_PROXY_MODE; inherited_key = [bool]($env:CLIPROXY_API_KEY -or $env:OPENAI_API_KEY -or $env:ANTHROPIC_API_KEY -or $env:ANTHROPIC_AUTH_TOKEN) } | ConvertTo-Json -Compress))
exit 0
'@)
    function Write-Cmd($path, $script) {
        [IO.File]::WriteAllText($path, "@echo off`r`n`"$pwsh`" -NoLogo -NoProfile -File `"$script`" %*`r`nexit /b %ERRORLEVEL%`r`n")
    }
    $fakeHerdr = Join-Path $temporary 'herdr.cmd'; Write-Cmd $fakeHerdr $fakeHerdrPs
    $runtimeExe = $fakeRuntime
    $bridge = Join-Path $repo 'scripts/launchers/windows/herdr-proxy-runtime.ps1'
    foreach ($name in @('claude','pi','codex')) {
        [IO.File]::WriteAllText((Join-Path $shim "$name.cmd"), "@echo off`r`n`"$pwsh`" -NoLogo -NoProfile -File `"$bridge`" $name %*`r`nexit /b %ERRORLEVEL%`r`n")
        [IO.File]::WriteAllText((Join-Path $shim "$name.ps1"), "& `"$bridge`" $name @args`nexit `$LASTEXITCODE`n")
    }
    $codexProfile = Join-Path $temporary 'cliproxy.config.toml'
    [IO.File]::WriteAllText($codexProfile, "model_provider = `"cliproxy`"`nmodel = `"gpt-6-sol`"`n")
    $settings = @{ shellEngine = $pwsh; shellExecutable = $pwsh; root = $repo; endpoint = $endpoint; credentials = $credentials; herdr = $fakeHerdr; claude = $runtimeExe; pi = $runtimeExe; codex = $runtimeExe; model = 'gpt-6-sol'; piConfig = $piConfig; codexConfig = $codexConfig; codexProfile = $codexProfile; shimDirectory = $shim; herdrConfig = $herdrConfig }
    [IO.File]::WriteAllText($settingsPath, ($settings | ConvertTo-Json -Depth 10))
    $env:HERDR_PROXY_SETTINGS = $settingsPath; $env:HERDR_PROXY_TEST_CAPTURE = $capture; $env:HERDR_PROXY_HERDR_CAPTURE = $herdrCapture

    $status = & $pwsh -NoLogo -NoProfile -File $proxy status 2>&1
    if ($LASTEXITCODE -ne 0 -or ($status -join "`n") -notmatch 'Gateway\s+healthy' -or ($status -join "`n") -notmatch 'Models\s+2' -or ($status -join "`n") -notmatch 'Routing\s+reset-aware') { throw 'Healthy status did not report gateway, model count, and routing.' }

    Remove-Item -LiteralPath $herdrCapture -ErrorAction SilentlyContinue
    $beforePath = $env:PATH
    $dry = & $pwsh -NoLogo -NoProfile -File $proxy --dry-run -- --help 2>&1
    if ($LASTEXITCODE -ne 0 -or (Test-Path $herdrCapture) -or ($dry -join "`n") -match 'fake-client-key') { throw 'DryRun launched Herdr or exposed credentials.' }
    if ($env:PATH -cne $beforePath) { throw 'DryRun changed parent PATH.' }

    $launch = & $pwsh -NoLogo -NoProfile -File $proxy -- --help 2>&1
    if ($LASTEXITCODE -ne 0) { throw 'Proxy launch failed with fake Herdr.' }
    $herdrRecord = Get-Content $herdrCapture -Raw | ConvertFrom-Json
    if (($herdrRecord.args -join ' ') -notmatch '--session havok-proxy-' -or ($herdrRecord.args -join ' ') -notmatch '--help' -or $herdrRecord.mode -ne '1' -or $herdrRecord.inherited_key) { throw 'Herdr argument passthrough, marker, or credential scrubbing failed.' }
    if ($env:PATH -cne $beforePath) { throw 'Proxy launch changed parent PATH.' }
    $pathCapture = $herdrRecord.path
    if ($pathCapture -notmatch [regex]::Escape($shim)) { throw 'Subprocess did not receive the private shim PATH.' }
    Remove-Item Env:HERDR_PROXY_MODE -ErrorAction SilentlyContinue
    $normalPath = $env:PATH
    & $fakeHerdr --help | Out-Null
    $normalRecord = Get-Content $herdrCapture -Raw | ConvertFrom-Json
    if ($normalRecord.mode -or $normalRecord.path -ne $normalPath) { throw 'Normal Herdr invocation inherited proxy marker or changed PATH.' }

    [Environment]::SetEnvironmentVariable('HERDR_PROXY_MODE', $null, 'Process')
    Remove-Item Env:HERDR_PROXY_MODE -ErrorAction SilentlyContinue
    $shimRun = & (Join-Path $shim 'pi.cmd') '--sentinel' 'two words' 2>&1
    if ($LASTEXITCODE -eq 0) { throw 'Runtime shim accepted missing HERDR_PROXY_MODE.' }
    $env:HERDR_PROXY_MODE = '1'
    $shimRun = & (Join-Path $shim 'pi.cmd') '--sentinel' 'two words' 2>&1
    if ($LASTEXITCODE -ne 0) { throw 'Runtime shim failed fake execution.' }
    $runtimeRecord = Get-Content $capture -Raw | ConvertFrom-Json
    if ($runtimeRecord.cliproxy_key -ne 'test-only-client-key' -or ($runtimeRecord.args -join ' ') -notmatch '--sentinel two words' -or ($shimRun -join "`n") -match 'test-only-client-key') { throw 'Pi shim did not preserve args, inject key via env, or suppress key output.' }
    $shimRun = & (Join-Path $shim 'pi.ps1') '--literal' '%CLIPROXY_API_KEY%' 2>&1
    if ($LASTEXITCODE -ne 0) { throw 'PowerShell runtime shim failed literal percent argument.' }
    $runtimeRecord = Get-Content $capture -Raw | ConvertFrom-Json
    if (($runtimeRecord.args -join ' ') -notmatch [regex]::Escape('--literal %CLIPROXY_API_KEY%')) { throw 'PowerShell shim expanded or altered literal percent argument.' }
    $shimRun = & (Join-Path $shim 'claude.cmd') '--sentinel' 'two words' 2>&1
    if ($LASTEXITCODE -ne 0) { throw 'Claude runtime shim failed fake execution.' }
    $runtimeRecord = Get-Content $capture -Raw | ConvertFrom-Json
    if ($runtimeRecord.anthropic_token -ne 'test-only-client-key' -or $runtimeRecord.anthropic_base -ne $endpoint -or ($runtimeRecord.args -join ' ') -notmatch '--sentinel two words' -or ($runtimeRecord.args -join ' ') -match 'test-only-client-key') { throw 'Claude shim did not preserve args or auth environment.' }
    $shimRun = & (Join-Path $shim 'codex.cmd') '--sentinel' 'two words' 2>&1
    if ($LASTEXITCODE -ne 0) { throw 'Codex runtime shim failed fake execution.' }
    $runtimeRecord = Get-Content $capture -Raw | ConvertFrom-Json
    if (($runtimeRecord.args -join ' ') -notmatch '--sentinel two words' -or ($runtimeRecord.args -join ' ') -match 'test-only-client-key' -or $runtimeRecord.codex_home -ne (Split-Path $codexConfig)) { throw 'Codex shim did not preserve args, CODEX_HOME, or credential safety.' }
    $login = & (Join-Path $shim 'pi.cmd') login 2>&1
    if ($LASTEXITCODE -eq 0 -or ($login -join "`n") -notmatch 'No direct fallback|refused') { throw 'Login fallback was not refused.' }
    foreach ($arg in @('-cfoo', '--profile')) {
        $blocked = & (Join-Path $shim 'codex.cmd') $arg 2>&1
        if ($LASTEXITCODE -eq 0) { throw "Codex override $arg was accepted." }
    }
    $env:HERDR_PROXY_MODE = '1'
    $recovery = & (Join-Path $repo 'scripts/launchers/windows/proxy-recovery.ps1') 2>&1
    if ($LASTEXITCODE -eq 0 -or ($recovery -join "`n") -notmatch 'relogin is disabled' -or ($recovery -join "`n") -match '(?i)(run .*login|authenticate account)') { throw 'Proxy recovery did not suppress direct relogin.' }
    [Environment]::SetEnvironmentVariable('HERDR_PROXY_MODE', $null, 'Process'); Remove-Item Env:HERDR_PROXY_MODE -ErrorAction SilentlyContinue

    $installDir = Join-Path $temporary 'bin'; $stateDir = Join-Path $temporary 'install-state'; $installPi = Join-Path $temporary 'install-models.json'; $null = New-Item -ItemType Directory $installDir
    [IO.File]::WriteAllText($installPi, (@{ providers = @{ direct = @{ baseUrl = 'https://direct.invalid/v1'; models = @(@{ id = 'direct-model' }) } } } | ConvertTo-Json -Depth 8))
    $normalPi = Get-Content $installPi -Raw; $normalCodex = Get-Content $codexConfig -Raw
    $oldPath = $env:PATH; $env:PATH = $installDir + ';' + $oldPath
    $installer = Join-Path $repo 'tools/install-herdr-proxy.ps1'; $uninstaller = Join-Path $repo 'tools/uninstall-herdr-proxy.ps1'
    $installOutput = & $pwsh -NoLogo -NoProfile -File $installer -InstallDirectory $installDir -StateDirectory $stateDir -CredentialsJson $credentials -HerdrExecutable $fakeHerdr -ClaudeExecutable $runtimeExe -PiExecutable $runtimeExe -CodexExecutable $runtimeExe -PiConfig $installPi -CodexConfig $codexConfig -HerdrConfig (Join-Path $temporary 'installed-herdr.toml') -Endpoint $endpoint -Model 'gpt-6-sol' 2>&1
    if ($LASTEXITCODE -ne 0) { throw ('Installer failed: ' + ($installOutput -join ' | ')) }
    if ($LASTEXITCODE -ne 0 -or -not (Test-Path (Join-Path $installDir 'herdr-proxy.cmd')) -or (Get-Content $installPi -Raw) -eq $normalPi -or (Get-Content $codexConfig -Raw) -ne $normalCodex) { throw 'Installer did not create owned command or preserve normal Codex configuration.' }
    $installedPi = Get-Content $installPi -Raw | ConvertFrom-Json
    if (-not $installedPi.providers.cliproxy -or -not (Test-Path (Join-Path $stateDir 'install.json'))) { throw 'Installer did not add the opt-in Pi provider or manifest.' }
    $shell = Join-Path $stateDir 'herdr-proxy-shell.exe'
    $shellDenied = & $shell 2>&1
    if ($LASTEXITCODE -ne 1) { throw 'Private shell accepted missing proxy marker.' }
    $env:HERDR_PROXY_MODE = '1'; $env:HERDR_PROXY_SHELL_ENGINE = $pwsh
    $env:PATH = $shim + ';' + $env:PATH
    $env:HERDR_PROXY_PATH = $env:PATH
    $shellOutput = 'Get-Command claude | Select-Object -ExpandProperty Source; exit' | & $shell 2>&1
    if ($LASTEXITCODE -ne 0 -or ($shellOutput -join "`n") -notmatch [regex]::Escape((Join-Path $shim 'claude.ps1'))) { throw 'Private profile-free shell failed runtime interception.' }
    Remove-Item Env:HERDR_PROXY_MODE,Env:HERDR_PROXY_SHELL_ENGINE,Env:HERDR_PROXY_PATH
    $env:PATH = $installDir + ';' + $oldPath
    & $pwsh -NoLogo -NoProfile -File $uninstaller -StateDirectory $stateDir | Out-Null
    if ($LASTEXITCODE -ne 0 -or (Get-Content $installPi -Raw) -ne $normalPi -or (Get-Content $codexConfig -Raw) -ne $normalCodex -or (Test-Path (Join-Path $installDir 'herdr-proxy.cmd'))) { throw 'Uninstaller did not restore Pi or remove owned files.' }
    $env:PATH = $oldPath

    $unsafe = $settings.Clone(); $unsafe.pi = Join-Path $shim 'pi.cmd'; [IO.File]::WriteAllText($settingsPath, ($unsafe | ConvertTo-Json -Depth 10))
    $doctor = & $pwsh -NoLogo -NoProfile -File $proxy doctor 2>&1
    if ($LASTEXITCODE -eq 0 -or ($doctor -join "`n") -notmatch 'unsafe') { throw 'Doctor accepted recursive runtime configuration.' }
    [IO.File]::WriteAllText($settingsPath, ($settings | ConvertTo-Json -Depth 10))
    [IO.File]::WriteAllText($codexProfile, "model_provider = `"cliproxy`"`nmodel = `"gpt-6-sol`"`n[model_providers.cliproxy]`nbase_url = `"https://api.openai.com/v1`"`n")
    $profileDoctor = & $pwsh -NoLogo -NoProfile -File $proxy doctor 2>&1
    if ($LASTEXITCODE -eq 0 -or ($profileDoctor -join "`n") -notmatch 'unsafe|Codex') { throw 'Doctor accepted an unsafe profile override over a valid main Codex provider.' }
    [IO.File]::WriteAllText($codexProfile, "model_provider = `"cliproxy`"`nmodel = `"gpt-6-sol`"`n")
    $healthyDoctor = & $pwsh -NoLogo -NoProfile -File $proxy doctor 2>&1
    if ($LASTEXITCODE -ne 0 -or ($healthyDoctor -join "`n") -match 'unsafe|FAIL:') { throw 'Doctor rejected the restored valid Codex profile.' }

    Stop-Process -Id $server.Id -Force; $server = $null
    $unhealthy = & $pwsh -NoLogo -NoProfile -File $proxy status 2>&1
    if ($LASTEXITCODE -eq 0 -or ($unhealthy -join "`n") -notmatch 'NOT started|unavailable') { throw 'Unavailable gateway did not fail closed.' }
    Write-Output 'Herdr proxy fake-runtime, isolation, fail-closed, doctor, dry-run, and no-fallback checks passed.'
} finally {
    if ($oldPath) { $env:PATH = $oldPath }
    if ($server) { Stop-Process -Id $server.Id -Force -ErrorAction SilentlyContinue }
    Remove-Item Env:HERDR_PROXY_SETTINGS,Env:HERDR_PROXY_TEST_CAPTURE,Env:HERDR_PROXY_HERDR_CAPTURE,Env:HERDR_PROXY_MODE -ErrorAction SilentlyContinue
    $resolvedTemporary = [IO.Path]::GetFullPath($temporary).TrimEnd([IO.Path]::DirectorySeparatorChar)
    $expectedParent = [IO.Path]::GetFullPath([IO.Path]::GetTempPath()).TrimEnd([IO.Path]::DirectorySeparatorChar)
    if ([IO.Path]::GetDirectoryName($resolvedTemporary) -ne $expectedParent) { throw 'Temporary cleanup escaped its parent; cleanup refused.' }
    Remove-Item -LiteralPath $resolvedTemporary -Recurse -Force -ErrorAction SilentlyContinue
}
