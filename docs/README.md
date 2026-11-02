# P0-01 Contract Tooling

`contracts/openapi.yaml` is the normative HTTP and plugin wire contract. Its named schemas live under `contracts/schemas/`; positive and negative fixtures live under `contracts/examples/`. Generated Go and TypeScript types are build outputs and must not be hand-edited.

## Validation

Run from the BBA directory. `prefetch` is needed only when the pinned tool cache is absent; it is the sole network-enabled pipeline stage. All Node and Go validation/generation containers run with Docker `--network none`.

```sh
tools/contract.sh check
tools/contract.sh generate
```

`generate` reruns the complete check, bundles the OpenAPI document, generates TypeScript and Go models, compiles all valid fixtures against the generated TypeScript types, executes their JSON round-trip, and exercises valid fixtures through the generated Go models. Individual local checks are also available:

```sh
python3 tools/refgate.py --root contracts
python3 tools/validate_contracts.py contracts
python3 -m unittest discover -s tests/contract -t . -v
```

Generated types are build outputs; never hand-edit them. `build/`, `node_modules/`, `.cache/`, and `.tmp/` are ignored tool/cache content.

## Contract Scope

`contracts/openapi.yaml` is the only normative HTTP/plugin contract. OpenAPI 3.0.3 references local closed schema documents under `contracts/schemas/`; fixtures under `contracts/examples/` contain positive and negative values for every exported component. The ref gate rejects remote, absolute, parent-traversing, and symlink-escaping references, and recursively checks every reachable local document.

Task Entry is limited to task creation, authoritative current task state, and pause/cancel. Calls authenticated by `TaskEntryCredential` carry `X-QQ-Account-ID` and `X-QQ-Sender-ID` from authenticated platform event context; the backend must verify runtime/account ownership and sender/principal mapping. Task-entry access does not expose subtasks, results/evidence, events, reconciliation, account controls, capability enumeration, or planning.

Task event `seq`, owner epochs, and fence/sequence values are decimal strings to preserve values beyond JavaScript's safe integer range. Required nullable response fields remain present as JSON `null`. Go generation enables `nullable-type: true` and pins `github.com/oapi-codegen/nullable v1.1.0` to distinguish absent fields, explicit nulls, and concrete values. Go fixture round-trips compare complete decoded JSON without null normalization.

The OpenAI-compatible chat completion endpoint described as optional future model integration in `02-contracts.md` is intentionally not in this contract. Add it only as part of the separately authorized model integration phase, with workload identity, bounded request/response schemas, budgets, and no inherited QQ action permission.

P0-12/P0-13 plugin operations and P0-17 model-request/apply operations remain production-disabled (`x-feature-enabled-in-p0: false`). OpenAPI marks the P0-15 `chat-send-permits` route `true`; this is contract metadata only, not proof of a deployed/operational production API. P0-15 includes a frozen offline Character runtime and fixture-backed send gate; its evidence and limits are in [P0-15 offline acceptance](p0-15-character-runtime-2026-10-27.md). P0-01 does not implement a production API server, database, scheduler, or adapter. Contract presence or fixture success does not authorize live QQ access, deployment, service restart, frozen NapCat changes, or production writer changes.

## P1a ChatLuna Group Analysis Collector Acceptance (2026-10-29)

The independent `koishi/analysis-collector` App was accepted for P1a collection-only behavior with fixture groups and `--network none`. It loads the pinned Koishi 4.18.11, ChatLuna 1.4.4, group-analysis 1.4.1, Puppeteer 3.9.0, and the Koishi HTTP service; no QQ adapter is installed and no real QQ read/write occurs.

| Check | Result | Evidence |
| --- | --- | --- |
| Collector typecheck/build | PASS | Node 22 container, `--network none` |
| Collector acceptance tests | PASS, 3/3 | Real ChatLuna and group-analysis; Puppeteer service fixture; whitelist persistence, non-whitelist/selfId zero rows, dispatch/model/send/internal denials, state lock/binding, bounded missing-service failure |
| `tools/contract.sh check` | PASS, 63 tests | 224 fixtures; 111 invalid and 113 valid; zero validator errors/warnings |
| `tools/contract.sh generate` | PASS | TypeScript fixture smoke and Go nullable round-trip smoke passed; generator config keeps unreferenced models and explicit nulls |
| `tools/kernel.sh test-all` | PASS | All Go packages, including `internal/taskengine` race run |
| Character full suite | PASS, 21/21 | Node 22 container, `--network none` |

The P1a switch matrix is fixed in collector diagnostics: `cronSchedule`, `registerTools`, `userTitleAnalysis`, `personaAnalysisMessageInterval`, and `enableAllGroupsByDefault` are forced off; `outputFormat` is forced to `text`. Group-analysis command, persona command, prompt variables, golden-quote analysis, and tool splitting have no dedicated upstream switch and are denied at the collector boundary. No model, analysis, report, or outbound send path is exposed.

P1b remains gated and unimplemented. Before activation it requires one collector per group/selfId, explicit whitelist and time-window evidence, paging/deduplication/selfId-conflict fixtures, real schema-validated `Summary.v1` instances including the 500/4000 boundaries and producer scope consistency, asynchronous windowed analysis outside the short-reply SLO, and the existing BBA text-only permit before any report send. P1c remains gated and unimplemented. Before activation it requires the P1b evidence, text-only report rendering, approval and send gates, timeout/rejection/unknown no-resend behavior, and a controlled read-only ARTEX dual-graph connection. No P1b/P1c implementation was started.

The real Puppeteer browser launch was attempted offline with the host Snap Chromium binary and failed because the isolated Node image lacks `libnspr4.so`; the service-fixture acceptance therefore proves collector/plugin boundaries and group-analysis injection, while real browser startup remains an environment prerequisite. The failure was bounded as `service_injection_timeout`; no network fallback was used.

