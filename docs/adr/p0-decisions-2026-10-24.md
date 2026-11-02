# ADR: P0 Decisions (2026-10-24)

- Status: Accepted by product owner on 2026-10-24
- Scope: Contract and deterministic kernel implementation authorization P0-01 through P0-11 only. This ADR does not authorize P0-12 or later, live QQ reads/writes, deployment, production writer switching, or frozen NapCat runtime modifications.
- Supersedes: `docs/pre-implementation-decisions.md` recommendations and unresolved labels. That report is retained as the decision process record; this ADR is normative where they differ.

## Decisions

### D1 — NapCat plugin ID

Use the locally registered plugin ID `napcat-plugin-cleaner`; canonical server-side adapter paths are `/api/Plugin/ext/napcat-plugin-cleaner/search-group` and `/api/Plugin/ext/napcat-plugin-cleaner/join-group`. Update caller base paths, not the frozen NapCat directory or registration ID. P0-01 contract/fake tests protect the path; never validate through real QQ writes.

### D2 — Pending, unknown, and timeout

`submitted`, `pending`, and `unknown` are not success. While an external effect may still occur, retain the associated goal slot and target claim. A 24-hour timeout does not release quota, reassign the account, or retry: transition to manual reconciliation. Old 24-hour release-plus-cooldown behavior is excluded from P0 defaults. Any future policy exception requires separate explicit approval.

Local-quiescence pause semantics (adopted clarification, 2026-10-24; expressly confirmed by the product owner in the separate P0-12 authorization). Pausing separates local sending quiescence from external unsettledness:

- A task may settle `PAUSING → PAUSED` once no local managed dispatch is still active, no execution remains in `DISPATCHING`, and no issued permit can still be used by a compliant dispatcher, regardless of externally pending or unknown results.
- The implementation counts process-local active local dispatches in a registry scoped to the managed worker's lifetime. A dispatch remains counted even if recovery has already moved the database operation from `DISPATCHING` to `UNKNOWN` while the adapter call is still running, so a settled pause cannot overlap an adapter call that only this process knows is live.
- Externally pending/unknown work keeps its original operation, slot, claim, action lock, and quota. Pause settlement and resume must not requeue it, reissue a permit, reset its retry budget, or retry it. Resume revalidates scope, accounts, baseline, deadline, and unresolved work, and uses only remaining free capacity.
- This is a process-local quiescence proof only. It is not production process-manager, cross-process, cross-machine, old-owner, or global single-writer isolation. If an old worker/process is not proven stopped, owner migration remains forbidden under D5, and the bridge crash semantics require `BLOCKED` rather than assuming safety.

This clarification relaxes neither slot/claim retention nor the 24-hour read-only reconciliation path. The owner has confirmed that fifth-round independent dual review passed. Commands and outputs in the P0-04–11 report remain labeled as this workspace's own runs; the independent-review conclusion is recorded from the owner's confirmation.

### D3 — Character account isolation

For every QQ account with Character enabled, MVP uses a separate Koishi runtime and separate Character database. P0 does not namespace-patch Character to merge accounts. Single-runtime multi-account Character is P1 only after isolation tests cover cache, locks, triggers, variables, history, memory, tool context, and every proactive send path.

### D4 — Console QR login

QR login remains in the unified console. The console backend proxies NapCat WebUI; browsers do not directly cross-origin call WebUI. Tokens, cookies, verification codes, and other secrets must not enter browser persistent storage, model context, or ordinary logs. `host="::"` plus `accessControlMode=none` is a separate deployment-security item: P0 must not widen exposure; later propose loopback/controlled-private-network tightening.

### D5 — Writer ownership

Use strangler/shadow: read-only comparison; enumerate every writer; stop old writer; reconcile all in-flight and unknown effects; switch owner; verify new owner; retain explicit rollback point. At any time exactly one effective writer may own a given account and `action_family`. The in-process plugin execution path is a potential third writer and must be included. No automatic production switch is authorized.

### D6 — Audit truth

PostgreSQL persistent `task_event`, `execution`, `gateway_operation`, `inbox`, and `outbox` records are the Task Engine audit/recovery truth. The old 2000-entry in-memory ring is UI/debug cache only, never business audit or recovery state.

### D7 — Strict acquire candidate source

P0 `group.acquire` uses an explicit fixed candidate list/import. Existing keyword search is retained as a future CandidateProvider adapter only; it is not the strict acquire default until authorization, pagination/bounds, source provenance, and pending/reconciliation capabilities have been verified.

### D8 — ARTEX and AGPL

Review dependency and license boundaries before reuse. Prefer verified, domain-independent infrastructure; do not pull ARTEX Agent or penetration-testing domain dependencies into the new Task Engine for small code reuse. Clarify license impact before copying source across boundaries.

### D9 — Installation and model baseline

Treat only future actual install versions/configuration readbacks as facts for Koishi, ChatLuna, Character, local 9B, Swift, and other model/runtime components. Snapshots and design documents are not installed versions or model benchmarks.

## Implementation gates

- P0-01 may define later-phase paths and schemas, but those operations remain disabled until their stage is separately authorized. Contract inclusion is not operational enablement.
- P0-01 acceptance: one normative `contracts/openapi.yaml`; local schemas only; schema validator passes; generated Go and TypeScript types agree with the OpenAPI contract; all examples validate against their declared schemas; unsafe/remote schema refs are rejected.
- P0-02 onward must preserve strict slot/claim, evidence, authorization, retry, writer ownership, and persistent audit rules above.
- P0-12 and later are not authorized by this ADR. No real QQ write, group application/join, bulk account action, frozen NapCat directory change, old BBA asset deletion, or production writer switch.

