#!/usr/bin/env bash
# P0-14 R22 four-account read-only engine smoke.
#
# One disposable PostgreSQL fixture (--network none, unix socket under /fixture)
# plus the approved four-port health probe followed by an isolated engine smoke.
# The runner can only dial loopback OneBot endpoints; the fixture database is
# the only database reachable from the Go container. The NapCat state file is
# mounted read-only. No message send, group join/application, QR refresh,
# login, plugin write, service lifecycle change or production write is
# performed by this script. The disposable container and volume are removed by
# an EXIT/INT/TERM trap.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
EVIDENCE="${P014_ENGINE_EVIDENCE_DIR:-${P014_EVIDENCE_DIR:-/tmp/bba-p0-14-engine-$(date -u +%Y%m%dT%H%M%SZ)}}"
STATE_FILE="${P014_STATE_FILE:-/home/sun/.local/state/bba-napcatd/state.json}"
BINDINGS="${P014_BINDINGS:-3210:443952741,3211:36374700,3212:3256395185,3213:454755768}"
PG_DSN='postgres://bba_fixture@/bba_fixture_admin?host=/fixture&sslmode=disable'

if [[ -e "$EVIDENCE" ]]; then
  echo "evidence directory already exists: $EVIDENCE" >&2
  exit 1
fi
if [[ ! -f "$STATE_FILE" ]]; then
  echo "napcatd state file is not a readable file: $STATE_FILE" >&2
  exit 1
fi
mkdir -m 700 "$EVIDENCE"

# Sorted fingerprint of the four-account runner, the engine/taskengine Go and
# SQL code, the generated task/heartbeat wire types, and both evidence scripts.
CODE_FILES=()
while IFS= read -r file; do
  CODE_FILES+=("$file")
done < <(
  cd "$ROOT"
  {
    find backend/cmd/p0-14-four -maxdepth 1 -type f -name '*.go' -print
    find backend/internal/taskengine -type f \( -name '*.go' -o -name '*.sql' \) -print
    printf '%s\n' backend/internal/contract/types.gen.go
    printf '%s\n' tools/p0-14-four.sh tools/p0-14-engine.sh
  } | LC_ALL=C sort -u
)
for required in backend/cmd/p0-14-four/main.go backend/cmd/p0-14-four/main_test.go \
  backend/internal/taskengine/migrations/0001_core.sql backend/internal/contract/types.gen.go \
  tools/p0-14-four.sh tools/p0-14-engine.sh; do
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

FIXTURE_NAME="bba-p014-engine-$(date -u +%Y%m%dT%H%M%SZ)-$$"
SOCKET_VOLUME="$FIXTURE_NAME-socket"
ENGINE_DB=""
# The engine smoke creates its own disposable database under /fixture and
# deliberately retains it for the independent psql proof below. Drop it before
# removing the socket volume/container, even when the run failed.
cleanup() {
  if [[ -n "$ENGINE_DB" ]]; then
    docker exec "$FIXTURE_NAME" psql -h /fixture -U bba_fixture -d bba_fixture_admin \
      -Atc "DROP DATABASE IF EXISTS \"$ENGINE_DB\" WITH (FORCE)" >/dev/null 2>&1 || true
  fi
  docker rm -f "$FIXTURE_NAME" >/dev/null 2>&1 || true
  docker volume rm "$SOCKET_VOLUME" >/dev/null 2>&1 || true
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

docker volume create "$SOCKET_VOLUME" >/dev/null
docker run --rm --network none -v "$SOCKET_VOLUME:/fixture" postgres:16-bookworm chmod 0777 /fixture
docker run -d --rm --network none --name "$FIXTURE_NAME" \
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
  echo "go_image=$(docker image inspect golang:1.26 --format '{{.Id}}')"
  echo "postgres_image=$(docker image inspect postgres:16-bookworm --format '{{.Id}}')"
  echo "code_sha=$CODE_SHA"
  echo "code_files=$EVIDENCE/code-files.txt"
  echo "engine_smoke=1"
  echo "env_P014_ENGINE_SMOKE=1"
  echo "env_BBA_FIXTURE_ONLY=1"
  echo "fixture_container=$FIXTURE_NAME"
  echo "fixture_socket_volume=$SOCKET_VOLUME"
  echo "fixture_dsn=$PG_DSN"
  echo "networks=host loopback reads; fixture network=none"
  echo "state_source=$STATE_FILE"
  echo "state_mount=read_only"
  echo "bindings=$BINDINGS"
  echo "mode=read_only"
  echo "engine_db=isolated_fixture"
  echo "writes=0"
  echo "evidence_dir=$EVIDENCE"
} > "$EVIDENCE/environment.txt"

