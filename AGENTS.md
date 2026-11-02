# BBA Agent 开发与交接指南

本文件是仓库级开发上下文入口。它描述当前工作树中能由源码、配置或设计决策直接确认的行为；阶段验收记录不是当前运行状态，也不构成生产授权。

## 项目概述

BBA 是一个围绕 QQ 账号、任务编排、审计和受控运行时边界构建的系统。核心服务是 Go Task Engine，使用 PostgreSQL 保存执行事实；同源控制台由 Go 二进制嵌入并提供。仓库还包含 OpenAPI 契约工具链、独立 Koishi Character runtime、只采集的分析 Collector、NapCat cleaner 插件候选、fixture 与验收工具。

仓库聚合多个可独立构建/运行的部分，不是一个根目录统一启动的单体。存在代码、契约或 UI 页面不表示其生产路由已启用、外部依赖已部署或功能已验收。当前发布状态和阶段缺口以 [`docs/product-acceptance-matrix.md`](docs/product-acceptance-matrix.md) 与 [`docs/adr/bba-final-closeout-2026-10-31.md`](docs/adr/bba-final-closeout-2026-10-31.md) 为准；最新 B6 closeout 仍标记待独立审核、no-ship，release gate 不等于通过。

## 权威来源与冲突处理

1. 当前实现（尤其启动装配、HTTP handler、数据库迁移、权限检查）决定“代码现在做什么”。
2. [`contracts/openapi.yaml`](contracts/openapi.yaml) 是 HTTP/plugin wire contract 的规范来源；其 schemas 在 [`contracts/schemas/`](contracts/schemas/)，fixtures 在 [`contracts/examples/`](contracts/examples/)。契约定义不证明 API 已接线或可生产使用。
3. 已接受 ADR 记录稳定决策；若实现看起来违背 ADR，先核实并报告差异，不要静默绕开安全边界。主要决策见 [`docs/adr/p0-decisions-2026-10-24.md`](docs/adr/p0-decisions-2026-10-24.md) 与 [`docs/adr/bba-final-closeout-2026-10-31.md`](docs/adr/bba-final-closeout-2026-10-31.md)。
4. `docs/README.md`、`docs/product-release-runbook.md`、验收报告包含大量按阶段记录的历史快照。引用时保留日期和范围，不能把历史 PASS 当作本次验证。
5. `backend/internal/contract/types.gen.go`、`koishi/contracts/types.gen.ts` 和产品嵌入资源是生成/同步输出，不是手改源文件。

## 技术栈与依赖

- Go 1.26 模块 [`backend/go.mod`](backend/go.mod)，模块路径 `bba.local/taskengine`。直接运行依赖包括 `github.com/jackc/pgx/v5`、`github.com/oapi-codegen/nullable`、`github.com/oapi-codegen/runtime`。
- TypeScript 5.9 / Node.js 22 用于 OpenAPI 类型与 Koishi 子模块。根 [`package.json`](package.json) 只定义合同工具链和开发栈脚本，并非统一前端应用包；Node 最低版本声明为 22.12.0。根开发依赖锁定 Redocly CLI 2.71.0、openapi-typescript 7.13.0、TypeScript 5.9.3。
- `koishi/character-runtime` 固定 Koishi 4.18.11、ChatLuna 1.4.4、Character 0.0.235、OneBot adapter 6.9.5；具体依赖见其 `package.json`。源码为 `src/`，构建输出为 `dist/`。
- `koishi/analysis-collector` 固定 Koishi 4.18.11、ChatLuna 1.4.4、group-analysis 1.4.1、Puppeteer 3.9.0；依赖与脚本见该目录 `package.json`。源码为 `src/`，构建输出为 `lib/`。
- PostgreSQL 是 Task Engine 的执行与结果真相源；本地 fixture/验收脚本使用 PostgreSQL 16。第三方工具的获取、镜像和缓存策略见 `tools/prefetch.sh`、`tools/contract.sh`、`tools/kernel.sh`。

## 架构与目录

