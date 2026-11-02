# P0-18 Capacity and Restore Evidence (2026-10-27)

## Status

**P0-18: PARTIAL ISOLATED EVIDENCE; NOT ACCEPTED.** The direct local model-provider capacity ladder and the disposable PostgreSQL `pg_dump`/`pg_restore` recovery drill were run. The capacity probe missed the architecture's proposed latency target and did not exercise ChatLuna/Character routing. The restore drill exercised the real Task Engine store and recovery/API paths on a disposable fixture, not live accounts. Design dependencies P0-14 (four-account controlled writes/fault injection) and P0-17 overall acceptance remain open; this report does not waive them.

Production routes and model feature flags remain disabled. No QQ interface, production database, or existing BBA/Koishi service was accessed. The explicitly authorized local model endpoint was accessed with synthetic prompts only. The model credential was read from the owner-provided local file for the probe and was not persisted; the file was exposed by an interactive file-reading operation during setup and should be rotated.

## Capacity ladder

A one-burst-per-level direct provider probe used JSON-mode synthetic arithmetic with a unique session marker for each request and a 30-second request deadline. It contacted the API model ID `swift-1.5-iq3_s` directly; it did not use the repository's loopback-only ChatLuna provider, Character, a Gateway queue, real session history, or QQ. Results and per-request redacted records are in `evals/results/p018-local-provider-20261027-r2/`.

| Concurrent synthetic sessions | Completed | Deadline timeouts | Unvalidated outputs | All-request p95 |
| ---: | ---: | ---: | ---: | ---: |
| 1 | 1/1 | 0 | 0 | 4679.867 ms |
| 4 | 4/4 | 0 | 0 | 24549.785 ms |
| 8 | 6/8 | 2 | 2 | 31653.726 ms |
| 20 | 6/20 | 14 | 14 | 30026.193 ms |

All completed answers, JSON shapes, and synthetic session markers matched expectations; there were zero automatic retries. Timed-out responses are `unvalidated`, not schema failures. The <=8 second short-reply p95 design target is missed at 4, 8 and 20 concurrent requests. The test is one synthetic structured burst per level, so it is neither a Character short-reply SLO run nor evidence of steady-state capacity. Server CPU, server RSS, GPU/VRAM, and model-server/Gateway queue delay were not observable and remain null; measured client CPU/RSS are reported separately in `summary.json`.

The model inventory declared `n_ctx=262144` and API ID `swift-1.5-iq3_s`. That is server metadata, not a tested context limit. The static serving config names Swift-Qwen3.8-Flash-Next-GSQ-RCO-abliterated IQ3_S GGUF shards and a tokenizer pack. Disk file hashes for both shards, tokenizer/chat-template, config and API server were measured and are recorded in the P0-16 report. The running service did not attest these bytes or an immutable author/build revision. These probes do not establish P0-16 evaluation scores.

Capacity evidence SHA-256:

- Initial burst `sessions.jsonl`: `bbb48d879fdd8fdbe364c89bc60e4978c7aee2410a39c532191fc659c8847e86` (`evals/results/p018-local-provider-20261027/sessions.jsonl`); initial `summary.json`: `2cf4fbf431cb82ef318de9f76e0fea4986c97920ee44a1a033fa55600a6ce0e7`. This run's expired requests were initially classified incorrectly as schema failures; it is retained as superseded failure evidence, not used for final timeout/schema accounting.
- Corrected burst `sessions.jsonl`: `cfb6afb3523305e968cb48a4323ab4121c49d93e9ed4425c14480d12c5977aef`; `summary.json`: `27f2392931a65cfb160673b981d25bc63b9b0547951cf06fabb81b9797319148` (`evals/results/p018-local-provider-20261027-r2/`). A synthetic fixture independently verified valid-output, malformed-JSON, and hard deadline classification before this run.
- Standalone local probe summary: `68904b727e43f4afe3e085c0febd496e2e75eacf578538be67c8febc66716bbe` (`evals/results/p018-local-probes-20261027/summary.json`).
- Runner source: `7044b153a8a319d4b1713fa5f4a7833f08a18c8bd424dbf81b341d470ab49edf` (`tools/p018-local-capacity.py`).

## Backup and restore drill

Command: `P018_EVIDENCE_DIR=/tmp/bba-p018-restore-20261027-final tools/p018-restore-smoke.sh`.

The runner created source and destination PostgreSQL databases in a temporary container/network-none fixture, seeded three distinct states using the real Task Engine store, ran `pg_dump`, restored into a fresh database, replayed recovery, and verified the public `GET /v1/tasks/{id}` projection and independent SQL proof. All stages exited 0. The restored snapshot matched the source before recovery; `RecoverDispatching` processed one operation once (second pass zero), timeout reconciliation processed three states (second pass zero), and the recording adapter saw zero calls after restore. No new execution, permit, retry, or result was created. Three goal slots, target claims, and action locks remained held. Public projections retained one pending external action and two unknown executions; none was reported as success.

Evidence is in `/tmp/bba-p018-restore-20261027-final`. `drill-summary.json` records dump size 150998 bytes and SHA-256 `8a55d0ecd0f32a4266e79eafd47335eddc03ef2bb5daf1785fa74b80c94c3193`; independent SQL proof passed. Manifest verification, fixed sensitive-marker scan, and hash checks all exited 0; manifest SHA-256 is `e5e38e9e71a060102c1b86488041111dcdfe7933c9ee1d0619cec369aac6ac7b`. The runner destroyed the disposable container and volume.

