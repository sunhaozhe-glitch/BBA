# ADR: Controller 第一阶段独立研发控制器

状态：第一阶段实现，待父代理独立验收与 PR。基线为 origin/main `43c049c42d66f51bd1b59750d2750bccb8d46b66`；本轮不提交、不推送、不合并。

## 背景与范围

Controller 调度的是研发 fixture 工作和 ACP 进程生命周期。它不创建或执行 QQ Task，不连接 Go Task Engine、Chat Permit、PostgreSQL 或业务生产系统。因此 SQLite 不取代业务 PostgreSQL，也不争夺 QQ 执行所有权。代码、schema、测试与证据在 controller/，CI 独立，不修改现有业务组件。

## 研究事实

读取根 AGENTS.md 和已有 ACP 元数据探针。旧探针曾确认 OMP 18.15.0 的 protocolVersion=1、loadSession=true、sessionCapabilities.resume，以及 initialize/session/new/load/resume 的参数。旧探针只创建空会话，明确未发送真实 prompt；不能推断完成历史恢复、真实模型、权限请求、cancel 或 side effects。

本机实际可导入 MCP SDK 2.3.0、jsonschema 4.26.0，使用 2.3.0 的 Server on_list_tools/on_call_tool 与 stdio_server 接线。没有安装软件、调用模型或执行 OMP。unittest 足以覆盖 asyncio 与真实子进程，未引入 pytest。

## 决策

1. 使用独立 Python src 包与固定 SDK 依赖，stdio MCP 是正式入口。CLI 只提供启动配置/调试，七工具共用精确 schema。模型、repo、argv、mode 不接受 MCP 输入。
2. 默认 OBSERVE；fixture 仓库与授权由启动侧固定，DEVELOPMENT 另需启动侧授权。第一阶段真实模型 prompt 永远 fail closed。真实 OMP 进程构造保留在 Supervisor，只有独立 metadata authority 才能到达，Controller/CLI 不调用它。
3. 专属 SQLite WAL 使用 BEGIN IMMEDIATE；状态/CAS version/event/slot 在同一事务。随机唯一事件 ID 不等于内容摘要，重复合法通知保留。持久 config v1、仓库路径摘要绑定与 owner flock 防止错误重启/多 owner。slot 默认 1、最大 2，未知写集、父子冲突、未完成依赖禁止并发。
4. 不确定执行 UNKNOWN 保留槽与屏障，无自动重放。恢复只 load/resume metadata，不 prompt。模拟显式终态 receipt 是 fixture 人工核对接口，不能冒充真实权威回执；真实 reconcile 待后续适配器和授权。
5. Git 管理固定精确仓库，本地 fixture origin 明确 fetch main，验证 base SHA 祖先，为 task 创建独立分支与 worktree。工作树脏数据与历史文件永久 retain，不提供自动清除或外部 write。
6. ACP stdin/out/err 分离，RPC Future 按 ID 关联。请求 timeout、断线、EOF/协议错误和未知退出保守处理；反向所有权限/fs/terminal 拒绝。独立 PI_CODING_AGENT_DIR 且无 --session-dir，process group cleanup 有事件。实际模型 prompt 被代码层拒绝。
7. 证据只保存脱敏结构和摘要，不保存 raw RPC/stdout，stderr 持久化前脱敏，长行省略。提交、session、授权、请求响应摘要、worktree、验收/返工/cleanup 可追溯。commit/PR/CI 无观察即 NOT_CREATED/NOT_RUN。
8. 每任务三轮返工预算与原因/修改/结果记录，三次失败 PAUSED。当前模拟 rework 只演练记录和停机机制，不自动调用模型修复或执行外部写。
9. 使用 unittest 实际 SQLite、多线程 claim、子进程事务 crash、fixture Git worktree、模拟 ACP 子进程及真实 MCP SDK client。每垂直切片先 RED 再实现 GREEN，日志归档并去掉本机隐私路径。

## 结果与权衡

第一阶段可独立运行并展示研发控制器完整 fixture 生命周期。数据库只初始化专属新库；没有旧版本迁移承诺。操作员是本地状态目录与启动配置的权限主体；此实现不是远程多租户服务或对恶意本地文件替换的安全沙箱。

事件/流/输入/输出/并发有明确额度；Git selectors 流式输出硬上限 64 KiB、20 秒 timeout，并清理进程组。worktree retain 与历史任务保留需要离线归档，尚无自动磁盘 quota。原始敏感数据应从输入端排除，正则脱敏不能识别任意无标签秘密。

未覆盖真实 OMP、模型 turn/history/cancel、真实模型权限、QQ、生产数据库、网络远程 Git、GitHub CI 和独立审查；均不得由 fixture 通过外推。真实开发 writer/模型上线需后续 ADR、明确授权、真实权威 reconciliation 和单独受控验收。

详细合同、状态机、运行手册、限制和验收见 controller/README.md；最终实际输出见 controller/docs/evidence/final-verification.txt。

## 多模型长期架构与本阶段增量

Hermes 是唯一总指挥；OMP 承担多模型工程执行，本地模型主施工，高级模型必须明确授权并受预算约束。单模型仅是早期安全边界，不将 Controller 写成固定单模型路由。

