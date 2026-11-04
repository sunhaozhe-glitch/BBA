# BBA 仓库级 Agent 开发与交接指南

> 本文件是仓库级开发规则和架构约束的首要入口。**规则是约束，源码是实现事实，测试记录只证明其声明范围。** 接口以 `contracts/openapi.yaml` 为准，数据结构以 migration 为准，架构决策查 `docs/adr/`；发现冲突应记录和解决，不能把设计目标当成已实现能力。
>
> 本次源码梳理基线：2026-11-04，`origin/main` 提交 `5e44e86`。这是代码与文档核对基线，不是运行实例、独立验收或生产发布证明。后续开发须重新检查当前分支、装配、配置和证据。

## 1. 项目概述、技术栈与目录

BBA 面向 QQ AI 运营：统一管理账号、群聊和客户，由 Agent 分析信息、提出计划，在服务端授权和审批约束下执行动作，并保留消息、任务、许可与结果证据。当前底座是 Go Task Engine + PostgreSQL + 同源控制台，另有 Koishi 交互/采集运行时和独立 Python 研发 Controller。计划任务、CRM、完整多步运营 Agent 和产品 MCP 管理仍需建设或验收，不能由菜单存在推断完成。

### 1.1 技术栈与依赖来源

| 层 | 已核实实现 | 版本与依赖的维护入口 |
|---|---|---|
| 后端 | Go、标准库 `net/http`、pgx/v5、直接 SQL；Go embed 打包静态资源 | `backend/go.mod`、`backend/go.sum`；当前声明 Go 1.26.0 |
| 业务数据库 | PostgreSQL；`core` 与 `bridge` schema；显式 SQL migration | `backend/internal/taskengine/migrations/`；隔离 harness 使用 PostgreSQL 16 |
| 产品前端 | 原生 HTML/CSS/JavaScript，无独立 React/Vite 应用；Python 复制/生成资源 | `frontend/`、`tools/build-product.py` |
| HTTP 契约 | OpenAPI 3.0.3、JSON Schema/fixtures、Redocly、openapi-typescript、oapi-codegen、Go nullable | 根 `package.json`/lockfile、`tools/go.mod`、`tools/oapi-codegen.yaml`；根 Node engines 为 >=22.12.0 |
| QQ 交互/采集 | TypeScript strict + NodeNext、Koishi、ChatLuna；两个独立 npm 包 | 各自 `package.json`/lockfile/tsconfig；Node 22，Koishi 4.18.11、ChatLuna 1.4.4，插件版本独立固定 |
| 研发 Controller | Python >=3.11、SQLite、asyncio、stdio MCP、ACP 子进程协议 | `controller/pyproject.toml`；当前固定 `mcp==2.3.0`、`jsonschema==4.26.0`；CI 使用 Python 3.14 |
| 开发验证 | Bash/Python、Docker 隔离 PostgreSQL/Go/Node、Python jsonschema/PyYAML、按需浏览器脚本 | `tools/`、`tests/`、`.github/workflows/` |

根 npm 包是**契约工具包**，不统一安装两个 Koishi 包，也不构建 Controller。准确版本始终读 manifest/lockfile；不把本机已安装版本、历史测试环境或模型名称当成部署事实。

### 1.2 目录导航

| 路径 | 职责与编辑边界 |
|---|---|
| `backend/cmd/taskengine-server/` | 主服务装配、鉴权 resolver、API surface、`/product/` 产品 UI 与 `/` 开发 Console |
| `backend/cmd/task-bridge/` | 独立 loopback 网关，校验许可/身份，调用 NapCat/OneBot adapter |
| `backend/cmd/taskengine-migrate/` | 显式初始化、检查和升级数据库 |
| `backend/cmd/bba-operator/` | 操作员 CLI；其余 `cmd/p0-*`、`kernel-smoke`、`product-fixture` 是阶段工具/验证入口，不能当默认生产启动器 |
| `backend/internal/taskengine/` | Task、Reserve、Bridge 状态、Chat Permit、模型/配置、内容规则、对话、通知与恢复 |
| `backend/internal/identity/`、`backend/internal/artex/` | runtime 身份 read-back；有界 ARTEX HTTP adapter |
| `contracts/openapi.yaml`、`contracts/schemas/`、`contracts/examples/` | HTTP/plugin wire contract、正反 fixtures |
| `backend/internal/contract/`、`koishi/contracts/` | 生成类型及契约消费者；不手改 `types.gen.*` |
| `frontend/` | 唯一产品源码，含 `NOTICE`、`licenses/`、工具目录输入 `napcat-initial.txt` |
| `backend/cmd/taskengine-server/product/` | 生成的 embed 副本；`web/` 是保留的 Go 开发 Console，不是新产品源码 |
| `koishi/character-runtime/`、`koishi/analysis-collector/` | 账号隔离的交互运行时；collection-only 采集与 Summary |
| `controller/` | 独立研发任务 Controller；其 `schemas/` 是 MCP 合同，不能混入 QQ HTTP 执行语义 |
| `plugins/napcat-plugin-cleaner/`、`patches/` | NapCat 适配候选及补丁；源码存在不证明已加载或可用 |
| `tools/`、`tests/`、`docs/`、`.github/` | 构建/harness、契约及插件测试、ADR/验收/运维文档、CI |
| `build/`、`.cache/`、`node_modules/`、`evals/results/` | 本地生成物/依赖/证据，受 `.gitignore` 管理，不提交 |

接手顺序：本文件 → 相关 ADR/能力矩阵 → `taskengine-server/main.go` 和 `resolver.go` → 具体 handler/存储方法 → migration/contract → 前端或 runtime 消费者 → 对应测试。编辑前用 `rg --files -g AGENTS.md` 重新盘点规则作用域；当前跟踪入口只有根文件。

## 2. 不可违反的架构原则

