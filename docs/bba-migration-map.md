# bba-migration-map — 历史 BBA 能力逐项迁移判定（P0-00，只读）

**本行统一标注**：来源 = `/run/media/sun/6F53FE6DFAA8CECF/封存/BBA`，HEAD `44a1d647e5ac3fbecf44019a86b67e2d75e8fb86`（实测 `git rev-parse`），工作树 17 项已跟踪修改 + 9 项未跟踪（实测 `git status`，含 0013/`qq_group_join_*`）。历史测试/验收记录均为**历史层级**，本轮未运行任何测试/构建/服务；本轮零写入。决策词表按 03 §7（retain/migrate/rewrite/retire/defer）；本表无 retire（消费者未穷尽前禁 retire，02 §9 行 459–460）。

| 能力 | 现有路径/服务 | 调用方 | 已有测试/真实证据 | 决策 | 替代目标 | 切换与回滚 |
|---|---|---|---|---|---|---|
| 显式迁移账本（checksum/漂移拒跑/链顶回退/协调锁）| `app/db/migrate.go:119,176`、`app/cmd/migrate/main.go:28`、`migrations/0008–0013.sql`+`down/` | `migrate` CLI、`app/migrate-deploy.sh`、CI | 静态 + 历史（CURRENT §3.9；本轮未跑） | retain | 沿用；新内核表并入同一 ledger | 前向修复；Down 按既有冻结规则 |
| 启动自检清单（~80 对象，fail closed）| `app/db/contract_selfcheck.go:22,104` | `PrepareDeployment`/`CheckDeployment` | 静态 + CI 历史（`deployment-contract.yml:59-60`）| retain | 新内核对象逐条入清单 | 清单项缺失=启动失败 |
| 任务存储 / 暂停 / 恢复 / 终态 CAS | `app/db/tasks.go:585,592,631,644` | `manager.go`、`scheduler.go`、`engine.go`、`server.go:814,779,1021` 控制路由 | 静态 + 历史测试（`task_control_routes_test.go` 等；未跑） | migrate | 持久字段语义不变，换 P0-04/05 承载（幂等/版本冲突） | 双写窗口 + 回滚旧表 |
| 意图级 pause/resume/cancel（soft/hard、批控≤100）| `app/server/task_control.go:97,187,270` | `server.go:814,817,779`；前端 `api.ts:378-415` | 静态；断言历史（`intercept_*_test.go`） | rewrite | 新领域无意图/planner；暂停语义由新状态机重定义并配**新测试**（持久优先→取消在飞→转 unknown 的顺序可继承） | 保留旧端点至新面可用（P0-19） |
| 领取事务/守卫锁/fencing | `app/db/exploration.go:2944`（+`task_wakeups FOR UPDATE` 段） | planner/worker、契约测试 | 静态 + 历史真跑 | migrate | claim/租约形状照搬，去 graph 专属（P0-05 `FOR UPDATE SKIP LOCKED`/等价） | 单库原子性要求见设计覆盖表 |
| QQ 动作 claim/permit/unknown（四道门）| `app/db/qq_actions.go:671-1001`、`server/qq_action_runtime.go` | `qq_actions_api.go`、`qq_workflows_api.go`、drain hook | 静态 + 历史 P5（SIGTERM：wire 后 unknown、发送 1 次） | retain | 直接作 P0-08/09/11 实现基底；`Abort` 仅零发送证明、unknown 永不自动重发（CURRENT §5.3 历史） | 状态闭集冻结 |
| 主动加群引擎（含登录运行时共用基础设施）| `server/qq_group_join_runtime.go:42-72,196-209,287-347`、`db/qq_group_join.go`、`migrations/0013*.sql`（**未提交**） | `server.go:310-311` 根 context 启动；管理路由 `server.go:1038+`；前端 `group-join-panel.tsx`（未跟踪） | 静态源码；测试 `qq_group_join_test.go` 存在未跑；0013 未入库 | **migrate**（语义映射）＋**待裁决**：24h pending→`timeout` 释放+冷却（runtime `:335-346`）与 01 §5 行 252「pending/unknown 不释放额度」冲突，须负责人裁决后定 migrate/rewrite | 新内核 goal_slot/claim 模型（02 §6.1:319-320） | 不删除（未提交改动属用户资产） |
| 扫码登录链路（控制台内完成）| `server/qq_login_runtime.go:36-55,650-673`、`qq_login_api.go`、`napcat/login.go:29-63`、`web/.../qq-login-dialog.tsx`、`api.ts:897-947`、`server.go:1004-1009` | 控制台前端 + 配置实例 `local-6099` + `napcatd`（6199 当前在监听，实测） | 历史验收 CURRENT §3.12/§6（10s 心跳、55min 凭证复用、刷新不重放、在线判定四条件）；本轮未实测 | retain | 与新设计「现有合格登录机制」（02 §1 行 10）对接；产品决策（2026-10-14 拍板）详见 design-differences §4 | 保留至新控制台入口就绪 |
| 鉴权 / Basic gate / SSE 凭据规则 | `server/auth.go:115-227`、`basic_auth.go:85-166`、SSE 头 `server.go:2768,2819,2775,2847` | 全站 `/api/*`；前端 `api.ts:227` | 静态 + 历史 P6（SSE cookie 通过、无 URL token） | retain | 新内核复用同一中间件；query-string 凭据保持拒绝 | 同源 Cookie 规则不变 |
| 模型配置/池/熔断/计量 | `db/config.go:97-419`（`llm_profiles`）、`llmhealth.go`、`llmretry.go`、`llm_usage.go` | `orchestration.go`、`llmpool.go`、Web `/api` | 静态；测试历史（未跑） | retain | 作为 Model Gateway（P0-16）存储契约来源；L1/L2 路由语义归新内核 | key 只经 env 注入 |
| 备份/恢复（DB 级 + 任务归档）| `pg_dump -Fc`/`pg_restore --list`、部署脚本 pre-deploy 备份（CURRENT §3.9/§6 历史）；`task_archives.go:371,549`+`task_archives_restore.go:159` | 部署脚本、归档 API `server.go:780-782` | 静态 + 历史演练产物（run-001 哈希/回放核验） | retain（DB 级）＋ migrate（任务归档改挂新内核） | 恢复后零自动重发、unresolved 显式展示（P0-18） | 恢复需单独批准停机 |
| 审计（in-memory ring 2000，重启即丢）| `app/guard/guard.go:29,41,75`；`GET /api/audit` `server.go:3525` | agent hook、前端审计页 | 静态结构事实 | rewrite | 新内核需持久审计账本（§1 第 10 条按现状不满足） | 迁移前不下线现有端点 |
| 通知投递租约 claim | `db/notification_delivery.go:124-246` | `server/notifier.go` | 静态 + 历史单测 | defer | 非加群主链，证据不足以判归属 | 不删除 |
| 旧 Python 内核（模块 02/12/13）| `reference-modules/02-QQ任务事件内核/task_store.py:61,159`（BEGIN IMMEDIATE+SAVEPOINT、状态闭集）等 | 仅本地 CLI/测试，无进程外调用方 | 静态；手册记录历史 unittest 通过 | migrate（事务约定）＋ defer（Python 本体不入新栈） | 语义搬 PG；SQLite 不入新栈 | 不删旧目录 |

