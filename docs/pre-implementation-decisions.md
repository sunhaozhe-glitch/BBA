# pre-implementation-decisions — 实施前裁决清单（2026-10-24）

状态：**已由负责人于 2026-10-24 正式裁决；P0-01 已完成并通过契约验收；本文件的 A/B 是历史备选与原建议，遇冲突以 `docs/adr/p0-decisions-2026-10-24.md` 为准。** 原「待裁决/P0-01 暂停」状态已失效；该文件仅保留决策过程证据，不作为当前执行口径。P0-01 已新增契约、校验工具和生成类型，并运行隔离本地 fake HTTP smoke；实际命令、退出码和限制见 `docs/README.md`。未部署/重启服务、未请求真实 NapCat/WebUI 或 QQ，封存树既有改动保留。

证据等级（沿用四份盘点报告口径）：**实测**=本轮直接观察；**静态**=配置/源码/快照静态读取；**历史**=历史验收/文档记录（不构成本轮实测）；**[INFERENCE]**=推断；**unknown**。

裁决词表：A/B 为可选方案；「推荐」是我方建议，非裁决。

---

## A. 最高优先级裁决项

### D1. 插件 ID：历史 `dxc-join-api` vs 本机注册 `napcat-plugin-cleaner`

- **当前事实**：本机唯一插件目录 `napcat-plugin-cleaner`；注册 ID = `package.json.name`（napcat.mjs 扫描器 `:65275,:65279`），`pluginRouters.set(e.id)`（`:65800-65801`）；ext 路由按 URL 中的 pluginId 查表（`:66575-66596`，`/api` 挂载 `:70540`）。故按本机静态配置，有效 base = `/api/Plugin/ext/napcat-plugin-cleaner/{search-group,join-group}`；历史硬编码 `dxc-join-api` 的路径会命中「无注册路由 404」[INFERENCE，基于上述行号]。引用全清单见 §C。
- **旧实现/旧规则**：DXC 与封存 BBA 的调用方都按 `dxc-join-api` 固定 base（DXC `join-api.ts:13,24`；BBA `client.go:52` 常量注释「fixed and cannot be overridden by config」）；插件自身注释也以 `dxc-join-api` 自称（index.mjs:1-2,142）。
- **新设计要求**：03 §2 行 29 要求插件能力以实际输入输出为准；P0-01 契约须固定真实路径；P0-02 夹具要求 supported 声明与实际响应一致。
- **冲突点**：部署身份与调用契约的 ID 不一致；BBA 侧 base 路径被常量钉死。
- **方案 A（改部署 ID）**：把插件重命名为 `dxc-join-api`（目录名 + package.json name + plugins.json 键，共 3 处）并重启 NapCat。代价：改动真实 NapCatQQ 运行目录——第二审查材料 §0 行 18 冻结实验纪律明确「不得修改真实 DXC/server 或 NapCatQQ 运行目录」，此动作需显式批准；重启 NapCat 属服务启停边界（AGENTS 行 46，历史静态）。
- **方案 B（改调用 base）**：BBA 改 `client.go:52` 常量（测试引用均为符号引用，自动跟随；仅 `client_test.go:1446` 一处 JSON 字面量）；DXC 侧可用 env `PLUGIN_API_BASE`（join-api.ts:33）或每 bot `plugin_api_base`（index.ts:108-110）覆盖，零代码改动 [INFERENCE]。
- **代价/风险/迁移影响**：A=改动面 3 文件但触碰冻结运行目录且需重启；B=改动面 BBA 2 处 + DXC 0 处，但固化了「插件名≠契约名」的命名债（注释需同步）。两案迁移影响相同：P0-02 夹具必须对真实 endpoint 实测回执后契约才算数。
- **推荐**：**B（改调用 base）**，改动面最小且不触碰冻结资产；若负责人偏好恢复文档意图（插件自称 dxc-join-api），A 亦可，但需先批准「改运行目录 + 重启」。
- **暂不裁决则阻塞**：P0-01（契约路径无法固定）→ P0-02 → P0-12/P0-13（真实搜索/申请链路）。

