# ADR: 离线目标循环与进程内受限编排

- 日期：2026-11-05；状态：接受本阶段契约与架构边界，运行实现和验收另据源码/证据。
- 范围：BBA 主服务内新增离线目标循环；运行实现及下列命令是否通过以当前提交的 PR/执行证据为准，不授权部署。

## 决策与历史边界

允许一个受限的 BBA Go 进程内编排模块，复用既有 registry 配置快照、PostgreSQL、HTTP 鉴权和唯一 `frontend/` 产品面。它协调分析/规划，不争夺 Task Engine 的 QQ 执行所有权。原 clarification-only 模型路由、输出验证及 `request_join` Proposal 保持原语义；复用 provider 基础设施不要求新规划输出契约也只能 clarification。独立、严格校验的规划输出不提供 callable tools 或权限。

本 ADR 明确替代有效 [2026-10-31 final closeout](bba-final-closeout-2026-10-31.md) Decision 中 “ARTEX is a bounded external fact/goal graph reference.” **作为永久核心范围限制**的含义，及其 S2 旧 no-runtime 限制中禁止新增本进程受限 BBA 模块的部分。S2 的原工程事实“当时未复制/链接 ARTEX Go”仍成立，本阶段也不复制/链接上游运行时。外部 adapter 的 outbox/checkpoint/reconcile 边界不变。[2026-10-31 Phase A](2026-10-31-artex-phase-a.md) 的 UI 范围仍是该阶段历史事实，`frontend/` 仍是唯一产品源码。2026-10-29 文档是 HISTORICAL，不修改历史原文。

## 参考与本地职责

来源仓库：`https://github.com/RuoJi6/ARTEX`。固定参考 ARTEX `v0.3.24` / `e740334b9042d6da64156b0adb37a293117e140e` 的 `agent/planner.go`、`agent/worker.go`、`server/engine.go`、`db/schema.sql`。仅适配双图语义、角色分工和事件触发：资产图记录目标群等受管理资源；探索图同时记录 goal、intent、fact、digest、finding、hint 及其生成和推导关系，通过 anchor 关联资源（不是“任务 DAG + 事实库”）；planner 根据新证据调整有界计划，analyst 产生带引用的分析，协调器领取步骤、归约结果并推进状态。原渗透测试领域仅作来源背景，SDK shell、MCP、默认工具及领域动作均不导入。

本轮复用范围：

| 范围 | 处理 | 原因 |
| --- | --- | --- |
| BBA registry、Agent/Model/Skill 版本、operator HTTP 鉴权、PostgreSQL 事务、唯一前端 | 直接复用 | 配置和权限已有来源，运行快照落入 PostgreSQL |
| ARTEX 双图、anchor、planner/worker 结果唤醒和规划记忆 | 固定版本语义适配为 BBA 受限模块 | 群资源和证据的领域不同，恢复和副作用由 BBA 负责 |
| ARTEX Go 运行时、Bash/MCP/默认工具、渗透测试调度 | 仅核对参考，不复制、链接或启动 | 不引入任意动作、第二执行内核或常驻服务 |
| 既有 BBA ARTEX adapter/outbox | 保留独立外部同步职责，本轮不接线 | 不向本地域反向写入或控制 run；未来导出须另行授权 |

PostgreSQL 是 run、round、logical step、attempt、event、evidence、memory 和 stop 的唯一权威；重启从持久事实恢复，内存唤醒只作提示。新本地资产/探索图属于此域，外部 `graph_sync` adapter 无权写入；图是可追溯投影，既不是执行许可，也不能作为未经验证的事实。消息/模型输出始终是不可信输入。配置在开始时固定 Agent/Model/Skill 版本及受保护快照，响应仅含无密钥投影，不暴露 endpoint 凭据。

启动、有效新窗口、步骤结果和到期检查触发有界推进；不足证据进入等待，不用重复扫描伪造新一轮。事件/窗口身份和请求 digest 去重；状态、事件和幂等回执原子提交。领取使用租约/attempt fence，过期领取只允许重试同一 logical step；旧 attempt、旧 run version、停止后的迟到结果不得推进状态。跨 workspace/主体/thread/group 必须重新鉴权，先鉴权再读幂等回执。

