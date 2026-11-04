# B7 Agent / QQ 会话来源边界核验

**目标：** 操作员的 Agent 消息接口只能创建或追加合法 Agent 线程；已有 QQ 群/私聊 ID 不能作为 Agent 线程追加消息，拒绝请求不留下消息、通知或发送许可。

**依据：** `AGENTS.md` 第 2、3.3、4.1、11 节；`docs/product-acceptance-matrix.md` B7-01/02/05。当前主服务确实装配该接口。PR #8 是现有候选补丁，#9/#10 已包含其提交；#11 的目标为组合分支。本轮复用并更新 #8，不重复建同一修复 PR，不把其余依赖功能带入本轮。

**基线：** main `d43f91a9ecac6fc3f8d704a660f8882f64f40721`；原 PR #8 head `579a20c9c5ce347ff736d61b689632e5e5d29721`。在独立 `codex/m1-b7-source-boundary` 工作分支中操作，不修改其他 worktree。

**方案与编辑边界：**

- `backend/internal/taskengine/operator_conversation.go`：核对现有候选补丁，在事务中验证 `kind`、`conversation_kind`、`account_id`，保留合法行内部 ID 与显示/外部会话 ID 不同的既有兼容性；不新建执行链、不扩大权限、不更改 API/schema。
- `backend/internal/taskengine/operator_conversation_test.go`：重用 #8 的来源边界用例，在未修复基线验证失败，必要时补拒绝写入及正向控制。
- 本计划与验收矩阵：仅补本轮证据范围，不改历史验证为当前 PASS，不把模拟测试记为生产/人工独审。
- 以合并提交保留原 PR 的提交祖先和当前 main，更新同一个 PR；推送须核对原 head，不能覆盖并行修改或强推。

## 步骤

- [x] 读取实际主服务鉴权与事务路径；在隔离 harness 中复现已有测试失败。
- [x] 整合已有 #8 补丁；验证来源边界、正常 Agent 创建、跨 workspace、拒绝时零副作用。
- [x] 跑隔离 Go race suite、真实 Go+临时 DB HTTP/浏览器流程，核对未发放 QQ 许可及清理。
- [x] 补证据与范围说明，完成提交前审查；交付原 PR #8 最新候选。
- PR 最新 head CI 与合并状态在 GitHub 动态核对，不在提交中预记未来 PASS；未经本轮明确授权不合并。

## 验证边界

只允许新建隔离 fixture 和假数据；不连接现存数据库、真实 QQ、付费模型或外部 writer。本轮不解决 #9 的通知身份、#10 的账号/runtime 边界、#11 的 principal 幂等，也不以本次成功替代上述项目的验收。原始命令、退出码与日志存入未跟踪的 `evals/results/m1-b7-source-boundary-20261104/`。

## 本轮验证记录（2026-11-04）

身份为 `AUTHOR_SELF_TEST`；先复用原回归证明 main 的跨来源追加错误返回 201（退出码 1），再证明原候选对既有合法命名 Agent 错误返回 404（退出码 1）。调整后以下命令/检查通过：

| 检查 | 结果和边界 |
|---|---|
| `TMPDIR="$PWD/build/test-tmp" bash tools/kernel.sh test -run '^TestConversationAgentAppend' -v` | 退出 0；两项定向回归 PASS；原测试还覆盖跨 workspace、runtime 拒绝及零 QQ permit |
| `TMPDIR="$PWD/build/test-tmp" bash tools/kernel.sh test-all` | 退出 0；隔离 Go 全 package race suite，无失败；部分包没有测试 |
| 当前源码构建的 Go + 新建 PostgreSQL + B7 browser smoke | schema check 25/head25、13 断言 PASS，退出 0；非真实 QQ |
| 同一服务的额外 HTTP 与数据库读回 | operator 对 QQ 群/私聊 Agent POST 均 404；reader 403、runtime 401；拒绝前后五类表计数不变；既有命名 Agent 可追加，新 Agent 可创建并追加两条；QQ 域没有 source=agent 的消息 |
| fixture 清理与 permit | Chat/dispatch permit 为 0/0；专有容器删除、两端口关闭，launcher 退出 0 |

临时服务构建 SHA-256、源码摘要、命令退出码与原始日志在本机证据目录；提交前核对源码与测试内容一致。没有 migration 或 wire 字段改动，不重复生成类型；但实际发现 OpenAPI 尚无 B7 路由，契约检查不能证明本轮 B7 来源边界，已在 AGENTS 记录该既有缺口。B7 合同补齐应另立有界变更，不和本轮修复混合。

文档回滚以撤销候选提交为准；若退回原 #8，仍带有此处已复现的命名 Agent 兼容性问题。合并和发布需要另行授权，不把作者自测记成独立执行或生产验收。

独立 Agent 只读源码复核未发现本轮阻断或重要问题；未重复运行作者测试，不代表人工 approval。
