# BBA 仓库级 Agent 指南与总体架构

> 本文件是 BBA 仓库级开发规则和架构约束的首要入口。它规定稳定边界、开发流程和验收原则；具体阶段任务、接口字段、数据库结构与当前状态，以对应 ADR、OpenAPI、migration、能力矩阵和本轮源码为准。
>
> **重要：** 本文件不是生产部署授权，也不代表任何功能已经通过真实 QQ、真实模型、MCP 或发布验收。每轮工作都必须从当前分支源码核实，不得把历史报告当作当前事实。

## 1. 产品目标

BBA 的长期目标是一个完整的 QQ AI 运营平台：运营人员在统一控制台管理多个 QQ 账号、群聊和客户；Agent 根据运营目标分析信息、生成计划和建议；在明确授权、审批和能力约束下，通过受控运行时执行允许的动作；所有消息、任务、客户证据、审批和执行结果形成可追溯记录。

核心业务闭环：

```text
运营目标 / 操作员指令
        |
        v
Agent 理解、检索、分析与规划
        |
        v
Proposal / 审批 / 授权裁决
        |
        v
Task Engine 或独立 Chat Permit 路径
        |
        v
Task Bridge / Runtime / Adapter
        |
        v
Observation / Inbox / 事件归档
        |
        v
客户证据、运营报表、Dashboard
        |
        +------> Agent 后续分析与建议
```

这是一条产品数据闭环，不代表所有操作共用同一条执行链。任务执行与日常交互聊天必须保留各自已建立的执行边界，统一的是治理要求：身份、workspace/account scope、能力、授权、许可、幂等、审计、外部副作用确认和停止机制。

## 2. 不可违反的架构原则

1. **保留现有技术底座，增量演进。** 不推倒 Go Task Engine、PostgreSQL、Task Bridge、OpenAPI、现有 Go Console、Koishi Character Runtime、Analysis Collector、ARTEX adapter/outbox。先复用、补齐和接线，只有有证据证明无法满足需求时才提出替换。
2. **Task Engine 是任务执行唯一内核。** 不建立争夺执行所有权的第二套通用任务/Reserve 内核。允许计划定义、occurrence 状态和有界到期扫描；计划必须物化现有 Task 并复用其授权、执行和恢复协议。
3. **聊天许可不是 Task Reserve 的别名。** `chat_permit.go` 已明确：日常聊天许可独立于 Task Engine，不创建 task、subtask、goal slot、execution、target claim 或 dispatch permit。不得为了“统一架构”把现有 Chat Permit 强行改造成加群任务链。若未来要把聊天纳入任务化管理，必须单独设计桥接、状态、幂等、审批和恢复协议。
4. **Agent 是决策和规划层，不是权限主体。** 模型输出、Prompt、Skill、搜索结果、群摘要、客户画像、ARTEX 图数据均不能授予 QQ 权限。所有工具调用由服务端按 workspace、principal、account、action family、能力、策略、审批和当前状态裁决。
5. **PostgreSQL 是业务与执行事实来源。** 持久化的 task/event/execution/permit/bridge/outbox/inbox/result、客户证据和审批记录是事实；内存状态、日志、UI 缓存、静态注册表、fixture、模型上下文和 ARTEX 投影都不能替代持久事实。
6. **不确定外部副作用一律保守。** `submitted`、`pending`、`unknown` 不等于成功。发生超时、断连、进程崩溃或缺少权威回执时，不自动重发、不换账号、不释放可能仍占用的 claim/slot/预算；进入只读核对或人工处理。
7. **能力未知即 fail closed。** 目录中存在工具、Schema 中存在动作、插件配置为 enabled、端口可连接或 fixture 成功，都不证明真实运行时具备该能力。
8. **所有数据和动作必须有边界。** workspace/account/conversation/identity 必须隔离；mutation 必须有授权、版本或 fence、幂等和审计；QQ 外部写必须有对应动作族单次许可和可验证结果；其他外部写遵循各自授权、outbox/checkpoint/reconcile 协议。
9. **只保留一个前端产品源码。** 产品控制台以 `frontend/` 为源码，按仓库构建流程同步到 Go embed 资源。不得另建第二套控制台或只修改生成副本。
10. **遵守许可证与来源边界。** 检查 `frontend/NOTICE` 和各目录许可；不得宣称整个仓库是 MIT。ARTEX 仅作为信息架构/交互参考及受控集成，不得直接复制其领域模型或调度器来替代 QQ 业务内核。

## 3. 当前技术架构与职责边界

### 3.1 核心组件

