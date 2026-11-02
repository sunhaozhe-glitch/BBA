# Controller ROUND2 返工结果

状态：READY_FOR_PARENT_REVIEW；AUTHOR_SELF_TEST / ISOLATED_FIXTURE。基线 `43c049c42d66f51bd1b59750d2750bccb8d46b66`，分支 `feat/controller-phase1`。全部仍为新增未提交树，无安装、提交、推送、真实 OMP/模型或生产操作。

## R1-1：持续审计失败不得中断资源回收

根因是 Controller.close 在 peer.close 抛错后无保护地再次写 cleanup_error；该写入再失败会跳过其他 peers、worker gather 和 owner.close，且 closed 标记阻止后续重试。

close 现在串行化关闭尝试，closed 仅封锁新 dispatch，owner.closed 才代表资源关闭完成。每个 peer 的 close、错误审计和 cleaned 持久化各自隔离；finally 汇合 worker，并补查期间登记的 peer。进入关闭时快照 worker，避免 done callback 从 running 删除后丢失 worker 异常。错误保存在抛出异常的 cleanup_errors 中，并附类型摘要；兼容原异常类型，不静默成功。全部已知进程组/reader 确认退出才关闭 owner；仍有活资源则保留 owner，重复 close 可重试。

真实两个 sim_peer + Store.event 持续注入 SQLite readonly OperationalError 的持久回归通过；workers 均完成、错误被收集，owner 关闭，2 个 slot 保留，无虚假的 cleanup_confirmed。双故障（禁止一个 peer.close + 禁止审计）验证继续回收另一个 peer、活资源仍持有 owner；解除注入后再次 close 回收剩余 peer，第三次 close 幂等。

独审 close_store_fault.py 复制至新 round2-probes 目录，仅初始化 error 变量及将断言改为修复期望，原脚本和旧证据未覆盖。实际运行三次均 `alive=[false,false], owner_closed=true, retained=2`，并继续抛 OperationalError。该测试在真实存储调用点注入异常，未制造真实只读磁盘；不能由此声称磁盘不可写时仍可审计。进程实际退出与持久 cleanup 是不同事实，无法持久化时不放宽 slot，后续可信重启核对仍走既有屏障。

## R1-2：单任务 claim 故障隔离

根因是满普通事件额度的 QUEUED task 在 claim 事务写 RUNNING event 时抛错，事务虽回滚，调度协程整体终止。

每个任务 claim 独立捕获异常，回滚后以关键安全 state event 一次性进入 PAUSED，记录 reason=claim failed 与异常类型，不记录原始错误 payload；继续后续队列，不自动重试刷事件。真实 quota Conflict 与注入单任务 OperationalError 两项回归均通过，无关任务 SUCCEEDED，slot 正常释放。如果暂停/诊断也无法落库，异常明确向调度协程和 close 传播，不将失败冒充成功，也不无界重试。

公开可达性：submit 为公开 MCP；填满 QUEUED 的 2000 条普通事件需要内部 Store.event 注入，尚未证明当前七工具公开可达到。因此保留原 P2 条件性健壮性结论，不声称公开 DoS。

## R1-3：成功回执后的最终验收事件独立于普通额度

根因是 acceptance artifact 与普通 artifact event 同一事务，满普通额度时两者一同回滚，模型已结算 SUCCEEDED 但任务 UNKNOWN 且无 acceptance。

已观察的模拟 stopReason 先写 prompt_receipt 关键安全事件；acceptance artifact 含该 receipt，关联 event 使用既有 128 条安全通道，不消耗普通额度。artifact 100 条上限、普通事件 2000 条上限和安全轮替上限均保留，未增加模型能力或 schema。业务状态仍由模拟验收/rework 与当前状态判断，模型成功不替代验收。确实无法完成验收（内部填满 artifact 上限）时，新增回归证明任务 UNKNOWN、slot 保留、模型预算已结算且成功 receipt 仍可查。既有 hang/未知回执/重启保守语义全部通过。

公开可达性：成功响应来自真实 fixture peer，成功响应后的普通额度填满由 threading gate + 内部 Store.event 注入；未证明标准公开单次 success 会自行耗尽额度。不把模拟成功当真实模型或业务生产验收。

## 实际执行与证据

命令统一从仓库根以 `PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=controller/src:controller/tests python3` 执行。

| 入口 | 结果 | 日志 |
|---|---|---|
| unittest test_rework_round2（初始 R1-1 两项） | RED：2 failures → GREEN：2 tests OK | r2-1-red/green.txt |
| unittest R1-2 两项 | RED：2 failures + 调度异常收尾2 errors → GREEN：2 tests OK | r2-2-red/green.txt |
| unittest R1-3 满普通额度 | RED：UNKNOWN != SUCCEEDED → GREEN：1 test OK | r2-3-red/green.txt |
| worker 异常收集强化断言 | RED：0 != 2 → GREEN：1 test OK | r2-worker-red/green.txt |
| receipt/真实验收失败与 close 补充检查 | 2 tests OK；未为已有行为再改实现 | r2-extra-check.txt |
| -m unittest discover -s controller/tests -v | **59 tests in 83.836s，OK，exit0** | r2-final-suite.txt |
| round2-probes/close_store_fault.py | 三次无泄漏、owner关闭、slot保留，exit0 | r2-close-store-fault.txt、round2-probes/close-store-fault.json |
| round2-probes/independent_probes_executed.py（原脚本字节副本） | 8类各3次，24观察、0 probe_error，exit0 | r2-independent-probes.txt、round2-probes/independent-probes.json |

原观察脚本的 close_secondary_audit_failure 刻意禁止第一个 peer.close，三次均 alive=[true,false]、owner_closed=false，表示未回收资源仍受 owner 保护，脚本 finally 自行回收。新增持久回归验证其后的 retry。queue 观察固定等 .2 秒，其中一次 tail 已 SUCCEEDED 但 cleanup 尚未完成、slot=1；这不是释放证据。持久回归等待 worker 完成后断言 slot=0。三次 quota_success_receipt 均 task/model=SUCCEEDED、artifacts 含 worktree/acceptance、slots=0、cleanup=false。

新增日志为真实输出的路径脱敏副本（`<REPO>` / `<HOME>`），原始输出仅留私有 scratch。asyncio debug 的慢任务诊断保留，不伪造干净输出。ROUND1 历史日志、manifest、原独审目录均不覆盖。当前完整清单为 r2-file-manifest.json（不对自身摘要），执行命令/时间/退出码为 r2-execution.json，范围与摘要校验为 r2-scope-check.json。

## 边界与交接

修改产品实现仅 controller.py、store.py；新增六个持久回归，更新 README、完成报告、ADR。专用 CI 已 discover 全测试，无需改工作流。未修改其他业务组件或扩大架构。

待父代理独立执行与独审；本报告不构成独立审查。GitHub CI、安装、PR/合并、真实 OMP/model/history/cancel、真实磁盘只读/ENOSPC、QQ/生产均 NOT_RUN。既有 OBSERVE、真实模型 deny、精确版本、fixture catalog 与 cleanup gates 保持。安全事件仍只保留最近128条，关键 receipt 可在后续大量安全操作后轮替，acceptance artifact 保留完整 receipt；未新增无限归档承诺。
