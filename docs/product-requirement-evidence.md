# Final product requirement-to-evidence matrix

> **2026-11-05 阅读范围：** 本表保留 S1–S6 历史证据与原外部来源定位；来源未随仓库交付时不能当作必读文件或当前证据。长期目标及后续验收以[根规则](../AGENTS.md)、[有效设计补充](adr/bba-final-closeout-2026-10-31.md#后续适用规则2026-11-05)和[产品验收矩阵](product-acceptance-matrix.md)为准。图同步、旧 Console、fixture 及历史接受状态均不证明 ARTEX 核心 planner、真实模型或完整产品已交付。

Source: `../astra-handoffs/BBA-final-closeout-omp.md` S1–S6 and `/run/media/sun/6F53FE6DFAA8CECF/BBA/qq-automation-design/{01-architecture,02-contracts,03-implementation-plan}.md`. This is the final closeout tracking matrix. Fourth-round artifacts are preserved historical baselines, not new product acceptance.

`PENDING` means no final-closeout proof yet. `FIXTURE_ACCEPTED` requires the real module chain against boundary fixtures. `LIVE_REQUIRED` identifies separately authorized external acceptance. Only the final acceptance runner can produce `ENGINEERING_ACCEPTED`; release prerequisites remain separate.

| ID | Requirement | Implementation / actual consumer | Permanent verification / evidence | Unified UI entry | Engineering / release status |
|---|---|---|---|---|---|
| KERNEL-01 | PostgreSQL execution truth, immutable attempts, scoped identities | task/scheduler/bridge/observations | kernel race suite + product runner | Tasks / trace | FIXTURE_ACCEPTED |
| ACCOUNT-01 | Multi-account identity, readiness, isolation and capability projection | heartbeat, runtime credentials, resolver | two-account isolation + identity readback | Accounts | FIXTURE_ACCEPTED / LIVE_REQUIRED |
| ACCOUNT-02 | NapCat login sessions with generation, cancellation and identity verification | login provider + durable session consumer | real-protocol fixture state machine / browser | Account login | FIXTURE_ACCEPTED / LIVE_REQUIRED |
| TASK-01 | Fixed account/target scope, target deduplication, baseline, goal slots, quotas | task, expansion, scheduler | permanent join/baseline/budget regressions | Task create/detail | FIXTURE_ACCEPTED |
| TASK-02 | Pause/cancel/approval, deadlines, authorization revision | task control + reserve/dispatch gates | claim→cancel→reserve race, expiry | Task controls / risk | FIXTURE_ACCEPTED |
| TASK-03 | Persistent worker rotation, leases, restart and failure isolation | Worker recovery/outbox | worker cursor suite + restart runner | Operations | FIXTURE_ACCEPTED |
| PLAN-01 | Legacy compatibility and explicit proposal orchestration mode | task orchestration + legacy scheduler exclusion | proposal mode/legacy integration | Task mode | FIXTURE_ACCEPTED |
| PLAN-02 | Intent-only apply preserved; explicit approved materialization | proposal application + materializer | approval/digest/idempotency/scope/budget checks | Plan materialize | FIXTURE_ACCEPTED |
| PLAN-03 | Persistent work items, DAG, attempt→execution, lease epoch fencing | proposal work consumer→Reserve→Bridge | double-worker, stale fence, dependency failure | Plan trace | FIXTURE_ACCEPTED |
| PLAN-04 | Crash recovery and UNKNOWN without resend | worker/bridge recovery + work reducer | claim/reserve/dispatch crashes, retained slots | Unknown/reconcile | FIXTURE_ACCEPTED |
| PLAN-05 | Trusted observation exact causal result propagation | observation→execution→attempt→work→application/task | exact ID chain + late-result rules | Full causal trace | FIXTURE_ACCEPTED |
| GRAPH-01 | Verified ARTEX source/API/version/license | standalone REST adapter | protocol fingerprint + contract fixture | Provider diagnostics | FIXTURE_ACCEPTED / LIVE_REQUIRED |
| GRAPH-02 | Fact/goal roots, provenance-rich anchors and mappings | graph sync outbox consumer | Summary/event→external nodes→mapping | Dual graph | FIXTURE_ACCEPTED / LIVE_REQUIRED |
| GRAPH-03 | Outbox/checkpoint, partition isolation, ambiguous writes reconciliation | graph synchronization worker | timeout, crash before checkpoint, restart, duplicates | Graph sync status | FIXTURE_ACCEPTED |
| GRAPH-04 | Bounded planning snapshot and stale-fact rejection | snapshot→candidate→S1 validation | Summary/revision/scope invalidation; bounded rounds | Plan basis | FIXTURE_ACCEPTED |
| ANALYSIS-01 | Whitelisted messages, single collector owner, durable windows, references | real Collector | two accounts/groups, duplicate/restart/injection | Analysis report | FIXTURE_ACCEPTED |
| ANALYSIS-02 | Statistical baseline and injectable semantic provider with evidence/confidence/expiry | Summary producer/publisher | real producer→backend storage | Read-only report | FIXTURE_ACCEPTED |
| CHARACTER-01 | Real GroupContext/prompt, persona and cross-account isolation | backend provider→Character→ChatLuna prompt | permanent cross-module runner | Group context / persona | FIXTURE_ACCEPTED |
| CHARACTER-02 | Active speaker, filtering, text-only permit and boundary transport result | Character gate→backend permit→transport | two-group send, unknown no retry | Chat trace / risk | FIXTURE_ACCEPTED / LIVE_REQUIRED |
| CHARACTER-03 | Repeated startup/shutdown without lifecycle exception or leak | pinned ChatLuna provider lifecycle | repeated lifecycle regression, exact expected fault classification | Operations diagnostics | FIXTURE_ACCEPTED |
| MODEL-01 | Scoped provider configuration, timeout/concurrency/budget and no model authorization | model gateway / providers | protocol fixture + scoped configuration | Model settings | FIXTURE_ACCEPTED / LIVE_REQUIRED |
| MODEL-02 | Frozen holdout, model/template/resource provenance and human quality score | quality evaluation artifacts | evidence references / contamination / persona / pacing scoring | Evaluation status | FIXTURE_ACCEPTED / LIVE_REQUIRED |
| UI-01 | Existing console unified accounts/tasks/analysis/dual graph/plans/models/risk/operations | embedded web + authenticated handlers | actual browser interaction | Existing console | FIXTURE_ACCEPTED |
| UI-02 | Accurate fixture/dry_run/live, empty/error/403/409/5xx/offline states | UI readback and capabilities | browser failure states and refresh GET-only | Existing console | FIXTURE_ACCEPTED |
| UI-03 | Keyboard and narrow viewport usability, secrets kept server-side | login/UI forms/session views | browser keyboard/mobile + sanitized network | Existing console | FIXTURE_ACCEPTED |
| OPS-01 | One owned isolated fixture acceptance command; exact raw results and cleanup | tools/product-acceptance.sh fixture | migration/race/contracts/Node/cross-module/browser/restart/restore | Operations | FIXTURE_ACCEPTED |
| OPS-02 | Independent evidence and requirement completeness verification | tools/product-acceptance.sh verify | missing evidence and tamper negatives | Acceptance report | FIXTURE_ACCEPTED |
| OPS-03 | Explicit blocked release preconditions, no implicit real authorization | tools/product-acceptance.sh release-check | missing prerequisite→BLOCKED/nonzero | Release status | FIXTURE_ACCEPTED / LIVE_REQUIRED |
| OPS-04 | Deployment, migration, backup/restore, rollback, sole writer and monitoring | release runbook + isolated rehearsal | executable rehearsal logs | Operations | FIXTURE_ACCEPTED / LIVE_REQUIRED |
| AUDIT-01 | Source baseline/delta, original artifacts unchanged, exact requirement coverage | final package/report/matrix | source/package hashes + old inventories | Acceptance report | FIXTURE_ACCEPTED |
| RELEASE-01 | Authorized QQ/login/ARTEX/model/production acceptance | explicit external acceptance records | named accounts/groups/action bounds/window/stop criteria | Release checklist | LIVE_REQUIRED |

Final-closeout baseline: `evals/results/bba-product-closeout-20261030T133540Z/baseline/` contains the verified fourth-round source list, the 263-file development-before snapshot and immutable historical package inventories. New development will change current source hashes; historical packages must not be re-finalized.
