#!/bin/sh
# Offline Linux checks. Paths are passed as arguments, never evaluated as shell code.
set -eu
cd -- "$(dirname -- "$0")/.."
python3 tools/local_ci.py --linux