set +e
docker run --rm --network host --user "$(id -u):$(id -g)" \
  -e HOME=/tmp/home -e GOPATH=/work/.cache/gopath -e GOMODCACHE=/work/.cache/gomod \
  -e GOCACHE=/work/.cache/gobuild -e GOPROXY=off -e GOSUMDB=off -e GOFLAGS=-mod=readonly \
  -e BBA_FIXTURE_ONLY=1 -e BBA_FIXTURE_DSN="$PG_DSN" \
  -e P014_EVIDENCE_DIR=/evidence -e P014_STATE_FILE=/config/state.json \
  -e P014_BINDINGS="$BINDINGS" -e P014_CODE_SHA="$CODE_SHA" \
  -e P014_ENGINE_SMOKE=1 \
  -v "$ROOT:/work" -v "$SOCKET_VOLUME:/fixture" \
  -v "$STATE_FILE:/config/state.json:ro" \
  -v "$EVIDENCE:/evidence" \
  -w /work/backend golang:1.26 go run ./cmd/p0-14-four 2>&1 | tee "$EVIDENCE/stdout.log"
RUN_EXIT=${PIPESTATUS[0]}
set -e

# runEngineSmoke migrates its own disposable database under /fixture and
# retains it so this harness can prove the resulting rows independently.
ENGINE_DB=""
ENGINE_DB_ERROR=""
if [[ -f "$EVIDENCE/engine-db-evidence.json" ]]; then
  ENGINE_DB="$(python3 - "$EVIDENCE/engine-db-evidence.json" 2>/dev/null <<'PY'
import json
import sys

try:
    value = json.load(open(sys.argv[1], encoding="utf-8")).get("fixture", {}).get("database", "")
except Exception:
    value = ""
print(value if isinstance(value, str) else "")
PY
)"
  if [[ ! "$ENGINE_DB" =~ ^[A-Za-z0-9_]+$ ]]; then
    ENGINE_DB_ERROR="invalid_or_missing_fixture_database"
    ENGINE_DB=""
  fi
else
  ENGINE_DB_ERROR="missing_engine_db_evidence"
fi
{
  echo "engine_database=${ENGINE_DB:-unknown}"
  echo "engine_database_error=${ENGINE_DB_ERROR:-none}"
  echo "engine_db_evidence=engine-db-evidence.json"
} >> "$EVIDENCE/environment.txt"

# Independent read-only DB proof: schema, four account/runtime mappings,
# heartbeat and credential coverage, dry-run task ownership, and zero
# execution/gateway/permit/result rows.
set +e
if [[ -n "$ENGINE_DB" ]]; then
docker exec -i "$FIXTURE_NAME" psql -h /fixture -U bba_fixture -d "$ENGINE_DB" \
  -v ON_ERROR_STOP=1 -A -t <<'SQL' > "$EVIDENCE/db-engine-psql.jsonl" 2> "$EVIDENCE/db-engine-psql.err.txt"
