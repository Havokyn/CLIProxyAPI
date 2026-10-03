# Local Windows Herdr proxy mode

Normal mode: `herdr`. Proxy mode: `herdr-proxy`.

Herdr orchestrates terminals. Claude, Pi and Codex are runtimes. CLIProxy owns
credential selection, quota observations, cooldowns, reset-aware ranking,
reserve-last policy and session affinity. Tailscale carries private network
traffic; it does not authenticate subscriptions.

## Install and use

Installation requires the local Go compiler to build the private pane shell.
From this repository, run `pwsh -NoProfile -File tools/install-herdr-proxy.ps1`.
The installer resolves real executables before creating private runtime shims,
pins the installed Herdr 0.9.3 executable, and installs `herdr-proxy.cmd` and
`herdr-proxy.ps1` in the existing user PATH directory `~/.local/bin`.
It never modifies user/system PATH or ordinary runtime commands.

```powershell
herdr-proxy --help
herdr-proxy status
herdr-proxy doctor
herdr-proxy --dry-run
herdr-proxy
herdr-proxy -- --session experiment
```

Native runtime arguments are preserved. `--` separates wrapper commands from
Herdr arguments. A native session label becomes `havok-proxy-<label>-<unique>`.
Every launch creates a fresh named server: attaching an existing direct server
would inherit that server's original environment and bypass interception.
The printed session name identifies the session to detach/stop explicitly.
Detached proxy servers remain alive as normal Herdr servers do. The wrapper
does not stop them or touch the primary workspace. Launching it again creates
another isolated session rather than reattaching. Installation/session update,
handoff and remote administration use normal Herdr explicitly; local proxy mode
refuses those operations. Remote proxy installation is a separate future task.

The private PATH exists only in the proxy process tree. A private compiled shell bootstrap starts PowerShell with -NoProfile -NoExit,
preventing profile functions from shadowing runtime shims. It restores the
inherited private PATH after startup because packaged PowerShell can rebuild
PATH. This restoration happens before interactive commands are accepted. Herdr 0.9.3 `agent start --kind` invokes canonical `claude`, `pi`, and
`codex` commands in an existing shell pane. Both CMD and PowerShell shims call
absolute real runtime paths, avoiding recursion. Absolute direct runtime paths,
manually changed PATH, and arbitrary third-party scripts are outside command
interception; use the canonical runtime commands in proxy panes.

Claude uses the existing `ANTHROPIC_BASE_URL` / `ANTHROPIC_AUTH_TOKEN` mechanism.
The existing `SR_OPUS_PROMPT` file is appended when configured. Competing direct
OAuth and cloud-provider environment switches are removed in the Claude child.
Pi explicitly selects the existing `cliproxy` custom provider and model. Only
that provider is merged during installation; direct providers remain intact.
Its key field is `${CLIPROXY_API_KEY}`, never a literal secret. Codex uses its
existing `cliproxy.config.toml` profile with the provider in normal `config.toml`;
both files are preserved. `--no-daemon` keeps its environment local. The client
key is read by each runtime wrapper and passed only through child environment.
Herdr receives no OAuth accounts, token files, client key or management key.

Preflight checks TCP connectivity, authenticated `/v1/models`, runtime paths,
recursion protection, proxy configuration, isolated pane shell and PATH
interception. When a management key is configured, it must authenticate and
`/v8/management/routing/reset-aware` must report active `reset-aware` routing.
With no management key, counts/strategy are `unknown`; model authentication
still gates launch. Status prints counts only, never account names or keys.
Missing pools do not by themselves block unrelated runtimes; an unavailable
provider fails at its gateway request and never falls back to direct login.

Dry run performs authenticated preflight but sends no inference and launches no
Herdr process. Doctor exits nonzero on an unsafe setup. HTTP diagnostic exceptions
are suppressed because they may contain authorization material.

## Capacity recovery

