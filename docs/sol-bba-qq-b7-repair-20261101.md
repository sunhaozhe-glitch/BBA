# BBA-QQ-B7 repair assignment — 2026-11-01

执行者：sol
项目根目录：`/run/media/sun/6F53FE6DFAA8CECF/QQ群项目/BBA`
架构基线：`/run/media/sun/6F53FE6DFAA8CECF/BBA/qq-automation-design`
原始 B7 任务：`docs/sol-bba-qq-b7-dual-dialogue-notify-20261101.md`

## 目标

在当前冻结工作树上完成 B7 的最小阻塞修复，使已有 QQ 对话/通知/草稿后端切片通过主服务路由访问，并恢复历史迁移保全门禁。只处理本任务列出的阻塞，不扩展 QQ 业务类型、计划任务、资产 CRM、MCP、NapCat 真实调用或前端大改。

## 当前已独立确认的证据

审查证据目录：`evals/results/astra-bba-qq-b7-review-20261101/`

- `migration-preservation.log`：命令退出码 1；`TestB4SlotMigrate22PreservesRows`、`TestB3Migrate21PreservesExistingData`、`TestWorkSettingsMigrate23PreservesRows` 均在 `historical contents changed` 或 `preexisting row contents changed` 失败。
- `schema.log`：`tools/kernel.sh schema` 退出码 0，0001→0016 可执行。
- `gofmt.log`：Docker `gofmt -l` 退出码 0，但列出 `cmd/taskengine-server/system_settings_surface_test.go`。
- `worktree.log`：当前改动和源码 SHA-256 清单。
- 当前源码：`backend/cmd/taskengine-server/resolver.go:264-310` 的 `serverRoutes`/`serverAPISurface` 没有对话 handler 字段或 QQ 对话路由；`backend/cmd/taskengine-server/main.go:156-166` 也没有装配 `store.OperatorConversationHandler(auth)`。
- 当前 handler 位于 `backend/internal/taskengine/operator_conversation.go:16-85`，路径覆盖 `/v1/operator/conversations`、agent thread message、`/v1/operator/qq-drafts` 和 `/v1/operator/notifications`，且明确不调用 QQ writer。
- `backend/internal/taskengine/migrations/0016_qq_conversations.sql` 合法新增四张 operator/QQ 表并把 schema version 写到 25。
- 三个历史测试的 snapshot 在迁移前收集旧表，迁移后却把新增表也纳入 map 后比较；现有代码位置：`migrate_b3_test.go:112-139`、`b4_slot_migration_test.go:67-95`、`work_settings_migration_test.go:67-95`。根因应按“只比较迁移前已存在的表和其稳定行内容”修正，同时单独断言 0016 新表存在，不能用扩大硬编码排除列表掩盖新增 schema。

## 必须完成

1. **主路由接线**
   - 在 `serverRoutes` 增加清晰命名的对话/通知 handler 字段。
   - 在 `main.go` 的真实装配中传入 `store.OperatorConversationHandler(auth)`。
   - 在 `serverAPISurface` 中把已有四类 endpoint 路由到该 handler；路由匹配必须覆盖真实 handler 支持的路径，避免吞掉既有 `/v1/operator/tasks`、系统设置和内部 runtime 路径。
   - handler 未配置时必须 fail closed（404 或项目既有不可用语义），不能 panic。

2. **主路由回归测试**
   - 在 `backend/cmd/taskengine-server` 增加永久测试，使用 marker handler 证明至少以下路径被转发：
     - `GET /v1/operator/conversations`
     - `GET /v1/operator/conversations/<id>/messages`
     - `POST /v1/operator/agent-threads/<id>/messages`
     - `POST /v1/operator/qq-drafts`
     - `GET /v1/operator/notifications`
     - `POST /v1/operator/notifications/<id>`
   - 至少包含一个不应由对话 handler 接管的既有路径和 nil handler fail-closed 断言。
   - 保留现有 auth、workspace/account 隔离、草稿规则命中、通知幂等/read CAS 定向测试，不删除或放宽负面断言。

3. **历史迁移保全修复**
   - 修改三个历史迁移测试的 snapshot 逻辑，使其记录迁移开始前的表集合，只比较这些旧表的稳定数据；不得把迁移后新增表的空数组作为“旧数据改变”。
   - 继续保留已有 `work_slots` 兼容列处理和 work settings 排除语义。
   - 在至少一个适当的测试中显式断言 schema 25 的四张 `0016` 新表存在，并且新表引入不改变旧表快照；迁移第二次和 reopen 的旧表数据也必须保持。
   - 不要通过修改 `0016_qq_conversations.sql` 删除表、回退 schema head、放宽业务约束或跳过历史测试来获得绿色。

4. **格式与证据**
   - 用 Docker 中的 `gofmt` 格式化所有本轮修改的 Go 文件，不能依赖主机 Go 工具链。
   - 运行并保存原始 stdout/stderr、UTC 时间和退出码：
     - `tools/kernel.sh test -run 'Test(ConversationWorkspaceAccountIsolation|DraftContentRuleAndPermitBoundary|NotificationProjectionIdempotencyAndReadCAS|ConversationAndNotificationHTTPAuth)$' -count=1`
     - `tools/kernel.sh test -v -run 'Test(B4SlotMigrate22PreservesRows|B3Migrate21PreservesExistingData|WorkSettingsMigrate23PreservesRows)$' -count=1`
     - `tools/kernel.sh test -run 'Test.*(Conversation|Operator|Route|SystemSettings|Migration|Migrate|WorkSettings|B3|B4)' -count=1`（若过宽导致无效收集，保留失败并改用明确的分组命令，不得伪造通过）
     - `tools/kernel.sh schema`
     - Docker `gofmt -l` 对所有改动 Go 文件；输出应为空且退出码为 0。
   - 每个命令记录实际退出码；测试失败时保留完整堆栈，不以外层包装器退出码代替子测试结果。
   - 生成新的交付证据目录，不覆盖 `evals/results/sol-bba-qq-b7-20261101-20261101T061618Z/` 或 Astra 审查目录。报告必须区分“修复后独立可验证”与“尚未实现”。

## 禁止范围

本任务不得：

- 登录 QQ、发送消息、加群、加好友、调用真实 NapCat/OneBot、启动或修改真实 QQ/NapCat/ARTEX/6300 运行目录。
- 写生产数据库、安装在线 MCP、调用付费模型、读取或输出任何凭据/API key/token/password/secret/连接字符串；如必须描述敏感值，使用 `[REDACTED]`。
- 删除 `.cache`、`build`、`evals` 或任何运行凭据。
- 把静态菜单、fixture、作者自述或 handler 单元测试当作主服务/前端完成证据。
- 修改产品验收矩阵为通过；最终状态由 Astra 根据新源码和新日志更新。

## 交付报告要求

报告必须列出：

- changed-files（相对项目根目录）和每个文件 SHA-256；
- 主路由前后行为、迁移 snapshot 修复逻辑和新增断言的源码位置；
- 每个上述命令的原始日志路径、退出码、测试范围；
- 明确说明前端真实消费者、QQ 入站事件 adapter、NapCat runtime、真实 QQ 动作仍未实现/未执行；
- 如果任何要求未完成，标为 `需修改`，不要用“部分完成”掩盖未完成的阻塞项。