1. **增量演进。** 保留 Go Task Engine、PostgreSQL、Task Bridge、OpenAPI、Go Console、Character、Collector 和 ARTEX adapter/outbox；先复用接线，有证据证明无法满足需求才提出替换。
2. **QQ 业务任务只有一个执行内核。** 计划必须物化现有 Task，复用 Reserve/许可/恢复。独立研发 Controller 的 SQLite 队列不是 QQ 任务或第二套业务 Reserve，禁止由其绕过 Go 权限链。
3. **日常 Chat Permit 独立。** 发放/消费不创建 task、subtask、goal slot、execution、target claim 或 dispatch permit；聊天任务化必须另行设计桥接、审批、幂等和恢复。
4. **Agent 不是权限主体。** 模型、Prompt、Skill、搜索、QQ 消息、Summary、CRM 证据和 ARTEX 图均不能授予 QQ 权限；服务器按 workspace/principal/account/action family/能力/策略/审批/当前状态裁决。
5. **持久事实分域。** PostgreSQL 是 QQ 业务与执行事实来源；受保护 JSON registry 是配置及其配置回执来源；Controller SQLite 只记录研发任务。日志、内存、UI、模型上下文和外部图不能替代各域持久事实，也不能证明外部效果真实成功。
6. **不确定副作用保守处理。** `submitted`、`pending`、`unknown` 不等于成功；超时/断连/崩溃后不自动重发、不换账号、不释放可能占用的 claim/slot/预算，只读核对或人工处理。
7. **能力未知即 fail closed。** 工具目录、Schema、enabled 配置、端口连通、fixture 成功都不是 runtime 能力证明。区分 Tool Catalog、Tool Schema、Runtime Capability、Tool Authorization。
8. **边界不可省略。** workspace/account/conversation/identity 隔离；mutation 要有授权、版本/fence、幂等与审计；QQ 写要有动作族单次许可，其他外部写遵循各自 outbox/checkpoint/reconcile 协议。
9. **唯一产品前端。** 只从 `frontend/` 修改并同步 embed；不得另建控制台或只改生成副本。
10. **许可证和来源必须保留。** 读 `frontend/NOTICE` 与 `frontend/licenses/`；ARTEX 来源同时涉及 AGPLv3/MIT 边界，不能宣称整个仓库 MIT，也不能复制其领域模型或调度器替代 QQ 内核。

## 3. 核心模块、接口与数据流

下表中未带目录的 Go 文件均位于 `backend/internal/taskengine/`。定义了 handler 不代表主服务允许访问；修改接口必须同时核对 `backend/cmd/taskengine-server/main.go`、`resolver.go`、`product.go`、HTTP handler 和消费者。

### 3.1 业务执行内核

| 模块 | 源码定位 | 关键约定 |
|---|---|---|
| Store/错误/身份 | `base.go`、`heartbeat.go`、`heartbeat_http.go`、`backend/internal/identity/` | pgxpool + 显式事务；会话使用 UTC；业务错误经 `Code` 映射；账号身份靠 runtime read-back |
| Task/API | `task.go`、`task_http.go`、`p019.go` | 创建、查询、控制、候选导入/baseline；权限及幂等检查不能只放在 UI |
| Reserve/资源 | `scheduler.go`、`lease_budget.go`、`task_lock.go` | 事务内重新裁决；`scheduler.go` 是 dispatch 资源预留，不是日历计划任务系统 |
| Bridge/观察 | `bridge.go`、`bridge_runtime_http.go`、`observations.go`、`deadline_sweep.go` | 不可变 payload/digest、单次许可、outbox→inbox、结果归约、UNKNOWN 核对 |
| 连续 Worker | `worker.go`、`worker_steps.go`、`worker_proposal.go`、`worker_proposal_recovery.go` | 有界扫描和持久 cursor；任务协调使用 session advisory lock，仍须 epoch/fence/version 校验 |
| Proposal/DAG | `proposal.go`、`proposal_work.go`、`proposal_http.go`、`proposal_work_http.go` | Proposal→持久 Work Item→现有 Reserve；materialize 不调用外部 adapter |
| 内容规则 | `content_rules.go`、`content_guard.go`、`content_http.go` | 规则版本、历史、审核回执；最终 payload 的规则检查不能被早期审核替代 |
| ARTEX | `graph.go`、`graph_sync*.go`、`backend/internal/artex/` | 本地图投影与外部同步分层；outbox/checkpoint；不确定写走 reconcile |

```text
Operator / 前端 / CLI
  → resolver（主体、workspace、permission）→ Task / candidate / baseline / approval
  → Reserve 事务（execution、goal slot、claim、budget、outbox）
  → Bridge（payload digest、runtime identity、owner epoch/fence、单次 permit）
  → adapter → observation / bridge outbox → core inbox reducer → result / Task 投影
```

- Reserve 锁顺序：task → target/claim → account/usage（账号排序）→ subtask/execution/slot；事务中不得外呼网络。
- 一个 permit 最多一次 adapter call。保持严格目标数和最终 membership 检查，不把申请已提交视为已入群；已有成员 baseline 不算新成功。
- Worker 的协调锁不等于生产单 writer 迁移协议。账号 + action family 始终只有一个有效 writer，陈旧 owner/fence 必须被拒绝。
- **当前主服务 `-worker` 固定使用 `WorkerFixtureTransport`**，`worker_steps.go` 内是模拟发送。开启 worker/proposal 开关不等于接通真实 QQ Bridge；有真实网关源码也不能推断连续 Worker 已走真实网关。

### 3.2 Character 与 Collector

```text
可信 QQ trigger → 账号/runtime/context/active-speaker 检查
  → Character 生成文本 → exact text digest + durable Chat Permit
  → Task Bridge / 单次 text adapter → sent 或 unknown outcome

Collector 采集 → 持久窗口 → Summary producer/publisher
  → Go Summary 校验/存储 → 不可信 GroupContext → 分析/规划/Character 上下文
```

- Go：`chat_trigger.go`、`chat_admin.go`、`chat_permit.go`、`chat_permit_http.go`、`chat_http.go`；`summary.go`、`summary_http.go`、`group_report_http.go`。
- Character：`koishi/character-runtime/src/runtime.ts` 管账号 App 生命周期；`ingress.ts`/`gate.ts`/`transport.ts`/`ledger.ts` 约束入站、出站和重放；`backend.ts` 绑定后台协议；`durable-db.ts` 提供持久文件数据库；`group-context*.ts` 校验上下文。
- runtime、database、cache、persona、adapter 按账号隔离；配置入口是 `src/types.ts` 的 manager/account options，不假设存在通用环境变量加载器。当前 writer 合同只允许受控文本，不擅自扩展多段/媒体。
- permit 已消费但无权威 outcome 时是 unresolved/unknown，禁止重新发放同一动作许可。
- Collector：`src/runtime.ts`/`config.ts`/`boundary.ts` 强制 collection-only；`summary-producer.ts`/`summary-publisher.ts` 生产发布 Summary；`semantic-analysis.ts` 有 fixture/provider 分析接口，存在接口不证明真实模型已接通。
- Collector 独立持久目录/账号绑定，不执行 command/chat dispatch 或未经授权的模型/消息调用。Summary 必须验证 account/group/window/version/expiry 后才能进入 GroupContext，且始终是不可信输入。

### 3.3 运营 Agent、模型与配置