## Acceptance Record (2026-10-24)

Executed in the target BBA directory:

| Executed command | Exit code | Observed result |
| --- | --- | --- |
| `python3 tools/refgate.py --root contracts` | 0 | `387 local ref(s) in 6 file(s)` |
| `python3 tools/validate_contracts.py contracts` | 0 | 82 components with both valid and invalid fixtures; 190 fixtures (100 valid, 90 invalid); `errors=0`, `warnings=0` |
| `python3 -W error::DeprecationWarning -m unittest discover -s tests/contract -t . -v` | 0 | 31 tests passed |
| `python3 -W error::ResourceWarning -m unittest tests.contract.test_refgate tests.contract.test_validator tests.contract.test_plugin_fake -v` | 0 | 23 tests passed |
| `tools/contract.sh check` | 0 | Complete local validation and Python suite passed; Redocly: `Your API description is valid.` |
| `tools/contract.sh generate` | 0 | Complete check, bundle, Go/TypeScript generation, TypeScript compilation, 104 TypeScript fixture JSON round-trips, and Go fixture smoke passed; final line: `contract.sh: generation + type smoke OK` |
| `go test -tags contractsmoke -run '^TestFixturesRoundTripGeneratedTypes$' -count=1 -v ./internal/contract` (inside the offline Go container) | 0 | `encoded/decoded 104 valid fixtures through generated Go types`; `PASS` |
| `go run ./.tmp/contract-runtime` (inside the same offline Go container; throwaway program removed after execution) | 0 | Generated `Pagination` preserved absent cursor, explicit null, and string cursor; generated `DecimalSequence` preserved `"9007199254740993"` |

The two direct Go commands used `golang:1.26` with `--network none`, project mount `/work`, working directory `/work/backend`, `GOPATH=/work/.cache/gopath`, `GOMODCACHE=/work/.cache/gomod`, `GOCACHE=/work/.cache/gobuild`, `GOPROXY=off`, `GOSUMDB=off`, `GOFLAGS=-mod=readonly`, and `CONTRACT_ROOT=/work/contracts`. Existing cached dependencies were reused without another prefetch.

The final generation output reports five informational Redocly unused-component warnings (`Pagination`, `AccountIdentitySource`, `AccountIdentityPage`, `VersionRequest`, and `Disposition`); these remain intentionally exported components and are fixture/type-smoke covered. Build output is generation/type/fixture smoke evidence, not runtime Task Engine integration evidence.

P0-02 is complete with a local capability matrix, schema-validated isolated fake-adapter tests, and a separate sol read-only observation record. The real read-only record does not establish live cleaner join, candidate deployment, or any QQ write capability. See `docs/capabilities/P0-02.md` for the evidence matrix, frozen-versus-candidate distinction, assumption decisions, unknowns, P0-03 identity gate, and detailed command outputs.

The recursive contract suite discovers 46 tests (31 existing contract tests plus 15 P0-02 capability tests). `python3 -W error::DeprecationWarning -W error::ResourceWarning -m unittest discover -s tests/contract -t . -v` exited 0 (`Ran 46 tests ... OK`). `python3 tools/refgate.py --root contracts` exited 0 (`387 local ref(s) in 6 file(s)`). `python3 tools/validate_contracts.py contracts` exited 0 (82 components, 190 fixtures, zero errors/warnings).


At the P0-03 acceptance checkpoint, implementation covered the contract, deterministic fake-runtime, and registry/guard layers only. No runtime self-info request was sent: the configured local OneBot port was not listening, so all local account bindings remained unverified. No QQ read/write, login, account switch, deployment, service restart, frozen NapCat tree modification, production database access, or writer switch occurred. P0-04 and later phases had not started at that checkpoint; the later P0-04–11 acceptance record is linked at the end of this document.

## P0-03 Account Identity And Runtime Isolation

`AccountIdentity` is the identity registry contract, separate from the existing `Account` status projection. `account_id` is the stable internal key; `qq_self_id` is null until authenticated OneBot `get_login_info` read-back returns `data.user_id`. The reviewed local NapCat handler is read-only and derives `user_id` from its current `core.selfInfo.uin`; config labels, instance IDs, endpoints, and adapter package IDs are binding metadata, not proof of account identity. The currently configured `local-6099` instance has no executor record and its configured OneBot port 3001 was not listening; it remains unverified.

The implementation is `backend/internal/identity/registry.go`. `Open(path)` uses an explicit caller-supplied ledger path; no production registry path was created or selected. The ledger is a single local JSON file guarded by `flock`. Each state change is written to a mode-0600 temporary file, flushed, atomically renamed and followed by a directory sync. Internal `identityRecord` retains the last read-back self-id/time/source; the returned `Identity` projection excludes those audit fields to satisfy the closed `AccountIdentity` schema. Reopening the ledger invalidates process liveness and converts previously verified bindings to stale. Runtime activation advances its generation. Read-back commits compare the generation captured before HTTP against both the durable record and active runtime; a restart during HTTP cannot authorize the new generation. The guard is not connected to a production writer.

Character-enabled accounts require unique `character_runtime_id`, `character_db_id`, and `character_cache_namespace`. The deterministic fake fixture partitions history/conversation keys, cache, sender/adapter calls, and task/retry context by account/runtime binding. It is test evidence only; no Koishi/Character deployment or database is present, and production runtime isolation remains unverified.

### Identity Model And Evidence

