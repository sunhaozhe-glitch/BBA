# Task Engine Development Console

产品入口为 `/product/`，唯一产品源码和 embed 同步规则见[UI 说明](artex-web-ui.md)；下述 `/` 是保留的开发 Console。后续设计边界见[收口 ADR 补充](adr/bba-final-closeout-2026-10-31.md#后续适用规则2026-11-05)，运行/恢复见[发布手册](product-release-runbook.md)。本手册不提供真实动作授权。

The Task Engine server embeds a same-origin development console at `/`. Authenticated `GET /v1/accounts`, `GET /v1/tasks`, `GET /v1/tasks/{id}`, and `GET /v1/tasks/{id}/graph` reads are available after login. When the server is started with `-development-operations`, an explicit per-tab development-operation gate also exposes local Task Engine ledger mutations for task create/control/reconcile and the bounded proposal lifecycle; the page never exposes live QQ execution. Opening task detail refreshes its BBA-owned graph projection and immutable snapshot; graph projection is a controlled lazy write, not a zero-write read. The browser token remains in tab memory and is cleared on disconnect or 401. Static assets are embedded in the server binary; no frontend build is required.

## Requirements

- Docker with the repository's pinned `golang:1.26` image and PostgreSQL 16 image available locally.
- An isolated development PostgreSQL database explicitly initialized for this build's `SchemaHead()`. Read the migration registry and binary identity, then check the server minimum and Bridge exact-head gates as applicable; do not copy historical schema 21 as the current head. Startup never migrates; do not reuse an older fixture across a schema bump.
- A resolver JSON file owned by the operator with permissions `0600` or stricter. It contains SHA-256 token digests, never raw bearer tokens. Grant `operator.read` and `task.read` for reads; local development mutations additionally require the specific `task.create`, `task.control`, `task.approve`, or `task.manage_all` permissions used by the route.
- `BBA_TASKENGINE_DSN` supplied through a protected environment mechanism. Never put DSNs, passwords, raw bearer tokens, QQ cookies or runtime secrets in this document, shell history, or source control.

## Isolated development stack

`tools/dev-stack.sh` runs the disposable local loop — PostgreSQL container, schema initialization and the console server — with fixture-only ports, data and credentials:

```sh
tools/dev-stack.sh start     # database on 127.0.0.1:18432, console on 127.0.0.1:18431
tools/dev-stack.sh status
tools/dev-stack.sh token     # fixture bearer token for the console login field
tools/dev-stack.sh logs
tools/dev-stack.sh stop      # SIGTERM drain for the server, then database stop
tools/dev-stack.sh reset     # remove containers, data volume and state directory
```

- Ports: `BBA_DEV_STACK_DB_PORT` (default `18432`) and `BBA_DEV_STACK_HTTP_PORT` (default `18431`) must differ from the real service ports `5432` and `18421`; the script refuses those values and a database/HTTP port collision.
- Data: the docker volume `bba-dev-stack-<instance>-pgdata` and the state directory `build/dev-stack/<instance>/` are scoped to `BBA_DEV_STACK_NAME` (default `default`). `build/` is ignored by the repository.
- Credentials: the fixture database password, console bearer token and DSN are generated into `0600` files inside the state directory; the resolver file holds only the token digest. `env` prints the fixture exports and `token` prints the bearer token, on explicit request only; no secret is written to source, logs or shell history.
- Safe stop: the server receives `SIGTERM` and up to 15 seconds to drain before `SIGKILL`; the database container is stopped with a 20 second timeout. `stop` retains the data volume and state, `reset` removes them.
- Worker: `start` launches the deterministic Task Engine worker explicitly (`-worker -worker-id dev-worker-<instance> -worker-recover`) with its fixture transport and a `BBA_DEV_STACK_WORKER_POLL` interval (default `2s`). It advances only the local Task Engine chain and performs no QQ action.
- Boundaries: no QQ adapter, OneBot/NapCat connection or production DSN is started. A non-fixture `BBA_TASKENGINE_DSN` is rejected rather than reused, the fixture DSN is always built from the generated password, and `BBA_DEV_STACK_SKIP_BUILD=1` reuses already built binaries.
- Rotation: the schema includes durable worker cursors, proposal work-item lease epochs, ARTEX graph outbox/checkpoints/planning snapshots and login-session generation fences. Each bounded worker tick checkpoints after work. Unknown or ambiguous external effects remain retained and are never automatically resent; only safe graph reads may retry within their explicit attempt budget.

## Start

Use the isolated stack above for the normal local loop. For a database you manage yourself, build the server with the same offline Go cache convention as the repository test runner, writing the output into the mounted repository so the host can execute it after the container exits:

```sh
mkdir -p build/bin
docker run --rm --network none \
  --user "$(id -u):$(id -g)" \
  -e HOME=/tmp/home \
  -e GOPATH=/work/.cache/gopath \
  -e GOMODCACHE=/work/.cache/gomod \
  -e GOCACHE=/work/.cache/gobuild \
  -e GOPROXY=off -e GOSUMDB=off \
  -v "$PWD:/work" -w /work/backend golang:1.26 \
  /usr/local/go/bin/go build -o /work/build/bin/bba-taskengine-server ./cmd/taskengine-server
```

The executable is `./build/bin/bba-taskengine-server` in the repository; `--rm` no longer discards it, because the output path is a mounted directory rather than a container-only `/tmp` path. Provide the database DSN through the local secret manager/environment and launch it with the protected resolver file:

```sh
BBA_TASKENGINE_DSN=... ./build/bin/bba-taskengine-server \
  -resolver-file /secure/path/taskengine-resolver.json \
  -worker -worker-poll 2s
```

The default listener is `127.0.0.1:18421`. Open `http://127.0.0.1:18421/` and enter the bearer token issued for this local resolver. The token is not saved across refreshes or browser restarts. Stop the foreground process with Ctrl-C; signal handling drains HTTP requests for up to 10 seconds, then waits for the worker's bounded notification drain before the store closes.

For the disposable fixture, `tools/dev-stack.sh start` supplies the fixture-only flags and worker transport. The console is the unified operator surface for accounts, tasks, reports/GroupContext, BBA and ARTEX graph status, proposal/work-item trace, login sessions, model diagnostics and risk/operations state. Proposal apply remains intent-only; explicit orchestration mode and `POST /v1/tasks/{id}/plan-proposals/{proposal_id}/materialize` create durable work items only after the approval, digest, version, scope and authorization checks pass. Materialization creates no external effect. The proposal worker then uses the existing Reserve, permit and bridge gates; observations project by exact execution/attempt/work-item IDs. The page shows receipts, 403/409/5xx/offline states and does not automatically retry mutations. Graph facts and Summary context are references, never execution proof or authorization.

Do not bind this development server to a non-loopback interface, reverse-proxy it publicly, enable QQ adapters, or use a production DSN. Schema initialization is explicit through `taskengine-migrate`; the server never migrates on startup. For validation use a disposable isolated fixture. Production service changes, live migrations, real QQ reads/writes, worker preflight/execute/recover/chat-send, live model inference and writer ownership changes remain outside this guide and require their own authorization.

## 按变更选择开发检查（2026-11-05 承接）

以下除注明外均从仓库根运行。先核对 scripts/`npm run`、工具版本、Docker 和依赖缓存；只跑相关且必要的检查，扩大范围说明理由，失败必须调查。

```sh
# 契约：缺缓存且确有需要时才显式联网 prefetch
bash tools/contract.sh prefetch
bash tools/contract.sh check
# 改 Schema 且需要重建类型时才运行：bash tools/contract.sh generate

# Go：必须使用隔离 harness，先设置专用可写 TMPDIR
mkdir -p "$PWD/build/test-tmp"
TMPDIR="$PWD/build/test-tmp" bash tools/kernel.sh test-all
TMPDIR="$PWD/build/test-tmp" bash tools/kernel.sh smoke
bash tools/kernel.sh schema

# 前端及候选插件
python3 tools/build-product.py
node --check frontend/app.js
node --test tests/plugin/*.mjs

# Controller：先按 pyproject 安装到独立 Python 环境；只执行隔离模拟测试
PYTHONPATH=controller/src python3 -m unittest discover -s controller/tests -v
```

通知与对话请求作用域回归分别为 `tests/product/notification-request-scope.test.cjs`、`tests/product/chat-request-scope.test.cjs`；使用可用的 Playwright/Chrome 环境执行 `node --test <测试文件>`，依赖定位见文件头。`tests/product/b7-real-service-acceptance.cjs` 必须针对调用者拥有的全新隔离 fixture 运行，并由调用者 finally 停止；不指向已部署服务。现有 CI 不执行这些浏览器测试，不能用 CI 绿灯替代本机证据。

Go 测试不得裸跑 `go test ./...` 绕过 harness 或掩盖缺 DSN skip。`kernel.sh` 用无网络容器与 Unix socket、不发布 DB host 端口；`schema` 是隔离 SQL 应用 smoke，不替代 server/Bridge 启动门测试。`prefetch` 准备缓存后契约 Node/Go 阶段使用 `--network none`。根 `contract:check` 只含 refgate/validator/Python tests/lint，完整类型生成与 smoke 见 `generate`。

两个 Koishi 包分别安装和验证（Node 22）；Collector 的 file 依赖要求先准备 Character：

```sh
(cd koishi/character-runtime && npm ci --ignore-scripts --no-audit --no-fund && npm run build && npm run typecheck && npm test)
(cd koishi/analysis-collector && npm ci --ignore-scripts --no-audit --no-fund && npm run check)
```

`prebuild`/`pretest` 会调用固定 ChatLuna patch；`--ignore-scripts` 安装不代表可以跳过后续 build。Collector 使用 `lib/`，Character 使用 `dist/`，都不能以旧编译产物替代源码验证。

只有任务要求产品验收时才创建新证据目录（占位路径需替换）：

```sh
bash tools/product-acceptance.sh fixture --output <new-evidence-directory>
bash tools/product-acceptance.sh verify --package <evidence-directory>
bash tools/product-acceptance.sh release-check --package <evidence-directory>
```

## CI 实际范围

- `.github/workflows/ci.yml`：PR 和 main push 触发 `contracts`（契约、TS 类型检查、前端语法、插件测试）、`backend`（隔离 race suite + build）、`character`（安装/build/typecheck/tests）。
- `.github/workflows/controller.yml`：仅匹配 `controller/**`、自身 workflow 或 `docs/adr/controller-phase1.md` 时触发 Controller unittest/stdio MCP fixture；只改根 `AGENTS.md` 不会触发它。
- 当前没有常驻 Collector、产品 embed 同步完整性或完整浏览器验收作业。CI 绿灯仅支持已运行项目；分支保护/required checks/approval 每次从 GitHub 实际设置读取，不在指南写死。
