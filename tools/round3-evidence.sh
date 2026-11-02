#!/usr/bin/env bash
# Round-3 evidence runner. It repeats the isolation checks the audit requires and
# saves the raw output of every step; nothing is copied from memory. Each log
# records the exact command line, UTC start/end and exit code, and the run
# manifest hashes every file in the package.
#
# Offline scope only: the Go, contract and Node steps run in pinned containers with
# --network none (the Node step needs a warm npm cache). No real QQ account, no
# production database or migration, no live model call, no live ARTEX access. The
# browser artifacts are produced by the main session against the fixture console and
# are hashed by the final manifest step.
#
#   tools/round3-evidence.sh                       # full run into a new timestamped dir
#   BBA_EVIDENCE_DIR=evals/results/<dir> tools/round3-evidence.sh   # write into that dir
#   tools/round3-evidence.sh manifest <dir>        # re-hash a package after artifacts land
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# Source fingerprint rule (recorded so the count is reproducible): these trees and
# root files, these extensions, caches and build output excluded.
PRED=(-name '*.go' -o -name '*.ts' -o -name '*.json' -o -name '*.md' -o -name '*.py' -o -name '*.sh' -o -name '*.sql' -o -name '*.mjs' -o -name '*.yml' -o -name '*.yaml' -o -name '*.patch')
FINGERPRINT_DIRS=(backend koishi tools docs tests contracts evals plugins)
FINGERPRINT_FILES=(package.json package-lock.json redocly.yaml tsconfig.contract.json tsconfig.contract-smoke.json .gitignore)
FINGERPRINT_PRUNE=(-not -path '*/.cache/*' -not -path '*/build/*' -not -path '*/node_modules/*' -not -path '*__pycache__*' -not -path '*/.tmp/*' -not -path '*/.work/*' -not -path '*/evals/results/*')

manifest_only() {
  local dir="$1"
  [[ -d "$dir" ]] || { echo "round3-evidence: no such directory $dir" >&2; exit 2; }
  ( cd "$dir" && find . -type f ! -name manifest.sha256 -print0 | sort -z | xargs -0 sha256sum > manifest.sha256 )
  ( cd "$dir" && sha256sum --check --quiet manifest.sha256 ) || { echo "round3-evidence: manifest verify failed" >&2; exit 1; }
  echo "round3-evidence: manifest verified for $dir ($(wc -l < "$dir/manifest.sha256") files)"
}

if [[ "${1:-}" == manifest ]]; then
  manifest_only "${2:?usage: tools/round3-evidence.sh manifest <dir>}"
  exit 0
fi

RUN_DIR="${BBA_EVIDENCE_DIR:-$ROOT/evals/results/bba-20261030-round3-$(date -u +%Y%m%dT%H%M%SZ)}"
case "$RUN_DIR" in /*) ;; *) RUN_DIR="$ROOT/$RUN_DIR" ;; esac
mkdir -p "$RUN_DIR/logs" "$RUN_DIR/browser"
STEPS="$RUN_DIR/steps.tsv"
: > "$STEPS"
FAILED=0

record() {
  local label="$1"
  shift
  local start end code out
  out="$RUN_DIR/logs/$label.out"
  start="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  set +e
  "$@" > "$out" 2>&1
  code=$?
  set -e
  end="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  {
    printf '%s\n$ %s\n\n' "$start" "$(printf '%q ' "$@")"
    cat "$out"
    printf '\nexit=%s\nend=%s\n' "$code" "$end"
  } > "$RUN_DIR/logs/$label.log"
  rm -f "$out"
  printf '%s\tstart=%s\tend=%s\texit=%s\n' "$label" "$start" "$end" "$code" >> "$STEPS"
  echo "$label exit=$code"
  if [[ "$code" != 0 ]]; then FAILED=1; fi
  return 0
}

cd "$ROOT"

# Toolchain identity, so the run can be attributed.
{
  echo "utc_start=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "host=$(uname -sr)"
  docker --version || true
  docker image inspect --format 'go_image={{.Id}}' golang:1.26 || true
  docker image inspect --format 'node_image={{.Id}}' node:22-bookworm || true
  docker image inspect --format 'postgres_image={{.Id}}' postgres:16-bookworm || true
} > "$RUN_DIR/environment.txt"

record schema ./tools/kernel.sh schema
record go-test-all ./tools/kernel.sh test-all
record chain-proposal ./tools/kernel.sh test -run 'TestPlanProposal' -v
record chain-worker ./tools/kernel.sh test -run 'TestWorker' -v
record chain-summary ./tools/kernel.sh test -run 'TestGroupSummary|TestGroupContext' -v
record contract-check ./tools/contract.sh check
record node22-verify env BBA_NODE_OFFLINE=1 ./tools/node22-deps.sh verify

# Source fingerprint of the tree as it now stands; the audit's earlier 246-file
# manifest belongs to the previous state and is reported, not overwritten.
FINGERPRINT_LIST="$RUN_DIR/source-files.sha256.files"
find "${FINGERPRINT_DIRS[@]}" -type f \( "${PRED[@]}" \) "${FINGERPRINT_PRUNE[@]}" -printf '%p\n' > "$FINGERPRINT_LIST"
for f in "${FINGERPRINT_FILES[@]}"; do [[ -f "$ROOT/$f" ]] && printf '%s\n' "$f" >> "$FINGERPRINT_LIST"; done
sort -u "$FINGERPRINT_LIST" -o "$FINGERPRINT_LIST"
xargs -a "$FINGERPRINT_LIST" -d '\n' -I{} sha256sum {} > "$RUN_DIR/source-files.sha256"
rm -f "$FINGERPRINT_LIST"
if ( cd "$ROOT" && sha256sum --check --quiet "$RUN_DIR/source-files.sha256" ); then
  printf 'source-fingerprint\tfiles=%s\texit=0\n' "$(wc -l < "$RUN_DIR/source-files.sha256")" >> "$STEPS"
else
  printf 'source-fingerprint\tverify=FAIL\n' >> "$STEPS"
  FAILED=1
fi

# Browser evidence is written by the main session into browser/; the manifest step
# hashes whatever is present when the package is complete.
manifest_only "$RUN_DIR"

[[ "$FAILED" == 0 ]] || { echo "round3-evidence: at least one step failed; see $STEPS" >&2; exit 1; }
echo "round3-evidence: package complete at $RUN_DIR"
