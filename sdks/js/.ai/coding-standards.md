# JS SDK coding standards

Scope: `sdks/js/packages/*` (`@tasktrooper/core`, `browser`, `react`, `vue`, `angular`, `react-native`, `replay`, `node`).
Cross-cutting rules (language, commits, PII, cardinality) live in [`../../../.ai/coding-standards.md`](../../../.ai/coding-standards.md);
wire format and public API surface live in [`../../../.ai/sdk.md`](../../../.ai/sdk.md) and
[`../../../.ai/contracts.md`](../../../.ai/contracts.md). This file only covers what is specific to the JS packages.

## Package map

| Package | Role | Depends on |
|---|---|---|
| `core` | Platform-neutral RUM client: event model, session, batching, transport, retry, offline queue, URL scrubbing | nothing (no runtime deps) |
| `browser` | Web RUM: views, errors, network, actions, Web Vitals, IIFE build `window.Trooper` | `core`, `web-vitals` |
| `react` / `vue` / `angular` | Thin framework adapters over the `browser` singleton API | `browser`, `core` + framework as peer |
| `react-native` | RN RUM + native bridge (TurboModule `TrooperRum`) | `core`, `react-native` peer |
| `replay` | rrweb session replay as a `browser` integration | `@rrweb/record`, `browser` peer |
| `node` | OpenTelemetry distro (OTLP/protobuf), `trooper.*` conventions, framework/queue helpers | `@opentelemetry/*` |

- Platform-neutral logic goes in `core`, never duplicated per platform. If RN and web both need it, it belongs in `core`.
- `node` is separate: it is OTel-based and does not use `core`. Don't import `core` from `node` or vice versa.
- Adapters stay thin: they call the `browser` public API (`init`, `track`, `setView`, `captureError`…), they don't reach into `BrowserSdk` internals. Reference: `packages/vue/src/index.ts`.

## Build & module format

- Build tool is **tsup** only (`tsup.config.ts` per package). Output: ESM (`dist/*.js`) + CJS (`dist/*.cjs`) + `.d.ts`/`.d.cts`, with sourcemaps.
- `package.json` must keep the dual `exports` map (`import`/`require`, each with `types` first) plus `./package.json`. Copy an existing package (e.g. `packages/core/package.json`) when adding one.
- New subpath entry → add it to tsup `entry` **and** `exports` (see `packages/react`: `./react-router`, `./nextjs`, `./tanstack-router`).
- Targets: `es2020` for browser/RN packages, `es2022` for `angular`, `node22` (`platform: 'node'`, `shims: true`) for `node`.
- Script-tag builds (`browser`, `replay`) are a second tsup config with `format: ['iife']`, `globalName`, `noExternal: [/.*/]`, `minify`. Keep the global names `Trooper` / `TrooperReplay`.
- `react` emits a `"use client";` banner (RSC / Next.js App Router). Keep it for every React entry.
- `"type": "module"`, `"files": ["dist", "README.md", "LICENSE"]` (RN additionally ships `src`, `android`, `ios`, podspec).
- Use `import.meta.url` only in `node` (tsup shims it for CJS; `register.ts` relies on that).

## Side effects & tree-shaking

- `"sideEffects": false` on every package except `node`, whose only side-effectful entry is `./register` (`dist/register.{js,cjs}`).
- Importing a module must not do work: no listeners, timers, global patches or network at module top level. Everything starts in `init()`.
- Test-only metadata stays out of the entry point (e.g. `core/src/schema.ts` is not exported from `index.ts`).
- Bundle budget: `core + browser + web-vitals` **< 15 KB min+gzip**, enforced by `pnpm --filter @tasktrooper/browser size` (`packages/browser/scripts/size.mjs`). `replay` has its own `size` script. Not run in CI — run it when touching `core` or `browser`.
- Don't add runtime dependencies to `core`/`browser` without checking the size report. Prefer tiny local helpers over libs.

## TypeScript

- All packages extend `tsconfig.base.json`: `strict`, `noUnusedLocals`, `noUnusedParameters`, `noImplicitOverride`, `noFallthroughCasesInSwitch`, `isolatedModules`, `verbatimModuleSyntax`.
- `verbatimModuleSyntax` → type-only imports must use `import type` / `type` specifiers:
  ```ts
  import { safe, type RumEvent } from '@tasktrooper/core';
  ```
- `node` overrides `lib`/`target` to `ES2022` without `DOM`; never use DOM globals there.
- No `any` in public types. Use structural "Like" interfaces for optional peers instead of importing their types (e.g. `RouterLike` in `vue`, `FetchLike` in `core/src/transport.ts`) so optional peers stay optional.
- Wire types in `core/src/types.ts` use the proto's `snake_case` field names; everything else is camelCase.

## Never crash the host

The rule from root `AGENTS.md` #5, implemented by two helper sets — use them, don't hand-roll:

| Where | Helpers | Counter |
|---|---|---|
| RUM (`core` and everything on top) | `safe(where, fn)`, `guard(where, fn)`, `reportInternal(where, err)` in `core/src/util.ts` | `internalErrorCount()` |
| `node` | `safe(component, fn, fallback)`, `recordError(component, err)` in `node/src/errors.ts` | `trooper.sdk.errors{component}` metric + `internalErrorCount()` |

- Every public function is safe before `init`, during SSR and after `close()`. Pattern (`browser/src/index.ts`):
  ```ts
  export function track(name: string, props?: Record<string, unknown>) {
    safe('track', () => sdk?.client.capture({ type: 'CUSTOM', custom: { name: String(name), props: toStringMap(props) } }));
  }
  ```
