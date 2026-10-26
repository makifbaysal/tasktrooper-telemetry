# TaskTrooper Telemetry — JavaScript client SDKs (RUM)

Browser and React Native SDKs that send RUM events as JSON to `POST /v1/rum` (ADR-011, `.ai/contracts.md` §2, §9).
Wire schema: [`proto/rum/v1/rum.proto`](../../proto/rum/v1/rum.proto) — snake_case field names, enums as strings.

| Package | What |
|---|---|
| [`@trooper/core`](packages/core) | Platform-neutral engine: event model (mirrors rum.proto), DSN, session (30 min idle / 4 h max), batching (≤100 events / 5 s), retry/backoff, offline queue, sampling, `beforeSend`, URL scrubbing, fetch/XHR instrumentation |
| [`@trooper/browser`](packages/browser) | Web: views (initial + SPA), Web Vitals, errors, fetch/XHR + `traceparent`, clicks / rage / dead clicks, `sendBeacon` on hide, IndexedDB offline queue |
| [`@trooper/react`](packages/react) | `TrooperProvider`, `ErrorBoundary`, `useTrooper`, router templates: `/react-router`, `/tanstack-router`, `/nextjs` |
| [`@trooper/vue`](packages/vue) | Vue 3 plugin, `errorHandler`, vue-router templates |
| [`@trooper/angular`](packages/angular) | `provideTrooper()`, `TrooperErrorHandler`, Router templates |
| [`@trooper/react-native`](packages/react-native) | ErrorUtils, Hermes rejections, XHR, AppState flush, react-navigation screens, app start, native bridge |

```ts
import { init, identify, track } from '@trooper/browser';
init({ dsn: 'https://<ingest-key>@ingest.example.com/<project>', app: 'shop-web', version: GIT_SHA, environment: 'production',
       tracePropagationTargets: ['https://api.shop.com'] });
identify('user-42', { plan: 'pro' });
track('checkout_started', { cart_value: 120 });
```

## Guarantees

- **Never crashes the host** (AGENTS.md rule 5): every public function and every instrumentation hook is wrapped; SDK errors are
  counted (`internalErrorCount()`) and logged only with `debug: true`. Network is async, batched, with backoff.
- **Privacy by default** (rule 6): no headers, bodies or input values are collected; query **values** are stripped from every URL
  (keys kept); URL user-info removed. `beforeSend` can modify or drop any event — if it throws, the event is dropped.
- **Delivery** (contracts §2/§9): `x-trooper-key` header; 429/5xx/network errors → exponential backoff 1 s → 60 s with jitter
  (honours `Retry-After`); other 4xx → batch dropped. Failed batches go to the offline queue (IndexedDB / AsyncStorage / memory,
  ≤ 5 MB, oldest dropped first) and are retried oldest-first. On page hide: `navigator.sendBeacon(<endpoint>?key=<key>)`.
- **Views**: names are route templates when a router integration is used, otherwise a normalized path (`/users/123` → `/users/:id`).
  `VIEW_START` carries `loading_time_ms` (it is emitted once load / route settle is measured), `VIEW_END` carries `time_spent_ms`.
- **Sampling** is per session (`sampleRate`), so sampled sessions are complete.

## Development

All packages are in the root pnpm workspace (`sdks/js/packages/*`).

```bash
pnpm install                                       # from the repo root
pnpm -r --filter "./sdks/js/packages/**" build     # tsup: ESM + CJS + .d.ts (+ IIFE for browser)
pnpm -r --filter "./sdks/js/packages/**" test      # vitest (happy-dom for DOM packages, mocked react-native)
pnpm --filter @trooper/browser size                # bundle-size report, fails above 15 KB gzip
```

Tests resolve sibling packages to their sources (`vitest.shared.mjs`), so they don't need a prior build; `typecheck` does.
`core/test/proto.test.ts` parses `rum.proto` and fails if the TypeScript types drift from it.

Example page: [`examples/browser/index.html`](examples/browser/index.html) (loads the IIFE build, DSN `http://tt_dev_ingest_key@localhost:4318/1`).

## Ingest requirements (for the browser SDK)

- CORS on `/v1/rum`: `OPTIONS` preflight allowing `content-type`, `x-trooper-key` (fetch path).
- `sendBeacon` posts `Content-Type: text/plain;charset=UTF-8` (the only way to avoid a preflight) with the key in `?key=`;
  the body is the same JSON.
- Requests to propagation targets carry `traceparent` and `baggage: tt.svc=<app>` (merged into an existing `baggage`); cross-origin APIs must allow both headers in CORS `Access-Control-Allow-Headers`.