## 固定边界与 fixture gate

固定最多 **4 rounds、每 round 2 logical steps、每 step 2 attempts、每 attempt 30 秒、run 30 分钟**，重试不另计 logical step。事务/存储暂时失败由主服务内 loop 以 200 毫秒至 5 秒有界退避重读持久待办，每次 Tick 最长 35 秒；每个角色 attempt 的租约仍为 30 秒，不能借重试增加逻辑步骤或动作权限。到达运行期限为 `TIMED_OUT`，预算耗尽为 `LIMIT_REACHED`；启动后 `round=0`，窗口不足或失效为 `WAITING_INPUT` 并持久化明确 reason。只有新鲜、匹配 group、顺序有效且有可用消息的窗口才能被消费；过期/缺失/无效窗口不推进分析。注入拒绝可返回 `WINDOW_EXPIRED` / `VALIDATION_FAILED`，等待原因仍可查询。Schema 无法比较时间，时间顺序、消息归属窗口、到期和大小的字节校验由服务端执行。

synthetic window 入口仅在 **全部**条件成立时启用：`-offline-goal-fixture`、`-development-operations`、`BBA_FIXTURE_ONLY=1`，以及实际 PostgreSQL 连接为本地（loopback 或 `/fixture` Unix socket）并核验 `current_user=bba_fixture`；或者在同样本地连接条件下核验 `current_database=bba_dev AND current_user=bba_dev`。不得从 flags、DSN 标签或库名猜测 fixture；数据库核验失败即关闭。主隔离 runner 显式设置环境。本阶段 gate 也约束启动离线 fixture run；关闭 gate 时仍可读取已授权历史记录。列表 `meta.fixture_enabled` 仅报告该核验结果，不授予写权限。

本阶段 implementation 固定 `deterministic_fixture`，绝不调用模型、QQ 或付费服务。后续真实模型阶段须另行确认 endpoint、数据边界、预算和充分证据/不足证据/tool-failure 测试；不得以本次 fixture 结果宣称模型质量或真实工具能力。

## Operator wire contract

所有 `/v1/operator/goal-runs` 操作只接受既有 operator Bearer 鉴权，GET 要求 `conversation.read`，POST 同时要求 `conversation.read` 与 `conversation.write`；沿用 workspace/当前主体和 Agent thread 所有权隔离。mutation 必须携带 `Idempotency-Key`，键在 workspace/principal 内唯一，请求 digest 绑定动作、run 与请求体（启动绑定启动体）；同请求读回原回执，换 body 为 `IDEMPOTENCY_CONFLICT`。

- `GET/POST /v1/operator/goal-runs`：列表 `data` 为数组，`meta.fixture_enabled`；启动体为 `thread_id, group_id, goal, planner_ref:{id,version}, analyst_ref:{id,version}`。
- `GET /v1/operator/goal-runs/{run_id}`：`data` 为 Run，含 configuration、memory、rounds、evidence、双图投影。
- `POST /v1/operator/goal-runs/{run_id}/fixture-windows`：只接收合成 `event_id, group_id, window_start, window_end, expires_at, messages:[{id,at,text}]`。event 身份重复不重复消费，不可用数据保留拒绝/等待证据。
- `POST /v1/operator/goal-runs/{run_id}/stop` 和 `/confirm`：体含 `expected_version`，CAS 失败拒绝。stop 持久化后阻断领取和迟到结果；confirm 只允许当前 `AWAITING_CONFIRMATION` 且证据/版本仍有效时，由操作员确认；无模型自动确认。`CONFIRMED` 仅确认分析结论，不创建 Task/Proposal/permit 或 QQ 动作。

状态为 `WAITING_INPUT, RUNNING, AWAITING_CONFIRMATION, CONFIRMED, STOPPED, TIMED_OUT, LIMIT_REACHED`。错误沿用现有 envelope，包含 `WINDOW_EXPIRED, GOAL_FIXTURE_DISABLED, GOAL_STATE_CONFLICT, GOAL_LIMIT_REACHED, VALIDATION_FAILED, ACTION_FORBIDDEN`，其余鉴权、scope、存储及幂等错误仍 fail closed；HTTP retryable 从不授权重复外部动作。

