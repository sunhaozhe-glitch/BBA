# OMP 第二轮续建：停止扩散，打通开发环境完整业务链

## 目标和范围

项目 `/run/media/sun/6F53FE6DFAA8CECF/QQ群项目/BBA`。
继续遵循 `docs/astra-omp-continuation-prompt.md` 的最终产品要求。本文件针对当前已完成工作调整实施优先级，不取消登录、双图、长期任务与最终 UI。

本轮不是重复盘点，也不是再写一份计划。先读取最新矩阵和实际源码，保留已完成代码，随后连续实现以下交付。普通代码、测试、隔离数据库和本地开发 UI 不应被“尚无生产授权”阻塞。真实 QQ 写、生产服务变更、真实数据库迁移和 writer 切换仍需具体有效授权。网络安装依赖不是 QQ 业务操作：先使用本地缓存，缺包则尝试项目锁定的公开注册表依赖下载（不上传源码、密钥、群数据、不改全局配置、不升级版本）；如果工具确实要求批准或网络不可达，记录具体错误并一次性提出所需权限，不再只写“需要授权”就结束整个任务。

## 本轮开始时的已核实基线

Astra 实际执行：
- `bash tools/kernel.sh test-all`：退出 0，全后端 race suite 通过。
- Node 22、network-none、项目只读挂载下 Character `npm test`：23/23 通过，包含真实 Character prompt 的 GroupContext 测试。ChatLuna teardown TypeError 仍出现但测试退出 0；保留为已知依赖问题，不声称日志无错误。
- 同环境 collector `npm run typecheck`：TS2688，找不到 node 类型定义。依赖可重复安装仍未交付，不应永久沿用历史 PASS。

源码增量认可并保留：
- `observations.go:287` 的父任务归并及终态事件；但目前依赖 observation 触发，没有独立 deadline sweeper。
- `character-runtime/src/group-context.ts` 已接实际 prompt；但 provider 只有测试实例，没有 collector/backend 生产消费者。
- collector 有 owner/window/L0 和 `saveSummary/listSummaries` 校验与持久化。
- `graph.go`、schema14 和控制台已有 BBA 自有投影，不是 ARTEX 集成或 Planner。
- 嵌入式同源 Web UI 已有账号/任务/图的只读骨架；继续沿用，不新建第二个前端。

必须修正当前口径：`saveSummary(windowId, summary)` 接受调用者传入的摘要；检索到的调用来自测试。它是摘要接收/保存能力，不等于已实现真实摘要生成器。验收矩阵应将“生成”“保存”“消费”拆行。原矩阵 Character 的21项与当前23项也要按对应版本更新，不能混用。

## 交付 1：可重复构建与可用的开发启动命令

- 在固定 Node22 下解决 collector 的本地 `file:../character-runtime` 依赖、@types/node、group-analysis及其锁文件；清理 package.json 重复 engines 键，保留锁定版本。
- 从干净、用户可写的隔离目录安装并从当前 src 构建两个包，再运行 collector/Character typecheck、测试。不得只测试旧 lib，不用 NODE_PATH 拼接作为最终安装方案。
- 已有启动指南存在确定的产物路径错误：`docs/taskengine-development-console.md:17–31` 在 `docker run --rm` 的容器内编译到 `/tmp/bba-taskengine-server`，没有将该输出目录挂到宿主，容器结束后却要求宿主执行同一路径。修正为挂载的构建输出路径，或在同一受管容器运行；真实执行构建并证明最终可执行文件存在，再启动隔离开发服务验证健康与首页。
- 新增一个开发命令启动隔离 DB、迁移、服务和必要 worker，明确所有fixture服务与真实服务不同端口/数据目录，支持停止与清理。默认无真实 QQ adapter、无生产 DSN。

## 交付 2：常驻确定性 worker + 定时收口

补齐尚未交付的 worker，不循环调用 p0-20 单次 CLI：
- 生命周期、租约/抢占、分页展开、claim/Reserve、受门禁 dispatch、outbox/inbox消费、对账请求消费、deadline扫尾及退出排空。
- fixture transport 默认启用；真实adapter只能显式启用并满足现有许可条件。
- 复用 `reconcileTaskProgress`，提供有界批量的事务入口，保证没有新 observation 的过期任务也能在调度周期内结项；pending/unknown 继续保留外部未决记录和额度，不因任务deadline允许重发。
- 测试：静默截止、无候选任务、完成目标、重复tick、并发worker、崩溃恢复、旧owner恢复、取消后晚到事件。以实际父任务状态与事件数量断言，不只检查函数返回成功。
- worker恢复不产生第二次未知QQ动作；任务终态不意味着外部未决消失。

## 交付 3：真正的摘要生成到 Character 的端到端链

链路必须是：
合成/获准群事件 → 实际 collector 持久窗口 → 分析 job → Summary生成器 → 校验/保存 → 实际后端 Summary读取 → GroupContext provider → 实际 Character模型请求 → 现有permit/transport → outcome。

