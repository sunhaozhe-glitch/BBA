# B7 阶段 1：principal 持久请求作用域

状态：PARTIAL / BLOCKED / NO_SHIP；NOT_RELEASED。仅本阶段，禁止自动进入阶段 2。

## 基线与现场

仓库根 `/run/media/sun/6F53FE6DFAA8CECF/QQ群项目/BBA`。原主工作区分支 `docs/artex-web-ui-sync`，HEAD `4ef04d2b03f20225d864badf31c384ce1c56f0a5`，未跟踪 `controller/` 保留。原组合工作区 HEAD `8f916059ed3261770b31f7ed2bdf196b3e1ae75e`；唯一未提交改动是在 operator_conversation.go 定义未调用的 `(principal,key)` JSON 摘要辅助函数。文件摘要和完整 patch 保存于证据根 `original-helper.sha256`、`original.log`、`original-helper.go`。它在本轮开始前已存在，来源不能归因于本轮；不修改、不恢复、不删除原文件。新隔离工作区按确认后的完整账本身份另写 helper。

最新 origin/main=`5e44e86b175de2563b25ef71b4aace64e8f8a9ff`，remote HEAD=main；AGENTS.md 与 main 字节摘要一致。工作分支 `fix/b7-principal-event-scope` 从最新 main 建立，然后对已核实祖先链 fast-forward。PR 使用新增固定快照 `stage/b7-principal-dependencies-20261103` 为 base，base SHA=8f91605，不修改既有 PR head。

依赖图（箭头为后继）：

    main 5e44e86 → #8 579a20c → #9 31489e6 ──┐
                       └──→ #10 770e776 ─────┴→ e67aec5 → 1ed2461
    → 3b0c3f8 → cfc08c8 → d20e355 → 8f91605 → 本阶段分支
    #10 770e776 → 5df5219（独立轮换分支，3b0c3f8 是组合中的对应修复）

真实父提交：8f91605←d20e355；d20e355←cfc08c8；cfc08c8←3b0c3f8；5df5219←770e776。#8/#9/#10 当前均 OPEN/base main，各 SHA 见上。组合并非 main；该快照分支不是 main 合并批准，后续依赖收敛仍需单独处理。审查应比较快照 base 与本阶段 head。

## 身份、事务与最小修复

resolver 将 Bearer token 的服务端 SHA256 记录解析为 workspace/principal/permission，principal 不是请求 body 或显示名称。handler 在每次请求（包含 replay）检查鉴权、权限、principal 存在性，拒绝 runtime 走操作员路径；Draft 另先检查 actor account scope。事务仍锁 principal；首次创建/追加对 conversation 加 FOR UPDATE 锁。Agent 必须是 agent/agent、account NULL；Draft 在 workspace 下确认 account，再通过 workspace/account/kind/external conversation 定位 QQ 会话。scope 未放宽，未变更权限接口。

持久账本 identity 维持 `(workspace,principal,method,path,key)`。Agent 的会话在 path；Draft 的 account/kind/external conversation 是 request digest 的组成部分，同 principal 同 path/key 改资源必须 409，不能重新解释为独立请求。account/conversation 通过持久 conversation 行及消息唯一索引进一步隔离。Agent 与 Draft 使用同一 helper，不引入第二套账本。

operator_message 的主键 id 是消息身份；event_id 是来源事件/请求去重键。QQ inbound 的 event_id 仍为 runtime 来源事件 ID；仅操作员 Agent/Draft 请求改为：

    opreq:v1: + SHA256(JSON(["operator-request-v1",workspace,principal,method,path,key]))

JSON 数组避免分隔符拼接歧义，版本域隔离编码，ASCII 摘要符合 core.id 128 字符上限。相同正文不决定请求身份。不同 principal 同会话同 key 是两条独立消息。Agent 通知 event_key 同步使用 `agent-message:` 加新请求 event_id；Draft 通知原本绑定独立 draft ID，无需更改。消息、Draft、通知、账本在同一事务提交；不捕获数据库冲突当成功、不删除约束、不改内存状态。

