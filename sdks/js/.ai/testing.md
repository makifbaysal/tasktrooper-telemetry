# JS SDK testing

Scope: `sdks/js/packages/*`. Repo-wide testing rules: [`../../../.ai/testing.md`](../../../.ai/testing.md).
Coding rules for these packages: [`coding-standards.md`](coding-standards.md). Wire contract under test:
[`../../../.ai/sdk.md`](../../../.ai/sdk.md), [`../../../.ai/contracts.md`](../../../.ai/contracts.md).

## What runs where

| Gate | Command | Runs |
|---|---|---|
| CI job `sdk-js` (`.github/workflows/ci.yml`) | `pnpm -r --filter "./sdks/js/packages/**" build` then `… test` | tsup build + `vitest run` in every package |
| `make check` (repo root) | same two commands | same as CI; must end with `check OK` |
| Release (`release-npm.yml`, tag `js-v*`) | version check, build, test, publish | same tests, then npm publish |
| Manual only | `pnpm --filter @tasktrooper/<pkg> typecheck` | `tsc -p tsconfig.json` (src + test) |
| Manual only | `pnpm --filter @tasktrooper/browser size` / `@tasktrooper/replay size` | bundle budget (browser < 15 KB gzip) |

- `typecheck` and `size` are **not** in CI or `make check`. Vitest strips types without checking them, so run `typecheck` yourself after changing types or public signatures. It resolves sibling packages through `dist/*.d.ts`, so build first.
- Run `size` whenever you touch `core` or `browser`; it exits 1 over budget.

## Running locally

```bash
pnpm install                                            # repo root
pnpm -r --filter "./sdks/js/packages/**" test           # all packages
pnpm --filter @tasktrooper/core test                    # one package
pnpm --filter @tasktrooper/browser exec vitest run test/views.test.ts   # one file
pnpm --filter @tasktrooper/core exec vitest run -t "flushes after 5 s"  # one test by name
pnpm --filter @tasktrooper/node exec vitest              # watch mode
```

No build needed for tests: `vitest.shared.mjs` aliases `@tasktrooper/core`, `browser` and `react` to their `src/index.ts`.
A new package that is imported by tests of another package must be added to that alias map.

## Layout & naming

- Tests live in `packages/<pkg>/test/`, never next to sources. Name: `<area>.test.ts` (`.test.tsx` for JSX).
- Shared setup in `test/helpers.ts`; module mocks in `test/mocks/` (wired through `resolve.alias` in `vitest.config.ts`).
- Every package has its own `vitest.config.ts`; packages that consume siblings spread `alias` from `../../vitest.shared.mjs`.
- `tsconfig.json` includes `test/`, so tests obey the same strict rules (`noUnusedLocals`, `import type` …).

## Environments

| Package | `test.environment` | Notes |
|---|---|---|
| `core` | `node` | Pure logic, no DOM |
| `browser`, `react`, `vue`, `angular`, `replay` | `happy-dom` | Not jsdom. `browser/test/lifecycle.test.ts` adds `fake-indexeddb/auto` for the offline queue |
| `react-native` | `node` | `react-native` aliased to `test/mocks/react-native.ts` (`AppState.__emit`, `TurboModuleRegistry`) |
| `node` | `node`, `testTimeout: 20_000` | Real OTel SDK, real HTTP servers |

- Add happy-dom gaps as local polyfills in the test file (see `replay/test/rrweb-real.test.ts` `beforeAll`), not in `src`.
- Framework mocks for optional peers go in `test/mocks/` + alias (`react/test/mocks/next-navigation.ts`, `next-router.ts`).

## Fakes: transport and ingest

Never hit the network. Pick the lowest layer that covers the behavior:

1. **Fake `Transport`** (core logic: batching, retry, offline, ordering) — inject via `ClientConfig.transport`; queue scripted `SendResult`s. Reference: `mockTransport()` in `core/test/client.test.ts`.
   ```ts
   const t = mockTransport([{ status: 'retry', retryAfterMs: 2000 }]);
   const c = new Client({ dsn: 'http://k@localhost:4318/1', app: 'app', platform: 'PLATFORM_WEB', sdk: { name: 't', version: '0' }, transport: t.transport });
   ```
