# Opt-in CLIProxyAPI launchers

`windows/herdr-proxy.ps1` is the top-level local Windows orchestration launcher.
Install its command with `tools/install-herdr-proxy.ps1`; see
[Herdr proxy operations](../../docs/herdr-proxy.md). Ordinary `herdr` is unchanged.

See the [fleet operator guide](../../docs/havok-fleet.md) for complete setup and credential provisioning.

Windows entry points live in `windows/`. Use PowerShell 7.4+ and keep their repository-relative common helper available. Parameters include `-Endpoint` (origin, default `http://127.0.0.1:8317`), `-Model`, `-KeyFile`, optional existing `-CredentialsJson`, `-Executable`, and `-DryRun`. Dry runs print an operation plan without reading credentials, connecting, launching, or writing configuration. Claude leaves its normal model default intact unless explicitly supplied. Codex uses an isolated `-ProfileHome`; Pi uses `-PiConfigPath`. The launchers preserve inherited environment values when they return.

POSIX templates live in `posix/`; remote bootstrap installs them as `pi-proxy`, `claude-proxy`, and (when available) `codex-proxy`, with `cliproxy-env.sh` alongside. Python 3.11+ is required. They accept `CLIPROXY_BASE_URL` (an HTTPS origin or loopback HTTP origin, without `/v1`), `CLIPROXY_MODEL`, and `CLIPROXY_KEY_FILE` environment overrides. Nonsecret `CLIPROXY_BASE_URL` and `CLIPROXY_MODEL` defaults may be placed in `~/.config/cliproxy/launcher.env`, parsed as data rather than executed. `CLIPROXY_DRY_RUN=1` validates settings and prints a plan without reading the key or writing config. Keys require user ownership, a mode-700 parent, and a mode-600 file; links are refused.

Claude receives `ANTHROPIC_BASE_URL` and `ANTHROPIC_AUTH_TOKEN`; a competing `ANTHROPIC_API_KEY` is removed for the proxy child. POSIX accepts `CLIPROXY_CLAUDE_MODEL` for a Claude-specific model override.

Codex uses `CLIPROXY_API_KEY`, `wire_api = "responses"`, and an isolated `~/.codex-cliproxy` home. Windows generates both standard config and the current Codex named `cliproxy.config.toml` profile, then runs `--no-daemon --profile cliproxy`. POSIX requires the bootstrap-generated profile and runs `--profile cliproxy` with a nonsecret endpoint override.

Pi uses its actual custom-provider models.json mechanism: `api: "openai-responses"`, `baseUrl: "<origin>/v1"`, and `${CLIPROXY_API_KEY}` environment interpolation. Both variants merge only the opt-in provider, preserving other providers and existing model metadata, and invoke `--model cliproxy/<model>`. Pi's installed custom-model documentation explicitly supports `${NAME}` interpolation and leading `!command` key readers; the latter was used by the original local setup.

Running opt-in Pi or Windows Codex can write their managed configuration. No credentials are persisted in those generated files. Ordinary `pi`, `claude`, and `codex` commands retain their direct-provider configuration. Secret values are never put in client command arguments.