### D2. 加群 pending / unknown / 24h timeout 的最终语义

- **当前事实**：封存 BBA 0013 全套（未提交 WIP）：单活跃 slot（partial unique，`0013_qq_group_join.sql:84-85`）；pending 24h 未批准→`timeout`→进 24h 冷却（runtime `:335-346`，映射 `qq_group_join.go:917-918` 要求 timeout 携 cooldown）；unknown→blocked 永久排除不重发（`qq_group_join.go:915-916` + runtime `:287-306`）；冷却≥3 暂停新搜索（`qq_group_join.go:1261-1268`）。以上均为静态源码（静态；该树 dirty，测试文件存在未跑）。
- **旧实现/旧规则**：DXC 原始引擎同语义（历史源码 `group-join.ts:6,73-77` R3：pending 超时 24h→冷却；这是「生产在跑」的历史规则，spec-console §2 行 6）。
- **新设计要求**：01 §5 行 237–241、252：请求可能已发出后，超时/lease 过期/掉线**不释放** slot 与 target_claim；pending/unknown 存在时不释放额度找新群；审批超时只进阻塞/人工核查（行 253），不假定拒绝；P0 不提供 best-effort 超额模式（行 255）；unknown 不计成功（README:18）。
- **冲突点**：「24h 超时→自动释放进冷却」与「pending/unknown 不释放额度」直接冲突；历史语义等于把 timeout 当作一种可自动核销的拒绝。
- **方案 A（采用新设计）**：timeout 不再是释放条件——窗口到期只产生 BLOCKED/人工核查事件，slot/claim 保留，直到明确拒绝或未发送证明。代价：长尾申请占住唯一 slot，吞吐下降、人工核查负载升；0013/DXC 行为要改，且须按 03 §7 rewrite 定义配**新测试**保护语义。
- **方案 B（沿用历史语义）**：保留「24h 释放+冷却」，把它写成设计显式例外（等效 best-effort 例外条款，记录可能超额计数）。代价：与设计硬约束冲突需书面豁免；风险：同一群被不同任务重复申请、计数虚高（02 §5.4 行 299 归并规则受影响）。
- **推荐**：**A**（§1 原则 4/6 与 01 行 254「额度安全优先」是新设计的地基；历史语义可作为裁决后另设的显式产品开关，不入 P0）。
- **暂不裁决则阻塞**：P0-04（状态闭集/约束冻结）→ P0-07、P0-10、P0-11（`TestPendingNotRetried` 语义）、P0-13。

### D3. Character 多账号隔离：单 Runtime 补 namespace vs MVP 每账号独立 Runtime + 独立 DB