2. **Stubbed `globalThis.fetch`** (platform SDK end-to-end: what lands in `/v1/rum`) — `setup()` in `browser/test/helpers.ts`, `stubIngest()` in `react/test/helpers.ts`. Assert on decoded `RumBatch` events, not on call counts.
3. **Fake ingest HTTP server** (`node`) — `startFakeIngest()` + minimal OTLP/protobuf decoder in `node/test/otlp.ts`; used by `node/test/e2e.test.ts`.
4. **In-memory OTel** (`node` unit tests) — `setupTracing()` in `node/test/helpers.ts` (`InMemorySpanExporter`, real sampler/processor/propagator). Always call its `teardown()`.

- Use the dev DSN shape `http://tt_dev_ingest_key@localhost:4318/1`; never a real key.
- Heavy third-party libs are mocked through a shared factory: `vi.mock('@rrweb/record', async () => (await import('./rrweb-mock')).rrwebMock())`. Keep **one** real-library test (`replay/test/rrweb-real.test.ts`) to catch mock drift.

## Timers & async

- Batching/backoff tests use fake timers: `beforeEach(() => vi.useFakeTimers())`, `afterEach(() => vi.useRealTimers())`.
- Use `await vi.advanceTimersByTimeAsync(ms)` when promises run inside the timer (flush, retry, drain); plain `advanceTimersByTime` only for sync callbacks.
- Assert boundaries exactly (`4999` → nothing, `+1` → sent), as `core/test/client.test.ts` does.
- Backoff jitter: pass a deterministic `random` to `backoffDelay(attempt, () => 0.5)` instead of mocking `Math.random`.
- End-to-end browser tests flush explicitly (`await flush()`) after a short real wait for view settle; don't depend on the 5 s timer.

## Isolation

- SDKs are singletons. Every test that calls `init()` must `close()` in `afterEach` (`teardown()` in `browser/test/helpers.ts` also resets `history`).
- Reset module-level state through the provided test hooks (`resetSelfMetrics()` in `node/src/errors.ts`, `heartbeatTiming` for heartbeat timing). Add a hook rather than reaching into privates.
- Restore anything you patch on `globalThis` (`fetch`, `XMLHttpRequest`, `console`, OTel globals via `trace.disable()` etc.).

## Required test categories

Add tests in the same PR as the change, in these categories:

- **Never throws** — public API before `init`, after `close`, with a throwing `beforeSend`/integration/listener, with garbage input (circular objects, `null`). Assert `not.toThrow()`; for new swallow paths also assert that `internalErrorCount()` increased (done in `node`, not yet in `core`/`browser` tests). References: `browser/test/errors.test.ts`, `replay/test/replay.test.ts` ("never throws into the host"), `node/test/errors.test.ts`.
- **Host errors pass through** — wrappers re-throw the host's own exception unchanged (`node/test/messaging.test.ts` `processMessage`).
- **Delivery** — batch size ≤ 100, 5 s flush, 2xx/429/5xx/4xx classification, `Retry-After`, backoff, offline queue FIFO, beacon split under `KEEPALIVE_LIMIT`.
- **PII / cardinality** — URLs scrubbed (`core/test/url.test.ts`), view names are templates (router tests in `react/test/routers.test.tsx`, `vue`, `angular`), replay masking (`replay/test/privacy.test.ts`), no headers/bodies captured (`*/test/network.test.ts`).
- **Contract** — wire field names. `core/test/proto.test.ts` parses `proto/rum/v1/rum.proto` and fails if `core/src/types.ts` (via `core/src/schema.ts` `FIELDS`/`ENUMS`) drifts. Changing the proto or a wire type → update `types.ts` **and** `schema.ts`; the compiler rejects a non-exhaustive `FIELDS` list.
- **Config** — DSN parsing (valid, prefix, port, invalid → throws) and env resolution (`node/test/config.test.ts`).

## Style

- `describe` per feature, `it('<behavior in plain English>')`; one behavior per test.
- Table-style cases via `it.each` for parsers/normalizers (`normalizePath`, `parseRetryAfter`, `classifyStatus`).
- Assert on observable output (events in the batch, span attributes, `stats`), not on private fields.
- No snapshot files; write explicit expectations so contract changes are reviewed line by line.

## Do / Don't

- Do run `typecheck` and `size` locally when relevant — CI won't.
- Do fake time; don't `sleep` for seconds to wait for a flush.
- Do use the helpers in `test/helpers.ts`; don't re-implement a fetch stub per file.
- Don't import from `dist/` or rely on a prior build in tests.
- Don't leave the singleton initialized between tests.
