# B7 #10 Account Read / Runtime Identity Implementation Plan and Author Evidence

**Goal:** 在现有对话 handler 中限制账号读取，并使 QQ inbox fresh/replay 都绑定可信 runtime token 的 owner epoch。

**Architecture:** 内部 `Actor` 兼容新增 `OwnerEpoch`，从现有 `RuntimeActor` 经 `operatorConversationAuth` 原样传递。inbox 事务先锁 account，再锁 runtime credential，最后复用 `authorizeRuntime` 验证 token、账号 owner 和持久 credential。消息读取复用现有 `Actor.AccountIDs` 语义。

**Tech Stack:** Go net/http、pgx、PostgreSQL 16 隔离 harness；无 wire、DDL、生成类型或产品 UI 修改。

**Spec:** 本轮用户限定的 #10 修复；根 `AGENTS.md`；现有 `base.go` runtime auth、`resolver.go`、主服务装配及 migrations。独立审核、PR、组合验证和全套 test-all 由父审核负责。本作者不再派子代理。

## Scope and implementation steps

- [x] 核实工作树 `/home/sun/.codex/worktrees/m1-runtime-review/BBA`，分支 `codex/m1-runtime-review`，初始无未提交内容，基线 `73a81569c9911419e6d26c4d50a5a5ce750b8465`。
- [x] 先仅导入原 #10 三份测试，在 main 基线上确认读取、错误 owner、credential 删除/inactive/epoch 缺陷为红。
- [x] `git merge --no-commit --no-ff 770e7766b88c96c89665535b66cdf9b214d5e388`；原提交父 `579a20c9c5ce347ff736d61b689632e5e5d29721`；无冲突，仅四个 #10 文件进入 merge。未带入旧 #8 的 `id == conversation_id` 限制。
- [x] 原候选的既有测试通过，再新增 token epoch fresh/replay 红测。仅内部新增字段使测试可编译，原候选授权逻辑此时未变。
- [x] 对 `operatorConversationAuth` 的现有真实 resolver-file 测试增加 epoch 断言，确认原转换丢失 epoch 为红。
- [x] 最小修复：Actor epoch 传递；事务内 account→credential 的 `FOR SHARE`；调用现有 `authorizeRuntime`。校验先于 conversation/message/notification 写和 replay 查回。
- [x] 更新 epoch-1 runtime 测试 fixture；增加精确拒绝、全部持久表快照与永久并发屏障回归。
- [x] 保留 operator resolver 格式限制，增加拒绝 operator `AccountIDs` 配置的回归。
- [x] 最终定向 taskengine + cmd resolver 验证、gofmt / diff 检查；runtime 提交及父节点写入本地 evidence。

## Actual behavior and assembly limits

`messages` 先验证 workspace、conversation ID 和账号范围，受限 actor 不能读取其他账号 QQ 消息；匹配账号和 unscoped operator 保持可读。`account_id IS NULL` 的 Agent 域保持现有语义，不把内部 ID 与 conversation_id 相等作为来源条件。

**账号读取的接线限制：** `readResolverFile` 当前明确拒绝 operator record 的非空 `AccountIDs`，`loadResolver` 也不设置 operator `Actor.AccountIDs`。所以账号受限 operator 的证据只支持 handler/custom auth contract，不能宣称主服务已支持配置这种 operator token；本轮没有放宽此格式。

Runtime token 的 epoch 是 resolver 文件中的可信身份声明。请求 body、header 不增加 epoch 参数，不能从当前两行持久状态推断调用者 epoch。合法新 epoch 可写新 event，也可读回同 event 的 replay；旧 token 在两行同步轮换后 fresh/replay 都拒绝。空账号、空/错误 scope、缺失账号、跨 workspace、错误 runtime、撤销/inactive、account/credential epoch 不匹配、token epoch 缺失/非正/错误均 fail closed。拒绝不创建消息、通知、任务、execution 或发送许可。

**#11 边界：** 本轮不改 principal 幂等。通知 SQL 与通知 event_key 身份未修改；同 event_id 跨账号通知碰撞由 #9 独立处理。

## Lock order and operations boundary

当前 inbox 顺序：account `FOR SHARE` → 对应 credential `FOR SHARE` → `authorizeRuntime` → 对话/消息/本地通知写入或 replay → 提交/回滚。两行锁持有到事务结束。canonical auth 要求 workspace/account scope、runtime、token epoch、account epoch、credential epoch、active 全部匹配。

源码核对：`heartbeat.go` 在 account `FOR UPDATE` 后更新账号投影；`login.go` 也锁 account 后更新投影；它们不是本轮 owner/credential 轮换协议。检索到的 credential INSERT 写入口主要是 fixture/阶段工具，未找到统一运行期 owner+credential 轮换事务 API。**本轮不能宣称已验收完整运维切换协议。** 涉及两行的运维事务应按 account→credential 同序获取锁并在同一事务提交；反向 credential→account 与 inbox 锁序可能死锁，长事务会延长 inbox 等待。此项应由父审核核对实际运维写事务，不以 SQL 行一致代替协议。

