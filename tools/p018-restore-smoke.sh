#!/usr/bin/env bash
# P0-18 reproducible backup/restore drill (isolated fixture only).
#
# One disposable PostgreSQL 16 container (network none, unix socket under
# /fixture) holds two databases: the seeded source and a fresh restore target.
# The Go seed stage builds three unresolved external states through the real
# task engine: a join request later observed pending after a lost receipt, an
# UNKNOWN-after-send execution, and an operation left DISPATCHING after a
# permit. pg_dump writes a custom-format dump inside the container; pg_restore
# loads it into the second database.
# The Go verify stage proves the restored copy equals the source before
# recovery (schema and every table's data fingerprint), replays
# RecoverDispatching/ExpirePermits/ReconcileTimeouts/work-item reconciliation,
# allows zero adapter calls, and reads the public GET /v1/tasks API. A second,
# independent psql proof checks the unresolved rows, the proposal ledger, the
# S2 graph mappings, the S4 login session and the zero-new-work ledger deltas.
#
# This script never reads a production DSN, credential file or QQ state. It
# never contacts QQ. No host port is published and the fixture is destroyed by
# an EXIT/INT/TERM trap.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
EVIDENCE="${P018_EVIDENCE_DIR:-/tmp/bba-p018-restore-$(date -u +%Y%m%dT%H%M%SZ)}"
RUN_ID="${P018_RUN_ID:-$(date -u +%Y%m%dt%H%M%S)_$$}"
PG_DSN='postgres://bba_fixture@/bba_fixture_admin?host=/fixture&sslmode=disable'
FIXTURE_NAME="bba-p018-restore-${RUN_ID}"
SOCKET_VOLUME="${FIXTURE_NAME}-socket"
DUMP_NAME="p018-${RUN_ID}.dump"
SRC_DB="p018_${RUN_ID}_src"
DST_DB="p018_${RUN_ID}_dst"
DB_PSQL_EXIT=1
DB_PROOF_EXIT=1
VERIFY_EXIT=1
RESTORE_EXIT=1
CREATE_EXIT=1
DUMP_EXIT=1
SEED_EXIT=1

if [[ ! "$RUN_ID" =~ ^[a-z0-9][a-z0-9_]{3,31}$ ]]; then
  echo "P018_RUN_ID must match ^[a-z0-9][a-z0-9_]{3,31}$: $RUN_ID" >&2
  exit 2
