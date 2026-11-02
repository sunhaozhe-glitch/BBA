# Task Engine Development Console

The Task Engine server embeds a same-origin development console at `/`. Authenticated `GET /v1/accounts`, `GET /v1/tasks`, `GET /v1/tasks/{id}`, and `GET /v1/tasks/{id}/graph` reads are available after login. When the server is started with `-development-operations`, an explicit per-tab development-operation gate also exposes local Task Engine ledger mutations for task create/control/reconcile and the bounded proposal lifecycle; the page never exposes live QQ execution. Opening task detail refreshes its BBA-owned graph projection and immutable snapshot; graph projection is a controlled lazy write, not a zero-write read. The browser token remains in tab memory and is cleared on disconnect or 401. Static assets are embedded in the server binary; no frontend build is required.

## Requirements

- Docker with the repository's pinned `golang:1.26` image and PostgreSQL 16 image available locally.
- An isolated development PostgreSQL database at schema 21 (`0010_proposal_work.sql`, `0011_artex_sync.sql`, and `0012_login_sessions.sql`), the current migration head. The server requires schema >= 21 and never migrates; initialize a brand-new fixture explicitly with `taskengine-migrate`. Do not reuse an older fixture across a schema bump.
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
- Rotation: schema 21 stores durable worker cursors, proposal work-item lease epochs, ARTEX graph outbox/checkpoints/planning snapshots and login-session generation fences. Each bounded worker tick checkpoints after work. Unknown or ambiguous external effects remain retained and are never automatically resent; only safe graph reads may retry within their explicit attempt budget.

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

## Verify

The server package and embedded asset boundary are covered by the repository's isolated race suite and contract checks:

```sh
./tools/kernel.sh test-all
./tools/contract.sh check
python3 -m unittest discover -s tests/product -p 'test_*.py' -v
```

The complete isolated closeout path keeps raw evidence in a new candidate directory:

```sh
bash tools/product-acceptance.sh fixture --output evals/results/bba-product-candidate-<UTC>
bash tools/product-acceptance.sh verify --package evals/results/bba-product-candidate-<UTC>
bash tools/product-acceptance.sh release-check --package evals/results/bba-product-candidate-<UTC>
```

`fixture` covers migration, Go race, contracts, Node22, proposal, graph, login, model, Collector/Summary/Character, browser and backup/restore. `verify` checks business assertions and required scenarios in addition to manifests. `release-check` never deploys and remains `RELEASE_BLOCKED` until separately authorized live evidence exists. The isolated runner does not prove production deployment, live QR, live QQ action, live ARTEX, real-model quality, production migration or sole-writer handover. Use `docs/product-release-runbook.md` for those explicit rehearsals.
