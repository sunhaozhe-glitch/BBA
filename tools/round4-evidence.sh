#!/usr/bin/env bash
# Round-4 evidence runner and package CLI.
#
# Offline, fixture-only scope: the Go, contract and Node steps use pinned
# containers with --network none (the Node step needs a warm npm cache).  There
# is no real QQ account, no production database/migration, no live model call
# and no live ARTEX access.
#
# The runner writes into a staged directory (BBA_EVIDENCE_DIR or --dir), or
# creates a new evals/results/bba-round4-<UTC> directory.  It refuses to
# overwrite any runner-owned file, so a failed attempt leaves all of its raw
# logs and steps.tsv in place; stage a new directory to retry.
#
#   tools/round4-evidence.sh run [--dir DIR]
#   tools/round4-evidence.sh finalize DIR [--rebuild]
#   tools/round4-evidence.sh verify PROJECT_ROOT PACKAGE_DIR [--hash-only-source]
#   tools/round4-evidence.sh negative PROJECT_ROOT PACKAGE_DIR
#   tools/round4-evidence.sh scan DIR [--output FILE]
#   tools/round4-evidence.sh inventory DIR [--json]
#   tools/round4-evidence.sh fingerprint PROJECT_ROOT OUTPUT
#
# Typical order:
#   BBA_EVIDENCE_DIR=evals/results/bba-round4-<UTC> tools/round4-evidence.sh run
#   # add REPORT.md and browser/ evidence, then:
#   tools/round4-evidence.sh finalize evals/results/bba-round4-<UTC>
#   tools/round4-evidence.sh verify "$PWD" evals/results/bba-round4-<UTC>
#   tools/round4-evidence.sh negative "$PWD" evals/results/bba-round4-<UTC>
set -euo pipefail

SELF="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/$(basename "${BASH_SOURCE[0]}")"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
VERIFIER="$ROOT/tools/round4-evidence-verify.py"
PYTHON="${BBA_PYTHON:-python3}"

die() { echo "round4-evidence: $*" >&2; exit 2; }

# Source fingerprint scope.  The independent verifier re-implements this rule in
# Python; source-verify will fail if the two drift.
FINGERPRINT_DIRS=(backend koishi tools docs tests contracts evals plugins patches)
FINGERPRINT_FILES=(package.json package-lock.json redocly.yaml tsconfig.contract.json tsconfig.contract-smoke.json .gitignore go.mod go.sum)
FINGERPRINT_PRED=(
  -name '*.go' -o -name '*.ts' -o -name '*.tsx' -o -name '*.js' -o -name '*.jsx'
  -o -name '*.css' -o -name '*.html' -o -name '*.py' -o -name '*.sh' -o -name '*.bash'
  -o -name '*.sql' -o -name '*.mjs' -o -name '*.cjs' -o -name '*.json' -o -name '*.jsonl'
  -o -name '*.md' -o -name '*.yml' -o -name '*.yaml' -o -name '*.patch'
  -o -name 'go.mod' -o -name 'go.sum'
)
STEP_LABELS=(
  chain-proposal
  chain-worker
  chain-summary
  schema
  go-test-all
  contract-check
  node22-verify
  source-fingerprint
  source-verify
)

fingerprint_tree() {
  local root="$1" out="$2"
  [[ -d "$root" ]] || die "fingerprint: no such project root $root"
  [[ -e "$out" ]] && die "fingerprint: refusing to overwrite $out"
  local list0 tmp status=0
  out="$(realpath -m "$out")" || die "fingerprint: cannot resolve $out"
  list0="$(mktemp "${TMPDIR:-/tmp}/bba-round4-fp.XXXXXX")" || die "fingerprint: mktemp failed"
  tmp="$out.tmp.$$"
  if ! (
    cd "$root" || exit 1
    find "${FINGERPRINT_DIRS[@]}" \
      \( -type d \( \
        -name node_modules -o -name .cache -o -name build -o -name lib \
        -o -name dist -o -name coverage -o -name __pycache__ -o -name .tmp \
        -o -name .work -o -name .git \
      \) -prune \) -o \
      \( -type d \( -path 'evals/results' -o -path '*/evals/results' \) -prune \) -o \
      \( -type f \( "${FINGERPRINT_PRED[@]}" \) -print0 \) || exit 1
    for f in "${FINGERPRINT_FILES[@]}"; do
      [[ -f "$f" ]] && printf '%s\0' "$f"
    done
    exit 0
  ) > "$list0"; then
    status=1
  fi
  if [[ "$status" == 0 ]]; then
    LC_ALL=C sort -z -u "$list0" -o "$list0" || status=1
  fi
  if [[ "$status" == 0 ]]; then
    : > "$tmp" || status=1
  fi
  if [[ "$status" == 0 ]]; then
    if ! (
      cd "$root" || exit 1
      while IFS= read -r -d '' f; do
        sha256sum -- "$f" >> "$tmp" || exit 1
      done < "$list0"
    ); then
      status=1
    fi
  fi
  rm -f "$list0"
  if [[ "$status" != 0 ]]; then
    rm -f "$tmp"
    die "fingerprint: failed while hashing $root"
  fi
  if [[ -e "$out" ]]; then
    rm -f "$tmp"
    die "fingerprint: refusing to overwrite $out"
  fi
  ln -- "$tmp" "$out" || { rm -f "$tmp"; die "fingerprint: cannot link $out"; }
  rm -f "$tmp"
  echo "fingerprint: root=$root output=$out"
}

