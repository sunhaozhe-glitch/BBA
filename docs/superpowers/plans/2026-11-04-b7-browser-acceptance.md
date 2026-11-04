# M1 B7 Browser Acceptance Plan

> **For agentic workers:** Use superpowers:subagent-driven-development to execute the bounded tasks below.

**Goal:** 验收现有双对话/草稿/通知 UI，补足真实服务浏览器证据，记录未交付项而不宣称完整 M1 通过。
**Architecture:** 复用 frontend、已有 Playwright fixture 和隔离 PostgreSQL/真实 Go binary。UI fixture、真实服务、API 辅助检查分开记录；不接真实平台。
**Spec:** AGENTS.md §4.1/§6/§10-12；docs/product-acceptance-matrix.md B7-01..07。
**Baseline:** origin/main a25ea659a1796f2de3f095773afc3bf1beba4ae9。

## Global Constraints
- 最多两个子代理；主代理审核/独立复核。凭据、截图、原始日志只留本机忽略目录。
- 不修改业务 Go/DDL/权限/依赖，不新增分页产品功能，不调用真实 QQ/模型/MCP/ARTEX，不部署或迁移现存库。
- 真实服务仅由一个执行者启动，使用全新输出目录；finally 清理并核对容器/端口。
- 弱断言与测试标题不算证明；失败原样留证，测试 oracle 修正必须有针对性 RED/GREEN。

## Task 1: UI fixtures and request-scope regression
**Files:** tests/product/chat-request-scope.test.cjs；tools/b7-chat-notify-browser.cjs。
- [x] 原样执行 scope suite、三视口 UI fixture、launcher 守护测试，保存命令/退出码/资产摘要。
- [x] 若有测试信封漂移或恒真断言，先验证对应错误会被漏检，再以最小改动修正并复跑；不改产品实现。
- [x] 报告实际断言范围、失败/缺口和依赖，不冒称真实服务验收。

## Task 2: Real-service browser incremental acceptance
**Files:** tools/b7-manual-fixture-browser-smoke.cjs（按需最小强化）；tools/b7-manual-fixture-seed.sql；必要时 tools/b7-manual-fixture.py；新增 tests/product/b7-real-service-acceptance.cjs（如有必要）。

执行中确认旧 fixture 未种 runtime credential 且账号 owner 与 resolver 不符，已保留404失败。仅补测试身份数据与当前授权链一致，不修改服务端鉴权或账号真实能力。
- [x] 在新 PostgreSQL16/真实 binary/file resolver 上执行现有 smoke，核对 schema/build/种子及结果。
- [x] 补浏览器触发的真实通知 CAS409、空态与权限错误；API 分页及 read:false 只作为辅助证据。
- [x] 以合成 >50 数据证明现有 UI 分页是否可消费；如不支持，记录已确认功能缺口，不让预期缺口伪装成通过。
- [x] 记录页面截图/网络/数据库零外部执行事实；finally 停止并读回清理。

## Task 3: Review, evidence and pull request
**Files:** docs/product-acceptance-matrix.md；docs/superpowers/plans/2026-11-04-b7-browser-acceptance.md。
- [x] 主代理审完整 diff/原始日志、抽查截图、独立复跑适用回归，区分作者自测与独立执行。
- [x] 矩阵只追加本轮范围/commit/命令/限制；保留历史不覆盖，无虚假完成百分比。
- [ ] 提交推送单一目的 PR，等最新 head CI；无本轮明确授权不合并。

## 执行结果索引

主代理独立执行 scope21、三视口UI39、Python product18通过；全新真实服务fixture smoke13和调查11完成，复现通知迟到P2、消息/通知分页P2、headerP3四项问题，M1仍PARTIAL、通知导航FAIL。schema26/seed3/6/2/2/零外部执行事实/清理均读回。原样404及环境失败保留，不改写为通过。详细范围见验收矩阵2026-11-04浏览器增量节；动态PR/CI状态见本轮PR与本机证据索引，不在本计划伪造永久完成状态。
