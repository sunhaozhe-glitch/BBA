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

- [x] 在当前 main 上运行失败用例，分别证实同主体精确重放缺口和跨主体同键碰撞，保存真实退出码/日志。
- [x] 复用 task 幂等存储，主体/会话锁序明确，事务内不外呼；同一主体请求序列化，跨主体共享会话的 seq 安全。
- [x] 校验认证、当前权限、workspace、account 与 Agent/QQ 来源边界在重放前仍成立；已有合法命名 Agent 会话可追加。
- [x] 消息、草稿、通知、账本原子写入；失败不得部分落库。无提交的失败不永久占幂等键。
- [x] 同主体同键同请求返回原状态/原响应；异有效请求返回冲突；不同主体同键各自成功。兼容有账本的历史原 event_id 重放；对升级前无账本记录明确策略，不猜造旧回执。
- [x] 覆盖并发、请求字段差异、来源拒绝、scope、回滚和零 Chat/dispatch permit/task/model 副作用；运行隔离定向测试与 gofmt/diff-check。

命令：`mkdir -p build/test-tmp; TMPDIR="$PWD/build/test-tmp" bash tools/kernel.sh test -run 'Test(Principal|OperatorRequest|Conversation|DraftContent|B7)' -v`。仅允许 harness，不裸跑 Go。

## Task 2：兼容性与独立审核（只读子代理 + 主代理）

- [x] 只读核对旧依赖差异、账本 scope、旧记录、请求路径规范化、锁序、seq、错误/敏感信息，不把作者自测当独审。
- [x] 主代理审查全部最终 diff 与子代理审计结论，缺陷反馈实施子代理修正，不另开重复执行链。
- [x] 主代理独立运行完整 `tools/kernel.sh test-all`；真实 binary+新 PostgreSQL+文件 resolver 验证 HTTP、进程重启、lost response、撤销和副作用计数，绑定源码与构建身份。

## Task 3：文档与 PR 收口（主代理）

- [x] 据实更新 AGENTS/验收矩阵/本计划，说明升级前无账本历史、并发范围、回滚边界及未覆盖项；原始日志只在忽略目录。
- [x] 保留原 #11 历史，最终相对当前 main 仅包含经审查的有界变更；不强推，不复制旧依赖快照的其他功能。
- [ ] 发布/更新原 PR #11，对齐 main，核实最新 head 三项 CI、reviews、branch protection/rulesets。未获新合并授权，保持未合并。

## 证据与完成状态

原始证据存本机 `evals/results/m1-principal-review-20261104/`，按 commit/dirty/source digest/命令/退出码/原始日志记录。当前是实施计划，以上条目在完成前均不是 PASS。本轮不代表完整 M1、live 或生产发布验收。


## 实施与审核记录

- 原地使用已有隔离 runtime worktree，从 main5aaef18 新建 `codex/m1-principal-review`；保留此前所有工作树及证据。bounded Git fetch 超时后，通过认证 API 核对远端 main/PR11 SHA，所需对象本地已存在。
- 初次本地模型只落盘 `principal_event_scope_test.go` 并复现 RED；随后无有效代码修改，已关闭。默认模型子代理接手最小实现与永久回归；初次编译错误日志保留，最终业务 RED 为 red-03/red-04，GREEN 为 green-01（33 顶层通过）。
- 兼容性只读审查发现旧 base 有必要的 ledger/锁前置，也含不应回搬的 runtime 等差异。新代码只补 Agent/Draft；新 slash 写拒绝，规范请求兼容单 slash 历史 ledger；整数无损重放。
- 主代理独立真实 HTTP 53 项通过，真实 main binary 证实缺陷，再用旧有 ledger binary 建历史并切新 binary 验证；清理回读通过。完整 `tools/kernel.sh test-all` 退出0，运行期间后端与测试输入不变（含部分无测试 package）；最终源码审查未发现功能阻断；主代理发现并要求撤回 inbox key 校验一行越界修改，实施子代理已撤回，审查复核通过。最终源码重新完整 race 退出0（taskengine110.565s），HTTP53项通过；证据分别在 `m1-principal-final-full-20261104/` 和 `m1-principal-final-http-20261104/`，均绑定最终文件摘要，运行期间输入不变。

### 接手必须知道的限制

1. 旧 Agent raw key 没有主体归属，命中当前 thread/key 则冲突；旧 Draft 按 created_by 跨目标核对。同正文不构成自动恢复或补账本依据。
2. 当前账本只支持这两类固定201成功回执，不是任意HTTP状态/响应头重放服务；JSONB不保存原排版，首次/重放都经Go无损确定序列化。本轮验证的字节一致不代表恢复历史任意HTTP排版。
3. principal锁序列化同主体写入；会话锁仅保护参与该协议的Agent/Draft。inbox未加入锁，交叉并发可能409回滚，单列后续验收。
4. 本轮无新migration，但新旧raw/scoped-event writer不可混跑；停旧写后切换，旧binary回退会恢复缺陷且可能重复写，不宣称安全回滚。继承已合入通知迁移的binary/schema约束。
5. 原PR11历史保留；最终tree按最新main和受审实现生成，只审最终差异，不代表批准旧依赖快照全部变化。没有合并/live/部署授权。


最终已审实现 `operator_conversation.go` SHA256：`d0e9023a6f66d1a2d50acf01950ef776d430af7eb27bf1a84a63e464620479ac`；两个永久测试摘要与作者首次GREEN一致。源码审查由独立子代理执行、主代理复核；完整race/HTTP由主代理执行，均不替代人工approval。

本轮实现提交 `582fa89`；本地整合提交 `4c45330` 仅保留原PR11历史祖先，整合前后tree完全一致，未回搬旧依赖快照的文件变化。最终PR差异以main5aaef18为基线核对。
