# capability-audit — 关键词加群插件 / Koishi / ChatLuna / 本机模型（只读盘点）

日期：2026-10-23。证据等级：**实测**=本轮观察（`ss`/`nvidia-smi`/目录枚举）；**静态配置**=读取的磁盘配置；**源码**=静态读取代码；**快照**=设计包 `evidence/snapshots/` 上游镜像；**历史记录**=历史验收/文档，非本轮实测；**[INFERENCE]**=推断；**unknown**。未调用任何 HTTP 端点；未读取/复述 token、密码等秘密值。

## 1. 关键词加群插件（03 §2「关键词加群插件」行）

本机安装：`/run/media/sun/6F53FE6DFAA8CECF/QQ群项目/QQ/Napcat/opt/QQ/resources/app/app_launcher/napcat/plugins/napcat-plugin-cleaner/`（目录内唯一插件）。

- `package.json:2` 名称/ID `napcat-plugin-cleaner`；`:3,:7-8` 描述为 “DXC 搜索/加群 API”，作者 DXC。启用：`config/plugins.json` 仅 `{"napcat-plugin-cleaner": true}`（静态配置）。
- `index.mjs` 的 `plugin_init` 注册 `POST /search-group`、`POST /join-group`；join 要求显式 `confirm=1`；搜索 count 限 1–50；插件侧超时 search 10s、join 15s；无 cursor、无待审批状态查询、无业务限流（DXC 侧已有）。
- **ID 不一致（阻塞项）**：插件自身注释与调用方都使用 `dxc-join-api` 作为路由 ID，但 NapCat 按注册 ID 查路由：扫描器以 `package.json.name` 为 ID（`napcat.mjs:65275,65279`），`pluginRouters` 按该 ID 注册（`napcat.mjs:65800-65801`），`/api/Plugin/ext/:pluginId` 按 param 查 `getPluginRouter`（`napcat.mjs:66575-66596`，经 `Ln.use("/api", _r)` 挂载于 `napcat.mjs:70540`）。按本机静态配置，历史调用方硬编码的 `/api/Plugin/ext/dxc-join-api/...` 将命中无路由 404 [INFERENCE，基于上述行号]；本轮未发 HTTP，实测归实施阶段。
- WebUI 鉴权路径存在且插件端点在其下（源码）：`/api/auth` 与 `/api/Plugin` 同挂 `_r`（`napcat.mjs:66854,66863,70540`）。历史 adapter 契约：token→`/api/auth/login` 换 1h 凭证（本地 55min 缓存）、search 读操作可在重登后重试一次、join 一旦派发 unknown 不自动重发（`封存/BBA/app/napcat/client.go:280-440`，历史源码）。
- 插件进程内能力 ≠ 已运行：`6099` 静态配置启用，但 `ss -ltnp`（实测，2026-10-23）无监听；插件路由存在只证明“已配置启用”，不证明本轮加载。

### 能力判定（对照 02 契约动作表，行 225–228）

| 03 §2 检查项 | 实际 | 判定 |
|---|---|---|
| 搜索与申请分离 | `search-group`/`join-group` 两端点 | 满足 |
| cursor 分页 | 无；count 1–50 单次 | **不满足**（无分页） |
| 群号 | join 按 group_id（插件转发 groupCode） | 满足 |
| 逐次回执 | 每请求即时 JSON 回执（含 result 码） | 满足 |
| 待审批/取消查询 | 插件无此端点；DXC 用 action 路径+轮询通道（`封存/DXC/server/src/join-api.ts:115-165`，历史源码） | **插件不提供** |
| 超时语义 | 10s/15s 插件侧超时；调用方须按 unknown 处理 | 满足（语义归调用方） |

按 02 契约 `observe_join_request` “能力存在且可验证才启用”：本插件对 observe 类 = 未确认，fail closed。

## 2. 主动加群两分钟 tick（03 §2 调用链核验）

- 原始 DXC：`封存/DXC/server/src/group-join.ts:109-113` 立即执行一次 + `setInterval(2*60*1000)`；`runGroupJoinOnce`（:59-107）= 搜索→过滤→最多一个 pending→申请→记录。
- 封存 BBA 迁移：`app/server/qq_group_join_runtime.go:42-72`（`qqGroupJoinPassInterval = 2 * time.Minute`，pass deadline 110s），`:196-209` 立即一次 + ticker；`server.go:310-311` 根 context 启动。
- 封存 BBA 执行链：账号 lease（`app/db/qq_group_join.go:1750-1778`，TTL 5min）、单活跃 slot（`:1248-1259` + 0013 partial unique 索引）、逐关键词搜索、过滤评分、持久 claim 后申请；**pending 24h 未批准 → 释放进 24h 冷却**（runtime `:335-346`）；未知派发按未知永久排除、不重发（runtime `:287-306`）；开场消息每确认最多一条、持久 claim 后发送（runtime `:506-591`）。
- 语义差异（须负责人裁决）：设计 01 行 237–241、252 要求 pending/unknown 不释放 slot；封存实现把“24h 超时未批准”作为释放路径（与 DXC 原实现一致，属迁移既定行为）。这是历史语义与新契约的冲突点，列入差异报告 §4。
- 该目录存在未提交改动（见 source-audit），以上为静态源码证据；不代表目标目录现状，更不代表正在运行。

