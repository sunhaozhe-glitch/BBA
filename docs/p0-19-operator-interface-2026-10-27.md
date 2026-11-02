# P0-19 Operator Interface (2026-10-27)

## Status

**P0-19 isolated source/fixture acceptance: PASSED. Deployment and P0-20 readiness: NOT CLAIMED.** The minimal operator surface is a Go CLI backed by the Task Engine HTTP contract. It lists accounts and tasks, fetches task detail, and issues pause, cancel, and read-only reconcile requests. PostgreSQL behavior was exercised only against disposable databases created by `tools/kernel.sh`.

## Delivered behavior

- `GET /v1/accounts` returns workspace accounts in stable cursor pages. Authorization accepts `operator.read` or the broader `task.manage_all` manager permission; `task.read` alone cannot list workspace accounts.
- `GET /v1/tasks` returns task pages scoped to the caller's principal; `task.manage_all` permits cross-principal task visibility. Outcome counters use the canonical read projection: valid results are confirmed, `disposition='pending'` is pending external, and `state='UNKNOWN'` is unknown. These unresolved classes do not count as success and may overlap reserved targets.
- `GET /v1/tasks/{id}` returns the current task projection and version.
- Pause/cancel and reconcile read the task version before mutation. The client prints HTTP 202 and the complete server receipt. It labels 202 as durable request acceptance, never completion of an external action.
- Mutations accept an explicit `-idempotency-key` and `-expected-version` for exact request replay after uncertain delivery. If the POST response is lost, the CLI reports both values. Without an explicit key, a cryptographically random key is generated and printed with a successful receipt.
- Reconcile is read-only with respect to task state/version and execution: it writes a durable operator request event/outbox entry and idempotency receipt. No consumer that performs a server-side external membership read is implemented; the receipt is not evidence that such a read occurred.
- The CLI requires an explicit origin and a regular token file with mode `0600` or stricter. Non-loopback URLs require HTTPS; redirects are disabled; requests and response aggregation are bounded.

## Verification

- `./tools/kernel.sh test -run '^$' -count=1` compiled the backend.
- `./tools/kernel.sh test -run '^TestP019|^TestOperatorCLI' -count=1` passed after the final account-read authorization change. PostgreSQL-backed tests cover account/task pagination, principal scoping, confirmed/pending/unknown counters, read-only reconcile invariants, HTTP 202 receipt replay, task-entry denial, and `operator.read` allowance versus `task.read` denial for account listing. CLI fake-server tests cover pagination, authorization header, exact mutation body/key/version replay, receipt/status output, HTTP conflict, and lost-POST retry details.
- `./tools/kernel.sh test-all` passed across all Go packages with race detection after the final changes.
- The executable CLI smoke builds `backend/cmd/bba-operator`, starts only a local `httptest` server, and invokes the binary to list a fixture task. No external endpoint was contacted.
- HTTP fixtures also check detail/list counter parity, invalid page limits, two-page account traversal, task-entry method/authorization boundaries, and separate account read/control permissions. A received non-202 HTTP response is reported as a definitive error; only uncertain transport delivery includes replay guidance.

`./tools/contract.sh generate` passed with 505 local refs, 219 fixtures, 54 contract tests, 113 TypeScript fixture round-trips, and generated Go/TypeScript types. P0-17 flags were not enabled.

The CLI accepts flags before the subcommand. Example: `bba-operator -url https://operator.example -token-file /secure/operator.token tasks`. For task controls, pass `-idempotency-key <stable-key> -expected-version <observed-version>` before the `pause`, `cancel`, or `reconcile` subcommand, then provide the task ID and reason. The CLI expects the deployment to enforce its authenticated operator session; this workspace does not provide or deploy that resolver.

## Boundaries

No production database, deployed Task Engine server, QQ API, existing BBA/Koishi service, or external write path was accessed. This acceptance proves local implementation and fixture behavior only. The CLI has not been connected to a deployed authentication resolver or server. Reconcile's durable request currently has no worker that performs the requested external membership read; its receipt proves durable request acceptance only. P0-17 proposal/apply/model-request flags remain false. P0-18 remains partial/not accepted; P0-20 remains blocked on its dependencies and separate owner approval.

The P0-19 local source/fixture behavior is frozen at the sorted SHA-256 manifest digest `d98c571500610731a0cb4a5edbc5faad2fec5ba4b2eb711ab4c5222902de9210` over the operator CLI, task/account authorization, task projection, generated contract, and their focused tests. This fingerprint excludes docs, the deployment resolver, and the production server. Deployment acceptance remains open.