错误继续使用契约现有 409 IDEMPOTENCY_CONFLICT 或通用冲突 envelope，不读取其他 principal 的账本，不回显正文或资源结果。replay 先经过当前 resolver、permission 和 scope 检查。真实 HTTP 探针在删除 Alice resolver 凭证并重启后，旧请求返回 401；原 runtime 撤销回归保持。

## Schema、历史兼容和消费者

main tail=0016/schema=25；依赖组合 tail=0017/schema=26；本阶段不新增 migration。消息索引仍 `(workspace_id,conversation_id,event_id) WHERE event_id<>''`，seq 唯一约束、append-only trigger、Draft message 唯一约束均不变；通知使用依赖 migration 0017 的 scoped 唯一索引。SchemaHead()=26；server minimum=24，Bridge exact head=26，不能机械改成同一门限。

不需要 migration：列类型、约束和读取格式均容纳新值，旧行保持不可变；replay 先读原账本，未重新生成旧 event_id。实际旧 binary 创建原始 event_id=legacy-key 的 Agent/Draft 后，新 binary 精确重放，数据库副作用不增。历史消息正常读路径不解析 operator event_id；仓库搜索的生产消费者只有 handler insert、inbox dedup 和通知投影，前端读消息不暴露此内部字段。外部未知 SQL 消费者不可枚举，不保证其格式依赖。

本阶段在已升级 schema26 上可读旧记录，不宣称旧/新 binary 完整滚动兼容。依赖 0017 已改变 ON CONFLICT arbiter，旧 schema25 binary 与 schema26 SQL 混跑仍需协调停写升级。回滚旧 binary 会恢复跨 principal 同 key 缺陷，须停止该写入用例；不能用回滚证明缺陷已消失。无隐式 startup migration；未触碰生产数据库或真实 writer。

OpenAPI 当前未列出 operator conversation/agent/draft endpoints，现有 wire 行为及固定 201 replay 由 handler/永久回归规定。这是既有合同覆盖缺口，本阶段不顺手扩充 API schema。`bash tools/contract.sh check` 仍执行；不能将其绿色说成 operator API schema 完整覆盖。

## 先红后绿与实际验证

原始证据根（不提交历史证据、binary、缓存或凭据）：

    .work/b7-stage1-20261103-162442/

每轮命令时间、exit code、SHA 在同名 JSON，原始日志在同名 log；实际执行者均 Hermes。此报告记录至代码提交 `303513e`；最终文档 head 的重新运行记录由 `FINAL-*.json` 精确绑定，不把下列旧 SHA 结果移植到最终 head。

| 证据 | 命令 | exit / 适用 SHA | 结果 |
|---|---|---|---|
| RED.log/json | bash tools/kernel.sh test -run TestHermesPrincipalRequestIdentity -v | 1 / 8f91605 | HARNESS_ERROR：容器无法解析宿主 cache symlink，不是产品 RED；复制 BBA 内 cache 后重试 |
| RED-defect.log/json | 同上 | 1 / 8f91605 | Agent 与 Draft 第二 principal 均 409 CONSTRAINT_CONFLICT |
| RED-permanent.log/json | bash tools/kernel.sh test -run TestPrincipalEventScopeLifecycle -v | 1 / 3155faf | 新永久回归 Agent 与 Draft 同样失败 |
| GREEN-target.log/json | bash tools/kernel.sh test -run 'Test(HermesPrincipalRequestIdentity\|PrincipalEventScopeLifecycle)' -v | 0 / 3155faf+修复 diff 摘要 | 两路径精确重放、body conflict、跨 principal、并发、计数/授权通过；未提交树有 diff 摘要 |
| HTTP-1.log/json | python3 tools/b7-principal-scope.py --output ../http-1 --legacy-source ../../b7-audit-20261103-150312/combined | 1 / c1a0f7a | fixture socket 路径过长，PostgreSQL 未启动；已清理，不计产品失败 |
| HTTP-2.log/json | python3 tools/b7-principal-scope.py --output ../http-2 --legacy-source ../../b7-audit-20261103-150312/combined | 0 / e17950f | 真 binary/真 HTTP；旧 binary 历史重放、并发、响应丢失、进程重启、token 撤销通过 |
| test-all-1.log/json | bash tools/kernel.sh test-all | 0 / e17950f | 全 Go race suite，包括原账号/runtime、通知、并发/重启回归 |
| contract-1.log/json | bash tools/contract.sh check | 2 / e17950f | 65 Python 测试通过后缺 node_modules，未通过完整检查 |
| contract-2.log/json | bash tools/contract.sh check | 0 / e17950f | 复用 BBA 根已存在依赖；65 测试与 OpenAPI lint 通过 |

