## Unreleased

- Session replay (wireframe, ADR-021 / contracts.md §23): `TaskTrooper.init(..., replay: TrooperReplayOptions(...))`.
  Element-tree wireframes (≤ 2 fps, only on change, node/time budget), touches, keyboard, text masked by default
  (`TrooperUnmask`), inputs always masked, per-session sampling + on-error 60 s buffer, 5 s / 256 KB gzipped segments to
  `POST /v1/replay` with an offline store (≤ 2 MB). `TrooperReplay.isRecording` / `stats` / `flush()`.
- Hybrid bridge: `setContext` carries `sampled`; new `isNativeReplayActive` channel method (Android) — the Dart
  recorder is not started when the native replay is installed.
- `TrooperClient.errorListeners`; `Dsn.replayEndpoint`; `RumTransport.sendBytes`.

## 0.1.0

- Initial release.
