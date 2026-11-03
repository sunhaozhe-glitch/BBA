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

## B7 阶段 1 principal 请求作用域（2026-11-03，PARTIAL / BLOCKED / NO_SHIP）

本节仅覆盖组合依赖快照 `8f916059ed3261770b31f7ed2bdf196b3e1ae75e` 后的本阶段增量；main 仍为 `5e44e86b175de2563b25ef71b4aace64e8f8a9ff`，不能将下列结果归于 main。设计/完整命令、退出码、日志、风险见 `docs/b7-stage1/REPORT.md`；本地原始证据根 `.work/b7-stage1-20261103-162442/`，不提交 binary/凭据/历史证据包。

| 能力 | 实现状态 | 接线状态 | 验证身份 | 环境 | 验收 | 版本、构建、schema、证据 | 发布状态/风险 |
|---|---|---|---|---|---|---|---|
| Agent/Draft 跨 principal 同 key 与通知去重 | IMPLEMENTED | WIRED_TO_CONSUMER | AUTHOR_SELF_TEST | ISOLATED_FIXTURE | PASS（限定代码版本） | RED `3155faf`、`RED-permanent.json` exit1；GREEN `3155faf`+diff 摘要 `GREEN-target.json` exit0；生产改动后 `e17950f` `test-all-1.json` exit0；schema26 | NOT_RELEASED；不是第三方审查 |
| 同 principal 精确 replay/body conflict、并发、零许可 | IMPLEMENTED | WIRED_TO_CONSUMER | AUTHOR_SELF_TEST | ISOLATED_FIXTURE | PASS（限定代码版本） | `TestPrincipalEventScopeLifecycle`，原断言未降低；消息/通知/Draft/ledger/permit 计数，`GREEN-target.log`；最终 HEAD 以 `FINAL-*.json` 绑定 | NOT_RELEASED；未穷尽全部 scope HTTP 组合 |
| 历史 event_id/旧 binary 请求、丢失响应、服务重启后 replay | IMPLEMENTED | WIRED_TO_CONSUMER | AUTHOR_SELF_TEST | ISOLATED_FIXTURE | PASS（限定代码版本） | `e17950f` `HTTP-2.json` exit0；`http-2/result.json` 为 binary SHA256/schema26/真实 HTTP 响应；计数12消息/12通知/6Draft/零许可；无注入 | NOT_RELEASED；旧 schema25 与依赖26不保证混跑 |
| 合同与已有回归 | IMPLEMENTED | WIRED_TO_CONSUMER | AUTHOR_SELF_TEST | CONTRACT | PASS（限定代码版本） | `e17950f` `contract-2.json` exit0；test-all exit0；operator endpoints 尚未列于 OpenAPI，合同绿色不补足该缺口 | NOT_RELEASED；最终文档 HEAD 另跑 |
| 第三方独立复审/独立执行 | IMPLEMENTED | WIRED_TO_CONSUMER | NOT_REVIEWED | ISOLATED_FIXTURE | BLOCKED | MOP施工0；未建立不付费无云回退路由；Hermes为作者和执行者 | NOT_RELEASED / NO_SHIP |
| main/live/真实 QQ/ARTEX/本阶段浏览器 | PARTIAL | PARTIAL | NOT_REVIEWED | PRODUCTION | NOT_RUN | 没有该范围证据；本阶段无前端改动 | NOT_RELEASED |

## B7 2026-11-03 隔离增量验收（PARTIAL / NO_SHIP）

本轮 main 为 `5e44e86b175de2563b25ef71b4aace64e8f8a9ff`，schema 源码 head=25、migration tail=`0016_qq_conversations.sql`。组合候选包含 #9/#10（共同包含 #8），schema/隔离 DB head=26、tail=`0017_operator_notification_scope.sql`。轮换独立修复提交 `5df521968726ede616a612c486a4e253cdb68aab` 基于 #10；组合修复 `cfc08c86a0cb07f2d94b8d708ff24ddb19eb811f`。所有结论仅离线 fixture，未发布。

验证者/实现者均为 Hermes（MOP 非付费路由未建立，未派发；不是独立于实现作者的第三方审查）。原始证据根：`.work/b7-audit-20261103-150312/`，完整说明见该目录 `REPORT.md`。Go 容器 go1.26.10，Node 容器 v22.23.4。修复 binary 的精确 SHA256 见 `evidence/binary-provenance.json`，浏览器运行时源码随后仅 gofmt，不能把旧构建宣称为最终提交逐字重建。

