# Testing: Go services (`services/`)

Applies to every module in `services/go.work` and to `tools/trooper-cli`. Repo-wide gates and secret rules:
[`../../.ai/testing.md`](../../.ai/testing.md). Coding rules: [`coding-standards.md`](coding-standards.md).

## Test tiers

| Tier | How it is marked | Needs infra | Runs in CI / `make check` | Command |
|---|---|---|---|---|
| Unit | plain `_test.go` | no | **yes** | `make test` or `cd services/<m> && go test ./...` |
| Benchmarks | `func BenchmarkX` | no | no (run manually) | `make bench-ingest` |
| Integration | `//go:build integration` | Postgres / ClickHouse (`make infra && make migrate`) | no | `GOWORK=off go test -tags integration -count=1 ./...` |
| End-to-end | `//go:build e2e` (`ingest/e2e`, `integrations/e2e`, `loadtest/e2e`) | full compose stack | no | `make e2e-ingest`, `GOWORK=off go test -tags e2e -count=1 -v ./e2e/` |

- The default `go test ./...` must pass on a machine with **no** Docker, network or credentials. Anything that
  touches a real database, broker, S3 or external API goes behind a build tag.
- Integration/e2e tests skip, not fail, when the dependency is down: `t.Skipf("postgres unavailable: %v", err)`
  (`api/internal/server/integration_test.go`).
- Run integration/e2e tests before merging changes to SQL, migrations, the ingest pipeline or the alert engine; the
  per-service command is in each `services/<m>/README.md`.
- File naming: `<feature>_integration_test.go` next to the code, or a dedicated `integration/` / `e2e/` package for
  cross-binary flows (`alerting/integration`, `ingest/e2e`).

## Unit tests

- **Table-driven** by default (AGENTS rule 8). Slice of anonymous structs, loop with `t.Run(tc.name, ...)` when cases
  have names, plain loop for tiny `{in, want}` tables. References: `ingest/internal/norm/norm_test.go`,
  `api/internal/tq/tq_test.go`.

```go
for _, tc := range []struct {
	name    string
	sql     string
	want    string
	wantErr error
}{ /* ... */ } {
	t.Run(tc.name, func(t *testing.T) {
		got, err := Bind(ctx, ClickHouse, tc.sql)
		if !errors.Is(err, tc.wantErr) || got != tc.want {
			t.Errorf("Bind(%q) = %q, %v; want %q, %v", tc.sql, got, err, tc.want, tc.wantErr)
		}
	})
}
```

- Standard library `testing` only: no testify, gomock or other assertion/mock libraries. Failure messages follow
  `Func(input) = got, want want`.
- `t.Fatal` for setup failures, `t.Errorf` for assertions so a table reports every bad case.
- Helpers call `t.Helper()`; cleanup via `t.Cleanup`; env via `t.Setenv` (never `os.Setenv`); files via `t.TempDir()`.
- Test names: `TestFunc`, `TestType_Method` or `TestBehaviour` (`TestOpenAPICoversRoutes`); integration tests start with
  `TestIntegration...` so `-run Integration` selects them.
- No `t.Parallel()` is used in the codebase; tests that use `t.Setenv` cannot be parallel anyway. Do not add it without reason.
- Inject time: structs take `Now func() time.Time` and tests pin it (`testNow` in `api/internal/server/server_test.go`).

## Fakes, not mocks

- Handlers depend on consumer-side interfaces; tests pass a hand-written fake. The `api` pattern embeds the interface so
  unimplemented methods panic loudly:

```go
// fakeStore implements only what the tests call; anything else panics through the nil embedded interface.
type fakeStore struct {
	Store
	gotTen tenant.Tenant
}
```

- HTTP: drive handlers through `httptest.NewRecorder` / the real router (`newTestServer(t, &fakeStore{})`), and fake
  upstream APIs (Slack, GitHub, cloud providers, `services/api` for MCP) with `httptest.NewServer`.
