# Product Acceptance Matrix

Status vocabulary: `未实现` = no implementation found; `实现未接线` = source exists without an active consumer; `fixture通过` = the named isolated path and its negative cases pass; `真实受控通过` = a bounded real external action was exercised under explicit authorization; `可运行发布` = deployed and operationally accepted. Status is scoped to the row and is not a product completion percentage.

The current closeout is based on the S1–S6 implementation in this tree and the permanent product runner. Historical fourth-round packages remain immutable evidence; they are not re-finalized. The final candidate must be generated after source closure with `tools/product-acceptance.sh fixture` and independently checked with `verify`.

| Requirement | Implementation / actual consumer | Permanent verification / evidence | Unified UI entry | Engineering / release status | Remaining boundary |
|---|---|---|---|---|---|
| Account identity, connection, login, risk and capability projection | `heartbeat.go`, account APIs, resolver and runtime projection | Go race suite; login protocol fixture; browser readback | Accounts / runtime | fixture通过 / LIVE_REQUIRED | Real account identity and live readiness still require authorization |
| In-console QR login and instance management | `login.go`, `login_http.go`, `0012_login_sessions.sql`, fixture provider | login lifecycle, generation fence, cancellation, identity readback and browser evidence | Account login | fixture通过 / LIVE_REQUIRED | NapCat provider and real QR use are not authorized |
| Task creation, scoped approval and controls | `task.go`, `task_http.go`, `proposal.go`, `proposal_http.go` | Go race suite; authenticated browser fixture | Tasks / risk | fixture通过 | No live QQ action is enabled by the fixture server |
| Task listing, detail, counters and graph projection | task HTTP handlers, `graph.go`, embedded console | Go race suite; graph/browser scenarios | Task detail / graph | fixture通过 | BBA graph is derived state, not ARTEX truth |
| Pause, resume, cancel, deadlines and authorization revision | task controls, scheduler, bridge and proposal work reserve gates | claim-cancel-reserve, stale authorization, deadline and restart tests | Task controls / risk | fixture通过 | External effects already in flight remain UNKNOWN/observed |
| Persistent worker rotation and restart recovery | `worker.go`, `worker_cursor.go`, `0009_worker_cursor.sql` | full Go race suite and restore rehearsal | Operations | fixture通过 | Production worker deployment is LIVE_REQUIRED |
| OneBot identity and group snapshot | P0-14 read-only commands and identity projection | preserved controlled read evidence plus offline fixture | Accounts / operations | 真实受控通过 for named historical read; fixture通过 for current package | New external reads need a new scope and authorization |
| Controlled QQ business writes and fault injection | bridge, permit, observation and P0-20 executor | isolated unknown/no-resend and recovery fixtures | Task trace / risk | fixture通过; LIVE_REQUIRED | No real write, test group or live fault injection was authorized |
| Collector whitelist and durable ownership | `koishi/analysis-collector/src/runtime.ts` and boundary | Node22 suite; cross-module runner | Analysis report | fixture通过 | Production collector deployment is LIVE_REQUIRED |
| Durable analysis windows and L0 statistics | Collector window store and `runSummaryAnalysisJob` | cross-module two-account/two-group windows, duplicate and restart checks | Analysis report | fixture通过 | Synthetic source messages only |
| Summary.v1 validation, persistence and GroupContext | Summary producer/publisher, `summary.go`, `group_report_http.go` | contract suite; cross-module Summary identity and expiry checks | Analysis report / group context | fixture通过 | No real QQ semantic corpus or real-model quality claim |
| Read-only report display and controlled report route | Group report handler and existing same-origin console | browser report context and 403/empty/error checks | Analysis report | fixture通过 | Report sending is not enabled as an unapproved side channel |
| Character account isolation and text permit | `koishi/character-runtime/src`, gate, backend permits and boundary transport | Node22 23/23 suite; cross-module prompt/permit/transport checks | Chat trace / risk | fixture通过 / LIVE_REQUIRED | No production model or outbound transport was used |
| GroupContext injection into actual Character prompt | `group-context-provider.ts`, prompt plugin and ChatLuna runtime | cross-module real prompt path with Summary id/version/window identity | Group context / chat trace | fixture通过 | Context is untrusted reference data and cannot authorize work |
| Character lifecycle without teardown fault | pinned ChatLuna 1.4.4 patch and `test/lifecycle.test.ts` | three start/stop cycles, internal error capture and listener counts | Operations diagnostics | fixture通过 | Upstream/version changes require patch revalidation |
| Scoped model provider and bounded gateway | `model_provider.go`, `model_http.go`, provider config and fixture model | model protocol, protected config, concurrency deadline and replay checks | Model settings | fixture通过 / LIVE_REQUIRED | Real model quality and capacity are not claimed |
| Frozen model holdout and quality rubric | `evals` holdout/rubric/provenance artifacts and cross-module proof | hashed fixture-only holdout; no real evaluation flag | Evaluation status | fixture通过 / LIVE_REQUIRED | Human review and real model scores remain required |
| Proposal intent, approval and legacy compatibility | `proposal.go`, `proposal_http.go`, migrations 0006/0008 | approval/digest/version/scope tests; legacy apply remains intent-only | Plan review | fixture通过 | Model output cannot apply or send |
| Explicit proposal orchestration mode and materialization | `proposal_work.go`, `proposal_work_http.go`, migration 0010 | materialize idempotency, no-send-before-worker and mode switch tests | Plan materialize | fixture通过 | Feature flag and operator authorization remain required |
| Durable work items, DAG, attempts and fences | `work_item`, `work_item_attempt`, proposal worker and existing Reserve/Bridge | concurrent claim, stale lease, dependency, cancel and exact causal trace tests | Plan trace | fixture通过 | External effects remain subject to bridge ownership |
| UNKNOWN and crash recovery without resend | proposal worker, bridge recovery, observation reducer | claim/reserve/dispatch crash fixtures and restore rehearsal | Unknown / reconcile | fixture通过 | Live provider reconciliation remains external work |
| ARTEX verified REST adapter and protocol boundary | `backend/internal/artex`, `artexfixture`, local protocol notes | graph protocol and client error/retry/write-negative tests | Provider diagnostics | fixture通过 / LIVE_REQUIRED | Local ARTEX inspected: tag `v0.3.23`, HEAD `620ca5d...`, AGPL-3.0; no live endpoint used |
| ARTEX fact/goal roots, anchors and mappings | `graph_sync.go`, migration 0011, `GraphSyncWorker` | graph chain Summary/event→outbox→external fixture→mapping checks | Dual graph | fixture通过 / LIVE_REQUIRED | Live ARTEX credentials and endpoint acceptance remain absent |
| ARTEX outbox/checkpoint and reconciliation | `graph_sync_worker.go`, graph HTTP, adapter write boundary | stale lease, timeout, duplicate, partition and no-resend tests | Graph sync status | fixture通过 | Ambiguous writes require operator readback |
| Bounded planning snapshot and stale rejection | `graph_sync_plan.go`, snapshot tables and proposal binding | stale graph/Summary/task/auth revision, expiry and round-budget tests | Plan basis | fixture通过 | Snapshot never widens task scope |
| Unified same-origin console | `taskengine-server/web`, server route assembly | authenticated browser checks and refresh GET-only behavior | Existing console | fixture通过 | Live deployment and credentials are separate |
| Accurate modes and failure states | runtime flags, feature gates, HTTP error mapping and UI readback | browser 403/409/5xx/offline/empty/paused/cancelled/unknown scenarios | Existing console | fixture通过 | Fixture/dry-run are not live authorization |
| Keyboard, narrow viewport and secret handling | embedded UI, in-memory token, sanitized runtime projection | browser keyboard/mobile screenshots and secret scan | Existing console | fixture通过 | No browser-held NapCat secret |
| One-command engineering acceptance and evidence | `tools/product-acceptance.sh`, `product_fixture.py`, `product-browser.cjs` | fixture records raw streams/argv/UTC/exit, verify checks business assertions and tamper negatives | Operations | fixture通过 | Release gate deliberately remains fail-closed |
| Deployment, migration, backup/restore, rollback and monitoring | `product-release-runbook.md`, `taskengine-migrate`, `product-restore.sh` | isolated schema-21 restore, fingerprint equality, zero resend and cleanup | Operations | fixture通过 / LIVE_REQUIRED | Production rehearsal and sole-writer evidence remain required |
| Authorized external release acceptance | release evidence schema and `release-check` | missing prerequisite returns exit 3 and `RELEASE_BLOCKED` | Release checklist | LIVE_REQUIRED | Requires named scope, accounts, actions, window and stop conditions |

