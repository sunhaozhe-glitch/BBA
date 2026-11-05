# ARTEX Phase A：单一前端构建与既有 Go 只读产品面

> **2026-11-05 补充/取代：** 本文保留 Phase A 的前端来源、许可、实现和验证事实。“指标诚实性”中“确认意向+至少一运营QQ好友事实”的后续产品条件由[根规则](../../AGENTS.md)及[有效 CRM 设计约束](bba-final-closeout-2026-10-31.md#crm数据与指标设计约束待实施)取代：明确意向即可计数，好友关系独立记录。缺少 CRM 数据源时仍显示 unavailable/null，文档修订不产生客户能力。前端移植不是 ARTEX 核心规划复用结论；历史无 Go 源码复制不限制待调查确认的后续方案。

状态：已实施，待 Astra 独立亲跑审核。不是生产切换批准。

## 来源与取舍
依据 astra-handoffs/bba-artex-confirmed-framework.md；基准 ARTEX v0.3.24 / e740334b9042d6da64156b0adb37a293117e140e。镜像标签相符不证明构建字节一致。没有访问或绕过 ARTEX 登录。

复用 web/src/app/globals.css 中性明暗主题、Card 容器、sidebar-items.ts 功能/系统分组、dashboard/page.tsx 保留排布局；移植为无运行时依赖的 HTML/CSS/JS，而不是把 Next/React 全应用及渗透任务执行器搬入。frontend 是唯一新增前端源码目录；tools/build-product.py 校验清单并复制资产至 backend/cmd/taskengine-server/product，由 go:embed 编译。它是真实产品前端、调用既有 Go 服务，不是独立 Python 预览或第二任务后端。代价：非 React 组件二进制复用，复杂后续页可能需要将这一个目录升级为带构建器的组件应用；不能再叠加第三前端。

原 /、/app.js、/app.css 控制台与全部旧任务契约保留。新 /product/dashboard、/product/tasks、/product/tools；其余完整导航对应占位页面明确待接入。深链接由产品路由返回同一外壳；/product/*.json 不公开注册源。认证后的 GET /v1/operator/tools 才提供白名单目录。CSP 保持同源，不启 CORS，不加载 CDN。

## 聚合与权限
GET /v1/operator/dashboard 复用 trusted resolver Actor、operator.read + task.read、ListTasks、ListAccounts 与 core.model_request.response.usage。task.manage_all 仍按旧契约控制任务范围；普通操作员仅自己的任务。账户旧接口是 workspace 投影，不假称已经有逐操作员账号授权。默认 projection 仅 workspace manager 可见；普通操作员返回 ACCOUNT_SCOPE_MAPPING_REQUIRED，不返回全量账号。

可选 -dashboard-account-source local6300 -dashboard-account-mapping /absolute/0600-mapping.json。配置只接受 bindings：workspace_id/principal_id/account_id/instance_id，且目标 account 必须已经存在同 workspace，self_id 已核实并与 overview.uin 完全一致。管理员也必须显式映射，不提供“admin 看全部 6300”的捷径。配置拒绝 URL、token、未知字段、重复映射和宽松文件权限。无映射不发请求。固定唯一 GET http://127.0.0.1:6300/api/overview；禁代理/重定向/凭据或入站头转发；2秒超时、1MiB响应上限；只解析 id/uin/online/phase/flag，并只返回账号/实例/QQ/状态/原因/采样时间。nick/face/quicklist/token/端口均不透出。浏览器永不收到 NapCat/6300凭据；现有BBA bearer仍由操作员在页面内存提供，不存储。

mapping 示例（均为 fixture，不要直接映射真实用户）：
{"bindings":[{"workspace_id":"ws_fixture","principal_id":"principal_fixture","account_id":"acc_fixture","instance_id":"instance_fixture"}]}

默认心跳有效期90秒，缺失/过期/未来异常都不能算在线。实例在线/离线/异常互斥，合计=授权注册实例数量；同一已知QQ多个实例全部异常，另给 unique_known_accounts/conflict_instances，客户数绝不以实例数替代。无服务缓存，cache_age_seconds=0，observed_at标采样时刻；6300不提供观测时间，因此采样成功不保证内部状态新鲜，不能声称完成真实账号运行健康验收。上游失败不静默回退旧在线数字。

## 指标诚实性
客户与资产无满足“确认意向+至少一运营QQ好友事实”的真实CRM源，返回 availability=unavailable / reason / value=null。规则未配置、未启用。不是永久不可用设计：B/C阶段补规则版本、证据、关系及客户去重投影。
Token 仅当前操作员已完成规划网关 response.usage；无记录或缺字段返回 null，缓存读写未知；不是所有ARTEX/模型总账。日图按模型请求创建UTC日期，不伪称付费精确计费。任务活跃范围明确为 RUNNING/WAITING_EXTERNAL/VERIFYING/PAUSING/RECONCILING。

## 工具和内容门
原始180条清单逐三行解析，保留 .ocr_image / .handle_quick_operation / ArkShareGroup 大小写标点。180唯一、0重复、4组仅候选别名不合并；参数schema与实例支持均null/未验证，全部enabled=false。分类搜索只读。credential类、send_packet和quick_operation标阻断，本批没有任何调用路由，不能用目录当执行授权。后续最终出站审核、媒体不可覆盖处理仍是完整验收项。旧审批不删除、不放宽。

## 许可与发布
frontend/NOTICE + licenses/ARTEX-AGPL-3.0.txt + licenses/ARTEX-web-MIT.txt 保留归属和两份许可。上游根是AGPL，web声明MIT，不能宣称整个仓库MIT；逐文件覆盖与组合分发/网络源码义务仍需Astra发布前审核。本批未复制上游Go实现，未正式部署。

## 验证与回滚
独立 evals/results/sol-artex-phase-a-* 保存源码备份、前后SHA、真实退出码、三屏截图、API响应和能力矩阵。只在专属dev-stack fixture数据库迁移/写草稿及用量记录，未连接QQ写适配器。资源按本轮实例名清理。回滚仅按 changed-files.json 与 backup/恢复被改文件、移除本轮新增文件；不得覆盖用户后续变更。
