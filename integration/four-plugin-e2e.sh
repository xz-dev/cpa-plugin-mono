#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
exec python3 integration/four-plugin-e2e.py "$@"