The final engineering state is independently represented by `acceptance.json` in the newest candidate package. `ENGINEERING_ACCEPTED` never implies `RELEASE_ACCEPTED`.


## B4 工作并发设置限定收口（2026-11-01，sol 作者自测）

仅工作并发默认与首次启动固化链：隔离 fixture 自测通过；Astra 最终独立验收待执行，不代表系统配置全部完成或可发布。
证据：evals/results/sol-artex-b4-settings-closeout-20261101-032505。完整 Go race 保持180s；smoke、Character 默认包与 npm build/typecheck/test、B1/B2及 source/embedded 会话、B3/B4在线均有最终成功子退出。保留先前失败，不以外层0替代。
B4真实产品页面保存1/2、刷新/重启、实际消费者N1/N2、任务详情固化1/2、只读403、真实409、退出/切账号迟到200与409、DB失败不显示后备均按runner检查。1440/1024/390截图已视觉审读，无明显遮挡/横向溢出；窄屏辅助文字较密、个别拆词为非阻断可读性限制。
全局代理、网络搜索及其余系统设置仍待接入/逐项验收；真实QQ写、生产迁移部署、付费模型不在本轮授权内。

## B6 本批状态：部分实施，未验收 / no_ship

B5 仍仅模型出口代理核心限定通过，以 Astra b5 closeout 为准；不重开已关闭持久化/目录缺陷。B6 新增 Brave/Tavily 有界协议适配、workspace CAS 配置/审计、Agent web_search 历史字段和显式 search 查询规划消费路径，以及中文设置 UI 和嵌入同步。适配器本地协议测试和已有 Agent 定向回归通过，但尚未证明页面保存→同一真实产品进程→搜索 fixture→模型请求→规划回执；不得声称完整功能通过。完整 test-all race 本次180秒超时退出1，不以增加超时替代。DDGS、DeepSeek 禁用；代理组合、跨workspace并发、CAS/重启/迟到会话新字段验证、三屏截图、B1/B2/B3/B4/B5/Character/npm 最终回归未完成。所有先前未实现完整产品目标保留，不扩CRM/QQ写/调度器/通知，不发布或迁移。详见 .b6-path 指向本批 REPORT.md。


## B6 repair 作者部分交付（2026-11-01）

证据：evals/results/sol-artex-b6-repair-20261101-060244。原180秒完整race退出0（461顶层PASS/8SKIP，taskengine171.176s）；专用真实10秒墙钟门独立退出0。新增永久真实生产二进制runner，浏览器搜索保存→同PID真实resolver/registry→官方域TLS搜索fixture→模型独立untrusted user数据→规划202/receipt，搜索1模型1，重放不增调用，变query409。修复搜索/模型active拼键碰撞，独立map。B5两runner、B3/B4 live、smoke、默认Character退出0；source/embedded会话及SHA一致。B1/B2最终组合失败，npm三项未执行，恶意runner边界/出口组合/真实在途快照/跨workspace网络并发仍待补，三屏DOM通过但未视觉审读。非Astra独立验收，RELEASE_BLOCKED；保留此前历史状态与失败。详见本轮REPORT.md。

## B6 closeout 作者验证（2026-11-01，待 Astra 独审）

证据：evals/results/sol-artex-b6-closeout-20261101-063851/REPORT.md。B1 权限态断言和 runner 目录/nonce 边界已修复；真实在途快照、双 workspace、三路网络并发及实际 HTTP 零增量拒绝已补证。最终原180秒完整race退出0（463顶层PASS/9SKIP）；真实10秒专用测试、B6产品、B1/B2与源/嵌入会话、B5两runner、B3/B4、smoke、默认Character及npm三项均退出0。历史失败原样保留。三屏截图已实际看图，代理复选框与标签分离、长选项及低对比度为残余视觉问题；不声称视觉无问题。B5历史凭据候选/来源不完整仍未核销，候选不等于泄漏。完整产品目标和其他 LIVE_REQUIRED 不变；非Astra验收，不发布。

## QQ 产品目标增量（2026-11-01，需求已确认，当前未整体验收）