- `model_http.go` 持久化模型请求、校验输出并沿既有 Proposal 通路处理；`model_provider.go` 是有界的兼容 chat-completions provider。不得复制第二条模型执行链。
- `model_registry.go`、`model_registry_lock.go` 是 opt-in 文件 registry；`agent_config*.go` 管 Agent/Skill 版本和绑定，`agent_model_http.go` 固定配置快照并生成回执；`web_search*.go`、`global_proxy*.go` 共用 registry 的 workspace 配置。
- registry 使用受保护 JSON、进程生命周期 flock 和持久化协议；含密钥，不能提交或直接暴露给浏览器。它不是 PostgreSQL Task 状态、CRM 或通用 Agent run 数据库。数据库先登记模型请求，bound provider 再保存文件回执，两者不是跨存储原子事务；崩溃后不能假定自动恢复完成。
- 当前 Agent/Skill 的 `tools` 只能为空，Skill 能力只接受 `structured_output`；模型上下文明确没有 callable tools。配置字段存在不代表能注册任意工具；相关校验见 `agent_config.go` 和 `agent_model_http.go`。
- Agent/Skill/Model CRUD、模型请求、搜索上下文已有实现；**尚不能据此认定 thread→持久多步 run/step→工具授权→停止/恢复的完整运营 Agent 已交付**。QQ 入站和操作员 Agent 消息属于不同信任域。
- 完整 MCP 平台不是生成 Proposal 的前置条件；真正执行某个动作前，该动作的最小 adapter、能力、授权和结果证据必须就绪。

### 3.4 HTTP 入口与接线条件

| 路由族（省略资源 ID） | 装配/实现 | 实际边界 |
|---|---|---|
| `/v1/accounts`、`/v1/tasks`、任务 controls/reconcile | `resolver.go` 的 `taskEngineSurface`、`task_http.go` | 默认以读取和受控 reconcile 为主；任务写入口受 `-development-operations` 限制 |
| 任务 plan-proposals/orchestration/materialize/work-items/graph-sync | `serverAPISurface` + 对应 handler | Proposal 接受、物化、执行分别核对开关、scope、版本和审批 |
| `/v1/model-requests`、`/v1/agent-model-requests` | `model_http.go`、`agent_model_http.go` | 后者随 registry 装配；模型产生建议不授予 QQ 权限 |
| `/v1/operator/models`、`/agents`、`/skills` | `main.go` 的 registry wrapper | 仅配置 `-model-registry-file` 时装配，不能只凭 OpenAPI 判断启用 |
| `/v1/operator/conversations`、`/agent-threads/.../messages`、`/qq-drafts`、`/notifications` | `OperatorConversationHandler`，已在主服务装配 | 消息持久化、查询、草稿审核、本地通知；草稿 handler 不调用 QQ writer |
| `/v1/operator/qq-inbox/events` | `operatorConversationAuth` + `operator_conversation.go` | 路径虽叫 operator，写入仍仅信任 runtime 身份；operator token 不能伪造 QQ 入站 |
| `/v1/internal/group-summaries`、`/group-context`、`/chat-triggers`、`/chat-context`、`/chat-send-permits` | 主服务 runtime handlers | runtime credential/account scope；permit 子操作按 handler 核对 |
| `/v1/internal/bridge/` 与 Chat Permit 网关操作 | 独立 `task-bridge/main.go`、`gateway.go` | 与主服务不是同一监听器；许可校验后才接 adapter |
| `/v1/operator/dashboard`、`/tools`、`/runtime` | `product.go`、`dashboard.go`、`product_runtime.go` | dashboard 来源有可用性；tools 是静态未验证目录；runtime 返回运行配置边界 |
| `/v1/operator/system/`、content-rules/content-reviews、login 相关路由 | settings/content/login handlers | 代理、搜索依赖 registry；work slots 在 PostgreSQL；登录取决于显式 provider |

这里是定位图，不复制完整 API。逐操作的 method、请求字段、权限名、错误码、分页和幂等规则读 OpenAPI、对应 handler/tests。内部实现的 API 比主服务实际暴露范围更大；`x-feature-enabled-in-p0` 不是装配证明。

QQ inbox 的 runtime 身份由 `operatorConversationAuth` 传递可信 `OwnerEpoch`，事务按 account→credential 持共享锁，再复用 `authorizeRuntime`；fresh 写入与 replay 均须先通过当前 owner、active、账号 scope 和 token/account/credential epoch 校验。自定义 auth 必须传入可信 token epoch，不能从数据库当前值补齐。消息读取支持内部 `Actor.AccountIDs` 范围；当前文件 resolver 不接受账号受限的 operator 配置，handler 测试不代表这种 token 已在主服务接线。

### 3.5 独立研发 Controller

依据 `controller/src/bba_controller/` 与 `docs/adr/controller-phase1.md`：这是研发编排实验组件，不连接 QQ 业务数据库，也没有把产品 MCP 页面接到 Go Task Engine。

- `__main__.py`/`server.py`：`python -m bba_controller` 的本地 stdio MCP；stdout 只承载协议。七工具 `bba_task_submit/status/wait/cancel/resume/list/evidence`，权威 Schema 是 `contracts.py`，`controller/schemas/` 导出须与其一致。
- `controller.py`：启动侧授权；默认 `OBSERVE`，修改类工具要求明确授权的隔离 fixture 仓库。`DEVELOPMENT` 需额外启动授权，但第一阶段仍不开放真实仓库施工或真实模型 prompt。禁止对 BBA 真实仓库授予 fixture 身份。
- `store.py`：SQLite 事务/CAS、request_id 幂等、依赖/写集合冲突、slot、事件/会话/证据及 `execution_cleanup`；默认 1 slot、最大 2，与 Go task work slots 无关。重启将未决执行保守恢复；任务终态不证明子进程已清理。
- `worktrees.py`：受保护仓库绑定及独立 fixture worktree；`state-dir` 与仓库目录必须互不包含，路径/祖先 symlink 拒绝；owner flock 防止双进程恢复冲突。依赖 POSIX/Linux 进程与 `/proc` 核对，不宣称通用跨平台沙箱。
- `acp.py`/`sim_peer.py`：有界 NDJSON、超时、取消、进程组清理、反向权限拒绝；真实 ACP 仅有受限元数据接口，真实 prompt 拒绝，UNKNOWN 不自动重放。
- `models.py`/`model_contracts.py`：独立的模拟 profile/policy 快照和 simulation-credit 预算，不是货币/token 精确计费；本地 simulation 校验、调用预留/结算、UNKNOWN 保留预算、fallback deny；不等于真实 OMP/provider 路由控制。
- 不直接暴露 stdio 给不可信远程用户。原始 prompt/凭据不进证据；磁盘/SQLite 不可写时不能承诺审计成功。历史 Controller 报告的“CI 未运行”或本机证据路径应按提交重新核对。

## 4. 产品语义与后续实现边界