| 组件 | 主要职责 | 不应承担 |
|---|---|---|
| Go Task Engine (`backend/internal/taskengine`) | 任务、账号授权、Reserve、Worker、Proposal/Work Item、状态机、持久化和恢复 | 日常 Chat Permit 的强制替代品；第二套 CRM/调度执行器 |
| 主服务 (`backend/cmd/taskengine-server`) | HTTP 装配、resolver、API surface、provider/handler 初始化、嵌入控制台 | 绕过权限的业务逻辑；隐式数据库迁移 |
| Task Bridge (`backend/cmd/task-bridge`) | loopback runtime gateway、单次 permit 校验、运行时身份绑定和 adapter 调用 | 自行决定业务权限或绕过 Task Engine |
| PostgreSQL | 业务事实、执行事实、幂等、审计、恢复 | 仅作缓存或只存 UI 投影 |
| OpenAPI (`contracts/`) | HTTP/plugin wire contract、schema、正反 fixtures | 运行时启用证明 |
| `frontend/` | 唯一产品 UI 源码 | 以静态页面冒充后端功能 |
| Character Runtime (`koishi/character-runtime`) | 按账号隔离的交互运行时、可信 trigger 检查、Chat Send Gate | 任意账号共享 runtime/cache/persona；绕过 Chat Permit |
| Analysis Collector (`koishi/analysis-collector`) | collection-only 数据采集、窗口和 Summary 生产 | command/chat/model dispatch 或任何未经授权的消息发送 |
| ARTEX adapter/outbox | 外部图同步、checkpoint 和 reconcile | BBA 任务执行事实来源 |
| Cleaner / NapCat patches | 受控运行时适配候选与能力边界 | 未验收即视为已部署、已授权的生产 writer |

### 3.2 任务执行路径

任务执行沿用现有链：

```text
Operator API
 -> resolver / workspace + permission scope
 -> Task + trusted candidate/baseline
 -> Reserve (single transaction)
 -> execution / slot / claim / budget / outbox
 -> single-use permit
 -> Task Bridge
 -> runtime identity + owner epoch/fence
 -> adapter
 -> observation / inbox reducer / result / task projection
```

- `Reserve` 是创建新 dispatch attempt 的事务入口；事务内重新校验任务、审批、账号身份/状态/能力、预算、目标 claim 和 slot。
- Bridge 必须核对不可变 payload digest、scope、account/runtime/owner epoch/fence 和 permit。
- 一个 permit 最多对应一次 adapter call。
- proposal materialize 只创建持久 Work Item，不直接执行外部动作；后续仍由既有 worker/Reserve 链推进。
- 任何新增任务类型必须分别声明：是否可识别、是否可创建、是否可持久化读回、是否可执行、执行证据是什么。不得用“任务中心支持”笼统覆盖这些维度。

### 3.3 独立聊天路径

日常 Character 聊天沿用独立路径：

```text
trusted inbound trigger
 -> account/runtime/context/active-speaker checks
 -> chat send request + exact text digest
 -> durable single-use Chat Permit
 -> Task Bridge / text adapter
 -> sent / unknown outcome receipt
```

- Chat Permit 与任务 dispatch permit 是不同权限和状态域。
- 当前 writer contract 不能被擅自扩展为多段、媒体或其他 QQ 动作。
- permit 已消费但没有权威结果时视为 unresolved/unknown，不得重新发放同一动作许可。
- 操作员 Agent 对话与 QQ 收件箱是两个信任域：Agent 消息不能自动成为 QQ 出站；QQ 入站内容不能自动成为 Agent 系统指令。

### 3.4 Agent、模型、Proposal 与工具

当前仓库已有 Agent/Skill/Model 配置绑定、provider 调用和搜索上下文能力，也已有 Proposal、Work Item、DAG 等管线。不得描述成“完全没有 Agent 运行能力”；同样不得把已有模型调用误称为完整多步 Agent 工具执行运行时。

后续 Agent 运行能力应在现有基础上增量补齐：
- 对话线程如何启动、关联和查询一次 Agent run；
- 多步 run 的持久状态、步骤、停止、超时、取消和恢复；
- run 开始时固定 Agent、Model、Skill、规则和工具 Schema 版本；
- 模型提出工具请求后，由服务端独立进行权限和能力裁决；
- 工具执行结果作为不可信观察回注模型，不直接成为权限；
- 超时、重试和崩溃恢复不得重复创建 Proposal、Task 或外部动作；
- 运行审计能关联 thread、run、step、proposal、approval、task/permit 和最终证据。

完整 MCP 管理平台不是 Agent 生成 Proposal 的前置条件。只有某一阶段明确承诺真实执行某个动作时，该动作所需的最小 adapter、runtime capability、authorization 和验收证据才是该动作上线的前置条件。

必须区分：
1. Tool Catalog：产品可展示的工具目录；
2. Tool Schema：输入输出结构与版本；
3. Runtime Capability：某个具体账号/runtime 已验证具备的能力；
4. Tool Authorization：某个主体在当前 scope/时间/策略下是否获准调用。

MCP server 注册或仓库 URL 不等于 endpoint 可用、工具已安装或动作获准。MCP 不得绕过现有执行治理。

## 4. 产品信息架构与目标模块

产品主导航：

1. 仪表盘
2. 对话
3. 任务
4. 计划任务
5. 资产（客户 CRM）

系统与运营配置（保留全局代理、网络搜索、Work Agent 并发及既有作用域）：

6. LLM
7. Agent
8. MCP
9. Skill
10. 工具
11. 通知推送
12. 拦截规则
13. 审批记录
14. 日志
15. 系统配置

页面存在、菜单可见或静态数据能展示，不代表功能接线完成。每个页面必须列明实际 API、持久化来源、权限范围、错误/空/未知状态及独立验收证据。