SELECT row_to_json(t) FROM (SELECT 'schema' AS kind, max(version) AS version FROM core.schema_version) t;
SELECT row_to_json(t) FROM (
  SELECT 'account' AS kind, a.id AS account_id, a.self_id, a.runtime_id, a.adapter_instance_id,
         a.connection_state, a.login_state, a.control_state, a.identity_status,
         a.request_join_verified, a.membership_verified,
         (SELECT count(*) FROM core.runtime_heartbeat h
           WHERE h.workspace_id=a.workspace_id AND h.account_id=a.id AND h.runtime_id=a.runtime_id) AS heartbeats,
         (SELECT count(*) FROM core.runtime_credentials c
           WHERE c.workspace_id=a.workspace_id AND c.account_id=a.id AND c.runtime_id=a.runtime_id AND c.active) AS active_credentials,
         (SELECT bool_or(c.membership_authority) FROM core.runtime_credentials c
           WHERE c.workspace_id=a.workspace_id AND c.account_id=a.id AND c.runtime_id=a.runtime_id) AS membership_authority,
         (SELECT count(*) FROM core.task_account ta
           WHERE ta.workspace_id=a.workspace_id AND ta.account_id=a.id) AS task_accounts
  FROM core.account a ORDER BY a.self_id
) t;
SELECT row_to_json(t) FROM (
  SELECT 'credential' AS kind, c.runtime_id, c.account_id, c.active, c.membership_authority
  FROM core.runtime_credentials c ORDER BY c.account_id, c.runtime_id
) t;
SELECT row_to_json(t) FROM (
  SELECT 'heartbeat' AS kind, h.runtime_id, h.account_id, count(*) AS heartbeats,
         max(h.sequence) AS max_sequence, min(h.received_at) AS first_received_at,
         max(h.received_at) AS last_received_at
  FROM core.runtime_heartbeat h GROUP BY h.runtime_id, h.account_id ORDER BY h.account_id
) t;
SELECT row_to_json(t) FROM (
  SELECT 'task' AS kind, t.id AS task_id, t.mode, t.state, t.goal_count,
         t.principal_id, t.approval_id, ta.account_id, a.self_id, a.runtime_id
  FROM core.task t
  LEFT JOIN core.task_account ta
    ON ta.workspace_id=t.workspace_id AND ta.task_id=t.id
  LEFT JOIN core.account a
    ON a.workspace_id=ta.workspace_id AND a.id=ta.account_id
  ORDER BY t.id
) t;
SELECT row_to_json(t) FROM (
  SELECT 'counts' AS kind,
         (SELECT count(*) FROM core.account) AS accounts,
         (SELECT count(*) FROM core.runtime_credentials) AS credentials,
         (SELECT count(*) FROM core.task) AS tasks,
         (SELECT count(*) FROM core.task_account) AS task_accounts,
         (SELECT count(*) FROM core.idempotency) AS task_idempotency,
         (SELECT count(*) FROM core.runtime_heartbeat) AS heartbeats,
         (SELECT count(*) FROM core.heartbeat_idempotency) AS heartbeat_idempotency,
         (SELECT count(*) FROM core.goal_slot) AS goal_slots,
         (SELECT count(*) FROM core.goal_slot WHERE state='free') AS free_goal_slots,
         (SELECT count(*) FROM core.task_event) AS task_events,
         (SELECT count(*) FROM core.outbox) AS outbox,
         (SELECT count(*) FROM core.execution) AS executions,
         (SELECT count(*) FROM bridge.gateway_operation) AS gateway_operations,
         (SELECT count(*) FROM core.dispatch_permit) AS dispatch_permits,
         (SELECT count(*) FROM core.result) AS results,
         (SELECT count(*) FROM core.subtask) AS subtasks,
         (SELECT count(*) FROM core.account_usage) AS account_usage,
         (SELECT count(*) FROM core.inbox) AS inbox,
         (SELECT count(*) FROM core.membership) AS memberships,
         (SELECT count(*) FROM core.goal_slot WHERE state IN ('reserved','fulfilled')) AS held_goal_slots,
         (SELECT count(*) FROM core.target_claim) AS target_claims,
         (SELECT count(*) FROM core.account_action_lock) AS action_locks,
         (SELECT count(*) FROM core.approval) AS approvals,
         (SELECT count(*) FROM core.target) AS targets,
         (SELECT count(*) FROM core.task_target) AS task_targets,
         (SELECT count(*) FROM core.baseline_account) AS baseline_accounts,
         (SELECT count(*) FROM core.baseline) AS baselines,
         (SELECT count(*) FROM core.retry) AS retries,
         (SELECT count(*) FROM core.observation_conflict) AS observation_conflicts,
         (SELECT count(*) FROM bridge.outbox) AS bridge_outbox
) t;
SQL
DB_PSQL_EXIT=$?
else
  : > "$EVIDENCE/db-engine-psql.jsonl"
  printf 'proof=fail reason=%s\n' "$ENGINE_DB_ERROR" > "$EVIDENCE/db-engine-proof.txt"
  DB_PSQL_EXIT=1