导航保留：仪表盘、对话、任务、计划任务、资产，以及 LLM、Agent、MCP、Skill、工具、通知推送、拦截规则、审批记录、日志、系统配置。全局代理、网络搜索、Work Agent 并发配置保留各自作用域。每个页面都要列明真实 API、持久来源、权限、空/错误/未知状态及验收证据。

### 4.1 双对话、草稿与通知

Agent thread/message 与 QQ account/group/private conversation 分域存储；消息、附件和搜索结果不可信。Agent 消息追加前须在事务中核对目标会话属于 Agent 域（kind、conversation_kind、无 QQ account）；`INSERT ... ON CONFLICT DO NOTHING` 不等于来源校验。现有合法 Agent 行允许内部 `id` 与 `conversation_id` 不同，不能用两者相等作为来源判断。当前 Agent 消息写入不自动启动多步 run；QQ 草稿审核不等于已发送。通知是持久 `local_only` 投影，不等于已推送到外部服务。

后续按消息、任务、CRM 各自事件逐项接线，要求 scope、去重、分页 cursor、已读并发安全与脱敏；不能预先宣称所有事件已有通知。基础对话闭环与完整 M5 Agent run 分开验收。

### 4.2 五种业务任务

| 类型 | 当前代码边界 |
|---|---|
| `group.acquire` | 保留既有 Task/Reserve/Bridge 链；真实能力须单独核对 runtime/adapter/live 证据 |
| `group.analyze` | 可识别、创建 DRAFT、读回；执行 unsupported，未来才接 Summary/Collector 证据 |
| `chat` | 可识别、创建 DRAFT、读回；任务执行 unsupported；日常 Chat Permit 保持独立 |
| `customer.add_me`、`customer.add_customer` | 可识别、创建 DRAFT、读回；执行 unsupported；动作语义和好友能力尚不能猜测 |

`task.go` 的类型从 canonical `core.task.request_payload` 派生，不是独立数据库 type 列。后四种只允许 dry_run/read_only，`approval_ref` 必须存在且为 null，join 文本占位为空，fixed 候选恰好一个；submit 在写入前以 `CAPABILITY_UNAVAILABLE` 拒绝。细节查 `qq03ValidateCreateRequest`、`qq03_task_types_test.go`、`docs/qq03-task-type-matrix.md`。生命周期、能力状态和外部效果分开显示；明确拒绝不能伪装成待重试。

### 4.3 计划任务（设计约束，尚不是 `scheduler.go` 已有能力）

实施前形成 Scheduler ADR，定义以下协议，再复用 Task Engine：

- occurrence 唯一键绑定 schedule ID + 规范化触发时刻；版本是快照属性，不能靠版本递增重复触发。保存 IANA timezone、本地表达式和 UTC 时刻；DST 不存在/重复本地时间须明确策略并测试。
- occurrence→Task 优先同一 PostgreSQL 事务写入事件/幂等。跨事务必须有稳定幂等键、中间状态、owner/expiry/CAS 租约、唯一约束和崩溃恢复；重跑只能读回同一个 Task。
- 定义 pending→claimed/materializing→materialized 及可重试/永久拒绝状态。默认不无限补跑；misfire 明确容忍窗口和 skip 原因，coalesce/catch-up 设时间与数量上限。
- 默认 forbid overlap；前次运行、待审批或 UNKNOWN 时不得绕过，按配置 skip/排队并记录原因。
- 编辑/暂停/删除递增 schedule version；创建 Task 前重验版本/enabled，旧 occurrence 进入 superseded/cancelled；跨版本同刻最多一个 Task。已物化 Task 必须走自身取消/审批/UNKNOWN 规则。
- 创建计划不自动授权每次外部动作。每次执行重验当前能力、授权、审批期限、预算和规则；低风险模板授权需单独风险评估。
- 覆盖领取及 Task 创建前后崩溃、多 worker、重启、时钟漂移、版本修改、审批撤销和数据库恢复。

### 4.4 CRM（产品约束，不是已交付的客户表）

- 核心是**有原始证据的明确意向**，识别成功即可计入引流成功，好友关系是独立属性，不是计数门槛；群成员数不是客户数。
- workspace 内按稳定 QQ identity 或可审计人工确认去重；不按昵称合并，不把跨群/跨账号贡献简单相加，也不把跨 workspace 全局身份库作为首期前置条件。
- 区分当前有效意向、历史确认记录/人数、撤销/纠错和好友关系；生命周期状态与当前是否计数独立，纠错追加而非抹除历史。
- 证据关联 customer/workspace/account/conversation/message/event、rule/model/skill version、decision/review/correction/attribution；模型置信度/理由是判断，不是客户自述事实。
- stop-contact 是执行约束：workspace/account/customer 范围、原因、审计和撤销屏障；建任务/草稿及最终 dispatch 均检查，撤销未执行授权并禁止换账号绕过。已 dispatch/UNKNOWN 不能假装取消。
- 不默认永久保留原文，也不臆造统一保留天数；先明确法域/业务目的、最小化采集、受控删除/脱敏/tombstone、证据失效和备份清除。tombstone 不等于删除原文；不得绕过 `operator_message` 的 append-only 触发器。
- 新持久化域同阶段验证隔离、幂等和当前 schema 的恢复，先证据链/统计口径，再复杂 UI。

### 4.5 Dashboard、审批与日志

沿用 ARTEX 布局，但账号在线/离线/异常/总数替代“确认发现”，资产用意向客户口径，不引入流量交互、活动流、发现和流量状态码。指标必须有可追溯来源与新鲜度；unknown/stale 不显示为 offline/online，UNKNOWN 不计成功，不能用演示数字填空。审批绑定 digest、scope、主体、资源、版本、期限、预算，变化后重审。日志按实际链路关联 request/thread/run/task/execution/permit/bridge operation/observation 和审计主体，禁止凭据及不必要原文。

## 5. 阶段路线与依赖

| 阶段 | 工作边界 |
|---|---|
| M0 基线 | 锁定 commit/工作树/build/schema，核对真实装配与矩阵；缺陷按 P0 安全/数据损坏/越权、P1 核心阻断、P2 功能缺口、P3 文档体验分级；不做无边界历史报告清零 |
| M1 双对话 | 在已有 handler/路由/存储上补缺，分别验收 QQ 入站/草稿/通知和 Agent 基础消息；首轮不调真实 writer；多步 run 不作为基础消息前置条件 |
| M2 账号/任务 | 复用 identity/runtime/owner epoch；逐类型验收识别、创建、读回、执行、证据，分配重验在线/能力/暂停/UNKNOWN/预算/范围 |
| M3 计划 | 先 ADR 定义 occurrence/misfire/overlap/DST/授权，再实现与恢复测试 |
| M4 CRM | 先去重、证据、纠错、stop-contact、生命周期，再 UI；同期恢复/隔离验证 |
| M5 运营 Agent | 复用 registry/provider/Proposal；补 thread→run、持久 step、版本快照、幂等、取消/超时/恢复、服务端工具裁决；首期按角色配置，不另建多套 Agent 服务 |
| M6 工具/MCP | 按业务需要接最小可验收工具，分开 Catalog/Schema/Capability/Authorization/lifecycle；研发 Controller 不代表此阶段完成 |
| M7 整合/发布 | 汇总指标、备份恢复、限流/密钥/停止/告警与发布演练；各域 Dashboard/通知随域交付，不拖到最后首次接线 |