```text
contracts/                         OpenAPI、闭合 schema、正反例
backend/cmd/taskengine-server/     主 HTTP 服务、路由组合、resolver、内嵌 UI
backend/cmd/task-bridge/           loopback runtime/NapCat gateway 进程
backend/cmd/taskengine-migrate/    显式数据库初始化/升级/只读检查命令
backend/cmd/bba-operator/          操作员 CLI
backend/cmd/p0-*/                  阶段性 fixture / 受限 runner，不是主服务入口
backend/internal/taskengine/       PostgreSQL 领域逻辑、HTTP handlers、worker、migrations
backend/internal/contract/         OpenAPI 生成的 Go wire types
backend/internal/artex/            ARTEX REST client 与受控 goal writer
frontend/                           产品控制台源码与 NOTICE
backend/cmd/taskengine-server/product/ 同步后并由 go:embed 编译的产品资源
koishi/character-runtime/           按账号隔离的 Character/ChatLuna runtime 库
koishi/analysis-collector/           独立 collection-only Koishi App/库
koishi/contracts/                    openapi-typescript 生成的 TypeScript 类型
plugins/napcat-plugin-cleaner/      仓库内 cleaner 插件候选源码
patches/                             NapCat 上游 patch 候选；部署另需授权
 tools/                              contract、fixture、开发栈、产品验收脚本
 tests/contract/, tests/product/     Python 合同/产品验证
 tests/plugin/                       Node cleaner 候选离线行为测试
 evals/                               评测资产和历史结果；不是运行时数据
 docs/                                架构决策、能力矩阵、流程与阶段证据
```

上表中的 `backend/cmd/p0-*`、`evals/`、`tests/` 和 `tools/` 多数用于离线验证或阶段工作；不能仅凭命名视为常驻生产组件。`koishi/bba-character-runtime/` 当前目录为空，不要假定它提供包或 API。

## 核心模块与职责

### Task Engine

- [`backend/cmd/taskengine-server/main.go`](backend/cmd/taskengine-server/main.go)：读取 `BBA_TASKENGINE_DSN`、解析 bearer resolver、打开 PostgreSQL、校验 schema、装配 providers/routes；可选启动 worker/ARTEX graph worker；固定 loopback listener；处理信号并关闭。
- [`backend/cmd/taskengine-server/resolver.go`](backend/cmd/taskengine-server/resolver.go)：读取受保护 resolver JSON，使用 token SHA-256 匹配 operator/runtime 身份；将 API path 限定到相应 surface；校验固定 Host。operator 和 runtime credential 是不同身份。
- [`backend/cmd/taskengine-server/product.go`](backend/cmd/taskengine-server/product.go)、`console.go`：分别服务 `/product/` 和旧 `/` 控制台的嵌入静态资源；不提供通用文件服务器。产品 API 的实际挂载还会经过主程序与 `serverAPISurface`。
- [`backend/internal/taskengine/base.go`](backend/internal/taskengine/base.go)：PostgreSQL Store、事务、Actor/RuntimeActor 和授权基础。默认连接池 24，timezone UTC，statement timeout 15s、lock timeout 5s；不要在新 handler 中绕过 actor/workspace 授权。
- [`task.go`](backend/internal/taskengine/task.go)、`task_http.go`：创建、读取、分页、控制、对账及任务状态投影。HTTP body 做严格解码；创建和控制使用 workspace/principal/resource 范围内的幂等键。
- [`scheduler.go`](backend/internal/taskengine/scheduler.go)：`Reserve` 是创建新 dispatch attempt 的唯一事务入口；在锁内重新校验任务、baseline、approval、账号身份/状态/能力、预算、目标 claim 和 goal slot，不发外部网络请求。
- [`worker.go`](backend/internal/taskengine/worker.go)、`worker_steps.go`：有界、可重入的后台循环；推进固定候选导入、reserve/permit/bridge/outbox/inbox、恢复和只读对账。worker lease 使用 PostgreSQL session advisory lock。
- [`bridge.go`](backend/internal/taskengine/bridge.go)、`bridge_runtime_http.go`：持久化 bridge operation/outbox、permit 与 dispatcher 边界。有效执行链必须重新校验 payload digest、scope、account/runtime/owner epoch/fence 和许可；一次 permit 最多一次 adapter call。
- 其余领域模块包括 login、chat send permit、model/provider、agent/skill registry、content rule/review、work settings、summary、graph sync。按实际 handler 装配和 feature gate 接入；包内存在实现不代表 surface 已暴露。

### HTTP 与运行边界

