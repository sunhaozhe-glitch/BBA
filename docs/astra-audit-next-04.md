# Astra 独立审计与 omp 第四轮任务书

## 审计结论

第三轮有实质进展，但**需修改后再验收**。源码与全 Go race 测试通过，不等于产品链路已闭环；本轮未执行真实 QQ、生产迁移、部署或 live ARTEX 操作。

## 复核证据与未关闭项

- 全量 `bash tools/kernel.sh test-all` 本轮退出 0；包含 `internal/artex`、`internal/taskengine`。这是本次独立复跑证据。
- `evals/results/bba-20261030-round3-20261030T015830Z/REPORT.md` 存在，记录了有用的隔离验证与 UI 观察；包内 browser 截图、网络记录及 fixture summary 文件也存在。但执行 `sha256sum -c .../source-files.sha256 --quiet` 失败：新清单使用相对路径，应从项目根目录校验；包清单 `manifest.sha256` 也未通过从其目录执行的校验。故该归档的完整性未通过，不能按当前状态作为不可变证据包引用。先保存原件，不要覆盖；新建修复后的证据包并解释校验根目录/原因。
- Proposal/worker 集成测试 `proposal_basis_test.go` 先 apply，再 `worker.Tick()` 后观察到一条 dispatch。但 apply 后无 execution/permit/claim/outbox；测试没有证明该 proposal 的内容或 application 被 worker 消费，也没有把执行与 proposal/task step 做因果关联。不得把它描述为“proposal application 被 worker 消费”。补充可追溯的内核关联，或将产品声明降格为“apply 记录 intent，后续既有任务调度独立执行”，并在 UI/验收矩阵清楚表述。
- `runtimeActors` 当前最多扫描 `workerMaxRuntimePages = 10` 页。分页改善了此前首批偏置，但超过扫描上限的后续 runtime 仍可能长期饥饿；没有持久游标/稳定轮转。需加入可恢复、公平轮转的分页，并用超过十页的测试证明每个 runtime 最终可被处理，且运行中新增/删除不会造成跳过或重复副作用。
- 上述未关闭项属于验收和实现边界，不抵消本轮已有的 Summary/GroupContext、ARtex 只读适配器、控制台与回归测试进展。ARTEX client 只是只读接入，不是双图语义对接/anchor/任务图执行完成。

## 给 omp 的第四轮提示词

```text
继续在 /run/media/sun/6F53FE6DFAA8CECF/QQ群项目/BBA 工作。先读 docs/astra-audit-next-04.md、docs/astra-audit-next-03.md、docs/product-acceptance-matrix.md 和当前实现；不要重置、覆盖审计原件或触碰真实 QQ/生产环境。

目标：关闭第三轮审计的两个实质阻塞，并生成可核验的新证据包。按“红测复现 -> 最小实现 -> 定向测试 -> 全量验证 -> 证据归档 -> 自审”的顺序执行，不要先做新 UI/功能扩张。

1. Proposal apply 因果链：检查当前 application、task ledger、worker、proposal tests 和 UI 的实际语义。若产品目标是“apply 后由 worker 执行该 proposal”，实现明确、可审计、幂等的关联/消费状态，使 execution、permit、claim、dispatch 皆可追溯至 proposal/application/step，worker 只能执行已批准且版本/范围仍有效的工作；处理崩溃恢复、重复 tick、撤销/过期/版本冲突。先写会失败的集成测试，证明无 apply 不执行、apply 后只执行获批 step、同一 application 不重复执行、结果回流到对应 proposal/task。如果当前安全模型不允许此链路，则不要假装实现：明确将 UI、API 文案和 acceptance matrix 定义为“apply 只记录 intent，既有任务调度独立”，移除会误导的测试命名/结论，并把真正的 proposal-driven dispatch 列为未完成。

2. Runtime 公平轮转：替换固定最多十页且每轮从头开始的枚举，设计稳定、持久、可恢复的 cursor/轮转策略，明确并发实例、runtime 增删、空页与数据库错误的语义。先构造超过当前 10 页上限的红测，证明末尾 runtime 在有界 tick 次数内得到服务；验证 cursor 重启恢复、失败项隔离、不丢失 bridge outbox 与恢复动作、不重复执行外部副作用。给出复杂度和游标存储/事务边界说明。

3. 验收与文档：同步 OpenAPI、验收矩阵、开发控制台/ARTEX ADR（仅涉及实际改动）。清楚区分 BBA 本地图投影、ARTEX 只读 API、ARTEX 双图/anchor/写入适配三者。不得把 fixture、统计 Summary、只读 ARTEX GET 或本地 dry_run 描述成真实群语义、真实 QQ 发送或 ARTEX 写入验收。

4. 新建独立结果目录 `evals/results/bba-round4-<UTC时间>/`，不覆盖 round3。记录精确命令、UTC 起止、exit code、工具链/镜像、原始日志；运行 schema、完整 Go race suite、proposal/worker/summary 定向集成、contract、Node 22 验证。修好证据脚本/归档：source-files.sha256 必须在项目根目录可校验，manifest.sha256 必须从包目录可校验；新增 REPORT 后重建 manifest，最后再校验两者。提供一个独立 `verify` 命令，故意改动临时副本应失败、原包不变应通过。保存浏览器证据及所有响应的状态码/方法，脱敏凭据、cookie、个人群数据；失败的证据不得删掉或伪装成通过。

5. 结束前自审：逐条列出改动文件、红测失败与绿测结果、所有命令的真实退出码、未完成/未验证边界、证据目录与清单摘要。检查服务/容器是否仍运行并按本轮隔离资源清理；不得清理用户既有实例或卷。不要提交、部署、迁移生产数据库、执行真实 QQ 写操作或访问 live ARTEX；不要声称“所有模块完成”。如果关键架构决定无法从现有 ADR 确认，先记录冲突与选项并停止该项，不要自行扩大授权。
```