各阶段拆为有界 PR。真实 QQ、模型、MCP、ARTEX、生产发布分别授权、分别验收，一个域的 live 证据不覆盖其他域。

## 6. 状态与证据模型

每项能力分开记录：

| 维度 | 允许的状态 |
|---|---|
| 实现 | `NOT_IMPLEMENTED` / `PARTIAL` / `IMPLEMENTED` |
| 接线 | `NOT_WIRED` / `PARTIAL` / `WIRED_TO_CONSUMER`，指明真实 consumer |
| 验证身份 | `AUTHOR_SELF_TEST` / `INDEPENDENT_SOURCE_REVIEW` / `INDEPENDENT_EXECUTION` / `NOT_REVIEWED` |
| 环境 | `UNIT` / `CONTRACT` / `ISOLATED_FIXTURE` / `CONTROLLED_LIVE` / `PRODUCTION` |
| 结论 | `PASS` / `FAIL` / `BLOCKED` / `NOT_RUN` |
| 发布 | `NOT_RELEASED` / `RELEASE_CANDIDATE` / `RELEASE_ACCEPTED` |

证据记录 commit、适用的 binary/build identity、schema/migration set、命令/退出码/原始日志、时间、执行者/审查者及方式、脱敏 scope、fixture/live 边界和未覆盖风险。未提交树另记 diff/文件摘要，提交后核对内容一致；历史未记字段不能补造。

实现≠接线，自测≠独审，fixture≠受控 live，live≠生产，CI≠发布批准。验收矩阵逐行按证据范围判断，不计算虚假的完成百分比；历史报告必须按日期/commit/schema 读取。

## 7. 数据模型、迁移与契约

### 7.1 PostgreSQL 业务域

权威 DDL 在 `backend/internal/taskengine/migrations/`，下表仅是实体关系导航；复合键/外键中 workspace/account 边界不可省略。

| 域 | 主要实体及关系 |
|---|---|
| 身份/授权 | `workspace`→`principal`/`account`；runtime credential/heartbeat、approval、account action lock |
| 任务 | `task`→`task_account`/`task_target`→target/baseline/subtask；execution 绑定任务、目标、账号、runtime、digest/fence |
| 资源/效果 | goal_slot、target_claim、account_usage、dispatch_permit；membership/result 区分既有关系与本次结果；retry 保留原尝试关联 |
| 审计/传递 | task_event 有任务内 seq；core.outbox→bridge.gateway_operation/bridge.outbox→core.inbox；idempotency 按 workspace/principal/method/path/key 约束 |
| 聊天 | chat_send_policy、group_binding、chat_trigger、chat_send_usage、chat_send_permit/outcome、专用幂等记录；独立于任务域 |
| 规划/知识 | model_request、plan_proposal/application/basis、group_summary；work_materialization/item/attempt；本地图投影与 graph_sync_outbox/checkpoints |
| 产品支持 | login_session/event、content_rule/history/review、workspace_work_settings/audit |
| 双对话 | operator_conversation→operator_message→qq_draft；operator_notification 按 workspace/持久 conversation/event_key 去重（NULL conversation 保留 workspace 范围），保存 read/version，delivery_state 约束为 local_only |

`operator_message` 只追加，禁止 UPDATE/DELETE；QQ 事件按会话/event_id 去重，消息有 seq/body_digest。当前 migration 没有独立 CRM、日历 occurrence 或运营 Agent run/step 持久化域，不得把相似名称当成已实现。

**其他持久化：** 模型/Agent/Skill/搜索/代理配置及相关回执在 `model_registry.go` 定义的受保护 JSON；Character/Collector 的账号隔离 JSON 数据文件是运行时本地状态；Controller 的 `controller.sqlite3` 有 tasks/events/submit_requests/sessions/artifacts/slots/cleanup 和独立 model policy/budget/call 表。这些状态不能相互冒充业务执行结果。

### 7.2 迁移和兼容性

1. `migrate.go` 注册 migration tail/head，`migrate_head.go` 暴露 `SchemaHead()`。**SQL 文件序号不是 schema version**；初始 migration 就写入了较高的历史版本。不要在长期指南固定“当前 head”。
2. 主服务 `validateServerSchema` 是最低版本门，Bridge `validateGatewaySchema` 要精确匹配 `SchemaHead()`，两者不同。启动门通过不证明所有新页面的数据表已齐全；部署应显式对齐本次 schema。
3. `Store.Open`/服务启动不迁移。`taskengine-migrate` 无参数仅初始化空库；`--check --expected-version N [--expect-head H]` 只读；`--upgrade --expected-version N [--expect-head H]` 显式升级，N/H 从本轮读取获得，不能照抄历史示例。
4. migration 只追加，不重编号已发布文件、不引入隐式 startup/down migration；保持事务、约束、升级/审计语义。
5. 涉库变更同时核对 migration tail/head、binary、server/Bridge gate、迁移测试、`--check`、隔离数据库实际 head；验证受支持旧版升级、当前启动、关键查询、隔离恢复及保留/删除语义。
6. 通知作用域迁移 `0017_operator_notification_scope.sql` 同时改变唯一索引与三处写入冲突目标；新旧写入 SQL 不支持跨 schema 混跑。先停止旧写入方，再显式迁移并切换匹配的 server/Bridge binary；不能只回退 binary，迁移不会补回历史已漏通知。

### 7.3 契约与编码

- `contracts/openapi.yaml` 是 HTTP/plugin 规范；先更新正反 fixtures，再 `bash tools/contract.sh check`，必要时才 `generate`。`check` 不等于全部生成类型 smoke；不手改 `types.gen.go`/`types.gen.ts`。
- `$ref` 仅本地受限引用，禁止远端、绝对路径、父目录及软链逃逸。保留 required/nullable 区别；Go nullable 用于区分缺失、null 和有值。
- QQ ID、wire epoch/fence/sequence 按 schema 用十进制字符串，不转成丢精度 JS number；Go 内部 int64 不改变 wire 约定。
- Go 修改使用 gofmt；TS 保持 strict/NodeNext 和相对 `.js` import，只改 `src/`，不手改 `dist/`/`lib/`。保留 Character 的 pinned ChatLuna lifecycle patch，升级依赖必须重验。
- Controller MCP 合同独立维护于 `contracts.py`/`model_contracts.py` 和导出 schemas；不把其 Python/SQLite 模型写入 Go 生成类型。

