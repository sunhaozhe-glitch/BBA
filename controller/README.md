# Controller 第一阶段

独立 Python 研发控制器，入口为 `python -m bba_controller`。不连接 QQ、Task Engine、PostgreSQL、生产服务或模型。默认 OBSERVE，仅允许查询持久任务；经过启动侧明确授权的隔离 fixture 仓库可以执行模拟 ACP 生命周期。真实模型 prompt 在 Supervisor 层始终拒绝。

从仓库根运行（已有依赖，无需安装）：

```sh
PYTHONPATH=controller/src python3 -m unittest discover -s controller/tests -v
PYTHONPATH=controller/src python3 -m bba_controller --help
```

发行包依赖和构建依赖固定在 `pyproject.toml`；本轮没有执行安装。CI 单独安装固定版本，运行同一 unittest 命令。

## 启动和权限

只读 BBA 示例（state-dir 必须在受保护仓库之外，必须是当前用户受控目录）：

```sh
PYTHONPATH=controller/src python3 -m bba_controller \
  --repository /absolute/BBA --state-dir /absolute/private-controller-state
```

隔离 fixture 示例（仓库须是专门创建的可丢弃 Git 仓库；origin 必须是本地绝对路径 bare 仓库，有明确 main 分支）：

```sh
PYTHONPATH=controller/src python3 -m bba_controller \
  --repository /absolute/fixture --fixture-repository /absolute/fixture \
  --authorize-fixture --state-dir /absolute/fixture-controller-state --slots 2
```

不要给 BBA 真实仓库传 `--authorize-fixture`。fixture 是启动操作员的权限决定，不能由 MCP 请求提供。生产 DEVELOPMENT 必须同时指定 `--mode DEVELOPMENT --authorize-development`，但第一阶段仍不开放真实工作树写入和真实模型。此标记不授予 fixture 权限。stdio 是本地受信进程边界，不提供远程 HTTP 或 bearer 认证；不要直接暴露给不可信用户。

`--repository` 精确核对 Git 顶层，状态库固定仓库路径摘要，重启切换仓库被拒绝。单进程 owner flock 防止第二个 Controller 错把活跃工作恢复为 UNKNOWN。SQLite claim 仍在事务内实施全局配额，默认 1、最大 2。存储目录不得被不可信进程同时修改；静态路径和祖先 symlink 均拒绝，不提供抵抗恶意本地 root/并发文件替换的沙箱。

## 七工具合同

权威代码 `src/bba_controller/contracts.py`，导出 `schemas/bba_task_<name>.input.json` 和 `.output.json`；测试逐项核对导出与 MCP 发现一致。所有对象拒绝未知参数。统一成功为 `{ok:true,data:{...}}`，错误为 `{ok:false,error:{code,message}}`，MCP 同时设置 isError。

| 工具 | 输入 | 成功 data |
|---|---|---|
| bba_task_submit | kind=simulation、base_sha；可选 write_set、dependencies、behavior、rework_failures、model_policy_ref | task |
| bba_task_status | task_id | task |
| bba_task_wait | task_id、timeout_ms 0..1000 | task、timed_out |
| bba_task_cancel | task_id | task |
| bba_task_resume | task_id，可选 simulation_receipt | task、restored |
| bba_task_list | limit 1..50、offset | tasks、next_cursor |
| bba_task_evidence | task_id、after 事件序号、limit 1..100 | task、events、artifacts、session、next_cursor、model |

任务 ID 为 32 位小写十六进制，base SHA 为 40 位小写十六进制。工具不接受 repo、prompt、argv、mode、模型或授权参数。write_set 只能是规范相对路径；拒绝绝对路径、父目录、.git、symlink、控制字符和 shell 特殊字符。未知写集 null/省略阻止所有重叠执行；空数组代表明确无写集。依赖必须已经存在且全部 SUCCEEDED 才可领取。只有相同任务 ID 才能访问相应记录；本地进程授权覆盖其整个状态库，不实现多租户认证。

错误码：INVALID_ARGUMENT（schema）、PERMISSION_DENIED（启动权限/路径）、NOT_FOUND（任务/工具）、CONFLICT（状态/CAS/配额/owner）、LIMIT_EXCEEDED（请求或结果字节/并发）、UNKNOWN（传输不确定）、INTERNAL_ERROR（安全通用错误，不回显原始 payload）。没有自动 retry。

## 状态机和恢复

```text
QUEUED -> RUNNING -> SUCCEEDED / FAILED / CANCELLED / UNKNOWN / PAUSED
QUEUED -> CANCELLED / PAUSED
PAUSED -> QUEUED / CANCELLED  （QUEUED 仅内部合法状态转换，MCP 不自动重跑）
UNKNOWN -> authoritative reconcile -> SUCCEEDED / FAILED / CANCELLED
```

RUNNING 重启恢复为 UNKNOWN，保留 scheduler slot 和路径冲突屏障。超时、断线、协议错误、异常退出及无回执取消也进入 UNKNOWN。cancel 只是 ACP notification；只有 pending prompt 返回 stopReason=cancelled 才确认 CANCELLED。queued/paused 可以本地确认取消。已取消领取但未开始的 worker 会重验状态，不再发 prompt。