fi
set -e

set +e
if [[ -n "$ENGINE_DB" ]]; then
python3 - "$EVIDENCE/db-engine-psql.jsonl" "$EVIDENCE/engine-db-evidence.json" > "$EVIDENCE/db-engine-proof.txt" 2>&1 <<'PY'
import json
import sys

path = sys.argv[1]
rows = []
try:
    with open(path, encoding="utf-8") as handle:
        for line in handle:
            line = line.strip()
            if line:
                rows.append(json.loads(line))
except Exception as exc:  # noqa: BLE001 - evidence must record malformed rows
    print(f"proof=fail reason=invalid_db_evidence error={type(exc).__name__}")
    sys.exit(1)


def of(kind):
    return [row for row in rows if row.get("kind") == kind]


problems = []
try:
    with open(sys.argv[2], encoding="utf-8") as handle:
        engine_evidence = json.load(handle)
except Exception as exc:  # noqa: BLE001 - evidence must record malformed outcome
    problems.append(f"engine_evidence_invalid={type(exc).__name__}")
else:
    if engine_evidence.get("outcome") != "PASS":
        problems.append(f"engine_outcome={engine_evidence.get('outcome')}")

schemas = [row.get("version") for row in of("schema")]
if schemas != [12]:
    problems.append(f"schema_versions={schemas}")

accounts = of("account")
if len(accounts) != 4:
    problems.append(f"account_count={len(accounts)}")
for field in ("self_id", "runtime_id", "adapter_instance_id"):
    values = [row.get(field) for row in accounts]
    if any(not value for value in values) or len(set(values)) != 4:
        problems.append(f"{field}_distinct={len(set(values))}")

account_ids = {row.get("account_id") for row in accounts}
heartbeats = {row.get("account_id"): row.get("heartbeats", 0) for row in of("heartbeat")}
credentials = {row.get("account_id"): row for row in of("credential")}
if len(credentials) != 4 or len(of("credential")) != 4:
    problems.append(f"credential_rows={len(of('credential'))}")
if len(heartbeats) != 4 or len(of("heartbeat")) != 4:
    problems.append(f"heartbeat_rows={len(of('heartbeat'))}")
for account in accounts:
    account_id = account.get("account_id")
    if account.get("identity_status") != "verified":
        problems.append(f"identity_not_verified account={account_id} status={account.get('identity_status')}")
    if (account.get("connection_state"), account.get("login_state"), account.get("control_state")) != ("online", "logged_in", "paused"):
        problems.append(
            f"heartbeat_projection account={account_id} "
            f"connection={account.get('connection_state')} login={account.get('login_state')} control={account.get('control_state')}"
        )
    if account.get("heartbeats") != 1 or heartbeats.get(account_id) != 1:
        problems.append(f"heartbeat_count account={account_id} count={account.get('heartbeats')}")
    if account.get("active_credentials") != 1 or not credentials.get(account_id, {}).get("active"):
        problems.append(f"credential_count account={account_id} count={account.get('active_credentials')}")
    if account.get("request_join_verified") or account.get("membership_verified"):
        problems.append(f"write_capability_verified account={account_id}")
    credential = credentials.get(account_id, {})
    if credential.get("runtime_id") != account.get("runtime_id") or credential.get("membership_authority"):
        problems.append(f"credential_scope_or_authority account={account_id}")
    if account.get("task_accounts", 0) != 1:
        problems.append(f"task_account_count account={account_id} count={account.get('task_accounts')}")

