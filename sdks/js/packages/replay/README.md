# @trooper/replay

Session replay for the web — an integration for [`@trooper/browser`](../browser) that records the DOM with
[`@rrweb/record`](https://github.com/rrweb-io/rrweb) 2.2.0 and sends segments to `POST /v1/replay` (ADR-021, contracts §23).
Replays share the RUM `session_id`, so the player's timeline (views, actions, errors, requests → traces) comes from RUM.

Separate package so `@trooper/browser` stays small: ~3.5 KB gzip of our code + ~22.5 KB for rrweb (~26 KB total, `pnpm size`).

## Install

```bash
pnpm add @trooper/browser @trooper/replay
```

```ts
import { init } from '@trooper/browser';
import { replayIntegration } from '@trooper/replay';

init({
  dsn: 'https://<ingest-key>@ingest.example.com/<project>',
  app: 'shop-web', version: '1.4.2', environment: 'production',
  integrations: [
    replayIntegration({ sampleRate: 0.1, onErrorSampleRate: 1.0 }),
  ],
});
```

Script tag — load after the browser IIFE; `window.TrooperReplay` exposes the same exports:

```html
<script src="https://unpkg.com/@trooper/browser/dist/trooper.iife.js"></script>
<script src="https://unpkg.com/@trooper/replay/dist/trooper-replay.iife.js"></script>
<script>
  Trooper.init({ dsn: '…', app: 'shop-web', integrations: [TrooperReplay.replayIntegration()] });
</script>
```

## Options

| Option | Default | |
|---|---|---|
| `enabled` | `true` | `false` turns replay off entirely |
| `sampleRate` | `0.1` | fraction of sessions recorded from the start |
| `onErrorSampleRate` | `1.0` | fraction of the *other* sessions that keep the last 60 s in memory and, on the first ERROR, send it and keep recording |
| `maskAllText` | `true` | every text node replaced by `*` (length and whitespace kept) |
| `maskAllInputs` | `true` | every input / textarea / select value masked |
| `blockSelector` | `[data-trooper-block]` | element recorded as an empty box of the same size (content never leaves the page) |
| `maskSelector` | `[data-trooper-mask]` | masks text/inputs when `maskAllText` / `maskAllInputs` are off |
| `unmaskSelector` | `[data-trooper-unmask]` | opts a subtree out of masking; the nearest of mask/unmask wins |

The returned integration also has `getMode()` (`'off' | 'buffer' | 'session'`), `flush()` and `stop()`.

## Privacy

Default is "show the layout, hide the content":

- All text and all input values are masked in the browser before anything is sent; unmask only what you know is safe
  (`<span data-trooper-unmask>Order total</span>`).
- **Always masked**, even under `data-trooper-unmask`: `input[type=password]` and fields with `autocomplete`
  `current-password` / `new-password` / `cc-*`.
- **Never recorded**: `<canvas>` content (shows empty), cross-origin iframes, `<script>` and comments, fonts and images as data
  (images load from their URL at playback), network request/response bodies.
- `data-trooper-block` keeps an element's size but nothing inside it.
- Replay is only recorded for sessions sampled in by RUM (`init({ sampleRate })`); `user_id` / `anonymous_id` are the RUM ones.

## How it works

- **Sampling** — decided once per session (`sampleRate`, else `onErrorSampleRate` → buffer mode, else off) and stored
  in `localStorage` under `tt_replay:<session_id>` as `{ m: mode, n: next segment }` — shared by every tab of the session,
  like the RUM session, and kept across page loads (fallback: `sessionStorage`, then memory). An ERROR in any buffering tab
  switches the shared decision to `session`. Entries of older sessions are removed when a session starts or rotates.
- **Segment numbers** — taken with a read-increment-write of the shared counter right at the moment a segment is sealed, so
  tabs of one session interleave their numbers instead of overwriting each other. Every tab still starts its own recording
  with a FullSnapshot (its first segment has `has_full_snapshot: true`).
- **Session mode** — events go into a segment that is sent after 5 s or once ~256 KB (uncompressed) are collected. Each page
  load starts with a FullSnapshot, and a new FullSnapshot (checkout) is taken every 5 min while events flow; segments after a
  checkout start with it.
- **Buffer mode** — a checkout every 60 s; the previous and current windows (≥ 60 s, each starting with a FullSnapshot) stay in
  memory. On an ERROR RUM event they are sent (split ≤ 256 KB) and the session switches to session mode for good.
- **Session rotation** (30 min idle / 4 h / `clearUser()`) — the open segment is sent under the old id, a new decision is made
  and recording restarts with a FullSnapshot at segment 0.
- **Stats** per segment: `clicks` (rrweb mouse clicks), `inputs` (input interactions, consecutive events on one field count
  once), `errors` and `rage_clicks` (from `@trooper/browser` ERROR / rage ACTION events).
- **Delivery** — `POST <origin>/v1/replay?key=<key>` with `credentials: 'omit'`. Gzip via `CompressionStream` when available
  (`Content-Type: application/json`, `Content-Encoding: gzip` — needs the CORS preflight), else plain JSON as
  `text/plain;charset=UTF-8` (no preflight). 429 / 5xx / network errors → backoff 1 s → 60 s with jitter (honours
  `Retry-After`), other 4xx → dropped; the retry queue is capped (30 segments / 5 MB, oldest dropped). On
  `visibilitychange: hidden` and `pagehide` the open segment is sent synchronously with `fetch(keepalive)` as plain JSON,
  split into parts under the 64 KB keepalive budget.
- **Never throws** — rrweb runs with an `errorHandler`; every callback is wrapped; failures are counted as internal SDK errors
  (`debug: true` logs them). If rrweb cannot start, replay turns itself off for the page.

## Development

```bash
pnpm --filter @trooper/replay build       # ESM + CJS + .d.ts + dist/trooper-replay.iife.js
pnpm --filter @trooper/replay test        # vitest + happy-dom (rrweb mocked; one smoke test with the real recorder)
pnpm --filter @trooper/replay typecheck   # needs @trooper/browser built
pnpm --filter @trooper/replay size
```
