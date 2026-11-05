# B7 Pagination Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [x]`) syntax for tracking.

**Goal:** Make the B7 conversations, conversation messages, and local notifications views consume the existing `next_cursor` contract with safe load-more behavior and regression coverage.

**Architecture:** Keep the existing same-origin API and server pagination contract unchanged. Add client-side cursor state and explicit load-more controls for the three B7 lists; each request remains bound to the current session, route, tab, conversation, and list generation so a late response cannot replace a newer view. Extend the existing browser fixture and real-service acceptance probes to prove first-page, second-page, stale-response, empty/error, and no-external-write behavior.

**Tech Stack:** Vanilla JavaScript, HTML rendered by `frontend/app.js`, Playwright browser fixtures, Node.js test scripts, existing Go/PostgreSQL acceptance launcher.

**Spec:** `docs/product-acceptance-matrix.md` B7-01 through B7-06 and M1 pagination findings.

## Global Constraints

- Keep the existing OpenAPI cursor contract: `next_cursor` is always present; an empty string means the final page; a non-empty cursor is passed as `cursor`, not inferred from rendered rows.
- Preserve the existing operator/runtime/workspace/account boundaries and `send_permit: absent` behavior.
- Do not add a database migration, change Go handlers, call QQ, call a paid model, or add a second frontend source.
- Keep stale responses and failed reads from replacing current data with empty or demo content.
- Keep frontend source in `frontend/`; regenerate the Go embed only after source and tests are settled.

### Task 1: Add failing pagination behavior tests

**Files:**
- Modify: `tools/b7-chat-notify-browser.cjs`
- Modify: `tests/product/b7-real-service-acceptance.cjs`

**Interfaces:**
- The browser fixture responds to `cursor=50` for conversation, message, and notification list paths.
- The UI exposes a visible `加载更多` button only when the corresponding response has a non-empty `next_cursor`.
- The second page is appended to the existing list, and the request includes the returned cursor.

- [x] **Step 1: Extend the local fixture with 51 synthetic rows and cursor-aware GET responses.**

The fixture must keep the current two-account/two-conversation isolation data and add enough rows to make the first page contain 50 items and the second page contain the oldest synthetic row. Record every browser request so the test can assert the cursor query.

- [x] **Step 2: Add assertions for all three list consumers.**

Assert that conversations, messages, and notifications each show the first page, expose `加载更多`, request the returned cursor exactly once, retain first-page rows, and render the tail row after the second page. Assert no load-more control remains when `next_cursor` is empty.

- [x] **Step 3: Add a stale load-more response test.**

Hold a second-page request, switch route or conversation, complete the held response, and assert the newer view remains unchanged and no stale list rows appear. Keep the notification mutation/navigation regression assertions intact.

- [x] **Step 4: Run the focused browser test and confirm RED.**

Run: `node tools/b7-chat-notify-browser.cjs build/b7-pagination-red frontend`

Expected: the existing baseline checks pass and the new pagination assertions fail because the current UI only requests `offset=0` and renders no load-more control.

### Task 2: Implement cursor state and load-more rendering

**Files:**
- Modify: `frontend/app.js`
- Test: `tools/b7-chat-notify-browser.cjs`

**Interfaces:**
- Add independent cursor/loading/error state for the conversation list, selected message list, and notification list.
- Keep the existing `chatViewCurrent` and `notifyViewCurrent` guards as the authoritative validity checks.
- Render `data-pagination` controls with a stable `加载更多` label and list-specific action values.

- [x] **Step 1: Add resettable pagination state.**

Track `chatCursor`, `chatHasMore`, `chatListLoading`, `chatListError`, `chatMsgCursor`, `chatMsgHasMore`, `chatMsgLoading`, `chatMsgError`, `notifyCursor`, `notifyHasMore`, `notifyLoading`, and `notifyError`; reset them in `resetSession`, `loadChat`, `loadChatMsgs`, and `loadNotify` as appropriate.

- [x] **Step 2: Add a cursor query helper.**

Build list URLs with `limit=50&offset=0` for the first page and `limit=50&cursor=<server next_cursor>` for later pages. Do not compute a cursor from item count or sequence numbers.

- [x] **Step 3: Update `loadChat` and conversation rendering.**

