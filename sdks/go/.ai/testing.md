# Go SDK — testing

Cross-cutting test rules: [`../../../.ai/testing.md`](../../../.ai/testing.md). Contract the tests assert:
[`../../../.ai/contracts.md`](../../../.ai/contracts.md) (§2 config/auth, §4 attributes + self-metrics).

## Gates — what actually runs

| Where | Runs Go SDK tests? |
|---|---|
| `.github/workflows/ci.yml` (PRs / main) | **No.** Covers `services/` (go.work), `apps/web`, `sdks/js` only. |
| `make check` (repo root) | **No.** gofmt/vet/test only for `services/` modules. |
| `.github/workflows/release-go.yml` (on `sdks/go/v*` / `sdks/go/integrations/*/v*` tag push) | `go vet ./...` + `go test -count=1 ./...` for **the tagged module only**, `GOWORK=off`. No gofmt, no `-race`. |

So a broken SDK reaches `main` unless you run the checks yourself. `make check` green does **not** mean the Go SDK is green.

## Required locally before every SDK PR

```bash
cd sdks/go
gofmt -l .                      # must print nothing
scripts/test-all.sh -race       # build + vet + test: core, every integrations/*, examples (GOWORK=off)
```

- `GOWORK=off` is deliberate: each module must build on its own, exactly as `release-go.yml` and consumers see it.
  If a module only passes with the workspace, its `go.mod` is wrong (missing require / replace, run `-tidy`).
- `scripts/test-all.sh -tidy` runs `go mod tidy` in every module first; commit the resulting `go.mod`/`go.sum` changes.
- Still run root `make check` (repo rule 11) if you touched anything outside `sdks/go`.

## Test layout

| File / dir | What | Package |
|---|---|---|
| `core_test.go` | DSN, route normalisation, SQL sanitising, entry/caller propagation, `CaptureError`, messaging | `trooper_test` |
| `contract_test.go` | Cross-SDK wire contract: `tt=r:<rate>` tracestate, consumer attempt header, `TROOPER_ENABLED=false` | `trooper_test` |
| `start_test.go` | `Start` against an `httptest` **fake ingest** (`fakeIngest`): decodes OTLP protobuf (gzip), asserts `x-trooper-key`, paths, retry on 429/5xx, drop on 4xx, env precedence | `trooper_test` |
| `profiling_test.go`, `heartbeat_test.go`, `job_test.go`, `profilelabels_test.go` | feature tests, same fake-server pattern | `trooper_test` |
| `e2e_test.go` | real ingest-gw; build tag `e2e` | `trooper_test` |
| `export_test.go` | test-only hooks (`SetFirstExportDelay`) | `trooper` (internal) |
| `llm/testdata/*.json, *.sse` | recorded provider responses / streams for parser tests | — |
| `integrations/<name>/*_test.go` | each integration against an in-process server/router via `troopertest` | either |

## Rules

- **Black-box by default:** tests live in `package trooper_test` and use only the public API. Need an internal knob?
  Add a small exported func to `export_test.go` (compiled only in tests) that returns a `restore func()`:
  ```go
  func SetFirstExportDelay(d time.Duration) (restore func()) { … }
  // test: defer trooper.SetFirstExportDelay(10 * time.Millisecond)()
  ```
  Same pattern in `llm/export_test.go` (`ResetCaptureContent`).
- **Table-driven** (repo rule 8): `tests := []struct{ name string; … }{…}` + `t.Run(tt.name, …)`. See `TestParseDSN`,
  `TestExportRetryAndDrop`, `integrations/gin/gin_test.go`.
- **No real network** in unit tests. Use `httptest.NewServer` as the ingest endpoint; never depend on
  `localhost:4318`. Script server responses per request (`fakeIngest.statuses`) to test retry/drop.
- **Assert the host-safety contract** for every new public entry point: no panic on nil/garbage input, non-nil return,
  error counted. Patterns: `TestStartNeverFailsHard`, `TestEntryResolverPanicIsSwallowed`, compare
  `trooper.SDKErrors()` / `trooper.DroppedBatches()` before/after.
- **Assert privacy/cardinality** whenever you add capture: e.g. route is the template (`/users/:id`, not `/users/42`),
  query stripped (`TestClientURLQueryStripped`), SQL literals replaced, no headers/bodies on spans.
- **Globals:** `Start` and `troopertest.New` replace OTel globals. Use `t.Setenv` for env (it forbids
  `t.Parallel`, which is fine; the suite is not parallel). Always call the returned `shutdown`.
- Use `t.Context()` / `tb.Context()` (Go ≥1.24) rather than `context.Background()` in new tests where lifetime matters.
- Tests that change a contract behaviour must cite the contract section in a comment
  (`// explicit OTEL_* wins (contract §2)`).

## `troopertest` (public helper package)

`troopertest` is **shipped API** for users testing their own instrumentation, and the harness for our integration tests:

```go
h := troopertest.New(t, "svc")            // always-sample provider + trooper processor + propagator; globals restored on cleanup
req.Header = …troopertest.CallerHeaders("upstream")  // traceparent + baggage tt.svc=upstream
s := h.SpanByKind(trace.SpanKindServer)
troopertest.AttrString(s, trooper.AttrRoute) // "/users/:id"
troopertest.HasException(s)
```

- Changes to it are public API changes: keep signatures backward compatible, document every exported func.
- Prefer adding a helper here over copying span-lookup loops into each integration test.
- Must not import any integration module (it would create cycles for integration tests).

## E2E (optional, needs infra)

```bash
make infra && make migrate && make run-gw     # repo root; ingest-gw on :4318
cd sdks/go && TROOPER_E2E_DSN=http://<ingest-key>@localhost:4318/<project-id> go test -tags e2e -run E2E .
```

- Key/project come from dashboard onboarding or `api bootstrap` (no seed key, ADR-028).
- Skips (does not fail) when `TROOPER_E2E_DSN` is unset or `/healthz` is unreachable. Asserts `DroppedBatches()`
  did not grow, i.e. the gateway accepted every batch. Run it when you change export, headers, resource or DSN logic.

## Benchmarks

- No benchmarks exist yet. Hot-path changes (span processor `OnStart`, `BeginEntry`, propagator, `NormalizeRoute`,
  `SanitizeSQL`) should add a `BenchmarkXxx` with `b.ReportAllocs()` and quote before/after in the PR.
