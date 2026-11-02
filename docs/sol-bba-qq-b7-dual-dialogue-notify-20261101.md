# BBA QQ 首轮实施任务书：双对话、通知与设置路由回归

- 任务 ID：`BBA-QQ-B7-20261101`
- 执行者：本地 `sol` profile
- 项目主体：`/run/media/sun/6F53FE6DFAA8CECF/QQ群项目/BBA`
- 架构基线：`/run/media/sun/6F53FE6DFAA8CECF/BBA/qq-automation-design`
- 参考界面：ARTEX dashboard 的信息架构和视觉形态；不得复制第二套 BBA backend/UI
- 任务状态：已授权开始工程实现；真实 QQ 外部动作仍未授权

## 目标

交付一个能在隔离 PostgreSQL fixture 中被独立验收的第一条 QQ 产品链：操作员可以在同一产品页查看 Agent 对话和 QQ 收件箱两个隔离视图；QQ 入站事件有持久化、workspace/账号/群私聊边界、分页和去重；操作员发给 QQ 的内容只能生成待审核草稿和规则/发送许可回执，不调用真实 writer；通知页能读取与这些事件相关的脱敏通知投影。与此同时，为未配置 `-model-registry-file` 时全局代理/网络搜索设置路由补齐主服务装配回归，验证当前 nil receiver 的明确 unavailable 响应，不预设存在 panic，也不通过创建假 registry 掩盖未配置状态。

这轮的产品目标超出“增加两个菜单页”：完成事件/消息 → 持久化 → operator API → 产品消费者 → 草稿/规则/许可回执的可回读闭环。实现只能使用现有 Task Engine、inbox/outbox、content rule/review、chat permit、resolver、migration 和产品装配边界；如果已有实体不足，追加最小 migration，不新建第二个任务引擎或独立模型通道。

## 已确认的业务决定

- 对话是双视图：Agent 线程，以及按 QQ 账号、群聊/私聊隔离的 QQ 收件箱。
- Agent 对话消息不得自动进入 QQ 出站；QQ 入站内容不得自动成为 Agent 系统指令。
- 明确意向即计入“引流成功客户数”，但资产 CRM 不在本轮实现；不能以演示数字补齐 dashboard。
- 本轮只做代码、migration、fixture 和离线验收。

## 明确禁止

不得登录 QQ、发送 QQ 消息、加群、加好友、调用真实 NapCat/OneBot、启动或修改真实 NapCat/ARTEX/6300 运行目录、写生产数据库、安装在线 MCP、访问付费模型、读取或输出任何凭据。不得为了测试修改已有永久 fixture、历史报告、`dist` 或嵌入副本来绕过失败。不得把 fixture transport、静态 `product/tools.json`、HTTP 202、审批记录或作者报告写成真实 QQ 成功。

若某命令会启动服务或写入外部目录，先确认它只使用临时/隔离路径，并把实际命令、路径和退出码记录到证据目录。

## 开始前必须读取

1. 根目录 [`AGENTS.md`](../AGENTS.md)，特别是持久性、permit、writer、migration、fixture 和敏感值规则。
2. [`docs/product-acceptance-matrix.md`](product-acceptance-matrix.md) 的 QQ 产品目标增量。
3. [`docs/adr/bba-final-closeout-2026-10-31.md`](adr/bba-final-closeout-2026-10-31.md)。
4. [`backend/cmd/taskengine-server/main.go`](../backend/cmd/taskengine-server/main.go)、`product.go`、`resolver.go`、`product_runtime.go`。
5. 现有 `core.inbox`、`core.outbox`、`core.chat_*`、`core.content_*`、`core.approval` 迁移和对应 Go handler/test。
6. 原始架构 [`01-architecture.md`](../../BBA/qq-automation-design/01-architecture.md) 与 [`02-contracts.md`](../../BBA/qq-automation-design/02-contracts.md)。

先用源码证据判断哪些表/handler可复用；任务书中的建议命名不是强制接口。如果现有实现已经覆盖一项，补消费者和测试，不重复建模。

## 实施范围

### A. 双对话 operator API 与持久化

实现或映射最小只读/草稿接口，建议路径如下；若仓库已有等价路径，保留其兼容语义并在报告中说明：

- `GET /v1/operator/conversations`：按 workspace、账号、`conversation_kind`、`conversation_id` 分页；返回稳定 cursor、最后事件时间、未读计数；不能跨 workspace/账号读取。
- `GET /v1/operator/conversations/{id}/messages`：只读分页，默认脱敏；消息明确标记 `source=agent|qq`、`direction=inbound|outbound_draft`、`delivery_state`，不把 draft 伪装为 sent。
- `POST /v1/operator/agent-threads/{id}/messages`：只进入 Agent 线程；不创建 QQ permit，不调用 QQ writer；保留 request/event correlation。
- `POST /v1/operator/qq-drafts`：创建 QQ 出站草稿，绑定 workspace、账号、会话、payload digest、规则版本/内容审核结果和 expected version；默认为 `draft` 或 `blocked`，不得返回 sent/succeeded。
- `GET /v1/operator/qq-drafts/{id}`：回读草稿、审核/拦截结果和发送许可状态；若没有单次 permit，明确 `send_permit=absent`。
- 若要接收入站 fixture，使用现有 machine/runtime 事件入口或新增最小受鉴权的 fixture-only adapter；同一 `event_id` 同 digest 幂等，同 ID 异 digest 返回冲突，不能由 operator API 伪造真实 runtime 身份。