fi
if [[ "$EVIDENCE" != /* ]]; then
  echo "P018_EVIDENCE_DIR must be an absolute path: $EVIDENCE" >&2
  exit 2
fi
if [[ -e "$EVIDENCE" ]]; then
  echo "evidence directory already exists: $EVIDENCE" >&2
  exit 1
fi
mkdir -m 700 "$EVIDENCE"

# Fingerprint the drill, the kernel it exercises, and the wire types it reads.
CODE_FILES=()
while IFS= read -r file; do
  CODE_FILES+=("$file")
done < <(
  cd "$ROOT"
  {
    find backend/cmd/p018-restore -maxdepth 1 -type f -name '*.go' -print
    find backend/internal/taskengine -type f \( -name '*.go' -o -name '*.sql' \) -print
    printf '%s\n' backend/internal/contract/types.gen.go
    printf '%s\n' tools/p018-restore-smoke.sh
  } | LC_ALL=C sort -u
)
for required in backend/cmd/p018-restore/main.go backend/cmd/p018-restore/snapshot.go \
  backend/internal/taskengine/migrations/0001_core.sql \
  backend/internal/contract/types.gen.go tools/p018-restore-smoke.sh; do
  if [[ ! -f "$ROOT/$required" ]]; then
    echo "missing fingerprinted source: $required" >&2
    exit 1
  fi
done
(
  cd "$ROOT"
  sha256sum "${CODE_FILES[@]}"
) > "$EVIDENCE/code-files.txt"
CODE_SHA="$(sha256sum "$EVIDENCE/code-files.txt" | cut -d' ' -f1)"

cleanup() {
  docker rm -f "$FIXTURE_NAME" >/dev/null 2>&1 || true
  docker volume rm "$SOCKET_VOLUME" >/dev/null 2>&1 || true
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

docker volume create --label bba.p018.drill=1 --label "bba.p018.run=$RUN_ID" "$SOCKET_VOLUME" >/dev/null
docker run --rm --network none -v "$SOCKET_VOLUME:/fixture" postgres:16-bookworm chmod 0777 /fixture
docker run -d --rm --network none --name "$FIXTURE_NAME" \
  --label bba.p018.drill=1 --label "bba.p018.run=$RUN_ID" \
  --tmpfs /var/lib/postgresql/data:rw \
  -v "$SOCKET_VOLUME:/fixture" \
  -e POSTGRES_USER=bba_fixture -e POSTGRES_DB=bba_fixture_admin \
  -e PGHOST=/fixture -e POSTGRES_HOST_AUTH_METHOD=trust \
  postgres:16-bookworm postgres -c listen_addresses='' -c unix_socket_directories='/var/run/postgresql,/fixture' -c unix_socket_permissions=0777 >/dev/null

ready=0
for _attempt in $(seq 1 600); do
  if docker exec "$FIXTURE_NAME" psql -h /fixture -U bba_fixture -d bba_fixture_admin -Atc 'SELECT 1' >/dev/null 2>&1; then
    ready=1
    break
  fi
  sleep 0.1
done
if [[ "$ready" != 1 ]]; then
  docker logs "$FIXTURE_NAME" >&2 || true
  echo "isolated PostgreSQL fixture did not become ready" >&2
  exit 1
fi

{
  echo "utc_started=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "host=$(uname -srm)"
  echo "go_image=$(docker image inspect golang:1.26 --format '{{.Id}}' 2>/dev/null || echo not-local)"
  echo "postgres_image=$(docker image inspect postgres:16-bookworm --format '{{.Id}}' 2>/dev/null || echo not-local)"
  echo "code_sha=$CODE_SHA"
  echo "code_files=$EVIDENCE/code-files.txt"
  echo "run_id=$RUN_ID"
  echo "source_database=$SRC_DB"
  echo "restored_database=$DST_DB"
  echo "fixture_container=$FIXTURE_NAME"
  echo "fixture_socket_volume=$SOCKET_VOLUME"
  echo "fixture_dsn=$PG_DSN"
  echo "networks=none"
  echo "mode=disposable_pg_dump_restore_drill"
  echo "scope=isolated_fixture_kernel_pg_dump_restore"
  echo "real_four_account_fault_injection=not_claimed"
  echo "writes=fixture_only"
  echo "real_qq_calls=0"
  echo "external_credentials_read=0"
  echo "evidence_dir=$EVIDENCE"
} > "$EVIDENCE/environment.txt"

echo "== seed =="
set +e
docker run --rm --network none --user "$(id -u):$(id -g)" \
  -e HOME=/tmp/home -e GOPATH=/work/.cache/gopath -e GOMODCACHE=/work/.cache/gomod \
  -e GOCACHE=/work/.cache/gobuild -e GOPROXY=off -e GOSUMDB=off -e GOFLAGS=-mod=readonly \
  -e BBA_FIXTURE_ONLY=1 -e BBA_FIXTURE_DSN="$PG_DSN" \
  -e BBA_P018_STAGE=seed -e BBA_P018_RUN="$RUN_ID" -e BBA_P018_EVIDENCE_DIR=/evidence \
  -v "$ROOT:/work" -v "$SOCKET_VOLUME:/fixture" -v "$EVIDENCE:/evidence" \
  -w /work/backend golang:1.26 go run ./cmd/p018-restore 2>&1 | tee "$EVIDENCE/seed.log"
SEED_EXIT=${PIPESTATUS[0]}
set -e
echo "seed_exit=$SEED_EXIT" > "$EVIDENCE/exits.txt"

finalize() {
  local run_exit="$1" proof_exit="$2"
  set +e
  # Machine-readable drill outcome for the parent's aggregation. The fixture
  # kernel proof is deliberately not presented as a live four-account run.
  python3 - "$EVIDENCE" "$RUN_ID" "$SRC_DB" "$DST_DB" "$SEED_EXIT" "$DUMP_EXIT" \
    "$CREATE_EXIT" "$RESTORE_EXIT" "$VERIFY_EXIT" "$DB_PSQL_EXIT" "$DB_PROOF_EXIT" <<'PY'
import json
import pathlib
import sys

evidence = pathlib.Path(sys.argv[1])
run_id, src, dst = sys.argv[2], sys.argv[3], sys.argv[4]
seed_exit, dump_exit, create_exit, restore_exit, verify_exit, psql_exit, proof_exit = map(int, sys.argv[5:12])


def load(name):
    try:
        return json.loads((evidence / name).read_text(encoding="utf-8"))
    except Exception:
        return None


def read_text(name):
    try:
        return (evidence / name).read_text(encoding="utf-8").strip()
    except Exception:
        return ""


seed = load("seed-evidence.json") or {}
verify = load("verify-evidence.json") or {}
after = verify.get("after_recovery") or {}
summary = {
    "scope": "isolated_fixture_kernel_pg_dump_restore",
    "note": "disposable PostgreSQL fixture only; not real four-account QQ fault injection",
    "run_id": run_id,
    "source_database": src,
    "restored_database": dst,
    "exits": {
        "seed": seed_exit,
        "pg_dump": dump_exit,
        "pg_restore_create": create_exit,
        "pg_restore": restore_exit,
        "verify": verify_exit,
        "psql_proof": psql_exit,
        "db_proof": proof_exit,
    },
    "dump": {
        "file": f"p018-{run_id}.dump",
        "bytes": read_text("dump-size.txt"),
        "sha256": read_text("dump-sha256.txt"),
    },
    "seed": {
        "outcome": seed.get("outcome"),
        "adapter_calls_before_dump": seed.get("adapter_calls_before_dump"),
        "scenarios": [
            {key: item.get(key) for key in ("name", "task_id", "operation_id", "execution_id", "account_id", "runtime_id", "target_id", "group_id",
                                            "work_item_id", "proposal_id", "application_id", "attempt_id")}
            for item in seed.get("scenarios", [])
        ],
        "unresolved_before": seed.get("snapshot", {}).get("unresolved"),
        "projections_before": seed.get("snapshot", {}).get("projections"),
        "work_items_before": seed.get("snapshot", {}).get("work_items"),
        "schema_version": seed.get("snapshot", {}).get("schema_version"),
        "schema_fingerprint": seed.get("snapshot", {}).get("schema_fingerprint"),
        "data_fingerprint": seed.get("snapshot", {}).get("data_fingerprint"),
        "graph_root_external": seed.get("graph_root_external"),
        "graph_outbox_id": seed.get("graph_outbox_id"),
        "graph_snapshot_id": seed.get("graph_snapshot_id"),
        "login_session_id": seed.get("login_session_id"),
        "login_event_id": seed.get("login_event_id"),
        "checks": seed.get("checks", []),
    },
    "restore": {
        "restored_matches_source": verify.get("restored_matches_source"),
        "adapter_calls_after_restore": verify.get("adapter_calls_after_restore"),
        "recovered_dispatching": verify.get("recovered_dispatching"),
        "recovery_second_pass": verify.get("recovery_second_pass"),
        "expired_permits": verify.get("expired_permits"),
        "reconciled_timeouts": verify.get("reconciled_timeouts"),
        "reconcile_second_pass": verify.get("reconcile_second_pass"),
        "work_items_reconciled": verify.get("work_items_reconciled"),
        "work_items_second_pass": verify.get("work_items_second_pass"),
        "work_items_claimable": verify.get("work_items_claimable"),
        "expired_work_item_claims": verify.get("expired_work_item_claims"),
        "work_items_after": after.get("work_items"),
        "unresolved_after": after.get("unresolved"),
        "projections_after": after.get("projections"),
        "totals_before": verify.get("before_recovery", {}).get("totals"),
        "totals_after": after.get("totals"),
        "restored_schema_fingerprint": verify.get("before_recovery", {}).get("schema_fingerprint"),
        "restored_data_fingerprint": verify.get("before_recovery", {}).get("data_fingerprint"),
        "checks": verify.get("checks", []),
    },
    "db_proof": {
        "pass": proof_exit == 0 and psql_exit == 0,
        "file": "db-restore-proof.txt",
        "result": read_text("db-restore-proof.txt"),
    },
}
(evidence / "drill-summary.json").write_text(json.dumps(summary, indent=2) + "\n", encoding="utf-8")
PY
  python3 - "$EVIDENCE" > "$EVIDENCE/sensitive-scan.txt" <<'PY'
import pathlib
import sys

evidence = pathlib.Path(sys.argv[1])
skip = {"sensitive-scan.txt", "evidence-sha256.txt", "evidence-verify.json", "manifest-verify.txt"}
bad = []
for path in sorted(evidence.rglob("*")):
    if not path.is_file() or path.name in skip:
        continue
    data = path.read_bytes()
    for marker in (b"password", b"Authorization", b"Bearer ", b"api_key",
                   b"api-key", b"onebot_token", b"webui_token", b"client_secret"):
        if marker in data:
            bad.append(f"FOUND marker={marker.decode()} file={path.name}")
if bad:
    for line in sorted(set(bad)):
        print(line)
    print("scan=fail")
    sys.exit(1)
print("scan=pass")
PY
  local scan_exit=$?

  (
    cd "$EVIDENCE"
    find . -type f ! -name evidence-sha256.txt ! -name evidence-verify.json ! -name manifest-verify.txt -print0 \
      | LC_ALL=C sort -z | xargs -0 sha256sum
  ) > "$EVIDENCE/evidence-sha256.txt"

  (
    cd "$EVIDENCE"
    sha256sum -c evidence-sha256.txt
  ) > "$EVIDENCE/manifest-verify.txt" 2>&1
  local hash_exit=$?
  printf 'hash_exit=%s\n' "$hash_exit" >> "$EVIDENCE/manifest-verify.txt"

  python3 - "$EVIDENCE" "$run_exit" "$proof_exit" "$scan_exit" "$hash_exit" <<'PY'
import hashlib
import json
import pathlib
import sys
import time

evidence = pathlib.Path(sys.argv[1])
manifest = (evidence / "evidence-sha256.txt").read_bytes()
result = {
    "manifest_sha256": hashlib.sha256(manifest).hexdigest(),
    "manifest_verified": int(sys.argv[5]) == 0,
    "run_exit": int(sys.argv[2]),
    "proof_exit": int(sys.argv[3]),
    "scan_exit": int(sys.argv[4]),
    "hash_exit": int(sys.argv[5]),
    "utc_verified": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
}
(evidence / "evidence-verify.json").write_text(json.dumps(result, indent=2) + "\n", encoding="utf-8")
PY
  echo "P0-18 restore drill run_exit=$run_exit proof_exit=$proof_exit scan_exit=$scan_exit hash_exit=$hash_exit evidence=$EVIDENCE code_sha=$CODE_SHA"
  if [[ "$run_exit" != 0 ]]; then return "$run_exit"; fi
  if [[ "$proof_exit" != 0 ]]; then return "$proof_exit"; fi
  if [[ "$scan_exit" != 0 ]]; then return "$scan_exit"; fi
  return "$hash_exit"
}

if [[ "$SEED_EXIT" != 0 ]]; then
  finalize "$SEED_EXIT" 1
  exit $?
fi

SEEDED_SRC="$(python3 - "$EVIDENCE/seed-evidence.json" 2>/dev/null <<'PY'
import json
import sys

try:
    print(json.load(open(sys.argv[1], encoding="utf-8")).get("source_database", ""))
except Exception:
    print("")
PY
)"
SEEDED_DST="$(python3 - "$EVIDENCE/seed-evidence.json" 2>/dev/null <<'PY'
import json
import sys

try:
    print(json.load(open(sys.argv[1], encoding="utf-8")).get("restored_database", ""))
except Exception:
    print("")
PY
)"
if [[ "$SEEDED_SRC" != "$SRC_DB" || "$SEEDED_DST" != "$DST_DB" ]]; then
  echo "seed evidence database names do not match this run" >&2
  finalize 1 1
  exit $?
fi

echo "== pg_dump =="
set +e
docker exec "$FIXTURE_NAME" pg_dump -h /fixture -U bba_fixture -d "$SRC_DB" \
  --format=custom --no-sync --file="/fixture/$DUMP_NAME" > "$EVIDENCE/pg-dump.log" 2>&1
DUMP_EXIT=$?
set -e
echo "dump_exit=$DUMP_EXIT" >> "$EVIDENCE/exits.txt"
if [[ "$DUMP_EXIT" != 0 ]]; then
  finalize "$DUMP_EXIT" 1
  exit $?
fi
docker exec "$FIXTURE_NAME" sha256sum "/fixture/$DUMP_NAME" > "$EVIDENCE/dump-sha256.txt"
docker exec "$FIXTURE_NAME" stat -c '%s' "/fixture/$DUMP_NAME" > "$EVIDENCE/dump-size.txt"
{
  docker exec "$FIXTURE_NAME" pg_dump --version
  docker exec "$FIXTURE_NAME" pg_restore --version
  docker exec "$FIXTURE_NAME" psql --version
} > "$EVIDENCE/client-versions.txt" 2>&1
DUMP_BYTES="$(cat "$EVIDENCE/dump-size.txt")"
if [[ ! "$DUMP_BYTES" =~ ^[0-9]+$ ]] || (( DUMP_BYTES < 1024 )); then
  echo "pg_dump produced an empty or invalid dump" >&2
  finalize 1 1
  exit $?
fi

echo "== pg_restore =="
set +e
docker exec "$FIXTURE_NAME" psql -h /fixture -U bba_fixture -d bba_fixture_admin \
  -v ON_ERROR_STOP=1 -c "CREATE DATABASE \"$DST_DB\"" > "$EVIDENCE/pg-restore-create.log" 2>&1
CREATE_EXIT=$?
if [[ "$CREATE_EXIT" == 0 ]]; then
  docker exec "$FIXTURE_NAME" pg_restore -h /fixture -U bba_fixture -d "$DST_DB" \
    --exit-on-error --single-transaction --verbose "/fixture/$DUMP_NAME" \
    > "$EVIDENCE/pg-restore.log" 2>&1
  RESTORE_EXIT=$?
fi
set -e
{
  echo "create_exit=$CREATE_EXIT"
  echo "restore_exit=$RESTORE_EXIT"
} >> "$EVIDENCE/exits.txt"
if [[ "$CREATE_EXIT" != 0 || "$RESTORE_EXIT" != 0 ]]; then
  if [[ "$CREATE_EXIT" != 0 ]]; then
    finalize "$CREATE_EXIT" 1
  else
    finalize "$RESTORE_EXIT" 1
  fi
  exit $?
fi

echo "== verify =="
set +e
docker run --rm --network none --user "$(id -u):$(id -g)" \
  -e HOME=/tmp/home -e GOPATH=/work/.cache/gopath -e GOMODCACHE=/work/.cache/gomod \
  -e GOCACHE=/work/.cache/gobuild -e GOPROXY=off -e GOSUMDB=off -e GOFLAGS=-mod=readonly \
  -e BBA_FIXTURE_ONLY=1 -e BBA_FIXTURE_DSN="$PG_DSN" \
  -e BBA_P018_STAGE=verify -e BBA_P018_RUN="$RUN_ID" -e BBA_P018_EVIDENCE_DIR=/evidence \
  -v "$ROOT:/work" -v "$SOCKET_VOLUME:/fixture" -v "$EVIDENCE:/evidence" \
  -w /work/backend golang:1.26 go run ./cmd/p018-restore 2>&1 | tee "$EVIDENCE/verify.log"
VERIFY_EXIT=${PIPESTATUS[0]}
set -e
echo "verify_exit=$VERIFY_EXIT" >> "$EVIDENCE/exits.txt"
if [[ "$VERIFY_EXIT" != 0 ]]; then
  finalize "$VERIFY_EXIT" 1
  exit $?
fi

echo "== independent psql proof =="
set +e
docker exec -i "$FIXTURE_NAME" psql -h /fixture -U bba_fixture -d "$DST_DB" \
  -v ON_ERROR_STOP=1 -A -t <<'SQL' > "$EVIDENCE/db-restore.jsonl" 2> "$EVIDENCE/db-restore.err.txt"
SELECT row_to_json(t) FROM (
  SELECT 'schema' AS kind, max(version) AS version FROM core.schema_version
) t;
SELECT row_to_json(t) FROM (
  SELECT 'projection' AS kind, t.id AS task_id, t.state,
    (SELECT count(*) FROM core.result r
      WHERE r.workspace_id=t.workspace_id AND r.task_id=t.id AND r.valid) AS confirmed_unique_count,
    (SELECT count(*) FROM core.goal_slot s
      WHERE s.workspace_id=t.workspace_id AND s.task_id=t.id AND s.state='reserved') AS reserved_target_count,
    (SELECT count(*) FROM core.execution e
      WHERE e.workspace_id=t.workspace_id AND e.task_id=t.id
        AND e.effect_certainty='confirmed_submitted') AS pending_external_count,
    (SELECT count(*) FROM core.execution e
      WHERE e.workspace_id=t.workspace_id AND e.task_id=t.id
        AND e.state IN ('DISPATCHING','UNKNOWN')) AS unknown_execution_count
  FROM core.task t
  ORDER BY t.id
) t;
SELECT row_to_json(t) FROM (
  SELECT 'execution' AS kind, e.task_id, e.operation_id, e.id AS execution_id,
    e.account_id, e.target_id, e.state AS execution_state,
    COALESCE(e.disposition,'') AS disposition, e.effect_certainty,
    COALESCE(g.state,'') AS gateway_state,
    COALESCE(g.disposition,'') AS gateway_disposition,
    COALESCE(g.effect_certainty,'') AS gateway_effect_certainty,
    COALESCE(p.state,'') AS permit_state,
    (SELECT count(*) FROM core.goal_slot s
      WHERE s.workspace_id=e.workspace_id AND s.task_id=e.task_id
        AND s.target_id=e.target_id AND s.state='reserved') AS reserved_slots,
    (SELECT count(*) FROM core.target_claim c
      WHERE c.workspace_id=e.workspace_id AND c.task_id=e.task_id
        AND c.target_id=e.target_id AND c.state='active') AS active_claims,
    (SELECT count(*) FROM core.account_action_lock l
      WHERE l.workspace_id=e.workspace_id AND l.execution_id=e.id AND l.held) AS held_send_locks,
    s.state AS subtask_state
  FROM core.execution e
  JOIN core.subtask s ON s.workspace_id=e.workspace_id AND s.id=e.subtask_id
  LEFT JOIN bridge.gateway_operation g
    ON g.workspace_id=e.workspace_id AND g.operation_id=e.operation_id
  LEFT JOIN core.dispatch_permit p
    ON p.workspace_id=e.workspace_id AND p.operation_id=e.operation_id
  WHERE e.workspace_id='ws_p018'
  ORDER BY e.operation_id
) t;
SELECT row_to_json(t) FROM (
  SELECT 'totals' AS kind,
    (SELECT count(*) FROM core.execution WHERE workspace_id='ws_p018') AS executions,
    (SELECT count(*) FROM bridge.gateway_operation WHERE workspace_id='ws_p018') AS gateway_operations,
    (SELECT count(*) FROM core.dispatch_permit WHERE workspace_id='ws_p018') AS dispatch_permits,
    (SELECT count(*) FROM core.retry WHERE workspace_id='ws_p018') AS retries,
    (SELECT count(*) FROM core.result WHERE workspace_id='ws_p018') AS results,
    (SELECT count(*) FROM core.goal_slot WHERE workspace_id='ws_p018' AND state='reserved') AS reserved_goal_slots,
    (SELECT count(*) FROM core.target_claim WHERE workspace_id='ws_p018' AND state='active') AS active_target_claims,
    (SELECT count(*) FROM core.account_action_lock WHERE workspace_id='ws_p018' AND held) AS held_action_locks,
    (SELECT count(*) FROM core.inbox WHERE workspace_id='ws_p018') AS inbox,
    (SELECT count(*) FROM core.task_event WHERE workspace_id='ws_p018') AS task_events,
    (SELECT count(*) FROM core.outbox WHERE workspace_id='ws_p018') AS outbox,
    (SELECT count(*) FROM bridge.outbox WHERE workspace_id='ws_p018') AS bridge_outbox,
    (SELECT count(*) FROM core.chat_send_permit WHERE workspace_id='ws_p018') AS chat_permits,
    (SELECT count(*) FROM core.chat_send_outcome WHERE workspace_id='ws_p018') AS chat_outcomes,
    (SELECT count(*) FROM core.model_request WHERE workspace_id='ws_p018') AS model_requests,
    (SELECT COALESCE(sum(consumed),0)::bigint FROM core.account_usage WHERE workspace_id='ws_p018') AS account_usage_consumed,
    (SELECT COALESCE(sum(reserved),0)::bigint FROM core.account_usage WHERE workspace_id='ws_p018') AS account_usage_reserved,
    (SELECT count(*) FROM core.task_event WHERE workspace_id='ws_p018' AND kind='manual_reconciliation') AS manual_reconciliations,
    (SELECT count(*) FROM bridge.outbox WHERE workspace_id='ws_p018' AND event_type='execution.observed') AS recovery_observations,
    (SELECT count(*) FROM core.outbox WHERE workspace_id='ws_p018' AND event_type='manual_reconciliation') AS reconcile_outbox
) t;
SELECT row_to_json(t) FROM (
  SELECT 'work_item' AS kind, wi.task_id, wi.id AS work_item_id, wi.state, wi.attempt_count,
    COALESCE(wi.execution_id,'') AS execution_id, wi.proposal_id, wi.application_id,
    (SELECT count(*) FROM core.work_item_attempt a
      WHERE a.workspace_id=wi.workspace_id AND a.work_item_id=wi.id) AS attempts
  FROM core.work_item wi
  WHERE wi.workspace_id='ws_p018'
  ORDER BY wi.task_id, wi.id
) t;
SELECT row_to_json(t) FROM (
  SELECT 'aux' AS kind,
    (SELECT count(*) FROM core.work_materialization WHERE workspace_id='ws_p018') AS work_materializations,
    (SELECT count(*) FROM core.task_graph_roots WHERE workspace_id='ws_p018') AS graph_roots,
    (SELECT count(*) FROM core.task_graph_anchors WHERE workspace_id='ws_p018') AS graph_anchors,
    (SELECT count(*) FROM core.graph_sync_nodes WHERE workspace_id='ws_p018') AS graph_nodes,
    (SELECT count(*) FROM core.graph_sync_outbox WHERE workspace_id='ws_p018') AS graph_outbox,
    (SELECT count(*) FROM core.graph_sync_checkpoints WHERE workspace_id='ws_p018') AS graph_checkpoints,
    (SELECT count(*) FROM core.graph_planning_snapshots WHERE workspace_id='ws_p018') AS graph_snapshots,
    (SELECT count(*) FROM core.graph_planning_snapshots WHERE workspace_id='ws_p018' AND state='open') AS graph_open_snapshots,
    (SELECT count(*) FROM core.graph_sync_outbox WHERE workspace_id='ws_p018' AND state='needs_reconcile'
      AND last_error_code IS NOT NULL) AS graph_needs_reconcile,
    (SELECT count(*) FROM core.graph_sync_outbox WHERE workspace_id='ws_p018' AND state='sent'
      AND operation='graph.pull') AS graph_sent_pulls,
    (SELECT count(*) FROM core.login_session WHERE workspace_id='ws_p018' AND state='ONLINE'
      AND projected AND binding_checked AND identity_self_id IS NOT NULL) AS login_online_projected,
    (SELECT count(*) FROM core.login_session_event WHERE workspace_id='ws_p018' AND kind='online') AS login_online_events
) t;
SQL
DB_PSQL_EXIT=$?
if [[ "$DB_PSQL_EXIT" == 0 ]]; then
  python3 - "$EVIDENCE/seed-evidence.json" "$EVIDENCE/verify-evidence.json" \
    "$EVIDENCE/db-restore.jsonl" > "$EVIDENCE/db-restore-proof.txt" 2>&1 <<'PY'
import json
import sys

seed_path, verify_path, db_path = sys.argv[1], sys.argv[2], sys.argv[3]
problems = []


def of(rows, kind):
    return [row for row in rows if row.get("kind") == kind]


try:
    seed = json.load(open(seed_path, encoding="utf-8"))
    verify = json.load(open(verify_path, encoding="utf-8"))
    rows = [json.loads(line) for line in open(db_path, encoding="utf-8") if line.strip()]
except Exception as exc:  # noqa: BLE001 - a malformed proof is a failed proof
    print(f"proof=fail reason=invalid_evidence error={type(exc).__name__}")
    sys.exit(1)

if verify.get("outcome") != "PASS":
    problems.append(f"verify_outcome={verify.get('outcome')}")
if verify.get("restored_matches_source") is not True:
    problems.append("restored_matches_source=false")
if verify.get("adapter_calls_after_restore") != 0:
    problems.append(f"adapter_calls={verify.get('adapter_calls_after_restore')}")
if verify.get("recovered_dispatching") != 1:
    problems.append(f"recovered_dispatching={verify.get('recovered_dispatching')}")
if verify.get("reconciled_timeouts") != 3:
    problems.append(f"reconciled_timeouts={verify.get('reconciled_timeouts')}")
if len(verify.get("checks", [])) < 8:
    problems.append(f"verify_checks={len(verify.get('checks', []))}")
if verify.get("work_items_reconciled") != 2:
    problems.append(f"work_items_reconciled={verify.get('work_items_reconciled')}")
if verify.get("work_items_second_pass") != 1:
    problems.append(f"work_items_second_pass={verify.get('work_items_second_pass')}")
if verify.get("work_items_claimable") != 0:
    problems.append(f"work_items_claimable={verify.get('work_items_claimable')}")
if verify.get("expired_work_item_claims") != 0:
    problems.append(f"expired_work_item_claims={verify.get('expired_work_item_claims')}")

seed_snapshot = seed.get("snapshot", {})
schema = of(rows, "schema")
expected_version = seed_snapshot.get("schema_version")
if len(schema) != 1 or not isinstance(expected_version, int) or schema[0].get("version") != expected_version:
    problems.append(f"schema={schema} expected={expected_version}")
if not isinstance(expected_version, int) or expected_version < 19:
    problems.append(f"schema_version={expected_version} want at least 19 (proposal work schema)")
for field in ("schema_fingerprint", "data_fingerprint"):
    if not seed_snapshot.get(field):
        problems.append(f"seed_{field}_missing")
    if not verify.get("before_recovery", {}).get(field):
        problems.append(f"restored_{field}_missing")

projections = {row.get("task_id"): row for row in of(rows, "projection")}
executions = {row.get("operation_id"): row for row in of(rows, "execution")}
totals_rows = of(rows, "totals")
if len(projections) != 3:
    problems.append(f"projection_rows={len(projections)}")
if len(executions) != 3:
    problems.append(f"execution_rows={len(executions)}")
if len(totals_rows) != 1:
    problems.append(f"totals_rows={len(totals_rows)}")

# The independent SQL read must equal the Go snapshot's work ledger exactly.
work_items = {row.get("task_id"): row for row in of(rows, "work_item")}
go_work_items = {row.get("task_id"): row for row in verify.get("after_recovery", {}).get("work_items", [])}
if len(work_items) != 3:
    problems.append(f"work_item_rows={len(work_items)}")
for task_id, row in work_items.items():
    db_row = {key: value for key, value in row.items() if key != "kind"}
    if go_work_items.get(task_id) != db_row:
        problems.append(f"psql_go_work_item_mismatch={task_id}")

aux_rows = of(rows, "aux")
if len(aux_rows) != 1:
    problems.append(f"aux_rows={len(aux_rows)}")
else:
    aux = {key: value for key, value in aux_rows[0].items() if key != "kind"}
    expected_aux = {
        "work_materializations": 3, "graph_roots": 2, "graph_anchors": 2,
        "graph_nodes": 2, "graph_outbox": 2, "graph_checkpoints": 2,
        "graph_snapshots": 1, "graph_open_snapshots": 1,
        "graph_needs_reconcile": 1, "graph_sent_pulls": 1,
        "login_online_projected": 1, "login_online_events": 1,
    }
    for field, want in expected_aux.items():
        if aux.get(field) != want:
            problems.append(f"aux.{field}={aux.get(field)} want={want}")

scenarios = {item["name"]: item for item in seed.get("scenarios", [])}
seed_totals = seed.get("snapshot", {}).get("totals", {})
seed_projections = {item["task_id"]: item for item in seed.get("snapshot", {}).get("projections", [])}
expected = {
    "pending": {
        "execution_state": "OBSERVED", "disposition": "pending",
        "effect_certainty": "confirmed_submitted", "gateway_state": "UNKNOWN",
        "permit_state": "consumed", "reserved_slots": 1, "active_claims": 1,
        "held_send_locks": 1, "subtask_state": "WAITING_EXTERNAL",
        "pending_external_count": 1, "unknown_execution_count": 0,
    },
    "unknown": {
        "execution_state": "UNKNOWN", "disposition": "unknown",
        "effect_certainty": "unknown", "gateway_state": "UNKNOWN",
        "permit_state": "consumed", "reserved_slots": 1, "active_claims": 1,
        "held_send_locks": 1, "pending_external_count": 0, "unknown_execution_count": 1,
    },
    "dispatching": {
        "execution_state": "UNKNOWN", "disposition": "unknown",
        "effect_certainty": "unknown", "gateway_state": "UNKNOWN",
        "permit_state": "issued", "reserved_slots": 1, "active_claims": 1,
        "held_send_locks": 1, "pending_external_count": 0, "unknown_execution_count": 1,
    },
}

for name, want in expected.items():
    scenario = scenarios.get(name)
    if not scenario:
        problems.append(f"missing_scenario={name}")
        continue
    execution = executions.get(scenario["operation_id"])
    if not execution:
        problems.append(f"missing_execution={name}")
        continue
    for field in ("execution_state", "disposition", "effect_certainty", "gateway_state",
                  "permit_state", "reserved_slots", "active_claims", "held_send_locks"):
        if execution.get(field) != want[field]:
            problems.append(f"{name}.{field}={execution.get(field)} want={want[field]}")
    if "subtask_state" in want and execution.get("subtask_state") != want["subtask_state"]:
        problems.append(f"{name}.subtask_state={execution.get('subtask_state')} want={want['subtask_state']}")
    projection = projections.get(scenario["task_id"])
    if not projection:
        problems.append(f"missing_projection={name}")
        continue
    for field in ("confirmed_unique_count",):
        if projection.get(field) != 0:
            problems.append(f"{name}.{field}={projection.get(field)} want=0")
    if projection.get("reserved_target_count") != 1:
        problems.append(f"{name}.reserved_target_count={projection.get('reserved_target_count')} want=1")
    for field in ("pending_external_count", "unknown_execution_count"):
        if projection.get(field) != want[field]:
            problems.append(f"{name}.{field}={projection.get(field)} want={want[field]}")
    if projection.get("state") in ("SUCCEEDED", "PARTIAL"):
        problems.append(f"{name}.state={projection.get('state')}")
    seed_projection = seed_projections.get(scenario["task_id"])
    if seed_projection and projection.get("state") != seed_projection.get("state"):
        problems.append(f"{name}.state_changed={seed_projection.get('state')}->{projection.get('state')}")

# The independent SQL totals must equal the Go snapshot exactly, except for the
# audit rows the recovery paths are allowed to add.
if totals_rows:
    db_totals = {key: value for key, value in totals_rows[0].items() if key != "kind"}
    after = verify.get("after_recovery", {}).get("totals")
    if after != db_totals:
        problems.append("psql_totals_do_not_match_go_snapshot")
    for field in ("executions", "gateway_operations", "dispatch_permits", "retries",
                  "results", "reserved_goal_slots", "active_target_claims", "held_action_locks",
                  "inbox", "chat_permits", "chat_outcomes", "model_requests",
                  "account_usage_consumed", "account_usage_reserved"):
        if db_totals.get(field) != seed_totals.get(field):
            problems.append(f"totals.{field}={db_totals.get(field)} seed={seed_totals.get(field)}")
    for field in ("task_events", "outbox", "bridge_outbox",
                  "manual_reconciliations", "recovery_observations", "reconcile_outbox"):
        if db_totals.get(field, 0) - seed_totals.get(field, 0) != {
            "task_events": 5, "outbox": 5, "bridge_outbox": 1,
            "manual_reconciliations": 3, "recovery_observations": 1, "reconcile_outbox": 3,
        }[field]:
            problems.append(f"audit.{field}={db_totals.get(field)} seed={seed_totals.get(field)}")
    if db_totals.get("executions") != 3 or db_totals.get("held_action_locks") != 3:
        problems.append(f"absolute_counts={db_totals}")
    if db_totals.get("retries") != 0 or db_totals.get("results") != 0:
        problems.append(f"new_work retries={db_totals.get('retries')} results={db_totals.get('results')}")

    # The psql rows and the Go-observed rows must agree field for field.
    go_rows = {row.get("operation_id"): row for row in verify.get("after_recovery", {}).get("unresolved", [])}
    for operation_id, row in executions.items():
        db_row = {key: value for key, value in row.items() if key != "kind"}
        if go_rows.get(operation_id) != db_row:
            problems.append(f"psql_go_row_mismatch={operation_id}")
    go_projections = {row.get("task_id"): row for row in verify.get("after_recovery", {}).get("projections", [])}
    for task_id, row in projections.items():
        db_row = {key: value for key, value in row.items() if key not in ("kind", "scenario")}
        expected_projection = {key: value for key, value in go_projections.get(task_id, {}).items() if key != "scenario"}
        if expected_projection != db_row:
            problems.append(f"psql_go_projection_mismatch={task_id}")

print(f"scenarios={len(scenarios)} executions={len(executions)} projections={len(projections)}")
if totals_rows:
    print(
        "zero_new_work executions={executions} gateway_operations={gateway_operations} "
        "dispatch_permits={dispatch_permits} retries={retries} results={results} "
        "adapter_calls_after_restore={verify}".format(verify=verify.get("adapter_calls_after_restore"), **totals_rows[0])
    )
    print(
        "holds reserved_goal_slots={reserved_goal_slots} active_target_claims={active_target_claims} "
        "held_action_locks={held_action_locks}".format(**totals_rows[0])
    )
if problems:
    for problem in problems:
        print(f"problem={problem}")
    print("proof=fail")
    sys.exit(1)
print("proof=pass")
PY
  DB_PROOF_EXIT=$?
else
  : > "$EVIDENCE/db-restore-proof.txt"
  DB_PROOF_EXIT=1
fi
set -e
echo "db_psql_exit=$DB_PSQL_EXIT" >> "$EVIDENCE/exits.txt"
echo "db_proof_exit=$DB_PROOF_EXIT" >> "$EVIDENCE/exits.txt"

if [[ "$DB_PSQL_EXIT" != 0 || "$DB_PROOF_EXIT" != 0 ]]; then
  finalize "$VERIFY_EXIT" "$DB_PROOF_EXIT"
  exit $?
fi

finalize 0 0