- **当前事实**（快照静态，均指向无账号命名空间）：Character 全部内存态键 `group:<guildId>`/`private:<userId>` 无 selfId（message.ts:149,163,173,178,198,225,250,258,266,280,311,317,369,397,407,445,695,840,938,1009,1013,1060；filter.ts:660）；落库主键 `sessionKey` 同串（variable_store.ts:6-41）；锁仅进程内（message.ts:3,40,492-501）；`_triggerSeq` 服务实例级共享（:56,224,694,828-852）；发送存在绕过 `session.send` 的直连路径（send.ts:24-42,90-107,148-172,227-247），仅加 session.send 钩子拦不住全部输出（01 行 508）。本机无 Koishi/ChatLuna 安装证据 → 安装版本 unknown（实测枚举级）。
- **旧实现/旧规则**：无「旧实现」——本机无既有 Koishi 部署可继承；历史 BBA 的角色管线不并入（01 §13 行 579 已判「没有 account key 的会话存储」属重写项）。
- **新设计要求**：01 §3（行 136–170）：无上游隔离补丁时每启用 Character 的账号一个 Koishi Runtime + 独立 DB；§3.2 行 155 统一会话键必须贯穿全部平面；README:17 同口径。
- **冲突点**：第二审查材料 §3 行 115「暂定单逻辑 runtime 多 adapter，隔离验证通过后才加账号」与 01/README「无补丁时每号独立 runtime+DB」；03 P0-15 行文已预设「独立 Runtime」。
- **方案 A（MVP 每账号独立 Runtime+DB）**：符合 01/README/03 行文；不深改 Character。代价：进程/数据库数量（MVP 4 号可控；扩 20 号的资源账未测——GPU 仅实测 2×RTX 3060 12G，消息量/RSS unknown）；Character AGPL-3.0 依赖链在每 runtime 重复。
- **方案 B（单逻辑 Runtime+namespace 补丁）**：符合第二审查 §3 的字面「暂定」；代价：补丁必须覆盖键/锁/触发/持久/发送全平面（01 行 155 禁「只改一处 Map key」），fork 上游维护成本，`group:群号` 存量状态无归属映射 → 冷启动+留旧快照（01 行 157）。
- **推荐**：**A 用于 P0**（Character 启用账号）；不启用 Character 的任务执行账号可按 01 行 145 先挂共享 runtime 但须过 bridge 路由/事件标识验证；B 作为 P1 降资源项，以「补丁+全平面验证」为准入。此为建议，两文档口径仍待负责人裁决。
- **暂不裁决则阻塞**：P0-03（`TestAccountIdentityIsolation` 的部署形态前提）、P0-15、P0-16、P0-18 资源阶梯。

### D4. 控制台扫码登录的最终职责边界 + WebUI 直连 vs 后端代理

- **当前事实**：产品决定=登录流程在控制台内完成（用户确认，2026-10-14 拍板；成文旁证 `封存/DXC/docs/spec-console-v1.md:3-10`，历史）。本机双份 6099 配置：QQ 安装 `webui.json` host `"::"`/port 6099/accessControlMode none（静态，token 值存在未复述）；登录运行时 state 目录 webui.json host 127.0.0.1（静态）——两配置属不同进程视图。当前 6099 无监听（实测）；napcatd 6199 与托管实例 6210 在监听（实测）。
- **旧实现/旧规则**：封存 BBA 历史实现=控制台**后端代理** WebUI（login.go 契约注释 :29-38；历史验收 CURRENT §3.12：10s 心跳、25s 快照新鲜度、在线四条件、刷新不重放、前端 5s 轮询、二维码浏览器本地渲染）；spec-console 行 29 留「WebUI 直接跨源 vs DXC 代理」二选一（历史待审条目）。
- **新设计要求**：02 §1 行 10「现有合格登录机制」+ CSRF；02 §1 行 15 禁止信任可声明 selfId；01 §14 行 697 secret 不进日志/上下文、行 698 本机之外传输 TLS/受控私网；02 §6.1 行 309 `login_state` 维度分离。
- **冲突点**：设计包主文本没有登录流程条款（覆盖缺口，design-differences §4）；直连 vs 代理未裁决；`host="::"` + accessControlMode none 的暴露面与行 698 需确认。
- **职责边界建议（不发明路由/API，只划层）**：控制台页面持登录状态机（等待扫码/已扫码确认中/已登录/需重扫/冷却）+ 用户显式动作（刷新/重扫/绑定/落库）；后端持 WebUI 凭证交换与每账号绑定点；读路径（状态/二维码）与写动作（刷新/重启）分离；扫码成功→按在线四条件核验身份（含 self_id 与凭据一致）才入账落库。
- **方案 A（后端代理）**：与历史实现、loopback 纪律、secret 边界一致；代价：后端多一层登录面。
- **方案 B（浏览器直连 WebUI）**：跨源需 CORS 放行，token/凭证进浏览器 → 与 secret_ref 边界冲突，且 `host="::"` 下暴露面变大；历史文档也标注需「二选一，审核时定」（行 29）。
- **推荐**：**A（后端代理）**；`host="::"` 是保留给「WebUI 仅供本机后端调用」还是收紧回 loopback，一并裁决。
- **暂不裁决则阻塞**：P0-01（Task API 操作员边界无登录定义）、P0-12（真实登录身份验收无入口）、P0-19（最小操作面的操作员会话前提）。

