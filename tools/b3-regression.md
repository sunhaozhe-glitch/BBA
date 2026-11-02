# B3 UI / B1+B2 可重复回归

在 BBA 根目录运行：

```sh
# 不需要数据库或真实模型：源码/嵌入会话 + B3 三屏 UI + B2 自测
python3 tools/b3-regression.py

# 完整 B1、B2：各自新建隔离本地服务与 PostgreSQL，并使用本地 TLS 假模型
python3 tools/b3-regression.py evals/results/my-unique-b3-run --integration all
# 可分别选择 --integration b1 或 --integration b2
```

输出目录必须位于本项目 `evals/results/` 下且未存在，防止旧证据混入。默认按 UTC 创建独立目录。需 Node、Python、现有 Playwright 安装；浏览器默认 `/opt/google/chrome/chrome`。完整集成另需 Docker、缓存的 postgres:16-alpine/golang:1.26 镜像与 Go 离线依赖、OpenSSL。拒绝继承 `BBA_TASKENGINE_DSN`；不要配置生产凭据或使用生产服务。

## 执行内容与退出码
- 检查前端及测试 JavaScript 语法；运行现有 `build-product.py`，同步三份生成静态资产并比较 SHA-256，不修改后端业务逻辑。
- 源码、嵌入 app.js 各跑会话回归；源码、嵌入完整静态资产各跑 B3 UI。
- B3 UI 在 1440/768/375 宽度测试 CRUD、expected_version/409、只读、历史、试跑、审批筛选/分页/说明、关闭/切页/换会话后的迟到响应和旧 401、错误态、敏感 DOM 清空、凭据不持久化、无横向溢出。
- B2 独立夹具具有必选的本地假模型配置，模型不执行调用。完整集成保留现有 B1/B2 安全断言与真实 API、TLS 假模型消费、幂等及重启持久化检查。
- 每一步保存独立 `.log` 与 `exits.json`；任何子检查非零、构建失败或源码/嵌入散列不一致，合并 runner 最终返回 1。不会因后续通过掩盖早先失败。
- 完整集成各保存 `cleanup-verification.json`，读回容器/卷不存在、自有服务停止和三端口关闭；清理失败同样传播。

## 证据边界
`b3-content-browser.cjs` 显式拦截浏览器请求并使用内存 API 夹具；它证明 UI 状态与请求行为，不证明 B3 后端、规则引擎或实际发送门。截图来自真实 headless Chromium。B1/B2 完整集成使用真实本地 backend+数据库，但模型仅为本地假服务，无付费调用。此 runner 不代表 B3 最终发送门完成，不代表 Astra 独立验收，不扩下一批功能。

主要证据：`summary.json`、`asset-hashes.json`、`exits.json`、`session-{source,embedded}/assertions.json`、`b3-ui-{source,embedded}/assertions.json`、每宽度请求追踪和 PNG、`b{1,2}-integration/integration-summary.json` 与清理读回。`--integration none` 的通过不意味着 B1/B2 完整集成已经执行。