- Swap package-level clients/endpoints instead of hitting the network (`cloudcheck.HTTPClient`,
  `t.Setenv("TT_TEST_SLACK", srv.URL+"/slack")` in `alerting/internal/notify/notify_test.go`).
- AI: use `ai.NewMock()`; tests never call a real LLM provider.
- Self-telemetry: `ingest/internal/selfobs/selfobstest` installs in-memory span/log recorders.
- In-process e2e without infra is fine as a unit test: `mcp/internal/server/e2e_test.go` runs the real MCP server
  against a real MCP client over in-memory transports and a fake `services/api`.

## What to test per layer

| Layer | Test |
|---|---|
| Pure logic (normalisation, flatten, burn rate, RCA, state machines) | table-driven unit tests with edge cases |
| `api` store SQL | tenant marker present (`tq` rejects otherwise); behaviour via `-tags integration` against compose |
| `api` handlers | status codes + JSON shape + tenant/query passed to the fake store; error mapping (404/400/409/500) |
| `api` routes | `TestOpenAPICoversRoutes` (route ↔ `openapi.yaml` parity) must stay green |
| Ingest hot path | unit test **and** benchmark (below) |
| Kafka/ClickHouse writers, alert engine rounds | integration/e2e |
| Config / env | `t.Setenv` cases, including production fail-closed guards |
| Cross-language logic | golden files shared with the web (`alerting/internal/rca/testdata/golden.json` is generated from `apps/web/src/lib/rca.ts`) |

Every bug fix adds a failing case to the relevant table first.

## Benchmarks (ingest hot path)

- Required for code on the ingest path (`services/ingest/internal/{norm,flatten,writer,gw,profiling,vercel}`).
- Style: `b.ReportAllocs()` + `for b.Loop()` (Go 1.24+), realistic input built once before the loop:

```go
func BenchmarkRoute(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		Route("/v1/orders/123/items/550e8400-e29b-41d4-a716-446655440000")
	}
}
```

- Run `make bench-ingest` (`go test -run '^$' -bench . -benchmem ./internal/...`) before and after a hot-path change
  and put the ns/op + allocs/op delta in the PR description. Benchmarks are not run in CI; there is no stored baseline.

## Fixtures and test data

- Static fixtures live in `testdata/` next to the test (`codeintel/internal/extract/testdata/{gorepo,jsrepo,pyrepo}`,
  `ingest/internal/vercel/testdata`). `testdata/` is excluded from the gofmt gate, so fixture Go code may be unformatted.
- No golden-update flag exists; regenerate golden files with the documented command in the test comment.
- Integration/e2e tests create their **own** org/project/ingest key (no seed data, ADR-028) with a reserved fixture id per
  suite and delete it at the end. Pick an unused id (9101, 9201, 9301, 9401, 9501, 9601, 9701, 9801 are taken; grep first).
  `TT_ITEST_KEEP=1` keeps the data for debugging where supported.
- Prefix synthetic names so they are easy to spot/clean (`ingest2-e2e-` service prefix in `ingest/e2e`).

## Secrets in tests

- Only obviously fake values: `auth.Static{"good": {...}}`, `"test-key"`, signing secrets generated in the test.
  Never commit or read real tokens; tests must not depend on `TT_SECRETS_KEY` (the dev key is used when unset).
- Connection strings default to the compose dev credentials (`trooper:trooper@localhost`), overridable via `TT_*` env.

## Running locally

```bash
cd services/api && go test ./internal/server/ -run TestOpenAPICoversRoutes -v   # one test
go test -race ./...                                                             # recommended for concurrent code; not a CI gate
make test                                                                       # all modules, unit only
make infra && make migrate && (cd services/alerting && GOWORK=off go test -tags integration -count=1 -v ./integration)
cd tools/trooper-cli && go test ./...                                           # not covered by make test / CI
```

Use `-count=1` for integration/e2e runs so cached results never hide infra state.