### D5. 旧 BBA / DXC / 外部脚本 writer 所有权切换（避免双 writer）

- **当前事实**：潜在 writer = ①新内核执行器（设计中）②封存 BBA 0013 加群 runtime（静态源码：随 server 根 context 启动、立即执行一次；所在容器 `bba-cdev-login-f1` 在跑〔实测〕，容器内观察到 start.sh+migrate，server 进程是否活着未单独确认 [INFERENCE]；其 DB 里 qq_group_join_configs 是否有 enabled 行未查 → unknown）③DXC 生产控制台（历史「生产在跑」；当前 6002/6099 无监听〔实测〕→ 当前未运行）④本机插件进程内能力（配置启用，进程未加载〔实测+静态〕）⑤不可穷举外部脚本（consumers_not_enumerable，已在盘点报告报告）。
- **旧实现/旧规则**：DXC/BBA 各按「单活跃申请+冷却≥3 暂停」自治；无跨系统 writer 仲裁。
- **新设计要求**：01 行 583 同一账号同一动作类任何时刻单一写入 owner，shadow 只读不得真加群/发消息；§1 原则 9；02 §9.2 Switch 阶段：先只读比对→暂停旧 writer→排空/核查在途→逐账号切 owner→恢复新 writer。
- **冲突点**：多套「生产在跑/容器在跑」的历史并行能力若同时激活即双 writer；封存 BBA runtime 的启动即触发特性（立即 tick）与 D2 语义绑定。
- **方案 A（立即冻结旧 writer 再切换）**：0013 runtime 在 D2 裁决前不启用（policy 行保持 disabled）；DXC 加群链显式停用或保持「历史在跑」原状但确认无 enabled 账号；新内核逐账号接管（P0-20 字面执行）。代价：切换期无自动加群吞吐。
- **方案 B（shadow 比对先行）**：旧 writer 继续（其当前实际未运行——6099/6002 无监听〔实测〕，等价于事实停用），新内核先在测试号跑通 P0-12/13/14，再按 02 §9.2 正式切换。代价：真实号接管前无产能。
- **推荐**：**B**（与 03 顺序「盘点→契约→无 QQ 内核→单号联调→四号→灰度」天然一致；且当前事实状态本就是「无人在跑」，双 writer 风险只在未来激活时）。切换清单（停发清单）：插件进程内能力单列为第三个潜在 writer，必须显式处置（推荐：只保留单一 adapter 调用它，禁业务插件直连）。
- **暂不裁决则阻塞**：P0-20（依赖 P0-18/19+负责人批准）；P0-13 起真实写路径的「谁有权写」。

### D6. 审计：内存 ring buffer 2000 条、重启即丢 → 持久审计

- **当前事实**：旧审计=进程内环形缓冲上限 2000 条、重启即丢（静态 `app/guard/guard.go:29,41,75`）；持久审计落 intercept 规则日志/Activity（静态，同 scout B10 结论）。
- **旧实现/旧规则**：`GET /api/audit` 读 ring（静态 `server.go:3525`）；持久面只有 Activity 表（历史验收 P6 口径）。
- **新设计要求**：§1 原则 10 不以新架构为由删除已验证的备份/恢复/审计/暂停/限额/幂等；02 §6 辅助表 task_events/审计持久化、evidence_objects 内容与审计元数据分离；01 行 701 默认保留最小结构化审计。
- **冲突点**：把旧「内存 ring 当审计真相」直接搬走=违反原则 10；按现状它本来就不满足新内核要求（migration-map 已判 rewrite）。
- **方案 A（新内核持久审计，旧端点保留兼容窗口）**：决策链/批准/暂停/对账事件全部落持久账本（02 §6.5 同事务要求）；旧 `GET /api/audit` 在 Contract 阶段前不下线（migration-map 已记）。代价：写路径新实现；兼容窗口内双读口径。
- **方案 B（给旧 guard 加落盘 flush）**：最小改动保留现状 UI，但 ring 语义（截断、重启丢）与 §1 原则 10 的「已验证审计保留」仍不达标。
- **推荐**：**A**；ring 只作展示缓存，真相在 DB。
- **暂不裁决则阻塞**：P0-19（验收含「暂停/取消/对账有准确收据」——收据必须可持久回读）；P0-10 事件 reducer 的落点确认。

