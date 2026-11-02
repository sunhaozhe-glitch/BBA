# Astra 第三轮审计与 OMP 软件开发续建任务书

## 审计结论

范围：当前 BBA 源码、bba-20261030-summary-console 报告/清单、相关实现与项目测试。不是生产发布验收，不是本轮浏览器或真实QQ验收；本轮无另一个独立审查者。当前结论：已有代码与基础测试可继续开发；本轮隔离业务验收证据不完整，需修改后再验收，不批准上线。

已独立复核：source-files.sha256 的摘要准确为 `3b2a0442f0a275461e04789ee67c53c8d0baa75ef8accb98b9fef2289a2de8ad`，246条、246个不同路径，sha256sum -c --quiet 退出0。指纹一致不证明业务正确。
实际复跑：kernel.sh test-all退出0（全Go race）；contract.sh check退出0（64测试及OpenAPI校验）；kernel.sh schema退出0（0001–0007执行成功）；Node22离线干净stage安装/构建/typecheck/测试及版本pin校验退出0，Collector 7/7通过。Node日志由进程结果保存，不能把测试总通过理解成所有新增接口已覆盖。

## 发现和验收缺口

### F1：证据包缺失（验收阻塞，事实已核实）
`evals/results/bba-20261030-summary-console/REPORT.md:1–7` 只有 Browser And Cleanup；“recorded above”所指正文不存在。目录当前只有REPORT和source-files.sha256，缺少声称的跨模块命令、输出、HTTP/prompt断言及浏览器记录。用户摘要、验收矩阵和源码不能替代v5/v6实际运行记录。不要补写猜测历史输出，应重跑并保存新证据。

### F2：worker恢复一次性标记过早消费（高优先级，静态确认；需红测）
`worker.go:426–434` 在实际恢复前将recoverPending=false；`worker_steps.go:44–67` 随后查询或单actor恢复失败均只返回错误，不恢复该标记。首轮短暂DB错误后，同一worker后续tick不再执行恢复。先故障注入复现，再改为全部处理成功后完成恢复、失败可重试；与活跃旧worker的隔离规则保持一致，不得因恢复重试重新发QQ请求。

### F3：runtime枚举固定首批导致漏处理（高优先级，静态确认；需红测）
`worker.go:519–529` 对所有active runtime按固定序LIMIT，无cursor；`worker_steps.go:48` 的一次恢复和`:313`每tick bridge outbox都用该函数。runtime组数超过BatchSize时，后面的组没有被扫描的机会，首批为空也不会向后推进。实现有界分页/公平轮转与恢复checkpoint；验收BatchSize+1组、首批空尾批有工作、分页中断重启、部分失败再次tick。

### G1：新接口合约不完整（已承认的集成缺口，不伪装为已通过）
GroupSummary/GroupContext、图读取以及proposal读取/approve等实际路由需与OpenAPI逐项对照。现有64测试通过不能证明未登记接口符合合约。公开/内部接口都要有明确schema、auth、版本和错误契约。

### G2：Summary HTTP缺少可定位自动回归
当前检索backend的*_test.go未发现GroupSummary入库/GroupContext专用覆盖。现有手动fixture不能替代自动回归。将NUL锁键缺陷、跨runtime/账号、不同内容复用ID、并发版本、过期、错误source/ref和最新版本选择固化为集成测试。

### G3：提案应用尚未驱动实际工作
`proposal.go:898–943` 写plan_application、提案状态、task版本与事件；当前worker未发现plan_application消费者。保留“apply不直接发送”，但还需显式plan step → 内核工作项映射及结果关联，才能称提案驱动任务。不能只验证账本已applied。

### G4：语义分析和ARTEX尚未验收
本轮通过的是确定性统计摘要，不能据此宣称理解群话题、意图或真实模型质量。ARTEX无live接线，当前为BBA自有图投影/模板提案。继续保留这些准确限制。

## 下一轮按软件开发流程执行

