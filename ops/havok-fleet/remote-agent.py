"""SSH-side operations. Only allowlisted status leaves the machine."""
import json
import os
from pathlib import Path
import re
import shutil
import stat
import subprocess
import tempfile
import tomllib
import urllib.error
import urllib.parse
import urllib.request


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def validate_endpoint(endpoint):
    if not re.fullmatch(r"https://[A-Za-z0-9.\-:\[\]]+/?", endpoint):
        raise ValueError("invalid endpoint")
    parsed = urllib.parse.urlsplit(endpoint)
    if (parsed.scheme != "https" or not parsed.hostname or parsed.username or parsed.password
            or parsed.path not in ("", "/") or parsed.query or parsed.fragment):
        raise ValueError("invalid endpoint")
    parsed.port  # Validate invalid/out-of-range ports before any file writes.


def protected_key(home):
    directory = home / ".config/cliproxy"
    path = directory / "client.key"
    for parent in (home / ".config", directory, path):
        if parent.is_symlink():
            return None, "key_unsafe"
    if not path.is_file():
        return None, "key_missing"
    for target, mode in ((directory, 0o700), (path, 0o600)):
        info = target.stat()
        if info.st_uid != os.getuid() or stat.S_IMODE(info.st_mode) != mode:
            return None, "key_permissions_invalid"
    key = path.read_text().strip()
    if not key or any(c.isspace() for c in key) or any(ord(c) < 33 or ord(c) > 126 for c in key):
        return None, "key_invalid"
    return key, "configured"


def http_probe(endpoint, key, smoke_model=None):
    route = "/v1/responses" if smoke_model else "/v1/models"
    body = json.dumps({"model": smoke_model, "input": "Reply OK.", "max_output_tokens": 16}).encode() if smoke_model else None
    req = urllib.request.Request(endpoint + route, data=body,
                                 headers={"Authorization": "Bearer " + key, "Content-Type": "application/json"})
    try:
        with urllib.request.build_opener(NoRedirect).open(req, timeout=20) as response:
            doc = json.loads(response.read(2 * 1024 * 1024))
            if smoke_model:
                valid = doc.get("status") == "completed" and bool(doc.get("output"))
            else:
                valid = isinstance(doc.get("data"), list)
            return {"http_status": response.status, "reachable": True, "valid_response": valid,
                    "model_count": len(doc["data"]) if not smoke_model and valid else None}
    except urllib.error.HTTPError as exc:
        return {"http_status": exc.code, "reachable": True, "valid_response": False, "model_count": None}
    except Exception:
        return {"http_status": None, "reachable": False, "valid_response": False, "model_count": None}


def tailscale_status():
    if not shutil.which("tailscale"):
        return {"state": "missing", "online": False}
    try:
        result = subprocess.run(["tailscale", "status", "--json"], capture_output=True, text=True, timeout=15)
        data = json.loads(result.stdout)
        peer = data.get("Self", {})
        return {"state": data.get("BackendState", "unknown"), "online": bool(peer.get("Online")),
                "hostname": peer.get("DNSName", "").rstrip("."), "ips": peer.get("TailscaleIPs", [])}
    except Exception:
        return {"state": "unknown", "online": False}


def atomic_write(path, content, mode=0o600):
    if path.is_symlink():
        raise ValueError("unsafe destination")
    path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    # Refuse linked parents rather than write outside the intended home.
    if any(parent.is_symlink() for parent in path.parents):
        raise ValueError("unsafe parent")
    if path.exists() and path.read_text() == content and stat.S_IMODE(path.stat().st_mode) == mode:
        return
    fd, temporary = tempfile.mkstemp(dir=path.parent)
    try:
        with os.fdopen(fd, "w") as handle:
            handle.write(content)
        os.chmod(temporary, mode)
        os.replace(temporary, path)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


def pi_configuration(existing, endpoint, model):
    data = json.loads(existing) if existing else {}
    providers = data.setdefault("providers", {})
    provider = providers.setdefault("cliproxy", {})
    provider.update({"baseUrl": endpoint + "/v1", "api": "openai-responses", "apiKey": "${CLIPROXY_API_KEY}"})
    models = provider.setdefault("models", [])
    if not any(item.get("id") == model for item in models):
        models.append({"id": model, "name": model + " via CLIProxy", "reasoning": True})
    return json.dumps(data, indent=2) + "\n"


def herdr_configuration(existing, runtimes):
    marker = "# BEGIN CLIPROXY OPT-IN"
    end = "# END CLIPROXY OPT-IN"
    if marker in existing:
        if existing.count(marker) != 1 or existing.count(end) != 1:
            raise ValueError("invalid managed block")
        start_index, end_index = existing.index(marker), existing.index(end) + len(end)
        existing = existing[:start_index] + existing[end_index:]
    parsed = tomllib.loads(existing)
    bindings = {"pi": "prefix+alt+p", "claude": "prefix+alt+c", "codex": "prefix+alt+x"}
    used = {item.get("key") for item in parsed.get("keys", {}).get("command", [])}
    commands = []
    for runtime in runtimes:
        if bindings[runtime] in used:
            raise ValueError("binding conflict")
        commands.append(f'[[keys.command]]\nkey = "{bindings[runtime]}"\ntype = "pane"\ncommand = "{runtime}-proxy"\n')
    result = existing.rstrip() + "\n\n" + marker + "\n" + "\n".join(commands) + end + "\n"
    tomllib.loads(result)
    return result