本节把用户确认的 QQ 私域运营控制台目标加入长期矩阵。状态只表示当前代码证据的范围，不是完成百分比；历史 B7/B8/B9 交接声明不自动升级为当前源码事实。

| ID | 目标 | 当前源码证据 | 本轮目标/验收证据 | 当前判定 |
|---|---|---|---|---|
| QQ-01 | ARTEX 风格仪表盘；QQ 在线/离线/异常/总数；资产节点显示明确意向客户数；删除流量交互、活动流、发现、流量状态码 | `backend/cmd/taskengine-server/product/app.js` 已消费账号概览；资产数仍为待接入占位，删除项需以 DOM/API 复核 | dashboard API 和页面在 fixture 中读同一持久投影；过期/未知不伪装离线；客户去重计数有证据 | 部分实现，未验收 |
| QQ-02 | 双对话：Agent 线程与 QQ 收件箱；出站仅草稿/规则回执 | `operator_conversation.go`、migration 0016、主服务装配及 `frontend/app.js` 已有消费者 | 见下方 B7 分项矩阵；以当前提交增量验收，不重复旧任务书 | 已实现并接线部分基础链，完整独立实跑待验收；不含完整 Agent run 或真实 QQ |
| QQ-03 | 任务覆盖加群、分析群、聊天、客户加我/我加客户 | 当前 Task Engine 有任务生命周期和历史 `group.acquire` 契约；尚无 QQ 业务全类型的独立消费者证据 | 每种类型有显式 schema、能力检查、审批/permit 绑定、unknown 与不重发测试 | 工程切片已交付（离线 fixture，作者自测）；决策矩阵见 [`docs/qq03-task-type-matrix.md`](qq03-task-type-matrix.md)，待独立验收 |
| QQ-04 | 计划任务复用任务引擎，支持时区、下次执行、重启补偿、重叠幂等 | 产品入口存在，后端/前端完整消费者未形成 | 计划持久化、调度回执、任务生成和重启/重叠测试 | 未接线 |
| QQ-05 | 资产表示明确意向客户；QQ 标识、来源、证据、规则版本、时间和归属可审计 | 产品资产入口为待接入；当前没有可引用的客户证据链 | 意向去重、证据撤销、人工修正和 dashboard 计数回读；明确意向即计数，好友关系单独记录 | 未接线 |
| QQ-06 | LLM 保留现有模型注册、密钥保护、代理、搜索和并发设置 | B4/B5/B6 已有 fixture/作者证据；设置 handler 对 nil registry 有显式 unavailable 返回路径，主装配覆盖不足 | 配置/未配置两种主服务装配的同进程 HTTP 回归；不泄露 secret，不改变现有作用域 | 部分实现，未充分独立验证 |
| QQ-07 | Agent 保留配置能力，未来执行域绑定 QQ 工具；模型不能获得 QQ 授权 | Agent/Skill 配置 handler 已装配；没有 QQ 工具执行消费者 | 仅通过显式 Skill/工具/permit 链；模型输出不能直接 materialize/send | 配置已存在，QQ 接线未实现 |
| QQ-08 | MCP 接入 ChatLuna、Character、group-analysis，区分仓库、插件和 MCP endpoint | 产品入口仍待接入；仓库 URL 本身不是 endpoint 或已安装能力 | 连接/版本/能力/健康状态可读，未配置时 fail closed；fixture 不冒充在线安装 | 未接线 |
| QQ-09 | Skill 设计为版本化 QQ 运营流程：群分析、意向识别、关键词策略、聊天辅助、客户跟进 | 当前 Skill 是配置/版本管理，未绑定 QQ 业务对象 | Skill 只能产生受 schema/权限约束的建议；每次运行绑定规则/工具/版本，不授予权限 | 设计待实施 |
| QQ-10 | 工具目录来自 NapCat 原生 API，目录、schema、运行 capability、调用授权分离 | `product/tools.json` 只是嵌入只读目录；尚无已验证 NapCat adapter/调用链 | fixture 能力声明、参数校验、permit/bridge/observation 回执；真实调用另行授权 | 目录存在，运行时未接线 |
| QQ-11 | QQ 入站/草稿等本地通知，未来扩任务/客户事件 | `operator_notification`、handler 与 `loadNotify/notifyRender` 已接线 | 见 B7-04/05/06；其余事件源与外部推送分别验收 | 本地投影部分实现/接线；完整验收待执行，外部推送未授权 |
| QQ-12 | 拦截规则从命令拦截改为关键词/模式拦截，作用于最终出站文本 | `content_rules`/`content_review` 和前端配置/试跑已有；需确认所有 writer 都经过最终检查 | 单段文本规则版本、命中证据、阻断不发 permit；任何直连 writer 回归失败 | fixture 部分通过，需扩大消费者覆盖 |
| QQ-13 | 审批记录覆盖任务、草稿、规则、工具和外部效果 | 现有 proposal/content review 路径分开；产品页有内容审批 | 每条审批绑定 payload digest、scope/version、决策和最终回执；批准不等于发送成功 | 部分实现 |
| QQ-14 | 日志沿用，但必须能关联 QQ 账号、会话、任务、工具和通知，不回显凭据/原文 | 当前有运行日志/审计表，但 QQ operator 查询面未形成 | 只读分页、脱敏、request/event correlation 和失败状态可回读 | 未接线 |
| QQ-15 | 系统配置至少保留全局代理、网络搜索、Work Agent 数；其余按 QQ 消费者保留 | Work Settings、proxy、search 已有不同程度路径；registry 未配置时设置路由可能进入 nil receiver | 配置和未配置路径均有同进程测试；设置真实影响模型/搜索消费者 | 部分实现，需修复/独审 |

### 已确认的外部范围

本轮仅允许代码、migration、fixture 和离线验收。任何测试都不得登录 QQ、发送消息、加群、加好友、启动真实 NapCat/ARTEX/6300、写生产数据库、安装在线 MCP 或调用付费模型。fixture、静态注册表、作者报告和菜单存在都只能支持工程层结论，不能支持真实 QQ 或发布结论。

## B7 当前基础链分项核对（2026-11-02）

本节覆盖上方 QQ-02、QQ-11 的过期“待接入”描述，不改写历史 S1–S6/B4/B6 证据。源码核对基线为 `4d648beed7e7be07ee0c738aed0456790cce267b`，本次只改文档；migration 源码 head=25，不代表本次打开或验收了数据库。