所有 mutation 都要带 scope、权限、版本和 Idempotency-Key；敏感正文不得进入日志。QQ 入站和 Agent 消息的信任域必须在 schema 和 handler 中显式区分。任何模型输出都只是不可信内容，不能取得 QQ 工具权限。

### B. 通知投影

把入站消息、草稿创建、拦截命中、审批决策和 QQ 任务事件投影为可回读的 operator 通知，至少支持：

- workspace/账号隔离；
- 稳定排序和 cursor 分页；
- 相同事件幂等，不重复生成通知；
- `unread/read` 状态带 expected version/CAS；
- 通知正文脱敏，只返回事件类型、对象 ID、摘要、时间、关联账号/会话和证据引用；
- 没有外部通知 provider 时显示 `delivery_state=local_only` 或等价明确状态，不冒充已推送。

如果现有 outbox 只服务任务或 ARTEX，它不是 operator 通知 API；补一个薄的查询投影，不要改变已有 outbox 的任务恢复语义。

### C. 前端真实消费者

修改前端源文件（优先 `frontend/`），运行现有产品构建同步嵌入资源；不要只改 `backend/cmd/taskengine-server/product/app.js`。要求：

- “对话”页面保留 Agent 对话/QQ 收件箱两个 tab；列表和详情从实际 API 读取，空结果显示真实空态，不放演示消息；账号、群/私聊、来源和入站/草稿状态可见。
- QQ 出站只显示草稿、审核结果、许可状态和回执；没有真实发送按钮，或按钮明确 disabled 并解释本轮未授权。
- “通知推送”页读取上述通知 API，支持未读/已读的真实状态和失败/不可用态。
- 任何 API 错误、权限错误、过期数据、cursor 失败都不能渲染为“无数据”或成功。
- 仍保留 ARTEX 风格布局和已有安全/窄屏/键盘行为；不顺手重做其他待接入页。

### D. nil registry 设置路由回归

检查 `main.go` 的 `registry` 可选初始化和 `/v1/operator/system/global-proxy`、`/v1/operator/system/web-search` 的装配。选择最小安全修复：未配置 registry 时这些路由必须返回明确、无 panic、无 secret 的 `FEATURE_DISABLED`/`CONFIGURATION_UNAVAILABLE`（按现有错误契约），配置 registry 时继续走真实 handler。加入同进程 handler/请求回归，覆盖 GET/PUT 和鉴权/方法错误；不要通过强制创建假 registry 掩盖 nil 分支。

## 必须覆盖的测试与证据

测试名称可以按现有命名规范映射，但必须能逐项对应：

1. `TestConversationWorkspaceAccountIsolation`：workspace、账号、群/私聊交叉读取均拒绝或为空，不能串数据。
2. `TestConversationSourceBoundary`：Agent 入站不会落入 QQ 出站；QQ 入站不会自动创建 Agent 指令或 QQ permit。
3. `TestInboxEventIdempotency`：相同 event/digest 重放只一条；相同 event/不同 digest 是冲突；乱序事件按稳定时间/seq读取。
4. `TestDraftContentRuleAndPermitBoundary`：命中关键词的最终文本为 blocked，不发 permit；未命中仅为 draft，未签发 permit；不能调用 writer。
5. `TestNotificationProjectionIdempotencyAndReadCAS`：事件只产生一条通知；并发 read 只有合法 CAS；跨 workspace 失败。
6. `TestConversationAndNotificationHTTPAuth`：未认证、错误权限、错误方法、非法 cursor、越界对象均得到确定错误。
7. `TestOptionalRegistrySettingsRoutes`：registry 配置/未配置两条装配路径请求均不 panic；未配置返回明确 disabled/unavailable；已有配置 handler 的保存/读取回归继续通过。
8. 前端/产品验收至少证明：source 与 embedded 资源一致；真实 fixture API 被读取；双 tab、空态、草稿状态、通知未读/已读和错误态均有 DOM 断言。不要只用字符串 grep 证明页面实现。

每个测试报告 fixture DSN、是否真实外部、精确命令、退出码和关键原始输出。任何无法运行的测试记录为阻塞，不得改成通过。

## 交付物与证据目录

在项目现有证据根目录下新建本轮唯一目录，例如 `evals/results/sol-bba-qq-b7-20261101-<utc>/`；不要覆盖历史结果。目录至少包含：

- `REPORT.md`：修改文件清单、migration/head、API 路由与前端消费者、测试命令/退出码/关键输出、fixture 与外部边界、未覆盖项、失败项。
- `changed-files.txt`：相对 `/run/media/sun/6F53FE6DFAA8CECF/QQ群项目/BBA` 的实际文件列表。
- `sha256sums.txt`：只对本轮声明的源码/测试/文档文件哈希；不要把报告自身混入“源码闭集”。
- 原始 stdout/stderr 或结构化 JSON 结果；敏感值一律 `[REDACTED]`。
- 如有 migration，记录迁移前后 schema head、启动不隐式迁移证据和隔离回滚/重建方式。

还要给出下一轮依赖清单：任务全类型、计划任务、意向客户资产和 dashboard 客户计数必须继续保留，不能因本轮完成就写成整体产品已完成。

## 完成判定

只有同时满足以下条件才可报告“本轮工程切片通过”：所有必测项真实执行并退出 0；源码与嵌入资源一致；fixture 中双对话、通知和草稿链可回读；nil registry 路径无 panic；没有真实 QQ 外部动作；报告、文件清单和哈希可复核。否则报告 `需修改` 或 `证据不足`，逐项列出阻塞。

不要因为 sol 自己说“完成”而关闭任务。Astra 将读取最终源码、复跑关键测试并独立判定。
