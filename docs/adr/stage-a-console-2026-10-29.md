# ADR: Stage A Operator Console (2026-10-29)

> **2026-11-05 范围说明：** 本文保留早期开发 Console 的只读决定和事实，不把它作为产品只能只读的永久边界。确认的产品入口为 `/product/`，唯一产品前端源码为 `frontend/`，见[产品 UI 说明](../artex-web-ui.md)及[根规则](../../AGENTS.md)。任何产品写入口仍须对应服务端授权与验收；本次不变更业务行为。

- Status: Accepted for the local development surface
- Scope: Authenticated, read-only account/task browser views served by `taskengine-server`.

## Context

The target tree has no active frontend. The archived `app/web` is MIT-licensed, but its client targets the archived backend's `/api/*` routes and its larger Next.js application is not wired to the current Task Engine `/v1/*` contract. The Task Engine is loopback-bound, fixed-Host, and exposes only account/task GET plus read-only reconcile POST in its deployment allowlist.

## Decision

Serve one dependency-free HTML/CSS/JavaScript application from `backend/cmd/taskengine-server`, embedded into the existing binary. The browser uses same-origin authenticated `GET /v1/accounts`, `GET /v1/tasks`, and `GET /v1/tasks/{id}` only. The token remains in JavaScript memory and is cleared on disconnect or authentication failure. The console does not persist tokens, call reconcile, or expose create/control/send operations. All API requests pass through the unchanged resolver and deployment allowlist.

Keep account connection/login/risk/control dimensions visible as server-provided values. Keep confirmed, pending, reserved and unknown task counts distinct. No browser-side readiness inference or mutation workflow is introduced.

## Consequences

- One server binary and origin; no frontend dependency install, cross-origin proxy, or second listener.
- Static console assets are public but contain no account/task data. Data requests still require the configured bearer credential.
- The UI is an operator read surface, not QR login, account management, approval, task control, or production deployment acceptance.
- Visual browser verification and a repeatable standalone collector install remain separate acceptance items.
