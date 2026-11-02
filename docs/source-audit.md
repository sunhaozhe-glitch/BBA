# source-audit — P0-00 仓库与来源盘点（只读）

日期：2026-10-23。本轮不实施、不登录 QQ、不调真实写接口、未运行任何测试/构建/格式化；除本盘点新增文件外零写入。

证据等级标记：**实测**=本轮观察输出；**静态配置**=读取的配置文件；**源码**=静态读取；**历史记录**=文档/历史验收，不代表本轮实测；**[INFERENCE]**=由前级证据推断；**unknown**=未找到证据，不编造。

## 1. 指定工作目录（未来实施目标）

- `/run/media/sun/6F53FE6DFAA8CECF/QQ群项目/BBA` 枚举为空目录（实测）。
- `git -C <该目录> status --short --branch` → `fatal: 不是 Git 仓库…`，exit 128（实测）。
- 因此 03 §2 要求的 AGENTS.md/项目宪法、当前分支、未提交修改、启动方式、业务边界在该目录**不存在**；不能从封存树复制口径冒充目标仓库事实。

## 2. 历史工程 `封存/BBA`（用户既有资产；本轮未写入）

- 路径：`/run/media/sun/6F53FE6DFAA8CECF/封存/BBA`。分支 `bba/continuous-development`，HEAD `44a1d647e5ac3fbecf44019a86b67e2d75e8fb86`（实测 `git rev-parse`；scout 经 `.git/HEAD`、`.git/refs/heads/bba/continuous-development:1`、reflog 一致复核）。
- 工作树 dirty（实测 `git status`，2026-10-23）：10 项修改（`app/db/contract_selfcheck.go`、`app/db/migrate_test.go`、`app/db/tasks.go`、`app/server/dto.go`、`app/server/manager.go`、`app/server/server.go`、`app/web/.../qq-shared.tsx`、`app/web/.../qq/page.tsx`、`app/web/src/lib/api.ts`、`app/web/src/lib/types.ts`）+ 7 项删除（`standards/01..06*.md`、`standards/QQ群项目.txt`）+ 9 项未跟踪。
- 未跟踪清单包含 0013 加群实现的完整一套：`app/db/migrations/0013_qq_group_join.sql`、`app/db/migrations/down/0013_qq_group_join.sql`、`app/db/qq_group_join.go`、`app/db/qq_group_join_test.go`、`app/onebot/readapi.go`、`app/server/qq_group_join_api.go`、`app/server/qq_group_join_runtime.go`、`app/server/qq_group_join_test.go`、`app/web/.../group-join-panel.tsx`。
- 旁证：`app/db/contract_selfcheck.go` 清单已含 0013 对象，而 `docs/CURRENT.md` §3.9 记载迁移为 0001–0012；即 0013 与自检扩展属未提交 WIP（scout 静态；`.git/index` 二进制不可 grep，tracked 归属为 [INFERENCE]）。
- 结论：封存树不是目标工作树。0013 加群代码是**进行中改动**，其历史验收不覆盖它；对它的任何引用都必须标历史/源码级。

## 3. 启动方式与运行边界（实测观察）

- `docker ps` 观察到 `bba-cdev-login-f1`（镜像 `bba-cdev-f1:8bb315d`，host 网络，无公布端口）正在运行；`docker inspect`：挂载 `/app/data` 卷、PostgreSQL 数据卷、`/runtime`（bind，可写）、`/runtime/artex-accounts`、`/runtime/migrate-accounts`（只读）。`docker top` 进程：`start.sh` + `migrate`。未读容器日志/详情，未改变容器。
- `ss -ltnp`（实测）：`127.0.0.1:6199` 由 `bba-napcatd`（pid 2192）监听 → napcatd 守护当前在跑；`127.0.0.1:6210` 由 `qq`（pid 2640）监听 → 一个 napcatd 托管实例当前在跑（napcatd WebUI 端口段 6210–6249，见历史记录 `封存/BBA/docs/CURRENT.md` §3.12）。
- `ss -ltnp`（实测，按 sport 全接口查询，2026-10-23）：`:6099`、`:6002`、`:3001` 均无监听 → QQ 安装目录对应的 NapCat 实例、DXC 服务、登录运行时 OneBot HTTP 面当前均未运行；只能说明配置启用，不能说进程已加载。
- `0.0.0.0:8166` 由 python（pid 405168）监听（实测）——与 Strata Swift IQ3_S 启动脚本端口一致（scout 静态）；绑定 0.0.0.0 而非 loopback，与 §14 私密网要求相关，列入差异报告。`8080`（IQ3_XXS）无监听。
- systemd 用户服务 `bba-napcatd.service` → `%h/.local/state/bba-napcatd`，3 个实例目录（`qq-49b19b7c8937`、`qq-aeffae12147a`、`qq-2c7cdf77e452`）（scout 枚举观察）；EnvironmentFile `bba-cdev/napcatd.env` 未读（秘密）。

