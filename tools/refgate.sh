#!/usr/bin/env bash
# Runs the local-only ref gate. Every third-party OpenAPI tool is invoked only
# after this script succeeds, including npm scripts and tools/contract.sh.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
exec python3 "$ROOT/tools/refgate.py" --root "$ROOT/contracts" "$@"
