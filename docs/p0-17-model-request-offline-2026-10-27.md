# P0-17 Model Request Offline Acceptance (2026-10-27)

## Status

**OVERALL P0-17: BLOCKED; provider/validator/ledger implementation sub-scope accepted and frozen.** The live OneHop integration smoke passed, but the design-required evaluation dataset/model provenance and approved Summary source are absent. This does not pass P0-17 design acceptance. The implementation-scope freeze is not production enablement.

`/v1/model-requests`, proposal, and apply remain `x-feature-enabled-in-p0: false`. The model handler is separate from the normal Task Engine handler and defaults to disabled unless explicitly mounted with trusted workload authentication and an enabled flag. No production route was mounted or enabled.

## Offline implementation boundary

The OpenAI-compatible provider accepts an explicit HTTPS `/v1` endpoint, model ID, provider ID, and injected HTTP client. It makes one bounded JSON-mode request, disables redirects, and performs no automatic retry. Its input is limited to prompt version, task version, scope revision, and goal count; task/account/group identifiers and message text are not sent to the provider. The handler verifies workload permission, task ownership/version/scope, deadline, idempotency, and a per-task provider-call budget capped at three. Each provider attempt is recorded once in `core.model_request`; completed and failed idempotency replays do not call the provider again.

The only accepted output is a `PlanProposal.v1` clarification request with no steps and at least one non-empty question. The output alias `task_ref` is mapped back to the authorized task identifier after validation. Executable steps, unknown fields, stale versions, wrong aliases, and other proposal forms are rejected. `Summary` remains unsupported because the task store has no approved source for message bodies or resolvable message references; inbox metadata is not treated as message content.

## Verification

| Command | Result |
| --- | --- |
| `tools/kernel.sh test -run 'Test(MigrateUpgradesSchema10And11To12|OpenAICompatibleProvider|ModelProposalValidation|ModelRequest|ConcurrentModelRequests)'` | Passed. In-process protocol/validation tests and isolated migration upgrades passed; provider fakes made no network calls. |
| `tools/kernel.sh test` | Passed. Full Task Engine suite ran with Go race detection. |
| `tools/kernel.sh schema` | Passed. Isolated empty PostgreSQL fixture applied migrations and reported `PostgreSQL schema version=12`. |
| `python3 tools/refgate.py --root contracts` | Passed: 499 local references in 6 files. |
| `python3 tools/validate_contracts.py contracts` | Passed: 86 components, 215 fixtures, zero errors and warnings. |
| `python3 -W error::DeprecationWarning -W error::ResourceWarning -m unittest discover -s tests/contract -t . -v` | Passed: 54 tests. |

The live smoke used one provider attempt; the subsequent idempotent replay added no provider call and returned the persisted result. The captured returned model ID is `deepseek-v4.1-flash-1017`. The test exercised the provider, strict proposal validator, response metadata mapping, and fixture-backed model ledger end to end. This does not validate the disabled production HTTP route or real task/message data.

`BBA_P017_LIVE_API_KEY` was injected from the authorized environment into a disposable test container; it was not stored in source or this report. The test used a synthetic task in the isolated PostgreSQL fixture and the repository's real OpenAI-compatible provider. It observed one provider call, valid clarification-only `PlanProposal.v1`, one completed ledger row with `attempts=1`, and successful idempotent replay. Provider token counts were recorded; task IDs, response text, and credentials are not retained here.

A separate protocol probe found the OneHop endpoint returned no usable `message.content` when DeepSeek-specific `thinking` fields were included. The generic provider therefore sends the standard OpenAI-compatible JSON-mode body without vendor-specific thinking fields. An earlier structured prompt trial also produced wrong field names and was correctly rejected. The final provider-path request passed strict schema validation. This proves one request's compatibility and schema result only, not a stable accuracy rate.

The successful live response reported server model ID `deepseek-v4.1-flash-1017`; this is returned API metadata, not authoritative weights/template or prompt-deployment evidence. `revision` and `quantization` remain null. The measured latency is a single observation, not a percentile or SLO result. The supplied key was pasted into chat and should be rotated.

Offline provider protocol tests use a custom in-process `RoundTripper` and made no network requests. They remain fixture evidence, distinct from the authorized live smoke above.

## Latest isolated rerun (2026-10-27)

The targeted suite passed again: migration 10/11→12, provider protocol and endpoint guards, clarification-only validation, successful and failed idempotent replay, three-attempt task budget, concurrent budget, Summary rejection and disabled handler. The live OneHop test was skipped because `BBA_P017_LIVE_API_KEY` was absent; no online credential was guessed or reused. This rerun strengthens the isolated implementation evidence only and does not change the blocked overall status.

## Not performed / acceptance blockers

- No direct DeepSeek API request was made; the authorized live request used the OneHop gateway. The OneHop credential was pasted in chat and should be rotated. No credential file was read during that earlier smoke.
- No authorized redacted `intent`, `character`, or `planning` tuning/locked holdout set, human review, schema/personality score, or broad latency/throughput/resource evaluation is available.
- No QQ interface, production database, or deployment was accessed. No change was made to any P0-17 production feature flag.

- Summary generation remains blocked: the task store has no approved message-body and resolvable message-reference source. Do not treat inbox metadata as content.

The previously authorized local Swift synthetic probes and capacity runs recorded in the P0-16/P0-18 reports exercised the separate local model endpoint, not the P0-17 OneHop provider or ledger path. They do not close P0-17 evaluation.

P0-17 remains unaccepted as a whole until authorized evaluation data, broader model behavior/quality review, authoritative model/prompt metadata, and the approved message-source policy where applicable are available. Production feature flags remain false.
## Implementation freeze fingerprint

The P0-17 implementation sub-scope fingerprint is `bde28370cd7eea423ed6d76d449590eef89de4953e79f3299bba30fa181caf12`, computed as SHA-256 over the sorted-by-contract file manifest `evals/results/p017-implementation-freeze-20261027/scope-sha256.txt`. It covers provider/handler implementation and tests, model-request migration, model schemas/OpenAPI and live-smoke runner; report/evaluation artifacts are excluded. This is a source identity record, not acceptance evidence or production enablement.

