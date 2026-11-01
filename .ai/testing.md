# Testing (repo-wide)

Cross-cutting rules only. How to write and run tests for a given language lives in each app's `.ai/testing.md`
(table in [coding-standards.md](coding-standards.md#per-app-standards)).

## Required gate

`make check` at the repo root must end with `check OK` before a commit/PR (AGENTS rule 11). It runs exactly what CI runs:

| Gate | Scope | CI job |
|---|---|---|
| `gofmt -l` (testdata excluded), `go vet ./...`, `go test ./...` | every module in `services/go.work` | `go / <module>` |
| `tsc -b`, `vite build` | `apps/web` | `web` |
| `pnpm -r build`, `vitest run` | `sdks/js/packages/**` | `sdk-js` |
| `actionlint` | `.github/workflows/*.yml` | `actionlint` |

If the core flow is broken, find and fix the cause; do not skip, filter or delete tests to get green.

## Not covered by the gate

Run these yourself when you touch the area; the commands are in the per-app testing docs.

- Go integration (`-tags integration`) and end-to-end (`-tags e2e`, `make e2e-ingest`) tests: need `make infra && make migrate`.
- Ingest hot-path benchmarks: `make bench-ingest` (required for ingest changes, compare before/after in the PR).
- `sdks/go`, `sdks/python`, `sdks/jvm`, `sdks/dotnet`, `sdks/android`, `sdks/ios`, `sdks/flutter` and `tools/trooper-cli`
  unit tests (only their release workflows run anything).
- `apps/web` has no automated tests yet; verify UI changes in the browser against a real api.

## What every change needs

- Bug fix → a test that fails without the fix (a new row in the existing table where possible).
- New pure logic → unit tests; Go uses table-driven tests (AGENTS rule 8).
- New `api` route → entry in `services/api/openapi.yaml` (enforced by `TestOpenAPICoversRoutes`) + handler test with a fake store.
- New SQL/migration → idempotent migration + an integration test run against compose.
- SDK change → tests for: errors never escape to the host, PII off by default, route normalisation/cardinality, and
  the wire format against [contracts.md](contracts.md) / [sdk.md](sdk.md).
- Logic that exists in two languages (e.g. RCA in `apps/web/src/lib/rca.ts` and `services/alerting/internal/rca`) →
  shared golden fixture (`services/alerting/internal/rca/testdata/golden.json`).

## Test data and secrets

- Tests run without real credentials, network or cloud accounts. External APIs (LLM, Slack, GitHub, AWS/GCP, Vercel)
  are replaced by fakes / local HTTP servers; the LLM path uses a mock provider.
- Only obviously fake values in tests and fixtures (`test-key`, `good`, generated signing secrets). Never paste a real
  token, DSN or ingest key, even a revoked one. Local infra uses the compose dev credentials (`trooper:trooper`).
- No seed data (ADR-028): integration/e2e suites create their own org/project/key with a reserved fixture id and clean
  up afterwards.
- Fixtures live in `testdata/` (Go) or next to the test; keep them small and synthetic, with no customer data or PII.