## 消费者清单（02 §9.1 要求；静态枚举，未运行）

- **task 控制**：`server.go:814`（单控）、`:779`（批控）、`:817`、`:1021-1022`（bot 级）。
- **归档**：`server.go:780-782`。
- **SSE**：`server.go:749`、`:841`、`side_questions.go:551`；唯一前端消费者 `web/src/lib/api.ts:227`（同 hostname）。
- **审计/活动**：`server.go:3525`、`:2482`、`:2577`、`:842`。
- **登录路由**：`server.go:1004-1009` + 前端 `qq-login-dialog.tsx`/`api.ts:897-947`。
- **加群管理**：`server.go:1038+`（GET/PUT 逐账号策略）+ `group-join-panel.tsx`（未跟踪）。
- **DB 直连**：`migrate` CLI→`db` 包；`agent/`→`exploration.go`；`server/`→`tasks.go`/`qq_actions.go`/`notification_delivery.go`/`config.go`；部署脚本。
- 无法穷举的外部脚本：**consumers_not_enumerable**（照 02 §9.1 口径报告）。

## 现有安全/契约测试命令（仅列出，本轮未执行）

- CI 全量：`go test -count=1 -p 1 -parallel 1 ./...`（`.github/workflows/deployment-contract.yml:60`，前置一次性 migrate `:59`）。
- 定向：`app/db/qq_group_join_test.go`（服务层契约：单轮单申请、轮询确认、开场最多一条、通知重投只确认一次——文件存在，本轮未跑）；`app/server/qq_group_join_test.go`；`p13_storage_test.go`、`migrate_test.go`、`auth_test.go`、`basic_auth_test.go`、`security_limits_test.go`。
- 前端：`tsc --noEmit`、`npm run build`、目标 Biome（历史通过记录 CURRENT §4.3）。

## AGPL（静态）

- `app/LICENSE`＝AGPL-3.0 全文（`:10,63,636,660`）；`app/README.md:12,466-468` 声明继承上游 `Autumn-27/ARTEX` AGPL-3.0；reflog 记录并入上游未改动源码；Go 导入路径 `github.com/Autumn-27/artex/...` 确认整棵 Go 树继承。
- 设计包 `evidence/ARTEX-meta.json`：`spdx_id=AGPL-3.0`，上游 commit `620ca5d9…`。
- 结论 [INFERENCE]：搬入新仓库的 Go 代码沿用 AGPL-3.0（含网络服务条款）；这是工程事实汇总，非法律结论（01 §14 行 705）。
