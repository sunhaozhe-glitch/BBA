# 全产品 capability-matrix

ARTEX v0.3.24 e740334b9042d6da64156b0adb37a293117e140e；源前缀 web/src/app/(main)/。所有未实现范围继续列入后续验收，不删除。

|页面|ARTEX源码/交互|BBA已有API/边界|本批新增|后续缺口（未完成）|实际测试|
|---|---|---|---|---|---|
| 仪表盘 | dashboard/page.tsx:指标/Token图/任务/资产/系统 | ListTasks,ListAccounts,model_request | 聚合API、QQ状态、未知null | CRM/资产/事件健康/全模型账本 | 数据库聚合、API、三屏 |
| 对话 | chat/page.tsx:线程/上传/历史/发送/停止/置顶/删除 | chat trigger/context/permit非完整收件箱 | 双标签占位 | Agent流式停止历史；QQ账号群私聊隔离、草稿、最终出站门 | 仅导航 |
| 任务 | function/tasks/page.tsx及detail:创建/控制/分类/归档 | GET tasks/详情；旧控制graph/proposal | 分页只读详情；修复详情UTC | 群分析/聊天/跟进/好友类型、图报告、新控制页 | 列表详情一致、UTC回归、三屏 |
| 计划任务 | function/schedules/page.tsx:CRUD/启停/立即运行 | 确定性worker不是用户计划API | 导航占位 | 持久时区/下次运行/历史/补偿/重叠/去重 | 仅导航 |
| 资产 | function/assets/page.tsx:搜索/公司scope/增删 | 任务事实图不是CRM | 客户定义占位、明确不可用 | QQ唯一、意向证据、规则版本、好友事实、修正历史 | null断言；CRM未测 |
| LLM | system/llm/page.tsx:profile保存/激活/测试/模型/池 | 复用规划网关/OpenAI适配器/runtime | B1多配置CRUD、保护文件持久化、独立写权限、密钥三态、版本并发、workspace默认路由、连接测试与真实规划消费 | Anthropic/Responses、模型枚举/轮询熔断、代理/RPS/RPM、全局金额预算、Agent/Skill显式绑定尚未完成；非完整ARTEX覆盖 | 本地TLS provider真实HTTP消费/重启/幂等/越权/隔离/超时/并发；三屏真实CRUD；无付费调用 |
| Agent | system/agents/page.tsx及detail:创建/删除/绑定 | 规划/Character边界 | 导航占位 | 主Agent规划/调度/表达分工、绑定版本 | 仅导航 |
| MCP | system/mcp/page.tsx:保存/刷新/工具/可见性/删除 | 已有隔离Summary链 | 导航并区分插件 | MCP与Koishi插件双类管理、受限封装 | 仅导航 |
| Skill | system/skills/page.tsx:文件目录CRUD/上传/usage/可见性 | 无完整运营Skill API | 六首批流程需求保留 | 中文schema/目标输入依赖步骤输出停止条件版本/启禁/绑定 | 仅导航 |
| 工具 | system/tools/page.tsx:目录/保存/重置/自定义/测试/绑定 | 既有能力投影/受限adapter | 180原名目录、搜索分类、全部未启用 | 参数schema/实例支持/持久启禁/调用历史/最终输出门 | 鉴权、原名、阻断类、三屏检索 |
| 通知推送 | system/notify/page.tsx:渠道CRUD/测试 | 无完整通知管理API | 导航占位 | 事件订阅/脱敏/投递结果/失败重试/配置消费 | 仅导航 |
| 拦截规则 | system/intercept/page.tsx:规则CRUD/启禁/judge/tool配置 | 旧chat permit边界保留 | 导航占位 | 所有最终payload统一审核、分组优先级试跑版本、媒体未知拒放 | 旧gate/permit回归；新功能未测 |
| 审批记录 | system/intercept/approvals/page.tsx及组件 | 旧任务/提案授权契约 | 保留契约，文案为内容审核 | 策略兼容迁移、误报修订重检、硬阻断证据 | 旧审批回归；新页未完成 |
| 日志 | system/logs/page.tsx及组件 | 任务运行证据非统一产品API | 导航占位 | 运行任务工具模型内容日志、关联ID筛选分页脱敏保留 | 仅导航 |
| 系统配置 | system/settings/page.tsx:setSettings | runtime只读/旧账号会话 | 导航占位，未改登录 | 代理scope/内网bypass/搜索/并发/时区/预算/频率/备份权限/紧急暂停 | 仅导航 |
