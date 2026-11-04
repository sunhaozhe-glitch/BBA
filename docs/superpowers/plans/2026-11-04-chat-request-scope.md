# 双对话迟到响应隔离实施计划

**目标：** 切换会话、页签、页面、刷新或重新认证后，旧请求不能改写当前消息、表单回执、错误或认证状态。

**依据：** `AGENTS.md` 第 3.2、9、11、12 节及 `docs/product-acceptance-matrix.md` B7-06。仅修复已有基础对话行为，不增加发送权限、多步 Agent、分页或后端范围。

**架构：** 保留同源原生 JS 与已有 `api()`。会话列表和详情使用递增请求代次，并绑定 session、页面代次、页签和会话 ID；表单额外绑定其 DOM 实例。选中新会话立即清除上一会话消息，加载/失败不伪装成空会话。迟到写请求可能已落库，不重试或撤销，仅忽略其旧视图回执。

**文件与验证：**

- `tests/product/chat-request-scope.test.cjs`：Node test + Playwright 加载真实前端，loopback HTTP fixture 控制响应次序，断言浏览器 DOM。使用公开模拟数据；不把它当作 Go/DB 或权限验收。
- `frontend/app.js`：最小增加对话代次与视图检查，修复 GET、POST、错误、页签/刷新竞争，保持 HTML 转义和内存凭据。
- `backend/cmd/taskengine-server/product/app.js`：仅运行 `python3 tools/build-product.py` 同步。
- 本计划补记实际结果；独立日期证据位于忽略目录，不提交日志、截图、配置、凭据或二进制。

## 执行步骤

- [x] 写可控响应测试，先在原实现运行，确认旧 GET、旧错误、旧 POST 的具体错误行为；正向保存和当前错误也须覆盖。
- [x] 修复请求与表单的视图绑定，立即呈现新会话加载/失败状态，认证/刷新/页签切换失效旧代次。
- [x] 重跑回归，确认 red→green；前端语法及插件测试、同步 embed 比较、隔离 Go 服务 + DB 浏览器 smoke。
- [x] 审查 diff 和敏感信息，记录范围、限制与命令；交付独立 commit/PR，未经明确授权不合并。
- PR 最新 head CI、人工审批和合并状态在 GitHub 核对；本提交不将未来 CI 或合并记为 PASS。

## 运行条件

浏览器回归命令：`NODE_PATH=<包含 playwright 的 node_modules> BBA_TEST_CHROME=<Chrome 绝对路径> node --test tests/product/chat-request-scope.test.cjs`。Playwright/Chrome 是显式外部测试条件；缺失必须失败并说明，不自动安装、不悄悄 skip。不变更锁文件或 CI，CI 不自动证明此浏览器用例已执行。

真实服务回归复用 `tools/b7-manual-fixture.py` 和 `tools/b7-manual-fixture-browser-smoke.cjs`，新建独立 fixture，结束后核对清理；只授权假数据和零真实动作。

## 本轮结果（2026-11-04）

基线 `516d517f3eb21f581dcbb36f518f8806c48a85b4`，作者自测 `AUTHOR_SELF_TEST`；非人工独审、非受控 live/生产验收。原实现的首 18 个浏览器测试中 15 个出现预期断言失败、3 个控制用例通过；修复后包含额外当前错误/认证控制的 21 个用例全部通过，0 skip。覆盖旧 GET/POST 成功、403/500/401、A→B→A、刷新、退出、导航、加载/失败、当前保存和拒绝回执。HTTP fixture 仅负责时序与假响应，不能证明 Go 权限或持久化。

真实 Go 服务回归另外通过：当前源码构建服务 + 新建 PostgreSQL fixture，已有 B7 浏览器 smoke 13 断言 PASS；schema check 为 25/head=25，Chat/dispatch permit 数为 `0|0`，退出码均为 0，临时容器已删除且端口已关闭。无真实 QQ、真实模型、生产 DB 操作；原始本机日志与截图在未跟踪的 `evals/results/m1-chat-request-scope-20261104/`，不提交。该回归不额外声称完整分页、后端 account scope 或所有并发场景通过。

Node 22 的两文件语法检查与已有插件测试退出 0（91 pass、1 skip：未安装 NapCat bundle）。产品资源通过 `python3 tools/build-product.py` 同步；契约与 SQL 未变化，不重复执行无关 migration/全套 Go race，PR CI 仍须检查预期的三项作业。新增浏览器回归需按上文显式运行，现有 CI 不自动覆盖。合并/发布仍需对应授权；回滚为撤销本次变更并重新构建资源。

补充 Agent 只读审查未发现阻断项，并另行运行 21 项浏览器回归（21 pass、0 fail/skip）。该审查不是人工 approval；真实 Go smoke 仅由作者运行，未补造独立执行证明。
