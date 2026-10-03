import importlib.util
import json
import os
from pathlib import Path
import shutil
import stat
import subprocess
import sys
import tempfile
import types
import urllib.error
import unittest
from unittest import mock


ROOT = Path(__file__).resolve().parents[2]
OPS = ROOT / "ops" / "havok-fleet"


def load_module(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    sys.modules[name] = module
    spec.loader.exec_module(module)
    return module


agent = load_module("fleet_remote_agent", OPS / "remote-agent.py")
peer = load_module("fleet_tailscale_peer", OPS / "tailscale-peer.py")
scanner = load_module("fleet_secret_scanner", OPS / "check-secret-safety.py")


def agent_settings(mode="install", **overrides):
    settings = {
        "mode": mode,
        "endpoint": "https://proxy.invalid",
        "model": "gpt-6-sol",
        "herdr": False,
        "smoke_test": False,
        "files": {"pi-cliproxy": "#!/bin/sh\n", "claude-cliproxy": "#!/bin/sh\n", "codex-cliproxy": "#!/bin/sh\n", "cliproxy-env.sh": "#!/bin/sh\n"},
    }
    settings.update(overrides)
    return settings


def validate_shape(value, schema):
    if isinstance(schema, dict):
        if not isinstance(value, dict) or set(value) != set(schema):
            raise ValueError("object keys differ from schema")
        for key, child_schema in schema.items():
            validate_shape(value[key], child_schema)
    elif isinstance(schema, list):
        if not isinstance(value, list):
            raise ValueError("expected array")
        for item in value:
            validate_shape(item, schema[0])
    elif not isinstance(value, schema):
        raise ValueError("value has the wrong type")


FLEET_SCHEMA = {
    "schema_version": int,
    "machines": [{"label": str, "ssh_target": str, "endpoint": str, "credential_label": str}],
}
INVENTORY_SCHEMA = {
    "schema_version": int,
    "health_state": str,
    "generated_at_utc": str,
    "local": {
        "listener": str,
        "process": str,
        "models": {"state": str, "http_status": (int, type(None)), "model_count": (int, type(None))},
        "management_ui": {"state": str, "http_status": (int, type(None)), "model_count": (int, type(None))},
        "reset_aware": {"state": str, "http_status": (int, type(None))},
    },
    "machines": [{
        "label": str,
        "ssh_target": str,
        "credential_label": str,
        "status": {
            "os": str,
            "tailscale": {"state": str, "online": bool, "hostname": str, "ips": [str]},
            "credential_state": str,
            "proxy": {"http_status": (int, type(None)), "reachable": bool, "valid_response": bool, "model_count": (int, type(None))},
            "runtimes": {"pi": bool, "claude": bool, "codex": bool},
            "launchers": {"pi": bool, "claude": bool, "codex": bool},
            "herdr_available": bool,
            "agent_count": (int, type(None)),
            "e2e_state": str,
            "bootstrap": str,
        },
    }],
}


class RemoteAgentTests(unittest.TestCase):
    def test_endpoint_validation_rejects_credentials_paths_and_non_https(self):
        agent.validate_endpoint("https://proxy.invalid:443")
        for endpoint in ("http://proxy.invalid", "https://user:password@proxy.invalid",
                         "https://proxy.invalid/v1", "https://proxy.invalid?key=value",
                         "https://proxy.invalid:99999", "https://proxy.invalid#fragment"):
            with self.subTest(endpoint=endpoint), self.assertRaises(ValueError):
                agent.validate_endpoint(endpoint)

    @mock.patch.object(peer.platform, "system", return_value="Linux")
    @mock.patch.object(peer.shutil, "which", return_value="/fake/tailscale")
    @mock.patch.object(peer, "run")
    def test_peer_bootstrap_never_enables_funnel_or_serve(self, run, _which, _system):
        run.return_value = types.SimpleNamespace(returncode=0, stdout=json.dumps({
            "BackendState": "Running", "Self": {"Online": True}}), stderr="")
        self.assertTrue(peer.onboard("fixture-peer")["online"])
        for call in run.call_args_list:
            self.assertNotIn("funnel", call.args[0])
            self.assertNotIn("serve", call.args[0])

    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.home = Path(self.temporary.name)

    def tearDown(self):
        self.temporary.cleanup()

    @mock.patch.object(agent, "tailscale_status", return_value={"online": True})
    @mock.patch.object(agent, "http_probe")
    def test_missing_or_unsafe_key_never_writes(self, probe, _tailscale):
        result = agent.operate(agent_settings(), self.home)
        self.assertEqual(result["credential_state"], "key_missing")
        probe.assert_not_called()
        self.assertEqual(list(self.home.rglob("*")), [])

        key_dir = self.home / ".config" / "cliproxy"
        key_dir.mkdir(parents=True, mode=0o700)
        key_path = key_dir / "client.key"
        key_path.write_text("test-key\n")
        os.chmod(key_dir, 0o700)
        os.chmod(key_path, 0o644)
        with mock.patch.object(agent.os, "getuid", create=True, return_value=key_path.stat().st_uid):
            result = agent.operate(agent_settings(), self.home)
        self.assertEqual(result["credential_state"], "key_permissions_invalid")
        probe.assert_not_called()
        self.assertFalse((self.home / ".pi").exists())
        self.assertFalse((self.home / ".local").exists())
        self.assertFalse((self.home / ".codex-cliproxy").exists())

    @mock.patch.object(agent, "tailscale_status", return_value={"online": True})
    @mock.patch.object(agent, "protected_key", return_value=("test-key", "configured"))
    @mock.patch.object(agent, "http_probe", return_value={"http_status": 503, "reachable": True, "valid_response": False, "model_count": None})
    def test_unhealthy_proxy_never_installs(self, _probe, _key, _tailscale):
        key_dir = self.home / ".config" / "cliproxy"
        key_dir.mkdir(parents=True, mode=0o700)
        key_path = key_dir / "client.key"
        key_path.write_text("test-key\n")
        os.chmod(key_dir, 0o700)
        os.chmod(key_path, 0o600)
        result = agent.operate(agent_settings(), self.home)
        self.assertEqual(result["bootstrap"], "proxy_unhealthy")
        self.assertFalse((self.home / ".pi").exists())
        self.assertFalse((self.home / ".local").exists())

    def test_pi_merge_preserves_direct_providers_and_models(self):
        existing = json.dumps({
            "providers": {"openai": {"baseUrl": "https://direct.invalid", "models": [{"id": "direct-model"}]}},
            "other": {"kept": True},
        })
        updated = json.loads(agent.pi_configuration(existing, "https://proxy.invalid", "gpt-6-sol"))
        self.assertIn("openai", updated["providers"])
        self.assertEqual(updated["providers"]["openai"]["models"][0]["id"], "direct-model")
        self.assertEqual(updated["other"], {"kept": True})
        self.assertEqual(updated["providers"]["cliproxy"]["models"][0]["id"], "gpt-6-sol")
        self.assertEqual(updated["providers"]["cliproxy"]["apiKey"], "${CLIPROXY_API_KEY}")

    def test_installer_uses_launcher_bundle_names(self):
        files = {
            "pi-cliproxy": "pi launcher\n",
            "claude-cliproxy": "claude launcher\n",
            "codex-cliproxy": "codex launcher\n",
            "cliproxy-env.sh": "shared helper\n",
        }
        runtimes = ["pi", "claude", "codex"]
        installed_herdr = agent.install(
            self.home, "https://proxy.invalid", "gpt-6-sol", files, False, runtimes
        )
        self.assertFalse(installed_herdr)
        for runtime in runtimes:
            self.assertTrue((self.home / ".local/bin" / (runtime + "-proxy")).is_file())
        self.assertTrue((self.home / ".local/bin/cliproxy-env.sh").is_file())
        self.assertTrue((self.home / ".config/cliproxy/launcher.env").is_file())
        self.assertTrue((self.home / ".codex-cliproxy/config.toml").is_file())
        self.assertTrue((self.home / ".codex-cliproxy/cliproxy.config.toml").is_file())

    def test_install_rollback_preserves_existing_direct_pi_configuration(self):
        pi_path = self.home / ".pi/agent/models.json"
        pi_path.parent.mkdir(parents=True)
        original = json.dumps({"providers": {"openai": {"baseUrl": "https://direct.invalid/v1", "models": [{"id": "direct-model"}]}}}, indent=2) + "\n"
        pi_path.write_text(original)
        files = {
            "pi-cliproxy": "pi launcher\n",
            "claude-cliproxy": "claude launcher\n",
            "codex-cliproxy": "codex launcher\n",
            "cliproxy-env.sh": "shared helper\n",
        }
        real_atomic_write = agent.atomic_write
        call_count = 0

        def fail_second_write(path, content, mode=0o600):
            nonlocal call_count
            call_count += 1
            if call_count == 2:
                raise OSError("simulated write failure")
            return real_atomic_write(path, content, mode)

        with mock.patch.object(agent, "atomic_write", side_effect=fail_second_write):
            with self.assertRaisesRegex(OSError, "simulated write failure"):
                agent.install(self.home, "https://proxy.invalid", "gpt-6-sol", files, False, ["pi"])
        self.assertEqual(pi_path.read_text(), original)
        self.assertFalse((self.home / ".config/cliproxy/launcher.env").exists())

    def test_http_probe_http_errors_redirects_and_malformed_json(self):
        for status in (401, 302):
            error = urllib.error.HTTPError("https://proxy.invalid", status, "status", {}, None)
            with mock.patch.object(agent.urllib.request, "build_opener") as opener:
                opener.return_value.open.side_effect = error
                result = agent.http_probe("https://proxy.invalid", "unit-test-key")
            self.assertEqual(result["http_status"], status)
            self.assertTrue(result["reachable"])
            self.assertFalse(result["valid_response"])
            opener.assert_called_once_with(agent.NoRedirect)

        response = mock.MagicMock()
        response.__enter__.return_value.read.return_value = b"{not-json"
        with mock.patch.object(agent.urllib.request, "build_opener") as opener:
            opener.return_value.open.return_value = response
            malformed = agent.http_probe("https://proxy.invalid", "unit-test-key")
        self.assertEqual(malformed, {"http_status": None, "reachable": False, "valid_response": False, "model_count": None})

    def test_atomic_write_is_idempotent_and_private(self):
        path = self.home / ".config" / "cliproxy" / "launcher.env"
        with mock.patch.object(agent.os, "replace", wraps=os.replace) as replace, \
                mock.patch.object(agent.stat, "S_IMODE", return_value=0o600):
            agent.atomic_write(path, "CLIPROXY_ENDPOINT=https://proxy.invalid\n")
            first_contents = path.read_text()
            agent.atomic_write(path, first_contents)
        self.assertEqual(replace.call_count, 1)
        if os.name != "nt":
            self.assertEqual(stat.S_IMODE(path.stat().st_mode), 0o600)

    def test_herdr_managed_block_update_and_conflict(self):
        original = '[[keys.command]]\nkey = "prefix+alt+z"\ntype = "pane"\ncommand = "keep-me"\n'
        first = agent.herdr_configuration(original, ["pi", "claude"])
        updated = agent.herdr_configuration(first, ["codex"])
        self.assertIn('command = "keep-me"', updated)
        self.assertEqual(updated.count("# BEGIN CLIPROXY OPT-IN"), 1)
        self.assertIn('command = "codex-proxy"', updated)
        with self.assertRaisesRegex(ValueError, "binding conflict"):
            agent.herdr_configuration('[[keys.command]]\nkey = "prefix+alt+x"\n', ["codex"])

    @mock.patch.object(agent, "tailscale_status", return_value={"state": "Running", "online": True})
    @mock.patch.object(agent, "protected_key", return_value=("UNITTEST_SECRET_MUST_NOT_ESCAPE", "configured"))
    @mock.patch.object(agent, "http_probe", return_value={"http_status": 200, "reachable": True, "valid_response": True, "model_count": 2})
    def test_probe_result_never_contains_key(self, _probe, _key, _tailscale):
        with mock.patch.object(agent.shutil, "which", return_value=None):
            result = agent.operate(agent_settings(mode="probe"), self.home)
        self.assertNotIn("UNITTEST_SECRET_MUST_NOT_ESCAPE", json.dumps(result))


class TailscalePeerTests(unittest.TestCase):
    @mock.patch.object(peer.platform, "system", return_value="Linux")
    @mock.patch.object(peer.shutil, "which", return_value="/usr/bin/tailscale")
    @mock.patch.object(peer, "run")
    def test_existing_install_sets_hostname(self, run, _which, _system):
        running = json.dumps({"BackendState": "Running", "Self": {"Online": True, "DNSName": "peer.invalid.", "TailscaleIPs": ["100.64.0.1"]}})
        run.side_effect = [types.SimpleNamespace(returncode=0, stdout=running), types.SimpleNamespace(returncode=0, stdout=""), types.SimpleNamespace(returncode=0, stdout=running)]
        result = peer.onboard("peer-test")
        self.assertTrue(result["online"])
        self.assertEqual(result["dns_name"], "peer.invalid")
        self.assertEqual(run.call_args_list[1].args[0][3], "set")

    @mock.patch.object(peer.platform, "system", return_value="Linux")
    @mock.patch.object(peer.shutil, "which", return_value=None)
    @mock.patch.object(peer.subprocess, "run", return_value=types.SimpleNamespace(returncode=0, stdout="", stderr=""))
    @mock.patch.object(peer, "run")
    def test_missing_install_and_manual_auth_url(self, run, install, _which, _system):
        run.side_effect = [
            types.SimpleNamespace(returncode=0, stdout="installer", stderr=""),
            types.SimpleNamespace(returncode=0, stdout="{}", stderr=""),
            types.SimpleNamespace(returncode=1, stdout="", stderr="Please visit " + "https://login.tailscale.com/a/abc" + " to authenticate"),
        ]
        result = peer.onboard("peer-test")
        self.assertTrue(result.get("authorization_url", "").endswith("/a/abc"), "manual authorization URL was not returned")
        self.assertEqual(install.call_args.kwargs["input"], "installer")

    @mock.patch.object(peer.platform, "system", return_value="Linux")
    @mock.patch.object(peer.shutil, "which", return_value="/usr/bin/tailscale")
    @mock.patch.object(peer, "run", return_value=types.SimpleNamespace(returncode=1, stdout="", stderr=""))
    def test_failed_health_is_not_reported_online(self, _run, _which, _system):
        result = peer.onboard("peer-test")
        self.assertEqual(result["state"], "join_failed")
        self.assertFalse(result["online"])


class SecretScannerTests(unittest.TestCase):
    def test_go_references_are_expressions_but_literals_still_fail(self):
        references = '\n'.join(('Access' + 'Token: tokenResp.AccessToken,',
                                'Refresh' + 'Token: refreshToken,',
                                'storage.Access' + 'Token = tokenData.AccessToken',
                                '"refresh_' + 'token": refreshToken,'))
        self.assertEqual(scanner.scan_text(references, go_source=True), [])
        self.assertTrue(scanner.scan_text(references))
        literal = 'AccessToken: "' + 'synthetic-not-allowlisted' + '",'
        self.assertTrue(scanner.scan_text(literal, go_source=True))
        raw = 'const payload = `\n' + references + '\n`'
        self.assertTrue(scanner.scan_text(raw, go_source=True))
        comment = '/*\n' + references + '\n*/'
        self.assertTrue(scanner.scan_text(comment, go_source=True))
        token = 'sk-' + 'x' * 30
        self.assertTrue(scanner.scan_text('AccessToken: ' + token + ',', go_source=True))

    def test_benign_references_pass_and_literals_fail(self):
        benign = '\n'.join((
            'Use the client_api_key field and Authorization header from the environment.',
            'api_key = os.environ["CLIPROXY_API_KEY"]',
            'api_key = "${CLIPROXY_API_KEY}"',
        ))
        sample_value = "A9mQ" + "7xL2pV" + "4zN8cR" + "6wK3hT" + "1dF5sB"
        bearer = "Authorization: " + "Bearer " + sample_value
        oauth = '{"access_token": "' + sample_value + '"}'
        self.assertEqual(scanner.scan_text(benign), [])
        self.assertTrue(scanner.scan_text(bearer))
        self.assertTrue(scanner.scan_text(oauth))
        short_literal = 'api_key = "' + "abc123" + '"'
        private_key_marker = "-" * 5 + "BEGIN " + "OPENSSH " + "PRIVATE KEY" + "-" * 5
        self.assertTrue(scanner.scan_text(short_literal))
        self.assertTrue(scanner.scan_text(private_key_marker))

    def test_diagnostics_do_not_include_matched_value(self):
        sample_value = "A9mQ" + "7xL2pV" + "4zN8cR" + "6wK3hT" + "1dF5sB"
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "sample.txt"
            path.write_text("client_" + "secret=" + sample_value)
            # The scanner API returns location and rule only, never source text.
            results = scanner.scan_text(path.read_text())
        self.assertEqual(len(results), 1)
        self.assertNotIn(sample_value, repr(results))


class FleetInventorySchemaTests(unittest.TestCase):
    def _load_example(self, name):
        return json.loads((OPS / name).read_text(encoding="utf-8"))

    def test_example_documents_match_exact_allowlisted_shapes(self):
        fleet = self._load_example("fleet.example.json")
        inventory = self._load_example("inventory.example.json")
        validate_shape(fleet, FLEET_SCHEMA)
        validate_shape(inventory, INVENTORY_SCHEMA)

    def test_secret_fields_are_rejected_at_every_level(self):
        fleet = self._load_example("fleet.example.json")
        fleet["machines"][0]["client_" + "api_key"] = "any-value"
        with self.assertRaises(ValueError):
            validate_shape(fleet, FLEET_SCHEMA)

        inventory = self._load_example("inventory.example.json")
        inventory["machines"][0]["status"]["oauth"] = {"refresh_" + "token": "any-value"}
        with self.assertRaises(ValueError):
            validate_shape(inventory, INVENTORY_SCHEMA)


@unittest.skipUnless(sys.platform == "linux", "POSIX launcher execution is Linux-only")
class PosixLauncherTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.root = Path(self.temporary.name)
        self.home = self.root / "home"
        config_dir = self.home / ".config/cliproxy"
        config_dir.mkdir(parents=True)
        os.chmod(config_dir, 0o700)
        self.key = "unit-test-key-value"
        key_path = config_dir / "client.key"
        key_path.write_text(self.key)
        os.chmod(key_path, 0o600)
        self.endpoint = "https://proxy.invalid"
        self.model = "linux-test-model"
        (config_dir / "launcher.env").write_text(
            f"CLIPROXY_BASE_URL={self.endpoint}\nCLIPROXY_MODEL={self.model}\n"
        )
        self.capture = self.root / "runtime-capture.json"
        self.fake_runtime = self.root / "fake-runtime.py"
        self.fake_runtime.write_text(
            "#!/usr/bin/env python3\n"
            "import json, os, sys\n"
            "from pathlib import Path\n"
            "record = {\n"
            "  'args': sys.argv[1:],\n"
            "  'proxy_key_present': os.environ.get('CLIPROXY_API_KEY') == os.environ.get('FLEET_EXPECTED_KEY'),\n"
            "  'claude_key_present': os.environ.get('ANTHROPIC_AUTH_TOKEN') == os.environ.get('FLEET_EXPECTED_KEY'),\n"
            "  'claude_base_url': os.environ.get('ANTHROPIC_BASE_URL'),\n"
            "  'codex_home': os.environ.get('CODEX_HOME'),\n"
            "}\n"
            "Path(os.environ['FLEET_CAPTURE']).write_text(json.dumps(record))\n"
        )
        os.chmod(self.fake_runtime, 0o700)
        self.launcher_dir = ROOT / "scripts/launchers/posix"

    def tearDown(self):
        self.temporary.cleanup()

    def _env(self):
        env = os.environ.copy()
        env.update({
            "HOME": str(self.home),
            "FLEET_EXPECTED_KEY": self.key,
            "FLEET_CAPTURE": str(self.capture),
            "PI_BIN": str(self.fake_runtime),
            "CLAUDE_BIN": str(self.fake_runtime),
            "CODEX_BIN": str(self.fake_runtime),
        })
        return env

    def _launch(self, client):
        result = subprocess.run(
            ["sh", str(self.launcher_dir / f"{client}-cliproxy")],
            env=self._env(), capture_output=True, text=True, check=False,
        )
        self.assertEqual(result.returncode, 0, f"{client} fake runtime did not complete")
        record = json.loads(self.capture.read_text())
        self.assertNotIn(self.key, self.capture.read_text())
        return record

    def test_pi_claude_and_codex_pass_overrides_without_persisting_key(self):
        pi_config = self.home / ".pi/agent/models.json"
        pi_config.parent.mkdir(parents=True)
        pi_config.write_text(json.dumps({"providers": {"openai": {"models": [{"id": "direct-model"}]}}}))

        pi = self._launch("pi")
        self.assertTrue(pi["proxy_key_present"])
        self.assertTrue("--model" in pi["args"] and "cliproxy/" + self.model in pi["args"])
        providers = json.loads(pi_config.read_text())["providers"]
        self.assertTrue("openai" in providers and providers["openai"]["models"][0]["id"] == "direct-model")
        self.assertTrue(providers["cliproxy"]["baseUrl"] == self.endpoint + "/v1")
        self.assertTrue(providers["cliproxy"]["apiKey"] == "${CLIPROXY_API_KEY}")

        claude = self._launch("claude")
        self.assertTrue(claude["claude_key_present"])
        self.assertTrue(claude["claude_base_url"] == self.endpoint)
        self.assertTrue("--model" in claude["args"] and self.model in claude["args"])

        codex = self._launch("codex")
        self.assertTrue(codex["proxy_key_present"])
        self.assertTrue(Path(codex["codex_home"]) == self.home / ".codex-cliproxy")
        self.assertTrue("--profile" in codex["args"] and "cliproxy" in codex["args"])
        self.assertTrue("--model" in codex["args"] and self.model in codex["args"])
        stored = b"".join(path.read_bytes() for path in self.home.rglob("*") if path.is_file() and path.name != "client.key")
        self.assertNotIn(self.key.encode(), stored)

    def test_dry_run_creates_no_files_and_does_not_read_key(self):
        dry_home = self.root / "dry-home"
        dry_home.mkdir()
        env = self._env()
        env["HOME"] = str(dry_home)
        env["CLIPROXY_DRY_RUN"] = "1"
        result = subprocess.run(
            ["sh", str(self.launcher_dir / "pi-cliproxy")],
            env=env, capture_output=True, text=True, check=False,
        )
        self.assertEqual(result.returncode, 0, "Pi DryRun failed")
        self.assertTrue("Plan:" in result.stdout and "Credential not read" in result.stdout)
        self.assertEqual(list(dry_home.rglob("*")), [])


if __name__ == "__main__":
    unittest.main()