## 8. 配置、凭据与外部动作

### 8.1 配置入口

| 入口 | 配置方式及边界 |
|---|---|
| Go 数据库 | `BBA_TASKENGINE_DSN`，仅环境变量；server、Bridge、migrator 使用；不新增 CLI DSN 或臆造 `.env` 自动加载 |
| 主服务监听/身份 | `-listen` 默认 `127.0.0.1:18421`，`-origin` 与 Host 匹配，必需 `-resolver-file`；格式查 `resolver.go` |
| resolver JSON | `credentials` 中 `token_sha256`、workspace、principal/permissions 或 runtime/owner_epoch/account_ids；operator/runtime 记录分离，不能混填 |
| Worker/任务开关 | `-development-operations`、`-worker`、`-worker-recover`、`-proposal-execution` 默认关闭；`-task-work-slots` 初始默认 1、范围 1..8；poll/batch/worker-id 查 `main.go` |
| 登录与规划 provider | `-login-provider-file` / `-login-fixture`，`-model-provider-file` / `-model-fixture`；真实与 fixture 配置互斥；模型文件和 API key 文件格式查 `product_runtime.go` |
| 模型/Agent/Skill registry | `-model-registry-file` 必须绝对路径、私有父目录；`-model-registry-approved-private-http-base` 是精确私网 HTTP 端点审批，不是任意内网放行 |
| ARTEX | `-artex-provider-file`；`backend/internal/artex/providerfile.go` 定义 base/token/allow_writes/timeout/retry；加载后主服务装配独立 graph worker，需单独核对副作用授权 |
| Dashboard | `-dashboard-account-source=projection` 默认；可选 `local6300` 与受保护 `-dashboard-account-mapping`，特殊本机读源不是通用账号发现 |
| 独立 Bridge | 默认 `127.0.0.1:18422`，`-origin`、必需 `-config`；runtime/account/owner_epoch、token digest、OneBot/WebUI loopback 端点与凭据查 `task-bridge/main.go` |
| 操作员 CLI | `backend/cmd/bba-operator/`：URL + `-token-file`，不要在参数中传 token 内容 |
| 隔离测试 | harness 注入 `BBA_FIXTURE_ONLY`、`BBA_FIXTURE_DSN`、`CONTRACT_ROOT`；`kernel.sh` 需要存在的可写 `TMPDIR` |
| 本地 dev-stack | `BBA_DEV_STACK_NAME/DB_PORT/HTTP_PORT/STATE_DIR` 等查 `tools/dev-stack.sh`；默认 HTTP 18431、DB 18432，区别于 server 直接启动默认端口 |
| Character/Collector | 各包 `src/types.ts`/配置 resolver 的显式 options：stateDir、身份、账号/runtime、backend/gateway 等；没有统一可照搬的生产 `.env` |
| Controller | `--repository`、`--state-dir` 必需；`--mode`、`--slots`、fixture/development 授权均为启动侧参数，MCP 请求不能升级授权 |

### 8.2 安全边界

- token、API key、DSN 密码、OneBot/WebUI 凭据不进入仓库、日志、PR、截图、证据包、模型上下文或浏览器持久化。公开个人标识合成/脱敏。
- resolver/provider/Bridge JSON 和 operator token 文件为常规文件、0600 或更严，遵守各 loader 大小上限；registry 父目录私有且单进程所有。不要读取/打印本机真实凭据来补文档示例。
- 保持 loopback、固定 Host/Origin、operator/runtime credential 分离、先鉴权再 replay。外呼有 timeout、响应体/并发上限、redirect policy 和 secret-safe errors。
- 真实 QQ 写必须按动作重验 owner、能力、目标、审批、预算、文本规则和许可；CRM 联系动作上线前接入最终 stop-contact 检查。配置快照不替代当前撤销检查。
- 仅对证明可重放的读取做有界 retry；写入 timeout/transport failure 视为 UNKNOWN。ARTEX 使用其自身 reconcile 协议，不强套 QQ permit。
- 未经明确授权，不登录 QQ、发消息、加群/好友、触发真实 NapCat/OneBot/ARTEX 写、连接生产数据库、调用付费模型、修改真实运行目录、重启服务或切换 writer。文档整理、fixture 测试不构成这些授权。
- fixture 只用隔离 PostgreSQL/loopback fake HTTP/假 adapter；不可传入生产 DSN。新动作必须具备限流、停止、审计和故障恢复，并遵守平台与适用隐私要求。

## 9. 前端与生成资源

<!-- omh:agent-instructions:begin -->
### ARTEX 风格 Web UI 入口与同步约定

- 产品是 `/product/` 的深色 QQ 私域运营控制台；不要误启 astra-handoffs 早期 Python ui-preview，不重复导入源码。
- 唯一源码 `frontend/`；`tools/build-product.py` 将 HTML/CSS/JS 复制到 `backend/cmd/taskengine-server/product/`，并由 `frontend/napcat-initial.txt` 生成 `tools.json`，不需要外部 ARTEX checkout。保留 `NOTICE`/licenses。
- `tools.json` 是未验证的目录，条目 disabled/schema unverified 不代表可调用；不能把“工具数量”当真实能力数量。
- 运行 `python3 tools/build-product.py` 和 `node --check frontend/app.js`。无 UI 变更时 `git diff --exit-code -- frontend backend/cmd/taskengine-server/product` 应为空；有意修改时审查两侧 diff，再次构建应无新增变化，并逐文件比对源码与副本。Git diff 非空本身不能判定同步失败。
- 页面及工具来源见 `docs/artex-web-ui.md`；其中本机实例地址是历史定位提示，使用前核实实际监听/进程，不默认启动或重启。
- 同步 PR 只包含源码、必要生成资源与文档；不提交 dev-stack 状态、token、DSN、数据库或运行日志。恢复 UI 不授予 worker、真实 QQ、模型、迁移或合并权限。
<!-- omh:agent-instructions:end -->

