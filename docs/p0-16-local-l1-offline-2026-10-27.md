# P0-16 Local L1 Offline Implementation Record

Date: 2026-10-27

## Status

**OVERALL P0-16: BLOCKED; offline implementation sub-scope accepted and frozen.** The required real-model evaluation is not complete, so P0-16 does not pass design acceptance. The local provider/evaluator implementation has passed only the offline source/protocol and simulated-fixture checks recorded here. Freeze applies to that offline source scope and its package/dependency pins; any change requires a new reviewed acceptance run. This freeze is not production enablement or approval to call a model service.

## Scope and evidence boundary

- Source changed under `koishi/character-runtime`; dependencies are pinned in that package. Node 22 `--network none` was used for typecheck, build, and tests.
- The provider uses the pinned ChatLuna `ChatLunaPlugin`, `PlatformModelClient`, `ModelRequester`, and `ChatLunaChatModel` extension points. It is explicit opt-in; it is not added to the manager's default providers or to production configuration.
- Provider requests require numeric loopback HTTP and a `/v1` base URL. It sets `stream: false`, disables SDK/provider retries, rejects tools and arbitrary request overrides, limits queue/concurrency/deadline and response/output size, and aborts in-flight calls when its Koishi scope is disposed.
- The offline tests used a temporary `127.0.0.1` fake HTTP server plus the real pinned ChatLuna client/model. Fixture responses and credentials prove request/protocol wiring and local guard behavior only; the separately authorized OneHop probe is documented below and is not this provider's result.
Authorized cloud substitute probe (2026-10-27): the owner supplied an OneHop endpoint and requested live LLM tests. A separate synthetic text-only probe returned a short correct answer from `deepseek-v4.1-flash-1017` in 1.26 s. It did not use the local Swift weights, locked holdout, or Character/ChatLuna L1 path. It is gateway evidence only.

Environment boundary for the original offline acceptance: it used no real QQ reads/writes, existing services, production database, credential file, or deployment. A later, separately authorized local-model probe and its credential handling are documented in the dedicated section below.

## Offline implementation acceptance and freeze (2026-10-27)

**PASS: offline implementation sub-scope only.** `node --import tsx --test test/local-l1.test.ts` under Node 22 (`--network none`) passed 4/4 targeted provider tests: ChatLuna request wiring, text/tool-output restrictions and no retry, bounded queue/deadline behavior, and rejection of remote and HTTPS loopback endpoints. The compiled public entry also exposes the provider and rejects HTTPS loopback. The simulated evaluator completed 24/24 expectations as recorded below. These checks do not satisfy the real-model gate.

## Authorized local Swift provider probes (2026-10-27)

The owner provided `/home/sun/桌面/1212.txt` and authorized synthetic local-model calls. The endpoint was read from the file and called directly; endpoint, key, and raw responses are intentionally absent. Inventory and completions returned API model ID `swift-1.5-iq3_s`; `/models` declared `n_ctx=262144`, also present in the static launch config. Static config names the Swift-Qwen3.8-Flash-Next-GSQ-RCO-abliterated IQ3_S GGUF shards, tokenizer pack, server script, GPUs `[0,1]`, and `--max-context 262144`. SHA-256 of the on-disk artifacts: shard 1 `638fbd42a01c7b39a9b7b21c5c8dc5eb1876222d8bd268d471304b647fcd6f94`; shard 2 `091a4f6a53697a12a2ea1e5e1d61414410d50c0d44e971304602bba6873fd23d`; `chat_template.jinja` `c3cf9e34abf4f9e36c2d72165aa9c132d3e2a725b6c2586aaa3a8af9d7a81041`; `tokenizer.json` `c0bd435efbc6c3b28964b47999afe16c4ad268c7c7c4e8cb3af410796a6f30bf`; serving config `90aaded4bb737617fbd62f093d0146a76806767247a9961373bdb1ab37432f77`; API server script `f15efbe6a1e3a97d58ddbc0fa9a68ce544c90f6c1b03cbf6cecf50c625a5a5f8`. These identify files present on disk; they do not attest the running service loaded these exact bytes or establish the model author's revision.