- operator 入口采用 `Authorization: Bearer ...`，resolver 从受保护 JSON 映射到 workspace/principal/permission；运行时身份另带 runtime/account/owner epoch。浏览器控制台同源调用，不直接跨域调用 NapCat。
- 主服务的 path 组合在 `serverAPISurface`（`backend/cmd/taskengine-server/resolver.go`）。例如任务读路径与 reconcile 之外的部分写操作受 `-development-operations` 限制；内部 runtime、model、login、graph、report、settings 分别委派给不同 handler。新增 handler 要同时检查 surface、初始化装配和 OpenAPI。
- `task-bridge` 是另一个进程，只监听 loopback；读取 mode 0600 的 runtime/NapCat config，并通过 runtime credential digest 绑定 workspace/runtime/account/owner epoch。OneBot/WebUI 凭据不进浏览器。
- 主服务内部 Task Engine route、对外 Bridge route、OpenAPI 所描述的理想 wire surface 不完全等价；先核对真实 handler 接线和对应 feature flag。

### 前端与 Koishi

- `frontend/index.html`、`app.css`、`app.js` 是新产品 UI 源码。`tools/build-product.py` 把它们和 cleaner 工具清单复制到 `backend/cmd/taskengine-server/product/`；`product.go` 嵌入后由 Go 服务提供。编辑 UI 源文件后必须通过项目构建脚本同步；不要只改嵌入副本，也不要误把待接入页面当成后端功能。
- `koishi/character-runtime/src/` 暴露每账号 runtime manager、durable DB/ledger、ingress、backend client、GroupContext prompt provider、transport guard 和 `CharacterSendGate`。runtime/db/cache/persona/adapter scope 必须按账号唯一。发送 gate 接受可信入站 trigger、请求后端单次 text permit；多段/媒体不属于当前 writer contract，未知发送结果不得重放。
- `koishi/analysis-collector/src/` 是单独的 collection-only App：需要明确 selfId/platform/listener group；用窗口 owner/epoch 与持久窗口收集消息；可产出并发布 `Summary.v1`。`boundary.ts` 有意阻断 command/chat/model dispatch 和所有 outbound bot 方法。P1b/P1c 未完成，不要增加未经授权的分析/报告发送旁路。
- Collector Summary 经 backend 验 schema、account/group/window/version/expiry 后存于 PostgreSQL；Character 的 GroupContext 是不可信参考上下文，不是权限或任务授权。
- `plugins/napcat-plugin-cleaner/` 和 `tests/plugin/` 是仓库内候选/离线 fixture。能力矩阵 [`docs/capabilities/P0-02.md`](docs/capabilities/P0-02.md) 区分 frozen original 与候选；候选源码/测试通过不证明插件已部署、已加载或可真实写 QQ。

## 关键数据模型与数据流

### PostgreSQL 执行模型

迁移源码在 [`backend/internal/taskengine/migrations/`](backend/internal/taskengine/migrations/) 并通过 `go:embed` 编入 binary；当前 `SchemaHead()` 为 **24**。主要实体分布：

- 身份/授权/运行状态：`core.workspace`、`principal`、`account`、`runtime_credentials`、`runtime_heartbeat`、`approval`。
- 任务与执行审计：`core.task`、`task_account`、`target`、`task_target`、`baseline*`、`subtask`、`execution`、`goal_slot`、`target_claim`、`account_usage`、`dispatch_permit`、`account_action_lock`、`task_event`、`inbox`、`result`、`retry`、`idempotency`。
- 双进程 bridge：`bridge.gateway_operation`、`bridge.outbox`。执行请求和审计是持久事实；内存 dashboard/cache 不是恢复事实。
- 扩展业务：`core.chat_*`、`model_request`、`plan_proposal`/`plan_application`/`work_item*`、`group_summary`、`task_graph_*`/`graph_sync_*`、`login_session*`、`content_*`、`workspace_work_settings*`。

实体字段、约束及 schema 版本以 SQL migration 为准，不以历史 runbook 表格为准。迁移为显式事务、追加式；没有 down migration。当前代码接受从 schema 10 起到当前 head 的升级；不要自行增加兼容降级或绕开版本门。

### 主要数据流

