# BBA

BBA 面向 QQ AI 运营：以 Go Task Engine 和 PostgreSQL 为执行与业务事实底座，提供同源控制台，连接按账号隔离的 Character/Collector 运行时，由 Agent 生成受权限和审批约束的建议与任务，并逐步形成有证据的客户资产闭环。

这是产品目标，不是全部功能完成声明。当前已有任务、许可、Bridge、恢复、模型与配置、分析和对话/草稿/通知等不同范围的实现及离线证据；计划任务、CRM、完整多步 Agent 和工具运行时仍需逐项建设或验收。CI 通过不等于真实 QQ 能力或生产发布通过。

## 开发入口

先读 [AGENTS.md](AGENTS.md)，再核对当前源码、[架构决策](docs/adr/)、[产品验收矩阵](docs/product-acceptance-matrix.md)及[任务类型矩阵](docs/qq03-task-type-matrix.md)。历史矩阵可能落后于源码，状态须绑定具体提交与证据范围，不以菜单、模型回复或历史 PASS 判断完成。

| 目录 | 内容 |
|---|---|
| `backend/` | Go Task Engine、主服务、独立 Task Bridge、PostgreSQL migration、受控 ARTEX adapter |
| `frontend/` | 产品控制台唯一源码，同步到主服务 Go embed 资源 |
| `contracts/` | OpenAPI、schema、正反 fixtures；Go/TS 生成类型在对应模块 |
| `koishi/character-runtime/` | 按账号隔离的交互运行时与单次聊天许可 gate |
| `koishi/analysis-collector/` | collection-only 采集、持久窗口与 Summary |
| `plugins/`、`patches/` | NapCat 适配候选，存在不代表已部署或能力已验证 |
| `tools/`、`tests/`、`docs/` | 隔离验证、构建工具、测试和设计/验收文档 |

任务沿用 Reserve/permit/Bridge/observation；日常 Chat Permit 保持独立，不能把所有聊天强套加群任务链。Agent、Skill、Summary 和搜索内容都不授予 QQ 权限，UNKNOWN 外部结果不自动重发。

## 验证

依赖 Docker、Python 3（合同工具使用 jsonschema、PyYAML）。Go 版本查 `backend/go.mod`；Node 与各包依赖查 package/lockfile，当前根 engines 要求 Node 22.12.0+。

从仓库根运行适用于本次变更的验证：

```sh
# 缓存缺失且需要时显式联网预取
bash tools/contract.sh prefetch
# 契约检查，不生成文件
bash tools/contract.sh check
# 隔离 PostgreSQL Go race suite
bash tools/kernel.sh test-all
# 主服务与 schema gate 的隔离 smoke
bash tools/kernel.sh smoke
bash tools/kernel.sh schema
```

Go 测试使用项目 harness，不能裸跑 `go test ./...` 绕过隔离环境或掩盖缺 DSN 跳过。

Character（Node 22）：

```sh
cd koishi/character-runtime
npm ci --ignore-scripts --no-audit --no-fund
npm run build
npm run typecheck
npm test
```

Collector 是独立包，须按其 package scripts/lockfile 单独验证；Character 通过不代表 Collector 通过。修改 UI 后从根运行 `python3 tools/build-product.py` 并核对嵌入资源，不能只改生成副本。

产品 fixture、verify 和 release-check 的入口见 [AGENTS.md](AGENTS.md) 与 [发布 runbook](docs/product-release-runbook.md)。先核对当前 binary/schema，不照搬历史版本号。fixture 只使用隔离资源，不能传入生产 DSN。

## 交付规则与路线

所有开发从最新远端主分支建立独立分支，通过 GitHub PR 提交；最新 head 的实际 CI 和必需检查通过、满足审查/保护规则并具备适用合并授权后才合并。禁止直改主分支、管理员绕过、跳过或忽略失败检查。合并后核实远端并同步本地。

CI 定义见 [.github/workflows/ci.yml](.github/workflows/ci.yml)。当前 `contracts`、`backend`、`character` 作业不代表已覆盖 Collector、全部浏览器链路或完整产品验收；修改涉及这些路径时补充相关验证。

路线按有界切片推进：当前基线校准 → 双对话基础收口 → 多账号/任务产品化 → 计划任务 → 客户 CRM → Agent 运营 → 工具/MCP → 整合与发布。各动作的最小能力/授权依赖可提前，完整多步 Agent 不作为基础收件箱交付的隐含条件。Dashboard、通知和恢复验证随数据域同步推进。阶段目标与现状分别记录。

## 外部范围与许可证

源码和离线验证不授权登录 QQ、发送消息、加群/好友、写真实 NapCat/ARTEX、连接生产数据库、调用付费模型、修改运行目录或切换 writer；真实动作遵守对应范围的明确授权、停止条件及独立验收。

公开仓库不提交凭据、生产数据/备份、运行二进制、依赖缓存和本地历史证据包。公开材料使用合成或脱敏个人标识。客户证据、停止联系与数据生命周期要求见开发指南，不能以审核通过冒充发送成功。

前端包含 ARTEX 来源与许可边界，详见 [frontend/NOTICE](frontend/NOTICE)。各组件遵守其许可证，不能将整个仓库笼统宣称为 MIT。
