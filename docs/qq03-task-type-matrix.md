# QQ-03 任务类型支持矩阵（离线工程切片）

状态：本轮为工程实现 + 隔离 fixture 证据；作者自测，待独立验收。范围仅代码、迁移、契约、前端与离线测试；未登录、未发送、未加群/加好友、未调用付费模型、未触碰生产 DB 或真实运行目录。

## 决策矩阵

| 任务类型 | 识别/创建 | 持久化与读回 | 生命周期/执行 | 依据 |
| --- | --- | --- | --- | --- |
| `group.acquire` | 已接线 | 已接线 | 既有执行链原样保留（既有测试覆盖）| 既有契约与引擎语义未改 |
| `group.analyze` | 已接线（本批 dry_run/read_only） | 已接线 | 执行明确 unsupported：仅识别与读回；分析执行依赖现有 Summary/Collector 能力证据，不真实登录或群访问 | `docs/capabilities/P0-02.md`；Task Engine 无分析动作族执行链 |
| `chat` | 已接线 | 已接线 | 执行明确 unsupported：任务对象仅识别与读回；日常聊天仍走独立 chat permit 运行时链，两者不混用 | chat permit 链是单独发送边界（稳定设计约束） |
| `customer.add_me` | 已接线 | 已接线 | 执行明确 unsupported：无已证实的好友动作 adapter 能力；不猜测真实好友申请 API、不触发写 | `docs/capabilities/P0-02.md` 无好友动作路由；未知即 fail closed |
| `customer.add_customer` | 已接线 | 已接线 | 执行明确 unsupported：同上 | 同上 |
| 未列出类型 | 拒绝（VALIDATION_FAILED） | 不持久化 | 永不执行 | 封闭枚举契约 |

## 行为规则（本切片实现）

- 类型识别、创建（DRAFT）与读回对全部五类可用；读回显示真实持久化类型。持久化决策：类型存在 `core.task.request_payload` jsonb（schema 0001 起存在、契约必填成员），读回层从 canonical payload 派生——不新增列、无新 migration（head 保持 25），代码在任意受支持 schema 层级可读写，旧行语义零变化；类型封闭枚举由契约 schema 与服务层校验强制（与既有 DB 无类型 CHECK 列的现状一致）。
- 仅 `group.acquire` 保留既有执行链；其余四类的 submit 在校验通过后、任何行写入之前被明确拒绝为 `CAPABILITY_UNAVAILABLE`，零写副作用（无 task/execution/reservation/outbox/permit 链行）。
- 四类新类型：mode 仅 dry_run/read_only（live 无执行链，拒绝）；join_reason/verification_message 为契约信封占位成员，必须为空；approval_ref 成员必须存在且为 null（审批只绑定 request_join 动作族）；goal 信封固定 count=1、两 flag 维持 schema 锁定的 true；candidate_source 仍为 closed `fixed` 导入，恰好一个候选（分析/聊天目标群或客户 QQ 标识）；keywords 与搜索页限制保持关闭。
- unknown 不自动重发：新类型从不 dispatch，也就没有可重发的外部动作；`group.acquire` 既有 UNKNOWN 语义未改。
- 跨 workspace：读不可见（RESOURCE_NOT_FOUND）；跨 workspace 账号 fail closed（VALIDATION_FAILED）。

## 为什么不扩大 live path

`docs/capabilities/P0-02.md` 证据边界：cleaner adapter 无好友/聊天/分析动作路由；`search_groups`/`request_join` 均 unverified。把未证实能力接进执行链会把 fixture 成功误报成 live 能力（已知易踩坑 #4）。因此 customer.add_me / customer.add_customer / chat / group.analyze 停在明确配置的 unsupported，仅 group.analyze 未来接线时依赖现有 Summary/Collector 只读能力证据。

## Fixture 与 live 边界

- 全部断言来自 `tools/kernel.sh` 隔离 PostgreSQL 16 fixture（TDD 测试文件 `backend/internal/taskengine/qq03_task_types_test.go`）与 `tools/contract.sh` 离线管线（generate 经项目 generator 重写 `types.gen.*`，无手改）。
- 本切片未证明任何真实 QQ 能力；矩阵“已接线”列指代码/契约/fixture 层。

## 未覆盖风险 / 下一批依赖

- 四类新类型的执行链本身（含 group.analyze 与 Summary/Collector 的接线、chat 与 permit 链的任务化桥接、好友动作的真实 adapter 验证）未实现，属明确 unsupported。
- UI 支持矩阵为展示层常量；运行期 per-account 能力仍走现有 `/v1/capabilities` 投影，未新增运行期联动。
- 本切片不做计划任务（QQ-04）与资产 CRM（QQ-05）；QQ-04 依赖本切片的类型持久化（request_payload 派生）与封闭枚举，调度语义（时区、下次执行、重启补偿、重叠幂等）在下一批。
- 独立验收需复核：持久化决策（request_payload 派生，无新 migration，head 25 未动）、`bash tools/contract.sh check` 与 generate 输出、`bash tools/kernel.sh test-all` 原始日志、前端源码与嵌入副本一致性。历史交接曾报告旧版 AGENTS 的 head 数字滞后；该陈述已在 PR #2（`4d648beed7e7be07ee0c738aed0456790cce267b`）移除，不再是待处理缺陷。本矩阵的 head 25 是 QQ-03 切片基线快照，不是未来当前值；涉库工作按 AGENTS 第 7 节重新核对源码、binary 和数据库。