### 4.1 双对话

- Agent 对话：操作员与 Agent 的 thread/run/message。
- QQ 收件箱：按 workspace、QQ account、群聊/私聊 conversation 隔离的入站消息及出站草稿/回执。
- 两类对话使用不同信任标记、API scope 和事件来源。
- QQ 消息原文、附件、外部搜索内容都是不可信输入，不得当作系统/开发者指令。
- 首轮出站可采用草稿、规则审核和 permit 状态回执；未获明确授权不得调用真实 QQ writer。

### 4.2 任务中心

当前 `qq03-task-type-matrix.md` 所列五类任务的状态必须逐项维护，不能推断执行能力：

| 类型 | 当前已知边界 |
|---|---|
| `group.acquire` | 保留既有加群执行链；具体真实能力仍需按当前 adapter 和 live 证据判定 |
| `group.analyze` | 可识别/创建/读回；执行依赖 Summary/Collector 的只读证据，当前矩阵标记 unsupported |
| `chat` | 可识别/创建/读回；日常聊天走独立 Chat Permit，不混用任务执行；任务化桥接另行设计 |
| `customer.add_me` | 可识别、创建 DRAFT、持久化读回，执行 unsupported；必须先定义动作语义和可观察完成证据，不得猜好友 API |
| `customer.add_customer` | 可识别、创建 DRAFT、持久化读回，执行 unsupported；必须先定义动作语义和可观察完成证据，不得猜好友 API |

任务状态、能力状态和外部效果状态必须分开表达。`CAPABILITY_UNAVAILABLE` 是明确拒绝，不是待重试任务。未知结果不自动重发。

### 4.3 计划任务 / Scheduler

计划任务复用现有 Task Engine，负责将规则物化为现有 Task，允许 occurrence 状态机及有界扫描，不另建通用任务执行器。

设计和实现必须先定义以下一致性策略：

- **Occurrence 身份：** 每次计划触发有稳定唯一键，绑定 schedule ID 与规范化目标触发时刻；计划版本作为快照属性，不因版本递增允许同次触发重复物化；时区使用 IANA TZ 数据，存储 UTC 时间及原始本地计划信息。
- **Occurrence → Task 原子性：** 优先在同一 PostgreSQL 事务中创建 occurrence 与 task，并写入事件/幂等记录。若现有边界无法同事务，必须使用稳定幂等键、明确中间状态、租约过期恢复和唯一约束；崩溃后重跑只能读回同一个 task，不能创建第二个。
- **领取状态：** occurrence 采用可恢复状态机，例如 pending → claimed/materializing → materialized，失败状态需可区分可重试与永久拒绝。租约必须有 owner、expiry 和版本/CAS。
- **错过触发：** 默认不无限补跑。每个 schedule 必须声明 misfire policy；先在 ADR 明确容忍窗口，避免正常扫描延迟被一律跳过；skip 记录原因。若启用 coalesce/catch-up，必须配置时间窗口和最大补偿数量。
- **重叠：** 默认 forbid overlap。上一次仍运行、待审批或 UNKNOWN 时，不允许新 occurrence 绕过旧动作；按计划配置 skip 或排队，并保留原因。
- **修改/暂停/删除：** schedule 版本递增。创建 task 前重验版本与 enabled；旧 occurrence 明确进入 superseded/cancelled 等终态。跨版本替代/更新必须有唯一性仲裁及竞态测试，同刻最多物化一个 task；已 materialize 的 task 不可静默删除，必须走 Task 自身的取消/审批/UNKNOWN 规则。
- **审批：** 创建计划的审批不自动等于每次外部动作的授权。默认每个 occurrence 在执行前重新校验当前授权、账号能力、预算、规则版本和审批有效期；只有经风险评估明确批准的低风险动作，才可设计受限的模板授权。
- **时区/DST：** 保存 IANA timezone 和本地计划表达式；不存在的本地时间、重复的本地时间必须有明确策略并写测试，不能依赖服务器本地时区。
- **恢复：** 覆盖领取后崩溃、task 创建后进程崩溃、多个 worker 并发、时钟漂移、版本修改、审批撤销和数据库恢复。

以上是架构默认约束；具体 misfire、overlap、DST 策略须在对应 Scheduler ADR/任务书中落定后再实现，不得让代码偶然决定业务行为。

### 4.4 客户资产 / CRM

CRM 的核心是“有证据的明确意向客户”，不是群成员数量，也不要求先建立好友关系。

业务语义：
- 明确意向识别成功即可计入引流成功客户；好友关系若可观察，作为独立属性/事件记录。
- 模型判断、置信度和理由是识别结果，不是客户自述事实；必须关联原始证据、规则/模型版本、识别时间和来源。
- 统计默认在 workspace 内去重；不建设跨 workspace 全局身份库作为第一阶段前置条件。
- 同一客户在多个群、多个账号出现，只计一个 workspace 内客户；来源与账号贡献保留多条归因，不能把账号贡献数简单相加作为客户总数。
- 至少区分：
  - 当前有效意向客户数：去重客户中至少有一条当前有效的明确意向；
  - 历史确认记录数/人数：历史曾确认的记录，包含后来撤销或纠正的历史；
  - 撤销/纠错记录：追加记录，不无痕覆盖旧判断；
  - 好友关系状态：单独记录，不作为意向计数门槛。