On a fresh load replace `chatItems` and save `raw.data.next_cursor`; on load-more append items and update the cursor. Keep first-page rows visible during a load-more request. Render a list-level error beside the current rows and show the control only while a non-empty cursor remains.

- [x] **Step 4: Update `loadChatMsgs` and message rendering.**

On a fresh conversation load replace `chatMsgs` and save the cursor; on load-more append the older page. Bind the control to the current conversation guard so switching conversations invalidates the request. Preserve the existing composer and draft boundary.

- [x] **Step 5: Update `loadNotify` and notification rendering.**

On a fresh route load replace notifications and save the cursor; on load-more append older notifications. Preserve `notifySeq` invalidation, CAS button guards, and the merged notification navigation fix from PR #17.

- [x] **Step 6: Wire one delegated click handler for list-specific load-more controls.**

The handler must prevent duplicate clicks while a request is active, preserve the current view on failure, and re-render only when the captured validity function still returns true.

- [x] **Step 7: Run syntax and focused browser tests.**

Run: `node --check frontend/app.js` and `node tools/b7-chat-notify-browser.cjs build/b7-pagination-green frontend`.

Expected: all existing B7 UI checks and the new pagination/stale-response checks pass.

### Task 3: Update the real-service gap probe and embedded asset

**Files:**
- Modify: `tests/product/b7-real-service-acceptance.cjs`
- Modify: `frontend/app.js`
- Generate: `backend/cmd/taskengine-server/product/app.js`

**Interfaces:**
- The real-service probe must classify pagination as PASS only after the browser consumes the cursor and displays the tail row.
- The test must continue to prove no task, chat permit, dispatch permit, or model request is created by the read-only browser flow.

- [x] **Step 1: Convert the known pagination gap checks into positive assertions.**

Keep direct API checks for `next_cursor`, then assert the browser issues a cursor request and displays the oldest synthetic message/notification/conversation. Do not make a fixture-only success stand in for real-service evidence.

- [x] **Step 2: Run the product asset generator.**

Run: `python3 tools/build-product.py`.

Expected: the generated Go embed copy matches `frontend/app.js` and `frontend/app.css` byte-for-byte under the repository’s build rules.

- [x] **Step 3: Run contract and frontend checks.**

Run: `bash tools/contract.sh check` and `node --check frontend/app.js`.

Expected: exit 0 with no contract or syntax changes.

### Task 4: Full bounded verification and evidence update

**Files:**
- Modify: `docs/product-acceptance-matrix.md`
- Modify: `docs/superpowers/plans/2026-11-05-b7-pagination.md`

**Interfaces:**
- Evidence must identify the exact commit, fixture scope, commands, exit codes, and remaining live boundary.

- [x] **Step 1: Run the focused product tests.**

Run the browser-local fixture, the notification request-scope regression, and the B7 launcher tests with the project’s Node 22 runtime.

- [x] **Step 2: Run the isolated Go/backend acceptance required by the latest main.**

Run the applicable `tools/kernel.sh` and `tests/product/b7-real-service-acceptance.cjs` launcher path against a fresh fixture database. Do not pass a production DSN or external QQ credentials.

- [x] **Step 3: Review the diff and generated asset identity.**

Check `git diff --check`, `git diff --stat`, source/embed equality, and `git status --short`. Confirm no credentials, runtime logs, database dumps, or ignored evidence are staged.

- [x] **Step 4: Update only the B7 pagination acceptance row.**

Record what passed, what was not run, and retain the explicit `NOT_RELEASED`/real QQ boundary. Do not mark B7-07 or the whole product complete.

- [x] **Step 5: Commit the bounded change.**

Use a focused commit such as `fix(product): consume B7 conversation pagination`.


## Execution Record

- Implementation commit: `4231bab` (`fix(product): consume B7 conversation pagination`).
- Local browser fixture: 51 checks passed at 1440/768/375 widths.
- Notification request-scope regression: 82 tests passed with the project Playwright/Chrome runtime.
- B7 real-service fixture: 12 checks passed; M1, pagination, and notification navigation were PASS; fixture cleanup verified.
- Product launcher guards: 10 tests passed. Go `test-all` exited 0 after using a user-writable `TMPDIR`; the first `/scratch` failure was an environment permission failure before tests started.
- Remaining product boundary: authenticated header text remains a P3 observation; live QQ, paid models, production databases, deployment and release remain out of scope.
