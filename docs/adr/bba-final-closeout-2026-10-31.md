# ADR: BBA S1–S6 final product closeout

> **2026-11-05 适用范围补充：** 下列 Decision 与 S1–S6 保留 2026-10-31 的决定、集成形态和证据事实。长期产品目标及后续设计约束以根 [AGENTS.md](../../AGENTS.md) 和本文末尾[后续适用规则](#后续适用规则2026-11-05)为准：其中 ARTEX 核心编排目标取代“外部图参考”作为永久产品边界的解释；日常 Chat Permit 不经过任务 Reserve。本次只统一文档，不新增运行能力、真实调用或发布授权。

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

## 后续适用规则（2026-11-05）

本节承接原根指南中的有效详细设计约束，仅约束后续实施；不是新功能完成声明。它补充 S1–S6，明确取代以下后续解释：Decision 的 ARTEX 外部参考限定不再是长期产品上限；“All QQ-sensitive effects ... Reserve”只适用于任务执行域；S2 的无源码复用仅描述当时图 REST adapter，不约束尚待确认的核心复用方案。原日期、版本、许可证和测试记录保持原样。

### ARTEX、双图与主规划协调

ARTEX 是核心智能编排的架构来源，资产图、探索图、Planner/Worker、规划记忆、多角色协作、结果驱动重规划须进入实际运行链。REST 图同步和固定模板提案不能代替规划运行时。实施前调查具体上游版本、模块、接口、衔接、维护成本和许可证，再提出直接复用、适配、独立进程或重新实现的推荐，由用户确认核心复用范围；不能预设全部重写或整套搬入。独立进程不免除许可审查；架构参考或本地实现不能标称已运行上游 ARTEX planner。

每个目标/run 有明确主协调者。主计划更新、协调移交与恢复须受服务端身份、scope、版本及适用 lease/fence、审计约束；规划协调、任务领取、Reserve 和 QQ writer ownership 各自独立。专业角色默认有界分析，返回证据、不确定性和建议；受委派的局部规划也须由主协调及服务端校验。不得以“禁止第二执行器”为由禁止必要的持久 run、round、分析 step 和角色协作。

规划输入须实际消费带来源的对象、关系、证据和规划记忆；结果须能改变下一步。信息不足、证据矛盾、失败时能等待、核对、调整或停止。持久状态和证据引用保存在 PostgreSQL，模型上下文和外部图不充当恢复依据。图、Summary、网页、工具结果仍是不可信数据；消费前按协议核验 workspace/account/group/conversation/window/version/expiry/来源。fixture 只证明接线与故障边界，真实模型另验结论依据和调整合理性。

### 新增持久化域的切片交付门槛

新增持久化域（包括 CRM、规划记忆和 run/step 等）必须在本阶段、同一功能切片内，按适用范围验证 workspace/account 隔离、幂等、当前 schema 的恢复以及删除/保留语义；不得把恢复验证全部推迟到最终发布。发布手册的迁移、备份恢复程序不能替代切片交付时的验证门槛。[开发手册](../taskengine-development-console.md#按变更选择开发检查2026-11-05-承接)引用本约束，用于选择该切片的必要检查；本轮仅承接文档，不补造业务测试结果。

### 任务、独立聊天与最终门禁

- 任务沿用 resolver → Task/候选/baseline/审批 → Reserve 事务 → execution/slot/claim/budget/outbox → 单次 dispatch permit → Bridge → adapter → observation/inbox/result。Reserve 锁顺序为 task → target/claim → account/usage（账号排序）→ subtask/execution/slot；事务中不得外呼。严格目标数、最终 membership 与 baseline 判定保留，申请已提交及既有成员不算新增成功。
- Proposal materialize 只创建持久工作记录，后续复用现有 Task Engine；模型不能直接写权威业务存储、发许可或调用 QQ writer。审批绑定 digest、scope、主体、资源、版本、期限和预算，变化后重审。
- 日常聊天沿用可信 QQ trigger → account/runtime/context/active-speaker 检查 → exact text digest → durable Chat Permit → Bridge/text adapter → sent/unknown outcome。发放/消费 Chat Permit 不创建 task、subtask、goal slot、execution、target claim 或 dispatch permit；聊天任务化或操作员主动发送须另行设计桥接、审批、幂等和恢复，不得把草稿确认伪造成可信 QQ trigger。
- Character runtime、数据库、cache、persona、adapter 按账号隔离；当前文本 writer 合同不能擅自扩展多段/媒体。Collector 保持 collection-only，窗口及 Summary 生产不代表完整群分析；受控语义分析不获得发送权限。优先复用 ChatLuna、Character、Group Analysis 及现有适配，不另建重复群聊引擎。
- 一个 permit 最多一次 adapter call；已消费而无权威 outcome 时保留 unresolved/unknown，不能重新发同一动作许可。最终 dispatch 重验停止/撤销、授权、stop-contact、能力、身份/owner fence、规则、审批、自动发送开关和适用限额。每账号/动作族单 writer；协调锁不是 writer 迁移证明。
- 默认草稿；单条确认只授权具体内容和目标。自动发送另配主体、账号、群/会话、动作、期限、限额及关闭入口。全局急停阻止新外部下发及相关自动推进；明确并测试急停与最终 dispatch 的并发边界。在途/UNKNOWN 不假装撤回；停止后迟到结果只归档核对，不自动恢复计划。

### QQ 入站身份与重放校验

QQ inbox 的可信 token `OwnerEpoch` 必须经认证层（`operatorConversationAuth`）传入。事务按 account → credential 顺序持共享锁，再复用 `authorizeRuntime`；fresh 写入与 replay 都须先校验当前 owner、active、账号 scope，以及 token/account/credential epoch。自定义 auth 必须传入可信 token epoch，不得从数据库当前值补齐调用者 epoch。相关[历史实现计划与测试记录](../superpowers/plans/2026-11-04-b7-account-runtime.md)继续承担实现和测试证据职责，不替代本节有效设计约束，也不代表本轮重新实跑或新增运行能力。

### Agent、配置与工具

复用 `model_http.go`、`model_provider.go`、Agent/Skill/Model registry、Proposal/Work Item 等现有成果，不复制第二条模型执行链。thread/run/step 关联、版本快照、停止/取消/超时/恢复、重复与迟到结果接受规则，以及最大步数、并发和预算须在实施任务中明确。模型重试须考虑 provider、总超时和可能重复计费，不得重复推进已停止 run。

受保护 JSON registry 是配置和配置回执来源，不是任务/CRM/run 数据库；保留 lifetime flock、mutex、fsync/rename/目录同步及 workspace 引用校验。数据库先占模型幂等位、bound provider 后保存文件回执，不是跨存储原子事务；started 遗留不能宣称自动恢复完成。4 MiB 上限及历史/回执增长需受控归档设计，满时拒绝写。当前 Agent/Skill tools 仅空数组，能力仅 `structured_output`；变更需显式协议和测试，不能凭配置字段推断可调用工具。

Tool Catalog、Tool Schema、具体 Runtime Capability 与当前 Authorization 分开核验；MCP 注册、仓库 URL、endpoint 连通均不证明能力或权限。完整 MCP 管理平台不是生成 Proposal 的前置条件；具体动作执行前须具备最小 adapter、能力、授权与结果证据。研发 Controller 的 SQLite/队列/模拟预算只管研发，不代表 QQ 产品多 Agent 或模型路由已接通。

### Scheduler 设计约束（待实施）

复用现有 Task Engine，允许 occurrence 状态机与有界扫描，不另建通用执行器。具体 misfire、overlap、DST 和审批策略须在实施前形成对应设计决定；以下默认约束承接原根指南：

- occurrence 唯一键绑定 schedule ID + 规范化触发时刻；version 只是快照属性，不能递增版本重复物化。保存 IANA timezone、本地表达式与 UTC 时刻；DST 不存在/重复时间有明确策略及测试。
- occurrence→Task 优先同一 PostgreSQL 事务写入事件/幂等。跨事务须稳定幂等键、中间状态、owner/expiry/CAS lease、唯一约束和崩溃恢复；重跑读回同一 Task。区分 pending→claimed/materializing→materialized 及可重试/永久拒绝。
- 默认不无限补跑；misfire 明确容忍窗口及 skip 原因，coalesce/catch-up 设时间与数量上限。默认 forbid overlap；前次运行、待审批或 UNKNOWN 不得被新 occurrence 绕过，按配置 skip/排队并记录原因。
- 编辑/暂停/删除递增版本；创建 Task 前重验 version/enabled，旧 occurrence 明确 superseded/cancelled，跨版本同刻最多一个 Task；已物化 Task 走自身取消/审批/UNKNOWN 规则。
- 创建计划不等于每次外部动作授权；执行前重验当前能力、授权、审批期限、预算和规则。低风险模板授权须另行风险评估。测试领取/Task 创建前后崩溃、多 worker、重启、时钟漂移、版本修改、审批撤销和数据库恢复。

### CRM、数据与指标设计约束（待实施）

明确意向识别成功即可计入引流成功；好友关系独立记录，不是计数门槛，群成员数不是客户数。在 workspace 内按稳定 QQ identity 或可审计人工确认去重；昵称不构成合并依据，不以跨 workspace 全局身份库为首期前置条件。跨群/账号保留多条归因，不能把贡献数相加当客户总数。

区分当前有效意向客户、历史确认记录/人数、撤销/纠错及好友关系；生命周期与当前计数分开。纠错追加，不抹历史。证据关联 customer/workspace/account/conversation/message/event、rule/model/skill version、decision/review/correction/attribution；模型理由和置信度是判断，不能当客户自述。

stop-contact 保留 workspace/account/customer 范围、原因、审计及撤销屏障；建任务/草稿和最终 dispatch 都检查，撤销未执行授权并禁止换账号、群或动作绕过。已提交/UNKNOWN 保留真实状态。原文、摘要、图引用、日志、导出和备份按法域及业务目的确定最小化、保留、脱敏/删除、证据失效与备份清除；不臆造统一期限，不绕过 `operator_message` append-only 触发器，tombstone 不等于原文已删除。

Dashboard 沿用已确认布局，显示账号在线/离线/异常/总数和去重意向客户；不引入流量交互、活动流、发现及流量状态码。指标须有来源及新鲜度，unknown/stale 不能显示成在线或离线，UNKNOWN 不计成功，不填演示数字。通知随消息/任务/客户事件逐项接线，要求 scope、去重、分页 cursor、已读并发安全、脱敏及明确 local_only；不得提前宣称全部事件已通知。

运行约束见[发布运行手册](../product-release-runbook.md)，契约与双对话兼容细节见[文档入口](../README.md)，能力与验收见[产品验收矩阵](../product-acceptance-matrix.md)。本节未授权任何实现或真实调用。
