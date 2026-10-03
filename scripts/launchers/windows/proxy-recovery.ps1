# This helper is intentionally safe even when invoked by an old cap guard.
if ($env:HERDR_PROXY_MODE -eq '1' -or $env:CLIPROXY_MODE -eq '1') {
    Write-Output 'Proxy mode: direct relogin is disabled. CLIProxy owns account failover.'
    Write-Output 'If no eligible account remains: PROXY POOL EXHAUSTED. Run herdr-proxy status and inspect gateway reset-aware diagnostics for reset times.'
    exit 1
}
Write-Output 'This proxy recovery helper does not authenticate accounts. Use your existing direct-mode recovery explicitly.'
exit 1