tasks = of("task")
if len(tasks) != 4:
    problems.append(f"task_count={len(tasks)}")
seen_tasks = set()
seen_accounts = set()
for task in tasks:
    task_id = task.get("task_id")
    account_id = task.get("account_id")
    if task_id in seen_tasks:
        problems.append(f"duplicate_task={task_id}")
    seen_tasks.add(task_id)
    if account_id is None:
        problems.append(f"task_without_account={task_id}")
        continue
    if account_id not in account_ids:
        problems.append(f"task_account_unknown={task_id}")
    if account_id in seen_accounts:
        problems.append(f"account_has_duplicate_task={account_id}")
    seen_accounts.add(account_id)
    if task.get("principal_id") != "principal_p014_engine" or task.get("approval_id") is not None:
        problems.append(f"task_principal_or_approval={task_id}")
    if task.get("goal_count") != 1:
        problems.append(f"task_goal_count={task_id}:{task.get('goal_count')}")
    if task.get("mode") != "dry_run":
        problems.append(f"task_mode={task_id}:{task.get('mode')}")
    if task.get("state") != "DRAFT":
        problems.append(f"task_state={task_id}:{task.get('state')}")
    matching = [row for row in accounts if row.get("account_id") == account_id]
    if not matching or matching[0].get("self_id") != task.get("self_id") or matching[0].get("runtime_id") != task.get("runtime_id"):
        problems.append(f"task_runtime_mismatch={task_id}")

counts = of("counts")
if len(counts) != 1:
    problems.append(f"count_row_count={len(counts)}")
else:
    for field, want in {
        "accounts": 4, "credentials": 4, "tasks": 4, "task_accounts": 4,
        "task_idempotency": 4, "heartbeats": 4, "heartbeat_idempotency": 4,
        "goal_slots": 4, "free_goal_slots": 4, "task_events": 4, "outbox": 4,
    }.items():
        if counts[0].get(field) != want:
            problems.append(f"{field}={counts[0].get(field)} want={want}")
    for field in ("executions", "gateway_operations", "dispatch_permits", "results",
                  "subtasks", "account_usage", "inbox", "memberships",
                  "held_goal_slots", "target_claims", "action_locks",
                  "approvals", "targets", "task_targets", "baseline_accounts",
                  "baselines", "retries", "observation_conflicts", "bridge_outbox"):
        if counts[0].get(field) != 0:
            problems.append(f"{field}={counts[0].get(field)}")