HTTP 探针所有内部构建、migration SQL、数据库核对和清理命令逐条在 http-N/commands.json、command-N.log，含起止时间/真实 exit。HTTP result.json 保存 exact response、build SHA256、schema head；版本 Go 容器 go1.26.10、宿主 Node v26.12.1、Python 3.14.9、Docker 29.1.3，Go 宿主未安装。HTTP-2 最终计数 messages=12/notifications=12/drafts=6/ledger=12，chat permit=0/dispatch permit=0/tasks=0/model requests=0。没有 writer/provider 配置或真实外部服务调用；HTTP probe 没有注入响应。临时 runtime token/DSN 均为新建合成 fixture 资源。

## 验收维度与未覆盖风险

| 能力 | 实现 | 接线 | 验证身份 | 环境 | 结论 | 发布 |
|---|---|---|---|---|---|---|
| Agent 跨 principal 同 key、精确重放、conflict、并发 | IMPLEMENTED | WIRED_TO_CONSUMER | AUTHOR_SELF_TEST | ISOLATED_FIXTURE | PASS（见绑定代码版本证据） | NOT_RELEASED |
| Draft 同上及零 permit | IMPLEMENTED | WIRED_TO_CONSUMER | AUTHOR_SELF_TEST | ISOLATED_FIXTURE | PASS（见绑定代码版本证据） | NOT_RELEASED |
| 响应丢失、进程重启、历史格式 replay | IMPLEMENTED | WIRED_TO_CONSUMER | AUTHOR_SELF_TEST | ISOLATED_FIXTURE | PASS（真实 HTTP） | NOT_RELEASED |
| 第三方独立审查/独立执行 | IMPLEMENTED | WIRED_TO_CONSUMER | NOT_REVIEWED | ISOLATED_FIXTURE | BLOCKED | NOT_RELEASED |
| 主分支、产品 live、真实 QQ/ARTEX、浏览器本阶段 | PARTIAL | PARTIAL | NOT_REVIEWED | PRODUCTION | NOT_RUN | NOT_RELEASED |

MOP 实际施工任务数=0。现有 default/plan/slow 指向外部付费路由；未建立本地且无外部回退的可验证路由，未发送 prompt。实际实现/测试执行者 Hermes；Hermes 审读了完整生产 diff、事务、索引、resolver 和真实 HTTP/DB 状态，但作为作者，这些仍是 AUTHOR_SELF_TEST。未调用其他模型。第三方独审 BLOCKED，不能称独立验收。

其他风险：SHA256 理论碰撞；外部 SQL 消费者不可枚举；未测试断电/PostgreSQL durability 故障；跨 workspace/account 的完整真实 HTTP 组合仍未穷尽（原永久隔离测试保留）；operator OpenAPI 缺口；base 快照含未合并候选，不能直接冒充 main-ready。未改 frontend/embed、license、migration 或治理配置。

PR、最终 HEAD CI 和最终测试/cleanup 以本阶段交付记录更新。未满足独立复审等停止条件时仍 NO_SHIP，不宣布阶段完成，不合并不发布。fixture 归属按唯一容器 label 核对后清理，http-N/cleanup.json 读回容器不存在/服务进程退出；harness 自带隔离 DB 清理。原工作区与原 helper 保持不变。
