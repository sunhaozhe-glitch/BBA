# B7 Existing HTTP Contract Completion Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [x]`) syntax for tracking.

**Goal:** 为已装配的 B7 八个 HTTP 操作补齐与当前实现一致的 OpenAPI、Schema、正反 fixtures 和可重复契约验证。

**Architecture:** 沿用现有 OpenAPI 3.0.3、本地 Schema、ContractLoader、Go/TS 生成管线。只固化当前 wire 行为；不调整业务、数据库、权限或前端。独立分支基于 main `b801fbfde73083ff0fa11750b4e658f4ef8be4ec`。

**Tech Stack:** JSON 格式的 OpenAPI/Schema、Python unittest/jsonschema、Redocly、openapi-typescript、oapi-codegen、隔离 Go/PostgreSQL harness。

**Spec:** 用户已批准本对话中的有界范围（2026-11-04“同意 开工”）：现有 8 操作、正反样例、单独 PR。源码事实以 `backend/internal/taskengine/operator_conversation.go`、`backend/cmd/taskengine-server/resolver.go` 及 migration 为准；架构约束以根 `AGENTS.md` 为准。

## Global Constraints

- 最多 2 个子代理并发；子代理实施，主代理审核与独立执行。
- 不改业务 Go、migration、前端、依赖版本或权限；只允许生成器修改 `types.gen.*`。
- 只描述 Bearer 装配，区分 operator 与 runtime，不声称 cookie/CSRF、QQ writer 或 Agent run 已接线。
- 现有 JSON 数字 version/seq/rules_version 是有界遗留差异，不默改为字符串；请求体 UTF-8 字节长度不能用 JSON Schema 字符长度冒充精确约束。
- 不复用不匹配的公共 cursor、ResourceId、幂等头或错误响应定义；先核对真实校验。required/nullable、数字、未知字段及可省略字段以 handler 解码/持久源为准。
- schema refs 仅受限本地引用；全部新增命名 component 有正反 fixture，fixtures 不能被当真实执行证据。
- 不连接现存数据库/真实 QQ/模型/ARTEX，不部署、不自动合并；保留本机忽略证据。

## 文件与接口分工

- `contracts/openapi.yaml`：八个 operation、精确参数/权限/状态及 description，注册必要组件。
- `contracts/schemas/operator-conversations.json`：B7 请求/响应/分页/错误/实体 Schema。
- `contracts/examples/operator-conversations.json`：所有新增组件正反 fixtures，明确数据合成。
- `tests/contract/test_operator_conversation_contract.py`：独立维护操作接线、精确结构/nullable/数字/未知字段/重放与分页边界回归；不只读取自生成样例得出正确结论。
- `backend/internal/contract/smoke_test.go`：新增命名组件的既有 Go fixture 类型注册，另保留历史 evidence 的 null/多形态/大整数编解码精度回归。
- 生成产物：`backend/internal/contract/types.gen.go`、`koishi/contracts/types.gen.ts`，只经 `tools/contract.sh generate`。
- 根 `AGENTS.md`、`docs/product-acceptance-matrix.md` 与本计划：稳定契约入口、范围和证据边界。

共享命名接口（`operator-conversations.json#/...`）：`OperatorConversationPageEnvelope`、`OperatorMessagePageEnvelope`、`OperatorAgentMessageRequest`、`OperatorAgentMessageEnvelope`、`OperatorQQDraftRequest`、`OperatorQQDraftEnvelope`、`OperatorQQDraftDetailEnvelope`、`OperatorQQInboxEventRequest`、`OperatorQQInboxEventEnvelope`、`OperatorNotificationPageEnvelope`、`OperatorNotificationReadRequest`、`OperatorNotificationReadEnvelope`、`OperatorConversationErrorEnvelope`。可增加必要实体，不复制 Go 实现逻辑。

八操作及建议 operationId：
- GET `/v1/operator/conversations` — listOperatorConversations
- GET `/v1/operator/conversations/{id}/messages` — listOperatorConversationMessages
- POST `/v1/operator/agent-threads/{id}/messages` — appendOperatorAgentMessage
- POST `/v1/operator/qq-drafts` — createOperatorQQDraft
- GET `/v1/operator/qq-drafts/{id}` — getOperatorQQDraft
- POST `/v1/operator/qq-inbox/events` — ingestOperatorQQInboxEvent
- GET `/v1/operator/notifications` — listOperatorNotifications
- POST `/v1/operator/notifications/{id}` — setOperatorNotificationRead

### Task 1: 契约回归护栏（并行测试作者）

**Files:** create `tests/contract/test_operator_conversation_contract.py` only。
**Consumes:** 上述命名、真实 Go handler/resolver/migration；不依赖 Task 2 作者结论。
**Produces:** 先 RED 的结构与边界契约测试，后由 Task 2 实现通过。

