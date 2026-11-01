# Go SDK — coding standards

Scope: `sdks/go` (module `github.com/makifbaysal/tasktrooper-telemetry/sdks/go`, package `trooper`) plus
`integrations/*`, `llm/`, `troopertest/`, `internal/`. Cross-cutting rules: [`../../../.ai/coding-standards.md`](../../../.ai/coding-standards.md).
SDK design and wire format: [`../../../.ai/sdk.md`](../../../.ai/sdk.md). Env vars, headers, `trooper.*` attributes,
self-metrics: [`../../../.ai/contracts.md`](../../../.ai/contracts.md) (§2, §4, §24, §25). Do not restate those here; link them.

## What this SDK is

- An **OpenTelemetry distribution**, not a custom tracer (ADR-011). It configures the global OTel
  Tracer/Meter/LoggerProvider + propagator and adds `trooper.*` conventions. New features build on OTel APIs.
- Export is OTLP/HTTP protobuf via the official `otlp*http` exporters (batching, retry/backoff, bounded queues come
  from them). Don't write a custom exporter or a blocking send path.

## Host safety (repo rule 5) — non-negotiable

- **Never panic into the host.** Every exported entry point that can fail internally has a deferred recover:
  ```go
  defer func() {
      if recover() != nil { self.countError("capture") }
  }()
  ```
  See `errors.go` (`CaptureError`, `RecordPanic`), `entry.go` (`safeResolve`), `job.go`, `profiling.go`.
- **Every swallowed error is counted** in `trooper.sdk.errors{component}`; lost items in `trooper.sdk.dropped{component}`.
  Root package: `self.countError(component)` / `self.countDropped(component, n)` (`selfmetrics.go`).
  Sub-packages (e.g. `llm`) must not import the root; use `internal/sdkerr.Count(component)`.
- New component name → add it to the list in the `selfMetrics` comment and to contracts §4.
  Existing: `exporter.{traces,metrics,logs,profiles}`, `processor`, `capture`, `resolver`, `profiler`, `profiler.cpu`, `llm`, `job`.
- `Start` never panics and always returns a **non-nil** `ShutdownFunc` (no-op on error). It may return an `error`
  (bad DSN); the app keeps running. Keep that contract (`start.go`, `TestStartNeverFailsHard`).
- OTel internal errors go through `errorHandler` (`selfmetrics.go`): stderr at most once per minute, every error with
  `Debug`. Don't add other `fmt.Fprint(os.Stderr, …)` / `log.Print` calls outside debug paths.
- Hot paths (span processor, `BeginEntry`, propagator) must not allocate or lock more than needed; never block on network.
- Queues are bounded (`MaxQueueSize`, default 2048); overflow drops, never blocks.

## Privacy and cardinality (repo rules 4, 6)

- Default records **no headers, query strings or bodies**. Integration package docs state this explicitly
  (see `integrations/gin/gin.go`).
- Routes are templates: always pass through `trooper.NormalizeRoute` (`route.go`) / `SetServerRoute`.
  Do: `trooper.route=/users/:id`. Don't: raw path, user id, request id as span name or metric label.
- SQL text goes through `trooper.SanitizeSQL`; query args are never recorded (`integrations/sql`, `integrations/pgx`).
- Client URLs have the query stripped (`TestClientURLQueryStripped`). LLM content capture is opt-in
  (`TROOPER_LLM_CAPTURE_CONTENT`, `llm.SetCaptureContent`).

## Public API shape

- Entry point: `trooper.Start(ctx, trooper.Config{…}, opts ...Option) (ShutdownFunc, error)`.
- `Config` is a plain struct; zero value = "unset". Resolution lives in `Config.withDefaults()` (`config.go`):
  explicit field > `OTEL_*` env > `TROOPER_*` env > default. Every new field documents its env var and default in its
  doc comment, and contracts §2 is updated in the same PR.
- Feature toggles that need sub-options use functional options: `WithProfiling(WithProfileTypes(…))` (`profiling.go`).
  Options are nil-safe (`Start` skips nil).
- DSN format `https://<key>@host[:port][/prefix]/<project>` is parsed only by `ParseDSN` (`dsn.go`); errors are prefixed
  `trooper: …`. Don't parse DSNs anywhere else.
- Exported identifiers for conventions are constants in `attrs.go` (`AttrRoute`, `BaggageServiceKey`, …). Use them;
  don't inline `"trooper.route"` strings in integrations.
- Exported methods that may be called on a zero/nil receiver say so ("Safe to call on nil", `Entry.Finish`).

## Integrations (`integrations/<name>`)

- One **separate Go module** per integration, with its own `go.mod`, `require …/sdks/go vX.Y.Z` **plus**
  `replace …/sdks/go => ../..` (consumers ignore `replace`). Add the directory to `go.work`.
- Package name `trooper<name>` (`troopergin`, `trooperhttp`, `troopersql`); import the core as `trooper`.
- Wrap the official OTel contrib instrumentation when one exists (`otelgin`, `otelhttp`, `otelgrpc`, `otelsql`,
  `redisotel`) and add trooper semantics on top: `BeginEntry` → `SetServerRoute` → `Finish`. Write your own only
  when no contrib exists (e.g. `mongo`, documented in README).
- Options: unexported `config` struct + `type Option func(*config)` + `WithXxx` constructors (`gin.go`).
  Expose a pass-through for the wrapped contrib options (`WithOtelGinOptions`).
- Panics: record with `trooper.RecordPanic`, then honour `trooper.PanicPolicy` (default `PanicRepanic`; always
  re-panic `http.ErrAbortHandler`).
- Package doc comment starts `// Package trooper<name> instruments …` and shows a 3–5 line usage snippet; put it in
  `doc.go` (preferred) or the main file.
- New integration → also add a row to `README.md` and an example if non-trivial (`examples/`).

## Style

- `gofmt` clean; `go vet` clean (`scripts/test-all.sh` runs both build and vet).
- Doc comment on every exported identifier, full sentences, starting with the name. Comments cite contract sections
  (`// contract: retry on 429/5xx, drop on other 4xx`, `.ai/contracts.md §4`).
- Errors returned to callers are prefixed `trooper: ` (`fmt.Errorf("trooper: invalid DSN: %w", err)`). No `log.Fatal` / `os.Exit`, and no SDK-originated `panic` (re-panicking the host's own panic under `PanicRepanic` is the only exception).
- Concurrency: `sync/atomic` types for counters, `sync.Mutex` for small shared state; document "safe for concurrent use".
- Test-only hooks go in `export_test.go` (package-internal), never as exported production API.

## Dependencies

- Core may depend only on `go.opentelemetry.io/otel/*`, `go.opentelemetry.io/contrib/instrumentation/runtime`,
  `go.opentelemetry.io/proto/otlp`, `google.golang.org/protobuf`. Framework/driver deps belong in an integration module.
- Keep OTel versions aligned across all `go.mod` files (same `otel` / `contrib` release everywhere).
- Go version: `go 1.26.3` in every `go.mod` and `go.work`; bump them together.

## Versioning and release

- `const Version` in `attrs.go` is reported as `telemetry.distro.version`; bump it with the release.
- Release = path-prefixed tags via `scripts/tag.sh X.Y.Z` (`sdks/go/vX.Y.Z`, `sdks/go/integrations/<name>/vX.Y.Z`);
  first bump every integration's `require` of the core. Procedure: `README.md` § Releasing.
- Commits: Conventional Commits, scope `sdk-go` (matches `sdk-python`, `sdk-jvm`, … in history), e.g. `feat(sdk-go): add echo panic policy`.