| Field / state | Meaning and evidence rule |
| --- | --- |
| `account_id` | Stable opaque internal key; forward and reverse lookup use this binding, never a label or nickname. |
| `qq_self_id` | Nullable decimal string. Set only from authenticated `get_login_info.data.user_id`; numeric JSON is decoded without float conversion. |
| `runtime_id`, `napcat_instance_id` | Explicit runtime/instance registration metadata. A runtime, NapCat instance, or adapter instance cannot be assigned to a second account. |
| `adapter_id`, `adapter_instance_id` | Adapter type/package may be shared; adapter instance and event/writer runtime ownership are account-specific. |
| `character_runtime_id`, `character_db_id`, `character_cache_namespace` | Unique per-account Character partitions, required by this P0-03 implementation under ADR D3. |
| `capability_profile_id` | Binding metadata only; it does not promote any P0-02 unsupported or unverified action. |
| `status`, `identity_verified_at`, `identity_source`, `runtime_generation` | Independent identity evidence. Existing `Account` connection, control, risk, and readiness projections are unchanged. `unverified` has null identity evidence; `verified` requires authenticated read-back in the active generation; reopen/reactivation makes prior verification `stale`; mismatching/duplicate self-id quarantines the attempted binding as `drift`. |

Static source inspected at `/run/media/sun/6F53FE6DFAA8CECF/QQ群项目/QQ/Napcat/opt/QQ/resources/app/app_launcher/napcat/napcat.mjs`: lines 72543–72547 map current `selfInfo.uin` to numeric `user_id` and `nick` to `nickname`; lines 74968–74979 implement `GetLoginInfo` by returning `Oi.selfInfo(this.core.selfInfo)`. This establishes a read-only action and response field, not the identity or health of a running instance. No runtime endpoint was requested.

| Local candidate | Observed evidence level | Verified identity / readiness |
| --- | --- | --- |
| Login UI instance `local-6099` | Static local configuration; executor map empty. OneBot `127.0.0.1:3001` had no listener at the P0-03 check. No HTTP read-back attempted. | `unverified`; `qq_self_id`, verification timestamp and source remain null. No inferred `account_id`/QQ binding was registered. |
| Koishi / ChatLuna / Character | No installed deployment was verified; source snapshots and fake partitions only. | Runtime identity and production isolation remain unverified. |

There are zero verified local runtimes. Fake accounts `acc_a`/`acc_b` and self-ids `10001`/`10002`/`10009` are synthetic and have no relationship to the local login instance.

### Isolation And Writer Guard Acceptance

| Boundary | Exercised behavior |
| --- | --- |
| Account identity / connection state | `TestAccountIdentityIsolation`: deactivating/restarting A leaves B verified and writable through its own fake reader; a crossed account/runtime invocation is rejected. |
| DB / group and private history | Same group and private target IDs retain distinct account histories; group/private kinds do not collide. |
| Active conversation | B cannot continue A's active conversation before B receives its own event. |
| Cache | Same cache key in A and B retains different values in explicit namespaces. |
| Sender / tool / adapter | A cannot call B's adapter instance; rejected calls produce no fake adapter action. |
| Task / retry | B cannot consume A's retry; rejection preserves it for A. These are isolation fixtures, not a task scheduler or retry implementation. |
| Runtime/account/adapter context | Wrong runtime, account or adapter-instance context fails closed before history mutation or tool action. |
| Reopen / runtime restart | Persisted verified evidence becomes stale; fresh verification is required. Deterministic blocking-reader regressions reject old generation responses in both `Verify` and `CheckWriter`, even if the new runtime was independently reverified. |

`CheckWriter` requires a nonempty action family, registered verified identity, active runtime generation, exact account/runtime/adapter/adapter-instance/expected-self-id binding, then a fresh authenticated self-info match committed to the ledger under generation checks. Missing identity, stale verification, unavailable runtime/reader, malformed self-info, self-id drift, duplicate registration, or failed ledger access returns an error. It never sends QQ actions or switches writers. The check is identity evidence at that instant, not a send permit, account/action lock, pause enforcement, or proof of global writer exclusivity; those later-phase boundaries remain disabled. No wall-clock verification TTL is inferred: generation invalidation and fresh per-call read-back are the implemented freshness rules.

Duplicate runtime/adapter-instance/NapCat-instance and shared Character partitions are rejected before registration. A duplicate self-id attempt is rejected and its attempted binding is quarantined; the established reverse mapping remains with the original account. Self-id drift preserves the established identity and records the conflicting observed self-id in internal audit evidence. Drift remains quarantined across reopen/reactivation, with no automatic rebinding or reconciliation. No live duplicate or drift was observed because no runtime read-back occurred; absence of live evidence is not proof of absence.

### P0-03 Acceptance Record (2026-10-24)

Environment: local working tree, Docker `golang:1.26` and `node:22-bookworm`; all Node/Go execution used `--network none` and existing caches, with no prefetch. Direct Go commands used the mount/cache/environment settings recorded above for P0-01. All new runtime behavior was exercised against temporary ledgers and fake HTTP inside the offline container.