The probes in `evals/results/p018-local-probes-20261027/summary.json` are independent, synthetic, provider-only attempts. A 32-token attempt ended with `finish_reason=length` and an incomplete incorrect answer; a separate 256-token attempt returned the expected short answer. A streaming probe returned the expected answer with first visible content at 2075 ms and total 2078 ms. One structured synthetic session probe returned the expected three-field JSON and answer in 4333 ms. These are smoke cases, not a golden score, holdout, or Character/ChatLuna integration result.

The 1/4/8/20 provider-only synthetic capacity ladder is recorded in `evals/results/p018-local-provider-20261027-r2/`. Completed requests had matching synthetic session markers; no automatic retries occurred. Results: 1/1 completed, all-request p95 4679.867 ms; 4/4, 24549.785 ms; 8/8 had 6 completed and 2 hit the 30 s deadline, with 2 unvalidated outputs and no schema failures; 20/20 had 6 completed and 14 hit the 30 s deadline, with 14 unvalidated outputs and no schema failures. All completed structured responses matched their synthetic expected values. Server CPU/RSS/GPU VRAM and Gateway queue time remain null. The 4+ session latency and timeout rates miss the architecture's proposed <=8 s short-reply p95 target; this was one structured burst per level, not a Character short-reply SLO run.

This probe contacted the historically recorded local model service under the owner's explicit authorization; it did not contact QQ, use the Character runtime, or access a production database. The supplied credential was read locally for process-memory injection and was not retained in source or result files. Because the file contents were exposed by the interactive file-reading tool during setup, rotate the model key before further use.

Frozen offline acceptance baseline (SHA-256): `koishi/character-runtime/src/local-l1.ts` `320024ca59a3d599bbe09ffac21ca5d167fead808586c4fbd578f49d688676af`; `koishi/character-runtime/test/local-l1.test.ts` `5205e5030e3780c05d47a7b0f03bcedfd50888536e25c1f5d85c1af88d166686`; `evals/validators/validate_outputs.py` `b0dba176fa2c76ab608386b9a37455e8cf8ed80a6791bc594b61ad6eb44bc77d`; `evals/fixtures/simulated_outputs.jsonl` `a72d1bcf7518af275eb557d0c91801b5c71e343ae7fe7cf3579fb0e89637ac01`; `evals/results/p0-16-simulated/outputs.jsonl` `59eb1358a87e532434aa047a2bf28f31c56f47f187faed9f10b440abf5074486`; `contracts/openapi.yaml` `30f0aefd41913ffaa0e7e596a811ffcf794de7d51ab313a5fc328d68bb10c288`; `contracts/schemas/models.json` `df4aa7f0e67216ad51c4021b7a74a3ba81c617de568270e5bf294b54947416f6`; `koishi/character-runtime/package-lock.json` `2f20ade8696364d3866a7bf0c5514eca04c778ed59997216312e9514c241fc78`.

Aggregate offline freeze digest: `df79991c3bd684683c0763bd272f46d3ba3058ae14a4189394056f64ade3ca4a`, computed as the SHA-256 of sorted individual `sha256sum` output for the complete runtime sources/tests/helpers, eval package/validator/simulated fixture/result JSONL, relevant OpenAPI and model/bridge schema files, and runtime package manifests. It is an integrity baseline, not a model-evaluation result; documentation is excluded.

## Offline checks

| Check | Observed result | Evidence level |
| --- | --- | --- |
| `npm run typecheck` (Node 22, `--network none`) | Passed | Source/type contract |
| `npm run build` (Node 22, `--network none`) | Passed | Compiled package |
| `npm test` | 16/16 tests passed (1 suite; TAP reports 9 top-level tests) | Offline fixtures; includes P0-15 suite and four L1 tests |
| L1 integration | ChatLuna resolved `bba-local-l1/fixture-swift-iq3s`; fake request used `/v1/chat/completions`, fixture bearer auth, supplied system/user messages, bounded output, and `stream:false` | Simulated HTTP fixture, not a model |
| Tool response | Tool-call response rejected; exactly one request was observed for that invocation | Simulated HTTP fixture |
| Queue behavior | One active + one queued; next request rejected full. Separately, expired queue entry rejected and operation did not execute | Deterministic admission fixture |
| Compiled public entry | Export resolved; remote host and HTTPS loopback endpoints rejected with `invalid_config` | Compiled package smoke |