---

## B. 其他影响 P0 架构/数据语义的裁决项

### D7. MVP 候选来源：固定清单 vs 关键词搜索

当前事实：搜索接口真实存在（插件已装启用，静态；见 §C 引用）；无 cursor、无待审批查询（静态）。旧规则：DXC/BBA 的 2 分钟 tick 以关键词搜索为输入（历史/静态源码）。新要求：第二审查材料 §1 行 63 第一阶段固定清单/导入数据，搜索在「接口存在+数据来源合法+授权」确认后才接入；01 §6/§12（行 260–285、589–610）默认关键词生成候选。冲突点：MVP 的 `group.acquire` 第一输入形态。A=固定清单（第二审查默认；无 cursor 缺陷不影响，导入即可）；B=搜索+清单双轨（01 默认；缺「授权/状态查询」两件前置，隐私/ToS 仍 unknown）。推荐：MVP 按 A 交付，B 作为 P1 接入（插件能力现状已探明，届时只差授权确认）。阻塞：P0-06 模板展开形态、P0-02 夹具范围、P0-13 候选来源验收。

### D8. ARTEX 复用口径：只借鉴接口 vs 复制测试过的基础设施

当前事实：封存树=AGPL-3.0（`app/LICENSE:10,63,636,660`，静态）；上游 meta SPDX AGPL-3.0（静态）；整棵 Go 树继承许可（reflog+导入路径，历史+[INFERENCE]）。旧规则：无（这是设计输入）。新要求：第二审查 §0 行 27 默认只借鉴领域无关接口、不作运行时依赖；01 §4 行 178–188 却列「优先复用测试过的 PostgreSQL 封装/鉴权/审计/SSE/迁移账本」。冲突点：从本仓（同 AGPL 链）复制测试过的模块是否豁免「不复制」默认。A=纯借鉴（新实现，边界最干净，重做成本低但快）；B=复制 migrate 账本/鉴权中间件等纯 infra（继承 AGPL 含网络条款，owner 单人运营场景下义务可控——非法律结论）。推荐：数据层（migrate 账本、事务封装）允许 B，其余按 A；须负责人书面确认许可路径。阻塞：技术栈 ADR（03 §2 行 39）→ P0-04/05 的实现基底选择。

### D9. 安装基线缺失：Koishi/ChatLuna/Koishi-DB 与本地 9B

当前事实：本机无 Koishi/ChatLuna 安装证据（实测枚举级）；无 9B 权重证据（unknown）；Swift IQ3_S 权重+tokenizer 有证据、服务当前在监听 0.0.0.0:8166（实测）；GPU 2×RTX 3060 12G（实测）；group-analysis 许可 NOASSERTION（快照）。旧规则：无。新要求：01 行 60–61 本地 9B/Swift 信息不足即默认禁用；第二审查 §0 行 12 不许假定目录结构。冲突点：P0-15/16 需要「安装哪一份、什么版本、哪个 DB」才能立兼容性矩阵。A=MVP 明确新增安装（版本钉快照 commit 或更新，Character 许可 AGPL 已确证）+ 每 runtime 独立 DB（若 D3=A）；B=等 9B/Swift registry 补全再定运行时布局。推荐：A（安装动作属实施阶段审批范围，本报告不动作）。阻塞：P0-15/16 无法开始；P0-18 资源阶梯无输入。