`0009_worker_cursor.sql` 对 credential 的 INSERT/UPDATE/DELETE 有 statement trigger，更新 `runtime_credential_population`；这属于运维 mutation 的合法持久变化。本轮不改该 trigger。并发测试从 mutation 事务内取预期完整快照，精确纳入 trigger 的变化，不忽略任何业务表。

永久 `TestB7InboxWaitsForRuntimeMutation` 覆盖四种 mutation 先持锁场景：owner、active 撤销、credential epoch、双行同步轮换。通过 `pg_stat_activity` + `pg_blocking_pids` 确认 inbox 卡在 `FOR SHARE`，提交前不能完成且 committed snapshot 不变；提交后 404，所有持久表只含 mutation 自身的预期变化。用当前源码回归，不引用历史 probe 代替。不声称覆盖所有锁排列或线上长事务。

## Shared fixture interoperability with #9

共享测试 helper 在 `operator_conversation_test.go`：

```go
runtime := conversationRuntimeActor("runtime_a", "account_a")
```

它初始化 runtime ID、account scope、`OwnerEpoch = 1` 和 chat.trigger；基础 `conversationActor()` 仍是 operator actor，epoch 为 0。若 #9 继续手工构造，最小兼容修改是：

```go
runtime := conversationActor()
runtime.RuntimeID = "runtime_a"
runtime.OwnerEpoch = 1 // must match the fixture account and credential
runtime.AccountIDs = []string{"account_a"}
runtime.Permissions["chat.trigger"] = true
```

若改变账号 owner，应使用匹配的 runtime/credential；轮换测试显式设置 token 新 epoch，不能根据数据库自动补 token claim。未编辑 #9 工作树；后续组合验证由父审核完成。

## Author evidence and reproducible commands

证据目录：`evals/results/m1-runtime-20261104/`（忽略目录，不提交）。均为 `AUTHOR_SELF_TEST / ISOLATED_FIXTURE`，使用本工作树源码、无网络容器、缓存依赖、独立临时数据库；harness 启用 `-race -count=1`，migration fingerprint 为 `e53b8fb166f26975331a1dd65db68dfafab184985b3aecec8c7cde03d5898a67`，本次 fixture 实际 head 为 25。该数字仅描述本轮证据，不作为后续 head 常量。

所有测试从指定工作树执行，前缀均为 `TMPDIR="$PWD/build/test-tmp" bash tools/kernel.sh`；命令、退出码和原始输出见相应 `.log` / `.exit`，最终清单另保存在本地 `commands.json`。

| 日志前缀 | harness 参数 | exit | 结论及限制 |
|---|---|---:|---|
| `01-main-red` | `test -run 'TestB7(Independent\|RuntimeOwner)' -v` | 1 | main 原缺陷复现；不是基础设施失败 |
| `02-original-candidate-green` | 同上 | 0 | 原候选既有测试通过，不足以验证 token epoch |
| `03-candidate-token-red` | `test -run 'TestB7RuntimeTokenFreshAndReplay' -v` | 1 | 初次回归含跨 workspace fixture principal 种子错误；不是最终该项证据 |
| `03b-candidate-token-red` | 同上 | 1 | 修正 fixture 后，只有四类 token epoch 缺口各自 fresh/replay 为红，含同步轮换旧 token |
| `04-resolver-epoch-red` | `test-all -run 'TestResolverGrantConversationPermissionsOnRealFile' -v` | 1 | 真实 resolver loader→auth bridge 的 Actor epoch 为 0 |
| `05-targeted-green` | `test -run 'Test(B7\|Conversation\|InboxEvent\|UnscopedOperator\|NotificationProjection\|DraftContent)' -v` | 0 | 修复后已有定向回归绿；发生于最终锁测试/fixture断言补全之前 |
| `06-lock-barriers` | `test -run 'TestB7InboxWaitsForRuntimeMutation' -v` | 1 | 测试 oracle 未计入 population trigger，四场景阻塞/404 已观测；随后修正测试快照 |
| `06b-lock-barriers` | 同上 | 0 | 完整 mutation 预期快照校正后四种屏障通过 |

最终定向结果：

| 日志前缀 | harness 参数 | exit | 范围 |
|---|---|---:|---|
| `07-final-targeted` | `test -run 'Test(B7\|Conversation\|InboxEvent\|UnscopedOperator\|NotificationProjection\|DraftContent)' -v` | 0 | 最终 taskengine 定向，含四种永久锁屏障、token fresh/replay 和 #8 合法 named Agent 回归 |
| `08-final-resolver` | `test-all -run 'Test(Resolver\|OperatorConversationAuth\|ServerAPISurfaceRoutesOperatorConversations)' -v` | 0 | cmd resolver/auth bridge/路由定向；其他 package 仅编译，不是全套执行 |

