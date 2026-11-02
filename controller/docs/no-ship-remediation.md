# PR #5 NO SHIP remediation: F1–F4

Base: fa65f1f2e82bf238f996440614b529aa073a30d3.
Scope: isolated development Controller only. No real OMP/model/provider/QQ/production integration or merge is authorized by these changes.

## F1 trusted simulation identity

Supervisor accepts only the existing fixed simulation command and known behavior enum, then replaces its execution argv with the current Python executable, -I -S, and the absolute installed sim_peer.py. Isolated mode excludes cwd/PYTHONPATH/user-site; no-site disables sitecustomize startup. The peer is stdlib-only. Normal and restore Controller paths share this Supervisor validation. Tests commit a hostile bba_controller package, sitecustomize, usercustomize and json.py into the fixture and exercise success, UNKNOWN/hang restore and exit. Marker files must remain absent. This is trusted-launch protection, not a hostile local filesystem race sandbox.

## F2 minimal Git config

Before repository Git operations, parse the exact config with git config --file --no-includes. Accept only ordinary repository core booleans/format 0, user identity, origin URL/default fetch mapping, and main branch tracking. Unknown or duplicate keys are refused, including includes, filters, fsmonitor commands, uploadpack overrides, URL rewrite, SSH commands and unsupported extensions. Fixture .git must be an ordinary directory; config.worktree, commondir and object alternates are refused. The local bare origin is validated too. Fetch uses its checked absolute path, not a configurable transport alias. Executable is /usr/bin/git; global/system config are disabled; hooks/fsmonitor/recursive submodules/automatic maintenance are disabled; all protocols except file are denied. Existing custom Git configurations can therefore be rejected intentionally.

## F3 canonical containment

Inspect the original path and ancestors for symlinks before resolving .., then use resolved absolute paths. State/repository and worktree/repository must be disjoint in both ancestor directions. Settings freezes canonical paths before Controller mkdir/SQLite. Tests compare repository inventories before/after rejected startup and cover symlink-before-dotdot traversal. No claim of resistance to concurrent malicious OS filesystem replacement is made.

## F4 request contract

bba_task_submit now requires request_id: 1–128 ASCII letters/digits/underscore/dot/colon/hyphen, first character alphanumeric. Scope is the whole local Controller state database. Retain the same ID when retrying after a missing response; use a new ID for a new intended task even if content is identical. The exact parsed parameter object (key-order independent, optional-field presence significant) is hashed only for same-ID conflict detection. Content digest is not an idempotency key. BEGIN IMMEDIATE and submit_requests.request_id PRIMARY KEY atomically bind request digest and task alongside original task/snapshot/budget/events. Replays return the original current task without a second submitted event; changed parameters conflict. Old tasks without IDs remain readable and are not retroactively assigned request identities. Internal Store callers can still omit request_id; the public MCP boundary cannot.

Tests cover thread concurrency, committed child process os._exit before response delivery, same-key conflict, distinct identities and the public Controller tool. Client schema consumers must update for the newly required field. This additive Controller SQLite table is not a business PostgreSQL migration.

## Evidence status

F1 regression observed RED then GREEN; F2 hostile local/origin config tests observed RED then GREEN; F3 containment tests observed RED then GREEN; F4 request replay tests observed RED then GREEN. First full suite: 68 tests, one error because an existing process fault-injection detector still matched the previous -m argv; detector updated to trusted absolute script with its original cleanup assertions retained. A subsequent full suite is being recorded outside the checkout. Independent review and GitHub PR/CI remain pending until observed. Original audit report and evidence are preserved unchanged.