- 客户合并必须基于稳定 QQ identity 或可审计的人工确认；不能只因昵称相同就自动合并。
- 证据记录应关联 workspace、customer identity、conversation/source、message/event reference、rule/model/skill version、decision、review/correction 和 attribution。
- 生命周期状态与是否计入当前有效意向分开建模；无效、撤销、拒绝联系不能通过改状态抹掉历史。

**停止联系是执行约束，不是 UI 标签：**
- 建立 workspace/account/customer 维度的 suppression/stop-contact 状态和审计原因。
- 创建任务/草稿时检查一次；最终 adapter dispatch 前必须再次检查。
- 停止联系后撤销尚未执行的相关授权/待执行动作，并阻止新动作。
- 已经 dispatch 或结果未知的动作不能假装取消成功，必须保留 UNKNOWN 并只读核对。
- 退订/拒绝联系的处理必须可追溯，并尊重适用平台规则和法律要求。

**数据生命周期：**
- 不默认永久保留所有消息原文。按部署地区、业务目的和法律评估制定可配置保留期限、最小化采集、脱敏/删除和备份过期策略。
- 当前 operator message 表存在禁止 UPDATE/DELETE 的触发器；任何变更必须设计受控删除/脱敏或 tombstone 机制、证据失效语义、审计保留与备份清除，不得直接绕过触发器。
- 在未完成法域与业务评估前，不在代码中臆造统一保留天数，也不宣称某种保留方式必然合规。

### 4.5 Dashboard、通知、日志与审批

- 沿用 ARTEX 主要布局：账号在线/离线/异常/总数替代“确认发现”，资产为意向客户数；删除流量交互、活动流、发现和流量状态码。
- Dashboard 读可回溯持久事实/投影，缓存可重建且有新鲜度，不用演示数字冒充事实。
- 账号状态区分 online/offline/error/unknown/stale；未知或过期不能显示成离线，也不能显示在线。
- 任务状态按真实生命周期区分 queued/running/completed/failed/unknown 等；UNKNOWN 不能计入成功。
- 客户统计使用 CRM 去重口径；来源账号贡献可多归因但不得重复加总。
- 通知跟随对应业务事件逐项接线：消息通知随双对话、任务结果通知随任务事件、客户意向通知随 CRM，不预先把尚未实现的事件算作已交付。
- 通知必须支持 workspace/account scope、去重、分页 cursor、未读/已读并发安全、脱敏及明确的 local-only 状态。
- 审批必须绑定具体 digest、scope、主体、资源、版本、期限和预算；内容或授权版本变化后需重新审批。
- 日志必须可关联 request、thread/run、task、execution、permit、bridge operation、observation 和审计主体，且不得泄漏凭据或不必要的消息原文。

## 5. 阶段路线与依赖

路线是按可验收垂直切片推进，不是承诺当前功能已完成。阶段可拆多个单一目的 PR，以可验证证据和明确的 out-of-scope 结束。

### M0 — 基线与当前提交收敛
- 锁定本轮基线 commit、工作树状态、当前 schema head、构建版本。
- 盘点主服务真实装配、resolver/API surface、OpenAPI、migration、前端源码/嵌入资源、CI 和产品验收矩阵。
- 修正文档与源码冲突；不开展无边界的“全仓历史报告清零”。
- 将缺陷分级：P0 安全/数据损坏/越权；P1 核心功能阻断；P2 一般功能缺口；P3 文档/体验。
- 当前 B7 已有主服务装配，后续必须基于当前提交做增量独立复核，不能沿用过期任务书声称 handler 尚未接线。

### M1 — 双对话基础闭环与通知
分开验收：
1. 基础消息闭环：QQ 入站持久化、conversation 隔离、分页、去重、收件箱 API/UI、出站草稿/拦截回执、通知投影。
2. Agent 线程基础闭环：线程/消息持久化与查询、信任域隔离。已有模型与 Proposal 通路按实际消费者增量验收；thread-to-run、多步运行、step、停止/恢复与工具执行授权属于 M5，不作为基础消息验收的隐含条件。

先检查当前已有 handler、route assembly、migration 和前端消费者；只补实际缺口。Agent 对话与 QQ 收件箱保持信任域隔离。首轮不调用真实 QQ writer。

### M2 — 多账号与任务产品化
- 复用 account/runtime/owner epoch/identity read-back 体系。
- 展示账号身份、登录会话、心跳、能力、风险、暂停和 runtime 归属。
- 对每种任务类型分别验收识别、创建、持久化读回、执行、观察证据；unsupported 类型保持明确拒绝。
- 账号分配必须校验在线/身份/能力/暂停/UNKNOWN/预算/授权范围。
- 不以端口、昵称、文件名或静态配置替代账号身份。

### M3 — 计划任务
- 先形成 Scheduler ADR，明确 occurrence 状态机、事务/幂等恢复、misfire、overlap、DST、schedule version、审批和授权策略。
- 再复用 Task Engine 落地最小闭环。
- 验证多 Worker、崩溃点、重启、重复领取、时区边界和 UNKNOWN 阻断。

