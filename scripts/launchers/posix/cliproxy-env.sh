#!/bin/sh
# Shared opt-in settings. Never source configuration or put keys in argv.
cliproxy_settings() {
    config_file=${CLIPROXY_LAUNCHER_CONFIG:-$HOME/.config/cliproxy/launcher.env}
    configured_url= configured_model=
    if [ -r "$config_file" ]; then
        while IFS='=' read -r setting value || [ -n "${setting:-}" ]; do
            case "$setting" in
                CLIPROXY_BASE_URL) configured_url=$value ;;
                CLIPROXY_MODEL) configured_model=$value ;;
                ''|\#*) ;;
                *) printf '%s\n' 'Invalid launcher setting.' >&2; return 2 ;;
            esac
        done < "$config_file"
    fi
    CLIPROXY_BASE_URL=${CLIPROXY_BASE_URL:-${configured_url:-http://127.0.0.1:8317}}
    CLIPROXY_MODEL=${CLIPROXY_MODEL:-${configured_model:-gpt-6-sol}}
    CLIPROXY_KEY_FILE=${CLIPROXY_KEY_FILE:-$HOME/.config/cliproxy/client.key}
    python3 - "$CLIPROXY_BASE_URL" "$CLIPROXY_MODEL" <<'PY'
import re, sys, urllib.parse
if not re.fullmatch(r'https?://[A-Za-z0-9.\-:\[\]]+/?', sys.argv[1]):
    print('Invalid endpoint origin.', file=sys.stderr)
    sys.exit(2)
url = urllib.parse.urlsplit(sys.argv[1])
url.port
valid = (url.scheme == 'https' or (url.scheme == 'http' and url.hostname in ('localhost', '127.0.0.1', '::1')))
if not valid or not url.hostname or url.username or url.password or url.query or url.fragment or url.path not in ('', '/') or not re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9._-]*', sys.argv[2]):
    print('Invalid endpoint origin or model.', file=sys.stderr)
    sys.exit(2)
PY
    [ "$?" -eq 0 ] || return 2
    CLIPROXY_BASE_URL=${CLIPROXY_BASE_URL%/}
    if [ "${CLIPROXY_DRY_RUN:-0}" = 1 ]; then
        printf '%s\n' 'Plan: load protected key and launch opt-in proxy runtime. Credential not read.'
        return 10
    fi
    CLIPROXY_KEY=$(python3 - "$CLIPROXY_KEY_FILE" <<'PY'
from pathlib import Path
import os, stat, sys
try:
    path = Path(sys.argv[1]).expanduser()
    if path.is_symlink() or any(p.is_symlink() for p in path.parents):
        raise ValueError()
    for target, mode in ((path.parent, 0o700), (path, 0o600)):
        info = target.stat()
        if info.st_uid != os.getuid() or stat.S_IMODE(info.st_mode) != mode:
            raise ValueError()
    key = path.read_text().strip()
    if not key or any(c.isspace() or ord(c) < 33 or ord(c) > 126 for c in key):
        raise ValueError()
    sys.stdout.write(key)
except Exception:
    print('Protected key missing, malformed, linked, or permissions invalid.', file=sys.stderr)
    sys.exit(2)
PY
    ) || return 2
    export CLIPROXY_BASE_URL CLIPROXY_MODEL
}
