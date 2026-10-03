$ErrorActionPreference = 'Stop'

function Read-HerdrProxySettings {
    $path = if ($env:HERDR_PROXY_SETTINGS) { $env:HERDR_PROXY_SETTINGS } else { Join-Path $env:LOCALAPPDATA 'CLIProxyAPI/herdr-proxy/settings.json' }
    try { $settings = Get-Content -LiteralPath $path -Raw | ConvertFrom-Json -AsHashtable }
    catch { throw 'Proxy settings missing or invalid. Run tools/install-herdr-proxy.ps1.' }
    foreach ($name in @('endpoint', 'credentials', 'herdr', 'claude', 'pi', 'codex', 'shimDirectory', 'piConfig', 'codexConfig', 'root')) {
        if (-not $settings[$name]) { throw "Missing proxy setting: $name" }
    }
    . (Join-Path $settings.root 'ops/havok-fleet/common.ps1')
    Assert-FleetEndpoint $settings.endpoint
    return $settings
}

function Assert-HerdrProxyExecutable([string]$Path, [string]$ShimDirectory) {
    if (-not [IO.Path]::IsPathFullyQualified($Path) -or -not (Test-Path -LiteralPath $Path -PathType Leaf)) { throw 'Real runtime executable missing or not absolute.' }
    $resolved = (Resolve-Path -LiteralPath $Path).Path
    $shim = [IO.Path]::GetFullPath($ShimDirectory).TrimEnd('\') + '\'
    if ($resolved.StartsWith($shim, [StringComparison]::OrdinalIgnoreCase) -or (Get-Item -LiteralPath $Path).LinkType) { throw 'Runtime recursion or linked executable refused.' }
}

function Get-HerdrProxyPreflight($Settings) {
    $result = [ordered]@{ gateway = 'unhealthy'; endpoint = $Settings.endpoint; routing = 'unknown'; models = 0; pools = @{ claude = $null; codex = $null; antigravity = $null }; management = 'unavailable'; runtimes = @{}; herdr = ''; failures = [Collections.Generic.List[string]]::new() }
    try {
        $secrets = Get-Content -LiteralPath $Settings.credentials -Raw | ConvertFrom-Json
        $key = [string]$secrets.client_api_key
        if ([string]::IsNullOrWhiteSpace($key) -or $key -match '\s') { throw 'Invalid key' }
        $uri = [uri]$Settings.endpoint
        $socket = [Net.Sockets.TcpClient]::new()
        try { $task = $socket.ConnectAsync($uri.Host, $uri.Port); if (-not $task.Wait(3000)) { throw 'Listener unavailable' }; $task.GetAwaiter().GetResult() } finally { $socket.Dispose() }
        $catalog = Invoke-RestMethod ($Settings.endpoint + '/v1/models') -Headers @{ Authorization = "Bearer $key" } -TimeoutSec 10
        if (-not $catalog.data.Count) { throw 'Empty catalog' }
        $result.models = @($catalog.data).Count
        $result.gateway = 'healthy'
    } catch { $result.failures.Add('CLIProxy unavailable or authenticated model catalog failed.') }
    if ($result.gateway -eq 'healthy' -and $secrets.management_key) {
        try {
            $routing = Invoke-RestMethod ($Settings.endpoint + '/v8/management/routing/reset-aware') -Headers @{ Authorization = ('Bearer ' + $secrets.management_key) } -TimeoutSec 10
            $result.management = 'available'
            $result.routing = [string]$routing.strategy
            foreach ($provider in @('claude', 'codex', 'antigravity')) { $result.pools[$provider] = @($routing.candidates | Where-Object provider -EQ $provider).Count }
            if ($routing.strategy -ne 'reset-aware' -or -not $routing.enabled -or $routing.preview) { $result.failures.Add('Runtime reset-aware strategy is not active.') }
        } catch { $result.failures.Add('Configured management access failed; routing cannot be verified.') }
    }
    foreach ($runtime in @('claude', 'pi', 'codex')) {
        try {
            Assert-HerdrProxyExecutable $Settings[$runtime] $Settings.shimDirectory
            if ([IO.Path]::GetExtension($Settings[$runtime]) -notin @('.exe', '.ps1')) { throw 'Runtime requires direct EXE or PowerShell argv handling.' }
            foreach ($extension in @('cmd', 'ps1')) {
                if (-not (Test-Path -LiteralPath (Join-Path $Settings.shimDirectory "$runtime.$extension"))) { throw 'Shim missing' }
            }
            if (-not (Test-Path -LiteralPath (Join-Path $Settings.root 'scripts/launchers/windows/launch-cliproxy.ps1'))) { throw 'Launcher missing' }
            $result.runtimes[$runtime] = 'ready'
        } catch { $result.runtimes[$runtime] = 'unsafe'; $result.failures.Add("$runtime proxy launcher unsafe or missing.") }
    }
    try {
        $pi = Get-Content -LiteralPath $Settings.piConfig -Raw | ConvertFrom-Json
        if ($pi.providers.cliproxy.api -ne 'openai-responses' -or $pi.providers.cliproxy.baseUrl.TrimEnd('/') -ne ($Settings.endpoint + '/v1') -or $pi.providers.cliproxy.apiKey -ne '${CLIPROXY_API_KEY}') { throw 'Pi provider unsafe' }
        if (-not @($pi.providers.cliproxy.models | Where-Object id -EQ $Settings.model)) { throw 'Pi proxy model missing' }
    } catch { $result.failures.Add('Pi cliproxy provider/model configuration unsafe or missing.') }
    try {
        $profile = Get-Content -LiteralPath $Settings.codexProfile -Raw
        $main = Get-Content -LiteralPath $Settings.codexConfig -Raw
        $rootProfile = ($profile -split '(?m)^\s*\[')[0]
        if ($rootProfile -notmatch '(?m)^model_provider\s*=\s*"cliproxy"\s*(#.*)?$') { throw 'Codex profile unsafe' }
        if ($rootProfile -match '(?m)^\s*(base_url|openai_base_url)\s*=') { throw 'Unsupported endpoint override' }
        $providerBody = ''
        foreach ($document in @($main, $profile)) {
            $sections = [regex]::Matches($document, '(?ms)^\[model_providers\.cliproxy\]\s*\r?\n(?<body>.*?)(?=^\[|\z)')
            if ($sections.Count -gt 1) { throw 'Duplicate Codex provider definition' }
            if ($sections.Count) { $providerBody = $sections[0].Groups['body'].Value }
        }
        foreach ($pattern in @('(?m)^env_key\s*=\s*"CLIPROXY_API_KEY"\s*(#.*)?$', '(?m)^requires_openai_auth\s*=\s*false\s*(#.*)?$', '(?m)^wire_api\s*=\s*"responses"\s*(#.*)?$', ('(?m)^base_url\s*=\s*"' + [regex]::Escape($Settings.endpoint + '/v1') + '"\s*(#.*)?$'))) {
            if ($providerBody -notmatch $pattern) { throw 'Codex provider unsafe' }
        }
    } catch { $result.failures.Add('Codex cliproxy profile configuration unsafe or missing.') }
    try {
        Assert-HerdrProxyExecutable $Settings.herdr $Settings.shimDirectory
        $credentialSnapshot = @{}
        foreach ($name in @('CLIPROXY_API_KEY', 'OPENAI_API_KEY', 'ANTHROPIC_API_KEY', 'ANTHROPIC_AUTH_TOKEN', 'CLAUDE_CODE_OAUTH_TOKEN')) {
            $credentialSnapshot[$name] = [Environment]::GetEnvironmentVariable($name, 'Process')
            [Environment]::SetEnvironmentVariable($name, $null, 'Process')
        }
        try { $result.herdr = (& $Settings.herdr --version | Out-String).Trim() }
        finally { foreach ($name in $credentialSnapshot.Keys) { [Environment]::SetEnvironmentVariable($name, $credentialSnapshot[$name], 'Process') } }
        if ($LASTEXITCODE -ne 0 -or $result.herdr -ne 'herdr 0.9.3') { throw 'Herdr version unsupported' }
    } catch { $result.failures.Add('Herdr 0.9.3 executable unavailable.') }
    try {
        if (-not (Test-Path -LiteralPath $Settings.herdrConfig)) { throw 'Isolated Herdr config missing' }
        $config = Get-Content -LiteralPath $Settings.herdrConfig -Raw
        Assert-HerdrProxyExecutable $Settings.shellEngine $Settings.shimDirectory
        Assert-HerdrProxyExecutable $Settings.shellExecutable $Settings.shimDirectory
        $shellPattern = [regex]::Escape("default_shell = '$($Settings.shellExecutable.Replace('\', '/'))'")
        if ($config -notmatch $shellPattern) { throw 'Unsafe pane shell' }
        $priorPath = $env:PATH
        try {
            $env:PATH = $Settings.shimDirectory + ';' + $priorPath
            foreach ($runtime in @('claude', 'pi', 'codex')) {
                $resolved = Get-Command $runtime -CommandType Application,ExternalScript | Select-Object -First 1
                if (-not $resolved.Source.StartsWith($Settings.shimDirectory + '\', [StringComparison]::OrdinalIgnoreCase)) { throw 'Shim interception failed' }
            }
        } finally { $env:PATH = $priorPath }
    } catch { $result.failures.Add('Process-local PATH shim or isolated shell configuration unsafe.') }
    $key = $null; $secrets = $null
    return [pscustomobject]$result
}

function Show-HerdrProxyStatus($Status) {
    Write-Output 'HAVOK HERDR PROXY'
    Write-Output ('Gateway          ' + $Status.gateway)
    Write-Output ('Gateway URL      ' + $Status.endpoint)
    Write-Output ('Routing          ' + $Status.routing)
    Write-Output ('Models           ' + $Status.models)
    foreach ($provider in @('claude', 'codex', 'antigravity')) {
        $count = if ($null -eq $Status.pools[$provider]) { 'unknown' } else { [string]$Status.pools[$provider] }
        Write-Output (('{0,-17}' -f ($provider + ' pool')) + $count)
    }
    foreach ($runtime in @('claude', 'pi', 'codex')) { Write-Output (('{0,-17}' -f ($runtime + ' launcher')) + $Status.runtimes[$runtime]) }
    Write-Output ('Management       ' + $Status.management)
    Write-Output ('Herdr            ' + $Status.herdr)
    foreach ($failure in $Status.failures) { Write-Output ('FAIL: ' + $failure) }
}

function Show-HerdrProxyPoolFailure($Settings, [string]$Provider, [string]$Model) {
    try {
        $secrets = Get-Content -LiteralPath $Settings.credentials -Raw | ConvertFrom-Json
        if (-not $secrets.management_key) { return }
        $url = $Settings.endpoint + '/v8/management/routing/reset-aware?provider=' + [uri]::EscapeDataString($Provider)
        if ($Model) { $url += '&model=' + [uri]::EscapeDataString($Model) }
        $routing = Invoke-RestMethod $url -Headers @{ Authorization = ('Bearer ' + $secrets.management_key) } -TimeoutSec 10
        if ($routing.enabled -and -not @($routing.candidates | Where-Object eligible).Count) {
            Write-Output ('PROXY POOL EXHAUSTED: ' + $Provider + '. Direct relogin is disabled.')
            $resets = @($routing.candidates | ForEach-Object { $_.short_window_reset_at; $_.longest_window_reset_at; $_.cooldown_until } | Where-Object { $_ } | Sort-Object)
            if ($resets.Count) { Write-Output ('Earliest recorded reset/cooldown: ' + $resets[0] + '. Check all applicable windows before resuming.') }
        }
    } catch { Write-Output 'Proxy capacity diagnostics unavailable. Direct relogin is disabled.' }
    finally { $secrets = $null }
}