resume 默认只启动模拟 peer 并对既有 session load/resume，记录结果，不发送 prompt、不释放 slot。仅模拟任务允许显式 `simulation_receipt` 人工核对终态；该参数永不用于真实执行。真实权威 reconciliation 适配器没有开放。UNKNOWN 不可自动重新入队。

每任务最多三条 rework 记录，含 round/reason/change/result，三次失败进入 PAUSED；UNKNOWN 的 rework 只记录 pause_required 并继续保留 slot。模拟提交的 rework_failures 只用于可重复测试验收记录，不宣称执行了实际代码修复。不会自动 commit、push、PR、部署或任何外部 write。

## ACP、Git 与 evidence

Supervisor 使用独立 stdin/stdout/stderr，NDJSON RPC ID 关联、乱序响应、通知逐条追加（合法相同通知不摘要去重），反向 permission 总是取消、fs/terminal 等请求拒绝并记录。pending 在断连中全部 UNKNOWN，cleanup 结束进程组、回收进程并记录退出码。每任务设置独立 PI_CODING_AGENT_DIR，隔离 HOME/config，不继承 provider 环境；没有 --session-dir。

真实 OMP argv 构造及受启动侧 metadata_authorized 保护的 Popen 路径已实现，本轮从未调用。real session/prompt 即使直接绕过 Controller 调 Supervisor 也拒绝。loadSession/resume 能力必须经过 initialize 广告；参数依据旧只读空 session 探针，历史 probe 对完成历史恢复/真实 cancel 没有验收。

Git 仅固定 argv，无 shell；禁用户 hooks/global 配置，fetch 明确 origin main 后验证 base 是其祖先，为任务创建 controller/<id> 分支与 worktree。脏树拒绝；重复 ID、未知 SHA、symlink 均拒绝。retain 永远保留用户文件，没有自动清理命令。两个并行 task 的 worktree、agent state、session 分离。

证据表保存输入摘要、授权、RPC 请求/响应脱敏摘要、session ID、worktree base/branch、模拟验收、返工与 cleanup；artifact 可保存 test/commit/PR/CI/acceptance 等类别。当前模拟 acceptance 中 commit/PR 为 NOT_CREATED、CI/真实模型为 NOT_RUN。原始 payload 和原始 stdout 不落盘；stderr 全行脱敏后才入库，超长或截断行直接省略。凭据键、Bearer、password/token/api_key、URL userinfo、常见 key 文本脱敏，但不能保证识别任意无标签秘密；不得输入真实秘密。

限制：MCP 行 64 KiB、工具参数 32 KiB、输出 128 KiB、最多 8 个并行工具处理；wait 最多 1 秒且异步。ACP 帧 64 KiB、单 stdout/stderr 流 1 MiB、pending 16、普通记录 1000；每任务普通事件 2000，另保留最近 128 条关键安全事件，轮替累计数以 safety_events_dropped 显示。UNKNOWN、结算、重启恢复、cleanup、receipt、最终 acceptance artifact 的关联事件不受普通额度阻断；普通恢复操作在 Popen 前要求至少 32 条普通事件余量。artifact 100 条，每条脱敏数据 8 KiB；活跃队列 100。Git 使用 selectors 流式读取、stdout/stderr 合计 64 KiB 硬上限和 20 秒 timeout，异常清理进程组。完成任务与 worktree 不自动删除，需操作员离线归档和容量管理。

## 运维和验收边界

停止客户端/关闭 stdio 后等待 cleanup；强杀 Controller 后重启，检查 UNKNOWN，不手工删 slot。备份应在 Controller 停止后使用 SQLite backup API，保留 worktrees/agents（只复制主 db 而漏 WAL 不安全）。不要以删除状态库恢复正常执行。未知 schema/slot 配置不匹配拒绝启动；第一版 schema 只初始化专属新库，不修改业务数据库。

开发证据见 `docs/evidence/`：按垂直切片保存实际 RED/GREEN 输出，RED 初始缺模块属于缺失能力，额外缺陷有行为断言失败证据。返工前日志已去除本机路径；ROUND1 原始 unittest 日志保留本地源码路径，供本地审查，公开前应另作路径脱敏副本，原证据不改写。ROUND2 完整命令记录于 r2-execution.json，新增日志是去除本机路径的副本，原始输出留在私有 scratch；ROUND1 原证据不覆盖。测试为作者自测 AUTHOR_SELF_TEST / ISOLATED_FIXTURE，真实 MCP 传输不等于真实模型。ROUND1 曾独审并发现 R1-1/2/3，ROUND2 修复后独审尚待父代理执行；GitHub CI、真实 OMP/模型/QQ/生产、真实历史恢复、真实权限和取消验收均 NOT_RUN。

## 多模型策略增量

长期架构固定为 **Hermes 唯一总指挥 → OMP 多模型工程执行**；本地模型主施工，高级模型必须另有明确授权与预算。本阶段只实现结构、接口、代码审计与模拟测试，单模型不是长期路由约束。

