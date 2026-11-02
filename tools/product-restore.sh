#!/usr/bin/env bash
# product-restore: isolated backup/restore/restart rehearsal with a finish report.
#
#   bash tools/product-restore.sh --output <new-directory> [--run-id <id>]
#
# The command owns exactly one disposable PostgreSQL 16 container and one unix
# socket volume, both named from its unique run id and labelled
# bba.p018.drill=1 / bba.p018.run=<id>. It creates the current schema head with
# the kernel migrations, seeds unresolved pending/UNKNOWN/DISPATCHING work plus
# the proposal work ledger, the S2 graph mappings and the S4 login session,
# dumps with pg_dump, restores with pg_restore, verifies a generic schema+data
# fingerprint over every application table, replays the restart recovery paths
# with zero adapter calls, and only cleans up resources it owns and labelled
# itself.
#
# Production is out of scope by construction: an inherited Task Engine DSN or
# any other database URL makes the command refuse to run, and every child
# process gets an explicit fixture-only environment. The drill itself is the
# existing p018 program (backend/cmd/p018-restore) driven by its frozen shell
# driver; this command never starts a second kernel and never writes outside its
# own output directory.
#
# Exit codes: 0 pass, 1 drill or verification failure, 2 usage/precondition
# refusal. Raw drill stdout/stderr and the machine-readable report.json stay in
# the output directory (UTC start/end, exits, hashes, cleanup state).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SCRIPT_REL="tools/product-restore.sh"
DRILL="$ROOT/tools/p018-restore-smoke.sh"

usage() {
  printf '%s\n' 'usage: bash tools/product-restore.sh --output <new-directory> [--run-id <id>]' >&2
  exit 2
}

note() { printf 'product-restore: %s\n' "$*" >&2; }
refuse() { printf 'product-restore: refusing: %s\n' "$*" >&2; exit 2; }

