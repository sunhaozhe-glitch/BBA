# Controller 第一阶段 ROUND1 返工报告

基线 `43c049c42d66f51bd1b59750d2750bccb8d46b66`，分支 `feat/controller-phase1`。唯一施工者，全部新增未跟踪树；未提交、推送、安装、真实 OMP/模型/生产操作。修改限于 controller/** 和 docs/adr/controller-phase1.md，专用 CI 保持原有范围。验证身份 AUTHOR_SELF_TEST / ISOLATED_FIXTURE；返工后独立审查尚未执行。

## F1：历史窗口饿死与阻塞队列公平性

根因：调度查询所有任务前 100 条，终态历史长期遮住后续 QUEUED。

修改：Store.queued 按 `(created,id)` keyset 分页，先过滤 QUEUED；每轮遍历全部分页，失败 claim 不截断后续任务。新增 105 条公开 submit/cancel 历史、90 条依赖阻塞队列、尾部可运行任务回归。

RED：`r1-f1-red.txt`，预期 SUCCEEDED 实际 QUEUED。GREEN：`r1-f1-green.txt`，exit 0。

## F2：结果终态与资源清理分离

根因：终态事务立即删 slot，receipt 在活 peer 检查之前；内存延迟无法覆盖 crash。

修改：claim 时事务写 execution_cleanup.pending；终态保留占用直到 close 确认进程组及 reader 结束。wait、依赖检查持久屏障。resume/receipt 检查 worker、peer 和持久 cleanup；恢复 peer 也持有屏障。失败清理保留 peer、slot、boot/pgid 身份和 cleanup_error；重启后的 resume 用 Linux /proc 核对该进程组无活成员后记录 cleanup_confirmed。终态但 cleanup 遗留也可通过该核对路径完成，无 replay。无身份的 crash 窗口保守阻塞，不推断完成。

RED：`r1-f2-red.txt`（活 peer 时 slot=0）、`r1-f2-restart-red.txt`（进程退出后不能核对）、`r1-f2-terminal-red.txt`（终态 crash 无核对入口）。GREEN：初始 `r1-f2-green.txt`、扩展 `r1-f3-green.txt`、`r1-extra-green.txt`；最终全量包含全部断言。正常成功及 UNKNOWN receipt 两条 cleanup gate、跨 Store 可见、依赖、同写集排队、cleanup 失败/重启均覆盖。

## F3：额度与审计故障的生命周期边界

根因：Popen 后首条审计异常脱离所有权；close 在审计异常后跳过 kill/wait；普通额度阻断安全状态/模型结算/recovery；重复恢复最后一次先启动后拒绝。

修改：Supervisor 构造失败清理；清理也失败时将 peer 交回 Controller 并保存占用及身份。close 使用独立互斥，记录失败仍执行 TERM/KILL、wait、reader join、streams close；Controller 对所有 peer 尽力收尾后再报告异常，错误落 cleanup_error 而非静默吞掉。普通 Supervisor 额度抛明确错误，关键清理审计仍记录。

Store 普通事件仍限 2000。独立安全通道最多保留最近 128 条关键事件；轮替累计数 `safety_events_dropped` 持久化并在关键事件 data 中可见。UNKNOWN/安全终态、模型 finish/recover、授权 receipt、disconnect/timeout/cleanup 不依赖普通额度；关键投影本身仍保留在任务、预算和 cleanup 表。只读 resume 在 Popen 前要求普通审计余量至少 32，不因 quota 拒绝启动 peer。公开恢复中关键事件也轮替，因此额度触发次数由旧约 199 次变为本次实跑第 245 次，不将旧次数当新合同。

RED：`r1-f3-red.txt` 三条实际失败，另 `r1-f3-close-red.txt`、`r1-f3-retention-red.txt`、`r1-f3-double-failure-red.txt`、`r1-f3-record-limit-red.txt`。GREEN：`r1-f3-green.txt`、`r1-f3-close-green.txt`、`r1-extra-green.txt` 及最终全量。一次初始 GREEN 尝试仍失败因安全事件独立后 230 次不再触发普通额度；将上限调整为 400 并保留原行为断言，无取消 quota。full-quota 测试通过 Store.event 注入普通事件到满额；公开 repeated resume 单独实跑验证可达性，两者不混写。

## F4：精确 profile 版本

根因：Controller 丢弃版本，Router 同名取首个授权项。

修改：Controller→Router→peer 全链保留精确 profile@version，peer 返回精确选择 receipt 并记录 model_selected。无版本内部兼容 API 仅匹配唯一同名引用；snapshot 校验 default_profile 属于 default_role。model_switch 含 from_ref/to_ref，同名不同版本也产生切换事件。

RED `r1-f4-red.txt`：default @2 实际 @1。GREEN `r1-f4-green.txt`；最终回归验证默认 @2、同名歧义拒绝、@1→@2 审计和非法默认 role。

## F5：受控模拟 catalog

根因：配置允许合法新 profile，peer 硬编码两个名称。

修改：从冻结任务快照筛选 enabled/local/simulation/provider allowlist 的 catalog，经隔离环境变量传入 fixture peer，精确 ref/role 校验。该扩展仅 simulation；真实模型入口保持拒绝。

RED `r1-f5-red.txt`：合法 custom@7 得 UNKNOWN。GREEN `r1-f5-green.txt`：SUCCEEDED、slot=0、reserved_units=0、peer 确认 custom@7。

## 可复跑命令与实际结果

仓库根逐项命令格式（对应 RED/GREEN 文件中的 test 名）：

```sh
PYTHONPATH=controller/src:controller/tests python3 -m unittest test_rework_round1.ReworkTests.<test_name> -v
PYTHONPATH=controller/src python3 -m unittest discover -s controller/tests -v
PYTHONPATH=controller/src python3 controller/docs/evidence/rework-probes/adapted.py
PYTHONPATH=controller/src python3 controller/docs/evidence/rework-probes/adapted_public.py
git diff --check
```

最终全量 `r1-final-suite.txt`：**53 tests in 63.806s / OK / exit 0**，原 40 项保留，新增 13 项持久回归。早期全量 `r1-full-suite.txt` 50 项也是历史，不替代最终输出。

原独审 probes.py/public_quota.py/controls.py 字节保留在 `evidence/rework-probes/original/`。适配器加载原 probe 源码，明确改动 cleanup 的不安全“等第二 peer 启动”为 QUEUED/占用断言，quota 计数区分普通与关键通道；不以 exit 0 代替行为断言。

`r1-adapted-probes.txt`：100 历史后 SUCCEEDED；slots=1 时两条 cleanup 路径均只有 1 活 peer、第二任务 QUEUED，活 peer receipt 返回 CONFLICT；满普通额度后 task/model UNKNOWN、slot=1、peer_alive=false、重启 opened=true；default 精确 @2；新 profile SUCCEEDED 且 reserved=0。末行 ALL_F1_F5_ADAPTED_ASSERTIONS_PASSED。

`r1-adapted-public-quota.txt`：iteration=245 首次拒绝，后续拒绝，leaked_live_peers=[]；断言额度拒绝未新增 Popen，末行 PUBLIC_QUOTA_NO_LAUNCH_NO_LEAK_ASSERTIONS_PASSED。

空白/范围检查结果记录于 `r1-scope-check.txt`，manifest 更新为当前全部交付文件。

## 文件与风险

产品修改：src/bba_controller/{controller,store,acp,models,sim_peer}.py；新增 tests/test_rework_round1.py；README、completion-report、本报告、ADR 与 evidence/manifest 更新。七 MCP schema 未变，现有合同测试通过。

剩余边界：只覆盖 fixture。真实 OMP/模型/QQ/生产、GitHub CI、返工后独审均 NOT_RUN。安全通道是有界近期审计，会轮替并显示累计；不是无限不可删事件存档。SQLite/磁盘不可写时仍可能无法落审计，资源占用坚持 fail closed。Popen 与首个身份持久化之间 crash 无可验证身份，需离线排查，不能用 receipt 绕过。/proc/boot/pgid 核对依赖 Linux 和可信本地操作员，不是恶意本地用户沙箱；历史/worktree 仍需容量管理。