### M4 — 客户 CRM
- 先定义客户去重、当前/历史意向、来源归因、证据撤销、人工纠错、停止联系和数据生命周期。
- 复用现有 schema 与事件模型，只有必要时才新增 migration。
- 先实现证据链与统计口径，再实现复杂客户运营 UI。
- 新增 CRM 持久化域时，同一阶段完成隔离 fixture、当前 schema 的恢复验证和数据隔离测试。

### M5 — Agent 运营能力
- 复用现有 Agent/Skill/Model registry、provider、Proposal/Work Item，不另建模型通路。
- 补齐 thread-to-run、持久多步运行、step、停止/恢复、工具授权、版本快照和幂等。
- 首期可按运营角色配置 Agent/Skill，不以建设多个独立 Agent 服务为前置条件。
- Skill 是版本化流程定义，不是权限凭证；模型输出只产生建议/Proposal。
- 每个真实动作必须先有可验收 adapter、runtime capability、authorization 和观察证据。

### M6 — MCP 与工具生态
- 分阶段实现 Tool Catalog、Schema、Runtime Capability、Authorization 和 MCP server lifecycle。
- 先接入确有业务需要且能独立验收的工具；不要求一次性接入所有 MCP。
- MCP 连接/工具调用不得直接绕过 Task Engine 或 Chat Permit。
- 任何新外部动作先交付最小安全闭环，再扩展管理界面。

### M7 — Dashboard、可观测性与发布验收
- 各业务域交付时同步增加对应 Dashboard/通知消费者，不把首次集成推迟到最后。
- 汇总客户、账号、任务、Agent、审批、风险和运行指标。
- 完成当前 schema 的备份恢复、生产运行手册、权限/密钥/限流/停止机制、告警和发布演练。
- 真实 QQ、真实模型、MCP、ARTEX 和生产发布分别独立授权、独立验收；一个域的 live 证据不自动覆盖其他域。

## 6. 状态与证据模型

不得只用一个五级状态标签概括功能。每项能力至少分开记录以下维度：

- **实现状态：** `NOT_IMPLEMENTED` / `PARTIAL` / `IMPLEMENTED`；
- **接线状态：** `NOT_WIRED` / `PARTIAL` / `WIRED_TO_CONSUMER`，并记录真实 handler、route、worker、adapter 或 UI consumer；
- **验证身份：** `AUTHOR_SELF_TEST` / `INDEPENDENT_SOURCE_REVIEW` / `INDEPENDENT_EXECUTION` / `NOT_REVIEWED`；
- **验证环境：** `UNIT` / `CONTRACT` / `ISOLATED_FIXTURE` / `CONTROLLED_LIVE` / `PRODUCTION`；
- **验收结论：** `PASS` / `FAIL` / `BLOCKED` / `NOT_RUN`；
- **发布状态：** `NOT_RELEASED` / `RELEASE_CANDIDATE` / `RELEASE_ACCEPTED`。

每份证据记录适用字段；未记录的历史字段不得补造。未提交树测试另记 diff/文件摘要，提交后核对内容一致：
- commit SHA；
- binary/build identity（适用时）；
- schema head / migration set（涉及数据库时）；
- 测试命令、退出码和原始日志；
- fixture/live 的边界；
- 账号/workspace/action scope（如适用，脱敏）；
- 证据生成时间；
- 实际作者/执行者/审查者及其验证方式，未独审标记 NOT_REVIEWED；
- 未覆盖风险。

`IMPLEMENTED` 不等于已接线；作者测试不等于独立验收；fixture 通过不等于真实受控通过；真实受控通过不等于生产可运行；CI 通过不等于产品发布批准。

产品验收矩阵是按行、按证据范围判断，不是产品完成百分比。历史报告必须标注日期、commit 和当时 schema，不能覆盖当前验证。

## 7. 数据库、契约与兼容性规则

1. migration 源码和当前 binary 的 `SchemaHead()` 是 schema 版本事实来源；不要在本文件或任务书中长期硬编码容易过期的 head 数字。
2. 每次涉及数据库的变更，核对 migration tail/head、`SchemaHead()`、server schema gate、Bridge schema gate、迁移测试、`taskengine-migrate --check` 和隔离数据库实际 head。
3. migration 只追加，不重编号已发布 migration；保持事务、约束、审计和升级语义；不做隐式 startup migration，不自行引入 down migration。
4. 对 schema 变更同时测试从受支持旧版本升级、当前版本启动、隔离恢复和关键查询。
5. `contracts/openapi.yaml` 是 HTTP/plugin wire contract 的规范来源；schema/fixtures 与生成 Go/TS 类型保持一致。
6. 修改 OpenAPI/schema 时先更新正反 fixtures，再运行 `bash tools/contract.sh check`；只有确实需要时才运行 `generate`。永远不要手改 `types.gen.*`。
7. `x-feature-enabled-in-p0` 等合同元数据不证明运行时 handler 已装配。
8. 每个新增持久化域都要在本阶段验证 workspace/account 隔离、幂等、恢复和删除/保留语义；不能把所有恢复验证推迟到最终发布阶段。

## 8. 安全、隐私与外部动作