record() {
  local label="$1"
  shift
  local log="$RUN_DIR/logs/$label.log"
  [[ -e "$log" ]] && die "record: refusing to overwrite $log"
  local start end code cmd
  cmd="$(printf '%q ' "$@")"
  cmd="${cmd% }"
  start="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  {
    printf '%s\n$ %s\n\n' "$start" "$cmd"
  } > "$log" || die "record: cannot create $log"
  set +e
  "$@" >> "$log" 2>&1
  code=$?
  set -e
  end="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  printf '\nexit=%s\nend=%s\n' "$code" "$end" >> "$log"
  printf '%s\tstart=%s\tend=%s\texit=%s\tcommand=%s\n' \
    "$label" "$start" "$end" "$code" "$cmd" >> "$STEPS"
  printf '%s exit=%s\n' "$label" "$code"
  [[ "$code" == 0 ]] || FAILED=1
}

run_probe() {
  local name="$1"
  shift
  local out code
  set +e
  out="$("$@" 2>&1)"
  code=$?
  set -e
  printf '%s_exit=%s\n%s\n' "$name" "$code" "$out"
}

run_image_probe() {
  local tag="$1" out code
  set +e
  out="$(docker image inspect --format '{{.Id}}' "$tag" 2>&1)"
  code=$?
  set -e
  printf 'image=%s id=%s inspect_exit=%s\n' "$tag" "${out:-<none>}" "$code"
}