`frontend/app.js` 使用内存 bearer token、请求 AbortController 和 session/route/detail 序列；双对话另以 `chatSeq` 绑定页签、会话 ID 和页面代次，写表单还绑定 DOM 实例，拒绝迟到消息、错误、401 和保存回执污染当前视图。切换会话先清空旧消息，加载/失败不显示为空会话；忽略旧写回执不代表取消后端写入，不自动重试。修改异步 UI 时保留这些边界、HTML 转义与同源 CSP。验证真实 API、权限、空/错误/未知状态、分页、刷新和浏览器交互。新增 Collector/嵌入一致性/浏览器 CI 应单独提交，不能假定现有 workflow 已覆盖。

## 10. 开发、验证与部署流程

### 10.1 按变更选择验证

以下除注明外均从仓库根运行。先核对 scripts/`npm run`、工具版本、Docker 和依赖缓存；只跑相关且必要的检查，扩大范围说明理由，失败必须调查。

```sh
# 契约：缺缓存且确有需要时才显式联网 prefetch
bash tools/contract.sh prefetch
bash tools/contract.sh check
# 改 Schema 且需要重建类型时才运行：bash tools/contract.sh generate

# Go：必须使用隔离 harness，先设置专用可写 TMPDIR
mkdir -p "$PWD/build/test-tmp"
TMPDIR="$PWD/build/test-tmp" bash tools/kernel.sh test-all
TMPDIR="$PWD/build/test-tmp" bash tools/kernel.sh smoke
bash tools/kernel.sh schema

# 前端及候选插件
python3 tools/build-product.py
node --check frontend/app.js
node --test tests/plugin/*.mjs

# Controller：先按 pyproject 安装到独立 Python 环境；只执行隔离模拟测试
PYTHONPATH=controller/src python3 -m unittest discover -s controller/tests -v
```

Go 测试不得裸跑 `go test ./...` 绕过 harness 或掩盖缺 DSN skip。`kernel.sh` 用无网络容器与 Unix socket、不发布 DB host 端口；`schema` 是隔离 SQL 应用 smoke，不替代 server/Bridge 启动门测试。`prefetch` 准备缓存后契约 Node/Go 阶段使用 `--network none`。根 `contract:check` 只含 refgate/validator/Python tests/lint，完整类型生成与 smoke 见 `generate`。

两个 Koishi 包分别安装和验证（Node 22）；Collector 的 file 依赖要求先准备 Character：

```sh
(cd koishi/character-runtime && npm ci --ignore-scripts --no-audit --no-fund && npm run build && npm run typecheck && npm test)
(cd koishi/analysis-collector && npm ci --ignore-scripts --no-audit --no-fund && npm run check)
```

`prebuild`/`pretest` 会调用固定 ChatLuna patch；`--ignore-scripts` 安装不代表可以跳过后续 build。Collector 使用 `lib/`，Character 使用 `dist/`，都不能以旧编译产物替代源码验证。

只有任务要求产品验收时才创建新证据目录（占位路径需替换）：

```sh
bash tools/product-acceptance.sh fixture --output <new-evidence-directory>
bash tools/product-acceptance.sh verify --package <evidence-directory>
bash tools/product-acceptance.sh release-check --package <evidence-directory>
```

### 10.2 本地开发与发布

- `tools/dev-stack.sh start|stop|status` / 根 `npm run dev:stack` 启动的是持久本地 fixture（Docker DB、显式 migration、种子数据、fixture Worker），不是生产部署。使用新实例名/独立目录并先核对端口；`reset` 删除对应 fixture 状态/volume，`env/token/runtime-token` 会输出凭据，不采入日志/公开材料。
- 构建前先同步产品资源。从 `backend/` 构建 `./cmd/taskengine-server`、`./cmd/taskengine-migrate`、`./cmd/task-bridge`、`./cmd/bba-operator`；Go module 根在 `backend/`，不是仓库根。
- 发布流程以 `docs/product-release-runbook.md` 为程序参考：核对本次 binary/schema/授权 → 受保护配置 → 备份与隔离恢复验证 → drain/reconcile 未决动作 → 显式迁移 → 启动门/身份核对 → 单 writer 切换及有证据的验收 → 监控与回滚点。任何真实步骤仍需对应授权。
- 不从历史 runbook 复制 schema 常量/DSN/实例地址；恢复后也不能自动重放 UNKNOWN。没有经过本轮演练，不能声称具备生产部署或回滚保障。

### 10.3 CI 实际范围

- `.github/workflows/ci.yml`：PR 和 main push 触发 `contracts`（契约、TS 类型检查、前端语法、插件测试）、`backend`（隔离 race suite + build）、`character`（安装/build/typecheck/tests）。
- `.github/workflows/controller.yml`：仅匹配 `controller/**`、自身 workflow 或 `docs/adr/controller-phase1.md` 时触发 Controller unittest/stdio MCP fixture；只改根 `AGENTS.md` 不会触发它。
- 当前没有常驻 Collector、产品 embed 同步完整性或完整浏览器验收作业。CI 绿灯仅支持已运行项目；分支保护/required checks/approval 每次从 GitHub 实际设置读取，不在指南写死。

## 11. 强制 GitHub Pull Request 工作流

**每轮开发都走独立分支和 PR；禁止直改/直推主分支、跳过 CI、强制合并或忽略失败。**

1. 开始检查 `git status`、当前分支、remote/remote HEAD、近期提交与未提交改动归属；`git fetch origin`，基于最新远端主分支建独立分支。工作树脏时先保护，可用独立 worktree；不擅自 stash、覆盖或丢弃用户改动。
2. 阅读本文件、相关 ADR/矩阵与当前装配，确认缺口后实施单一目的变更；适用时同步契约、migration、测试、前端副本和文档。
3. 提交前审查完整 diff、`git diff --check`、敏感信息、生成物和许可证；记录命令、退出码、结果。未跑写 `NOT_RUN` 和原因，阻塞写 `BLOCKED`，不造 PASS。
4. 提交、推送独立分支并创建 PR。描述背景/目标、最终变更、API/schema/权限/外部动作影响、验证和 CI、风险/未覆盖/回滚以及 fixture/live/production 边界；有 issue/template 则关联/使用。
5. 等待**最新 head commit** 的 required checks；无 required checks 时也核对预期实际作业全通过，空列表不是成功。失败须调查修复、重新验证推送并等新 head，不能用旧绿灯。
6. 核实实际 review approval/branch protection/rulesets；Agent 自审不替代必需 approval，不改治理配置，不用 admin bypass。只有检查与审查满足且本轮有适用的明确合并授权才合并，否则交付可审查 PR。
7. 获授权合并后记录 merge commit，fast-forward 本地主分支，核对远端 SHA/工作树，按策略清理；不得覆盖其他工作树的未提交改动。报告 PR/CI/验证/风险并核对相关矩阵与 runbook 是否需更新。

## 12. 完成定义

按适用范围逐项验收；不适用在 PR 说明原因：