In proxy mode the policy is **subscription limit → CLIProxy failover**.
It is never **subscription limit → manual relogin**.

`HERDR_PROXY_MODE=1` and `CLIPROXY_MODE=1` are inherited by all descendants.
The runtime shims refuse direct login/logout/relogin and provider/profile/config
overrides. A private `relogin` shim reports that direct recovery is disabled.
Proxy-aware external guards must check either marker before calling an absolute
relogin script, changing providers or reading subscription auth files:

```text
if HERDR_PROXY_MODE=1 or CLIPROXY_MODE=1:
    report gateway capacity failure; do not authenticate a subscription
else:
    preserve existing direct-mode recovery
```

Read-only inspection of the current Oryn repositories found `oryn-subs` usage
reporting and self-compaction guards, but no `relogin.sh` or automatic relogin
implementation. `oryn-subs` reads direct Pi auth and suggests refreshing an
expired token; it is not the gateway's source of quota truth and should not be
used as a proxy account monitor. No Oryn repository was modified. Any external
guard invoking absolute direct scripts must adopt the marker contract above;
the PATH shim cannot intercept absolute paths or an agent's prose instructions.

The observed Codex Subscription Sharing response has structured type
`usage_limit_reached`. Existing classification normalizes that type to temporary,
credential-scoped 429 capacity exhaustion even in other HTTP envelopes. Its
original upstream HTTP status was not available in the inspected preserved
records; the launcher does not guess it. Regression tests cover the exact
message with structured type, reset hints and multiple HTTP envelopes. Capacity
failures now trigger the existing coalesced authoritative read-only quota
refresh. Cooldowns remain intact while the observation updates. No authentication
invalidation, reset-credit redemption or email-specific special case is added.

Claude quota refresh uses the existing OAuth read-only `/api/oauth/usage` source
and converts validated windows to the existing normalized Auth quota state.
The central selector uses that state. No parallel quota database is created.
Only OAuth Claude credentials support this probe; API-key credentials rely on
passive upstream headers. Passive observations and read-only probes never reset
quota. New sessions rank current eligible credentials; continuations remain
sticky until the existing safe unusable-credential fallback applies.

## Troubleshooting

| Condition | Action |
|---|---|
| Gateway unavailable | Start/repair CLIProxy, then run doctor. Herdr was not started. Choose normal `herdr` explicitly for direct mode. |
| No eligible Claude account | Inspect Claude routing diagnostics and fresh quota observations; wait for recovery or repair the gateway-owned credential. Do not log Claude into another subscription. |
| No eligible Codex account | Inspect Codex routing diagnostics and authoritative usage; let the gateway select another eligible account. |
| All short-window quota exhausted | Report **PROXY POOL EXHAUSTED** with the earliest eligible reset from routing diagnostics. Wait for a fresh post-reset observation. |
| All weekly quota exhausted | Wait for the applicable weekly resets; short-window resets do not restore weekly capacity. |
| Sticky session exhaustion | Gateway may release the unusable binding according to existing safe fallback. Do not bounce accounts for healthy continuations. |
| Proxy pool exhausted | Stop new work for that provider/model. Inspect reset information. Never redeem manual reset credits automatically. |
| Management API unavailable | Configured management access failing blocks launch. Repair the key/source or gateway; do not scrape management HTML. Without a configured key diagnostics remain unknown. |
| Old inherited Herdr PATH | The installer pins the actual installed 0.9.3 executable for proxy mode; ordinary PATH remains unchanged. |

Uninstall with `pwsh -NoProfile -File tools/uninstall-herdr-proxy.ps1` after
stopping proxy sessions explicitly. Uninstall checks recorded hashes, removes
only owned files and restores the original Pi provider config. If files have
changed it refuses to overwrite them. Normal Herdr config and both Codex config
files are never rewritten. Droplet1/2/3 may have older Herdr versions; this task
does not update remote machines.