## 3. Koishi / ChatLuna 兼容性矩阵（03 §2 对应行）

| 组件 | 许可（快照/meta） | 账号隔离 | 安装版本 | 发送路径 |
|---|---|---|---|---|
| Koishi | MIT（`evidence/koishi-meta.json`） | 官方支持多 bot；同频道默认单受理人（ledger id 21） | 本机无安装证据 → **unknown** | 经 adapter |
| ChatLuna | AGPL-3.0（`chatluna-meta.json`，v1-dev 819b7f3） | README 未列 Http Server 已完成 | 无安装证据 → unknown | — |
| chatluna-character | **AGPL-3.0**（快照 `package.json:22` + `chatluna-character-meta.json`，commit 6ee173b） | **无账号命名空间**：全部内存态键 `group:<guildId>`/`private:<userId>`（message.ts:149,163,173,178,198,225,250,258,266,280,311,317,369,397,407,445,695,840,938,1009,1013,1060；filter.ts:660），落库主键 `sessionKey` 同串（variable_store.ts:6-41）→ 同群多账号共享状态/锁/cooldown/触发队列 | unknown（快照≠安装） | 三条 `_request` 直连绕过 `session.send`：file(:90-107)、video(:148-172)、含 img 段(:227-247)；QQ 私聊 markdown 直连(:24-42)；纯文本仍走 session.send(:335) |
| chatluna-group-analysis | NOASSERTION/Other（package.json 未快照 → 许可 unknown） | 落库 PK `platform_messageId` 无 selfId（message.ts:92-96）；读路径按账号路由但查询不带 selfId 过滤（:331-339 vs :560-600） | unknown | — |

- 触发存储 `ctx.chatluna_character_trigger` 源码不在快照集 → 其持久性 **unknown**（filter.ts:5,439,554；ledger id 9）。
- 锁仅进程内 ObjectLock（message.ts:3,40,492-501）→ 跨进程/多 runtime 无协调。
- 结论（对应设计 §3.2 行 155–157）：Character 状态无 selfId 命名空间是**快照级静态确证**，支持“无隔离补丁时每账号独立 runtime+DB”的保守路径；合并 runtime 的准入条件因此更贵。两文档口径差异（每号 runtime vs 暂定单逻辑 runtime）列入差异报告 §1。

## 4. 本机模型 registry 草案（未知保留 unknown）

| 项 | 观察 | 等级 |
|---|---|---|
| GPU | 2× RTX 3060，各 12288 MiB，驱动 580.178.04 | 实测（nvidia-smi） |
| Swift IQ3_S | 权重/量化：`Strata-data/models/swift-IQ3_S/…IQ3_S-0000{1,2}-of-00002.gguf` 两分片+`.done`；tokenizer `Strata-data/packs/swift-iq3_s/tokenizer`；启动 `Strata/run-swift-iq3_s.sh`（port 8166）| 实测（目录/列举级） |
| Swift IQ3_S 服务端 | `0.0.0.0:8166` 由 python 监听 → 当前在运行 | 实测（ss） |
| Swift 上下文/输出能力 | 未见配置证据 | unknown |
| 本地 9B | 全盘 gguf 仅 Swift 系（IQ3_S/IQ3_XXS/vocab/mmproj/mtp），无 safetensors → 权重无证据 | unknown |
| IQ3_XXS 服务 | 对应 unit `strata-swift.service`（8080）；当前无监听 | 实测（ss）+ 历史 |
| OMP 别名读回 | `default/plan=gpt-6.1-sol:high`、`slow=gpt-6-astra:high`、`smol=swift-1.5-iq3_s`、`tiny/memory=swift-1.5-iq3_xxs` | 实测（配置读回） |

按 01 行 61：Swift 路由在补齐实际模型信息（服务端、上下文、输出能力）前默认禁用——权重与服务进程有证据，但上下文/吞吐仍 unknown；不得据别名配置承诺容量。

## 5. 旧外部自动化 writer 盘点（03 §2「旧外部自动化」行）

- DXC 生产控制台路径（历史记录 `封存/DXC/docs/spec-console-v1.md` §2 行 6：加群配置 36374700、日志 8 行，“生产在跑”）：本轮 `ss` 实测 6002 无监听、QQ 安装 WebUI 6099 无监听 → 该 DXC server 当前未运行（其 OneBot WS client 配置在 `config/onebot11_36374700.json` `:9-11` 指向 `ws://127.0.0.1:6002`，静态配置）。
- 封存 BBA 全套（server、加群 runtime、WebUI/napcatd 登录链）：运行体观察=容器 `bba-cdev-login-f1` 在跑（source-audit §3）；其是否指向 6099 实例 unknown。
- 插件进程内能力（本机安装插件）：潜在第三 writer，按 01 行 583「同一账号同一动作类只有一个写入 owner」，切换清单必须显式处置它。
- 其他 cron/常驻 worker：本机未发现加群相关 cron（枚举级）；无法穷举的脚本标 consumers_not_enumerable。

## 6. 未确认 / 阻塞

- 插件 ext 路径 ID 修复后需实测 HTTP 回执（本轮未调）。
- Character/ChatLuna/Koishi 安装版本、DB 引擎、触发存储持久性、group-analysis 许可：unknown。
- 本地 9B、Swift 上下文/吞吐：unknown（registry 保持草案）。