| Executed command / scenario | Exit code | Observed result |
| --- | --- | --- |
| `go test registry.go registry_test.go lifecycle_test.go -run TestRuntimeRestartRejectsInFlightReadback -count=1` (identity directory, before fix) | 1 | Both `verify` and `writer` subcases reproduced `old runtime read-back authorized the new generation`. |
| Same generation regression after fix | 0 | Both paths reject the old response. |
| `go test ./internal/identity -run TestHTTPLoginInfoReader -count=1 -v` | 0 | Authenticated fixed action, wrong-runtime rejection, integer precision, malformed/trailing/oversized responses, loopback origins, redirects and credential-safe errors passed. |
| `go test -race -count=1 -v ./internal/identity ./internal/characterscope` | 0 | Identity and Character suites passed, including `TestAccountIdentityIsolation` and both in-flight restart subcases; no race report. |
| `tools/contract.sh generate` | 0 | 401 local refs; 86 components, 198 fixtures (104 valid, 94 invalid), zero validator errors/warnings; 48 Python tests; valid Redocly description; generated Go/TypeScript; 104 TypeScript round-trips and Go contract smoke passed. Final line: `contract.sh: generation + type smoke OK`. |
| `python3 -W error::DeprecationWarning -W error::ResourceWarning -m unittest discover -s tests/contract -t . -v` | 0 | `Ran 48 tests ... OK`, including unchanged P0-02 capability tests. |
| `go run ./.tmp/p003-smoke` (throwaway program, removed after acceptance) | 0 | `result: PASS`; 5 fake `get_login_info` requests; forward/reverse identity, unverified/inactive/stale guards, duplicate self-id, drift audit persistence, reopen quarantine and history/cache/active/sender/adapter/task/retry isolation exercised; zero real QQ requests/writes. |
| Actual smoke JSON checked with `ContractLoader.component_errors("AccountIdentity", ...)` and `semantic_errors(...)` | 0 | All 5 public projections passed schema and semantics: unverified, verified, duplicate drift, stale, self-id drift. No internal last-readback fields escaped into the public contract. |
| `go test ./internal/identity -run TestBindingIDContractLengthBoundary -count=1` (before boundary fix) | 1 | Reproduced oversized registration for all nine binding ID fields. |
| `go test -race -count=1 ./internal/identity ./internal/characterscope` (after final boundary fix) | 0 | Final identity and Character suites passed, including Unicode ID boundaries and unchanged generation/guard regressions. |
| `go run ./.tmp/p003-id-boundary` (throwaway program, removed) | 0 | `PASS: 128 Unicode-character ID persisted and JSON round-tripped; 129-character ID rejected before persistence; no runtime requests`. Binding IDs now enforce the contract's 128-character maximum before persistence. |

The generator reports five informational exported-but-unreferenced component warnings: `Pagination`, `AccountIdentitySource`, `AccountIdentityPage`, `VersionRequest`, and `Disposition`. These components remain fixture/type-smoke covered. Strict Python warning checks and schema validation passed; generator notices are not claimed to be absent.

### Effect On P0-04 And Later Scheduling

At the P0-03 checkpoint, P0-04 had not started. The requirements carried into later phases were to reference `account_id` as the internal key and preserve the explicit identity/runtime/adapter and Character partitions; labels, filenames, ports and directories remain unusable as identity evidence. Account-pool/scheduler eligibility must require current verified identity in addition to existing connection/control/risk/capability readiness, and must recheck identity at dispatch rather than trust a cached pool entry. Stale/unverified/drift/unavailable accounts cannot be promoted automatically. Identity failure does not free pending/unknown slots, switch an in-flight account, retry a write, or establish writer ownership; P0-02 and ADR D2/D5 constraints remain the baseline.

Production Character isolation, all real send paths, global writer ownership, external identity drift and process-crash durability on production storage are not established by these fake tests. No login, account switch, real QQ call, deployment/service restart, frozen NapCat change, production database access, or writer cutover occurred during P0-03 acceptance. That acceptance stopped at P0-03; the later P0-04–11 fixture acceptance is recorded separately below.

Acceptance version is the local working tree identified by these SHA-256 fingerprints; no commit or production release is claimed:

| Source | SHA-256 |
| --- | --- |
| `backend/internal/identity/registry.go` | `ae8aac5b0d2e57c2dd4b3221502c35080e0ce0b57f36d3b12b6eeaef8e95e716` |
| `backend/internal/identity/onebot_reader.go` | `348e78c86bb9bcb37c85d37eed815e3c05e7a976d07fe15f6a4d4792f1c4ca41` |
| `backend/internal/identity/lifecycle_test.go` | `922a4f52c278fb3628989801f653cf349883ecc9df5f6c9f05738e4c1fae6919` |
| `backend/internal/identity/registry_test.go` | `07c2f2bddebd15f350334b266fc09e7fcfb5cd4787d9fafeae13270d048cae7c` |
| `backend/internal/characterscope/runtime.go` | `9f796a3072db6dcb55cf720a672ab8f2cc4c499ee9d85f9409869b96d0a27c2b` |
| `contracts/schemas/common.json` | `63b2aeb9ead103022d6b683882924a7aefb6719e4e26f6619209c9ba7707570b` |
| `contracts/openapi.yaml` | `34b5e1764c67398d8c6292e4a2490bf04e166a3b0db1b930f156623914d5235e` |
| `tools/validate_contracts.py` | `f0b74802946b33f24275030bf0c230b3b39251d5ca36fded3b0b8f373a27d607` |

## P0-04–11 Task Engine Acceptance (2026-10-24)

P0-04 through P0-11 remain fixture-only acceptance. Three remediation rounds (six initial findings, three re-audit items, three double-review counter-examples) were rerun and confirmed by an independent fourth-round red-team review; that round still judged the stage not acceptable because of three new blockers: terminal correlated leaves were merged differently from passive leaves, concurrent observations with the same `event_id` and different digests were silently acknowledged, and an external pending request blocked both local pause settlement and resume. The fifth round addressed all three: terminal correlated and passive `not_joined` now only record membership projection, inbox, and late audit while preserving the closed result, slot, claim, and subtask; observation writes arbitrate after insert, persist conflicts in the append-only `core.observation_conflict` table, and return `OBSERVATION_DIGEST_CONFLICT` only after commit; and pause settlement uses a process-local live-dispatch registry, so externally pending/`UNKNOWN` work no longer blocks `PAUSING → PAUSED` while resume never requeues, resends, resets retry, or releases its holds. That registry proves local quiescence only; it is not production, cross-process, cross-machine, old-owner, or global-writer isolation.