| 项目 | 实现/接线 | 本轮结果与证据 | 未覆盖风险 |
|---|---|---|---|
| 轮换撤销 | resolver OwnerEpoch 传入 Actor，事务锁内核对签发 epoch | PASS（限定）：`http-fixture-8e7036/independent-http-green.json`；旧 token 重放/新事件/并发拒绝，副作用不增，重启保持；`http-fixture-4665e8/rotation-lock-evidence.json` 锁屏障与 active 撤销 | 未做第三方独立审查及移除修复的变异对照 |
| Agent/Draft 精确重放 | 复用持久 idempotency，principal/conversation 锁；基础前端接线 | PASS（限定）：17 项 HTTP、`evidence/replay-final-exit.json`=0，Draft 正常/blocked/unsupported/读取、并发重放、异 body 409、零 permit | 全部 workspace/principal/account 组合及响应丢失网络故障未穷尽；跨 principal 相同 key 的消息 event_id 唯一性仍需专项复审 |
| B7-02 Inbox | 既有 handler + runtime epoch fence | PARTIAL：旧身份拒绝、事件重放、真实 HTTP、61 条并发 Agent 消息分页读取无漏重；`http-fixture-4665e8/pagination-real-http.json` | Inbox 多 workspace/account 并发及分页期间新增事件稳定性未完整验证；offset cursor 不能视为并发快照 |
| B7-05 主进程 | 实际 binary+resolver+隔离 PostgreSQL | PASS（限定）：runtime/operator 互斥、reader 403、missing 404、DB stop 后 5xx/恢复、服务重启；17 项 HTTP 证据 | 多 workspace/account 的完整真实 binary 授权矩阵尚缺 |
| B7-06 消费者 | 分页全部消费；消息和提交迟到响应 fence；源/嵌入同步 | PARTIAL：`browser-green-r3/assertions.json` 13 项全过（真实 HTTP）；`browser-boundaries/assertions.json` 8 项全过（含注入）；三屏截图存在 | 视觉工具接口报错，BLOCKED，未实际审读截图；注入分页不等于真实大列表 UI；通知 CAS 迟到回执仍需复审 |
| main 既有 B7 回归 | 独立 main checkout | PASS（既有测试范围）：`evidence/main-final-r2-exit.json`=0 | 不覆盖候选修复，不可表述 main 已修复 |
| 组合完整 Go race | `bash tools/kernel.sh test-all` | PASS：`evidence/combined-full-exit.json`=0；taskengine 102.631s | 不替代浏览器、真实 QQ、第三方评审或当前 head CI |
| migration 26 兼容 | 通知唯一索引改变，binary SQL 必须匹配 schema | FAIL（混跑）：`evidence/migration-compatibility.json` 两方向 EXPLAIN 都退出3 | 必须协调停写/升级，不能旧新 binary/schema 任意滚动混跑 |

PR #8/#9/#10 本轮读取均 OPEN、各 head contracts/backend/character 成功、reviews=0，详见 `evidence/pr-latest.json`。本轮新增提交尚未推送，无 hosted CI，不合并。历史失败日志全部保留；setup 失败不计为产品 RED。

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

### CI 与仓库治理核对（2026-11-02 快照）

- 当前 `.github/workflows/ci.yml`：contracts（合同/TS/前端语法/插件）、backend（隔离 Go race/build）、character（build/typecheck/test）。未独立覆盖 Collector、完整产品浏览器、frontend/embed 一致性。本轮仅补文档；这三项增强作为独立 CI PR，路径条件设计需避免必需检查被跳过后永久 pending，不混入业务 PR。
- 通过 GitHub API 查询 `repos/sunhaozhe-glitch/BBA/branches/main` 返回 `protected=false`，`repos/sunhaozhe-glitch/BBA/rulesets` 返回空数组。当前未启用 main 分支保护/ruleset，并非“已核实启用”或“尚未核实”。没有仓库强制的 approval/required check 门禁不等于可绕过 AGENTS 的 PR/最新 CI 规则。本轮不修改仓库设置；未来启用审批/禁止绕过前需确定实际审查者及治理策略，避免单人仓库无法合并。