1. **任务执行**：operator bearer → resolver/权限与 workspace scope → task handler/PostgreSQL → 固定 candidate 与可信 baseline → `Reserve` 原子创建 execution、slot/claim、budget 与 outbox → bridge 验证同一不可变 payload/permit → runtime gateway → OneBot/NapCat adapter → 有证据的 operation/observation → inbox reducer/result/task projection。每次状态转移都必须可追溯到 operation/execution/event/evidence。
2. **不确定外部副作用**：`submitted`、`pending`、`unknown` 都不是成功。dispatch 后超时或无权威结果时保留 slot、target claim、action lock/预算和审计记录，转人工/只读核对；禁止自动重试、释放名额或换账号。只有权威 no-effect 证据才可能满足重试门。
3. **proposal/work**：模型产生受 schema 与权限约束的提案；显式审批、digest、task/scope/authorization revision、期限和预算检查后，materialize 只创建持久 work item，不直接发送。worker 仍走既有 `Reserve → permit → bridge` 链；旧 `apply` 仍是 intent-only。
4. **分析与对话**：Koishi Collector 窗口 → `Summary.v1` → 后端 `group_summary` → read-only GroupContext → 按账号隔离的 Character prompt；prompt 内容不授予工具或 QQ 权限。入站 Character trigger → context/active speaker 检查 → 单次文本 permit → Task Bridge 最终 writer。
5. **ARTEX**：必须显式绑定 fact/goal roots；本地事务写 `core.graph_sync_outbox`，worker 在事务外调用 ARTEX，再记录 checkpoint。ARTEX 是外部图引用，不是 Task Engine 执行事实；ambiguous goal write 进入 `needs_reconcile`，通过读回核实而不是重发。

## 契约与生成代码

- `contracts/openapi.yaml` 是单一规范 HTTP/plugin contract，OpenAPI 3.0.3；扩展路径为本地 `contracts/schemas/*.json`，不允许远端、绝对、父目录穿越或 symlink escape 的 `$ref`。
- schema 对象通常 `additionalProperties: false`。ID 不可解析；QQ ID、owner epoch、fence、sequence 使用十进制字符串，避免 JavaScript 精度丢失。required nullable 字段要保留并明确输出 `null`。
- Go nullable 语义依赖 `oapi-codegen` 配置 `nullable-type: true` 与 `github.com/oapi-codegen/nullable`；不要用普通零值解码掩盖“缺失 / null / 值”的区别。
- 改 OpenAPI/schema 时先更新正反 fixture，跑 `bash tools/contract.sh check`；需生成时才跑 `bash tools/contract.sh generate`。生成命令会改写 Go/TS types，运行前确认这是任务要求并检查差异；永远不要手改 `types.gen.*`。
- `x-feature-enabled-in-p0` 是合同元数据，不是运行时启用证明。P0-12/P0-13 plugin、P0-17 model routes 等仍有独立 gate；当前合同中 chat permit 的 `true` 也不能单独证明生产 API/adapter 已部署。

## 配置与环境变量

仓库没有通用 `.env` loader。主服务与 CLI 的数据库连接统一从环境读取；凭据/模型/provider 参数放在受保护文件中。不得把 token、API key、DSN 密码写入文档、提交、日志、测试输出或浏览器持久存储。

| 配置 | 用途与边界 |
|---|---|
| `BBA_TASKENGINE_DSN` | 主服务、`task-bridge`、`taskengine-migrate` 的 PostgreSQL DSN；不接受为方便而加入命令行参数。绝不能把生产值传给 fixture runner。 |
| `BBA_DEV_STACK_NAME`、`BBA_DEV_STACK_DB_PORT`、`BBA_DEV_STACK_HTTP_PORT`、`BBA_DEV_STACK_STATE_DIR`、`BBA_DEV_STACK_PG_IMAGE`、`BBA_DEV_STACK_GO_IMAGE`、`BBA_DEV_STACK_WORKER_POLL`、`BBA_DEV_STACK_SKIP_BUILD` | `tools/dev-stack.sh` 的本地 fixture 覆盖项；默认 HTTP/DB 都是 loopback 非生产端口；脚本拒绝不属于该 fixture 的继承 DSN。 |
| `BBA_FIXTURE_ONLY`、`BBA_FIXTURE_DSN` | Go fixture/test runner 显式隔离标志和 unix-socket DSN；仅由 fixture harness 设置。 |
| `CONTRACT_ROOT` | contract smoke 中 generated Go fixture test 定位契约目录的路径。 |
| `BBA_TASK_BRIDGE_TOKEN` | 独立 `cmd/p0-20-execute` 到 Bridge 的 runtime token；具体阶段命令仍需显式配置和授权。 |
| `BBA_EXEC_CONFIRM` | P0-20 写操作配置文件 digest 确认值；写子命令还需受保护精确配置。 |