Fifth-round code is the local 21-file working tree with combined SHA-256 `760d44a11c6ce47dd1ff3996aaa8e42f81fa3fd49b58e1445a21a65203363534`; schema version is 7, and `Migrate` accepts only 7, with no upgrade or rollback compatibility implementation. Final local evidence: `schema` exit 0 with `PostgreSQL schema version=7`; `kernel.sh test` 56 PASS / 1 helper SKIP; full `test-all` race and non-race each 78 PASS / 1 helper SKIP; the seven new top-level tests ×3 = 21 PASS; cumulative asset probes 10+20+16+24 = 70 PASS + 1 helper SKIP; crash and concurrency stress 15 and 60 PASS with zero failures and zero race warnings; smoke, build, vet, gofmt, validator, 48 contract tests, and Redocly all exit 0. The initial red probes recorded the intended failures; the F3 tests added later have their own red/green records. The earlier remediation probe copy timed out only because it waited for stale SQL text (`%FOR UPDATE OF s%`); a temporary copy changed only that synchronization latch to `%FOR UPDATE%`, after which every business assertion passed. That adapted copy is not claimed as the original unadapted probe.

The owner confirmed fifth-round independent dual review passed; P0-11 remains accepted. Its `760d44a1…` fingerprint is historical. Before the current instruction, the same 21-file list hashed to `4a446e332a77b322e0fe6ad7358100de2d97157f66fbdd271fe930ba024642f2`. The initial P0-12 fix did not change those 21 files, but the subsequent heartbeat contract remediation modified three of them (`migrations/0001_core.sql`, `migrate.go`, `task_http.go`) plus the heartbeat implementation/tests; later rounds changed further files. P0-12 uses napcatd webui 6211 / OneBot 127.0.0.1:3211, account 36374700, with the token read directly from state.json. The initial runner fixes were followed by heartbeat remediation and the seventh round; after the eighth-round `bridge.go` production change (two pre-send account-state gates plus `ACCOUNT_PAUSED`→`ACCOUNT_RESTRICTED`) and heartbeat fix, the 21-file list hashes to `35739715765c525e39ed8987d1bf9839bd81038c65d7d48a98ad0d6e0b299d84` and the 27-file P0-12 list to `cb7bc2912d32098321548a537325a4f01b918d51e1175ac2fdfef27b827f315f`; the earlier `86648f11…`/`5995505b…` values remain historical. The first authorized live invocation used 3 reads, no retry, matching identity, online/good status, 52 groups and heartbeat 202/applied under schema 8; it remains a separate historical record.

Seventh-round remediation (fixture-only) adds `core.heartbeat_idempotency` so both the original and every successful alias `Idempotency-Key` are bound to exactly one heartbeat fact; reusing an alias with a different payload now returns `IDEMPOTENCY_CONFLICT` without a second audit row. It also validates raw capability JSON before generated-type decoding, so missing or null required fields (including `supported`/`fresh_read`) cannot become `false`; and it gives every retry attempt its own evidence record so `response_text` cannot leak forward. `TestSingleRetryAcrossReadChain` was restored verbatim from Round 5 and the per-action-retry mutant is detected again (`requests=4`). Schema is now 10 and `Migrate` accepts only 10. The second (and final) live run on 2026-10-25T01:57Z used 3 reads, no retry, 52 groups, heartbeat 202/applied/version 2, schema 10, and per-step UTC evidence in `/tmp/bba-r7-live/`; it recorded 27-file SHA `4fc60086…`; the seventh-round final 27-file SHA was `5995505b998a33a84984644744d94b6f979586a5413cc443b1720d3d3269dddd`, and the eighth-round current 27-file SHA is `cb7bc2912d32098321548a537325a4f01b918d51e1175ac2fdfef27b827f315f`. All author tests, `test-all` (taskengine 36.766s), schema 10, smoke, contract check and build/vet/gofmt pass. All three probe batteries (redteam, review6, review7) were rerun verbatim with `-count=3` after **ADR:75** retired the stale-water-mark expectation: a stale delivery echoes the request sequence (`19`), keeps account version 2, and records one `stale` audit row; the assertion changed from `20` to `19` with original bytes preserved under `/tmp/bba-r8/retired-original/`. R8 coverage was incomplete: review7 runner probes were missing; the complete R9 rerun is recorded below. At R9, ADR:76 recorded conditional approval. P0-13 then received separate single-account authorization for remediation and at most one live run; its fixture regression and runner are implemented, and five runner defects were remediated with author tests (auth-evidence redaction, strict per-record group parsing, boolean `status.good`, fail-closed `Plugin/List` capability preflight, and one transport/5xx retry across the whole run). At R9 those confirmations were blank; R10 later completed them and is recorded below. The earlier live invocation stopped before any write because target group `36374700` was already one of the account’s 52 groups, so no task, permit, adapter call or write occurred; the separate out-of-runner cleaner join-route probe returned HTTP 503 and is not capability evidence. `backend/p0-13-join` is a 16,108,940-byte ELF build artifact and is excluded from all four fingerprint groups (21/27/30/31). P0-13 live acceptance remains open and must not be inferred from fixture tests; see [P0-13 implementation and run report](p0-13-join-2026-10-25.md).

R9 corrects the preceding R8 battery coverage claim: review7's runner probes were absent there (`[no tests to run]`). This round restores their exact source bytes and passes all three complete sol batteries with `-race -count=3`, plus valid recovered CoS probes. Seven author P0-13 runner tests pass ×3; eight isolated production mutants fail author assertions. Full author tests, contract checks, schema, kernel smoke and build/vet pass. The full `run()` offline smoke stops before writes on invalid status, mixed baseline and cleaner 403; active synthetic cleaner submits exactly once. R9 groups were 21/27/30/31 files; 31-file SHA is `7c50717b9d2150c43220b7f0dce0dfbe61c3a218e552e384a52fd5d6233db482`. Ordered lists, red-test provenance and complete evidence are in the [P0-13 report](p0-13-join-2026-10-25.md). At R9 both approval fields were blank; all R9 QQ/WebUI traffic was synthetic.


