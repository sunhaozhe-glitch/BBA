# ADR: BBA S1–S6 final product closeout

- Status: accepted for isolated engineering; live release remains blocked
- Date: 2026-10-31
- Scope: the current BBA source tree after the proposal, ARTEX, analysis/Character, login/UI and acceptance-runner implementation
- Supersedes: implementation-status claims in the pre-implementation ARTEX mapping and development-console notes. Historical ARTEX research facts remain useful only where this ADR does not replace them.

## Decision

PostgreSQL remains the sole execution and result authority. ARTEX is a bounded external fact/goal graph reference. Summary data is untrusted reference context. Models, graph text and approval records never grant an external action. All QQ-sensitive effects still pass the existing Reserve, permit, bridge and observation gates.

The current engineering integration is fixture-capable and explicitly configured. It does not constitute live QQ, live ARTEX, real-model quality or production acceptance.

## S1: proposal-driven durable work

Legacy `ApplyPlanProposal` remains intent-only. A task becomes proposal-orchestrated only through the explicit `POST /v1/tasks/{id}/orchestration` command while quiescent, and only the proposal worker is allowed to expand that task. `POST /v1/tasks/{id}/plan-proposals/{proposal_id}/materialize` validates the approval reference, proposal digest, task version, scope, authorization revision, expiry, budget, target/account scope and dependency DAG in one PostgreSQL transaction. It creates the application, materialization event/outbox and durable `work_item` rows; it does not reserve, issue a permit, call a model, call ARTEX or call QQ.

The worker claims one READY item with `FOR UPDATE SKIP LOCKED` and a lease epoch. It rechecks the task, authorization revision, pause/cancel state and exact account/target binding before `ReserveWorkItem`. Reservation continues through the existing scheduler, permit and bridge chain. A claim that has no execution may expire back to READY. An item that may have reached dispatch remains WAITING_RESULT/UNKNOWN and is never returned to READY. Observations project by exact execution/attempt/work-item identifiers; account/group coincidence is not causal evidence. Legacy tasks do not enter the proposal queue.

The operator trace is exposed by `GET /v1/tasks/{id}/work-items`, which reports proposal, application, materialization, step, work item, attempt, execution and observation identifiers separately.

## S2: ARTEX dual graph and planning snapshots

The local ARTEX source inspected for this integration is tag `v0.3.23`, HEAD `620ca5d96c33a417b08fd926e04df1e2b61ffa6d`, module `github.com/Autumn-27/artex`, licensed AGPL-3.0. BBA copies no ARTEX source and does not link it as a runtime dependency.

The verified HTTP boundary is the local ARTEX REST API: graph reads use `GET /api/exploration/graph` and `GET /api/exploration/nodes`; goal mutations use the documented `/api/tasks/{id}/goals` routes. The BBA client rejects an empty/`active` task alias, distinguishes not-configured/unauthorized/protocol failures, retries bounded safe reads only, and performs no automatic retry for mutating writes. An ambiguous goal write becomes `needs_reconcile`; reconciliation reads the current goal list and never resends the mutation.

BBA stores explicit fact and goal root bindings, provenance anchors, external node mappings, per-task/graph checkpoints and a transactional `graph_sync_outbox`. The GraphSyncWorker ingests trusted local task events and live, unexpired Summary versions in a transaction, calls ARTEX outside the transaction, then records the result in a separate checkpoint transaction. A planning snapshot binds graph revision, Summary id/version/manifest, anchor ids, task version, scope revision, authorization revision, model/template versions, bounded candidates, round budget and expiry. Any relevant change marks it stale and blocks proposal/materialization.

The feature is not silently considered connected when no provider file is configured. Provider configuration requires an explicit protected file; goal writes require `allow_writes: true`. The isolated product runner uses the protocol fixture and the same adapter/worker code. No live ARTEX endpoint was used in the engineering evidence.

## S3: analysis and Character closure

`tools/product-cross-module.cjs` drives synthetic platform messages through the real Collector listener and durable window owner, the Summary producer/publisher, the backend GroupContext endpoint, the real Character/ChatLuna prompt path, the backend text permit and a boundary fake transport. It covers two accounts and two groups, duplicate windows, expiry, injected text, restart and UNKNOWN/no-retry. The evidence records source manifest, scope, Summary/window identity, prompt context, permit decision and transport outcome. The fixture explicitly reports `real_model_evaluated=false`; it is not a human quality acceptance.

ChatLuna `1.4.4` is patched by `koishi/character-runtime/tools/patch-chatluna.cjs` at the exact upstream teardown pattern where Cordis may dispose the logger before `uninstallPlugin`. The patch fails closed if the pinned version or source pattern changes. `test/lifecycle.test.ts` starts and stops the real Character assembly three times, captures `internal/error`, and checks process listener counts; an unexpected teardown TypeError fails the test.

## S4: login and unified console

The server explicitly mounts orchestration, graph sync, login, model, GroupContext/report, chat gate and runtime routes in `serverAPISurface`. Login uses durable `CREATED → QR_READY → SCANNED → AUTHENTICATING → ONLINE` state, generation fencing, cancellation fencing, provider identity readback and account projection. Runtime credentials are server-side; the browser receives only the short-lived QR/session projection and keeps its bearer token in tab memory.

The embedded same-origin console is the single operator surface for accounts, tasks, reports/GroupContext, BBA graph and ARTEX sync status, proposal/work-item trace, login sessions, model/runtime diagnostics and pause/risk state. It reads back after mutations and exposes fixture/dry-run/live boundaries. The browser evidence covers keyboard, narrow viewport, empty/error/403/409/5xx/offline and refresh behavior; no live credential or external QQ operation was exercised.

## S5: one-command evidence and release gate

The permanent commands are:

```bash
bash tools/product-acceptance.sh fixture --output evals/results/bba-product-candidate-<UTC>
bash tools/product-acceptance.sh verify --package evals/results/bba-product-candidate-<UTC>
bash tools/product-acceptance.sh release-check --package evals/results/bba-product-candidate-<UTC>
```

`fixture` owns isolated PostgreSQL, browser and fixture services, records argv/stdout/stderr/UTC/exit codes, preserves failures, removes only owned resources, and writes package/source manifests after evidence is complete. `verify` validates business assertions and required scenarios in addition to hashes. `release-check` never deploys and returns `RELEASE_BLOCKED` until every live prerequisite has scoped external evidence.

## S6: evidence boundary

A candidate may be `ENGINEERING_ACCEPTED` when all local product paths and negative checks pass. That state does not imply `RELEASE_ACCEPTED`. Live release still requires separately authorized QQ actions and identity readback, live ARTEX dual-graph evidence, human model holdout review, production backup/restore, migration and rollback rehearsals, sole-writer ownership, runtime configuration and monitoring alerts. No such operation is performed by the fixture runner or implied by this ADR.

Evidence entry points:

- `docs/product-requirement-evidence.md` — requirement-to-evidence matrix
- `docs/product-release-runbook.md` — deploy, migration, backup/restore, drain, writer fence, rollback and monitoring procedure
- `tools/product-cross-module.cjs` — permanent S3 chain
- `tools/product-acceptance.sh` — S5 acceptance and release gate
- `koishi/character-runtime/test/lifecycle.test.ts` — ChatLuna lifecycle regression
