# P0-15 Character Runtime Offline Acceptance

Date: 2026-10-27

## Scope and boundaries

This acceptance exercised only the source tree at `koishi/character-runtime` in offline containers. It did not operate existing services, deploy code, log in to QQ, call real QQ read/write APIs, access a production database, or read a Character design snapshot. Real QQ reads: 0. Real QQ writes: 0.

OpenAPI marks P0-15 `chat-send-permits` `x-feature-enabled-in-p0: true`; this is contract metadata only. Production API/backend deployment, credentials, and live adapter routing were not exercised or verified. P0-17 model-request/apply flags remain `false`. No production runtime enablement is claimed.

## Acceptance and freeze (2026-10-27)

**PASS: P0-15 offline source/fixture acceptance.** The named design acceptance behaviors are covered by the targeted Node 22 run `node --import tsx --test test/runtime.test.ts test/character-runtime.test.ts` (`exit 0`, 11/11 tests passed): per-account group/private isolation, active group speaker lease, managed/cross-account bot suppression, text-only output, permit consume-once, direct/internal send denial, mandatory outcome, terminal unknown with no resend, and the pinned Character integration. The independent compiled-entry smoke and durable restart/fail-closed checks are recorded above. No real QQ or production service was used.

## Authorized synthetic LLM probe (2026-10-27)

After the offline acceptance, the owner explicitly authorized use of the supplied OneHop gateway for synthetic LLM checks. A standalone text-only persona probe returned `在合成测试里，2 + 2 等于 4。` from `deepseek-v4.1-flash-1017` in 2.40 s; it used no real persona, chat history, runtime, Character pipeline, account, QQ API, or send transport. This is gateway/model text-generation evidence only, not a Character integration test or a freeze prerequisite. P0-15's frozen offline runtime scope and source/dependency digest are unchanged.

Freeze does not certify deployment/management-plane integration or pass the separate PathRouted deployment re-verification and P0-14 four-account controlled-write/failure-injection gates. Those remain open; no running service was touched.

Acceptance baseline (SHA-256): `koishi/character-runtime/src/runtime.ts` `0b3c7e54c4c9bc03ed5f4053b30280ef5e5e72dc5fe4c46cf9fac8d3d85a632e`; `src/ingress.ts` `5c6975e980d0659175f2e8fc9d574d0837cc88417bb3c35ac2b168594d362f87`; `src/transport.ts` `a7430888dd55b58e01af9bf42646c18505e897faed3c2edeeadd62dbe965c29f`; `src/gate.ts` `f71f09c562745ea5153358f547f13b081adae0d94babe5c36147078a77030838`; `test/runtime.test.ts` `4b6d9e6d61b443ee45fe4cf624f5c78ba4111ded81fd167c9a097d14d93698c9`; `test/character-runtime.test.ts` `7fb12b899f2e7024a7e44ba2750bfc0ba01d9a9dabc1f63b270d97058ae9d87e`; `package-lock.json` `2f20ade8696364d3866a7bf0c5514eca04c778ed59997216312e9514c241fc78`.

The aggregate freeze digest is `64d7d476d7625009dd723a39bb2a7916f796e12aef161711f81398c524c8197e`, computed as the SHA-256 of sorted `sha256sum` output for `koishi/character-runtime/src/*.ts`, `test/*.ts`, `test/helpers/*.ts`, `test/harness/*.ts`, `package.json`, and `package-lock.json`. It identifies the complete offline runtime source/test/dependency baseline; documentation changes are outside the digest.

## Implemented runtime boundary

- Each account has a separate Koishi App, durable database, cache/preset directories, persona, backend client, and send ledger.
- The global ingress middleware rejects account/self mismatches and managed-bot events, then requires trusted backend context, an active speaker lease for groups, and an open trigger before continuing to Character.
- Koishi group auto-assignment is enabled so an admitted group event with no prior channel assignment reaches Character middleware. The runtime still uses one App/database per account; backend ingress decides admission.
- Outbound calls accept plain text only. One permit is issued and consumed before one adapter call. The local ledger records the terminal result and the backend outcome endpoint is mandatory. Adapter errors/timeouts become terminal `unknown`; they are never retried.
- Runtime construction does not start adapters unless `startApps: true` is explicit.

## Pinned dependencies

| Package | Version |
| --- | --- |
| Node | 22 (`node:22-bookworm`) |
| Koishi | 4.18.11 |
| ChatLuna | 1.4.4 |
| ChatLuna Character | 0.0.235 |
| OneBot adapter | 6.9.5 |

## Executed evidence

| Check | Result | Evidence boundary |
| --- | --- | --- |
| `npm run typecheck` | Passed | Node 22, container `--network none` |
| `npm run build` | Passed | Node 22, container `--network none` |
| `npm test` | 12/12 passed | Runtime, persistence, gate and pinned Character integration tests; fake backend/transport and temporary state |
| Runtime acceptance suite | 8/8 passed | Two-account isolation, group speaker lease, private epoch, managed-bot suppression, rich content rejection, direct internal-action denial, terminal unknown and no retry |
| Actual pinned Character integration | Passed | Pinned Character + fake ChatLuna provider/model + fake transport; exactly one gated text send and one mandatory `sent` outcome |
| Durable database test | Passed | Restart persistence and fail-closed reads/writes after injected commit failure |
| Compiled-entry smoke | Passed | `lib/index.js` public API; two isolated Apps/databases/roots, A-only text send, one trigger/permit/consume/`sent` outcome, ledger restored after manager restart, B sends 0; `--network none`, fake backend/transport, real QQ calls 0 |

The compiled-entry smoke exercises a simulated Character output through the compiled public API; it is independent of the test suite. The separate pinned Character integration loads the actual Character plugin with a fake ChatLuna model provider and fake transport. The fake backend listens only on loopback and uses distinct account credentials. Neither check called the production API or QQ.

## Observed limitations

- Character tokenizer startup attempted `tiktoken.pages.dev`. DNS failed under `--network none`; the upstream fallback used approximate token counts. No successful external connection was observed.
- ChatLuna emitted a teardown exception in `uninstallPlugin` while the fake provider was being removed. TAP still passed. Production teardown behavior was not exercised.
- The tests prove source behavior against these fixtures, not production backend routing, database durability/storage characteristics, runtime credential provisioning, or QQ delivery.

## Not completed

- PathRouted deployment followed by management-plane re-verification remains pending; no deployment was performed here.
- P0-14 controlled writes against the four real accounts and failure injection remain pending; no such writes or fault injection were performed here.
- Production API/database enablement and actual QQ send/read behavior remain unverified.