增加 model_profiles/model_policies/task_model_policy/model_call_events/model_budget，model_role、provider_allowlist、fallback_policy 用版本化 JSON 表达。模型配置 schema 无凭据/endpoint 字段，未知字段拒绝；profile/policy 版本不可变。提交事务冻结完整任务策略和 profile 快照，并初始化 simulation-credit 预算。原七工具不增，只在 submit 增 model_policy_ref、evidence 增 model 数据；引用只能选择启动侧已注册策略，不能注册或升级授权。

ModelRouter 是调用前的代码门禁：profile 注册、显式本地模拟、provider allowlist、role、session/task 绑定、预算预留、未决调用屏障均校验。结算不可超预留，UNKNOWN 保留预算且不自动重试。每次 call 记录 task/session/profile/version/role/start/end/outcome/failure_reason，切换独立审计；fallback 默认且本阶段固定 deny。高级 profile 仅保留 disabled 结构。模型授权与工具 permission 分开，允许模型仍不允许反向工具权限。

两个默认 mock-local profile 可选，模拟选择接口经专用 simulation/select_model 与 peer 接线，不是 OMP 标准协议。本地 profile 切换测试证明 Controller 状态/审计/预算，不证明真实模型路由。没有把 OMP 的模型选择、role、fallback、隐式调用和 provider 约束映射为已验收生产控制。

只读读取 routing-audit.md 与对应上游源码，确认 enabledProviders 是外来配置源 opt-in（capability/index.ts:356），model registry 的 find/getApiKey 用 disabledProviders（config/model-registry.ts:3290、3364）；ACP load/resume 经 #openStoredSession 调 switchSession（modes/acp/acp-agent.ts:1636、1709），可能恢复历史选择。session/agent-session.ts:1971 的 allowSessionModelFallback 默认关闭不等于所有角色/隐式调用均被禁，retry.modelFallback 与 title/memory/compaction/advisor 等需要另验。不能假设 OMP 18.15.0 原生执行 Controller 所有策略。真实 Strata 身份/权重与 fallback 审计仍 BLOCKED，真实推理保持代码拒绝。

原 40 项全量为返工前历史，不能覆盖独审 F1–F5；ROUND1 当前全量和逐项 RED/GREEN 见 controller/docs/rework-round1.md、controller/docs/evidence/r1-final-suite.txt。均 AUTHOR_SELF_TEST，返工后独审、GitHub CI 和真实模型验收 NOT_RUN。

## ROUND1 收敛

调度用 QUEUED keyset 分页；execution_cleanup 独立持久屏障保持到进程组与 reader 清理确认，结果终态本身不释放活资源。wait 和依赖均检查屏障，receipt/resume 不与执行 worker 交叠。cleanup 失败保留占用和进程组身份，重启后通过只读 /proc 核对；缺失身份不能推断安全。进程启动后构造/审计失败必须 cleanup，cleanup 再失败向 Controller 交回 peer 与占用。

普通事件仍限 2000；关键安全事件单独有界保留最近 128 条，轮替累计可见。关键状态、模型结算/recovery、cleanup、receipt 不依赖普通额度。只读恢复在 Popen 前检查普通额度余量。该通道解决额度耗尽，不承诺磁盘/SQLite 硬故障可写。

模型全链精确 profile@version、default role 校验、同名跨版本切换审计；无版本代码 API 只兼容无歧义选择。fixture catalog 从任务快照中筛选允许的本地模拟项传入 peer，不增加真实 provider 或模型出口。

## ROUND2 故障隔离与最终验收

close 对每个 peer 回收、二次错误审计及 cleanup 持久化分别保护，finally 汇合关闭开始时及期间的 worker；错误收集后上抛。closed 仅禁止新 dispatch，全部已知进程组/reader 已退出才关闭 owner；仍有活资源时保留 owner，允许重复 close 收尾。审计失败并不等于资源仍存活，也不等于 cleanup 已持久成功；无法落库时 slot 不放宽。磁盘不可写仍不可承诺审计。

单任务 claim 事务失败后，以一次安全状态事件进入 PAUSED 并记录异常类型，继续后续队列，避免无界重试。该安全写入也失败则明确传播存储异常。成功模拟回执先保存 prompt_receipt 安全事件；最终 acceptance artifact 关联 event 不依赖普通事件额度，artifact 100 条与安全事件128条上限保持。模型结算与任务验收独立，确实验收失败时保守 UNKNOWN 且保留回执/预算事实。

R1-2/R1-3 满额度情形为内部 Store.event 注入，未证明七个公开 MCP 工具可使 QUEUED 或单次标准 success 达额度，不夸称公开 DoS。持续 readonly 异常亦为真实记录调用点注入，未制造真实磁盘故障。

当前作者自测59项（原53+新增6）全绿，三次close_store_fault均无进程泄漏且owner关闭、slot保留；原观察脚本副本24条执行无probe_error。证据 controller/docs/rework-round2.md、controller/docs/evidence/r2-final-suite.txt 与 r2-file-manifest.json。历史证据保留，新增日志路径脱敏。ROUND2独审、GitHub CI与真实模型验收待后续授权/父代理，未提交推送。