- 目标、非目标、授权边界清楚；来自最新主分支的独立分支，未覆盖用户工作。
- 修改与真实 handler/consumer 一致，没有重复执行器；契约/DDL/生成类型/UI 副本同步。
- 相关隔离、权限负向、幂等/并发/超时/UNKNOWN/取消/恢复测试通过；新增持久化域有保留/删除与恢复验证。
- 本地命令、退出码、日志可追溯；最新 PR head 实际 CI 通过，独审和生产证据状态如实记录。
- 文档/能力矩阵不夸大；未覆盖项和回滚方式明确；合并必须满足审查及用户授权。
- 凭据、生产数据/备份、运行二进制、依赖缓存和本地历史证据包不提交公开仓库。

## 13. 已知缺口、冲突与易踩坑

以下为梳理基线的源码观察，后续修复后更新本表；不是本轮执行测试结论。

| 项目 | 事实、影响与核对位置 |
|---|---|
| 连续 Worker 的真实接线 | `taskengine-server/main.go` 明确 fixture transport；不能把 `-worker`、独立网关或旧 live 报告视为真实连续链已验收 |
| 任务执行覆盖 | `task.go` 后四种类型明确 unsupported；不要把 DRAFT 创建/读回算执行成功 |
| 日历/CRM/运营 Agent run | 当前 DDL/主服务无这些完整持久化链；`scheduler.go`、Controller tasks 和 Agent registry 分别不是这三项的完成证明 |
| CRM UI 文案冲突 | `frontend/app.js` 的 assets `pendingText` 仍有“意向 + 至少一个运营 QQ 好友关系才算引流成功”的旧文案，与第 4.4 节冲突；后续 UI 修复须按明确意向独立计数，不据旧文案改业务规则 |
| 双对话状态 | B7 主路由/handler 已装配；`operator_conversation.go` 只做消息/草稿/本地通知，不证明 Agent 自动回复、多步 run 或 QQ 出站已接通 |
| B7 HTTP 契约覆盖 | 当前 `contracts/openapi.yaml` 未列出 conversations、agent-threads、qq-drafts、qq-inbox/events、operator notifications 路由；handler/主服务已有实现不代表 OpenAPI 已覆盖。变更需核对实际实现与测试，后续单独补齐规范和正反 fixtures，不能用契约 CI 绿灯认定这些接口已验收 |
| Schema 日志/文档漂移 | Bridge 启动日志仍含固定历史 schema 值；runbook 含历史 head/命令，不能作为实际读数。读 `SchemaHead()`、gate 和数据库；最低启动门不等于全部页面兼容 |
| 配置与备份 | registry 与模型请求数据库无跨存储事务；文件当前上限 4 MiB，历史/回执增长会触及拒绝写入边界。PostgreSQL 备份不覆盖 registry、runtime 文件/Controller SQLite；需按域规划归档、备份和恢复 |
| Controller 与产品 MCP | 独立 stdio 模拟研发控制器存在；产品 MCP 菜单/工具目录不因此具备真实连接或业务工具执行能力；真实 OMP 路由/隐式调用/fallback 未闭合 |
| 历史文档与证据 | `docs/README.md` 主要是早期契约及阶段记录；验收/runbook/Controller 报告可能含旧 SHA/schema/本机缺失证据路径，不是当前运行证据 |
| CI 缺口 | Collector、embed 完整性、完整浏览器行为需按改动另验；Controller workflow 有 paths filter |
| 脚本副作用 | generate 覆盖输出，dev-stack start 启动/迁移 fixture，reset 删除状态，产品 fixture 新建资源/证据，token/env 输出秘密；先核对命令目标 |
| Adapter/ARTEX/身份 | 候选插件、补丁、静态 enabled、端口或昵称不证明账号身份及能力；图同步不证明 QQ 效果，不确定外部写禁止盲目重发 |

## 14. 权威索引与维护方式

| 需要解决的问题 | 先读 |
|---|---|
| 产品入口/本仓库规则 | `README.md`、本文件；README 的概述不替代当前模块盘点 |
| Wire contract/生成 | `contracts/openapi.yaml`、`contracts/schemas/`、`contracts/examples/`、`tools/contract.sh` |
| 基础执行/身份/许可证决策 | `docs/adr/p0-decisions-2026-10-24.md`、`docs/adr/stage-a-console-2026-10-29.md` |
| Agent/Skill、ARTEX 映射 | `docs/adr/20261031-b2-agent-skill-registry.md`、`docs/adr/bba-artex-mapping-2026-10-29.md`、`docs/adr/2026-10-31-artex-phase-a.md` |
| 任务类型/能力/产品验收 | `docs/qq03-task-type-matrix.md`、`docs/capabilities/`、`docs/product-acceptance-matrix.md` |
| 产品 UI | `docs/artex-web-ui.md`、`frontend/NOTICE`、`tools/build-product.py` |
| 部署/恢复程序 | `docs/product-release-runbook.md`、`tools/product-restore.sh`、migrator 源码；历史命令先校准版本 |
| 研发 Controller | `docs/adr/controller-phase1.md`、`controller/README.md`、`controller/src/bba_controller/`、`controller/tests/` |
| 验证自动化 | `.github/workflows/ci.yml`、`.github/workflows/controller.yml`、各 package scripts |

维护本文件时记录稳定边界和可跳转路径，不粘贴完整接口/DDL/凭据，不把一次 PASS 永久写成能力。架构、装配、持久化域、配置入口或工作流变化时同步更新对应章节；历史报告保留原日期和范围，不批量改写成当前事实。保留仍有效的安全、产品语义与交付规则。常规可逆选择自行推进，仅实质影响业务/授权的未决信息需确认。

## 15. 未决决策：实施前不得猜测

1. `customer.add_me` / `customer.add_customer` 的具体动作、可观察完成证据及真实 adapter 能力。
2. Scheduler 各任务的 misfire/overlap/DST/审批策略及 occurrence 事务恢复协议。
3. CRM 意向规则/人工纠错、原文与证据保留期限、删除/脱敏和备份清除策略；先明确法域和业务目的。
4. 运营 Agent 的最大步数、超时/并发/预算、工具失败、停止/恢复策略及 thread/run/step 关联；版本化配置和测试固定，不能由模型自行决定。
5. 每阶段哪些动作仅草稿/fixture，哪些可受控 live；真实 QQ、模型、MCP、ARTEX、生产发布逐项授权。
6. Controller 未来真实研发写入、真实 ACP/OMP 模型路由与预算控制如何验收；不得把当前模拟策略或 `is_local` 标签当真实模型认证。

**先核实当前实现，再定义最小缺口；先建立授权和证据，再开放动作；通过独立 PR 与 CI 后，仅在获得合并授权时合并。**
