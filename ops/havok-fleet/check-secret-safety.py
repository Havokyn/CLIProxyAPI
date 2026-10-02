"""Find likely literal credentials in the fleet tooling and its documentation.

Findings include only a relative path, line number, and rule name. Matched text
is deliberately never included in diagnostic output.
"""
from __future__ import annotations

import math
from pathlib import Path
import re
import sys
import argparse
import subprocess

ROOT = Path(__file__).resolve().parents[2]
SCOPED = (
    "ops/havok-fleet",
    "scripts/launchers",
    "docs/havok-fleet.md",
    "tests/fleet",
    "tools",
    "tests/local_ci",
)
TEXT_SUFFIXES = {".md", ".py", ".ps1", ".psm1", ".sh", ".json", ".toml", ".yaml", ".yml", ".txt", ".go", ".env"}
EXTENSIONLESS_LAUNCHERS = {"pi-cliproxy", "claude-cliproxy", "codex-cliproxy", "pre-push"}
BEARER = re.compile(r"\bBearer\s+([A-Za-z0-9._~+/=-]{16,})", re.IGNORECASE)
ASSIGNMENT = re.compile(
    r"(?i)(?:api[_-]?key|client[_-]?secret|access[_-]?token|refresh[_-]?token|"
    r"auth[_-]?token|id[_-]?token|management[_-]?key|machine[_-]?(?:key|credentials?)|password|secret[_-]?key|secret)\s*[\"']?\s*[:=]\s*[\"']?"
    r"([^\s\"',;}]+)"
)
OAUTH_JSON = re.compile(
    r"(?i)[\"'](?:access_token|refresh_token|id_token|client_secret)[\"']\s*:\s*"
    r"[\"']([^\"']+)[\"']"
)
PLACEHOLDERS = {
    "changeme", "change-me", "your-token", "your_api_key", "placeholder",
    "redacted", "example", "dummy", "null", "none", "false",
}
PRIVATE_KEY = re.compile(r"-{5}BEGIN (?:RSA |EC |OPENSSH )?PRIVATE KEY-{5}")
TOKEN_LITERAL = re.compile(r"\b(?:sk-(?:proj-)?[A-Za-z0-9_-]{20,}|ghp_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|ya29\.[A-Za-z0-9_-]{20,}|eyJ[A-Za-z0-9_-]{12,}\.[A-Za-z0-9_-]{12,}\.[A-Za-z0-9_-]{12,})\b")


def looks_like_secret(value: str, literal: bool = False) -> bool:
    value = value.strip().strip("<>").strip()
    if not value or value.lower() in PLACEHOLDERS or value.startswith(("${", "$env:", "<")):
        return False
    if value.lower().startswith(("test-only-", "unit-test-")):
        return False
    if any(marker in value for marker in ("$", "{{", "}}", "(", ")", "[", "]")):
        return False
    if value.lower().startswith(("os.", "process.", "env.", "!sh", "getenv", "variable")):
        return False
    if re.fullmatch(r"[A-Z][A-Z0-9_]{2,}", value):
        return False
    if literal and len(value) >= 4 and not re.search(r"\s", value):
        return True
    if len(value) >= 8 and not re.search(r"\s", value) and value.lower() not in PLACEHOLDERS:
        return True
    if value.lower().startswith(("sk-", "ghp_", "github_pat_", "ya29.", "eyj")):
        return True
    compact = value.replace("_", "").replace("-", "").replace(".", "")
    if len(compact) < 20 or not re.fullmatch(r"[A-Za-z0-9+/=]+", compact):
        return False
    counts = {char: compact.count(char) for char in set(compact)}
    entropy = -sum((count / len(compact)) * math.log2(count / len(compact)) for count in counts.values())
    return entropy >= 3.5


def scan_text(text: str) -> list[tuple[int, str]]:
    findings = []
    for line_number, line in enumerate(text.splitlines(), 1):
        if PRIVATE_KEY.search(line):
            findings.append((line_number, "private-key"))
            continue
        if TOKEN_LITERAL.search(line):
            findings.append((line_number, "token-literal"))
            continue
        for rule, pattern in (("bearer", BEARER), ("credential-assignment", ASSIGNMENT), ("oauth-json", OAUTH_JSON)):
            if any(
                looks_like_secret(
                    match.group(1),
                    literal=match.start(1) > 0 and line[match.start(1) - 1] in "\"'",
                )
                for match in pattern.finditer(line)
            ):
                findings.append((line_number, rule))
                break
    return findings


def files_to_scan(root: Path = ROOT):
    for relative in SCOPED:
        target = root / relative
        if target.is_file():
            yield target
        elif target.is_dir():
            yield from (
                path for path in target.rglob("*")
                if path.is_file() and path.suffix.lower() in TEXT_SUFFIXES
                or path.is_file() and path.name in EXTENSIONLESS_LAUNCHERS
            )


def scan(root: Path = ROOT) -> list[tuple[str, int, str]]:
    results = []
    for path in files_to_scan(root):
        try:
            content = path.read_text(encoding="utf-8", errors="replace")
        except OSError:
            raise OSError(f"Cannot read scoped source file: {path.relative_to(root).as_posix()}") from None
        for line, rule in scan_text(content):
            results.append((path.relative_to(root).as_posix(), line, rule))
    return results


def staged_findings(root: Path) -> list[tuple[str, int, str]]:
    """Scan index contents, including ignored credentials forcibly staged by mistake."""
    command = ["git", "-c", f"safe.directory={root.as_posix()}", "-C", str(root)]
    paths = subprocess.run(command + ["diff", "--cached", "--name-only", "--diff-filter=ACMR", "-z"],
                           capture_output=True, check=True).stdout.decode("utf-8").split("\0")
    results = []
    for relative in filter(None, paths):
        path = Path(relative)
        if path.name in {"credentials.json", "client.key", "management.key", "id_rsa", "id_ed25519", ".env", "config.yaml"} or path.name.startswith(".env.") or path.name.endswith(".private.key") or any(
                part in {"auths", "machine-keys", ".local", ".local-ci"} for part in path.parts):
            results.append((relative, 1, "credential-or-generated-file-staged"))
            continue
        raw = subprocess.run(command + ["show", ":" + relative], capture_output=True, check=True).stdout
        content = raw.decode("utf-8", errors="replace")
        if b"\0" in raw and not PRIVATE_KEY.search(content):
            continue  # Binary assets; text/index files are scanned regardless of extension.
        results.extend((relative, line, rule) for line, rule in scan_text(content))
    return results


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--staged", action="store_true", help="Also inspect staged index content")
    parser.add_argument("--root", type=Path, default=ROOT, help="Isolated fixture/repository root")
    options = parser.parse_args()
    try:
        findings = scan(options.root)
    except OSError as error:
        print(str(error))
        return 1
    if options.staged:
        try:
            findings += staged_findings(options.root)
        except (OSError, subprocess.CalledProcessError):
            print("Secret safety: could not inspect staged index; failing closed.")
            return 1
    if findings:
        for path, line, rule in findings:
            print(f"{path}:{line}: possible literal credential ({rule})")
        return 1
    print("Secret safety scan passed.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
