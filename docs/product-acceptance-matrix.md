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
| QQ-02 | 对话双视图：Agent 线程 + 按 QQ 账号/群/私聊隔离的收件箱；QQ 出站先做草稿、规则/许可回执 | 当前 `ChatRuntimeHandler`、`ChatPermitRuntimeHandler` 是 runtime 发送边界；产品页仍显示 Agent/QQ 双标签待接入 | [`docs/sol-bba-qq-b7-dual-dialogue-notify-20261101.md`](sol-bba-qq-b7-dual-dialogue-notify-20261101.md) 的事件→持久化→API→前端→草稿链；禁止真实 writer | 首轮实施目标 |
| QQ-03 | 任务覆盖加群、分析群、聊天、客户加我/我加客户 | 当前 Task Engine 有任务生命周期和历史 `group.acquire` 契约；尚无 QQ 业务全类型的独立消费者证据 | 每种类型有显式 schema、能力检查、审批/permit 绑定、unknown 与不重发测试 | 工程切片已交付（离线 fixture，作者自测）；决策矩阵见 [`docs/qq03-task-type-matrix.md`](qq03-task-type-matrix.md)，待独立验收 |
| QQ-04 | 计划任务复用任务引擎，支持时区、下次执行、重启补偿、重叠幂等 | 产品入口存在，后端/前端完整消费者未形成 | 计划持久化、调度回执、任务生成和重启/重叠测试 | 未接线 |
| QQ-05 | 资产表示明确意向客户；QQ 标识、来源、证据、规则版本、时间和归属可审计 | 产品资产入口为待接入；当前没有可引用的客户证据链 | 意向去重、证据撤销、人工修正和 dashboard 计数回读；明确意向即计数，好友关系单独记录 | 未接线 |
| QQ-06 | LLM 保留现有模型注册、密钥保护、代理、搜索和并发设置 | B4/B5/B6 已有 fixture/作者证据；设置 handler 对 nil registry 有显式 unavailable 返回路径，主装配覆盖不足 | 配置/未配置两种主服务装配的同进程 HTTP 回归；不泄露 secret，不改变现有作用域 | 部分实现，未充分独立验证 |
| QQ-07 | Agent 保留配置能力，未来执行域绑定 QQ 工具；模型不能获得 QQ 授权 | Agent/Skill 配置 handler 已装配；没有 QQ 工具执行消费者 | 仅通过显式 Skill/工具/permit 链；模型输出不能直接 materialize/send | 配置已存在，QQ 接线未实现 |
| QQ-08 | MCP 接入 ChatLuna、Character、group-analysis，区分仓库、插件和 MCP endpoint | 产品入口仍待接入；仓库 URL 本身不是 endpoint 或已安装能力 | 连接/版本/能力/健康状态可读，未配置时 fail closed；fixture 不冒充在线安装 | 未接线 |
| QQ-09 | Skill 设计为版本化 QQ 运营流程：群分析、意向识别、关键词策略、聊天辅助、客户跟进 | 当前 Skill 是配置/版本管理，未绑定 QQ 业务对象 | Skill 只能产生受 schema/权限约束的建议；每次运行绑定规则/工具/版本，不授予权限 | 设计待实施 |
| QQ-10 | 工具目录来自 NapCat 原生 API，目录、schema、运行 capability、调用授权分离 | `product/tools.json` 只是嵌入只读目录；尚无已验证 NapCat adapter/调用链 | fixture 能力声明、参数校验、permit/bridge/observation 回执；真实调用另行授权 | 目录存在，运行时未接线 |
| QQ-11 | 通知推送沿用现有形态，但接 QQ 入站、草稿、拦截、任务和结果事件 | 产品入口为待接入；现有 outbox/inbox 不是 operator 通知 API | 本轮至少可读事件投影、workspace/账号隔离、去重和已读状态；不发送外部通知 | 首轮实施目标 |
| QQ-12 | 拦截规则从命令拦截改为关键词/模式拦截，作用于最终出站文本 | `content_rules`/`content_review` 和前端配置/试跑已有；需确认所有 writer 都经过最终检查 | 单段文本规则版本、命中证据、阻断不发 permit；任何直连 writer 回归失败 | fixture 部分通过，需扩大消费者覆盖 |
| QQ-13 | 审批记录覆盖任务、草稿、规则、工具和外部效果 | 现有 proposal/content review 路径分开；产品页有内容审批 | 每条审批绑定 payload digest、scope/version、决策和最终回执；批准不等于发送成功 | 部分实现 |
| QQ-14 | 日志沿用，但必须能关联 QQ 账号、会话、任务、工具和通知，不回显凭据/原文 | 当前有运行日志/审计表，但 QQ operator 查询面未形成 | 只读分页、脱敏、request/event correlation 和失败状态可回读 | 未接线 |
| QQ-15 | 系统配置至少保留全局代理、网络搜索、Work Agent 数；其余按 QQ 消费者保留 | Work Settings、proxy、search 已有不同程度路径；registry 未配置时设置路由可能进入 nil receiver | 配置和未配置路径均有同进程测试；设置真实影响模型/搜索消费者 | 部分实现，需修复/独审 |

### 已确认的外部范围

本轮仅允许代码、migration、fixture 和离线验收。任何测试都不得登录 QQ、发送消息、加群、加好友、启动真实 NapCat/ARTEX/6300、写生产数据库、安装在线 MCP 或调用付费模型。fixture、静态注册表、作者报告和菜单存在都只能支持工程层结论，不能支持真实 QQ 或发布结论。
