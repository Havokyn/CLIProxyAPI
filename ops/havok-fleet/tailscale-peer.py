"""Remote Linux peer onboarding. Emits allowlisted status, never raw daemon state."""
import json
import platform
import re
import shutil
import subprocess


def run(args):
    return subprocess.run(args, capture_output=True, text=True, timeout=120)


def onboard(hostname):
    if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9-]{0,62}", hostname):
        return {"state": "invalid_hostname", "online": False}
    system = platform.system()
    if system != "Linux":
        return {"os": system, "state": "unsupported_os", "online": False}
    installed = bool(shutil.which("tailscale"))
    if not installed:
        # Official installer is fetched to a pipe; no shell expansion of user input.
        installer = run(["curl", "-fsSL", "https://tailscale.com/install.sh"])
        if installer.returncode:
            return {"state": "installer_fetch_failed", "online": False}
        install = subprocess.run(["sudo", "-n", "sh"], input=installer.stdout,
                                 capture_output=True, text=True, timeout=300)
        if install.returncode:
            return {"state": "install_failed", "online": False}
    result = run(["sudo", "-n", "tailscale", "status", "--json"])
    state = json.loads(result.stdout) if result.returncode == 0 else {}
    if state.get("BackendState") == "Running":
        if run(["sudo", "-n", "tailscale", "set", "--hostname=" + hostname]).returncode:
            return {"state": "hostname_failed", "online": False}
    else:
        # Bound interactive join; rerunning after login verifies the completed join.
        joined = run(["sudo", "-n", "tailscale", "up", "--hostname=" + hostname, "--timeout=15s"])
        urls = re.findall(r"https://login\.tailscale\.com/[A-Za-z0-9/_?=&.%-]+", joined.stdout + joined.stderr)
        if urls:
            return {"authorization_url": urls[0]}
        if joined.returncode:
            return {"state": "join_failed", "online": False}
    result = run(["sudo", "-n", "tailscale", "status", "--json"])
    state = json.loads(result.stdout) if result.returncode == 0 else {}
    peer = state.get("Self", {})
    return {"os": system, "installed_before": installed, "state": state.get("BackendState", "unknown"),
            "online": bool(peer.get("Online")), "dns_name": peer.get("DNSName", "").rstrip("."),
            "tailscale_ips": peer.get("TailscaleIPs", [])}


if __name__ == "__main__":
    try:
        print(json.dumps(onboard(bundle["hostname"])))
    except Exception:
        print(json.dumps({"state": "onboarding_failed", "online": False}))