- 不得把 token、API key、DSN 密码、OneBot/WebUI 凭据写入仓库、日志、PR、截图、证据包或浏览器持久化。公开个人标识合成/脱敏；业务所需身份仅在授权受控存储最小化使用。
- operator/runtime credential 分离，先鉴权再返回 replay，保持 Host/Origin 限制。resolver/model provider/ARTEX provider/Task Bridge JSON 与 operator token 文件须常规文件、mode 0600 或更严及当前大小上限。DSN 仅从环境读取，不加命令行入口；默认 loopback，不 startup 自动迁移，不承诺通用 .env loader。
- QQ 写使用对应 Task/Chat 许可、runtime identity/fence、幂等和 observation；ARTEX 等外部写沿用各自授权/outbox/checkpoint/reconcile，不强套 QQ permit。
- 最终 QQ 文本检查规则，按动作重验 owner、能力、目标、适用审批/授权/预算。CRM 联系动作上线前须接入 stop-contact 最终 dispatch 检查；本节未来要求不代表现有链已具备全部能力。
- 写请求 timeout/transport failure 视为 UNKNOWN；只对证明可安全重放的读取做有界 retry。
- 账号身份来自 runtime read-back；online 必须有新鲜心跳和身份绑定。过期状态为 stale/unknown，不得伪装 offline。
- 不得用模型输出、群聊内容、客户证据、网页搜索、Skill 文本或 ARTEX 数据覆盖服务端权限。
- 所有外部操作必须支持限流、停止、审计和故障恢复；遵守 QQ 平台规则、用户隐私和适用法律。
- 未获用户明确授权，不登录 QQ、不发消息、不加群/好友、不触发真实 NapCat/OneBot/ARTEX 写、不连接生产数据库、不调用付费模型、不修改真实运行目录、不重启服务或切换 writer。
- 本地 fixture 必须使用隔离 PostgreSQL/loopback fake HTTP/假 adapter；不得将生产 DSN 传入 fixture runner。

- 外部请求保持 timeout、响应体/并发上限、redirect policy、secret-safe errors。
- stop-contact 明确 workspace/customer/account 范围、撤销屏障及禁止换账号绕过；tombstone 不等于删除原文。

- 持久化记录证明已记录的状态，不自动证明证据内容真实或外部动作成功。Reserve 事务内不发外部网络请求；writer ownership 按 account + action family 保持单一有效 owner，拒绝陈旧 owner/fence。
- Character runtime/database/cache/persona/adapter 按账号隔离。Collector 保持 collection-only，Summary 经 account/group/window/version/expiry 校验后进入不可信 GroupContext。
- Go 使用 gofmt；TS 沿用 strict/NodeNext 与相对 `.js` import。只改 src、不手改 dist/lib；保留 pinned ChatLuna lifecycle patch，升级须重验。ID 按 schema 保持，QQ ID/epoch/fence/sequence 十进制字符串不转为丢精度 JS number，保持 required/nullable 严格解码；本地 `$ref` 禁远端、绝对、父目录及软链逃逸。
- run 版本快照不替代执行前当前授权与撤销检查。常规可逆实现选择自行推进，仅实质影响业务或授权的未决信息才追问。

## 9. 前端与生成资源

- 唯一产品 UI 源码：`frontend/`。
- 修改前端后运行仓库当前 `tools/build-product.py` 同步流程，检查源码与 `backend/cmd/taskengine-server/product/` 嵌入资源一致。
- 不直接编辑生成的嵌入副本作为唯一变更；不把待接入页面视为 API 已完成。
- 对新增/修改页面验证真实 HTTP API、权限、空状态、错误状态、未知状态、分页、刷新和浏览器行为。
- CI 当前覆盖范围不等于全部产品质量门禁；凡本次改动触及 Collector、前端嵌入一致性或浏览器交互，应在 PR 增加相应验证。是否将其纳入常驻 CI，应作为单独 CI 改进提交，不能假设当前 workflow 已覆盖。

## 10. 开发与验证命令

从仓库根目录执行。先检查 `npm run`、package scripts 和当前文件，确认命令存在后再运行。

```sh
# OpenAPI / contract 校验（不生成文件）
bash tools/contract.sh check

# Go Task Engine 隔离 PostgreSQL race suite
bash tools/kernel.sh test-all

# 主服务隔离 smoke / schema smoke
bash tools/kernel.sh smoke
bash tools/kernel.sh schema

# Character Runtime（Node 22；按本轮改动选择相关命令）
cd koishi/character-runtime
npm ci --ignore-scripts --no-audit --no-fund
npm run build
npm run typecheck
npm test

# 产品离线验收（仅在任务要求时生成新的证据目录）
cd ../..
bash tools/product-acceptance.sh fixture --output <new-evidence-directory>
bash tools/product-acceptance.sh verify --package <evidence-directory>
bash tools/product-acceptance.sh release-check --package <evidence-directory>
```