OUTPUT=""
RUN_ID=""
while (($# > 0)); do
  case "$1" in
    --output)
      (($# >= 2)) || usage
      OUTPUT="$2"
      shift 2
      ;;
    --run-id)
      (($# >= 2)) || usage
      RUN_ID="$2"
      shift 2
      ;;
    -h|--help) usage ;;
    *) refuse "unknown argument: $1" ;;
  esac
done
[[ -n "$OUTPUT" ]] || usage

# A production DSN or credential must never be inherited. The drill builds its
# own unix-socket DSN inside its own container, so an inherited BBA_FIXTURE_DSN
# is ignored rather than reused; the production names and drill overrides are
# refused outright.
for name in BBA_TASKENGINE_DSN BBA_DATABASE_URL DATABASE_URL BBA_PRODUCTION_DSN \
  PGPASSWORD BBA_P018_STAGE BBA_P018_RUN BBA_P018_EVIDENCE_DIR \
  P018_RUN_ID P018_EVIDENCE_DIR; do
  if [[ -n "${!name:-}" ]]; then
    refuse "$name is set; this command only runs its own isolated fixture"
  fi
done

command -v docker >/dev/null 2>&1 || refuse "docker is required"
docker info >/dev/null 2>&1 || refuse "docker daemon is not reachable"
command -v python3 >/dev/null 2>&1 || refuse "python3 is required for the finish report"
[[ -f "$DRILL" ]] || refuse "missing drill driver: $DRILL"

[[ ! -e "$OUTPUT" ]] || refuse "output must be a new path: $OUTPUT"
OUTPUT="$(python3 -c 'import os,sys; print(os.path.abspath(sys.argv[1]))' "$OUTPUT")"
PARENT="$(dirname "$OUTPUT")"
[[ -d "$PARENT" && -w "$PARENT" ]] || refuse "output parent must be an existing writable directory: $PARENT"

if [[ -z "$RUN_ID" ]]; then
  suffix="$( (openssl rand -hex 4 2>/dev/null || od -An -N4 -tx1 /dev/urandom | tr -d ' \n') )"
  [[ "$suffix" =~ ^[a-z0-9]{8}$ ]] || refuse "could not generate a run id suffix"
  RUN_ID="pr_$(date -u +%Y%m%dT%H%M%SZ | tr '[:upper:]' '[:lower:]')_$suffix"
fi
[[ "$RUN_ID" =~ ^[a-z0-9][a-z0-9_]{3,31}$ ]] || refuse "run id must match ^[a-z0-9][a-z0-9_]{3,31}$: $RUN_ID"

CONTAINER="bba-p018-restore-$RUN_ID"
SOCKET_VOLUME="$CONTAINER-socket"
LABEL_RUN="bba.p018.run=$RUN_ID"
if docker container inspect "$CONTAINER" >/dev/null 2>&1; then
  refuse "owned container name already exists: $CONTAINER"
fi
if docker volume inspect "$SOCKET_VOLUME" >/dev/null 2>&1; then
  refuse "owned volume name already exists: $SOCKET_VOLUME"
fi

STREAMS="$(mktemp -d "${TMPDIR:-/tmp}/bba-product-restore.XXXXXX")"
cleanup_streams() { rm -rf "$STREAMS"; }
trap cleanup_streams EXIT

RAW_TMP_OUT="$STREAMS/drill.stdout.log"
RAW_TMP_ERR="$STREAMS/drill.stderr.log"

# Safety net: the driver removes its own trap-owned resources. If it was killed
# without running its trap, only containers/volumes carrying this run's label
# are removed here.
cleanup_owned() {
  local containers volumes item
  containers="$(docker ps -aq --filter "label=$LABEL_RUN" 2>/dev/null || true)"
  if [[ -n "$containers" ]]; then
    # shellcheck disable=SC2086
    docker rm -f $containers >/dev/null 2>&1 || true
  fi
  volumes="$(docker volume ls -q --filter "label=$LABEL_RUN" 2>/dev/null || true)"
  for item in $volumes; do
    docker volume rm "$item" >/dev/null 2>&1 || true
  done
}
cleanup_owned

START_UTC="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
START_EPOCH="$(date -u +%s)"

note "run=$RUN_ID output=$OUTPUT"
note "container=$CONTAINER volume=$SOCKET_VOLUME"
note "delegating the isolated pg_dump/pg_restore/restart drill to $DRILL"

set +e
P018_RUN_ID="$RUN_ID" P018_EVIDENCE_DIR="$OUTPUT" bash "$DRILL" >"$RAW_TMP_OUT" 2>"$RAW_TMP_ERR"
DRILL_EXIT=$?
set -e

mkdir -p "$OUTPUT"
RAW_OUT="$OUTPUT/drill.stdout.log"
RAW_ERR="$OUTPUT/drill.stderr.log"
cp "$RAW_TMP_OUT" "$RAW_OUT"
cp "$RAW_TMP_ERR" "$RAW_ERR"
SUMMARY="$OUTPUT/drill-summary.json"
REPORT="$OUTPUT/report.json"

# The frozen driver writes its own evidence files (seed/verify JSON, exits.txt,
# the independent psql proof). Collapse them into one stable summary that the
# finish report and the runbook consume. Missing evidence is reported, never
# guessed: every absent field stays null.
OUTPUT="$OUTPUT" RUN_ID="$RUN_ID" SUMMARY="$SUMMARY" python3 - <<'PY'
import hashlib
import json
import os
import pathlib

out = pathlib.Path(os.environ["OUTPUT"])
summary_path = pathlib.Path(os.environ["SUMMARY"])


def load(name):
    path = out / name
    if not path.is_file():
        return None
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except ValueError:
        return None


def sha256(path):
    digest = hashlib.sha256()
    with open(path, "rb") as handle:
        for chunk in iter(lambda: handle.read(1 << 20), b""):
            digest.update(chunk)
    return digest.hexdigest()


seed = load("seed-evidence.json")
verify = load("verify-evidence.json")
seed_snapshot = (seed or {}).get("snapshot") or {}
verify_before = (verify or {}).get("before_recovery") or {}
verify_after = (verify or {}).get("after_recovery") or {}
exits = {}
exits_path = out / "exits.txt"
if exits_path.is_file():
    for line in exits_path.read_text(encoding="utf-8").splitlines():
        if "=" not in line:
            continue
        key, value = line.split("=", 1)
        try:
            exits[key] = int(value.strip())
        except ValueError:
            exits[key] = value.strip()
proof_path = out / "db-restore-proof.txt"
proof_text = proof_path.read_text(encoding="utf-8", errors="replace") if proof_path.is_file() else ""

restart_scenarios = []
for scenario in (seed or {}).get("scenarios") or []:
    row = next(
        (item for item in (verify_after.get("unresolved") or [])
         if item.get("operation_id") == scenario.get("operation_id")),
        {},
    )
    restart_scenarios.append({
        "name": scenario.get("name"),
        "operation_id": scenario.get("operation_id"),
        "execution_state": row.get("execution_state"),
        "disposition": row.get("disposition"),
        "effect_certainty": row.get("effect_certainty"),
        "gateway_state": row.get("gateway_state"),
        "permit_state": row.get("permit_state"),
        "reserved_slots": row.get("reserved_slots"),
        "active_claims": row.get("active_claims"),
        "held_send_locks": row.get("held_send_locks"),
    })

summary = {
    "run_id": os.environ["RUN_ID"],
    "driver": "tools/p018-restore-smoke.sh",
    "seed": {
        "present": seed is not None,
        "outcome": (seed or {}).get("outcome"),
        "schema_version": seed_snapshot.get("schema_version"),
        "schema_fingerprint": seed_snapshot.get("schema_fingerprint"),
        "data_fingerprint": seed_snapshot.get("data_fingerprint"),
        "unresolved_count": len(seed_snapshot.get("unresolved") or []),
        "work_items": seed_snapshot.get("work_items") or [],
        "checks": len((seed or {}).get("checks") or []),
    },
    "verify": {
        "present": verify is not None,
        "outcome": (verify or {}).get("outcome"),
        "checks": len((verify or {}).get("checks") or []),
        "work_items_reconciled": (verify or {}).get("work_items_reconciled"),
        "work_items_second_pass": (verify or {}).get("work_items_second_pass"),
        "recovered_dispatching": (verify or {}).get("recovered_dispatching"),
        "expired_permits": (verify or {}).get("expired_permits"),
        "reconciled_timeouts": (verify or {}).get("reconciled_timeouts"),
    },
    "restore": {
        "restored_matches_source": (verify or {}).get("restored_matches_source") is True,
        "adapter_calls_after_restore": (verify or {}).get("adapter_calls_after_restore"),
        "work_items_claimable": (verify or {}).get("work_items_claimable"),
        "expired_work_item_claims": (verify or {}).get("expired_work_item_claims"),
        "restored_schema_version": verify_before.get("schema_version"),
        "restored_schema_fingerprint": verify_before.get("schema_fingerprint"),
        "restored_data_fingerprint": verify_before.get("data_fingerprint"),
        "mismatch_components": (verify or {}).get("mismatch_components") or [],
        "work_items_after": verify_after.get("work_items") or [],
        "unresolved_after": verify_after.get("unresolved") or [],
    },
    "restart": {
        "recovered_dispatching": (verify or {}).get("recovered_dispatching"),
        "recovery_second_pass": (verify or {}).get("recovery_second_pass"),
        "expired_permits": (verify or {}).get("expired_permits"),
        "reconciled_timeouts": (verify or {}).get("reconciled_timeouts"),
        "reconcile_second_pass": (verify or {}).get("reconcile_second_pass"),
        "work_items_reconciled": (verify or {}).get("work_items_reconciled"),
        "work_items_second_pass": (verify or {}).get("work_items_second_pass"),
        "work_items_claimable": (verify or {}).get("work_items_claimable"),
        "expired_work_item_claims": (verify or {}).get("expired_work_item_claims"),
        "adapter_calls_after_restore": (verify or {}).get("adapter_calls_after_restore"),
        "unresolved_before": verify_before.get("unresolved") or [],
        "unresolved_after": verify_after.get("unresolved") or [],
        "scenarios": restart_scenarios,
    },
    "exits": exits,
    "db_proof": {
        "pass": proof_text.rstrip().endswith("proof=pass") and exits.get("db_proof_exit") == 0,
        "path": proof_path.name if proof_path.is_file() else None,
        "sha256": sha256(proof_path) if proof_path.is_file() else None,
    },
}
summary_path.write_text(json.dumps(summary, indent=2, ensure_ascii=False) + "\n", encoding="utf-8")
PY

{
  printf '== drill stdout ==\n'
  cat "$RAW_OUT"
} >&2
{
  printf '== drill stderr ==\n'
  cat "$RAW_ERR"
} >&2

# The driver's own EXIT trap has run by now; remove anything labelled with this
# run id that survived, then prove nothing owned is left behind.
cleanup_owned
LEFTOVER_CONTAINERS="$(docker ps -aq --filter "label=$LABEL_RUN" 2>/dev/null || true)"
LEFTOVER_VOLUMES="$(docker volume ls -q --filter "label=$LABEL_RUN" 2>/dev/null || true)"
if [[ -n "$LEFTOVER_CONTAINERS" ]]; then
  printf '%s\n' "$LEFTOVER_CONTAINERS" >&2
fi
if [[ -n "$LEFTOVER_VOLUMES" ]]; then
  printf '%s\n' "$LEFTOVER_VOLUMES" >&2
fi

END_UTC="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
END_EPOCH="$(date -u +%s)"
DURATION=$((END_EPOCH - START_EPOCH))

DRILL_EXIT="$DRILL_EXIT" OUTPUT="$OUTPUT" RUN_ID="$RUN_ID" CONTAINER="$CONTAINER" \
SOCKET_VOLUME="$SOCKET_VOLUME" START_UTC="$START_UTC" END_UTC="$END_UTC" \
DURATION="$DURATION" RAW_OUT="$RAW_OUT" RAW_ERR="$RAW_ERR" SUMMARY="$SUMMARY" \
REPORT="$REPORT" SCRIPT_REL="$SCRIPT_REL" LEFTOVER_CONTAINERS="$LEFTOVER_CONTAINERS" \
LEFTOVER_VOLUMES="$LEFTOVER_VOLUMES" python3 - <<'PY'
import hashlib
import json
import os
import pathlib

env = os.environ
output = pathlib.Path(env["OUTPUT"])
report_path = pathlib.Path(env["REPORT"])
problems = []


def sha256(path):
    digest = hashlib.sha256()
    with open(path, "rb") as handle:
        for chunk in iter(lambda: handle.read(1 << 20), b""):
            digest.update(chunk)
    return digest.hexdigest()


def stream(path):
    path = pathlib.Path(path)
    if not path.is_file():
        return {"path": path.name, "present": False, "bytes": 0, "sha256": None}
    return {"path": path.name, "present": True, "bytes": path.stat().st_size, "sha256": sha256(path)}


drill_exit = int(env["DRILL_EXIT"])
summary_path = pathlib.Path(env["SUMMARY"])
summary = None
if summary_path.is_file():
    try:
        summary = json.loads(summary_path.read_text(encoding="utf-8"))
    except ValueError as exc:
        problems.append(f"drill_summary_invalid: {exc}")
else:
    problems.append("drill_summary_missing")

if drill_exit != 0:
    problems.append(f"drill_exit={drill_exit}")
leftovers = {
    "containers": [line for line in env.get("LEFTOVER_CONTAINERS", "").splitlines() if line],
    "volumes": [line for line in env.get("LEFTOVER_VOLUMES", "").splitlines() if line],
}
if leftovers["containers"] or leftovers["volumes"]:
    problems.append("owned_resources_not_removed")

if summary is not None:
    exits = summary.get("exits") or {}
    if not isinstance(exits, dict) or any(int(value) != 0 for value in exits.values()):
        problems.append(f"drill_exits={exits}")
    restore = summary.get("restore") or {}
    if restore.get("restored_matches_source") is not True:
        problems.append("restored_matches_source_not_true")
    if restore.get("adapter_calls_after_restore") != 0:
        problems.append(f"adapter_calls_after_restore={restore.get('adapter_calls_after_restore')}")
    if restore.get("work_items_claimable") != 0:
        problems.append(f"work_items_claimable={restore.get('work_items_claimable')}")
    if restore.get("expired_work_item_claims") != 0:
        problems.append(f"expired_work_item_claims={restore.get('expired_work_item_claims')}")
    if restore.get("mismatch_components"):
        problems.append(f"fingerprint_mismatch_components={restore.get('mismatch_components')}")
    seed = summary.get("seed") or {}
    if not seed.get("schema_fingerprint") or not seed.get("data_fingerprint"):
        problems.append("seed_fingerprints_missing")
    if restore.get("restored_schema_fingerprint") != seed.get("schema_fingerprint"):
        problems.append("schema_fingerprint_mismatch")
    if restore.get("restored_data_fingerprint") != seed.get("data_fingerprint"):
        problems.append("data_fingerprint_mismatch")
    db_proof = summary.get("db_proof") or {}
    if db_proof.get("pass") is not True:
        problems.append("independent_db_proof_failed")
    restart = summary.get("restart") or {}
    for field, want in (
        ("recovered_dispatching", 1),
        ("recovery_second_pass", 0),
        ("expired_permits", 0),
        ("reconciled_timeouts", 3),
        ("reconcile_second_pass", 0),
        ("work_items_reconciled", 2),
        ("work_items_second_pass", 1),
        ("work_items_claimable", 0),
        ("expired_work_item_claims", 0),
        ("adapter_calls_after_restore", 0),
    ):
        if restart.get(field) != want:
            problems.append(f"restart.{field}={restart.get(field)} want={want}")
    scenarios = restart.get("scenarios") or []
    names = sorted(item.get("name") for item in scenarios)
    if names != ["dispatching", "pending", "unknown"]:
        problems.append(f"restart.scenarios={names}")
    for item in scenarios:
        state = item.get("execution_state")
        if item.get("name") == "pending":
            if state != "OBSERVED" or item.get("gateway_state") != "UNKNOWN" or item.get("permit_state") != "consumed":
                problems.append(f"restart.pending={item}")
        elif item.get("name") in ("unknown", "dispatching"):
            if state != "UNKNOWN" or item.get("permit_state") not in ("consumed", "issued"):
                problems.append(f"restart.{item.get('name')}={item}")
        for field in ("reserved_slots", "active_claims", "held_send_locks"):
            if item.get(field) != 1:
                problems.append(f"restart.{item.get('name')}.{field}={item.get(field)}")

state = "PASS" if not problems else "FAIL"
report = {
    "command": "product-restore",
    "scope": "isolated_fixture_kernel_pg_dump_restore",
    "note": "Disposable PostgreSQL fixture only. No production database, no QQ, no live service.",
    "run_id": env["RUN_ID"],
    "output": str(output),
    "driver": {"path": env["SCRIPT_REL"], "exit_code": drill_exit},
    "start_utc": env["START_UTC"],
    "end_utc": env["END_UTC"],
    "duration_seconds": int(env["DURATION"]),
    "resources": {
        "container": env["CONTAINER"],
        "socket_volume": env["SOCKET_VOLUME"],
        "labels": {"bba.p018.drill": "1", "bba.p018.run": env["RUN_ID"]},
        "networks": "none",
        "owned_resources_removed": not leftovers["containers"] and not leftovers["volumes"],
        "leftover_containers": leftovers["containers"],
        "leftover_volumes": leftovers["volumes"],
    },
    "streams": {
        "stdout": stream(env["RAW_OUT"]),
        "stderr": stream(env["RAW_ERR"]),
    },
    "drill_summary": {
        "path": summary_path.name,
        "present": summary is not None,
        "sha256": sha256(summary_path) if summary_path.is_file() else None,
    },
    "production_dsn_used": False,
    "real_qq_calls": 0,
    "external_credentials_read": 0,
    "problems": problems,
    "state": state,
}
with report_path.open("x", encoding="utf-8") as handle:
    json.dump(report, handle, indent=2, ensure_ascii=False)
    handle.write("\n")
print(json.dumps({"state": state, "report": str(report_path), "problems": problems}, ensure_ascii=False))
PY

STATE="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1], encoding="utf-8"))["state"])' "$REPORT")"
if [[ "$STATE" != "PASS" ]]; then
  note "FAIL: see $REPORT"
  exit 1
fi
note "PASS: $REPORT"
exit 0