主服务常用 flags（以 `main.go` 为准）：`-listen`、`-origin`、必需的 `-resolver-file`、`-worker`、`-worker-id`、`-worker-poll`、`-worker-batch`、`-worker-recover`、`-development-operations`、`-proposal-execution`、`-login-provider-file`/`-login-fixture`、`-model-provider-file`/`-model-fixture`、`-model-registry-file`、`-artex-provider-file`、dashboard source/mapping。默认只监听 loopback；启动不自动迁移；worker transport 在主开发服务中是 fixture transport。

- resolver JSON：常规文件、mode 0600 或更严格、最大 64 KiB；只存 bearer token 的 SHA-256 和 scoped identity/permission，不存 token 明文；拒绝未知字段和 operator/runtime 混合身份。
- model provider JSON：mode 0600 或更严格、最大 64 KiB；API key 另由受保护 `api_key_file` 提供；timeout 1–120s、并发 1–32。endpoint 的私有 HTTP 需要精确显式批准，不能借此开放任意地址。
- ARTEX provider JSON：`base_url`、`token`、`allow_writes`、timeout/retry bounds；mode 0600 或更严格，无默认地址/token；写入必须显式允许。细节见 `backend/internal/artex/providerfile.go`。
- Task Bridge JSON：mode 0600 或更严格、最大 1 MiB；runtime/account/owner epoch 与 OneBot/WebUI 凭据由此显式绑定。详情见 `backend/cmd/task-bridge/main.go`。
- `bba-operator` 用 `-url`、`-token-file`；token file 要 mode 0600 或更严格。运维步骤、授权 prerequisites、drain/backup/recovery 见 `docs/product-release-runbook.md`，但需留意下述历史 schema 数字已过期。

## 开发、测试与部署

从仓库根运行。

```sh
# 确认根 Node 脚本
npm run

# 隔离本地 Task Engine 栈（需 Docker）
tools/dev-stack.sh start
tools/dev-stack.sh status
tools/dev-stack.sh stop

# 合同：验证，不生成文件
bash tools/contract.sh check

# Go 全包 race suite：一次性 Postgres fixture、网络隔离
bash tools/kernel.sh test-all

# 主服务隔离 smoke / schema smoke
bash tools/kernel.sh smoke
bash tools/kernel.sh schema

# 产品离线接受与独立验证（生成新的证据包；仅在任务要求时运行）
bash tools/product-acceptance.sh fixture --output <new-evidence-directory>
bash tools/product-acceptance.sh verify --package <evidence-directory>
bash tools/product-acceptance.sh release-check --package <evidence-directory>
```

`tools/kernel.sh` 使用临时 PostgreSQL 16、Unix socket、Docker `--network none`、`GOPROXY=off`、只读 module 模式，并清理自己创建的资源。Go 仓库测试需要它的 fixture 环境；不要直接运行 `go test ./...` 来绕过 harness。完整 `test-all` 含 race 检测，运行时间可能较长。

合同 `check` 运行 ref gate、Python schema/fixture validator、合同单测和 Redocly lint；工具缓存不存在时才运行显式联网的 `tools/contract.sh prefetch`。不要为本地验证默认运行 `generate`，因为它会改写生成代码和 build 产物。

Character 子模块脚本（Node 22；命令从对应 package 目录运行）：

```sh
cd koishi/character-runtime
npm run typecheck
npm test
```

`npm test` 有 `pretest` 会先 build 到 `dist/`，`npm run build` 的 `prebuild` 会运行 pinned ChatLuna patch。Collector 的 package scripts 有 `typecheck`、`build`、`test`、`check`；但依赖可重复安装/宿主环境情况需按当前 lockfile 与实际命令核验，不要把 `lib/` 中现有编译副本当作 `src/` 当前验证结果。

部署不是本指南提供的一键操作。二进制构建命令和 release 顺序见 `docs/product-release-runbook.md`；先用当前 `taskengine-migrate --check --expected-version N --expect-head H` 确认，再按授权流程备份、drain、显式迁移和单 writer 切换。主服务启动最低 schema 24，task-bridge 要求与当前 `SchemaHead()` 完全一致；服务不会自动迁移。未被明确授权时，不登录账号、不触发 QQ/ARTEX/model 外部写、不重启已有服务、不切换 writer。

## GitHub PR 开发工作流