- Go 仓库测试通过 `bash tools/kernel.sh test-all` 的隔离环境运行；不要用裸 `go test ./...` 绕过 harness。
- `contract.sh prefetch` 可能联网，仅在工具缓存缺失且任务需要时运行。
- `contract.sh generate` 会改写生成代码；先确认任务需要，运行后审查完整 diff。
- Collector 的脚本和宿主依赖需按当前 package scripts/lockfile 核验；不要把 `lib/` 编译副本当作 `src/` 的验证结果。
- 只运行与本次变更相关且必要的验证；扩大验证范围时说明原因。任何失败必须调查，不能隐藏或通过删测试规避。

## 11. 强制 GitHub Pull Request 工作流

**每一轮开发必须遵循 PR 工作流。禁止直接修改主分支、跳过 CI、强制合并或忽略失败检查。**

### 11.1 开始前
1. 检查 `git status`、当前分支、remote、主分支名称和最近提交；确认没有覆盖用户未提交改动。
2. 获取最新远端主分支：`git fetch origin`。
3. 基于最新 `origin/main`（若仓库主分支名称不同，以实际 remote HEAD 为准）创建独立开发分支。建议命名：`feat/<scope>`、`fix/<scope>`、`docs/<scope>`、`test/<scope>`。
4. 禁止在 `main` 上直接编辑、提交或推送。若工作树不干净，先保护并确认已有改动归属，不得擅自 stash、丢弃或覆盖。

### 11.2 开发与本地验证
1. 先阅读本文件、相关 ADR、产品验收矩阵、能力矩阵、对应源码和测试。
2. 先确认当前实现和主服务实际装配；不要照搬过期任务书，不重复实现已存在的 handler/route。
3. 变更保持单一目的、最小范围；同时更新契约、migration、测试、文档和前端生成资源（适用时）。
4. 执行本文件第 10 节及本次改动涉及的 lint、typecheck、test、build、browser/collector/security/recovery 验证。
5. 提交前检查 `git diff --check`、完整 diff、敏感信息、生成文件、migration 和许可证边界。
6. 记录每条验证命令、退出码和实际结果。未运行必须写 `NOT_RUN` 和原因，不得声称通过。

### 11.3 提交与 PR
1. 在独立分支提交清晰、可审查的 commit。
2. 推送开发分支并创建 GitHub PR；不得直接推送主分支。
3. PR 描述至少包含：
   - 背景/问题与目标；
   - 变更范围和关键设计；
   - 涉及的 API/schema/migration/权限/外部动作；
   - 本地验证命令、退出码和结果；
   - CI 状态；
   - 风险、未覆盖项和回滚/恢复考虑；
   - fixture、controlled live、production 的明确边界。
4. PR 应关联 issue/task（如存在），并使用仓库 PR template（如存在）。

### 11.4 CI、审查与合并
1. 等待 PR **最新 head commit** 对应的所有 required CI checks 完成并通过。旧 commit 的绿色 CI 不算当前 PR 通过。
2. 任一检查失败，必须定位原因、修复、重新运行本地相关验证并推送新 commit，等待新 commit 的 CI 重新通过。
3. 必须满足仓库实际配置的 review approval、branch protection、required status checks 和其他规则；先检查实际设置，不得假设已启用或已满足。
4. 不得使用 admin bypass、强制合并、跳过检查、忽略 review 或把失败标记为非阻断来绕过规则。
5. 只有 required checks 全绿、审查规则满足且用户已授权本轮或有明确适用的持续合并授权时，才可合并。未获合并授权时，停在可审查 PR 并报告状态。
6. 不得把本地测试通过描述成 GitHub CI 通过，也不得把 CI 通过描述成真实 QQ/生产验收。

无 required checks 时仍核实预期实际触发作业在最新 head 全通过；空列表不是成功。不擅自更改治理配置。

### 11.5 合并后收尾
1. 确认 PR 已按规则合并，记录 merge commit。
2. 更新本地主分支并核对远端状态；按仓库策略清理已合并开发分支。
3. 检查是否需要同步更新 AGENTS.md、ADR、验收矩阵、能力矩阵、runbook 和 README；文档必须反映实际代码和证据。
4. 汇报 PR 链接、merge commit、CI 结果、验证命令、未覆盖风险和下一步任务。
5. 凭据、生产数据、数据库备份、运行二进制和本地历史证据包不得提交到公开仓库。

合并后 fast-forward 本地主分支，核对 remote SHA 和工作树。公开仓库不提交依赖缓存；Agent 审读不替代 GitHub 必需 approval。

## 12. 每个任务的完成定义（Definition of Done）

一个开发任务只有同时满足适用项才可标记完成：

- [ ] 目标、范围、非目标和授权边界清楚。
- [ ] 已从当前主分支基线创建独立开发分支。
- [ ] 修改与当前源码/实际 handler 装配一致，没有重复造轮子。
- [ ] API、OpenAPI、schema、migration、前端源码和嵌入资源（适用时）一致。
- [ ] workspace/account/permission/identity 隔离有测试。
- [ ] 幂等、并发、超时、UNKNOWN、取消、重启恢复（适用时）有测试。
- [ ] 负向测试证明未授权动作不会发生。
- [ ] 本地相关验证已运行，命令/退出码/日志可追溯。
- [ ] 当前 PR head 的 required CI 全部通过。
- [ ] Review/branch protection 满足，合并经过授权。
- [ ] 验收矩阵/文档更新且没有夸大能力。
- [ ] fixture、live 和 production 结论严格区分。