主实现计划新增 migration `0018_goal_runs.sql`，基于本轮 `origin/main` schema head 26 升为 27；这些是本轮实现坐标，不改变 schema 事实必须查 migration/`SchemaHead()` 的规则。migration 随本闭环实现追加，验收使用隔离数据库。实现验收需覆盖隔离/重放/领取崩溃/结果归约崩溃/迟到结果/stop/确认过期/限额，不能由 JSON fixtures 替代。

许可来源固定为上述 `v0.3.24` / `e740334b9042d6da64156b0adb37a293117e140e` 的根 `LICENSE`（GNU AGPLv3）和 `web/LICENSE`（MIT，Copyright (c) 2024 Mohammed Arham Khan）；本机 checkout HEAD 不替代该固定来源。BBA 既有 `frontend/NOTICE`、`frontend/licenses/ARTEX-AGPL-3.0.txt`、`frontend/licenses/ARTEX-web-MIT.txt` 保留，不宣称全仓 MIT。本阶段模块为本地实现，无上游 Go 代码复制或链接，不新增 copied-code 版权归属声明；未来复制、链接或分发须另行核对许可义务。

## 离线演示与本阶段验收

从本 PR 的仓库根目录执行（缓存 Go/Node 依赖和 Docker 镜像需已准备，不安装全局依赖）：

```sh
python3 tools/goal-loop-fixture.py start --output evals/results/offline-goal-demo-01
```

启动器只创建带自身标签的隔离 PostgreSQL、当前源码主服务和私有配置目录；返回 loopback `/product/chat` URL 及 0600 token 文件路径，不打印 token。首次访问使用该文件认证，进入 **Agent** 页签，填写一个新线程 ID、群 ID `20001`、本阶段目标，选择预置的 **Offline planner** 和 **Offline analyst**，点击启动。

1. 注入第一段稀疏窗口，等待第 1 轮 `WAITING_INPUT`；查看预算/时间缺口，以及原文、摘要、测试判断、提示各自标记。
2. 注入第二段补充窗口，等待第 2 轮 `AWAITING_CONFIRMATION`；查看新增证据 ID、先前缺口参与的新计划原因、资产/探索节点及 anchor。
3. 点击过期窗口，看到 `WINDOW_EXPIRED`；当前判断和轮次不被伪造或推进。提供新的有效窗口才能重新分析。
4. 点击“确认离线结论”进入 `CONFIRMED`，或点击“停止运行”进入 `STOPPED`。停止后迟到结果不能唤醒 run。

停止前可运行 `restart --output <同目录>`，刷新页面重新读取相同 run/step；`stop --output <同目录>` 只清理自身进程和容器，**删除此隔离数据库**并保留私有输出目录。输出目录必须新建且为 `evals/results/` 的直接子目录，禁止继承生产 DSN。

自动验收入口：

```sh
# 全部使用隔离 PostgreSQL；包含并发、事务失败、旧 attempt/轮次、停止、到期与迁移测试
TMPDIR=<仓库内忽略的临时目录> bash tools/kernel.sh test-all
bash tools/contract.sh generate
python3 -m unittest tests.product.test_goal_loop_fixture_launcher -v
# 显式指定启动器创建的私有目录；重启同一 fixture 并在其容器中执行完整 dump/restore 对比
BBA_GOAL_FIXTURE_OUTPUT=evals/results/offline-goal-demo-01 python3 -m unittest tests.product.test_goal_loop_integration -v
# 使用已有 Playwright/Chrome；第一个浏览器脚本为 API fixture，第二个为真实隔离主服务
node tools/goal-loop-browser.cjs <忽略的浏览器证据目录>
node tools/goal-loop-live-browser.cjs evals/results/offline-goal-demo-01/fixture-config.json <另一忽略的证据目录>
```

本阶段通过仅表示 **离线编排闭环通过**。确定性实现按“需求/预算/时间”字段演示证据检查，配置中的模型与 Prompt 不会被执行，不证明真实规划质量、Collector live 接入、`group.analyze` Task 执行或 QQ 能力。下一阶段应独立接入用户指定真实模型，先确认端点、数据范围、预算，再以信息充分、信息不足、工具失败三种情形验证规划；不能先扩展其他产品阶段来无限延后此项验证。