- `init()` never throws: invalid DSN, missing `window`, etc. → `reportInternal('init', e)` and return `undefined`. `parseDsn` itself throws; callers swallow.
- Wrap every host callback the SDK registers (event listeners, router hooks, integration `setup`, `onEvent`) in `safe`/`guard`. See `BrowserSdk.listen` and `Hub.emitEvent`.
- The `where`/`component` label is a short static string (`'transport.fetch'`, `'web-vitals'`) — never interpolate data into it (it becomes a metric label in `node`).
- Promises returned to the host never reject: `flush()`/`close()`/`forceFlush()`/`shutdown()` end in `.then(…, e => reportInternal(…))`.
- `beforeSend` that throws **drops** the event (fail closed — it may exist to strip PII).
- Exception: errors thrown by the *host's own* code that the SDK wraps (e.g. the handler in `node`'s `processMessage`) are recorded on the span and **re-thrown** unchanged. Never swallow host errors, never rethrow SDK errors.
- Console output only when `debug: true`, prefixed `[trooper]`.

## Transport, batching, backoff

Reuse `core`; don't write a second delivery loop.

- `Client` (`core/src/client.ts`) batches ≤ 100 events (`MAX_BATCH`) or 5 s (`flushIntervalMs`), FIFO, one in-flight drain.
- `classifyStatus`: 2xx ok · 429/5xx/network → `retry` (honor `Retry-After`) · other 4xx → `drop`.
- `backoffDelay`: exponential 1 s → 60 s with 50–100 % jitter. Failed/offline batches go to the `OfflineStore` (memory by default, IndexedDB on web via `browser/src/idb.ts`, `KeyValueOfflineStore` over the app-provided `storage` on RN; capped by `OFFLINE_MAX_BYTES`).
- Page hide / app background → `flush({ beacon: true })`: synchronous send, keepalive bodies split under `KEEPALIVE_LIMIT` (60 000 B).
- The SDK's own requests use the **unpatched** fetch captured before instrumentation, and the ingest URL is in `ignoreUrls` so the SDK never records itself.
- Auth header is `x-trooper-key`; `credentials: 'omit'`. Endpoint and payload shape: `contracts.md` §2.
- `node` uses OTel `Batch*Processor`s with a bounded queue (`maxQueueSize`, drop on overflow — never block).

## Config & DSN

- DSN format `https://<key>@<host>[:port][/prefix]/<project_id>` (`contracts.md` §2). RUM parser: `core/src/dsn.ts` (regex, because RN's `URL` polyfill drops user-info). Node parser: `node/src/config.ts` (`new URL`).
- Common RUM options live once in `CoreOptions`; platform packages extend it (`BrowserOptions extends CoreOptions`). Don't redeclare shared options.
- `node` options resolve option → env (`TROOPER_*`, `OTEL_*`) → default in `resolveConfig`. Document the env var in the option's JSDoc.
- Every option has JSDoc with its default. Defaults favor privacy and low overhead (`captureConsoleErrors` off, `profiling` off).
- `enabled: false` turns the SDK into a no-op; helpers keep working.

## PII & cardinality

- URLs are scrubbed before `beforeSend`: `stripQueryValues` keeps keys, drops values and user-info (`core/src/url.ts`). Never send raw `location.href`.
- View names are route templates: from the framework router when available (`vueRouteTemplate`, react-router/Next/TanStack entries), else `normalizePath` (`/users/123` → `/users/:id`). `node` uses `normalizeRoute`/`templateFromRequest`.
- No request/response headers or bodies, ever, by default. Only sizes (`bodySize`).
- User data only via `identify(id, traits)` — traits are opt-in and go through `toStringMap` (≤ 100 keys, values truncated to 512 chars).
- Free text is truncated (`truncate`): error message 2000, stack 16 000 chars.
- Replay masks all text and inputs by default (`replay/src/privacy.ts`); unmasking is opt-in per selector.

## Public API conventions

- Singleton functional API per platform: `init`, `identify`, `clearUser`, `track`, `captureError`, `setView`, `flush`, `close`, `getSdk`. Keep names identical across browser/RN/adapters.
- Adapter names: `TrooperProvider`/`useTrooper`/`ErrorBoundary` (React), `TrooperKey` plugin (Vue), `provideTrooper` (Angular), `trooperExpress`/`trooperKoa`/… (node).
- Optional framework peers go in `peerDependenciesMeta` as `optional`, with the integration in its own subpath entry so importing the main entry never requires them.
- Each package exports `SDK_NAME`/`SDK_VERSION` (or `VERSION`) constants; they must match `package.json` `version` (see Versioning).
- Exported surface = `src/index.ts` only. Additive changes are fine; renames/removals need a `.ai/sdk.md` update first.

## Versioning & release

- All packages share one version. Bump with `sdks/js/scripts/version.sh X.Y.Z`, then tag `js-vX.Y.Z` → `.github/workflows/release-npm.yml` publishes with provenance.
- `version.sh` only edits `package.json`; also update hard-coded constants (`core/src/index.ts` `CORE_VERSION`, `browser`/`react-native`/`replay` `SDK_VERSION`, `node/src/attrs.ts` `VERSION`).
- Internal deps are `workspace:*` (`replay` peers on `workspace:^`); pnpm rewrites them at publish.
- Commits: `feat(sdk-js): …`, `fix(sdk-js): …`, release `chore(sdk-js): release X.Y.Z`.

## Do / Don't

- Do put shared logic in `core` and add a test there; don't copy it into `browser` and `react-native`.
- Do wrap every callback the SDK hands to the host in `safe`; don't let an `addEventListener` handler throw.
- Do keep imports side-effect free; don't start timers in module scope.
- Do use `Like` interfaces for optional peers; don't `import type` from an optional peer in the main entry.
- Don't log with `console.*` outside `debug`; don't add metric labels/attributes with ids or raw URLs.
