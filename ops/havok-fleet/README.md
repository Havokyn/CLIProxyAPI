# Havok fleet operations

Reusable operator tooling for this fork. Read the [operator guide](../../docs/havok-fleet.md) before onboarding machines. These scripts are explicit commands; checking out or testing this directory does not contact or modify a fleet.

Requirements: PowerShell 7.4+, OpenSSH with configured aliases and verified host keys, Python 3.11+ on remote Linux machines, and installed agent runtimes. Linux peers need noninteractive sudo for Tailscale installation/join. Missing agent runtimes are reported, never installed automatically.

| Command | Purpose |
| --- | --- |
| `bootstrap-tailscale-peer.ps1` | Install only when missing; join a private tailnet with manual browser login |
| `bootstrap-remote-cliproxy-agent.ps1` | Register Pi, install available runtime launchers, optionally add Herdr bindings |
| `verify-fleet.ps1` | Read-only status object; inference only with `-SmokeTest` |
| `write-fleet-inventory.ps1` | Write a fresh, nonsecret status inventory to local storage |
| `gateway.ps1` | Explicit Windows gateway lifecycle using an owned PID record |
| `check-secret-safety.py` | Offline source/example credential scan; prints locations only |

All operator PowerShell commands accept `-DryRun`. Dry runs do not open SSH connections, read keys, write files, install packages, or launch/stop runtimes.

Copy `fleet.example.json` to `.local/fleet.local.json` and customize it locally. Do not commit operational inventories. `inventory.example.json` is synthetic; it does not establish fleet qualification. See `inventory.schema.json` for the output contract.

```powershell
./ops/havok-fleet/bootstrap-tailscale-peer.ps1 -SshTarget compute-1 -Hostname compute-1 -DryRun
./ops/havok-fleet/bootstrap-remote-cliproxy-agent.ps1 -SshTarget compute-1 -Endpoint https://gateway.example.ts.net -DryRun
./ops/havok-fleet/verify-fleet.ps1 -FleetPath ./ops/havok-fleet/fleet.example.json -DryRun
./ops/havok-fleet/write-fleet-inventory.ps1 -FleetPath .local/fleet.local.json -LocalKeyFile /path/to/protected/client.key
```

Offline checks:

```powershell
python -m unittest discover -s tests/fleet -v
pwsh -NoProfile -File tests/fleet/test-powershell-launchers.ps1
python ops/havok-fleet/check-secret-safety.py
git diff --check
```

The SSH payload contains reusable source and nonsecret settings only. Keys are read on the destination. Errors suppress remote stderr, response bodies, and credential source contents. A missing or improperly protected key stops onboarding before configuration changes. Configuration writes are atomic per file, unchanged content is skipped, and caught installation failures roll back completed file writes. A host crash still requires inspection and a rerun; this is not a distributed transaction or service manager.
