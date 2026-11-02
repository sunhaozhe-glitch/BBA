// Package artex adapts the ARTEX exploration graph (facts / nodes / edges and
// goals) reached over ARTEX's own REST API.
//
// # Boundaries
//
//   - Reads by default, writes only behind an explicit gate. Graph/node reads are
//     HTTP GET. The goal graph (write.go) is reachable only when
//     Config.AllowWrites is set; otherwise every write method fails locally with
//     CodeWriteNotAllowed and issues no I/O. There is no write path for facts,
//     nodes, edges, executions or applied proposals: only the two goal routes
//     ARTEX documents are mapped, and no request can send a QQ message.
//   - Not a graph projection, and not "wired". BBA's own core.task_graph_*
//     read-model is rebuilt from the BBA task kernel; this adapter never feeds it
//     and never reads it. Seeing this package compile, or seeing BBA's graph
//     projection produce rows, is NOT evidence that ARTEX is connected:
//     「BBA 图投影 ≠ ARTEX 接线」. A second, contract-level projection of ARTEX
//     reads lives in taskengine (core.graph_sync_*) and is fed only by the
//     outbox worker; it is a cache, never the execution authority.
//   - Offline-tested only. The tests replay recorded response fixtures through an
//     injected http.RoundTripper. They touch no network, no database, and no host
//     port. The live ARTEX endpoint was NOT contacted while building this package:
//     live :8082 execution is unperformed.
//   - No ARTEX source is copied into BBA. ARTEX is AGPL-3.0 (ARTEX/LICENSE:1);
//     this package only maps the JSON shapes ARTEX already publishes over HTTP,
//     with each mapping traced to the ARTEX file and line below.
//
// # Interface inventory (source of truth: local ARTEX tree)
//
// Transport and address:
//
//   - ARTEX serves one Go net/http mux; its default listen address is ":8787",
//     NOT ":8082": ARTEX/cmd/artex/main.go:55. A tree-wide grep finds 8082 only
//     as sample traffic strings in ARTEX/traffic tests, never as a listener in
//     ARTEX's server, cmd, config, or compose files — so this adapter invents no
//     default port and BaseURL is always supplied by the caller.
//   - Auth is a JWT (HS256, 7-day TTL) accepted from "Authorization: Bearer …",
//     the artex_token cookie, or ?token=: ARTEX/server/auth.go:87-118. Every
//     /api/* route except /api/auth/*, /api/health and /api/basic-auth/check is
//     behind it: ARTEX/server/auth.go:120-137. A missing token yields
//     {"error":"未授权"} with 401; an invalid/expired token yields
//     {"error":"token 无效或已过期"} with 401: ARTEX/server/auth.go:128-135.
//     This adapter sends only the Authorization header (never a cookie or a
//     query parameter, which would leak the token into logs).
//   - Error envelope for every non-2xx reply is {"error": "<message>"}:
//     ARTEX/server/server.go:4062.
//
// The read endpoints this adapter maps (there are exactly two route entries for
// exploration graph reads: ARTEX/server/server.go:845 and :846):
//
//   - GET /api/exploration/graph?task=<task id> → {"nodes":[…],"edges":[…]},
//     capped at 2000 nodes and 5000 edges; an unresolvable task returns the same
//     envelope with empty arrays and HTTP 200:
//     ARTEX/server/server.go:2630-2674.
//   - GET /api/exploration/nodes?task=&kind=&state=&q=&order=&page=&size= →
//     {"items":[…],"total":N,"page":N,"size":N,"edges":[…],"refs":{…},
//     "assets":{…}}: ARTEX/server/server.go:2676-2741.
//     Pagination is 1-based offset paging (page/size), size defaults to 20 and is
//     clamped to 200: ARTEX/server/server.go:2678-2679,
//     ARTEX/db/exploration.go:873-938. "kind" and "state" are comma-separated
//     multi-value filters: ARTEX/server/server.go:2688-2692.
//
// Node/edge shapes (all ids are decimal strings, ts is RFC3339):
//
//   - Node: {"id","type","payload","priority","state","origin","ts",
//     "source_task_id","inherited","delete_reason"} where "type" is the ARTEX
//     node kind, "payload" is a JSON *string* holding the raw payload JSON, and
//     source_task_id/inherited/delete_reason are omitted when empty:
//     ARTEX/server/dto.go:183-211 (encoding helpers at :18, :20, :24).
//   - Edge: {"src","dst","rel"} with string ids: ARTEX/server/dto.go:268-277.
//   - "refs" is a map keyed by node id whose values are the same node shape:
//     ARTEX/server/server.go:2728-2730. "assets" is a map keyed by node id of
//     {"id","type","label"} objects: ARTEX/server/dto.go:340-344,
//     ARTEX/server/server.go:2739 — this adapter does not model it (unconsumed
//     fields are ignored on decode).
//   - Node kinds and edge relations: ARTEX/db/constants.go:5-11 and :34-39.
//
// Capabilities that do NOT exist in the ARTEX source (so this adapter does not
// pretend to have them):
//
//   - There is no MCP server face. ARTEX is an MCP *client/host*: it registers
//     external MCP servers and caches their tools. The MCP protocol methods it
//     speaks are outbound JSON-RPC ("initialize", "tools/list", "tools/call",
//     "Mcp-Session-Id"): ARTEX/mcphttp/client.go:1-11, :31, :60, :247, :259,
//     :300, :330-331, :381; consumers: ARTEX/server/mcpdiscover.go:18-45.
//     No server-side handler exists in the tree. Therefore no MCP endpoint is
//     mapped here, and none of ARTEX's graph reads are reachable as MCP tools
//     from outside.
//   - There is no id-cursor pagination over HTTP. The agent-facing list_facts
//     tool pages by "before" (id < before) and returns total/has_more/
//     next_before: ARTEX/agent/tools.go:944-978, ARTEX/db/exploration_sources.go:117,
//     but that tool lives inside ARTEX's own agent loop. The only external paging
//     is the offset paging of /api/exploration/nodes above.
//   - There is no dedicated "get one node by id" route. The supported trick is the
//     documented numeric-q filter, which matches the node id exactly (or "#<id>")
//     in addition to a payload substring: ARTEX/db/exploration.go:904-906. Node()
//     uses exactly that and then verifies the returned id, so a payload-substring
//     hit can never be mistaken for the requested node. Because that OR-match can
//     fill a whole page, Node() reports CodeIndeterminate instead of a false
//     CodeNotFound when the page is saturated. ARTEX/server/server.go:2680 also
//     resolves an empty or "active" task parameter to the currently active task
//     (ARTEX/server/manager.go:1731-1736), which is why this adapter rejects an
//     empty TaskID locally instead of inheriting that ambiguity.
//   - No route writes facts, nodes, edges, executions or applied proposals. The
//     only write routes ARTEX publishes are the four goal operations documented in
//     write.go (ARTEX/server/goals_api.go), and they accept no idempotency key,
//     client token or unique label, so this adapter never automatically replays a
//     write.
//
// # Unwired vs failing
//
// The adapter distinguishes three states explicitly and never lets a caller
// confuse them:
//
//  1. Not wired — no BaseURL and/or no Token. Wired() is false and every method
//     fails with CodeNotConfigured wrapping ErrNotConfigured without issuing any
//     I/O. This is the "BBA has not been authorized to read ARTEX" state; it must
//     never be displayed as "connected".
//  2. Wired but unauthorized — a token was configured and ARTEX answered 401
//     (ARTEX/server/auth.go:128-135). That is CodeUnauthorized and is NOT retried.
//  3. Wired and failing — transport failures, retryable statuses, malformed
//     payloads. These are CodeUnavailable / CodeUpstreamError /
//     CodeRequestRejected / CodeInvalidResponse and carry HTTPStatus + Attempts.
//
// # Context, timeouts, retries, and the "unknown outcome" boundary
//
// Read requests are GET with no body, so a replay cannot mutate ARTEX state and
// the adapter retries them only when it has no usable answer yet. Write requests
// (write.go) are never retried and treat any non-answer as CodeOutcomeUnknown.
// Concretely, for reads:
//
//   - Each attempt runs under context.WithTimeout(ctx, Config.Timeout) (default
//     10s) derived from the caller's context; a caller cancellation stops the loop
//     immediately with CodeCanceled and no further attempt.
//   - Retries: up to Config.Retry.MaxAttempts total attempts (default 3, clamped
//     to 1..5) with exponential backoff from Config.Retry.BaseDelay (default
//     100ms, no jitter). Retryable: any transport-level error, plus HTTP 408, 429,
//     502, 503, 504. Not retryable: 401 (CodeUnauthorized), 403 (CodeForbidden),
//     404 (CodeNotFound), every other 4xx (CodeRequestRejected), 500 and every
//     other 5xx not listed above (CodeUpstreamError), and any
//     received-but-invalid payload (CodeInvalidResponse).
//   - "Unknown outcome, do not resend": once a response has been received, a body
//     that cannot be read, exceeds the 8 MiB cap, or fails to decode is returned
//     as an error WITHOUT a second request. Replaying would swap the server's
//     actual answer for a different one, and the caller must see the anomaly.
//     Redirects are not followed (http.ErrUseLastResponse); a 3xx surfaces as
//     CodeInvalidResponse.
//   - An empty graph is not an error. ARTEX answers HTTP 200 with empty arrays for
//     a task it cannot resolve (ARTEX/server/server.go:2633-2636,
//     2681-2686), so an empty result means "no readable nodes, or the task id is
//     not visible to this ARTEX" and must not be read as "task does not exist".
package artex