## Evidence rule

Separate current observation, static configuration/source, upstream snapshots, and historical acceptance in all implementation reports. A historical test/acceptance record is not a current test result. If code facts contradict this ADR or the approved design, stop at that boundary and report the evidence; do not silently choose a workaround.

## Owner rulings accompanying the P0-12 and P0-13 authorizations

These rulings record the owner's decisions; they do not add runtime behavior or broaden this ADR's P0-01–11 authorization scope.

- Strict acquire follows D7. Keyword retrieval remains a future CandidateProvider adapter; superseded main-design clauses will be reconciled at the next freeze. No keyword-search implementation is introduced here.
- Upgrade/rollback compatibility is explicitly waived at this stage for the single environment with no deployed database. Rejecting unknown schema versions remains the safe behavior; compatibility gates move to production readiness. No migration compatibility branch is added.
- Before every round of real writes, baseline must be read again. This is a mandatory operational rule, with no engine TTL or cache added. P0-12 authorizes no real writes.
- P0-12 was separately authorized for the napcatd-managed instance **webui 6211 / OneBot 127.0.0.1:3211** (the earlier 6210 binding was replaced), one explicitly bound account, `get_login_info`, `get_group_list`, `get_status`, and a bounded read-only health-report loop with persisted evidence. Each manual run permits at most 10 read requests, including at most one retry for a transient transport/5xx failure. No login, send, join/application, plugin write, service lifecycle change, production DB, frozen-directory edit, or writer switch is authorized.
- The bound account is QQ `36374700`. Under the current owner instruction, exactly one live invocation ran at 2026-10-24T18:11:45Z: the token was read directly from state.json, identity matched, status was online/good, 52 groups had UTC source times, and the local heartbeat returned 202 and persisted as applied with account version 2. This invocation used 3 of 10 QQ reads and no retry; earlier two invocations are separately retained as history. The entire run now permits only one transport/5xx retry in total, with per-step evidence persisted before continuing. No existing service was restarted; only disposable fixture infrastructure and a temporary local receiver were run. Snapshot deletion remains due on 2026-11-24.
- Heartbeat contract remediation (owner-instructed, fixture-only; no new live QQ call): the receipt returned by a successful heartbeat is persisted and reused for an `Idempotency-Key` replay; the same key with a different payload is `IDEMPOTENCY_CONFLICT`; the same `(owner_epoch, sequence)` with a different digest is `HEARTBEAT_SEQUENCE_CONFLICT` and must reach the client as HTTP 409 through the `ContractError` channel; a newer owner epoch reopens the sequence space; a stale delivery echoes the request sequence, records a `stale` audit row, and never moves the account projection. Capability declarations are validated against `common.json#/Capability`, not merely decodable. This advances the fixture schema to 9 (`core.runtime_heartbeat` gains the receipt columns plus the per-request idempotency unique key) and is not evidence of a production heartbeat agent or deployment.
- Seventh-round P0-12 remediation (owner ruling, fixture-only): an `Idempotency-Key` and any alias that successfully replays an event bind to the same single heartbeat fact; changing the payload under a bound key is `IDEMPOTENCY_CONFLICT` and must not append a second audit row. Capability declarations are validated from the raw request JSON, so generated-type zero values cannot erase required/null semantics. Every retry attempt keeps isolated evidence. The Round 5 `TestSingleRetryAcrossReadChain` is restored verbatim. Schema advances to 10 via append-only `core.heartbeat_idempotency`; no upgrade compatibility is added. The old red-team assertion requiring stale `sequence=20` was retained during that round as a reported non-green item; **ADR:75** (2026-10-25 owner ruling, `/tmp/bba-r8/RETIREMENT.md`) now retires that expectation: a stale delivery echoes the request sequence (`19`), keeps the account projection at version 2, and records exactly one `stale` audit row. The assertion changed only from `20` to `19` with a dated comment; original probe bytes remain under `/tmp/bba-r8/retired-original/`. ADR:74's request-echo rule is unchanged.
 - **ADR:76 current P0-13 status** (2026-10-25 owner instruction): the owner supplied controlled target group **829117089** and the cleaner-loaded confirmation text **“我是小肥猪”**, completing the two prerequisites and authorizing exactly one live invocation for account **36374700**, reason **“我是ai机器人”**. The runner independently verified cleaner `active` on 6211/3211 and target absence in both 52-group baseline reads. That single invocation ran at **2026-10-25T06:27:00.515548775Z–06:27:03.384280883Z**, using 7/10 budget calls, one D1 `napcat-plugin-cleaner` write, zero retries, and one submitted unresolved execution. Cleaner returned `{"ok":true,"result":1}` without a request reference; both post-write lists lacked membership. Outcome is `SUBMITTED_PENDING_MEMBERSHIP`; no valid result, no confirmed membership, and no further request or polling is authorized. The R11 candidate remains undeployed; its separate answer-bearing dual-review procedure is not yet run.

See the [P0-12 implementation and live-run record](../p0-12-readonly-2026-10-24.md) and the [P0-13 implementation and run record](../p0-13-join-2026-10-25.md).
- **R12 offline remediation status** (2026-10-25): F1/F2/F3 from the sol review were corrected and verified only through local candidate behavior tests, actual route-to-schema integration, contract generation, author tests, and offline batteries. This entry does not authorize deployment, instance restoration, QR scanning, question-answer writes, real runner execution, QQ writes, or writer switching. The accidental Electron-as-Node probe and the independently observed managed-instance GPU crash remain separate incident records.