cmd_run() {
  local dir=""
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --dir)
        [[ $# -ge 2 ]] || die "run: --dir needs a value"
        dir="$2"
        shift 2
        ;;
      *)
        die "run: unknown argument '$1'"
        ;;
    esac
  done
  if [[ -z "$dir" ]]; then
    dir="${BBA_EVIDENCE_DIR:-}"
  fi
  if [[ -z "$dir" ]]; then
    dir="$ROOT/evals/results/bba-round4-$(date -u +%Y%m%dT%H%M%SZ)"
  fi
  case "$dir" in
    /*) ;;
    *) dir="$ROOT/$dir" ;;
  esac
  RUN_DIR="$(realpath -m "$dir")"
  local results_root
  results_root="$(realpath -m "$ROOT/evals/results")"
  case "$RUN_DIR" in
    "$results_root"/*) ;;
    *) die "run: evidence directory must be under $results_root (got $RUN_DIR)" ;;
  esac
  [[ "$RUN_DIR" != "$results_root" ]] || die "run: evidence directory must be a package directory"
  if [[ ! -d "$RUN_DIR" ]]; then
    mkdir "$RUN_DIR" || die "run: cannot create $RUN_DIR"
  fi
  mkdir -p "$RUN_DIR/logs" "$RUN_DIR/browser" || die "run: cannot create package subdirectories"

  # Never overwrite runner-owned files.  Foreign files (for example a parent's
  # resource baseline or a red-test log) are allowed and are hashed at finalize.
  local owned=(steps.tsv environment.txt source-files.sha256 manifest.sha256 secret-scan.txt)
  local f label
  for f in "${owned[@]}"; do
    [[ -e "$RUN_DIR/$f" ]] && die "run: refusing to overwrite $RUN_DIR/$f; stage a new directory"
  done
  for label in "${STEP_LABELS[@]}"; do
    [[ -e "$RUN_DIR/logs/$label.log" ]] && die "run: refusing to overwrite $RUN_DIR/logs/$label.log"
  done

  cd "$ROOT"
  STEPS="$RUN_DIR/steps.tsv"
  : > "$STEPS" || die "run: cannot create $STEPS"
  FAILED=0

  {
    echo "utc_start=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    echo "run_dir=$RUN_DIR"
    echo "host=$(uname -sr)"
    local kernel_go_image="golang:1.26"
    local contract_go_image="${GO_IMAGE:-golang:1.26}"
    local contract_node_image="${NODE_IMAGE:-node:22-bookworm}"
    local kernel_postgres_image="postgres:16-bookworm"
    local browser_postgres_image="${BBA_DEV_STACK_PG_IMAGE:-postgres:16-alpine}"
    local browser_go_image="${BBA_DEV_STACK_GO_IMAGE:-golang:1.26}"
    local node22_image="${BBA_NODE_IMAGE:-node:22}"
    echo "kernel_go_image=$kernel_go_image"
    echo "contract_go_image=$contract_go_image"
    echo "contract_node_image=$contract_node_image"
    echo "kernel_postgres_image=$kernel_postgres_image"
    echo "browser_postgres_image=$browser_postgres_image"
    echo "browser_go_image=$browser_go_image"
    echo "node22_image=$node22_image"
    run_probe docker_version docker --version
    run_probe python3 "$PYTHON" --version
    local -A seen_images=()
    local tag
    for tag in "$kernel_go_image" "$contract_go_image" "$contract_node_image" \
      "$kernel_postgres_image" "$browser_postgres_image" "$browser_go_image" "$node22_image"; do
      [[ -n "${seen_images[$tag]:-}" ]] && continue
      seen_images[$tag]=1
      run_image_probe "$tag"
    done
  } > "$RUN_DIR/environment.txt" || die "run: cannot write environment.txt"

  record chain-proposal ./tools/kernel.sh test -run 'TestPlanProposal' -v
  record chain-worker ./tools/kernel.sh test -run 'TestWorker' -v
  record chain-summary ./tools/kernel.sh test -run 'TestGroupSummary|TestGroupContext' -v
  record schema ./tools/kernel.sh schema
  record go-test-all ./tools/kernel.sh test-all
  record contract-check ./tools/contract.sh check
  record node22-verify env BBA_NODE_OFFLINE=1 ./tools/node22-deps.sh verify
  record source-fingerprint bash "$SELF" fingerprint "$ROOT" "$RUN_DIR/source-files.sha256"
  record source-verify "$PYTHON" "$VERIFIER" verify-source \
    --project-root "$ROOT" \
    --manifest "$RUN_DIR/source-files.sha256"

  {
    echo "utc_end=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    echo "run_failed=$FAILED"
  } >> "$RUN_DIR/environment.txt"

  if [[ "$FAILED" != 0 ]]; then
    echo "round4-evidence: at least one step failed; raw logs and steps.tsv preserved in $RUN_DIR" >&2
    echo "round4-evidence: stage a new directory to retry; do not overwrite this one" >&2
    exit 1
  fi
  echo "round4-evidence: run complete at $RUN_DIR"
  echo "round4-evidence: next add REPORT.md and browser/, then: tools/round4-evidence.sh finalize '$RUN_DIR'"
}

cmd_finalize() {
  local dir="${1:-}"
  [[ -n "$dir" ]] || die "finalize: usage: finalize DIR [--rebuild]"
  shift
  local rebuild=()
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --rebuild) rebuild+=(--rebuild); shift ;;
      *) die "finalize: unknown argument '$1'" ;;
    esac
  done
  "$PYTHON" "$VERIFIER" finalize \
    --project-root "${BBA_PROJECT_ROOT:-$ROOT}" \
    --package-root "$dir" "${rebuild[@]}"
}

cmd_verify() {
  [[ $# -ge 2 ]] || die "verify: usage: verify PROJECT_ROOT PACKAGE_DIR [--hash-only-source]"
  local project="$1" package="$2"
  shift 2
  "$PYTHON" "$VERIFIER" verify --project-root "$project" --package-root "$package" "$@"
}

cmd_negative() {
  [[ $# -eq 2 ]] || die "negative: usage: negative PROJECT_ROOT PACKAGE_DIR"
  "$PYTHON" "$VERIFIER" negative --project-root "$1" --package-root "$2"
}

cmd_scan() {
  local dir="${1:-}"
  [[ -n "$dir" ]] || die "scan: usage: scan DIR [--output FILE]"
  shift
  "$PYTHON" "$VERIFIER" scan --package-root "$dir" "$@"
}

cmd_inventory() {
  local dir="${1:-}"
  [[ -n "$dir" ]] || die "inventory: usage: inventory DIR [--json]"
  shift
  "$PYTHON" "$VERIFIER" inventory --package-root "$dir" "$@"
}

usage() {
  sed -n '13,27p' "$SELF" | sed 's/^# \{0,1\}//'
}

case "${1:-run}" in
  run|"")
    shift || true
    cmd_run "$@"
    ;;
  finalize)
    shift
    cmd_finalize "$@"
    ;;
  verify)
    shift
    cmd_verify "$@"
    ;;
  negative)
    shift
    cmd_negative "$@"
    ;;
  scan)
    shift
    cmd_scan "$@"
    ;;
  inventory)
    shift
    cmd_inventory "$@"
    ;;
  fingerprint)
    shift
    [[ $# -eq 2 ]] || die "fingerprint: usage: fingerprint PROJECT_ROOT OUTPUT"
    fingerprint_tree "$1" "$2"
    ;;
  help|--help|-h)
    usage
    ;;
  *)
    usage >&2
    die "unknown command '$1'"
    ;;
esac
