# @trooper/browser

RUM for the web. Framework-agnostic; see `@trooper/react`, `@trooper/vue`, `@trooper/angular` for route templates and
framework error hooks.

```ts
import { init, identify, clearUser, track, captureError, setView } from '@trooper/browser';

init({
  dsn: 'https://<ingest-key>@ingest.example.com/<project>',
  app: 'shop-web', version: '1.4.2', environment: 'production',
  sampleRate: 1,                                    // per session
  tracePropagationTargets: ['https://api.shop.com', /\/api\//], // default: same origin only
  beforeSend: (e) => e,                             // modify / return null to drop
});
identify('user-42', { plan: 'pro' });              // traits are opt-in; PII is your responsibility
track('checkout_started', { cart_value: 120 }, 120);
captureError(err, { attributes: { step: 'payment' } });
setView('/products/:id');                          // name the current view (route template)
```

Script tag: `<script src="https://unpkg.com/@trooper/browser/dist/trooper.iife.js"></script>` → `window.Trooper.init(...)`.

| Option | Default | |
|---|---|---|
| `trackViews` | true | initial view + SPA (History API patch, popstate, hashchange); name = normalized path unless set by a router integration / `setView` |
| `trackVitals` | true | LCP, INP, CLS, FCP, TTFB via `web-vitals` (value + rating), attached to the initial view |
| `trackErrors` | true | `window.onerror` + `unhandledrejection` (`is_crash: true`), deduplicated |
| `captureConsoleErrors` | false | `console.error` → ERROR (`source: console`) |
| `trackNetwork` | true | fetch + XHR → RESOURCE (method, status, duration, sizes, `trace_id`/`span_id`) |
| `trackActions` | true | clicks → ACTION; name = `data-trooper-name` / `aria-label` / text (50 chars); rage (≥3 in 1 s), dead (no DOM mutation / navigation / request in 1 s; inputs excluded) |
| `persist` | true | anonymous id + session in localStorage, offline queue in IndexedDB (memory fallback) |
| `maxBatchSize` / `flushIntervalMs` | 100 / 5000 | |
| `integrations` | `[]` | add-ons set up on start, e.g. `replayIntegration()` from [`@trooper/replay`](../replay) |

**Views.** `VIEW_START` is emitted when the loading time is known — initial page: navigation timing `loadEventEnd`; SPA route:
time until the DOM is quiet for 100 ms with no request in flight (max 10 s) — or earlier if the view ends / the page is hidden.
Its `ts_ms` is still the navigation time. `VIEW_END` carries `time_spent_ms`.

**Delivery.** Batches via `fetch` with `x-trooper-key`; on `visibilitychange: hidden` / `pagehide` via
`navigator.sendBeacon(<endpoint>?key=<key>)` (text/plain body, split under 60 KB), falling back to `fetch(keepalive)`.
SSR-safe: `init` is a no-op without `window`.

**Integrations.** `integrations: [{ name, setup(client) }]` — `setup` runs once when the SDK starts (not when `enabled: false`) and may
return a teardown called by `close()`. The `IntegrationClient` gives `config` (`dsn`, `endpoint` origin, `key`, `projectId`,
`service`, `environment`, `version`), `sessionId()`, `sampled()`, `anonymousId()`, `userId()`, `view()`,
`onSessionStart(fn)` (session rotation), `onEvent(fn)` (every accepted RUM event, e.g. ERROR / rage ACTION), the unpatched
`fetch`, `backoffDelay`, `classifyStatus` and `reportInternal`. Throwing integrations and listeners are swallowed.

```ts
import { init } from '@trooper/browser';
import { replayIntegration } from '@trooper/replay';
init({ dsn, app: 'shop-web', integrations: [replayIntegration({ sampleRate: 0.1 })] });
```

Size (core + browser + web-vitals, min + gzip): ~13.4 KB for `init` only, ~13.9 KB for the IIFE. `pnpm size` checks the 15 KB budget.
