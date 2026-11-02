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

前两项分别重新生成资源和检查 JS 语法；第三项检查工作树相对 index 是否发生变化，不是独立业务验收。已在本次同步中实际执行，退出码均为 0。构建 Go 二进制和完整业务测试参见 docs/taskengine-development-console.md；本次文档同步未重新构建 Go 服务或重跑业务套件。

## 打开现有本机实例

当前恢复的本机开发实例地址：

    http://127.0.0.1:18451/product/

实例状态目录为 `build/dev-stack/artex-ui/`，登录令牌文件为其中的 `console-token`。令牌不提交、不写入 URL、不保存进文档。数据库为独立 `bba-dev-stack-artex-ui-db`；只启动此开发实例不等于授权迁移、worker、QQ 登录/发送或真实模型调用。

通用隔离开发栈的启动/停止及凭据管理见 docs/taskengine-development-console.md。该脚本默认启用 fixture worker，不能把它当成无 worker 的正式服务启动命令。启动前核对端口、数据库版本、provider 配置和授权范围；不要对已有数据库执行 reset 或自动迁移。

## 验收边界

本次只核对源码已入库、嵌入资源一致和入口说明；不是完整功能交付或上线批准。各模块接线和能力以当前 API、测试及能力矩阵为准。运行配置、令牌、数据库、日志、build 和历史截图不属于 UI 源码同步范围。