每轮开发必须从最新远端主分支建立独立分支，完成源码、必要测试和文档后，执行项目规定的 lint、typecheck、test、build。推送分支并创建 PR，记录变更范围、实际验证结果与潜在风险。必须等待当前 PR 最新提交的必需 CI 全部通过，并满足代码审查和分支保护规则后才可合并；禁止直接修改主分支、跳过 CI、强制合并或忽略失败检查。合并后核实远端状态，清理开发分支并同步本地主分支。凭据、历史运行证据包和二进制不提交到公开仓库；本地验证不等于 GitHub CI，通过 PR 保留完整追溯记录。

## 编码与测试约定

- Go 使用标准格式化，符合 `gofmt`；领域逻辑放 `backend/internal/taskengine`，进程旗标/装配放 `backend/cmd/*`。HTTP handler 负责严格解析、鉴权和错误映射，事务内由 Store 完成持久状态改变。
- SQL migration 必须追加新版本、在事务中应用并带约束/审计语义；更新 `migrationTail`、`migrationHead`、server/schema gate、migration tests 和操作文档。不要重编号已发布 migration，不做隐式 startup migration。
- TypeScript 使用 strict TypeScript/ESM source 的现有模式，import 相对路径带 `.js`（NodeNext）。运行时状态/配置以对应 package `src/` 和 package scripts 为准，不直接编辑 `dist/`、`lib/`。
- 外部请求需显式超时、响应体/并发上限、redirect policy 和 secret-safe errors。写请求的 timeout/transport failure 按 unknown 处理；只对有证明可安全重放的读取做有界 retry。
- 所有 mutation 应有 scope/permission/version/idempotency 检查。先鉴权再返回幂等 replay；用 CAS/digest/fence/owner epoch 防陈旧写入；不要将模型输出、审批文本或外部 graph/context 当作授权事实。
- 测试应使用隔离 PostgreSQL、loopback fake HTTP 或假 adapter，断言对用户可见的状态转换、边界和“不发生的副作用”。不编辑永久测试/验证资产以迁就文档或检查结果，不把 fixture 成功表述为 live 能力。

## 稳定设计约束

- PostgreSQL 的持久 task/event/execution/bridge/inbox/outbox/result 是执行与恢复真相。进程内计数器、日志、UI 投影、生成 fixture 和 ARTEX graph projection 都不是它的替代品。
- 账号身份来自认证 runtime read-back；标签、端口、文件名、插件 enabled 配置不是身份或在线证据。Character 账号隔离采取独立 runtime/database/cache/persona/adapter scope。
- Writer ownership 是 account + action family 的单一有效 owner。fixture/fake 能证明本地边界，不证明目标 NapCat/QQ 运行时已经接线。
- 外部效果必须经过 reserve、single-use permit、bridge、runtime binding、observation/evidence。`confirmed` 必须有契约允许的权威证据；submitted/pending/unknown 不得提升为成功。
- Chat permit 与任务执行 permit 分离；Character 文本上下文、GroupContext、model output 不可获得 QQ 管理权限。
- ARTEX fact/goal graph、Summary、模型提案均为 scoped reference/planning input。ARTEX 不拥有 BBA task execution truth；模型提案 materialization 不发送外部动作。
- 新 UI 优先复用 `frontend/` 单一源码与既有 Go API；不要复制 ARTEX 全栈/Go backend，也不要引入第二任务执行器或第三套 UI 源码。
- 许可边界仍需发布审查：`frontend/NOTICE` 记录 ARTEX AGPL 根许可及 web MIT 文件来源；不能宣称整个仓库是 MIT。

## 已知问题与易踩坑

