#!/usr/bin/env bash
# Generate Go contract types from the bundled OpenAPI document.
# The pinned oapi-codegen binary is built by tools/prefetch.sh.
# Generation runs in a network-disabled container for reproducibility.
#
# This script is runnable on the host; it re-execs itself inside golang:1.26
# with networking disabled so generation is offline and reproducible.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SPEC_REL="${1:-build/openapi.bundled.json}"

if [[ "${CONTRACT_INSIDE_DOCKER:-0}" != "1" ]]; then
  mkdir -p "$ROOT/.cache/gobuild" "$ROOT/.cache/gopath" "$ROOT/.cache/gomod"
  exec docker run --rm --network none \
    --user "$(id -u):$(id -g)" \
    -e HOME=/tmp/home \
    -e CONTRACT_INSIDE_DOCKER=1 \
    -e GOPATH=/work/.cache/gopath \
    -e GOCACHE=/work/.cache/gobuild \
    -e GOMODCACHE=/work/.cache/gomod \
    -e GOPROXY=off \
    -e GOSUMDB=off \
    -e GOFLAGS=-mod=readonly \
    -v "$ROOT:/work" \
    -w /work \
    golang:1.26 bash -c 'mkdir -p /tmp/home && bash /work/tools/generate_go.sh "$@"' _ "$SPEC_REL"
fi

cd "$ROOT"
bash "$ROOT/tools/refgate.sh"

BIN="$ROOT/.cache/bin/oapi-codegen"
if [[ ! -x "$BIN" ]]; then
  echo "generate_go: missing $BIN; run tools/prefetch.sh first (network fetch is explicit)" >&2
  exit 2
fi

SPEC="$ROOT/$SPEC_REL"
if [[ ! -f "$SPEC" ]]; then
  echo "generate_go: bundled spec not found: $SPEC" >&2
  exit 2
fi

mkdir -p "$ROOT/backend/internal/contract"
"$BIN" \
  --config "$ROOT/tools/oapi-codegen.yaml" \
  -o "$ROOT/backend/internal/contract/types.gen.go" \
  "$SPEC"

echo "generate_go: wrote backend/internal/contract/types.gen.go"