R10 live authorization was completed for target **829117089**. The single invocation at **2026-10-25T06:27Z** used **7/10 budget calls, one D1 write, zero retries**. Cleaner returned `{"ok":true,"result":1}` without a request reference; both post-write group lists lacked the target. Outcome: **SUBMITTED_PENDING_MEMBERSHIP**, no valid result, no confirmed membership. Acceptance remains open under the owner's “real request_ref or confirmed observation chain” criterion. No second request or additional polling followed. Current 31-file SHA: `7d87869360e8c61e1da110acf8c011650b740e1db5f0d6bc9e9956624d295abe`; four before/after hashes match. Evidence `/tmp/bba-r10/live`, checksum `/tmp/bba-r10/evidence-sha256.txt`; [full R10 report](p0-13-join-2026-10-25.md#r10授权补齐后的唯一一次实跑)。
 
R11 final verification updated generated contract outputs, reran `tools/kernel.sh test-all` (exit 0), and reran all three sol original-byte batteries with `-race -count=3` (each exit 0). The 21/27/30/31 fingerprints remain `35739715765c525e39ed8987d1bf9839bd81038c65d7d48a98ad0d6e0b299d84`, `cb7bc2912d32098321548a537325a4f01b918d51e1175ac2fdfef27b827f315f`, `4f559a271b24eb864124053c16ec17d436909bb57047d53230247b5313ecb77a`, and `7d87869360e8c61e1da110acf8c011650b740e1db5f0d6bc9e9956624d295abe`; plugin candidate and patch hashes are tracked separately in the P0-13 R11 section.
 
