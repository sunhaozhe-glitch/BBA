# BBA

BBA 是围绕 QQ 账号、任务编排、审计和受控运行时构建的系统，包含 Go Task Engine、内嵌控制台、OpenAPI 契约和隔离的 Koishi 子模块。

开发前请阅读 [AGENTS.md](AGENTS.md)，并以 [产品验收矩阵](docs/product-acceptance-matrix.md) 判断功能范围。源码导入和 CI 通过不代表生产部署或真实 QQ 操作已获授权。

## 开发与验证

依赖 Docker、Python 3（jsonschema、PyYAML）以及 Node.js 22。合同和 Go 验证使用容器与隔离 PostgreSQL fixture：

```sh
bash tools/contract.sh prefetch
bash tools/contract.sh check
bash tools/kernel.sh test-all
```

Character 子模块使用 Node.js 22：

```sh
cd koishi/character-runtime
npm ci --ignore-scripts --no-audit --no-fund
npm run build
npm run typecheck
npm test
```

所有变更通过独立分支和 GitHub PR 提交，须在 CI 通过且满足审查规则后合并。CI 定义见 [.github/workflows/ci.yml](.github/workflows/ci.yml)。历史验收日志、token、数据库与运行二进制保留在本地，不导入公开仓库。部署与外部操作仍须遵守项目授权边界。