1. **release runbook 的 schema 数字过时**：`docs/product-release-runbook.md` 多处仍写 schema 21 / bridge schema 14；当前 migration `0015_work_settings.sql` 写入 24，`migrate.go` 的 `migrationHead` 是 24，主服务最低值也是 24。改数据库前必须以当前 binary 的 `SchemaHead()` 和 `taskengine-migrate --check` 为准，不要照抄旧命令示例。
2. **Bridge 启动日志 schema 标签过时**：`backend/cmd/task-bridge/main.go` 的 `validateGatewaySchema` 使用当前 `taskengine.SchemaHead()` 严格校验，但启动日志目前硬编码打印 `schema=22`。日志不是实际 schema 读数；以 gate、DB `core.schema_version` 和当前 binary 为准。
3. **合同比运行表面宽**：OpenAPI 收录阶段性 routes；`serverAPISurface`、handler 是否非 nil、flags、permission 和外部 provider config 决定是否可调用。`x-feature-enabled-in-p0` 仅是元数据。
4. **异步外部状态易误判**：HTTP `202`、插件 `{result:1}`、已有 group snapshot 或模型成功都不自动构成最终业务成功。Cleaner 缺少某些 membership/pending-request observation 能力；参考 `docs/capabilities/P0-02.md`，缺证据时 fail closed。
5. **阶段状态不是产品完成度**：`docs/product-acceptance-matrix.md` 的状态按行和证据范围定义。当前 B6 网络搜索最近作者 closeout仍需独立审核；完整产品目标、live model/QQ/ARTEX、生产部署和 release prerequisites 未因此通过。
6. **阶段目录和历史报告混杂**：P0-12/13/14、P0-20 等 runner 与永久服务入口不同。先检查命令实际读写范围和前置环境；不要因名字像可执行操作就运行 live runner。
7. **前端源码与嵌入副本可能漂移**：UI 改动后核验 `tools/build-product.py` 生成的资源一致并运行实际服务/fixture 流程；不要把生成副本单独提交为源头。
8. **脚本行为有副作用**：`dev-stack.sh reset` 会删除该实例的 fixture volume/state；`contract generate` 会覆盖 generated types；product `fixture` 会创建新的证据目录和隔离资源。运行前确认目标路径和影响范围。

## 关键文档索引

- [`docs/README.md`](docs/README.md)：契约工具链、合同范围与阶段证据入口。
- [`docs/adr/p0-decisions-2026-10-24.md`](docs/adr/p0-decisions-2026-10-24.md)：安全、retry、账号隔离、writer 和 audit 原则。
- [`docs/adr/bba-final-closeout-2026-10-31.md`](docs/adr/bba-final-closeout-2026-10-31.md)：Task Engine/ARTEX/Character/产品验收架构边界。
- [`docs/product-acceptance-matrix.md`](docs/product-acceptance-matrix.md)：当前分模块状态和阻塞项。
- [`docs/product-release-runbook.md`](docs/product-release-runbook.md)：部署/备份/迁移/drain/recovery；schema 示例需按当前 binary 复核。
- [`docs/capabilities/P0-02.md`](docs/capabilities/P0-02.md)：adapter 事实、候选与未知能力的证据边界。
- [`docs/b6-web-search-contract.md`](docs/b6-web-search-contract.md)：B6 搜索契约，不是验收证明。

## 2026-11-01 QQ 产品目标与当前接管结论

### 权威输入

本产品实现主体是 `/run/media/sun/6F53FE6DFAA8CECF/QQ群项目/BBA`；QQ 自动化架构基线是 `/run/media/sun/6F53FE6DFAA8CECF/BBA/qq-automation-design`；ARTEX 只作为控制台的信息架构与交互参考，不能把 ARTEX 的渗透测试领域模型或调度器直接当作 QQ 业务实现。历史交接材料位于 `/run/media/sun/6F53FE6DFAA8CECF/QQ群项目/astra-handoffs`，只能作为带日期的声明和需求来源，必须回到当前源码、挂载路由、数据库迁移和新测试核验。

用户已确认的业务决策：

- 对话采用双视图：操作员与 Agent 线程，以及按 QQ 账号、群聊/私聊隔离的 QQ 收件箱。操作员发给 Agent 的内容不能进入 QQ 出站；QQ 内容也不能被误当作 Agent 指令。初期 QQ 出站允许做到草稿和规则/许可回执，真实发送另行授权。
- 只要识别出明确意向，就计入“引流成功客户数”。仍须保存客户 QQ 标识、来源、意向证据、规则版本、识别时间和归属关系；模型自述不能替代可追溯证据。当前口径不再要求先建立好友关系才计数，但好友关系若可观察仍应单独记录，不能与意向数混为一项。
- 本轮只允许代码、迁移、fixture 和离线验收；不登录、不发送、不加群、不加好友、不调用付费模型、不改 6300、ARTEX、真实 NapCat 运行目录或生产数据库。

### 产品范围与验收口径

功能主目录为仪表盘、对话、任务、计划任务、资产；系统目录保留 LLM、Agent、MCP、Skill、工具、通知推送、拦截规则、审批记录、日志和系统配置。具体逐项状态、任务编号和证据要求见 [`docs/product-acceptance-matrix.md`](docs/product-acceptance-matrix.md) 的“QQ 产品目标增量”章节。

