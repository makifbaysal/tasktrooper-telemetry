# TaskTrooper Telemetry — JavaScript SDKs (RUM + Node.js)

Browser and React Native SDKs that send RUM events as JSON to `POST /v1/rum` (ADR-011, `.ai/contracts.md` §2, §9).
Wire schema: [`proto/rum/v1/rum.proto`](../../proto/rum/v1/rum.proto) — snake_case field names, enums as strings.

| Package | What |
|---|---|
| [`@tasktrooper/core`](packages/core) | Platform-neutral engine: event model (mirrors rum.proto), DSN, session (30 min idle / 4 h max), batching (≤100 events / 5 s), retry/backoff, offline queue, sampling, `beforeSend`, URL scrubbing, fetch/XHR instrumentation |
| [`@tasktrooper/browser`](packages/browser) | Web: views (initial + SPA), Web Vitals, errors, fetch/XHR + `traceparent`, clicks / rage / dead clicks, `sendBeacon` on hide, IndexedDB offline queue |
| [`@tasktrooper/replay`](packages/replay) | Web session replay (`@rrweb/record` 2.2.0): `replayIntegration()` for `@tasktrooper/browser`, masking by default, per-session sampling + 60 s error buffer, segments to `POST /v1/replay` (ADR-021, contracts §23) |
| [`@tasktrooper/react`](packages/react) | `TrooperProvider`, `ErrorBoundary`, `useTrooper`, router templates: `/react-router`, `/tanstack-router`, `/nextjs` |
| [`@tasktrooper/vue`](packages/vue) | Vue 3 plugin, `errorHandler`, vue-router templates |
| [`@tasktrooper/angular`](packages/angular) | `provideTrooper()`, `TrooperErrorHandler`, Router templates |
| [`@tasktrooper/react-native`](packages/react-native) | ErrorUtils, Hermes rejections, XHR, AppState flush, react-navigation screens, app start, native bridge |
| [`@tasktrooper/node`](packages/node) | Backend: OpenTelemetry distribution for Node.js (OTLP/HTTP protobuf to `/v1/traces`, `/v1/metrics`, `/v1/logs`; `trooper.*` conventions, express/fastify/koa/Hono/NestJS routes, kafkajs/amqplib/BullMQ consumers, pino/winston) — not RUM |

```ts
import { init, identify, track } from '@tasktrooper/browser';
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
pnpm --filter @tasktrooper/browser size            # bundle-size report, fails above 15 KB gzip
pnpm --filter @tasktrooper/replay size             # replay add-on size (own code; rrweb reported separately)
```

Tests resolve sibling packages to their sources (`vitest.shared.mjs`), so they don't need a prior build; `typecheck` does.
`core/test/proto.test.ts` parses `rum.proto` and fails if the TypeScript types drift from it.

Example page: [`examples/browser/index.html`](examples/browser/index.html) (loads the IIFE build, replace the placeholder DSN `http://<ingest-key>@localhost:4318/<project-id>` with yours).

## Ingest requirements (for the browser SDK)

- CORS on `/v1/rum`: `OPTIONS` preflight allowing `content-type`, `x-trooper-key` (fetch path).
- `sendBeacon` posts `Content-Type: text/plain;charset=UTF-8` (the only way to avoid a preflight) with the key in `?key=`;
  the body is the same JSON.
- Requests to propagation targets carry `traceparent` and `baggage: tt.svc=<app>` (merged into an existing `baggage`); cross-origin APIs must allow both headers in CORS `Access-Control-Allow-Headers`.

## Releasing

All packages are published together to npm as `@tasktrooper/*`, always with the same version
(`workspace:*` deps are rewritten to that exact version by `pnpm publish`).

```bash
sdks/js/scripts/version.sh 0.2.0          # sets "version" in every packages/*/package.json
git commit -am "chore(sdk-js): release 0.2.0"
git tag js-v0.2.0 && git push && git push origin js-v0.2.0
```

The tag triggers [`.github/workflows/release-npm.yml`](../../.github/workflows/release-npm.yml): it fails unless the tag
version equals every package version, then builds, tests and runs
`pnpm -r --filter "./sdks/js/packages/*" publish --access public --no-git-checks` (with provenance; versions already on
npm are skipped, so a re-run is safe). Check a tarball locally with `pnpm pack` in a package dir or
`pnpm -r --filter "./sdks/js/packages/*" publish --dry-run --no-git-checks`.

Auth:

1. **First publish:** an npm package must exist before a trusted publisher can be attached, so the first release needs an
   npm automation/granular token (publish rights on the `tasktrooper` org) in the repo secret `NPM_TOKEN`.
2. **Afterwards:** on npmjs.com, for each package → Settings → Trusted publishing → GitHub Actions: repository
   `makifbaysal/tasktrooper-telemetry`, workflow `release-npm.yml`. Then delete the `NPM_TOKEN` secret; the workflow
   publishes via OIDC (`id-token: write`, npm ≥ 11.5.1).
