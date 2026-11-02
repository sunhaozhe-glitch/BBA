# 第一阶段交付报告（ROUND2 更新）

READY_FOR_PARENT_REVIEW；本轮完成 R1-1 持续审计故障资源回收修复与 R1-2/R1-3 最小修复。分支 feat/controller-phase1，基线 43c049c42d66f51bd1b59750d2750bccb8d46b66。全部仍新增未提交、未推送。

当前实际全量：`PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=controller/src:controller/tests python3 -m unittest discover -s controller/tests -v`，**59 tests in 83.836s / OK / exit0**。原53项保留，新增6项；证据 evidence/r2-final-suite.txt，逐项RED/GREEN、根因、可达性与限制见 [rework-round2.md](rework-round2.md)。ROUND1 53项和更早40项日志保留为历史，不覆盖。

close 分隔每个 peer 的进程回收与审计错误，finally 汇合 worker；活资源保留 owner 且允许重试，全部资源退出才释放 owner。无法落库时不宣称持久 cleanup，不释放 slot。适配独审 close_store_fault 三次均两个真实模拟进程退出、owner关闭、2个slot保留，错误仍上抛。claim 异常一次性隔离为 PAUSED 并安全诊断，后续任务继续。成功回执与最终验收事件脱离普通事件额度，保留 artifact 及预算事实；确实验收失败仍 UNKNOWN，模型成功不等于业务成功。

原独审观察脚本副本实际执行24条、无probe_error；包含刻意禁止close的双故障观察（alive=[true,false]且owner保留），新增回归验证解除注入后可重复关闭完成。公开可达性与内部注入严格区分；未证明当前公开工具可使QUEUED或单次标准success达到2000条普通事件。

范围为 controller/**、专用 .github/workflows/controller.yml、docs/adr/controller-phase1.md。本轮实现修改仅controller.py与store.py；workflow已有全量discover，无需改动。当前摘要清单 evidence/r2-file-manifest.json，范围核验 evidence/r2-scope-check.json；旧manifest保留历史。新增日志为去本机路径副本，真实原始输出在私有scratch，未复制凭据。

验证身份 AUTHOR_SELF_TEST / ISOLATED_FIXTURE；ROUND2独立执行/独审待父代理，GitHub CI、安装、PR/合并、真实OMP/模型/历史恢复/cancel、QQ与生产均NOT_RUN。普通quota与128条安全保留上限未取消，fixture/OBSERVE/真实模型deny/精确版本/持久cleanup门禁全量通过。未制造真实只读磁盘，不能承诺磁盘不可写仍可审计。