- 仪表盘沿用 ARTEX 的布局和未明确删除的区域；“确认发现”改为 QQ 状态（在线、离线、异常、总数），资产节点显示明确意向客户数；删除流量交互、活动流、发现和流量状态码。未知或过期状态不得伪装成离线，客户数不得使用演示数字或缓存猜测。
- 任务和计划任务复用现有 PostgreSQL Task Engine、版本、授权、幂等、暂停/恢复/取消、unknown 和对账语义；任务类型覆盖加群、群分析、聊天、客户加我或我加客户。提交、等待和 unknown 不计成功，unknown 不自动重发；能力不存在时返回明确 unsupported。
- 资产表示有明确意向的客户，不是全体群成员。客户去重、证据撤销、规则版本和人工修正都必须可审计。
- LLM 保留模型接入和密钥保护；Agent 保留配置管理但执行域改为 QQ 工具；MCP 必须区分 ChatLuna、Character、group-analysis 的插件/连接管理与真正 MCP endpoint；Skill 是版本化的中文运营流程，不授予工具权限；工具目录来自 NapCat 能力，但目录、schema、运行 capability 和调用授权必须分开；通知、日志和审批按 QQ 事件、出站内容、工具和任务结果接线；拦截规则在最终出站 payload 统一检查关键词/模式，不能只拦截 UI 按钮；系统配置至少保留全局代理、网络搜索和 Work Agent 数。

### 当前源码事实（截至本次独立复核）

- 产品导航有 15 个入口。仪表盘、任务、工具、LLM、Agent、Skill、拦截规则、审批记录和系统配置已有不同程度的 API/消费者；对话、计划任务、资产、MCP、通知推送和日志仍不能因菜单存在而判定完成。
- 当前产品前端把对话、计划任务、资产、MCP、通知推送和日志明确渲染为待接入；工具页读取嵌入的 `product/tools.json`，是只读注册目录，不是已验证的 NapCat 调用层；Skill/Agent 是配置与版本管理，不等于 QQ 工具执行。
- 当前系统配置已有 Work Agent 槽位、全局代理和网络搜索的前端路径；其运行时作用域和生效时机必须按现有测试保留。`main.go` 在未提供 `-model-registry-file` 时仍令 `registry` 为 nil，却无条件构造代理和搜索设置 handler 的方法值；完整 handler 当前显式检查 nil receiver，并返回配置/存储不可用响应，未证实请求级 panic。现有证据缺少覆盖主装配的未配置注册表回归测试，后续应验证实际组合路由的 HTTP 行为并保留 fail-closed，不应将其描述为已确认崩溃。
- 当前内部聊天路由是 runtime 侧的群摘要、上下文、触发器和 chat-send-permit；它不能替代操作员 Agent 线程和 QQ 收件箱的 operator API。新增功能必须追踪“事件/消息 → 持久化 → API → 前端消费者 → 草稿/规则/许可回执”，不能只增加静态页面。
- 外部 Git 安全网为 `/home/sun/.hermes/git/bba.git`，基线提交 `b052484`；BBA 工作树没有项目内 `.git`。任何变更都要记录相对该安全网的文件清单和 SHA-256，不能用历史 sol 报告代替当前源码证据。

### 实施顺序与禁止越界

第一轮执行任务见 [`docs/sol-bba-qq-b7-dual-dialogue-notify-20261101.md`](docs/sol-bba-qq-b7-dual-dialogue-notify-20261101.md)。先完成一个可独立验收的双对话/通知切片和基础阻断缺陷修复，再进入任务全类型、计划任务和资产 CRM；MCP、工具运行时、日志等配置页按真实消费者逐项接线。不要同时创建第二个任务内核、第二套模型通路或绕过已有 bridge/permit/content gate 的快捷发送路径。

实施者可以在隔离 fixture 中新增源码、迁移、契约、前端和测试；不得执行真实 QQ 动作或修改生产运行目录。每一批必须提交：实际修改文件、迁移与回滚边界、逐条测试命令和退出码、原始日志/截图、fixture 与真实外部范围区分、未覆盖项。作者自测、历史报告、菜单存在、schema 示例、静态注册表和“批准”都不能单独证明产品功能或发布就绪。