`models.py` 管理 `model_profiles`、`model_policies`、`task_model_policy`、`model_call_events`、`model_budget`。`model_role`、`provider_allowlist`、`fallback_policy` 是版本化 JSON 合同，见 `schemas/model-config.schema.json`。本阶段仅支持 simulation-credit 整数预算；不是货币或 token 精确计费。profile 与 policy 同版本不可覆盖，只能注册新版本；task 提交事务同时保存完整策略/profile 快照、预算与事件，开始时使用已固定快照，不读取新默认策略。

默认两个已注册本地模拟 profile：mock-local-builder 与 mock-local-reviewer；默认策略 simulation-default@1，亦可在 submit 指定已授权 simulation-review@1。MCP 只选择已注册 policy 引用，不能注册 profile、改变 provider/role/预算、启用 fallback 或升级真实推理权限。高级 reserved-advanced 结构存在但 disabled、不在允许 role/provider 集合内。

`ModelRouter.begin(task_id, session_id, profile, role, reserve_units, fallback=False)` 是代码选择接口：执行前检查 RUNNING 模拟任务、已确认模拟 session、snapshot 中的 profile/role、enabled、显式 is_local=true、simulation=true、provider allowlist、预算可用量和未决调用。预留与 start event 在同一事务；`finish` 按实际模拟单位结算，超额/重复结算拒绝。UNKNOWN 保留预算和调用屏障，重启 STARTED 恢复 UNKNOWN，不重放。模型切换生成独立 model_switch 事件；默认 fallback deny，第一阶段不提供自动 fallback。模型权限与 ACP 工具 permission 分离：选到允许模型不授予任何工具。

调用记录关联 task/session/profile/version/role/start/end/outcome/failure_reason，evidence.model 返回 snapshot、budget、calls。真实模型入口仍关闭。模拟 peer 通过显式 `simulation/select_model` 测试扩展接收 route，再发模拟 prompt；这是专用 fixture 协议，不是 OMP 标准方法，不冒充 OMP 已支持该能力。多个 profile 选择及切换已通过 SQLite/模拟测试，真实 OMP 路由、隐式调用、网络/服务端预算控制均未接线。

只读 routing 审计及对应 OMP 18.15.0 源码确认：capability/index.ts:356 的 enabledProviders 控制外来用户配置源，不是模型白名单；config/model-registry.ts:3290/3364 使用 disabledProviders；modes/acp/acp-agent.ts:1636/1709 的 load/resume 会 switchSession 历史模型；session/agent-session.ts:1971 默认为不允许 session model fallback，但不能由此推出所有隐式角色无调用；retry fallback、title/memory/compaction/advisor 等仍需独立约束。Controller 策略没有映射为这些 OMP 控件，不能凭 prompt 或 mock 声称生效。真实 Strata 模型名称/权重身份和服务端 fallback 尚未闭合，所以本地 is_local 标签也不构成真实模型认证。

ROUND2 当前验证见 docs/rework-round2.md 与 docs/evidence/r2-final-suite.txt：59 项（原 53 + 新增 6）通过。ROUND1 与原 40 项日志均保留为历史；当前文件摘要清单为 docs/evidence/r2-file-manifest.json。CI 配置已写入，GitHub CI 尚未执行。

ROUND1：调度只扫描 QUEUED keyset 分页，历史及阻塞队列不遮住后续工作。execution_cleanup 与 slot 分开持久化；终态可先记录，但 cleanup 未确认时 wait 返回 timed_out、依赖与冲突调度均不放行。receipt/只读 resume 先检查 worker/peer/持久屏障。失败收尾保留占用，重启后 resume 仅在记录的 boot/进程组确认无活进程时核对 cleanup（终态也适用），不自动 replay；身份缺失时保持阻塞，需要离线操作员排查。Linux /proc 检查不抵抗恶意本地进程；硬盘/SQLite 写入不可用时不能承诺审计可用。

ROUND2：close 分别保护各 peer 回收和审计失败，finally 汇合 worker；全部已知进程组/reader 确认退出才释放 owner。任一资源仍存活则保留 owner，重复 close 可重试。错误收集在抛出异常的 cleanup_errors 中，磁盘写失败不宣称持久 cleanup 成功，slot 保持到实际落库/核对。单任务 claim 异常事务回滚后进入 PAUSED，以一次安全状态事件诊断，不自动重试；如果该诊断也无法写入则调度异常明确上抛，不能冒充成功。成功模拟回执先独立记录，acceptance 保持 100 条 artifact 上限；确实验收失败时保留回执与模型预算事实、任务保守 UNKNOWN。

默认与切换全链使用精确 profile@version；无版本内部 API 仅在 role 中只有一个同名引用时兼容。default_profile 必须属于 default_role。模拟 peer 的 catalog 从冻结快照筛选 enabled/local/simulation/provider 后经隔离环境传入，支持合法自定义模拟 profile；真实模型能力仍关闭。
