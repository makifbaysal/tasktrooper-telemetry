# @trooper/core

Platform-neutral core shared by `@trooper/browser` and `@trooper/react-native`. Apps normally don't depend on it directly.

- `types.ts` — hand-written mirror of `proto/rum/v1/rum.proto` (`RumBatch`, `RumEvent`, payloads). `test/proto.test.ts` parses the
  .proto and compares every message/enum, and checks a serialized payload uses only proto field names.
- `Client` — enriches events (session, view, user, anonymous id, connection), samples per session, scrubs URLs, runs `beforeSend`,
  batches (≤100 events or `flushIntervalMs` = 5 s), delivers through a `Transport` with retry/backoff and an `OfflineStore`.
  `flush({ beacon: true })` calls the transport synchronously (page hide).
- `SessionManager` — rotation after 30 min inactivity or 4 h; optional sync `SessionStore` (shared between tabs).
- `parseDsn('https://<key>@host[:port][/prefix]/<project>')` → `{ key, origin, projectId, rumEndpoint }` (regex, RN-safe).
- `FetchTransport`, `classifyStatus` (2xx ok, 429/5xx retry, other 4xx drop), `backoffDelay` (1 s → 60 s, jitter).
- `MemoryOfflineStore`, `KeyValueOfflineStore` (AsyncStorage-compatible) — ≤ 5 MB, FIFO drop.
- `instrumentFetch` / `instrumentXhr` — RESOURCE events, W3C `traceparent` for `tracePropagationTargets`, ingest URLs ignored.
- `stripQueryValues`, `normalizePath`, `createTraceContext`, `shouldPropagate`, `guard`/`safe` (error isolation).

Zero runtime dependencies.