## 4. 业务边界（历史记录级）

- 封存树 `AGENTS.md`（静态）：改动须 commit+测试；成本感知委派（默认 delegate>primary）；全局并发≤2、迁移/共享 DB/同核心包串行、单写者；`bba/continuous-development` 上里程碑验收后可自动 commit；生产 DB 迁移/写入、服务启停、凭据变更、部署、真实 QQ 批量动作、不可逆外部行为、删除真实数据、换主框架/数据库/产品模型须停止并请求批准。`.omp/RULES.md` 为并存 sticky 规则（静态）。
- `docs/CURRENT.md`（历史记录）：单管理员 + 多账号机器人 + 真实任务 + 静态关键词工作流 + 提案审批 + 显式派发；四道门默认关闭；unknown 永不自动重试；无默认账号回退；`runtime_kind` 是执行边界。P5/P6/P8 验收均为历史层级记录，本轮未重跑。

## 5. OMP 模型别名读回（本机事实，未改配置）

`cfg://modelRoles` 读回（实测）：`default/plan=hyshentou/gpt-6.1-sol:high`、`slow=hyshentou/gpt-6-astra:high`、`smol=lan111/swift-1.5-iq3_s`、`tiny/memory=strata/swift-1.5-iq3_xxs`。与第二审查材料 §0 第 4 条声明的 alias 家族一致；无差异需报告。

## 6. 控制台登录决定（用户拍板，设计包未覆盖）

- 用户确认：登录流程在控制台内完成，2026-10-14 拍板（本轮视为产品事实）。
- 成文旁证：`/run/media/sun/6F53FE6DFAA8CECF/封存/DXC/docs/spec-console-v1.md:3-10`（老板 2026-10-14 口述首次成文；§1 登录移植：二维码展示/刷新/状态轮询/手机确认状态机移入控制台）。文中“草案/待审核”措辞不推翻用户决定。
- 设计包主文本仅有：02 §1（第 10 行）“现有合格登录机制”、02 §6.1（第 309 行）`login_state` 字段、03 §2（第 33 行）“登录体验”对照行；**没有**“扫码登录在控制台内完成”的产品流程条款。差异与建议见 `design-differences.md` §4。
- 历史实现（历史记录，非本轮实测）：`封存/BBA/docs/CURRENT.md` §3.12 记载控制台扫码登录（10s 心跳轮询、25s 快照新鲜度、`online` 需 WebUI 登录+core ready+QQInfo 在线+OneBot self_id 一致）；代码 `封存/BBA/app/server/qq_login_runtime.go:36-55,650-673`、`qq_login_api.go:1-12,31-64`、前端 `app/web/.../qq-login-dialog.tsx`（5s 状态轮询、浏览器本地渲染二维码、显式刷新/落库）。

## 7. 未确认项

- 目标目录的未来 AGENTS/宪法/分支模型：不存在，须实施批准时定义。
- 封存树 17 项修改的归属（哪些属 0013 主线）：按未跟踪文件名与自检旁证推断，标 [INFERENCE]。
- 容器 `bba-cdev-login-f1` 与当前 6099/6210 实例的确切对应：仅挂载/进程观察，未读日志 → unknown。
