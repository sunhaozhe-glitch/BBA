# B7 操作员消息与草稿幂等闭环核验

> 子代理实施，主代理源码审核与独立执行；遵循根 AGENTS.md。

## 目标与事实基线

- 基线 `main=5aaef1861fb577534ab3b8c40139e9d6da36c2c9`；原 PR #11 `4715e12dd78adc4c86b9fd2d0496beb6bca91b1c`。
- 当前 Agent/Draft 写入没有调用持久 `taskReadIdempotency/taskWriteIdempotency`，消息直接使用原始请求键。原 #11 的依赖快照含未合入的精确重放/会话锁，不能把其测试记录套到当前 main。
- 只闭合 Agent 消息、QQ 草稿的请求身份→持久账本→消息/通知单事务→原响应重放；保留已有 Go/PostgreSQL handler、权限、来源边界、schema26 和 runtime epoch 修复。
- 复用 #11 的 JSON 分帧/versioned digest 思路，事件身份与既有账本的 workspace/principal/method/path/key 一致。请求摘要与事件身份分开；Draft 摘要覆盖所有有效请求字段。
- 不新增迁移、权限、wire 字段、模型运行或 writer，不调整 inbox runtime 流程，不引入旧快照其他变化。不迁移现存库，不调用真实 QQ/模型/ARTEX，不部署、不合并。

## Task 1：有界实现与作者回归（实施子代理）

文件：`backend/internal/taskengine/operator_conversation.go`；新/复用 `principal_event_scope_test.go` 和必要的独立幂等测试文件。

- [ ] 在当前 main 上运行失败用例，分别证实同主体精确重放缺口和跨主体同键碰撞，保存真实退出码/日志。
- [ ] 复用 task 幂等存储，主体/会话锁序明确，事务内不外呼；同一主体请求序列化，跨主体共享会话的 seq 安全。
- [ ] 校验认证、当前权限、workspace、account 与 Agent/QQ 来源边界在重放前仍成立；已有合法命名 Agent 会话可追加。
- [ ] 消息、草稿、通知、账本原子写入；失败不得部分落库。无提交的失败不永久占幂等键。
- [ ] 同主体同键同请求返回原状态/原响应；异有效请求返回冲突；不同主体同键各自成功。兼容有账本的历史原 event_id 重放；对升级前无账本记录明确策略，不猜造旧回执。
- [ ] 覆盖并发、请求字段差异、来源拒绝、scope、回滚和零 Chat/dispatch permit/task/model 副作用；运行隔离定向测试与 gofmt/diff-check。

命令：`mkdir -p build/test-tmp; TMPDIR="$PWD/build/test-tmp" bash tools/kernel.sh test -run 'Test(Principal|OperatorRequest|Conversation|DraftContent|B7)' -v`。仅允许 harness，不裸跑 Go。

## Task 2：兼容性与独立审核（只读子代理 + 主代理）

- [ ] 只读核对旧依赖差异、账本 scope、旧记录、请求路径规范化、锁序、seq、错误/敏感信息，不把作者自测当独审。
- [ ] 主代理审查全部最终 diff 与子代理审计结论，缺陷反馈实施子代理修正，不另开重复执行链。
- [ ] 主代理独立运行完整 `tools/kernel.sh test-all`；真实 binary+新 PostgreSQL+文件 resolver 验证 HTTP、进程重启、lost response、撤销和副作用计数，绑定源码与构建身份。

## Task 3：文档与 PR 收口（主代理）

- [ ] 据实更新 AGENTS/验收矩阵/本计划，说明升级前无账本历史、并发范围、回滚边界及未覆盖项；原始日志只在忽略目录。
- [ ] 保留原 #11 历史，最终相对当前 main 仅包含经审查的有界变更；不强推，不复制旧依赖快照的其他功能。
- [ ] 发布/更新原 PR #11，对齐 main，核实最新 head 三项 CI、reviews、branch protection/rulesets。未获新合并授权，保持未合并。

## 证据与完成状态

原始证据存本机 `evals/results/m1-principal-review-20261104/`，按 commit/dirty/source digest/命令/退出码/原始日志记录。当前是实施计划，以上条目在完成前均不是 PASS。本轮不代表完整 M1、live 或生产发布验收。