具体要求：
1. 实现分析 job 和明确的 SummaryProducer 接口，至少一个可执行生产适配器；确定性统计只能称统计摘要，模型话题摘要需要实际模型接口。离线测试可用标明fixture的模型返回，但不得直接调用 saveSummary 塞一个固定摘要后宣称生成链通过。
2. 建立后端摘要持久/索引/读取契约，传递账号/群/窗口、摘要ID与版本、来源引用、隐私等级和有效期；鉴权和跨账号范围验证在后端完成。
3. 实现真正的 GroupContextProvider，通过该契约读取已存摘要，结合现有后端发言人/冷却/预算形成上下文。不能用模型读取的冷却数替代代码发送门。
4. 摘要上下文标为不可信引用数据；生成时间不能晚于允许时钟偏差，过期和引用失效应拒绝或降级，不携带上一群或上一轮残留。
5. 明确保留期清理：原文删除后，摘要/图引用失效策略与过期过滤生效，避免只在写入时校验保留期。
6. 真实跨模块离线集成必须使用当前 collector、实际后端、实际 Character。只在模型和QQ外部边界用fixture，捕获模型请求证明包含本次摘要ID/来源/版本，换群与换账号不串。

给出两个不同时间窗的消息，使第二次摘要内容变化，并证明 Character收到第二版。产物必须可从控制台点开，不接受只有测试日志。

## 交付 4：在现有 Web UI 上完成“可操作开发闭环”

当前只读 ADR 是阶段性范围，不是最终产品永久禁写。原任务已要求开发创建/暂停/恢复/取消等功能，不要每轮以“浏览器写操作需要生产授权”跳过它。

- 保留默认只读部署；提供显式开发操作模式，逐路由开放创建、范围配置、暂停/恢复/取消、对账以及后续提案批准。鉴权、对象归属、幂等、expected_version、审计全部保留，不整体移除allowlist。
- 同源开发浏览器以隔离 resolver 和合成数据验证完整登录鉴权 → 创建任务 → worker推进 → 查看摘要 → 查看Character生成/outcome → 查看图 → 暂停/取消/对账收据。浏览器认证是否通过必须实际观察，不能仅截图未登录页。
- token 只存内存；fixture凭证经受保护本地文件/工具机制注入测试，不写进源码或交付日志。
- UI展示 confirmed/pending/unknown、分析等待/失败、模型不可用和图同步状态；409冲突与刷新不重放写动作。
- 扫码登录模块继续按原任务实现适配器与fixture UI，不触发真实登录/二维码刷新也能完成开发验收；真实登录另列验收门。

## 交付 5：明确 BBA 图与 ARTEX 的关系，完成最小提案纵切

已有 BBA 图投影继续保留，但不能用它取代用户要求的 ARTEX 双图。当前图根 `core.task_graph_root` 与原设计 `task_graph_roots` 命名/职责不同，先写映射 ADR；不要为统一名字删现有表或建第二套任务真相。

- 只读检查本地 ARTEX 的版本、许可证、API/MCP定义。没有live接口授权时可实现接口适配与contract fixture，不虚构已连通。
- 明确 Task Engine账本、BBA派生投影、ARTEX事实/任务图之间的唯一所有者、映射、版本、失效、同步checkpoint和幂等边界。
- 当前图 GET 会调用 `projectTaskGraphTx`，锁父任务并写图根、快照。它不发送QQ，但不是数据库纯读取。将投影构建移到事件消费/worker，GET读取现有投影并显示滞后状态；或用ADR明确受控lazy projection及性能/权限边界，不能称为数据库零写。
- 给现有模型接口增加受限步骤提案与proposal持久生命周期、审批和应用器；不要直接把现有clarification校验放开。步骤种类由本轮业务模板固定，校验task version/scope/预算，应用幂等。
- 以“完成观察后建议下一次受控参与”为最小例子，验证新摘要→提案变化→明确批准→任务内核应用→fixture执行→结果回流。没有新证据不无限重规划，图侧不直接调用QQ。

## 本轮结束条件与报告

优先完成交付1–4的跨模块可运行闭环，再完成交付5；不要先扩展更多图字段和视觉效果。若有单一依赖阻塞，不停止可独立完成的worker/API/UI。

交付一个可运行验收命令与一份自包含证据目录，包含：源文件指纹、依赖锁、测试命令和退出码、跨模块轨迹ID、浏览器认证后结果、开发构建/启动/停止验证。没有Git则用源指纹，不造commit。保存证据在项目 evals/results 下，不仅引用容器临时路径。

更新验收矩阵，逐项分开“源码存在 / 已接消费者 / fixture通过 / 真实受控通过 / 发布”。保持真实QQ与生产不变。报告剩余真实操作时一次性列出所需账号、群、动作、预算、时限和授权，不把普通依赖安装和离线代码实施混在其中。
