# B7 通知身份按持久会话范围去重

**目标：** 通知投影按 workspace + 持久化会话行 + event key 去重，与已持久化的入站事件身份一致；无持久会话（NULL conversation）的投影仍按整个 workspace 去重；同一范围内重放不产生第二条。

**依据：** `AGENTS.md` 第 4.1、7.1 节。第 7.1 节现有描述是「按 event_key 去重」，本文件记录范围收窄后的语义，供主代理整合进验收矩阵；根指南与验收矩阵由主代理审查后同步。

**基线：** main `73a81569c9911419e6d26c4d50a5a5ce750b8465`；原 PR #9 候选 `31489e610e09419c1395f08c081673e3a820ffce`（原父 `579a20c`）。分支 `codex/m1-notification-review`，提交：`cde8a99`（原候选测试原样引入）→ `37e3fdb`（合并候选，父 `cde8a99` + `31489e6`）→ `dfbe4bb`（作者补测与 fixture 修正）。通知索引、三处 SQL 冲突目标与 harness 的实现沿用候选，保留最新 main 的来源检查。

**方案与编辑边界：**

- `migrations/0017_operator_notification_scope.sql`（追加）：删除 workspace 全局 `UNIQUE (workspace_id, event_key)`，建 `(workspace_id, COALESCE(conversation_id,''), event_key)` 唯一索引，记版本 26。只动约束与索引，不改任何历史行。
- `migrate.go`：tail 追加 0017，`migrationHead` 25→26。`serverMinimumSchema` 保持 **24**（原候选计划文档提到的 25 是那轮运行时的读取值，不是启动门）。
- `operator_conversation.go`：仅三处通知 INSERT 的 `ON CONFLICT` 目标（agent 消息、草稿、入站事件）。PR #8 的来源边界（允许合法行内部 ID 与显示会话 ID 不同）保留，未回潮 `id == conversation_id`。
- `tools/kernel.sh`：harness schema 尾随新 head。
- `b7_notification_scope_test.go`：复用原候选四项，新增三项，并把旧 head 数据库脚手架提为 helper。
- `tools/b7-manual-fixture-seed.sql` 已核对无需改动：手动种子是普通 INSERT、event key 互不相同，无 `ON CONFLICT` 目标。

## 步骤

- [x] 读取实际 DDL、migration tail/head、server 与 Bridge 启动门、三处通知 INSERT 全部调用点（无遗漏）。
- [x] 在未修复基线仅引入测试，跑红灯。
- [x] 合并原候选（`git merge --no-commit --no-ff 31489e6`，无冲突），绿灯。
- [x] 补 NULL 会话范围、注入失败回滚、旧 schema 边界三项；按账号使用其真实 runtime 修 fixture（不放松断言，不用反射等兼容技巧）。
- [x] 定向跑通知套件 + 既有会话/通知回归 + 迁移/schema 回归；`git diff --check` 干净。

## 本轮验证记录（2026-11-04）

作者测试身份为 `AUTHOR_SELF_TEST`，环境 `ISOLATED_FIXTURE`；原始命令、退出码与日志在本机未跟踪目录 `evals/results/m1-notification-20261104/`。

| 检查 | 结果与边界 |
|---|---|
| `TMPDIR="$PWD/build/test-tmp" bash tools/kernel.sh test -run 'TestB7NotificationScopedEventIdentity' -v`（未修复基线，仅测试） | 退出 1；`count = 1, want 3`——同一 event key 跨账号/跨会话被 workspace 全局键吞掉 |
| 同上，测试改为最终版本后在未修复基线重跑（`final-red-on-unfixed-baseline.log`） | 退出 1；同样 `count = 1, want 3` |
| `bash tools/kernel.sh test -run 'TestB7Notification\|TestB7DraftInjected' -v` | 退出 0；7 项 PASS，含真实 `25->26` 升级保留历史行 id/read/version 与 NULL 行 |
| `bash tools/kernel.sh test -run 'TestConversation\|TestDraftContent\|TestNotification\|TestInbox\|TestUnscoped' -v` | 退出 0；9 项 PASS，含 PR #8 来源边界与命名 Agent 兼容 |
| `bash tools/kernel.sh test -run 'Migration\|Schema\|Fixture' -v` | 退出 0；无失败，含独立空库全量迁移与既有分阶段升级 |
| 主代理独立真实 HTTP 验收 | `INDEPENDENT_EXECUTION`，schema 26，17 项 PASS：四组 acct/kind 相同 event 各一条、重放不增、read CAS、PR #8 正负向、无 permits；见 `evals/results/review-notification-http-20261104/result.json` |

覆盖：跨账号、群/私聊/不同会话相同 event key 各自成条；同范围重放不重复；并发同事件只投影一次；注入通知失败时整个草稿事务回滚（会话/消息/草稿/通知零残留，正向控制随后成功）；真实 25→26 升级历史行逐字节不变，旧约束移除、新索引就位，升级后跨范围可共存、同范围仍去重。

## schema 与兼容性边界

- **启动门保持 24，不是 25。** 门通过只说明允许启动，不说明新页面对应的表已齐全。
- **本 build 与 schema 25 不是滚动升级兼容**：新 build 仍能在 schema 25 上启动，但新的冲突目标在写入前即被拒绝（SQLSTATE 42P10，测试证明不写入任何行）。须先 `taskengine-migrate` 再部署本 build。
- **反向同样成立**：Bridge 门要求精确 head，schema 迁到 26 后旧 Bridge 不再启动。迁移与两个 binary 须一起切换。
- 旧 build 的冲突目标在 schema 25 上仍工作（已执行对应 SQL 正向控制），所以窗口期是「旧 build + 旧 schema」可用，不是任意组合。
- 未跑 `bash tools/contract.sh check`：本轮无契约文件改动，且按 PR #8 计划记录 OpenAPI 尚无 B7 路由，契约检查证明不了本行为。
- `COALESCE(conversation_id,'')` 为 NULL 通知提供 workspace 范围；`core.id` 的 DDL 禁止字面空串会话 ID，因此空串不会成为另一合法会话。三处 handler 都关联实际会话，NULL 路径通过直接 SQL 验证。

## 未覆盖风险

- 主代理已在实现提交 `4d56c81` 独立执行完整 `test-all`，退出 0（`review-notification-full-20261104/`）；该次 backend/测试输入未变化，后续仅有审查文档同步。真实 HTTP 的构建输入摘要也与当前生产源码一致。最新 PR head 的 CI 仍单独核对。
- 与 #10 的严格 owner 检查存在交互：本 fixture 已改为每个账号用其真实 runtime 认证，不再依赖一条凭据替别的 runtime 的账号行事。若 #10 引入 `OwnerEpoch` 新字段或共享 fixture 变更，需按其结果重跑本套件。
- 语义变化需写进验收矩阵：同一 event key 现在每个会话一条（此前整个 workspace 一条）。
- 子代理没有改根指南或验收矩阵；主代理负责审查后的文档同步与 PR 更新。未获合并/部署授权，作者自测不记为独立执行或生产验收。