证据口径：下表“源码审读”由 Sol 对当前文件执行，仅为本轮源码核对，不主张独立于历史实现作者；独立审查/独立实跑均 NOT_REVIEWED/NOT_RUN，也不是 GitHub approval。列出的永久测试经阅读确认存在，不因存在就标 PASS。原作者历史 B7 报告未在本次按行重新核销，作者验证一律记“历史报告存在，当前分项 NOT_VERIFIED”；历史未绑定当前 commit 的结果不补造为当前证据。当前基线的 [PR #2 CI](https://github.com/sunhaozhe-glitch/BBA/actions/runs/41391966815) 全绿，只证明对应 contracts/backend/character 作业范围，不是产品浏览器或 live QQ 验收。

下表的实现/接线结论仅限所列路径；所有行均为 `NOT_RELEASED`，受控 live/production 均 `NOT_RUN`。独立运行验收在下一任务按当前提交补证。

| ID / 验收项 | 当前实现与实际消费者 | 永久测试 / 可定位证据 | 作者验证 | 本轮独立审读 / 独立实跑 | 尚需验收 |
|---|---|---|---|---|---|
| B7-01 Agent 线程基础 | `operator_conversation.go` 的 `agentMessage`、`messages`；0016 的 conversation/message 表；前端线程消息 POST/GET。基础持久化接线，未启动模型 run | `TestConversationSourceBoundary` 检查 Agent 消息不进入 QQ 出站或生成 permit | 历史报告存在，当前分项 NOT_VERIFIED | 源码确认路径；实跑 NOT_RUN | 线程创建/读取、跨 workspace 读写、重复/并发请求、刷新与错误；不由 QQ draft 隔离测试代替 Agent 线程隔离验收 |
| B7-02 QQ Inbox | runtime 入站 `ingestEvent`，conversation/message GET，前端 `loadChat/loadChatMsgs`；按 workspace/account/kind/external ID 建会话。API 有 limit/cursor 与 next_cursor | `TestInboxEventIdempotency`、`TestConversationSourceBoundary`、`TestUnscopedOperatorSeesSeededRows`：同事件重放/异 digest、来源边界与稳定读取 | 历史报告存在，当前分项 NOT_VERIFIED | 源码确认路径；实跑 NOT_RUN | 多账号/群/私聊与跨 workspace 完整隔离；并发入站；跨页无漏重；消息明细 account scope；runtime→真实 QQ 接入未验收 |
| B7-03 QQ Draft | `createDraft/getDraft` 与前端 draft POST；记录 digest/rules_version/decision/evidence，回执 `send_permit=absent`，无 writer 调用 | `TestDraftContentRuleAndPermitBoundary`、`TestConversationWorkspaceAccountIsolation`：阻断、零 permit、跨 workspace draft 读取拒绝 | 历史报告存在，当前分项 NOT_VERIFIED | 源码确认路径；实跑 NOT_RUN | 正常/阻断/unsupported 回执、重复与并发、账号范围、UI 刷新/错误；不宣称草稿具备已发送效果或真实 permit 发放 |
| B7-04 本地 Notifications | Agent/inbox/draft 事务投影，event_key 去重；列表 limit/cursor，已读 version CAS；前端 GET/mark POST；local_only | `TestNotificationProjectionIdempotencyAndReadCAS`、`TestInboxEventIdempotency`、`TestUnscopedOperatorSeesSeededRows` | 历史报告存在，当前分项 NOT_VERIFIED | 源码确认路径；实跑 NOT_RUN | 并发已读、跨账号读/更新隔离、跨 conversation 同 event_id、分页/刷新/错误；尚不覆盖任务/CRM 全部事件、订阅重试或外部推送 |
| B7-05 主服务装配/鉴权 | main.go 初始化 OperatorConversationHandler；resolver.go 的 serverAPISurface 与 operatorConversationAuth 分发 operator/runtime | `TestServerAPISurfaceRoutesOperatorConversations`、`TestConversationAndNotificationHTTPAuth`、`TestInboxEventIdempotency`；前者是委派测试，不等于真实主进程端到端 | 历史报告存在，当前分项 NOT_VERIFIED | 源码确认已装配；实跑 NOT_RUN | 真实 binary+resolver+隔离 DB 的授权/未授权、runtime/operator 互斥与 account scope；不能以 route 存在判全链通过 |
| B7-06 前端消费者 | `frontend/app.js` 的 loadChat/renderChat/bindChatForm/loadChatMsgs/loadNotify/notifyRender 使用实际 API；本轮比对 app.js 与 embed 字节一致 | `tests/product/test_b7_manual_fixture_launcher.py` 为 launcher 测试资产；不等于浏览器断言已运行 | 历史报告存在，当前分项 NOT_VERIFIED | 源码和 app.js 同步检查通过；浏览器实跑 NOT_RUN | 实际 HTTP、登录/退出/切会话迟到响应、权限、刷新、空/错态、完整翻页与三屏；当前首屏请求不能证明 next_cursor 已消费 |
| B7-07 Agent 完整运行 | 现有 model/provider 与 Proposal 管线独立存在；agentMessage 只存入站消息，无 thread-to-run/model dispatch。完整运行 PARTIAL / NOT_WIRED_TO_THREAD | 现有模型/Proposal 测试不能替代 thread→run→step→工具结果的端到端证明 | 完整运行无本次可核销证据 | 源码确认边界；完整链 NOT_RUN | 按 AGENTS M5 单列多步/停止/恢复/工具授权/幂等，基础消息通过不自动升级此行 |

### 下一轮 B7 增量独立验收

从新的最新 main 锁定 commit/build/schema，用隔离 fixture 执行上表现有测试和真实服务/浏览器链，逐行记录命令、退出码、原始证据与缺口；优先核验 account scope、分页、并发/重放与会话切换。不重复实现已有 handler。任何发现的缺陷单独复现、修复、验证，不能凭本次审读标全项 PASS。始终不登录/发送/加群/好友，不调用付费模型，不接生产数据库。

### 2026-11-04 M1：PR #8 来源边界增量核验（候选分支）

基线 main `d43f91a9ecac6fc3f8d704a660f8882f64f40721`，当前工作分支 `codex/m1-b7-source-boundary`；保留原 #8 提交祖先并整合该 main，未将候选分支写成已合并或发布。下面是 `AUTHOR_SELF_TEST` / `ISOLATED_FIXTURE` 的增量证据，不改写上表历史独立执行状态。

- **基线失败已复现：** 隔离 harness 中 `TestConversationAgentAppendOnExistingQQID` 向 QQ 会话的 Agent 消息路径请求时错误返回 201；对应请求能通过现有主服务 operator 路由，不只是假想的未来 account scope。
- **候选兼容性失败已修正：** 原 #8 要求内部 `id == conversation_id`，会拒绝 migration 允许且现有 B7 seed 使用的合法 Agent 行；新用例 `TestConversationAgentAppendToExistingNamedThread` 在原补丁下 404，在调整后 201。当前检查同一事务中的 Agent kind/type 及无 QQ account，不新增 ID 等式约束。
- **定向测试：** `TMPDIR="$PWD/build/test-tmp" bash tools/kernel.sh test -run '^TestConversationAgentAppend' -v` 退出 0；覆盖 QQ 群/私聊拒绝、runtime 身份拒绝、合法创建/追加、跨 workspace 及拒绝时消息/通知边界。
- **真实主程序隔离 HTTP：** 新建 Go binary + PostgreSQL fixture，operator 对 QQ 群/私聊的 Agent POST 均 404/RESOURCE_NOT_FOUND，reader 为 403，runtime 为 401；四次拒绝前后会话/消息/通知/Chat Permit/dispatch permit 总数一致。已有命名 Agent 追加和新 Agent 创建/再次追加成功，QQ 会话无 Agent 来源消息；最终两类 permit 数均为 0。
- **既有浏览器流程：** 同一临时服务上 B7 smoke 13 断言通过；schema check 为 25/head=25，临时容器与端口清理通过。

范围仅支持 B7-01/02/05 的上述子项；未验证 #9 通知身份、#10 account/runtime 撤销与 epoch、#11 principal 幂等/精确重放，未覆盖分页、所有并发或真实 QQ。原始日志、构建身份和范围在本机未跟踪证据目录与 [本轮实施记录](superpowers/plans/2026-11-04-b7-agent-source-boundary.md) 中定位。CI、审查及合并状态须在原 PR #8 的最新 head 核对，不能沿用旧绿灯。

### CI 与仓库治理核对（2026-11-02 快照）

- 当前 `.github/workflows/ci.yml`：contracts（合同/TS/前端语法/插件）、backend（隔离 Go race/build）、character（build/typecheck/test）。未独立覆盖 Collector、完整产品浏览器、frontend/embed 一致性。本轮仅补文档；这三项增强作为独立 CI PR，路径条件设计需避免必需检查被跳过后永久 pending，不混入业务 PR。
- 通过 GitHub API 查询 `repos/sunhaozhe-glitch/BBA/branches/main` 返回 `protected=false`，`repos/sunhaozhe-glitch/BBA/rulesets` 返回空数组。当前未启用 main 分支保护/ruleset，并非“已核实启用”或“尚未核实”。没有仓库强制的 approval/required check 门禁不等于可绕过 AGENTS 的 PR/最新 CI 规则。本轮不修改仓库设置；未来启用审批/禁止绕过前需确定实际审查者及治理策略，避免单人仓库无法合并。


## M1 B7 通知身份增量核验（2026-11-04，PR #9）

基线 main `73a8156`，复用原候选 `31489e6`，由本地模型子代理实现/作者定向测试，主代理完成源码审核与 `INDEPENDENT_EXECUTION / ISOLATED_FIXTURE`。本节只更新 B7 通知身份范围，不覆盖其余历史待验收项。

- 通知身份为 workspace + 持久 conversation + event_key；同一 event_id 跨账号、群/私聊各自投影，同范围重放不新增。NULL conversation 仍按 workspace 去重。
- 作者定向回归证明未修复主线仅 1 条（应 3 条）；修复后跨范围、并发单投影、通知失败整事务回滚和真实 schema 25→26 保留历史 id/read/version/NULL 行通过。
- 主代理完整 `TMPDIR="$PWD/build/test-tmp" bash tools/kernel.sh test-all` 退出 0；后端代码/测试输入摘要在运行期间不变，部分 package 无测试。当前 binary + 新建 PostgreSQL + 真实 resolver/HTTP 的 17 项断言退出 0；没有发放 Chat/dispatch permit，fixture 容器与端口已清理。
- 原始独立证据在本机忽略目录 `evals/results/review-notification-full-20261104/`、`evals/results/review-notification-http-20261104/`；作者日志见计划。提交后的变更仅为审查文档，需核对代码输入摘要；最新 head CI 和实际合并状态在 PR 动态检查。
- `serverMinimumSchema` 仍为 24。新旧通知冲突目标不能跨 schema 混跑；停止旧写入后显式迁移，再切换匹配 server/Bridge，禁止只回退 binary。迁移不会补回历史丢失通知；本轮未执行现存数据库迁移、真实 QQ 或部署，发布为 `NOT_RELEASED`。

源码、升级方式、测试边界见 `docs/superpowers/plans/2026-11-04-b7-notification-scope.md`。账号/runtime 修复单独在 #10 核验；本节不是 B7 全项验收或生产放行。


## M1 B7 账号读取与 runtime 身份组合核验（2026-11-04，PR #10 依赖 #9）

基线 main `73a8156`，runtime 实现 `5298832` 保留原候选 `770e776` 的祖先；整合通知分支 `74df669` 得到 `88833cc`。子代理完成实现和作者红绿测试，主代理完成源码审核与 `INDEPENDENT_EXECUTION / ISOLATED_FIXTURE`，没有替代人工 approval 或生产验收。

- QQ inbox 将 resolver 的可信 token OwnerEpoch 传入 Actor，按 account→credential 共享锁后复用 canonical runtime 授权；fresh/replay 都验证当前 owner、active、账号 scope 及三方 epoch。原候选同步轮换后接受旧 token 的缺陷已用红测复现，最终旧 token 拒绝、新 token 正向通过。
- handler 的账号受限消息读取、撤销/缺失/错误身份和正向控制通过。文件 resolver 仍不支持账号受限 operator token；该读取证据只覆盖 handler/custom auth，没有扩大主服务配置权限。
- 四类运维 mutation 先持锁的永久回归通过 PostgreSQL 实际阻塞关系观测；提交 owner 改变、撤销、credential epoch 或同步轮换后，阻塞中的 inbox 拒绝，全部持久表快照只包含已授权 mutation。未穷举所有运维锁序或并发 replay 排列。
- 组合实现 `88833cc` 的完整 `TMPDIR="$PWD/build/test-tmp" bash tools/kernel.sh test-all` 退出 0；backend/测试输入运行期间不变。当前 binary + 新建 PostgreSQL（head 26）+ 真实 resolver 的组合 HTTP 29 项断言通过，包含通知四作用域/重放/已读 CAS、旧/新 token、#8 正负向及两类发送许可为零；fixture 已清理。
- 独立原始证据在本机忽略目录 `evals/results/review-runtime-full-20261104/`、`evals/results/review-combined-http-20261104/`；作者原始日志/范围见 `docs/superpowers/plans/2026-11-04-b7-account-runtime.md`。后续提交仅同步审查文档，须核对 backend 与构建输入摘要；最新 head CI 和实际合并状态在 PR 动态检查。

#10 的 PR base 为通知分支，便于只审 runtime 差异；先合并 #9，再将 #10 对齐 main 并重新核对 CI，不能把依赖分支当作最终发布目标。沿用 #9 的迁移和新旧 binary 兼容限制。本轮未合并、迁移现存 DB、连接真实 QQ/模型或部署，发布为 `NOT_RELEASED`；#11 principal 幂等、完整 B7/Agent run/生产发布均不在此结论内。


## M1 B7 操作员幂等闭环增量（2026-11-04，PR #11 更新中）

基线 main `5aaef18`，原 PR #11 `4715e12` 的 base 是旧依赖快照，不能将其前置能力当作主线事实。本地模型子代理只完成初次 RED 移植；后续默认模型子代理完成实现，主代理独立审核与执行。旧依赖的 inbox/runtime/UI 等差异不在本次变更内。

- 主线实际缺 Agent/Draft 的持久账本前置：同主体重试、跨主体同 key 均冲突。永久 RED 与真实旧 binary HTTP 均复现；保留初次编译错误记录，不把编译失败当业务 RED。
- 新链复用 `core.idempotency`；principal→conversation 锁下，消息/草稿/本地通知/账本一次事务提交；同主体重放原回执，异有效请求冲突，跨主体同 key 独立。Draft 摘要覆盖账号/类型/目标/文本/版本；QQ writer、Task/模型链未启动。
- 当前权限、主体、account/source 仍在重放前校验；原合法命名 Agent 会话兼容。新写规范路径拒绝末尾 slash；规范请求有界读历史 canonical/单 slash 账本，冲突不猜选。整数无损解码覆盖 rules_version 与嵌套 evidence。
- 无账本的旧 raw-key 不补造回执：Agent 当前 thread/key 保守拒绝；Draft 按创建主体跨历史目标拒绝，防止改目标绕过。旧有账本保持原 event_id/响应。有原始消息不等于有可恢复的请求身份。
- 作者定向 `tools/kernel.sh test` exit 0，33 个顶层测试 PASS；主代理完整 `TMPDIR="$PWD/build/test-tmp" bash tools/kernel.sh test-all` 退出0，运行期间后端/测试输入不变；部分 package 无测试。主代理当前源码 binary + 全新 PostgreSQL（head26）+ 文件 resolver/HTTP：53 项断言 PASS，涵盖旧 main/旧 ledger binary→新 binary、并发/丢回执/重启、权限撤销、回滚、数字精度、零 Chat/dispatch permit/task/model；容器、服务和端口清理通过。
- 主代理撤回了无关 inbox 校验一行后重新执行最终完整 race 与 HTTP，均通过；整段 inbox 实现与 main 一致。原始证据：本机忽略目录 `evals/results/m1-principal-author-20261104/`、`m1-principal-final-full-20261104/`、`m1-principal-final-http-20261104/`、`m1-principal-review-20261104/`。未提交树证据记录代码/测试摘要；提交后须重新核对一致，CI 以最终 PR head 为准。

范围仅支持 B7-01/03/04/05 的请求幂等子项；**Draft/inbox 并发 seq 分配仍非同一锁协议，可能一方409回滚**。完整分页/浏览器/契约与多步 Agent run 未验收；本轮无 wire/DDL/前端/生成类型改动，OpenAPI 覆盖仍待下一项。没有新 migration 不等于新旧业务写入可混跑：切换须先停旧 Agent/Draft 写入；只回退 binary 会恢复缺陷，不能当作安全回滚。本轮未合并、部署、迁移现存库或调用真实 QQ/模型，`NOT_RELEASED`。


## B7 既有 HTTP 契约入口与验收边界

`contracts/openapi.yaml` 记录八个已装配操作：会话列表、会话消息列表、Agent 消息追加、QQ 草稿创建/读取、runtime QQ inbox、通知列表与通知 read CAS。请求/响应的十三个公共组件见 `contracts/schemas/operator-conversations.json`，合成正反样例见 `contracts/examples/operator-conversations.json`；独立 wire oracle 为 `tests/contract/test_operator_conversation_contract.py`。这些操作沿用当前 handler/resolver；没有新增业务执行、权限、migration 或前端接线。

| 范围 | 合同保证与消费者 | 验证入口 | 仍须单独核验 |
|---|---|---|---|
| 八操作 wire | operator/runtime Bearer 分域、字段 required/nullable、数字 seq/version、review-only/permit absent、本地通知、两种错误 envelope（解码 400/415 的 text/plain 媒体为含末尾换行的 JSON 文本字符串，另行解析后检查错误对象）、RFC3339 offset 时间 | `bash tools/contract.sh check`；`generate` 同时验证生成 Go/TS 类型和所有命名组件的 fixtures | 真实 binary/resolver/隔离 DB 的请求响应、当前 auth/epoch、分页/replay/CAS，不由合成 fixtures 证明 |
| 边界差异 | UTF-8 字节上限另以描述记录；通知 read 省略/null→false；Inbox 幂等头仅非空、按 event_id 去重；输入 offset 有上限但输出 next_cursor 可能超过它；GET 历史 evidence 为任意 JSON | 独立 contract 回归检查结构；完整生成含 Go/TS smoke | 字节执行边界、并发/崩溃恢复、浏览器完整消费及外部效果；JSON Schema 字符限额不替代实际字节检查 |

本节是稳定合同导航，不核销历史 B7-01 至 B7-07 的其他待验收项；不构成 controlled live、浏览器、完整 M1、M5 Agent run、CI 或发布通过证明。实现作者命令/退出码、未提交输入摘要与主代理独立复验各保留本轮范围和身份，发布仍为 `NOT_RELEASED`。


## M1 B7 浏览器增量验收（2026-11-04）

基线为 main `a25ea659a1796f2de3f095773afc3bf1beba4ae9`；本轮仅维护测试/验收记录，不修改产品实现、Go、DDL、权限或依赖。源码、测试和服务构建身份/摘要、命令/退出码、原始日志及截图保存在本机忽略目录 `evals/results/m1-b7-browser-20261104/`。基线工作树与后续测试补丁分开记录，提交后以本轮 PR head/输入摘要核对；本文不补造历史执行身份。

| 范围 | 本轮结果与证据边界 | 尚未核销 |
|---|---|---|
| 请求作用域/迟到回执 | `tests/product/chat-request-scope.test.cjs` 主代理独立执行21项通过；切会话、A→B→A、迟到成功/401/500、刷新、退出、当前视图错误；fixture 分页信封改为真实 `data.next_cursor` | 测试内 HTTP fixture；不是 Go 授权/持久化/真实外部调用证明 |
| 双对话/草稿/通知 UI | `tools/b7-chat-notify-browser.cjs` 主代理独立执行39项通过（1440/768/375）；两处恒真断言已替换为可失败的表单/错误态检查 | 浏览器内 API fixture；不证明真实 QQ 发送、完整分页或生产效果 |
| 真实服务 UI | 新 PostgreSQL16/schema26 + 当前 Go binary + 文件 resolver；原样 smoke 13项中 runtime 入站404保留为失败，测试种子身份修正后13项通过，主代理在全新实例独立重跑通过（`independent-runtime/result.json`），输入摘要未漂移 | 不代表生产/QQ接入、所有并发与恢复 |
| 增强调查 | 主代理全新实例独立执行11项采证检查，4个产品问题均复现；真实浏览器通知CAS409/刷新恢复、reader403/零写、空会话；辅助API分页/read:false、runtime撤权、零执行事实；缺陷复现与功能通过分开 | 调查工具 exit0不代表M1通过；通知迟到与分页见下表 |
| fixture 启动器 | `python3 -m unittest tests.product.test_b7_manual_fixture_launcher -v` 补runtime种子一致性回归后主代理独立执行10项通过 | 纯函数守护，不替代服务生命周期清理/数据库验收 |

本轮保留原样基线与 mutation 证据：旧恒真断言会放过注入的错误，修正后拒绝；分页信封修正只消除测试 oracle 漂移，不意味着前端已消费 cursor。Go API 的分页/通知 CAS/read:false 辅助执行与浏览器层证据分开。

当前 `frontend/app.js` 中 conversations/messages/notifications 仍固定读取首页；未核销完整 M1 或多步 Agent run。真实 QQ、模型、MCP、ARTEX 与 production 均 `NOT_RUN`，发布 `NOT_RELEASED`。最新 CI 仅支持其实际作业范围，浏览器不在常驻 CI；本轮 CI 和审查结果按 PR 最新 head 核对。

### 本轮确认问题与后续顺序

| 优先级 / ID | 可复现触发与当前行为 | 后续独立 PR 验收门 |
|---|---|---|
| P2 / notification-navigation | 标记已读 POST 已在 Go 提交200，浏览器消费响应前切到对话；迟到回执调用 loadNotify(state.seq)，把对话页改成通知加载态，route守卫随后阻止渲染 | 为通知 mutation/read 增加 session/route/request generation 守卫；旧视图成功/403/401/超时与登出均不得污染新页；已提交写不得伪装取消 |
| P2 / message-pagination | 51条Agent消息持久化，API首页50条且next_cursor=50，API第二页含seq1；页面无后续页入口且不消费cursor | 独立前端分页PR，三种列表分别消费data.next_cursor；无漏重/迟到响应/刷新/空错态与有限加载 |
| P2 / notification-pagination | 51条同类通知持久化，API尾页可读；页面只显示首页，最旧合成项不可见 | 与消息/会话分页统一处理，但不复制Task的meta分页协议 |
| P3 / authenticated-header | 已认证且真实API已加载，对话页header仍显示“尚未连接” | 按真实请求/会话状态更新文案，不能误表示QQ在线或外部能力 |

`tests/product/b7-real-service-acceptance.cjs` 是当前基线调查/缺口复现工具，`validation_checks` 表示采证步骤正确完成，`product_findings` 和 `acceptance` 才是产品结论。修复后应将对应复现预期转换为期望行为回归，不把“要求缺陷存在”当永久验收门。通知迟到通过CDP仅延迟Go已生成的真实响应，不伪造响应/状态；数据库读回证明写已提交。

独立真实服务执行记录：schema实际读数26、初始种子3/6/2/2、binary SHA与源码/测试摘要见 `independent-runtime/initial-db.json` / `result.json`；执行前后输入摘要一致。最终 task/chat_send_permit/dispatch_permit/model_request 均0，容器消失和HTTP/DB端口关闭均读回为true。主代理独立结果与作者证据分别保存，未把旧失败改写成PASS。

本轮 Python product 全目录 `python3 -m unittest discover -s tests/product -v` 独立运行18项通过（含10项 launcher 守护/身份一致性测试），不含 Playwright；Node/Python语法和 `git diff --check` 通过。完整后端race未在本地重跑（本轮无Go/DDL修改），由本轮PR最新head的backend CI单独核对。


## M1 通知迟到回执修复（2026-11-04）

基线为合并 PR #16 后的 `origin/main` `28bf29575a615094785d7633b0da28ac76d86335`。本轮由子代理实施、主代理复核；以下执行均对应提交前冻结输入，提交后须核对内容摘要。源码 `frontend/app.js` 与 embed 摘要一致（`aa57ad6c2acb23f6d560bc61d7aceb60802c2c136b625e0caef71529a2f3ff1d`）；真实服务回归脚本摘要为 `5c14299caea14c322f10ba431cf6866965e0c0492791bac3790bf3a38b6d51a5`。PR、最新提交 CI 和合并状态以对应 PR/本机提交回执为准，本节不预先宣称合并或发布。

### 实现与边界

`frontend/app.js` 的通知读取新增 `notifySeq`，同时核对发起时的 session、route seq 与列表代次；已读回执另绑定原按钮 DOM 和 busy 状态。旧成功/错误/401/finally 不再影响新页、新会话或新按钮；同按钮进行中只发一次 POST。当前成功重读服务端，失败仍可显示错误并主动重试；超时/断连不自动重发，应刷新/重入核对。同步唯一产品源码与 embed；未改变 HTTP 合同、DDL、Go 授权、依赖或 CI 配置。

### 本轮证据

| 验证项 | 身份、环境与实际结果 | 证明范围 |
|---|---|---|
| 通知请求作用域 | 作者最终相同测试 RED：82项中43失败；修复后作者及主代理独立执行均82/82通过 | `tests/product/notification-request-scope.test.cjs` 使用真实前端与 loopback HTTP fixture；覆盖迟到 GET/POST、切 chat/返回/刷新/新登录、403/409/401/500/timeout/断连、当前行为、同页新代次、原按钮身份和防重复。提前触发超时定时器、为指定跨登录请求移除 signal 属于测试故障注入，不是 Go/持久化证明 |
| 既有产品回归 | 主代理独立执行 chat21、三屏UI39（1440/768/375）、Python product18均通过 | 保留原对话/草稿/UI/fixture guard；不是完整隔离或恢复验收 |
| 真实服务正向通知回归 | 最终同一脚本在旧前端作者 RED 失败；修复版作者及主代理全新实例均11项检查通过，`notification_navigation: PASS` | `tests/product/b7-real-service-acceptance.cjs` 在 CDP Response 阶段只延迟 Go 已提交的真实200；释放前DB及独立API确认 read=true/version4→5，切chat后内容/认证/错误保持，返回通知页仍已读/version5。750ms有界采样不是无限迟到证明 |
| 真实服务既有 smoke | 主代理独立13/13通过；schema实读26，初始会话/消息/草稿/通知3/6/2/2 | 全新PG16 + 本次embed真实Go binary + file resolver；当前 binary SHA256 `e85b3340333280a30854e989ffc0b6772e435a53bdd84c84e745d73b8853842e`，不是已部署实例 |
| 生成与清理 | 主代理核对语法、构建幂等、源/embed一致、diff检查；真实实例四类执行事实为0，专属容器消失/端口关闭均读回 | `task`、`chat_send_permit`、`dispatch_permit`、`model_request` 均0；不调用真实QQ/模型/MCP/ARTEX，不部署或迁移现存DB |

本机证据在忽略目录 `evals/results/m1-notification-scope-20261104/`：`frontend/` 保存作者最终相同测试 RED/GREEN，`independent-ui/` 保存主代理独立命令/日志/摘要；`runtime/fix-round1/` 保存最终真实服务作者 RED/GREEN 与采证故障检查，`independent-runtime-final/` 保存主代理新实例最终11项、smoke13、DB/版本及清理读回。每轮输入前后摘要一致；凭据、原始日志及截图不提交。独立源码审查与执行身份分开记录，Agent 审查不等于 GitHub 人工 approval。

保留失败：早期测试 harness 修订、真实旧版 RED、主代理首次修复版运行的截图空 `style` 属性误报均保留。最终先截图再固定对比快照，未放宽产品状态断言；只有观察到 UI 状态差异才登记该产品回归，CDP/DB/API采证故障记检查失败、通知 `NOT_VERIFIED` 并非零退出。响应等待与CDP清理有界并立即处理拒绝，采证错误不能伪装成产品通过。

### 当前结论与后续

本轮通知迟到缺陷在上述范围为 `IMPLEMENTED / WIRED_TO_CONSUMER / ISOLATED_FIXTURE / PASS`；**M1仍为PARTIAL，发布为NOT_RELEASED**。11项包含剩余缺口调查，exit0不代表完整产品通过。51条消息/通知的尾页API可读但UI仍不消费cursor（P2），已认证header仍显示“尚未连接”（P3）；会话51+分页、完整并发/持久恢复、外部推送及M5 run仍未验收。后续单独实现会话/消息/通知分页，再处理header状态；修改调查脚本时逐项把已修缺口转换成正向回归。

## M1 B7 分页与迟到响应再验收（2026-11-05）

本节补充并限定当前实现提交 `4231bab` 的独立验证范围。它只 supersede 2026-11-04 浏览器增量记录中关于 conversations/messages/notifications 固定读取首页的分页缺口，不改写历史原始证据，也不扩大为完整 Agent run 或真实 QQ 验收。

- `frontend/app.js` 现在消费三个列表的 `next_cursor`，使用服务端返回的 `cursor` 请求后续页；会话、消息和通知分别保留当前列表、加载状态、错误状态和 cursor。当前 route、session、tab、conversation、list generation 失效时，迟到的后续页不会覆盖新视图。
- 本地浏览器 API fixture：`node tools/b7-chat-notify-browser.cjs` 使用 51 条会话、消息和通知数据，在 1440、768、375 三种宽度下共 51 项检查通过；覆盖首屏、cursor=50、尾项显示、按钮消失、通知分页迟到响应，以及已有草稿/CAS/未授权发送回归。
- 真实服务隔离验收：`tests/product/b7-real-service-acceptance.cjs` 使用全新 PostgreSQL fixture、当前 Go binary 和同源浏览器执行 12 项检查，全部通过；结果为 `M1=PASS`、`pagination=PASS`、`notification_navigation=PASS`、`real_runtime_ingress=PASS`。数据库读回仍为零 Task、Chat Permit、dispatch permit 和 model request，fixture 容器、端口和凭据清理通过。
- 其他守护检查：`tests.product.test_b7_manual_fixture_launcher` 10/10 通过；`TMPDIR=<user-writable> bash tools/kernel.sh test-all` 退出 0，`internal/taskengine` race suite 通过；`bash tools/contract.sh check` 退出 0，83 个合同测试通过；frontend/embed 字节一致且 Node 语法检查通过。
- 当前剩余边界：真实服务探针仍记录 P3 `authenticated-header-label`（认证后页头仍显示“尚未连接”），这属于状态文案问题；真实 QQ、真实模型、MCP、ARTEX live endpoint、生产数据库、部署和发布仍为 `NOT_RUN` / `NOT_RELEASED`。B7-07 的 thread→run→model/tool 完整运行仍未接线。
