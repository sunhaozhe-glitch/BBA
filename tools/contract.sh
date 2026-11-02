#!/usr/bin/env bash
# P0-01 contract pipeline.
#
#   tools/contract.sh prefetch   # network-enabled, explicit, once
#   tools/contract.sh check      # ref gate + strict validator + tests + lint
#   tools/contract.sh bundle     # ref gate + redocly bundle
#   tools/contract.sh generate   # check + bundle + Go/TS types + type smoke
#   tools/contract.sh test       # Python unit/integration tests only
#
# Third-party node/Go tools run in pinned cached images with --network none.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
NODE_IMAGE="${NODE_IMAGE:-node:22-bookworm}"
GO_IMAGE="${GO_IMAGE:-golang:1.26}"
UID_GID="$(id -u):$(id -g)"

die() { echo "contract.sh: $*" >&2; exit 2; }

require_contracts() {
  [[ -f "$ROOT/contracts/openapi.yaml" ]] || die "missing contracts/openapi.yaml"
}

require_node_modules() {
  [[ -x "$ROOT/node_modules/.bin/redocly" ]] || die "run tools/contract.sh prefetch first"
  [[ -x "$ROOT/node_modules/.bin/openapi-typescript" ]] || die "run tools/contract.sh prefetch first"
  [[ -x "$ROOT/node_modules/.bin/tsc" ]] || die "run tools/contract.sh prefetch first"
}

node_offline() {
  docker run --rm --network none \
    --user "$UID_GID" \
    -e HOME=/tmp/home \
    -e NO_UPDATE_NOTIFIER=1 \
    -e REDOCLY_TELEMETRY=off \
    -v "$ROOT:/work" \
    -w /work \
    "$NODE_IMAGE" \
    bash -c 'mkdir -p /tmp/home && exec "$@"' _ "$@"
}

go_offline() {
  docker run --rm --network none \
    --user "$UID_GID" \
    -e HOME=/tmp/home \
    -e GOPATH=/work/.cache/gopath \
    -e GOMODCACHE=/work/.cache/gomod \
    -e GOCACHE=/work/.cache/gobuild \
    -e GOPROXY=off \
    -e GOSUMDB=off \
    -e GOFLAGS=-mod=readonly \
    -e CONTRACT_ROOT=/work/contracts \
    -v "$ROOT:/work" \
    -w /work/backend \
    "$GO_IMAGE" \
    bash -c 'mkdir -p /tmp/home && exec "$@"' _ "$@"
}

run_refgate() {
  bash "$ROOT/tools/refgate.sh"
}

run_validator() {
  python3 "$ROOT/tools/validate_contracts.py" "$ROOT/contracts"
}

run_tests() {
  ( cd "$ROOT" && python3 -m unittest discover -s tests/contract -t . -v )
}

run_lint() {
  require_node_modules
  node_offline ./node_modules/.bin/redocly lint contracts/openapi.yaml
}

run_bundle() {
  require_node_modules
  mkdir -p "$ROOT/build"
  node_offline ./node_modules/.bin/redocly bundle contracts/openapi.yaml \
    --output build/openapi.bundled.json --ext json
}

run_ts_types() {
  require_node_modules
  node_offline ./node_modules/.bin/openapi-typescript \
    build/openapi.bundled.json -o koishi/contracts/types.gen.ts
}

run_go_types() {
  bash "$ROOT/tools/generate_go.sh" build/openapi.bundled.json
}

run_ts_typecheck() {
  require_node_modules
  node_offline ./node_modules/.bin/tsc -p tsconfig.contract.json
}

run_ts_smoke() {
  require_node_modules
  mkdir -p "$ROOT/build/ts-smoke"
  python3 "$ROOT/tools/gen_ts_fixtures.py" "$ROOT/contracts" "$ROOT/build/ts-fixtures.gen.ts"
  node_offline ./node_modules/.bin/tsc -p tsconfig.contract-smoke.json
  node_offline node build/ts-smoke/build/ts-fixtures.gen.js
}

run_go_smoke() {
  CONTRACT_ROOT=/work/contracts go_offline \
    go test -tags contractsmoke ./internal/contract
}

check() {
  require_contracts
  run_refgate
  run_validator
  run_tests
  run_lint
}

generate() {
  check
  run_bundle
  run_ts_types
  run_go_types
  run_ts_smoke
  run_ts_typecheck
  run_go_smoke
  echo "contract.sh: generation + type smoke OK"
}

case "${1:-help}" in
  prefetch) exec bash "$ROOT/tools/prefetch.sh" ;;
  check) check ;;
  bundle) require_contracts; run_refgate; run_bundle ;;
  generate) generate ;;
  test) require_contracts; run_refgate; run_tests ;;
  validate) require_contracts; run_refgate; run_validator ;;
  help|--help|-h)
    sed -n '2,12p' "$0" | sed 's/^# \{0,1\}//'
    ;;
  *)
    die "unknown command '$1' (try: help)"
    ;;
esac