### Structured-output evaluator (simulated only)

`python3 evals/validators/validate_outputs.py evals/fixtures/simulated_outputs.jsonl --contracts-root contracts -o evals/results/p0-16-simulated/outputs.jsonl` exited 0: 24 cases, 5 accepted, 19 rejected, 0 case errors, and 0 expectation mismatches. A separate integrity check confirmed every result is labeled `simulated`, all 24 expected outcomes match, and every measured metadata field is null. The persisted JSONL is reproducible fixture-evaluator output, not model output.

The cases exercise contract validation for `IntentDraft`, `PlanProposal`, and `Summary`, plus closed fields, trusted account/target/message references, action and scope restrictions, task version/scope revision, deadline, attempt budget, duplicate step numbers, ambiguity confirmation, and required references. The 5/24 accepted count is a fixture composition, not schema accuracy, success rate, or a model score. No model call was made.

## Live and unmeasured criteria

| Criterion | Observed result |
| --- | --- |
| Real Swift API model ID | `/models` and completion responses returned `swift-1.5-iq3_s`. |
| Weights, tokenizer and chat-template provenance | Launch config names Swift-Qwen3.8-Flash-Next-GSQ-RCO-abliterated IQ3_S GGUF and Swift tokenizer pack; model inventory declared `n_ctx=262144`. Service/weight hash and tokenizer/chat-template revision evidence remains unavailable. |
| Real redacted intent/character/planning holdout | `null` / no approved dataset provided yet. |
| Schema/critical-field accuracy and blind personality rating | `null` / not measured; synthetic probes are not a score. |
| Direct local provider smoke | Synthetic arithmetic/structured JSON probes passed; one earlier 32-token response truncated (`finish_reason=length`). No Character/ChatLuna L1 path was exercised. |
| Direct provider-only 1/4/8/20 burst | 1: 1/1 completed, p95 4679.867 ms; 4: 4/4 completed, p95 24549.785 ms; 8: 6/8 completed, 2 hit 30 s deadline; 20: 6/20 completed, 14 hit deadline. Completed outputs passed synthetic schema/answer/marker checks. This misses proposed <=8 s at concurrency >=4 and is not Character SLO evidence. |
| Server CPU/RSS/GPU VRAM and Gateway queue wait | `null` / unavailable. Client utilization does not substitute for model-server resources. |
| Character model-path performance and session isolation ladder | `null` / not measured; direct probes did not invoke the existing loopback-only provider. |
| Online L2 upgrade prohibition during L1 overload | No Gateway is implemented in this item; provider has no online fallback. End-to-end Gateway routing remains unverified. |
| Architecture targets | Locked-holdout critical-field accuracy >=95%, structured validity >=99%, and short-reply p95 <=8s remain targets, not observed scores. |

## Blocking prerequisites and next authorization

Completing P0-16 evaluation requires owner-approved, genuinely redacted intent/character/planning tuning and locked holdout sets, checked for duplicates and answer leakage; a blind human rating rubric; service/weight/tokenizer/chat-template revision evidence; and the intended Character/ChatLuna local L1 path with server CPU/RSS/GPU, queue, TTFT and completion latency metrics. Direct local probes and P18 provider bursts above are discovery evidence only. Do not relax the existing loopback-only provider to reach the service bound on `0.0.0.0:8166`; a design change requires the owner's approval. Until the specified model path and datasets are provided and measured, keep quality and Character-path metrics null.

P0-17 `/v1/model-requests` remains production-disabled. P0-15 PathRouted deployment/management-plane re-verification and P0-14 four-account controlled writes/fault injection remain pending; neither is replaced by this model probe.


## Reproduction

```sh
cd koishi/character-runtime
npm run typecheck
npm run build
npm test
```

Use Node 22. The fixture model is labeled `fixture-swift-iq3s`; this label is not evidence that Swift was loaded or called.
