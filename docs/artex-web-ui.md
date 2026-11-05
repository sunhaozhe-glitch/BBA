# ARTEX 风格 BBA Web UI

用户确认的产品界面是深色「BBA · QQ 私域运营控制台」，由主服务提供 `/product/`；不是 astra-handoffs/ui-preview 的早期 Python 七页预览，也不是部署中的 ARTEX 本体。

## 源码与 GitHub 同步

唯一产品源码：`frontend/index.html`、`frontend/app.css`、`frontend/app.js`。
许可证及来源：`frontend/NOTICE`、`frontend/licenses/`。
工具目录源：`frontend/napcat-initial.txt`。

执行 `python3 tools/build-product.py` 将页面资源同步到 `backend/cmd/taskengine-server/product/`，并生成 tools.json。主服务通过 Go embed 打包这些资源，`backend/cmd/taskengine-server/product.go` 装配 `/product/` 路由。不要手改嵌入副本或再复制出第二套 UI。

这些文件已存在于 GitHub main 的基线 43c049c42d66f51bd1b59750d2750bccb8d46b66，也已位于本机 QQ群项目/BBA 目录。本次同步核对未发现缺失源码；无需重复导入。部署二进制可能是旧构建，页面可访问不证明它与当前源码一致。

## 构建核对

从仓库根执行：

    python3 tools/build-product.py
    node --check frontend/app.js
    git diff --exit-code -- frontend backend/cmd/taskengine-server/product

前两项分别重新生成资源和检查 JS 语法；第三项检查工作树相对 index 是否发生变化，不是独立业务验收。有意修改源码时，非空 diff 不能直接判同步失败：审查源码与 embed 两侧，再构建应无新增变化，并逐文件比对；无 UI 改动时两目录应无 diff。工具目录只代表未验证条目，disabled/schema unverified 不能当可执行能力。此处“退出码均为 0”仅保留原同步轮次记录，2026-11-05 文档统一本轮未运行资源构建或业务测试。构建 Go 二进制和完整业务测试参见 docs/taskengine-development-console.md；本次文档同步未重新构建 Go 服务或重跑业务套件。

## 打开现有本机实例

历史同步轮次记录的本机开发实例地址（2026-11-05 未核实监听、进程或可用性）：

    http://127.0.0.1:18451/product/

当时实例状态目录为 `build/dev-stack/artex-ui/`，登录令牌文件为其中的 `console-token`。令牌不提交、不写入 URL、不保存进文档。数据库为独立 `bba-dev-stack-artex-ui-db`；只启动此开发实例不等于授权迁移、worker、QQ 登录/发送或真实模型调用。

通用隔离开发栈的启动/停止及凭据管理见 docs/taskengine-development-console.md。该脚本默认启用 fixture worker，不能把它当成无 worker 的正式服务启动命令。启动前核对端口、数据库版本、provider 配置和授权范围；不要对已有数据库执行 reset 或自动迁移。

## 验收边界

本次只核对源码已入库、嵌入资源一致和入口说明；不是完整功能交付或上线批准。各模块接线和能力以当前 API、测试及能力矩阵为准。运行配置、令牌、数据库、日志、build 和历史截图不属于 UI 源码同步范围。

## 前端异步、交互与提交约束（2026-11-05 承接）

`frontend/app.js` 使用内存 bearer token、请求 AbortController 和 session/route/detail 序列；双对话另以 `chatSeq` 绑定页签、会话 ID 和页面代次，写表单还绑定 DOM 实例，拒绝迟到消息、错误、401 和保存回执污染当前视图。切换会话先清空旧消息，加载/失败不显示为空会话；忽略旧写回执不代表取消后端写入，不自动重试。通知读取另以 `notifySeq` 绑定列表代次，已读操作绑定发起页面的 session/route/list 及原按钮 DOM；成功重读立即作废旧列表，迟到成功/错误/401/finally 不得影响新页或新按钮。同按钮进行中禁止重复提交；超时/断连不自动重发，刷新或重入读取权威状态。修改异步 UI 时保留这些边界、HTML 转义与同源 CSP。验证真实 API、权限、空/错误/未知状态、分页、刷新和浏览器交互。新增 Collector/嵌入一致性/浏览器 CI 应单独提交，不能假定现有 workflow 已覆盖。

页面须说明真实 API、持久来源、权限和空/错误/未知状态；草稿确认不是发送成功，通知 `local_only` 不是外部推送。工具目录源与许可按 `frontend/NOTICE`、`frontend/licenses/` 保留，不用上游界面或目录数量证明 ARTEX planner 或 QQ 能力。只提交源码、必要生成资源和文档，不提交 dev-stack 状态、token、DSN、数据库、私有日志或依赖缓存。