print(f"accounts={len(accounts)} distinct_self_ids={len({r.get('self_id') for r in accounts})}")
print(f"distinct_runtimes={len({r.get('runtime_id') for r in accounts})} distinct_adapters={len({r.get('adapter_instance_id') for r in accounts})}")
print(f"tasks={len(tasks)} heartbeats={sum(heartbeats.values())} credentials={len(of('credential'))}")
if counts:
    count = counts[0]
    print(
        "zero_rows executions={executions} gateway_operations={gateway_operations} "
        "dispatch_permits={dispatch_permits} results={results}".format(**count)
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
  DB_PROOF_EXIT=1
fi
set -e
if [[ "$DB_PSQL_EXIT" != 0 ]]; then
  DB_EXIT="$DB_PSQL_EXIT"
else
  DB_EXIT="$DB_PROOF_EXIT"
fi

{
  echo "run_exit=$RUN_EXIT"
  echo "db_psql_exit=$DB_PSQL_EXIT"
  echo "db_proof_exit=$DB_PROOF_EXIT"
  echo "db_exit=$DB_EXIT"
} > "$EVIDENCE/exits.txt"

# Scan every evidence file for the state file's credential values and for
# credential-shaped markers. Values are never printed.
set +e
python3 - "$STATE_FILE" "$EVIDENCE" > "$EVIDENCE/sensitive-scan.txt" <<'PY'
import json
import pathlib
import re
import sys

state_path, evidence_dir = sys.argv[1], pathlib.Path(sys.argv[2])
try:
    state = json.load(open(state_path, encoding="utf-8"))
except Exception as exc:  # noqa: BLE001 - a scan failure is a failed run
    print(f"scan=fail reason=state_unreadable error={type(exc).__name__}")
    sys.exit(1)

key_pattern = re.compile(r"(token|secret|password|passwd|credential|api[_-]?key)", re.IGNORECASE)
secrets = []


def walk(node, path=()):
    if isinstance(node, dict):
        for key, value in node.items():
            walk(value, path + (str(key),))
    elif isinstance(node, list):
        for index, value in enumerate(node):
            walk(value, path + (str(index),))
    elif isinstance(node, str) and node and path and key_pattern.search(path[-1]):
        # Ignore trivially short values that would match ordinary evidence
        # prose; real OneBot/WebUI tokens are long opaque strings.
        if len(node.encode()) >= 8:
            secrets.append((".".join(path), node.encode()))


walk(state)
unique = {}
for field, value in secrets:
    unique.setdefault(value, field)
secrets = [(field, value) for value, field in unique.items()]

files = [path for path in evidence_dir.rglob("*") if path.is_file()]
skip = {"sensitive-scan.txt", "evidence-sha256.txt", "evidence-verify.json", "manifest-verify.txt"}
bad = []
for path in sorted(files):
    if path.name in skip:
        continue
    data = path.read_bytes()
    for field, value in secrets:
        if value in data:
            bad.append(f"FOUND state_field={field} file={path.name}")
    for marker in (b"Authorization", b"Bearer ", b'"onebot_token"', b'"webui_token"',
                   b'"access_token"', b'"refresh_token"', b'"client_secret"',
                   b'"webui_secret"', b'"password"'):
        if marker in data:
            bad.append(f"FOUND marker={marker.decode()} file={path.name}")

for field, _value in secrets:
    print(f"state_field={field} checked=yes")
if bad:
    for line in sorted(set(bad)):
        print(line)
    print("scan=fail")
    sys.exit(1)
print("scan=pass")
PY
SCAN_EXIT=$?
set -e

{
  echo "scan_exit=$SCAN_EXIT"
} >> "$EVIDENCE/exits.txt"

(
  cd "$EVIDENCE"
  find . -type f ! -name evidence-sha256.txt ! -name evidence-verify.json ! -name manifest-verify.txt -print0 \
    | LC_ALL=C sort -z | xargs -0 sha256sum
) > "$EVIDENCE/evidence-sha256.txt"

set +e
(
  cd "$EVIDENCE"
  sha256sum -c evidence-sha256.txt
) > "$EVIDENCE/manifest-verify.txt" 2>&1
HASH_EXIT=$?
set -e
printf 'hash_exit=%s\n' "$HASH_EXIT" >> "$EVIDENCE/manifest-verify.txt"

python3 - "$EVIDENCE" "$RUN_EXIT" "$SCAN_EXIT" "$DB_EXIT" "$HASH_EXIT" <<'PY'
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
    "scan_exit": int(sys.argv[3]),
    "db_exit": int(sys.argv[4]),
    "hash_exit": int(sys.argv[5]),
    "utc_verified": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
}
(evidence / "evidence-verify.json").write_text(json.dumps(result, indent=2) + "\n", encoding="utf-8")
PY

echo "P0-14 engine smoke exit=$RUN_EXIT scan_exit=$SCAN_EXIT db_exit=$DB_EXIT hash_exit=$HASH_EXIT evidence=$EVIDENCE code_sha=$CODE_SHA"
if [[ "$RUN_EXIT" != 0 ]]; then exit "$RUN_EXIT"; fi
if [[ "$SCAN_EXIT" != 0 ]]; then exit "$SCAN_EXIT"; fi
if [[ "$HASH_EXIT" != 0 ]]; then exit "$HASH_EXIT"; fi
exit "$DB_EXIT"
