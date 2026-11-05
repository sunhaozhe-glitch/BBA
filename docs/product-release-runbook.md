# Product release runbook (OPS-04)

Scope: deploy, backup, schema migration, drain, single-writer ownership, restart,
rollback and monitoring for the BBA Task Engine. Every command below is the real
binary or script in this repository. Nothing here grants live QQ, login, ARTEX,
model or production-database authorization: those remain fail-closed
prerequisites (`release/evidence.json`, see [Live authorization](#10-live-authorization-prerequisites)).

Status of this document: engineering procedure plus an isolated rehearsal that
`tools/product-restore.sh` executes on demand. The rehearsal record at the end is
meaningful only when it names an actual run directory produced by that command;
it is never prefilled with invented results.

> 2026-11-05 后续运行约束：本文保留后半部历史演练记录，旧 schema 数值、日志、实例和 fixture 结果不作为当前部署证据。设计适用[收口 ADR 补充](adr/bba-final-closeout-2026-10-31.md#后续适用规则2026-11-05)，当前授权/未决事项见[任务交接](astra-omp-continuation-prompt.md#2026-11-05-本轮文档统一交接)。下列程序是待授权时的要求，不表示本轮执行。

## 1. Invariants

1. **PostgreSQL is the execution truth.** No component may infer a send, a
   success, or a migration from a log line.
2. **Startup never migrates.** `taskengine-server` reads `core.schema_version`
   and refuses to serve below its own minimum (`validateServerSchema`). Schema
   changes happen only when an operator runs `taskengine-migrate`.
3. **Writes are explicit.** The only schema-writing command is
   `taskengine-migrate --upgrade --expected-version N`. The default mode
   (no flags) still initializes exactly one empty database and refuses anything
   else.
4. **One effective writer per account and action family.** Inventory every
   task, Character/chat, plugin and direct adapter writer. Task leases, planning
   coordination and writer ownership are different controls; database fences
   alone do not prove an old process has stopped. Task Reserve and independent
   Chat Permit keep their separate authorization and recovery protocols.
5. **UNKNOWN is not success and is never retried automatically.** A pending or
   unknown external effect keeps its goal slot, target claim, send lock and
   permit; restart only observes or quarantines it.
6. **Fixture only unless explicitly authorized.** `tools/product-restore.sh`
   refuses an inherited Task Engine DSN, every drill container runs with
   `--network none`, and no script defaults to live.

## 2. Binaries

Build from `backend/` (Go 1.26, offline module cache as used by
`tools/dev-stack.sh`):

```bash
cd backend
go build -trimpath -o ../build/release/taskengine-migrate ./cmd/taskengine-migrate
go build -trimpath -o ../build/release/taskengine-server  ./cmd/taskengine-server
go build -trimpath -o ../build/release/task-bridge        ./cmd/task-bridge
go build -trimpath -o ../build/release/bba-operator       ./cmd/bba-operator
```

Runtime configuration is read from the environment and protected files, never
from command-line arguments:

| Process | Required inputs |
| --- | --- |
| `taskengine-migrate` | `BBA_TASKENGINE_DSN`; no positional arguments; `-dsn` is rejected |
| `taskengine-server` | `BBA_TASKENGINE_DSN`, `-resolver-file` (mode 0600), `-origin`, `-listen` (loopback only) |
| `task-bridge` | `BBA_TASKENGINE_DSN`, `-config` (mode 0600 runtime/credential file), `-origin`, `-listen` (loopback) |
| `bba-operator` | `-url`, `-token-file` (mode 0600) |

Read the migration registry, `migrate_head.go` / `SchemaHead()`, actual binary
identity and authorized database readback for this release. SQL filename numbers
are not schema versions. The historical schema-21 rehearsal below remains a
historical result. Set `CURRENT` to the observed database version and
`SCHEMA_HEAD` to the verified target binary head; neither is a copied constant:


```bash
BBA_TASKENGINE_DSN="$DSN" ./build/release/taskengine-migrate \
  --check --expected-version "$CURRENT" --expect-head "$SCHEMA_HEAD"
# Compare schema/head/upgrade_required with this release record.
```

## 3. Deploy

```bash
# 1. Pre-flight: the database is at the version this build expects.
BBA_TASKENGINE_DSN="$DSN" ./build/release/taskengine-migrate \
  --check --expected-version "$SCHEMA_HEAD" --expect-head "$SCHEMA_HEAD"

# 2. Template only: verify transport wiring and sole-writer handover first.
BBA_TASKENGINE_DSN="$DSN" ./build/release/taskengine-server \
  -listen 127.0.0.1:18421 -origin "$OPERATOR_ORIGIN" \
  -resolver-file /etc/bba/resolver.json \
  -worker -worker-id prod-worker-1 -worker-poll 15s -worker-batch 20

BBA_TASKENGINE_DSN="$DSN" ./build/release/task-bridge \
  -listen 127.0.0.1:18422 -origin http://127.0.0.1:18422 \
  -config /etc/bba/bridge.json
```

`-worker-recover` is a one-shot crash-recovery pass on the first worker tick.
Use it only for the first start after an unclean stop; it replays
`RecoverDispatching`, permit expiry and timeout reconciliation without creating
new external attempts. Do not leave it on as a periodic substitute for draining.

Startup logs are diagnostics, not authoritative schema readback. The server's
`validateServerSchema` is a minimum-version gate; Bridge's
`validateGatewaySchema` requires exact `SchemaHead()`. A historical fixed schema
number in a Bridge log proves neither. Verify both gates and the database;
passing the minimum does not prove new pages have all required tables. On a
mismatch, stop and check the intended build/database/migration, rather than
pointing at another database.

The main server currently assembles `WorkerFixtureTransport` in `-worker` mode;
this flag and the existence of a bridge binary do not connect continuous work to
live QQ. The commands above are not a production writer-wiring acceptance. Any
live path requires its actual transport, identity and final dispatch gates to
be separately verified under authorization.

Live deployment additionally requires the authorizations in section 10. A
fixture deployment may use `-development-operations`; a live deployment must not.

## 4. Backup

Backups are logical dumps of the whole database, including `core`, `bridge` and
the schema-version row.

```bash
STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
pg_dump --format=custom --no-sync --file="/secure/bba-$STAMP.dump" "$DSN"
pg_restore --list "/secure/bba-$STAMP.dump" > "/secure/bba-$STAMP.list"
sha256sum "/secure/bba-$STAMP.dump" > "/secure/bba-$STAMP.sha256"
psql "$DSN" -Atc 'select max(version) from core.schema_version' > "/secure/bba-$STAMP.schema"
```

Rules:

- A backup is not usable until `pg_restore --list` succeeds and the recorded
  schema version matches the runbook step that produced it.
- Keep the dump, list, checksum and schema version together; the release record
  references the checksum, not a path.
- Do not back up with `--data-only`; restore-to-new-database needs the schema.
- Never overwrite a previous dump. Apply the approved retention, privacy and
  evidence-expiry policy; release references do not grant indefinite retention.
  Record any controlled expiry/deletion without silently rewriting history.
- PostgreSQL dumps do not cover the protected model/Agent/Skill/search/proxy
  registry, Character/Collector state or development Controller SQLite. Inventory
  each needed domain, secret-safe backup and compatible isolated restore; a DB
  rehearsal alone cannot establish whole-product recovery.

## 5. Schema migration (existing database)

`taskengine-migrate` semantics:

```text
default                                  init-only: brand-new database, else refuse
--check   --expected-version N [--expect-head H]   read-only precondition report
--upgrade --expected-version N [--expect-head H]   CAS upgrade to the registered head
```

- `N` must equal the version currently recorded in `core.schema_version`.
- The destination is always the binary's registered head (`--expect-head` pins
  that build identity before the first write).
- Exit codes: `0` success/match, `1` refusal/failure, `2` flag misuse.
- Output is one key=value line on stdout:
  `initialized schema=H`, `upgraded from=N to=H expected_version=N head=H`,
  `already_at_head schema=H expected_version=N head=H`, or
  `check schema=V|none expected_version=N head=H upgrade_required=bool result=…`.

Race protection (explicit): the version compare runs inside an operator lease
(`pg_try_advisory_lock(740412)`) held on one dedicated connection from the
existence/version check through `Store.Migrate` and the final
`version == SchemaHead()` verification. A concurrent operator upgrade fails fast
with `upgrade lease 740412 is held` and never reads a stale version. The kernel
transaction itself holds `pg_advisory_xact_lock(740411)` and applies every
pending migration additively in that one transaction, so commit implies a
complete head. Every refusal (empty database, wrong `N`, newer-than-build
database, unexpected `--expect-head`) happens before the first DDL; a refusal
can be proven inert by comparing the catalog before and after.

Procedure:

```bash
# 1. Pre-flight on the live database (read-only).
BBA_TASKENGINE_DSN="$DSN" ./build/release/taskengine-migrate \
  --check --expected-version "$CURRENT" --expect-head "$SCHEMA_HEAD"

# 2. Take a fresh backup as in section 4 and record its checksum.

# 3. Drain (section 6). Confirm zero in-flight writers.

# 4. Upgrade exactly that version.
BBA_TASKENGINE_DSN="$DSN" ./build/release/taskengine-migrate \
  --upgrade --expected-version "$CURRENT" --expect-head "$SCHEMA_HEAD"
# Record actual from/to/expected_version/head and exit code.

# 5. Post-check and redeploy.
BBA_TASKENGINE_DSN="$DSN" ./build/release/taskengine-migrate \
  --check --expected-version "$SCHEMA_HEAD" --expect-head "$SCHEMA_HEAD"
```

- Rerunning after a successful upgrade with the old `N` is a deliberate
  refusal when the version no longer matches `N`, not a silent no-op.
  Re-run `--check` and use the observed version.
- `already_at_head` is exit 0 and writes nothing.
- Migrations are append-only; never rewrite or renumber released migrations or
  migrate at startup. Verify supported-version upgrade, current startup gates,
  key queries, isolation and restore with the target build. The historical P0
  single-environment compatibility waiver is not a later release waiver.
- `0017_operator_notification_scope.sql` changes uniqueness and three writer
  conflict targets. Stop old writers before explicit migration and switch to
  matching binaries; mixed old/new SQL is unsupported. Migration does not
  reconstruct historical missed notifications.
- There is no down-migration command; see section 9.

## 6. Drain

Order matters: stop the producer of new external work first, the sender last.
Inventory and stop new work from planning/automation and independent Character/
chat writers too; task execution queries alone cannot establish chat quiescence.
Check the corresponding Chat Permit/outcome records and any active adapter call.
Global stop blocks new dispatch and further automatic advancement; it cannot
recall an in-flight or UNKNOWN action. Test its concurrency with final dispatch.
Late results may be archived/reconciled but must not restart stopped planning.

```bash
# 1. Stop the server (worker stops on SIGTERM; HTTP gets a 10s graceful shutdown;
#    the worker releases its task lease on exit).
kill -TERM "$SERVER_PID"; wait_for_exit "$SERVER_PID"

# 2. Prove quiescence. DISPATCHING/UNKNOWN are unresolved effects, not drain leaks.
psql "$DSN" -c "select state,count(*) from core.execution
                 where state in ('CREATED','ACCEPTED','DISPATCHING','UNKNOWN') group by 1 order by 1"
psql "$DSN" -c "select count(*) as claimed_work from core.work_item where state='CLAIMED'"
psql "$DSN" -c "select count(*) as held_send_locks from core.account_action_lock where held"
psql "$DSN" -c "select state,count(*) from bridge.outbox group by 1 order by 1"

# 3. Only now stop the bridge.
kill -TERM "$BRIDGE_PID"; wait_for_exit "$BRIDGE_PID"
```

- A `CLAIMED` work item with `execution_id IS NULL` is effect-free; on restart
  the sweep returns it to `READY` after the 30s lease. A `CLAIMED` item with an
  execution, or a `WAITING_RESULT` item, is an unresolved attempt and must not be
  reset.
- Never delete rows, reset `UNKNOWN` to `READY`, or expire an unresolved permit
  to make a drain look clean. `ExpirePermits` intentionally retires nothing that
  may already have been sent.
- If the bridge must be restarted before the server, expect the worker's
  dispatch calls to fail closed; the effect is recorded as `DISPATCHING` or
  `UNKNOWN` and is never retried automatically.

## 7. Single-writer ownership

A release must prove one effective writer per account/action family, including
plugins and independent chat paths. The deployment shape alone does not prove
this. The following task/graph controls cover their own participating paths;
they do not prove cross-process old-owner shutdown or every writer's coverage:

- **Worker lease:** a PostgreSQL session advisory lock held per task for the
  duration of one step. A concurrent worker skips the task; a crashed worker's
  session ends and the lock disappears.
- **Task/version/owner fences:** `Reserve`, `AcceptOperation`, `IssuePermit` and
  `Dispatch` re-check task version, runtime `owner_epoch`, dispatch permit and
  `core.account_action_lock`.
- **Proposal work fence:** `core.work_item.lease_epoch` rejects a stale claim,
  and `ReserveWorkItem` re-validates task mode, authorization revision, task
  state and permit before producing any execution.
- **Graph outbox fence:** `core.graph_sync_outbox` claims only `pending` rows;
  `sending`/`needs_reconcile` rows are never blindly retried.

Inspection:

```bash
psql "$DSN" -c "select workspace_id,runtime_id,account_id,owner_epoch,active
                from core.runtime_credentials order by 1,2,3"
psql "$DSN" -c "select operation_id,state from bridge.gateway_operation
                where state in ('DISPATCHING','UNKNOWN') order by 1"
psql "$DSN" -c "select state,count(*) from core.work_item group by 1 order by 1"
```

A second bridge configured with the same runtime but a different
`owner_epoch` cannot advance an operation created under the earlier epoch; fix
the configuration rather than "retiring" the rows.

## 8. Restart and recovery verification

Restart with the same stable `-worker-id`. After restart:

```bash
# No new attempt/result may appear for unresolved work.
psql "$DSN" -c "select state,count(*) from core.execution
                where state in ('DISPATCHING','UNKNOWN','OBSERVED') group by 1 order by 1"
psql "$DSN" -c "select state,count(*) from core.work_item group by 1 order by 1"
psql "$DSN" -c "select count(*) from core.retry"
psql "$DSN" -c "select count(*) from core.result where valid"
```

Expected: adapter calls zero, no new `core.retry` or `core.result` rows for the
unresolved attempts, `DISPATCHING` quarantined to `UNKNOWN`, unresolved work
items `WAITING_RESULT`/`UNKNOWN`, and every goal slot/target claim/send lock
still held.

The isolated rehearsal for exactly this path is section 11.

## 9. Rollback

There is **no `--rollback` flag and no down-migration**. The supported rollback
is application rollback plus restore-to-new-database:

```bash
# A. Application rollback: require tested compatibility for the exact old
#    binary, current schema, writer SQL and all relevant gates. A passing
#    server minimum or additive DDL alone does not prove compatibility.
#    Never downgrade the database in place.

# B. Data rollback: restore the last verified dump into a NEW database and
#    repoint BBA_TASKENGINE_DSN.
RESTORE_DB="bba_restore_$(date -u +%Y%m%dT%H%M%SZ)"
createdb -T template0 "$RESTORE_DB"
pg_restore --exit-on-error --single-transaction \
  --dbname="$RESTORE_DB" /secure/bba-<stamp>.dump
# then run the same --check on the restored database, start server+bridge against
# it, and only then retire the old database after the retention window.
```

Agent/Draft request identity and replay protocol changes also require stopping
old writers even without a new migration. Reverting an old binary can restore
the original defect or treat a new request as a new write; do not claim lossless
rollback. See [contract compatibility](README.md#契约与双对话兼容约束2026-11-05-承接).

Consequences to state in the incident record:

- Data written after the dump is lost; any external effect in that window must
  be treated as UNKNOWN and reconciled from the QQ/NapCat side, never resent.
- Schema is never rolled back in place. If the previous binary requires an older
  schema than the dump contains, that is an incompatible rollback: keep the new
  binary and fix forward.
- The rehearsal in section 11 proves the backup/restore half (schema and all
  table data identical, unresolved work preserved, zero resends). It does not
  prove a production restore; that needs the live authorization in section 10.

## 10. Live authorization prerequisites

All live actions are fail-closed. `tools/product-acceptance.sh release-check`
returns `RELEASE_BLOCKED` (nonzero) until `release/evidence.json` contains an
`ACCEPTED` record with `accepted_at`, `reviewer`, `scope.environment` (not
`fixture`), evidence path and sha256 for each of:

| Prerequisite | Must name |
| --- | --- |
| `qq_controlled_actions` | authorization reference, accounts, allowed actions, time window, stop conditions |
| `account_login_identity` | same, plus the account/instance binding to verify on readback |
| `artex_dual_graph_live` | endpoint, verified version/commit, credential owner, window, stop conditions |
| `model_holdout_review` | frozen holdout/rubric provenance and human scores |
| `production_backup_restore` | dump checksum, restore target, restored schema, verification output |
| `migration_rehearsal` | from/to schema, backup checksum, operator, soak window |
| `rollback_rehearsal` | restore-to-new-database target and repoint evidence |
| `sole_writer_ownership` | the process/credential inventory and the fence queries above |
| `runtime_configuration` | resolver/bridge/model config hashes and file modes |
| `monitoring_alerts` | alert rules and a fired test alert reference |

No script in this repository may default any of these to "true". A live command
is run only after the corresponding record exists; otherwise stop at
`RELEASE_BLOCKED` and keep the isolated work.

## 11. Backup/restore/restart rehearsal

```bash
OUT="/secure/rehearsal-$(date -u +%Y%m%dT%H%M%SZ)"
bash tools/product-restore.sh --output "$OUT"
```

Preconditions enforced by the command:

- `--output` must be a new path; its parent must be writable.
- No `BBA_TASKENGINE_DSN`, `DATABASE_URL`, `BBA_DATABASE_URL`,
  `BBA_PRODUCTION_DSN`, `PGPASSWORD`, or inherited `BBA_P018_*`/`P018_*` value
  may be set. An inherited `BBA_FIXTURE_DSN` is ignored: the drill builds its
  own unix-socket DSN inside its container.
- Docker must be reachable; the run id and every container/volume name are
  derived from it and labelled `bba.p018.drill=1` and `bba.p018.run=<id>`.
- Only resources carrying this run's label are removed; the exit path verifies
  that none survive.

Exit `0` means `report.json.state == "PASS"`. Artifacts under `$OUT`:

| File | Meaning |
| --- | --- |
| `report.json` | machine-readable finish report: UTC start/end, duration, drill exit, stream hashes, cleanup and problem list |
| `drill-summary.json` | seed/verify evidence summary, fingerprints, unresolved counts, work item states |
| `drill.stdout.log`, `drill.stderr.log` | raw driver output |
| `seed-evidence.json`, `verify-evidence.json` | Go stage evidence |
| `db-restore-proof.txt`, `db-restore.jsonl` | independent psql proof and its input rows |
| `environment.txt`, `code-files.txt`, `evidence-sha256.txt` | run environment, hashed source, drill-internal manifest |

The rehearsal seeds, for the current head: three unresolved executions (pending
receipt, UNKNOWN receipt, DISPATCHING before send), the proposal work ledger
(`work_materialization` → `work_item` → `work_item_attempt`) bound to those
executions, the S2 graph mappings (roots, provenance anchor, nodes, outbox,
checkpoints, planning snapshot) and an ONLINE S4 login session. It then:

1. dumps with `pg_dump --format=custom` and restores with
   `pg_restore --single-transaction` into a second database;
2. compares a generic schema fingerprint (relations, columns, defaults,
   constraints, indexes, functions, triggers, types) and a generic data
   fingerprint (every table's row count plus ordered row hashes, plus sequence
   positions) between source and restored;
3. replays `RecoverDispatching`, `ExpirePermits`, `ReconcileTimeouts`,
   `ReconcileWorkItemStates`, `ClaimReadyWorkItem`, `SweepExpiredWorkItemClaims`,
   `Dispatch`, `IssuePermit`, `ScheduleRetry` and `Reserve`;
4. requires zero adapter calls, no new execution/permit/retry/result, unchanged
   holds, UNKNOWN work items staying terminal, the pending work item staying
   `WAITING_RESULT`, and no claimable work.

Pass criteria to check in `report.json`: `state=PASS`, empty `problems`,
`resources.owned_resources_removed=true`, `production_dsn_used=false`, non-empty
`streams.stdout.sha256`, and in `drill-summary.json`:
`restore.restored_matches_source=true`,
`restore.adapter_calls_after_restore=0`,
`restore.work_items_claimable=0`,
`restore.expired_work_item_claims=0`,
`restore.restored_schema_fingerprint == seed.schema_fingerprint`,
`restore.restored_data_fingerprint == seed.data_fingerprint`,
`db_proof.pass=true`, and every `exits` value `0`.

This rehearsal is fixture-only and is not a production backup/restore. Do not
use it as evidence for `production_backup_restore`.

## 12. Monitoring

Authenticated runtime projection (Bearer token with `operator.read`):

```bash
curl -fsS -H "Authorization: Bearer $(cat /etc/bba/operator-token)" \
  http://127.0.0.1:18421/v1/operator/runtime
```

It reports `schema_version`, `environment`, worker flags and id, feature flags,
the last model request and configured limits. A `503
STATE_STORE_UNAVAILABLE` means the database path is broken, not that the worker
is idle.

SQL probes for the alerting layer:

```sql
-- schema and startup contract
select max(version) from core.schema_version;

-- unresolved external effects: must be visible, never auto-cleared
select state,count(*) from core.execution
 where state in ('DISPATCHING','UNKNOWN') group by 1;

-- work ledger: a growing READY/BLOCKED backlog or a stuck CLAIMED item
select state,count(*) from core.work_item group by 1 order by 1;
select count(*) from core.work_item where state='CLAIMED' and lease_until < now();

-- outboxes
select state,count(*) from core.outbox group by 1 order by 1;
select state,count(*) from bridge.outbox group by 1 order by 1;
select state,count(*) from core.graph_sync_outbox group by 1 order by 1;
select count(*) from core.graph_sync_outbox where state='needs_reconcile';

-- login sessions and account projection
select state,count(*) from core.login_session group by 1 order by 1;
select count(*) from core.account where control_state='blocked' or risk_state<>'clear';

-- model gateway
select state,count(*) from core.model_request group by 1 order by 1;
```

Alert when: schema differs from the deployed binary's `--check` expectation;
the server process exits or its worker line disappears; `UNKNOWN` or
`needs_reconcile` counts increase without an operator reconciliation; a
`CLAIMED` work item's lease is long expired; `bridge.outbox` or
`core.graph_sync_outbox` backlog grows; any account is `blocked`; login sessions
sit in `QR_READY`/`AUTHENTICATING` past `qr_expires_at`; `model_request` errors
repeat.

## 13. Rehearsal record (filled by the acceptance run)

The final acceptance run inserts the actual command, UTC window, exit code,
`report.json` sha256, fingerprints and unresolved/work-item summary here. An
empty row means "not yet rehearsed", not "passed".

| Field | Value |
| --- | --- |
| Command | `bash tools/product-restore.sh --output evals/results/bba-product-closeout-20261030T133540Z/restore-03` |
| UTC start / end | `2026-10-30T15:33:57Z` / `2026-10-30T15:34:04Z` |
| Drill exit / report state | `0` / `PASS` |
| `report.json` sha256 | `bc78ebb0fda5d32aa2daf091341be6f30938fc81990bc959157a14891e8cf1a5` |
| `drill-summary.json` sha256 | `cf02d104c23bcc14ba34a2a67f6c1a272884d11a5d4b8528cc484e6192eabab6` |
| Schema head / schema fingerprint | `21` / `55db152a2110043f03fd4f8a4d4db37b4d26e4e76ef35c9d23e0de8641cd5faa` |
| Data fingerprint (seed == restored) | `ff325799b10c4d8e6d9e3e732bebc5e3401ac013f5510dc55455eaeb3afe6af1` / `true` |
| Unresolved before: pending / UNKNOWN / DISPATCHING | `1 / 1 / 1` |
| Work items after: WAITING_RESULT / UNKNOWN / UNKNOWN | `1 / 1 / 1` |
| Adapter calls after restore | `0` (required) |
| Owned resources removed | `true` |
| Final product acceptance | Value |
| --- | --- |
| Fixture command | `bash tools/product-acceptance.sh fixture --output evals/results/bba-product-candidate-20261031T034821Z` (exit 0) |
| Candidate UTC / state | `2026-10-31T03:48:32.545605+00:00` to `03:53:01.287359+00:00`; `ENGINEERING_ACCEPTED`; all 30 requirements PASS |
| Candidate acceptance SHA-256 | `5f1e679a6187aabea101c8958ae25c5655660494874d485444e86de03b142645` |
| Candidate manifest SHA-256 | `4d995c7ea3eb82e8e8d2709cd76937d6ae4cfd2b2202942ffb804671d0ca4731` |
| Independent verify | `bash tools/product-acceptance.sh verify --package evals/results/bba-product-candidate-20261031T034821Z`; exit `0`, no problems |
| Browser | `PASS`; 91 interaction checks, 10 screenshots; synthetic fixture only |
| Restore command / UTC | `bash tools/product-restore.sh --output evals/results/bba-product-candidate-20261031T034821Z/restore`; `2026-10-31T03:52:55Z` to `03:53:00Z`; exit `0`, state `PASS` |
| Restore report / summary SHA-256 | `e526d8402803390c920c7a13611e246c2230a34a0fcf6618716a5dc6d8ae9e8d` / `b4b169e25e5dc761cfe49a17ea89474f5c687751556845f62e08251f875cf046` |
| Restore schema version / fingerprint | `21` / `55db152a2110043f03fd4f8a4d4db37b4d26e4e76ef35c9d23e0de8641cd5faa` |
| Restore data fingerprint (seed == restored) | `171a7656acfbe4202e98ac75b73562e73a299b9bcbdc77bd9f55af1bd0b988df` / `true` |
| Restore no-resend / claimable / expired claims | `0` adapter calls / `0` / `0`; owned container and volume removed; no production DSN, QQ calls, or external credentials |
| Permanent negative tests | 8 tests PASS, including complete-valid-baseline proof deletion, assertion forgery and Browser screenshot deletion followed by manifest rebuild |
| Candidate negative evidence | business proof deleted+rehashed, assertions forged+rehashed, and Browser screenshot deleted+rehashed: all detected; original unchanged |
| Independent package tamper negatives | all three tampered copies rejected; original inventory unchanged and original package re-verified successfully |
| Release check | `bash tools/product-acceptance.sh release-check --package evals/results/bba-product-candidate-20261031T034821Z`; exit `3`, `RELEASE_BLOCKED` |
| Missing release evidence | QQ controlled actions, account login identity, live ARTEX dual graph, human model holdout review, production backup/restore, migration and rollback rehearsals, sole-writer ownership, runtime configuration, monitoring alerts |
| Preserved failed attempt | `evals/results/bba-product-candidate-20261031T032440Z` retained unchanged; it misparsed unittest names and failed while writing its acceptance report. It is not accepted. |
| Superseded accepted run | `evals/results/bba-product-candidate-20261031T033055Z` passed; superseded by `034821Z`, which strengthens the missing-proof regression by exercising the production read/check path. |
| Scope / authorization | Synthetic isolated fixture only. No real QQ/login action, live ARTEX, real-model scoring, production DB migration/deployment, or writer switch was performed or authorized. |
| Final source-closed rerun | `bash tools/product-acceptance.sh fixture --output evals/results/bba-product-candidate-20261031T035521Z`; exit `0`; `2026-10-31T03:55:33.496390+00:00` to `2026-10-31T04:00:01.528239+00:00`; all 30 requirements PASS |
| Final source-closed independent checks | `verify` exit `0`; three rehashed tamper copies rejected and original unchanged; `release-check` exit `3`, `RELEASE_BLOCKED` |
| Final package hashes | acceptance `65643cb9fb232e1be909631a4ee56ba9c087a20ee649d4b85e70d41af859a5e0`; package manifest `d96e6bd45d6953176b90a6fe47f3a577447b6a5fc8487e2e31c6a4ba8a0e37e2`; source manifest `48c72cdfb40986133c250233405ac55c62d35571b5340c2f27d6dff757ea04ae` |
| Final restore report / summary SHA-256 | `c2f082300154b9f50b30dce2cb2e6600ffd89e2e52bfaba6dbd23643c4145fbf` / `4f85bf4ceaedb968ae198cffa250a6effc38cbc4ca863e7da5cf8274f3799277` |
| Final candidate resource/source checks | `owned_resources_removed=true`, `preexisting_preserved=true`, `production_dsn_used=false`, no leftovers; `history_unchanged=true`, `source_stable_during_fixture=true` |
| Latest accepted candidate before final source closure | `evals/results/bba-product-candidate-20261031T040218Z`; acceptance `ENGINEERING_ACCEPTED`; 30/30 requirements PASS; independent verify exit `0`; release-check exit `3`, `RELEASE_BLOCKED` |
| Latest candidate hashes | acceptance `869702d6d93b2031a9e438eafb145cd9035afdb561363eecd0074c584da0f659`; package manifest `471c0411b8abcbd48a24eb48c95004d35c2fe9da4d3f601e8cbced9703e07456`; source manifest `a5e7623870f2970788a8d312498b657408f75157d0dea27bc24ab613e0b613c2` |
| Latest restore | `2026-10-31T04:07:10Z` to `04:07:15Z`; report `aacceaedb4b9c99c129d272decb942db4d50ac6f664e4e189629aad980e8b0a0`; summary `29862c93936b0db9f75c260b22ae118b7427b2669a17ac8ed62bcd60e996e04e`; schema `21`; schema fingerprint `55db152a2110043f03fd4f8a4d4db37b4d26e4e76ef35c9d23e0de8641cd5faa`; data seed/restored `ae1d7194bc79e8d263bba022772a839d79f328faea3ac8f66d73475af8efa50e` / `true` |
| Latest tamper negative | Three rehashed tampered copies rejected: deleted business proof, forged business assertions, deleted Browser screenshot; original inventory unchanged and original verify passed. |
| Ultimate accepted candidate before final source closure | `evals/results/bba-product-candidate-20261031T040929Z`; acceptance `ENGINEERING_ACCEPTED`; 30/30 requirements PASS; 19/19 runner steps exit `0`; independent verify exit `0`; release-check exit `3`, `RELEASE_BLOCKED` |
| Ultimate package hashes | acceptance `e7585f8d2b07556c9395b2e53198ccab6e75934ae85c9ee9eef18ac67699d1d3`; package manifest `67fe72358286160cd5e982becccd1d0dc503bafb6d21a71e15525569467c9f83`; source manifest `f91b70ed1df460080e8b82ab9783e99331c4f7d44786510b074dded3ddf9d039` |
| Ultimate Browser / restore | Browser `PASS`, 91 checks, 10 screenshots, 0 failed assertions; Restore `2026-10-31T04:14:12Z` to `04:14:17Z`, exit `0`, state `PASS`, schema `21`, restored schema/data fingerprints match seed, adapter calls after restore `0` |
| Ultimate restore hashes | report `2a661122c44951c27ad33e046723d1424d1ec119d3e89b393c3429b95afe2ddd`; summary `b7e944bd1ab49d811c0b18bedd5f33c0be02d4ba064161177154fc7ecdbcb977` |
| Ultimate negative and cleanup | candidate negative proof all 3 detected and original unchanged; independent rehashed deletion/forgery copies all rejected and original verify passed; owned resources removed, preexisting resources preserved, no production DSN, no leftovers; history/source stable `true/true` |

| Source-closed candidate after final ADR and console/matrix updates | `evals/results/bba-product-candidate-20261031T052000Z`; fixture exit `0`; `ENGINEERING_ACCEPTED`; all 30 requirements PASS; all 19/19 recorded steps exit `0` |
| Candidate acceptance / manifest / source hashes | `a60fa4026d20436b73c3a4d651122cc7301d6d7fe66fe574695f064010db40ab` / `b158fc88ce03248e15e7bfca05650a13597d3f107055edadd1438fd587090f1c` / `fae3a2c2bd5ef727f279b64b71344a36848ca3d1083ff8aa558fdbe863b194f3` |
| Independent product verify / permanent tests | `verify` exit `0`, no problems; `tests/product` 8/8 PASS |
| Browser / restore | Browser PASS, all interaction assertions and 10 screenshots; restore PASS, schema `21`, schema/data fingerprints match, adapter calls after restore `0`, claimable `0`, owned resources removed, production DSN `false` |
| Candidate business tamper proof | Product verifier rejected deleted business proof, forged assertions and deleted Browser screenshot after each copy rebuilt its manifest; original candidate unchanged |
| Release gate | `release-check` exit `3`, `RELEASE_BLOCKED`; `release/evidence.json` absent and all 10 live prerequisites missing |
| Scope / authorization | Isolated synthetic fixture only. No real QQ/login action, live ARTEX, real-model scoring, production migration/deployment or writer switch was performed or authorized. |

| Final source-closed candidate before this record | `evals/results/bba-product-candidate-20261031T060000Z`; fixture exit `0`; `ENGINEERING_ACCEPTED`; 30/30 requirements PASS; 19/19 steps exit `0` |
| Final candidate hashes | acceptance `080c1069080074feea76fd1c6644c736eddc0d7db5ca1d1237fc64f60c8a4f40`; package manifest `09d9b7dcfe278c7b36f111d2d44ac1addf0a208b8aedd5d1c5a2df6a25beccb2`; source manifest `5fecad43f2024bf0987cad6b6c26d769b8ccb2c3427437cae4b4241e34b4b9c0` |
| Final verify / tamper / release | product `verify` exit `0`; product verifier rejected all three rehashed tamper copies and original was unchanged; `release-check` exit `3`, `RELEASE_BLOCKED` |
| Final restore / browser / isolation | restore report `9becd857202b6fb9e5961a25dbc2a09da64db646b3b66353b66fc497f4183dde`; summary `aa057d6fdf6796751295a8af75ca116ed8d8732dcab2ed30488c5b9a362dcf0a`; schema 21; restored data/schema matched; adapter calls `0`; browser PASS; owned resources removed; production DSN false |

| Ultimate candidate before final evidence record | `evals/results/bba-product-candidate-20261031T070000Z`; fixture exit `0`; `ENGINEERING_ACCEPTED`; 30/30 requirements PASS; 19/19 steps exit `0` |
| Ultimate hashes | acceptance `faf274855fb538a6598dec83edc1ad059c1231892abea15faaa6746496464596`; package manifest `0eab0c713080b9bae4921cc8f404053c3e36d571d9c5712e009f5b652780642e`; source manifest `91fa52d674aa65ac83e434415aff93a449d6d1aef298a1ee5a3bb695b915809d` |
| Ultimate verify / tamper / release | product `verify` exit `0`; all three rehashed tamper copies rejected and original unchanged; `release-check` exit `3`, `RELEASE_BLOCKED`; all 10 live prerequisites missing |
| Ultimate browser / restore / isolation | Browser PASS; restore schema `21`, schema/data fingerprints matched, adapter calls `0`, claimable `0`; owned resources removed; production DSN `false`; source stable during fixture `true`; history unchanged `true` |

## 受保护配置、请求与恢复检查（2026-11-05 承接）

以下承接原根规则中的运行要求，不是本轮运行结果。

- resolver/provider/Bridge JSON、operator token 文件按 loader 要求使用常规文件、0600 或更严及大小上限；registry 父目录私有、单进程持有 lifetime flock。不要读取/打印真实凭据补示例；token、API key、DSN 密码、OneBot/WebUI 凭据不进仓库、日志、PR、截图、模型上下文或浏览器持久化。
- 保留 loopback、固定 Host/Origin、operator/runtime credential 分离及先鉴权再 replay；外呼保持 timeout、响应体/并发上限、redirect policy 和 secret-safe errors。读取重试须证明安全，限制总超时、次数、退避与停止条件；QQ 写未知不自动重发、不换账号、不释放可能占用的 claim/slot/预算。ARTEX 等外部写按自身 outbox/checkpoint/reconcile 协议处理；模型调用按 provider/run 约束处理可能重复计费及重复/迟到响应。
- 模型 registry 文件与 PostgreSQL 请求账本没有跨存储事务；崩溃遗留 started 需按协议核对。4 MiB 文件上限、历史/配置回执增长及备份均须纳入检查，不直接扩大或清除历史绕过拒绝写入。
- 真实动作前重新核对 baseline、身份/owner、能力、范围、审批、预算、文本规则、停止/撤销、自动发送及 stop-contact。快照不替代当前撤销检查。盘点每个实际 writer 的最终门禁，未知能力即拒绝；没接入 stop-contact 的联系动作不得上线。
- fixture 仅隔离 PostgreSQL、loopback fake HTTP 和假 adapter，不传生产 DSN；本地开发 start 会显式迁移 fixture 并启动模拟 Worker，reset 会删除该实例状态，env/token 会输出秘密，先核对目标、独立实例名与目录。构建工具 generate 会覆盖生成资源，不能作为只读检查运行。
- Character/Collector 配置从各自 `src/types.ts` 和 resolver 的显式 options 核对 stateDir、身份、账号/runtime 与 backend/gateway；没有统一生产 `.env` 可照抄。Controller 的 repository/state-dir、mode/slots 与研发授权由启动侧约束，MCP 请求不得升级；其 SQLite/进程组清理、UNKNOWN 和模拟预算不证明 QQ 运行安全或真实模型路由。
- 未获本轮明确授权，不操作 QQ、真实模型（含本地模型）、搜索/MCP/ARTEX 等真实业务服务，不连接生产库、不改真实运行目录、不重启服务、不切 writer。本手册命令及历史授权都不自动授权这些动作。