The R11 cleaner and `set_group_leave` changes are offline candidate evidence only: 20/20 and 19/19 author behavior tests pass, while the frozen originals fail the corresponding assertions. No candidate deployment/load confirmation, candidate join, question-answer application, or new QQ write occurred. The separate sol raw read-only probe records usable login/status/group/friend/member/history/stranger/group-info reads and cleaner search, but profile/QZone-message-list failures and empty guild service data; it does not prove cleaner join or QQ write support. See [P0-13 R11](p0-13-join-2026-10-25.md#r11候选整改最终离线验证与部署停点) and [P0-12 R11](p0-12-readonly-2026-10-24.md#r11最终生成后验证与-sol-真实只读能力边界).
 
R12 completed the sol F1/F2/F3 offline remediation. Contract generation exited 0 with 401 refs, 86 components, 215 fixtures (111 valid/104 invalid), and 54 Python tests; Node cleaner/leave behavior was 89/89; author `test-all` was 264 PASS with exit 0; redteam/review6/review7 and valid CoS `supplement`/`legacy`/`legacy-exact` all exited 0 under `-race -count=3`. The historical 21/27/30/31 fingerprints remain unchanged; R12 evidence, self-contained 14-file frozen manifest, and incident discipline record are in [P0-13 R12](p0-13-join-2026-10-25.md#r12f1f2f3-整改封存与送审状态).

R12 is still offline candidate evidence. No candidate deployment/load confirmation, real runner, QR restoration, question-answer write, or new QQ write occurred; R10 pending membership acceptance remains unresolved. The accidental Electron-as-Node probe and the separately audited service GPU crash are recorded as distinct events, not collapsed into a claim of zero process lifecycle activity.
R13 implemented the two R12 behavior fixes in workspace source: top-level-only result-code extraction sharing `RAW_MAX_DEPTH` with raw diagnostics, and pre-descriptor property/node budgeting for oversized objects. The R13 candidate SHA is `94e201105af4b3c9b6a1c2bf9a01e27825ff2ba21c08659cfe522ede4ead9afd`; author cleaner tests are 72/72 and the sol supplement probe is 5/5. Node dual suites are 91/91, author `test-all` is 264 PASS, contract check/generate exit 0, and redteam/review6/review7/legacy/legacy-exact all pass under `-race -count=3`. The four historical fingerprints remain unchanged. See [P0-13 R13](p0-13-join-2026-10-25.md#r13结果链与属性预算整改全套复跑与送审状态) and `/tmp/bba-r13/evidence-sha256.txt`.

R13 remains offline evidence only: no deployment, service lifecycle action, QQ write, or real runner occurred. Submission is recorded without requesting further permission; real candidate load and membership acceptance remain open.


R14 copied the pinned R13 index/package into instance `qq-2c7cdf77e452`, verified both on-disk hashes, preserved plugins.json, and issued the single authorized supervisor restart (HTTP 202). Plugin/List returned `pluginManagerNotFound=true`; the existing session is `waiting_qrcode` with `coreReady=false`. A separate offline actual-route smoke proved the pinned candidate ignores the answer field. No sol/CoS join was dispatched, both write budgets remain unused, and P0-13 is not passed. Evidence: `/tmp/bba-r14`; see the R14 section in [P0-13](p0-13-join-2026-10-25.md). No further restart, QR refresh, runner, frozen-tree change or contract generation occurred.
R15 完成问答参数通路的离线整改：当前安装 `wrapper.node` 的证据要求 `joinGroup`/`reqToJoinGroup` 使用单个 `ReqToGroup`；候选将申请文案交付为 `richMsg`、答案交付为 `applyMsg`，wire `answer` 必填且空值在 native 前拒绝。R15 Node 双套件 92/92、作者全套 264 PASS、Go P0-13 定向、contract check/generate 均通过；旧 R13 snapshot 对新增回归 3 项失败，证明回归有效。候选 plugin SHA=`2d27b4e618fb9c8f959497d96ad6b91e28443928d85fc222f58a7c56cf3f4d96`，证据 `/tmp/bba-r15`。R15 尚未部署、未重启、未扫码、未运行真实 runner、未调用 QQ；真实 native 消费、管理员展示、答案匹配和 membership acceptance 仍未验证，后续部署/load 必须重新授权。
R16 已获授权并完成 R15 文件部署核验：实例 `qq-2c7cdf77e452` 的 `index.mjs` 从 R13 SHA `94e20110…` 更新为 R15 SHA `2d27b4e6…`，package/config SHA 保持不变。未重启、未刷新二维码、未启动真实 runner、未调用 QQ 写通道。WebUI auth HTTP 200，但 Plugin/List 仍为 `plugins=[]`、`pluginManagerNotFound=true`；登录状态仍为 `isLogin=false`、`waiting_qrcode`、`coreReady=false`。R16 证据在 `/tmp/bba-r16`；真实申请继续停止，需负责人完成现有扫码后再做只读 active/core-ready 核验。
R18 按负责人要求执行了一次新的受控问答申请：身份/在线/active 前置均通过，写前目标不在 51 群基线。唯一 cleaner 写调用 HTTP 200，但 runner 收到无法分类的响应，保守记为 `UNKNOWN`、无 `request_ref`，没有重试。随后一次只读完整群列表为 52 群且包含目标 `829117089`，因此成员已观察为存在；这不补造 native 回执，也不证明管理员展示/答案匹配或执行器关联的 submitted→confirmed 链。`/tmp/bba-r18` 保存脱敏记录和 SHA 校验。P0-13 停止继续申请；完整 native/审批关联证据仍未闭环。

R19（2026-10-26）负责人选择观察链口径，P0-13 以条件性通过结项：采纳 R18 的“写前 51 群缺席、唯一写、写后 52 群存在”，结项时只读复核仍为 52 群且目标存在。保留的已知缺口是：无 native `request_ref`、runner 仍为 `UNKNOWN`、无引擎内 submitted→confirmed、管理员展示/答案匹配未验证，且不能排除先前申请或外部批准。没有补造回执、没有新增 QQ 写。P0-14 fixture 机制验收与真实四号只读核验见 [P0-14 记录](p0-14-four-account-2026-10-26.md)；真实四号写/故障注入仍未执行。

P0-14 fixture 机制验收：四账号隔离、错误账号拒绝、跨账号 observation、unknown/no-resend、重启恢复和配额通过。R23 曾因 3210/3212/3213 不可达而 fail-closed；R24（2026-10-27 12:21 UTC）已复验四号身份在线且唯一，并通过 schema 12 隔离 engine smoke、独立 SQL proof、敏感扫描和 manifest；12 次只读调用、0 重试、0 QQ 写。证据 `/tmp/bba-p014-engine-schema12-20261027T122111Z/`，详见 [P0-14 记录](p0-14-four-account-2026-10-26.md)。真实 QQ 业务写、网络分区、进程重启尚未执行，需另行给定受控测试群、动作/文本和故障范围。
R24 engine smoke 的独立 proof 复用了已迁移到 schema 12 的 Task Engine；发现 runner 仍期待 schema 11，更新 proof expectation 后重跑全通过。业务逻辑未变。
P0-14 机制与当前只读/隔离 smoke 通过，不等于 P0-18 的 1/4/8/20 实际 Character 流量容量或真实写故障注入已验收。
## P0-15 Character Runtime Offline Acceptance (2026-10-27)

P0-15 offline source/fixture scope passed on 2026-10-27 and is frozen at its recorded digest. The targeted Node 22 runtime/Character suite passed 11/11; it covers per-account isolation, active-speaker control, cross-account suppression, text-only output, one-shot permits, direct/internal send denial, required outcomes, terminal unknown/no-resend behavior, and the pinned Character integration. A separate synthetic OneHop persona answer did not exercise Character. Freeze is source/fixture scope only: deployment management-plane verification and P0-14 real four-account controlled-write/fault-injection remain pending. No production API, database or QQ adapter was used. See [P0-15 report](p0-15-character-runtime-2026-10-27.md).

The pinned Character tokenizer attempted `tiktoken.pages.dev` and used its approximate-count fallback after network-none DNS failure. ChatLuna emitted a teardown exception while uninstalling a fake provider; the test suite still exited successfully. Neither event establishes production behavior.

P0-14 follow-ups remain open: PathRouted deployment/management-plane re-verification and controlled writes/failure injection against the four real accounts. These were not run here.

## P0-16 Local L1 Offline Implementation (2026-10-27)

P0-16 remains **INCOMPLETE: BLOCKED ON REAL MODEL EVALUATION**. Offline provider/evaluator source and fixture scope is accepted and frozen; its ChatLuna provider remains opt-in and loopback-only. A separately authorized probe directly called the local Swift-compatible API with synthetic requests and received `swift-1.5-iq3_s`. The provider-only 1/4/8/20 burst completed 1/1, 4/4, 6/8 and 6/20 requests; all remaining requests hit the 30 s deadline. Four-session p95 was 24.55 s, missing the design's proposed <=8 s target. This did not exercise Character, ChatLuna routing or a locked holdout and is neither a model score nor stable capacity claim. Model artifact/template provenance, server CPU/RSS/GPU, approved datasets, blind human review, and Character-path performance remain missing. Do not relax loopback restrictions to reach the service bound on `0.0.0.0:8166`. Full evidence: [P0-16 report](p0-16-local-l1-offline-2026-10-27.md).

On-disk model artifact hashes, model identity/probe boundary and capacity figures are recorded in the [P0-16 report](p0-16-local-l1-offline-2026-10-27.md). P0-17 implementation freeze fingerprint: `bde28370cd7eea423ed6d76d449590eef89de4953e79f3299bba30fa181caf12`; it does not change the overall blocked status. P0-18 partial capacity and restore drill evidence is in [P0-18 report](p0-18-capacity-restore-2026-10-27.md).

The local model key was read from the owner-provided file for the authorized probe and not retained in source/results. Its content was exposed by an interactive file-reading tool during this session; rotate it before further use. The earlier OneHop key was also pasted in chat and should be rotated. No QQ interface or production database was used.

The P0-16 evaluator exercised 24 explicitly simulated cases for `IntentDraft`, `PlanProposal` and `Summary`; all expectations matched, with 5 accepted and 19 rejected by fixture design. These counts are not accuracy. See the simulated result artifact and [P0-16 report](p0-16-local-l1-offline-2026-10-27.md).

## P0-17 Model Request Offline Acceptance (2026-10-27)

**OVERALL P0-17: BLOCKED; provider/validator/ledger implementation sub-scope accepted and frozen.** The isolated P0-17 suite passed again: migration upgrades, provider protocol, clarification-only proposal validation, success/failure idempotency, three-attempt budget, concurrent budget, Summary rejection and disabled handler. The live OneHop smoke was skipped because `BBA_P017_LIVE_API_KEY` was absent in this run; the earlier authorized live smoke remains a single historical observation. Production model routes remain disabled. No design-authorized redacted tuning/holdout evaluation or authoritative model/prompt revision evidence exists. Summary remains unsupported until there is an approved message-body/reference source. See [P0-17 report](p0-17-model-request-offline-2026-10-27.md).

The later local Swift probes used a separate direct model endpoint; they do not count as P0-17 OneHop provider/ledger evidence or satisfy P0-17. Production flags remain false.

## P0-18 Capacity and Restore Evidence (2026-10-27)

P0-18 remains **PARTIAL; NOT ACCEPTED**. The final isolated PostgreSQL 16 dump/restore rerun passed run, independent SQL proof, sensitive scan, and manifest verification; restored unresolved states remained visible, recovery created no new work, and adapter calls after restore were zero. Evidence and fingerprints are in the [P0-18 report](p0-18-capacity-restore-2026-10-27.md) and `/tmp/bba-p018-restore-freeze-final-20261027/`. A local fake HTTP fixture passed 1/4/8/20 protocol/correlation checks; it is not model or Character capacity evidence. The recorded direct-provider burst exceeded the proposed p95 target and timed out at 8/20. R24 is the latest P0-14 read-only/isolated smoke PASS; R23's earlier read-only failure is historical. Real P0-14 business writes/fault injection and P0-17 overall acceptance remain open, so P0-18 is not accepted.

P0-19 isolated source/fixture acceptance passed for the minimal Task Engine CLI and HTTP surface. The [P0-19 report](p0-19-operator-interface-2026-10-27.md) records account/task listing, confirmed/pending/unknown counters, pause/cancel/reconcile receipts, replay behavior, and verification commands. Account listing follows the design's `operator.read` permission (also allowed for `task.manage_all`; task-only and runtime task-entry identities are denied). `tools/kernel.sh test-all` passed with race detection. No production database, deployed API, QQ interface, or external write was accessed. Reconcile records a durable read-only request but has no external membership-read consumer; the CLI is not connected to a deployed authentication resolver/server. P0-18 remains partial/not accepted and P0-20 is not ready.
### Ordered R21 Follow-up (2026-10-28)

The current authorized follow-up rechecked the four bound QQ accounts read-only: 12 OneBot calls, zero retries or writes; identities match, all four currently report online/good. The earlier `local-3212` waiting-QR condition is not present in this recheck. A fresh comparison of the historical `/home/sun/Napcat` installation metadata paths found zero differences. This does not attribute the historical NapCat runtime-state changes, which remain unresolved; see [current B2r recheck](../evals/results/ordered-live-20261028T135541Z/B2R-CURRENT-RECHECK.md). No login recovery, service restart, installation mutation or QQ business write occurred in the recheck.

The P0-14 isolated engine smoke passed with independent SQL proof. The P0-18 PostgreSQL dump/restore run passed; unresolved external states remained visible and recovery added no new execution, permit, retry, result or adapter call. The user confirmed that target group `829117089` has no pending join requests; this is recorded as owner-provided confirmation, not a trusted API query. The cleaner still has no pending-request query endpoint.

The approved local Swift endpoint returned model `swift-1.5-iq3_s`. The P0-17 provider was extended to require exact private-endpoint approval while retaining HTTPS-only defaults. Its live synthetic model request/ledger/idempotency smoke passed after the prompt was tightened to the exact eight-key schema; the complete Task Engine race suite passed in that successful run. This is provider/ledger evidence, not model-quality or Character-path acceptance. P0-16 requires a separate real Character/ChatLuna path and approved holdout/human evaluation.

The actual Character/ChatLuna path was exercised at concurrency 1: 1/1 completed, with one permit, zero QQ calls, and a matching marker. One synthetic burst measured 4/4 at concurrency 4, 6/8 at 8 (two 30-second timeouts), and 4/20 at 20 (sixteen timeouts); it is not a stable capacity or SLO acceptance, and approved holdout/human quality scoring and server resource measurements remain absent. After separating the HTTP-error test from the tool-output failure state, the complete Character module passed 21/21; typecheck and a build to `/tmp` passed. The normal build still cannot write the existing `lib/` directory (EACCES); its permissions were left unchanged.

The separate P0-20 worker source is implemented and the full backend `tools/kernel.sh test-all` race suite passed, including `cmd/p0-20-execute`, `internal/taskengine`, and the unchanged read-only `cmd/taskengine-server`. No worker preflight or QQ write was run: no approved mode-0600 config or Task Engine/OneBot/WebUI credentials were present in the process environment or repository evidence. The worker remains unbuilt/deployed against the live service, and P0-20 dispatch is not accepted. The preflight still requires persisted online/login/control/risk/heartbeat/capability facts; no database rows were changed to satisfy those gates.



## ARTEX产品第一批
见 sol-artex-phase-a-progress.md，限定第一批自测完成，待Astra亲跑，其余产品范围未完成。
