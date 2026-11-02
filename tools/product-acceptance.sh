#!/usr/bin/env bash
# Product acceptance owns isolated resources; release-check never deploys.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
exec "${BBA_PYTHON:-python3}" "$ROOT/tools/product-acceptance.py" "$@"
