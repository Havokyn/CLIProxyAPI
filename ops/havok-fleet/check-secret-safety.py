"""Find likely literal credentials in the fleet tooling and its documentation.

Findings include only a relative path, line number, and rule name. Matched text
is deliberately never included in diagnostic output.
"""
from __future__ import annotations

import math
from pathlib import Path
import re
import sys

ROOT = Path(__file__).resolve().parents[2]
SCOPED = (
    "ops/havok-fleet",
    "scripts/launchers",
    "docs/havok-fleet.md",
    "tests/fleet",
)
TEXT_SUFFIXES = {".md", ".py", ".ps1", ".psm1", ".sh", ".json", ".toml", ".yaml", ".yml", ".txt"}
EXTENSIONLESS_LAUNCHERS = {"pi-cliproxy", "claude-cliproxy", "codex-cliproxy"}
BEARER = re.compile(r"\bBearer\s+([A-Za-z0-9._~+/=-]{16,})", re.IGNORECASE)
ASSIGNMENT = re.compile(
    r"(?i)(?:api[_-]?key|client[_-]?secret|access[_-]?token|refresh[_-]?token|"
    r"auth[_-]?token|id[_-]?token|password|secret)\s*[\"']?\s*[:=]\s*[\"']?"
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


def looks_like_secret(value: str, literal: bool = False) -> bool:
    value = value.strip().strip("<>").strip()
    if not value or value.lower() in PLACEHOLDERS or value.startswith(("${", "$env:", "<")):
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
            continue
        for line, rule in scan_text(content):
            results.append((path.relative_to(root).as_posix(), line, rule))
    return results


def main() -> int:
    findings = scan()
    if findings:
        for path, line, rule in findings:
            print(f"{path}:{line}: possible literal credential ({rule})")
        return 1
    print("Secret safety scan passed.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