项目 `/run/media/sun/6F53FE6DFAA8CECF/QQ群项目/BBA`。沿用前两份任务书最终目标，本轮顺序如下。保留已有代码和用户文件，不重新架构。普通隔离开发直接推进；真实QQ、生产迁移、运行服务变更、live ARTEX与真实模型调用按具体有效授权，不将历史授权无限扩大。

### 1. 固定基线与验收用例
读取当前规则和新变化。建立本轮需求→代码→测试→证据表。以源指纹固定审计对象；没有Git不要造commit。保持历史清单不变，新文件另存本轮清单。

将F2/F3写成先失败的自动测试，保存失败输出，然后最小修复。分别给每actor恢复进度与公平分页定义事务/重启语义。运行原worker、unknown/no-resend、并发、迟到审批和暂停回归。

### 2. 契约优先，补齐Summary集成回归
枚举实际路由对照OpenAPI，补齐新增请求/响应、权限、错误、幂等与version；生成Go/TS类型，避免重复手写定义漂移。保留私有接口的最小权限。
新增Summary数据库+HTTP回归：锁键含分隔符不触发Postgres编码错误、重复请求不增版、同ID异payload冲突、并发版本唯一且递增、账号与QQ self_id映射、错群/错runtime拒绝、过期不返回、最新版本与精确版本读取。区分本地模型与外部模型的隐私政策；不要把model_inference自动等同于外传，若要支持本地语义分析应明确provider位置与原文出站策略并测试。

### 3. 固化可重复端到端测试，不删除源码runner
新增受版本追踪的 `tools/` runner或集成测试，自动创建具名隔离栈与fixture数据：
两个collector窗口→确定性Summary生产/发布→真实后端→实际GroupContextProvider→实际Character prompt→fake模型与transport。
断言第二版替代第一版、account/group/引用匹配、prompt标为不可信数据；跨群、过期、发布失败、重复窗口不串数据。不要硬编码必须v5/v6；干净环境以返回版本与顺序关系断言。保留runner源码，清理仅删除容器、volume、状态、秘密等运行资源。
真实浏览器验收认证读取、刷新仅GET、显式开发模式创建dry_run、暂停/恢复/取消及409，逐个保存请求方法/状态/脱敏收据；没有执行的按钮明确不算通过。测试中的fixture credentials用受保护机制注入，日志不得含明文token。
每条命令保存退出码、关键输出、时间、源指纹、fixture界限；输出新自包含REPORT，禁止从记忆重造旧日志。验证REPORT所有引用存在，证据清单校验通过。

### 4. 验收门通过后继续产品业务纵切
不要止步于补报告。完成以下可独立开发工作：
- proposal审批/应用到明确工作项的幂等映射，worker消费者，执行结果回流；未批准/过期/冲突不创建工作，重复apply不创建第二份工作，apply本身仍不调用QQ。至少一次完整fixture链。
- 将Summary ID/version/evidence与提案输入及图anchor绑定。第二次观察能影响下一步提案，并解释采用哪个版本；BBA账本仍为执行真相。
- 实现ARTEX最小只读adapter和contract fixture，依据实际本地源码/接口，不把ADR称接线。live访问需要单独范围，缺少时继续离线实现。
- 在现有UI增加群分析报告和摘要来源/版本页、proposal详情/批准/应用/结果追溯。保持同一前端；二维码登录的适配器及fixture UI也可继续开发，不触发真实扫码刷新。
- 真实模型语义分析与回复质量、容量作为下一验收层；先交实现和离线数据集方案，不以统计摘要替代语义质量。

### 5. 独立复审与发布准备
全部测试绿后，提交精确变更文件、红绿记录、契约差异、自动端到端与浏览器证据。由不同审查者读取代码并验证高风险路径，作者自述或模型仅审读日志不算独立运行验证。生产迁移、备份恢复、灰度writer切换、真实QQ链与回滚单独列发布门，不因开发fixture通过提前放行。

最终报告分栏：实际修复、独立复跑、作者运行、未验证、真实外部操作零/实际次数。先完成开发与隔离链，再一次性提出所需真实授权清单。不要每个普通开发步骤重新等待批准。
