# ADR: BBA 任务/事实图与 ARTEX 双图映射（受控只读、无源码复用）

> **2026-11-05 后续适用范围：** 本文“只读、无源码复用、无运行时依赖”和 intent-only 决定只记录 2026-10-29 切片；S1–S6 的阶段实现见[2026-10-31 收口 ADR](bba-final-closeout-2026-10-31.md)。后续 ARTEX 核心编排与复用调查适用其[2026-11-05 补充](bba-final-closeout-2026-10-31.md#后续适用规则2026-11-05)及根 [AGENTS.md](../../AGENTS.md)，取代把本历史集成形态当成长期产品上限的解释。原上游版本、dirty 指纹、许可证与验证事实不变；不据此声称已运行 ARTEX planner。

- 状态：历史记录（2026-10-29 阶段边界）；当前实现与验收口径由 `docs/adr/bba-final-closeout-2026-10-31.md` 取代。本文保留当时的未授权、只读检查证据，不描述当前 BBA GraphSync 状态。
- 日期：2026-10-29
- 范围：BBA `core.task*` 执行真相、BBA 派生图投影、ARTEX 事实/任务图、受限提案纵切
- 非范围：ARTEX 源码复用、ARTEX 运行时依赖、真实 ARTEX 连接、真实 QQ 动作、生产 writer 切换

## 1. 背景与当前事实

用户要求 ARTEX 双图；本轮同时要求不照搬 ARTEX 源码、不建立第二套任务真相。Astra 02 文档已经指出 `core.task_graph_root`
与设计文本中的 `task_graph_roots` 命名/职责不同，要求先写映射 ADR，而不是为统一名字删表或造第二套真相
本条只记录当时的阶段边界；当前实现已由 `backend/internal/taskengine/graph_sync.go`、`graph_sync_worker.go`、`graph_sync_plan.go`、迁移 `0011_artex_sync.sql` 和 `docs/adr/bba-final-closeout-2026-10-31.md` 另行说明。

### 1.1 本地 ARTEX 事实（只读检查）

| 事实 | 值 | 证据 |
|---|---|---|
| 路径 | `/run/media/sun/6F53FE6DFAA8CECF/ARTEX` | 本机目录 |
| 版本 | tag `v0.3.23`；HEAD `620ca5d96c33a417b08fd926e04df1e2b61ffa6d`（2026-10-20） | `git describe --tags`；`git log -1` |
| 许可证 | `AGPL-3.0` | `ARTEX/LICENSE:1` |
| 语言/形态 | Go 后端 + Next.js 前端；模块 `github.com/Autumn-27/artex` | `ARTEX/go.mod:1`；README |
| 工作树 | 检查时 dirty：`server/server.go`、`db/db.go`、`agent/tools.go` 等 12 项已修改，另有未跟踪文件 | `git status --short`（只读） |
| 图模型 | `explorations` / `exploration_nodes` / `exploration_edges` / `exploration_anchors` | `ARTEX/db/schema.sql:153,170,248,276` |
| 任务模型 | `tasks(exploration_id UNIQUE)`、`task_relations`、`task_asset_links`、`task_scope` | `ARTEX/db/schema.sql:505,637,649,739` |
| API | REST（Go `net/http` mux），如 `/api/tasks`、`/api/tasks/{id}`、`/api/tasks/{id}/control`、`/api/exploration/graph`、`/api/exploration/nodes`、`/api/tasks/{id}/coverage-graph`、`/api/assets` | `ARTEX/server/server.go:727,728,748,767,792,806,845,846`；`server/server.go:2630` |
| MCP | ARTEX 是 MCP **客户端/宿主**：注册外部 MCP server、发现/缓存工具；未发现向外部暴露自身图/任务的 MCP server | `ARTEX/server/mcpdiscover.go:24,30`；`ARTEX/mcphttp/client.go:60`；静态检查无 `mcp.NewServer` |

源文件指纹（检查时工作树，不是 tag 归档；dirty 时不可当作发布指纹）：

| 文件 | SHA-256 |
|---|---|
| `LICENSE` | `0d96a4ff68ad6d4b6f1f30f713b18d5184912ba8dd389f86aa7710db079abcb0` |
| `go.mod` | `d71cc49946c74b6db98969e24735120cb4822f2b760029a4cc5dd41f2b6f6939` |
| `db/schema.sql` | `8dea552974b76e181d8246761ad2f9598e983a537e5aa9d5463675cfe21e854e` |
| `server/server.go` | `0bc6d2c5e741229f4ae18f2183e07f5185c653b781899589e883231709b28db2` |
| `mcphttp/client.go` | `f24df405715cf29c333671215f05837334be04a226e5aba5d86522ca17947ebf` |
| `server/mcpdiscover.go` | `7fddf6feb93f36a6e50cdbaad7be52df0d597ba481f45bdefeb41c167785012b` |

### 1.2 许可证边界

ARTEX 是 AGPL-3.0 网络服务软件。复制其源码进 BBA，或把 BBA 作为修改版网络服务提供，都会触发 AGPL 的对应义务。
本 ADR 的工程决策是：**不复制、不 vendoring、不链接 ARTEX 源码，不把 ARTEX 作为 BBA 运行时依赖**；只以独立进程的
REST/事件边界做领域无关映射。这里不给出法律结论；需要分发/对外服务前由负责人做法务确认。

## 2. 决策

1. **PostgreSQL Task Engine 是唯一执行/结果真相**：`core.task`、`core.execution`、`core.result`、`core.inbox`、
   `core.outbox` 的写入权只在 BBA 内核；ARTEX 和 BBA 图投影都不是任务真相。
2. **BBA 图投影是派生读数**：`core.task_graph_*` 可由内核状态重建，不是第二真相；允许删除/重建，不允许其反向授权执行。
3. **ARTEX 图是外部只读事实源**：其 `explorations/exploration_nodes/exploration_edges/exploration_anchors`
   由 ARTEX 自己拥有；BBA 只能通过 REST 只读拉取并落为带来源的 BBA 事实，不能直接改写 ARTEX，也不能把 ARTEX 节点当作
   BBA 任务状态。真实连接需单独授权；现有 client 仅通过注入 transport 的 GET graph/nodes fixture 验证，不落 BBA 事实、不建立双图关联。
4. **提案是 BBA 自己的有界内核对象**，不是 ARTEX intent 镜像，也不由 ARTEX planner 直接创建。应用仅记录 intent，
   不入队、不触发执行。既有任务调度独立运行；外部动作仍经 claim/permit/bridge 最后发送门。
5. **命名不迁移**：保留 `core.task_graph_root`（单数、每 task 一行）；设计文本 `task_graph_roots` 是旧口径。
   不创建第二张根表，不建同名视图伪装统一。
6. **历史 `task_graph_roots.bot_id` 不是当前表**：`docs/p0-14-four-account-2026-10-26.md:119` 描述的是历史系统字段；
   当前 BBA 图根没有 `bot_id`，账号归属由图 anchor 表达。

## 3. 所有权与映射

### 3.1 权威矩阵

| 领域 | 唯一所有者 | BBA 读写 | ARTEX 读写 | 冲突时 |
|---|---|---|---|---|
| 任务生命周期/预算/批准 | BBA `core.task` | 读写 | 不写 | BBA 胜 |
| 执行/结果/外部未决 | BBA `core.execution`/`core.result`/`core.inbox` | 读写 | 不写 | BBA 胜 |
| BBA 派生任务图 | BBA `core.task_graph_*` | 读；受控 lazy 投影写 | 不写 | 可由内核重建 |
| ARTEX 事实/任务图 | ARTEX `explorations/exploration_*` | 只读镜像（后续） | 读写（ARTEX 自己） | ARTEX 胜（仅其域） |
| QQ 账号/群、发送许可 | BBA | 读写 | 不写 | BBA 胜 |
| 提案/批准/应用 | BBA `core.plan_proposal`/`core.plan_application` | 读写 | 不写 | BBA 胜 |

### 3.2 字段级映射

| BBA | ARTEX | 映射规则 | 版本/失效 |
|---|---|---|---|
| `core.task.id`（text） | `tasks.id`（bigint）+ `tasks.exploration_id` | 不共用 ID；未来用 `(workspace, external_system='artex', external_id)` 映射行，external_id 唯一 | BBA 任务删除/重建时映射失效；不得用 ARTEX id 回写 BBA 主键 |
| `core.task.state`（DRAFT…FAILED） | `tasks.status`（created/running/paused/done/failed/timeout） | 只做显示映射；BBA 状态机为权威 | 不做双向状态同步；ARTEX 状态变化只落事实 |
| `core.task.version` / `scope_revision` | 无对应 CAS 列 | 不映射；BBA 独有。ARTEX `updated_at` 不能当 BBA 版本 | 版本冲突只在 BBA 内部判定 |
| `core.task_event` | `activity` / `exploration_nodes` | `task_event.id` 作为 `source_event_id` 写入 ARTEX 节点 payload（未来写适配器） | 以 BBA event seq 为幂等键 |
| `core.task_graph_root` | `explorations` | 1 task ↔ 1 exploration；`root_id` 是 BBA 确定性哈希，不是 ARTEX id | ARTEX `exploration_id` 变化视为外部重建，只更新映射与事实 |
| `core.task_graph_node(kind='goal')` | `exploration_nodes(kind='goal')` | 语义近似；BBA goal 数/成功条件更严格 | ARTEX goal state 不改变 BBA goal |
| `core.task_graph_node(kind='task')` | `tasks` 行 | BBA 内部节点，无 ARTEX 节点等价物 | 不写入 ARTEX |
| `core.task_graph_node(kind='account'/'target')` | `assets` / `exploration_anchors` | **不一一映射**：ARTEX asset 类型只有域名/IP/子域/应用/服务/端点，没有 QQ 账号/群类型 | 无原生资产时，只把外部引用写入节点 payload，不伪造 asset |
| `core.task_graph_node(kind='fact')` | `exploration_nodes(kind='fact'/'finding'/'digest')` | 按 `fact_kind` 与来源映射；模型摘要只作带来源模型产物，不升级为确定事实 | ARTEX `content_version` 只作为外部新鲜度提示 |
| `core.task_graph_anchor(account)` | 无 | BBA 自有；不写 `exploration_anchors` | — |
| `core.task_graph_anchor(target)` | `exploration_anchors(node_id, asset_id)` | 仅当存在真实 ARTEX asset 时才建立；QQ 群没有 asset 行 | 资产缺失 → 保留 BBA anchor，禁止造 asset |
| `core.task_graph_edge` | `exploration_edges` | 关系映射：`has_goal`→`spawns`，`supports_result`→`proves`，`observes_membership`→`derived_from`（仅显示） | 关系种类以 BBA 为准，不用 ARTEX 关系反向驱动 |
| `core.task_graph_snapshot` | 无 | BBA 快照/摘要，ARTEX 无等价物 | 不推送快照写 ARTEX |
| `core.plan_proposal` | `exploration_nodes(kind='intent')` | **只做概念映射**：BBA 提案是待批准建议；ARTEX intent 是其 planner 工作项 | 不共享状态枚举，不互相写入 |
| `core.plan_application` | 无 | BBA 内核应用记录；ARTEX 无对应表 | 应用 ID 可作为未来 ARTEX 事实的幂等键 |

### 3.3 `core.task_graph_root` 与 `task_graph_roots`

当前表（`backend/internal/taskengine/migrations/0005_graph_projection.sql:1`）是：

```
core.task_graph_root(workspace_id, task_id, root_id, graph_revision, task_version,
                     scope_revision, snapshot_digest, projected_at)
PRIMARY KEY(workspace_id, task_id)
```

- 每 task 恰好一个 root，`root_id = graph_<sha256(workspace\0task\0"root")>`。
- `graph_revision` 是投影代数，不是 ARTEX 版本。
- `task_version` / `scope_revision` 是投影时的 BBA 快照坐标，用于判断滞后。
- 设计文本 `task_graph_roots` 的复数/多根语义不在当前实现中；本阶段**不改表名**。若未来需要多根，必须新建带
  `root_kind` 的迁移并写清迁移/回退，不得直接重命名现有表。

## 4. 版本、checkpoint、幂等

### 4.1 BBA 内部

- `task.version`：控制/导入/提案应用等内核变更单调递增；冲突返回 `TASK_VERSION_CONFLICT`。
- `task.scope_revision`：范围变更（账号/目标/批准）时必须递增；提案创建/批准/应用都校验。
- 提案：`expected_task_version + scope_revision + sha256(PlanProposal) + expires_at` 四元组绑定；应用还要求
  `proposal.state='approved'`、`approval_ref` 仍有效、`plan_application(proposal_id)` 唯一、`(task_id,task_version)` 唯一。
- 幂等：HTTP 用 `core.idempotency`（workspace+principal+method+path+key），同 key 不同 body → `IDEMPOTENCY_CONFLICT`；
  应用本身由 `core.plan_application` 唯一约束二次兜底。

### 4.2 ARTEX 外部同步（设计，未实现）

ARTEX 没有请求幂等表，只有 `exploration_edges` / `task_asset_links` 的主键去重。因此：

- BBA→ARTEX 的写适配器必须以 BBA 稳定键（`task_event.id` 或 `plan_application.id`）作为外部 key，先查后写；
  遇到超时/未知只标 `unknown`，不盲目重试。
- ARTEX→BBA 的读适配器 checkpoint 至少包含：
  `external_task_id, last_node_id, last_content_version, last_updated_at, last_digest, last_success_at`。
- 每次拉取先按 `exploration_nodes.id + content_version` 去重，再落 BBA 事实；重复拉取不得新建事实。
- ARTEX 不可用时，BBA 使用已存投影/事实并显示滞后；**不得旁路执行**。取消后的晚到观察只更新事实和审计。

## 5. 图读取的写放大（受控 lazy projection）

当前 `GET /v1/tasks/{id}/graph` 的调用链是：

`GetTaskGraph`（`backend/internal/taskengine/graph.go:369`）
→ `projectTaskGraphTx`（`graph.go:110`）
→ `lockTask(..., FOR UPDATE)` + 写 `core.task_graph_root`/`node`/`fact`/`edge`/`snapshot`。

因此：**这不是数据库零写读接口**。本阶段决策：

- 接受为“受控 lazy projection”，仅用于开发/只读控制台；不得在文档或 UI 中称其为纯读。
- 权限边界：必须 `task.read` + `authorizeTask`；runtime actor 禁止；跨 principal 需 `task.manage_all`。
- 性能边界：GET 与任务控制共用 task 行锁，任务级串行；单 task 的投影写量与节点/事实数成正比。UI 需限频，不做轮询刷新。
- 后续迁移：把投影构建移到事件消费/worker，GET 只读现有投影并返回 `projected_at`/滞后状态；迁移前需 ADR 更新。

## 6. 最小提案纵切（本 ADR 配套实现）

实现文件：`backend/internal/taskengine/proposal.go`、`proposal_http.go`、`proposal_test.go`、`migrations/0006_plan_proposals.sql`。
固定模板 `controlled_participation_v1`：从当前任务已导入、未覆盖、无 active claim、无 retained hold、无有效 result 的目标中选最低
`external_id`，再从满足身份/在线/配额/baseline 条件的账号中选最低 id，形成一个 `request_join` 步骤。

状态机：

`pending --approve--> approved --apply--> applied`
`pending/approved --expire/reject--> expired/rejected`（终态；本阶段只实现 approve/apply）

边界：

- 创建：只接受 `use_template`；校验 task version/scope、任务状态、deadline、live approval、外部尝试预算、goal slot、目标/账号资格。
- 读取：`GET /v1/tasks/{id}/plan-proposals/{proposal_id}` 返回状态与 digest，供操作者审阅后批准/应用；不改变版本。
- 批准：`task.approve`；绑定现有 `core.approval` 行；不改变 task version；不发送。
- 应用：`task.control`；要求已批准、digest/approval 精确匹配、未过期、version/scope 未变、步骤重新校验、预算仍开放；
  写 `core.plan_application` + `plan.proposal.applied` 事件 + task version+1；**不写** work item/execution/permit/claim/usage/bridge。
- 防重规划：若已有 application，创建新提案还要求该 application 之后出现新的 observed execution
  （`core.execution.observed_at`）、trusted result（`core.result.confirmed_at`）或任务账号/目标上的 membership
  观察（`core.membership.observed_at`）。应用本身不是新证据，因此不能靠反复 apply 版本推进无限重规划。
- 外部动作：既有任务调度独立执行 `Reserve`→bridge→最后发送门，worker 不消费 application。Proposal→step→execution/result 的关联与消费者**未实现**；apply 后紧接 Tick 或 account/target 相同的图事实都不能证明提案驱动执行。

## 7. 后果与未决

1. 真实 ARTEX 连接未授权：只读 client 仅做离线 GET fixture；双图事实摄取、外部 anchor/checkpoint 和写适配未实现。Live REST 调用需单独范围/账号/时限授权。
2. ARTEX 工作树 dirty：指纹只代表本机检查时；发布对接必须以 tag/commit 归档重新取指纹。
3. QQ 账号/群没有 ARTEX asset 类型：不做伪造 asset；anchor 映射只覆盖真实可映射的资产，其余留 BBA 自有 anchor。
4. BBA 图 GET 仍写库：本阶段的受控 lazy projection 是显式接受的妥协，不是最终形态。
5. 提案目前只有固定模板和一个 `request_join` 步骤；扩展步骤种类需新的 schema/模板版本和本 ADR 更新。
6. OpenAPI 已登记 proposal create/get/approve/apply 及读取 basis；create/apply 仍标为 `x-feature-enabled-in-p0: false`。第四轮明确 apply 的 intent-only/no-consumer 语义，不改 DTO 形状，不宣称生产发布。
7. 不存在 `task_graph_roots` 到 `core.task_graph_root` 的自动迁移；名称差异保留在文档中，避免制造第二套任务真相。
8. `cmd/taskengine-server/main.go` 在显式 `-development-operations` 下挂载 `ProposalHandler`；`resolver.go` 同时只为该模式放行 proposal create/get/approve/apply 路由。默认模式不暴露这些操作，且 actor 仍须通过 operator 身份与对应 task 权限校验。开发控制台的 tab-local 开关只控制浏览器是否发起请求，不替代服务端 flag、鉴权或 allowlist。
   服务端 flag、鉴权和 allowlist 仍是边界；浏览器开关不授予额外权限。
9. 当前 schema 头部为 18（`0008` 写 17，`0009_worker_cursor.sql` 写 18）；`serverMinimumSchema=18` 覆盖 worker 持久轮转所需表。启动只校验不迁移；隔离验证显式初始化新库，生产迁移未执行。
10. 模型网关 `validateModelProposal` 仍是 clarification-only（`model_http.go`）；本纵切没有放松它，提案由确定性固定模板生成，
    不是模型步骤提案。需要模型生成步骤时必须新增独立验证器/合约，不能直接放开现有澄清校验。