def install(home, endpoint, model, files, enable_herdr, runtimes):
    pi_path = home / ".pi/agent/models.json"
    herdr_path = home / ".config/herdr/config.toml"
    # Parse and detect conflicts before touching existing config.
    pi_content = pi_configuration(pi_path.read_text() if pi_path.exists() else "", endpoint, model)
    herdr_content = None
    if enable_herdr and shutil.which("herdr"):
        herdr_content = herdr_configuration(herdr_path.read_text() if herdr_path.exists() else "", runtimes)
    launcher_config = "CLIPROXY_BASE_URL=" + endpoint + "\nCLIPROXY_MODEL=" + model + "\n"
    updates = [(home / ".config/cliproxy/launcher.env", launcher_config, 0o600),
               (pi_path, pi_content, 0o600),
               (home / ".local/bin/cliproxy-env.sh", files["cliproxy-env.sh"], 0o700)]
    for runtime in runtimes:
        updates.append((home / ".local/bin" / (runtime + "-proxy"), files[runtime + "-cliproxy"], 0o700))
    if "codex" in runtimes:
        codex = ('[model_providers.cliproxy]\nname = "CLIProxyAPI"\n'
                 f'base_url = "{endpoint}/v1"\nenv_key = "CLIPROXY_API_KEY"\n'
                 'wire_api = "responses"\nrequires_openai_auth = false\n\n'
                 f'[profiles.cliproxy]\nmodel_provider = "cliproxy"\nmodel = "{model}"\n')
        updates.append((home / ".codex-cliproxy/config.toml", codex, 0o600))
        named_profile = f'model_provider = "cliproxy"\nmodel = "{model}"\n' + codex.split('[profiles.cliproxy]')[0]
        updates.append((home / ".codex-cliproxy/cliproxy.config.toml", named_profile, 0o600))
    if herdr_content is not None:
        updates.append((herdr_path, herdr_content, 0o600))
    originals = {}
    for path, _, _ in updates:
        if path.is_symlink() or any(parent.is_symlink() for parent in path.parents):
            raise ValueError("unsafe destination")
        originals[path] = (path.read_text(), stat.S_IMODE(path.stat().st_mode)) if path.exists() else None
    completed = []
    try:
        for path, content, mode in updates:
            atomic_write(path, content, mode)
            completed.append(path)
    except Exception:
        for path in reversed(completed):
            original = originals[path]
            if original is None:
                path.unlink(missing_ok=True)
            else:
                atomic_write(path, original[0], original[1])
        raise
    return herdr_content is not None


def operate(settings, home=None):
    home = home or Path.home()
    endpoint = settings["endpoint"]
    model = settings["model"]
    validate_endpoint(endpoint)
    if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._-]*", model):
        raise ValueError("invalid model")
    mode = settings["mode"]
    if mode not in ("install", "probe"):
        raise ValueError("invalid mode")
    # Local runtimes were also installed under this conventional user prefix.
    os.environ["PATH"] = str(home / ".local/node/bin") + os.pathsep + str(home / ".local/bin") + os.pathsep + os.environ.get("PATH", "")
    runtimes = [name for name in ("pi", "claude", "codex") if shutil.which(name)]
    key, key_state = protected_key(home)
    ts = tailscale_status()
    proxy = http_probe(endpoint, key) if key else {"http_status": None, "reachable": False, "valid_response": False, "model_count": None}
    result = {"os": __import__("platform").system(), "tailscale": ts, "credential_state": key_state,
              "proxy": proxy, "runtimes": {name: name in runtimes for name in ("pi", "claude", "codex")},
              "herdr_available": bool(shutil.which("herdr")), "agent_count": None,
              "e2e_state": "not_tested", "bootstrap": "not_requested"}
    if result["herdr_available"] and os.environ.get("HERDR_ENV") == "1":
        try:
            agents = subprocess.run(["herdr", "agent", "list"], capture_output=True, text=True, timeout=15)
            if agents.returncode == 0:
                doc = json.loads(agents.stdout)
                if isinstance(doc.get("result", {}).get("agents"), list):
                    result["agent_count"] = len(doc["result"]["agents"])
        except Exception:
            pass
    if mode == "install":
        if key_state != "configured":
            result["bootstrap"] = key_state
        elif not ts["online"]:
            result["bootstrap"] = "tailscale_offline"
        elif proxy["http_status"] != 200 or not proxy["valid_response"]:
            result["bootstrap"] = "proxy_unhealthy"
        else:
            result["herdr_bindings_installed"] = install(home, endpoint, model, settings["files"], settings["herdr"], runtimes)
            result["bootstrap"] = "configured"
    result["launchers"] = {name: os.access(home / ".local/bin" / (name + "-proxy"), os.X_OK) for name in ("pi", "claude", "codex")}
    if settings.get("smoke_test") and key and proxy["valid_response"]:
        smoke = http_probe(endpoint, key, model)
        result["e2e_state"] = "verified" if smoke["http_status"] == 200 and smoke["valid_response"] else "failed"
    return result


if __name__ == "__main__":
    try:
        print(json.dumps(operate(bundle)))
    except Exception:
        print(json.dumps({"bootstrap": "failed", "probe_state": "failed"}))