This proves recovery and fail-closed dispatch behavior for the seeded fixture state. It is not a live database backup policy/retention test, DB failover, crash during an actual production backup, bridge outbox pump test, or P0-14 real four-account network/process fault injection.
## Final regression checks

- `tools/kernel.sh test-all` exited 0: full Go package suite with race detection, including compilation of `cmd/p018-restore`.
- RefGate passed 499 local refs; contract validation passed 86 components and 215 fixtures, zero errors/warnings; 54 contract tests passed.
- Existing Character runtime/Character/local-L1 fixture tests under Node 26.7.0 passed 15/15. The known ChatLuna fake-provider teardown exception remained visible. This run is supplementary and does not replace the recorded Node 22 offline freeze baseline.
- The standalone capacity runner's valid-response, malformed JSON, and slow-trickle deadline cases were exercised through a temporary loopback HTTP fixture. The final request-socket retention fix was smoke-tested separately; no production service was used for that fixture check.

The kickoff rerun (`P018_EVIDENCE_DIR=/tmp/bba-p018-kickoff-20261027T121113Z`) also passed; its compact evidence copy is under `evals/results/p018-restore-kickoff-20261027/`. It again reports zero adapter calls after restore, 1 recovered dispatch, 3 reconciliations, zero new execution/permit/retry/result, and unresolved states displayed. `tools/kernel.sh test-all` was rerun in the same kickoff and passed with Go race detection.

## Latest dependency rerun

The final restore drill was rerun on 2026-10-27 in a disposable PostgreSQL 16 container with `--network none`. The audited run exited `run_exit=0`, `proof_exit=0`, `scan_exit=0`, `hash_exit=0`; the full `tools/kernel.sh test-all` passed against this source tree. It recovered one dispatch exactly once, reconciled three seeded unresolved states exactly once, retained all reserved slots/claims/action locks, displayed one pending and two unknown executions, made zero adapter calls after restore, and created zero new executions, permits, retries, or results. Evidence: `/tmp/bba-p018-restore-freeze-final-20261027/`; `drill-summary.json` SHA-256 `e7bb4a18c4c75c48d72389ee72860139cd8877b1ec3dab8509cbdddafbbc4854`; manifest SHA-256 `1044b055341d65c8cb8dc89b1a1a747a7358bf92009bb73bef0b19f101223af5`; restore runner/source fingerprint `82a65be111b337bdb85e1640e2bd21956c6f50920138397cd642457b35720e34`. The environment records PostgreSQL image digest, code files, network isolation, and zero credential/QQ access. This freezes only the isolated restore fixture behavior.

The capacity runner was also executed against a local loopback fake HTTP server at 1/4/8/20 concurrent synthetic requests. All fixture responses passed JSON, arithmetic, and per-session marker checks; no retries occurred. Evidence: `/tmp/bba-p018-capacity-fixture-audit-20261027/`; sessions SHA-256 `9244f31dbbede37b7f90367febc8843aed74a185a335bc1d34409eee6f968510`, summary SHA-256 `090b77ecdf645c0488bb4db21a479779d7ed996252ca7a03c8e20e8abe748c9e`; runner SHA-256 `7044b153a8a319d4b1713fa5f4a7833f08a18c8bd424dbf81b341d470ab49edf`. The report labels this provider-protocol fixture, not a model, Character runtime, gateway queue, or capacity result; server CPU/RSS/VRAM/queue metrics remain null. The previously recorded direct-provider run remains the only local model burst: it missed the proposed <=8 s p95 at 4/8/20 and timed out at 8/20. No new model credential was used.

The earlier statement that the restore rerun did not clear a "current P0-14 live-read failure" was stale. R23's 2026-10-27 11:40 UTC read-only snapshot failed; later R24 at 12:21 UTC passed four-account read-only identity checks and schema-12 isolated engine smoke. R24 supersedes R23 as the latest read-only/fixture snapshot but does not satisfy P0-14 controlled business writes or real four-account fault injection. Both historical outcomes remain documented in the P0-14 report.

## Remaining acceptance gates

- P0-14 controlled business writes and real four-account network/process failure injection remain pending. P0-18's design dependency on P0-14 is therefore unmet despite R24's later read-only/fixture PASS.
- P0-17 has one isolated OneHop provider/schema/ledger smoke, but overall model evaluation and approved Summary message-body/reference source remain missing. Production model routes remain disabled.
- P0-16 still requires approved redacted tuning and locked holdout sets, duplicate/leak checks, human blind review, immutable artifact/template provenance, and performance/resource measurements through the intended local ChatLuna/Character model path.
- P0-15 offline source/fixture scope remains frozen; deployment management-plane re-verification remains pending.

No P0-18 overall acceptance or production readiness is claimed. The final restore-fixture scope is reproducible at source fingerprint `82a65be111b337bdb85e1640e2bd21956c6f50920138397cd642457b35720e34`; its evidence manifest is `1044b055341d65c8cb8dc89b1a1a747a7358bf92009bb73bef0b19f101223af5`. The capacity runner source is identified by SHA-256 above. These are implementation/fixture fingerprints, not an accepted capacity baseline.