若某项不适用，必须在 PR 中写明理由；若被阻塞，状态应为 BLOCKED，不得以口头说明代替验收。

## 13. 当前事实核验与易踩坑

每次开发前以当前分支为准复核，以下是长期易错点：

1. **Schema 版本核对：** 当前 `SchemaHead()` 以 migration 源码和当前 binary 的实际结果为准。开始涉库工作时重新核对 migrations、`SchemaHead()`、server/bridge gate、迁移测试及隔离数据库，不在长期指南中固定当前 head。区分当前 migration head、主服务最低启动版本、Bridge 精确 head 门与历史证据版本；它们不是同一个值，不应机械替换为相同数字。
2. **B7 主路由：** 当前 GitHub `main` 的主服务已有对话/通知相关装配证据；后续是对当前提交做增量复核，不得沿用过期任务书声称完全未装配。每次仍须检查实际 `main.go`、resolver/API surface、handler、feature flags 和前端消费者。
3. **Agent 能力：** 已有 Agent/Skill/Model 绑定、provider 调用和搜索上下文；不等于完整多步工具执行 Agent Runtime。新增需求应明确具体缺口，不得低估或夸大。
4. **任务类型：** QQ-03 矩阵当前把四种新任务类型的执行标为 unsupported；“可识别/创建/读回”不等于可执行。
5. **Chat Permit：** 与 Task Engine Reserve 独立；不得强制统一成一条执行链。
6. **计划任务：** occurrence 与 task 创建必须有事务或稳定幂等恢复协议；原子领取本身不足以证明不漏单、不重单。
7. **CRM 计数：** 明确意向按 workspace 内去重；当前有效意向、历史确认、撤销和多账号归因分开统计；好友关系独立。
8. **消息不可变与隐私：** 当前消息表有禁止 UPDATE/DELETE 的触发器。任何生命周期设计都必须覆盖原文保留、脱敏/删除、证据失效、审计和备份清除；不得直接绕过约束。
9. **CI 覆盖：** 当前 workflow 覆盖 contracts、backend 和 Character 等指定作业；Collector、嵌入资源一致性和真实浏览器验证需按改动补充，不能假设已被 CI 覆盖。
10. **历史证据：** 历史 schema、commit、fixture 和作者报告只支持其原始范围；新代码/新 schema 必须产生新证据。
11. **UI 资源：** `frontend/` 与 Go embed 资源可能漂移；修改后必须运行同步脚本并检查 diff。
12. **Cleaner / adapter：** 候选代码和离线测试不证明已部署、已加载或真实 QQ 能力。
13. **ARTEX：** graph/outbox 是外部引用和同步边界，不是 BBA Task Engine 执行事实；ambiguous write 必须 reconcile，不得盲目重发。

14. 脚本副作用：dev-stack reset 删除对应 fixture 状态；generate 覆盖输出；产品 fixture 新建隔离资源/证据，先核对目标。
15. Bridge 历史日志 schema=22 不是数据库读数，核对当前 gate、core.schema_version、binary SchemaHead()。

## 14. 权威文档索引

开始具体任务时按需阅读：

- `README.md`：项目入口和验证摘要。
- `AGENTS.md`：本仓库开发与架构规则。
- `contracts/openapi.yaml`、`contracts/schemas/`、`contracts/examples/`：API 契约及正反 fixtures。
- `docs/adr/`：已接受架构决策；冲突先提出，不静默绕过。
- `docs/product-acceptance-matrix.md`：按行能力状态与证据边界。
- `docs/qq03-task-type-matrix.md`：QQ 任务类型当前支持矩阵。
- `docs/capabilities/`：adapter/runtime 能力事实与未知项。
- `docs/product-release-runbook.md`：备份、迁移、drain、恢复和发布流程；执行前核对当前 binary/schema。
- `frontend/NOTICE`：前端来源与许可证边界。
- 对应阶段任务书：仅作为该任务范围输入，必须与当前源码和本文件交叉核对。

## 15. 未决产品决策：不得擅自猜测

以下问题必须在对应任务开始实施前形成 ADR/任务书决策：

1. `customer.add_me` 的精确定义：是接收对方好友申请、引导对方添加，还是其他事件/动作？完成证据是什么？
2. `customer.add_customer` 的精确定义：是主动发送好友申请、处理已有请求，还是其他动作？真实 adapter 能力是否存在？
3. Scheduler 对每类任务的 misfire、overlap、DST 和审批策略；默认策略按第 4.3 节保守处理。
4. CRM 原文和证据的保留期限、删除/脱敏和备份清除策略；需按部署地区与业务目的评估，不由 Agent 自行编造期限。
5. 各阶段哪些动作仅生成草稿，哪些允许 fixture，哪些允许受控 live；真实外部动作必须逐项授权。
6. Agent 多步运行的最大步数、超时、并发、停止/恢复和工具失败策略；必须通过版本化配置和测试固定，不能靠模型自行决定。

**最终原则：先核实当前实现，再定义最小缺口；先建立授权和证据，再开放动作；先通过独立 PR 与 CI，再合并主分支。**