完整原始日志、退出码和 source hashes 留在本地 evidence。

父审核另提供 `evals/results/review-runtime-http-20261104/result.json`，并报告 18 项独立真实 HTTP PASS。本作者没有把该报告当作自身执行证据；最终 binary/build inputs 对齐和独立结论由父审核核对。

## Remaining limits

未运行真实 QQ、模型、外网 provider、现存业务数据库、生产部署或发布；未新增发送许可。按父审核分工，本作者不重复全套 test-all，不宣称 CI 或发布通过；父审核负责全套和 #9/#10/#11 组合验证。并发屏障覆盖 mutation 先持锁的四种 fresh 场景，replay 的权限拒绝由独立 handler 矩阵覆盖，未逐项跑并发 replay 排列。未解决其他 runtime endpoints 的锁协议，也未把 file resolver 变为热更新机制。


## Authorized stacked integration with #9 (2026-11-04)

父审核后续明确要求：#10 依赖 #9，先提交 runtime 实现，再整合通知分支；父审核负责设 PR base 为 `fix/b7-notification-scope`，本分支不推送。此后续授权扩展最初编辑边界，允许继承通知 SQL、migration、根指南与验收矩阵，并仅调整新增通知测试中的 runtime fixture；没有在通知工作树中做编辑。

- [x] runtime 独立实现已提交 `5298832a611aa31440d632e2a72a776cf4c26d30`，父节点为 main `73a81569c9911419e6d26c4d50a5a5ce750b8465` 与原 #10 `770e7766b88c96c89665535b66cdf9b214d5e388`。
- [x] `git merge --no-commit --no-ff codex/m1-notification-review`，目标精确为 `74df6694eb577e590c976db2a4a3a6a0f4fb4f1d`；无冲突，原 #9 `31489e6` 的祖先历史随分支保留。
- [x] 完整保留 #9 的 `0017` migration、migrate/head、三处通知 SQL、kernel harness、根指南、验收矩阵和通知计划文档。相对通知分支，生产 diff 仍仅为 runtime/account 修复。
- [x] 先运行未适配的两项通知 inbox 测试，返回 404、exit 1，确认缺少 token epoch 的组合缺口。日志 `10-stacked-fixture-red`。
- [x] 仅修改 `b7_notification_scope_test.go` 的三个 runtime 构造：runtime A/B 分别绑定其真实账号，统一使用 `conversationRuntimeActor(runtimeID, accountID)`，OwnerEpoch=1；不放松任何测试断言。
- [x] 组合后的两域与 resolver 定向回归均 exit 0；格式和 diff 检查通过，整合 merge 的 SHA/parents 见本地 commit.json；未运行完整 test-all。

组合后的 fixture 使用 #9 的 schema head 26；本文件上面的 head 25/fingerprint 描述整合前的 runtime 红绿证据。两域组合证据以 `11-stacked-targeted` / `12-stacked-resolver` 的实际 fingerprint/head 为准。schema 25/26 与新旧写入 SQL 的兼容限制沿用通知计划文档，不能将最初 runtime 不改 DDL 的边界误读为此组合不包含通知 migration。

最终提交、parents、生产源码/迁移摘要与命令退出码清单保存到本地 `evals/results/m1-runtime-20261104/commit.json`、`production-sha256.json`、`commands.json`；父审核独立进行组合完整 race、combined HTTP 与 build_inputs 对齐，本作者不借用其结论作为自测。


组合最终作者检查：

| 日志前缀 | harness 参数 | exit | 范围 |
|---|---|---:|---|
| `10-stacked-fixture-red` | `test -run 'TestB7Notification(ScopedEventIdentity\|ConcurrentSameScopedEventProjectsOnce)' -v` | 1 | 组合前 fixture 缺少 token epoch 的真实拒绝；随后只修 fixture |
| `11-stacked-targeted` | `test -run 'Test(B7\|Conversation\|InboxEvent\|UnscopedOperator\|Notification\|DraftContent)' -v` | 0 | 两域完整定向，包括通知七项、25→26/旧 schema 边界、rollback、runtime 负正向和四种锁屏障 |
| `12-stacked-resolver` | `test-all -run 'Test(Resolver\|OperatorConversationAuth\|ServerAPISurfaceRoutesOperatorConversations)' -v` | 0 | resolver 文件→Actor epoch 1/2、operator scope 格式拒绝、auth/路由；不是完整 test-all |
| `13-stacked-checks` | `gofmt -l`、`git diff --check`、依赖文件一致性检查 | 0 | 相对通知依赖仅三个生产文件不同；通知原 migration/head/harness/文档逐字节保留 |

组合 fixture 的实际 migration fingerprint 为 `2baf51257f4c452b1669368bec4a7b8cb04df7e155c55c40d6249a6e3d10526c`，head=26；所有测试由 harness 在隔离 PostgreSQL 16、禁网 Go 1.26 容器执行，race/count=1。