---

## C. 插件 ID 阻塞项——纯只读复核（本轮无修改、无请求）

**`dxc-join-api` 全部引用位置**（本轮 grep 实测）：

1. `封存/DXC/server/src/join-api.ts:13,24` — 默认 base 常量；`:33` env `PLUGIN_API_BASE` 可覆盖。
2. `封存/BBA/app/napcat/client.go:14,52` — 常量 `PluginAPIPath`，注释明示端点名固定、config 不可覆盖 base path；`:80-84` base URL 路径必须等于该常量。
3. `封存/BBA/app/napcat/client_test.go:109-110,394,471,847` 等 — 均以符号引用常量（改常量自动跟随）；`:1446` 一处 JSON 字面量含旧 base。
4. `QQ群项目/.../plugins/napcat-plugin-cleaner/index.mjs:1-2,61,63,68-70,88,96,109,112,124,131,137,142,146` — 全部为注释/日志前缀，非路由配置；实际路由是相对注册 `ctx.router.post('/search-group'|'/join-group')`（`:90,:117`）。
5. `封存/DXC/web` 不存在（grep 目标缺失），历史 DXC 前端无引用。

**`napcat-plugin-cleaner` 的实际注册链与对外 base path**（静态源码+配置）：插件目录名/`package.json:2` name → 扫描器 `scanDirectoryPlugin` 以 `i?.name || e` 作 ID（napcat.mjs:65275,65278-65288）→ `pluginRouters.set(e.id, …)`（:65800-65801）→ WebUI `/api/Plugin/ext/:pluginId` 按注册 ID 查 router（:66575-66596）→ `/api` 挂载于主 app（:70540）。`config/plugins.json:1` 以 `napcat-plugin-cleaner` 为启用键。故本机有效对外 base path = **`/api/Plugin/ext/napcat-plugin-cleaner/{search-group,join-group}`**（含 WebUI 鉴权中间件）。

**改动面判断**：方案 B（调用方改 base）≈ BBA 1 常量+1 测试字面量，DXC 侧 env 即可，零代码；方案 A（部署 ID 改回 `dxc-join-api`）= 运行目录内 3 处 + 重启，且触碰「不得修改真实 NapCatQQ 运行目录」冻结纪律（第二审查 §0 行 18）。**结论：B 改动面更小。** 两案都需在实施阶段做一次真实 HTTP 实测收口（本轮未发请求；当前 6099 无监听〔实测〕）。

---

## D. 裁决 → P0 阻塞映射（裁决前 P0-01 起全线暂停）

| 裁决项 | 直接阻塞的 P0 步骤 |
|---|---|
| D1 插件 ID | P0-01 → P0-02 → P0-12/13 |
| D2 pending/unknown/24h | P0-04 → P0-07/10/11 → P0-13 |
| D3 Character 隔离 | P0-03、P0-15/16、P0-18 资源阶梯 |
| D4 登录边界+直连/代理 | P0-01（操作员边界）、P0-12、P0-19 |
| D5 writer 所有权 | P0-20；P0-13 起的「谁有权写」 |
| D6 审计持久化 | P0-19、P0-10 落点确认 |
| D7 候选来源 | P0-06、P0-02 夹具范围 |
| D8 ARTEX 复用/许可 | 03 §2 技术栈 ADR → P0-04/05 基底 |
| D9 安装基线 | P0-15/16/18 |

**历史边界声明（仅指本清单最初编写阶段）**：原报告编写阶段仅新增本报告文件；未修改业务代码/配置/冻结目录，未部署或重启任何服务，未发任何 HTTP 请求，未登录 QQ、未申请/加入群。封存树与指定空目录的既有状态全部保留。后续 P0-01 契约实施与本地 HTTP smoke 以 `docs/README.md` 的验收记录为准。
