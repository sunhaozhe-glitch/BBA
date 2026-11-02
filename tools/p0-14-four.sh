#!/usr/bin/env bash
# P0-14 R20 four-account read-only evidence run.
#
# The runner can only dial loopback. It reads login identity, online/good
# status, and the complete group list for each bound account. It never sends a
# message, joins a group, refreshes a QR code, restarts an instance, or writes
# the NapCat state file. Evidence follows the P0-13 layout: request/response
# records, steps, summary, environment, a secret scan, and a SHA-256 manifest.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
EVIDENCE="${P014_EVIDENCE_DIR:-/tmp/bba-r20-$(date -u +%Y%m%dT%H%M%SZ)}"
STATE_FILE="${P014_STATE_FILE:-/home/sun/.local/state/bba-napcatd/state.json}"
BINDINGS="${P014_BINDINGS:-3210:443952741,3211:36374700,3212:3256395185,3213:454755768}"
if [[ -e "$EVIDENCE" ]]; then
  echo "evidence directory already exists: $EVIDENCE" >&2
  exit 1
fi
mkdir -m 700 "$EVIDENCE"

CODE_FILES=(
  backend/cmd/p0-14-four/main.go
  backend/cmd/p0-14-four/main_test.go
  backend/cmd/p0-14-four/engine.go
  backend/cmd/p0-14-four/engine_test.go
  tools/p0-14-four.sh
)
CODE_SHA="$(cd "$ROOT" && sha256sum "${CODE_FILES[@]}" | sha256sum | cut -d' ' -f1)"

{
  echo "utc_started=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "host=$(uname -srm)"
  echo "go_image=$(docker image inspect golang:1.26 --format '{{.Id}}')"
  echo "code_sha=$CODE_SHA"
  echo "state_source=$STATE_FILE"
  echo "bindings=$BINDINGS"
  echo "mode=read_only"
  echo "writes=0"
  echo "evidence_dir=$EVIDENCE"
} > "$EVIDENCE/environment.txt"

set +e
docker run --rm --network host --user "$(id -u):$(id -g)" \
  -e HOME=/tmp/home -e GOPATH=/work/.cache/gopath -e GOMODCACHE=/work/.cache/gomod \
  -e GOCACHE=/work/.cache/gobuild -e GOPROXY=off -e GOSUMDB=off -e GOFLAGS=-mod=readonly \
  -e P014_EVIDENCE_DIR=/evidence -e P014_STATE_FILE=/config/state.json \
  -e P014_BINDINGS="$BINDINGS" -e P014_CODE_SHA="$CODE_SHA" \
  -v "$ROOT:/work" -v "$STATE_FILE:/config/state.json:ro" -v "$EVIDENCE:/evidence" \
  -w /work/backend golang:1.26 go run ./cmd/p0-14-four 2>&1 | tee "$EVIDENCE/stdout.log"
RUN_EXIT=${PIPESTATUS[0]}
set -e

set +e
python3 - "$STATE_FILE" "$EVIDENCE" > "$EVIDENCE/sensitive-scan.txt" <<'PY'
import json
import pathlib
import sys

state_path, evidence_dir = sys.argv[1], pathlib.Path(sys.argv[2])
state = json.load(open(state_path))
checks = []
for instance in state.get("instances", []):
    port = instance.get("onebot_port")
    for key in ("onebot_token", "webui_token"):
        token = instance.get(key)
        if isinstance(token, str) and token:
            checks.append((port, key, token.encode()))

files = [p for p in evidence_dir.rglob("*") if p.is_file()]
bad = []
for path in sorted(files):
    if path.name in {"sensitive-scan.txt", "evidence-sha256.txt", "evidence-verify.json"}:
        continue
    data = path.read_bytes()
    for port, key, token in checks:
        if token in data:
            bad.append(f"FOUND port={port} field={key} file={path.name}")
    for marker in (b"Authorization", b"Bearer ", b'"onebot_token"', b'"webui_token"'):
        if marker in data:
            bad.append(f"FOUND marker={marker.decode()} file={path.name}")
for port, key, _ in checks:
    print(f"port={port} {key}=absent")
if bad:
    for line in sorted(set(bad)):
        print(line)
    sys.exit(1)
print("scan=pass")
PY
SCAN_EXIT=$?
set -e

(
  cd "$EVIDENCE"
  find . -type f ! -name evidence-sha256.txt ! -name evidence-verify.json -print0 \
    | sort -z | xargs -0 sha256sum
) > "$EVIDENCE/evidence-sha256.txt"

python3 - "$EVIDENCE" "$SCAN_EXIT" <<'PY'
import hashlib
import json
import pathlib
import sys
import time

evidence = pathlib.Path(sys.argv[1])
manifest = (evidence / "evidence-sha256.txt").read_bytes()
result = {
    "manifest_sha256": hashlib.sha256(manifest).hexdigest(),
    "scan_exit": int(sys.argv[2]),
    "utc_verified": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
}
(evidence / "evidence-verify.json").write_text(json.dumps(result, indent=2) + "\n")
PY

echo "P0-14 four read-only exit=$RUN_EXIT scan_exit=$SCAN_EXIT evidence=$EVIDENCE code_sha=$CODE_SHA"
if [[ "$RUN_EXIT" != 0 ]]; then exit "$RUN_EXIT"; fi
exit "$SCAN_EXIT"