- [x] 阅读真实入口和持久结构，独立列出八操作契约 oracle。
- [x] 写有意义的测试：路径/方法/请求响应引用/真实 auth/状态；required/nullable、未知字段、数字/不同回执、分页和不伪造许可。
- [x] 对缺失契约运行新测试，保存业务 RED（缺 schema/路径的失败，不把语法/依赖错误当 RED）。
- [x] Task 2 完成后检查 GREEN；代码/样例变化不能削弱 oracle。

### Task 2: 契约、样例、生成类型及交接文档（实施作者）

**Files:** contracts 三文件、两个生成类型、`backend/internal/contract/smoke_test.go` 组件注册与精度回归、AGENTS、矩阵；不改 Task 1 测试。
**Consumes:** 共享命名接口、当前 handler/resolver/migration；Task 1 测试为验证消费者。
**Produces:** 真实行为的完整八操作合同及可消费生成类型。

- [x] 阅读实际请求、成功/错误响应、路由/鉴权、数据约束和现有契约工具要求；先记录接口映射。
- [x] 实现 OpenAPI 和 Schema；对 UTF-8 字节限制、数字协议、header/字段缺省、cursor 行为使用准确说明，不编造能力。
- [x] 添加所有新增组件的正反 fixtures，含空/nullable/冲突/只读许可等实际状态。
- [x] 运行 `bash tools/contract.sh check` 与新回归；再运行 `bash tools/contract.sh generate`，保留必要产物，重复生成应无新增差异。
- [x] 以实际已验证范围更新稳定文档，不把合同/样例 PASS 当 live 或完整 M1 验收。

### Task 3: 主代理集成审核、独立验证与 PR

**Files:** review artifacts only；需要修正交回对应作者。
**Consumes:** Tasks 1–2 代码与作者报告。
**Produces:** 经审核的有界 PR、最新 head CI、审查与证据。

- [x] 核对完整 diff、每个操作与 Go 实现的一致性、无权限/业务回退、无凭据、许可证/生成同步。
- [x] 独立运行 contract check、生成及 TS/Go smoke；核对重新生成无漂移。
- [x] 用实际 binary/resolver/新隔离 DB 抽取八操作真实 HTTP 请求响应与错误，按新增契约验证；覆盖空/非空、分页、replay、CAS/权限负向。原始响应只在本机脱敏 fixture 证据。
- [x] 独立源码审查发现问题后交作者修正并重验；不扩成业务改造。
- [ ] 提交/推送新分支、创建 PR、核对最新 head 三项 CI/实际 reviews/治理；待本轮合并授权。（此项发生在提交后，动态结果以 PR 与忽略证据为准，不为勾选另造提交。）

## 基线与完成状态

现有 Python contract suite 和依赖准备已退出 0；完整 baseline check 单独记录。所有动态执行结果保存 `evals/results/m1-b7-contracts-20261104/`，按未提交输入摘要/最终 commit 绑定。开始时本计划条目均未核销；完成后依证据更新。

## 本轮实施与证据绑定（2026-11-04）

- 三角色分别完成测试、契约实施、独立源码审查，主代理独立运行；最多两并发。基线完整 contract check exit0，缺路径/Schema回归先RED后GREEN。
- 初审两项缺口为文本错误媒体与本地时区，均保留历史记录。最终审查又发现 text/plain 仍用对象 Schema，主代理复现并修正 probe；新增单项测试 RED（8个实际失败、0errors）后，将媒体实例改为字符串，另校验解析后 JSON。限定复核已通过，无遗留源码阻断。
- 最终独立 `bash tools/contract.sh generate` exit0：83 Python tests，Go/TS 239个有效 fixture编解码/类型 smoke，重复生成输入摘要无变化。旧Go 1261声明无删改、新增181；旧OpenAPI路径与组件语义保持。
- 最终独立 `TMPDIR="$PWD/build/test-tmp" bash tools/kernel.sh test-all` exit0，运行期间输入摘要保持一致（部分package无测试）。
- 最终实际 binary/resolver + 新PG head26，显式 TZ=Asia/Shanghai：八操作56 HTTP／268断言 PASS；按实际媒体解码实例验证，再单独验证plain文本内JSON；中文样例明确总JSON<16KiB且文本UTF-8>8192，实际返回400。包含分页、权限、精确201重放、inbox201/200/撤权、read CAS/缺省null、历史JSON null/数组/标量、时区。执行侧许可/task/model计数为0，临时容器/服务/端口清理通过。
- 原始自测与早期独立证据保留各自输入，不以旧PASS替代最后修订。最终记录为 `evals/results/m1-b7-contracts-20261104/independent-*-fix1.json/log`、`evals/results/m1-b7-contracts-http-final-fix1-20261104/result.json`；未提交输入总摘要 `ce281054c899994ccf19deae0cc59385a12c1bffd89b74b44501cdfbd2266426`（backend/contracts/tests/tools/koishi合同，不含此计划文档）。提交后须核对该摘要仍一致。
- 未部署、迁移现存库或调用真实QQ/模型/ARTEX。浏览器/完整M1/多步Agent/发布未核销；`NOT_RELEASED`。TS数字精度、时间pattern非完整日历校验、UTF8字节需执行层校验等限制保留。
