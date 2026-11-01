# Coding standards (repo-wide)

Cross-cutting rules only. Language and toolchain rules live next to the code, in each app's `.ai/` folder (table below).
The working rules in [`AGENTS.md`](../AGENTS.md) are the source; this file links them to where they are enforced.

## Per-app standards

| Area | Language / toolchain | Coding standards | Testing |
|---|---|---|---|
| `services/` (all Go services + `services/pkg`; also `tools/trooper-cli`) | Go 1.26, go.work | [services/.ai/coding-standards.md](../services/.ai/coding-standards.md) | [services/.ai/testing.md](../services/.ai/testing.md) |
| `apps/web` | React 19, Vite, strict TS | [apps/web/.ai/coding-standards.md](../apps/web/.ai/coding-standards.md) | [apps/web/.ai/testing.md](../apps/web/.ai/testing.md) |
| `sdks/js` | TS, pnpm workspace, vitest | [sdks/js/.ai/coding-standards.md](../sdks/js/.ai/coding-standards.md) | [sdks/js/.ai/testing.md](../sdks/js/.ai/testing.md) |
| `sdks/go` | Go | [sdks/go/.ai/coding-standards.md](../sdks/go/.ai/coding-standards.md) | [sdks/go/.ai/testing.md](../sdks/go/.ai/testing.md) |
| `sdks/python` | Python | [sdks/python/.ai/coding-standards.md](../sdks/python/.ai/coding-standards.md) | [sdks/python/.ai/testing.md](../sdks/python/.ai/testing.md) |
| `sdks/jvm` | Java/Kotlin, Gradle | [sdks/jvm/.ai/coding-standards.md](../sdks/jvm/.ai/coding-standards.md) | [sdks/jvm/.ai/testing.md](../sdks/jvm/.ai/testing.md) |
| `sdks/dotnet` | C#, .NET SDK | [sdks/dotnet/.ai/coding-standards.md](../sdks/dotnet/.ai/coding-standards.md) | [sdks/dotnet/.ai/testing.md](../sdks/dotnet/.ai/testing.md) |
| `sdks/android` | Kotlin, Gradle | [sdks/android/.ai/coding-standards.md](../sdks/android/.ai/coding-standards.md) | [sdks/android/.ai/testing.md](../sdks/android/.ai/testing.md) |
| `sdks/ios` | Swift, SwiftPM/CocoaPods | [sdks/ios/.ai/coding-standards.md](../sdks/ios/.ai/coding-standards.md) | [sdks/ios/.ai/testing.md](../sdks/ios/.ai/testing.md) |
| `sdks/flutter` | Dart, Flutter | [sdks/flutter/.ai/coding-standards.md](../sdks/flutter/.ai/coding-standards.md) | [sdks/flutter/.ai/testing.md](../sdks/flutter/.ai/testing.md) |

Repo-wide testing rules: [testing.md](testing.md).

## Docs first

- New service, table, SDK API, alert type, `TT_*`/`TROOPER_*` variable or Kafka topic → update the owning doc in the
  same PR, before the code: [architecture.md](architecture.md), [data-model.md](data-model.md), [sdk.md](sdk.md),
  [alerting.md](alerting.md), [contracts.md](contracts.md). Architectural choices get an ADR in [decisions.md](decisions.md).
- If code and a doc disagree, fix one of them in the same PR; never leave them diverged.
- Everything is English: `.ai/` docs (root and per-app), code, identifiers, comments, commits and PRs.

## Multi-tenancy

- Every telemetry row carries `org_id` + `project_id`; every read filters by them. No query without a tenant filter.
- Go `api`: enforced by `services/api/internal/tq` (`{{tenant}}` marker). Elsewhere: explicit `project_id` parameters.
  Details: [services/.ai/coding-standards.md](../services/.ai/coding-standards.md#data-access-and-multi-tenancy).

## Cardinality

- No user id, request id, trace id or raw URL in metric labels or rollup keys. Endpoints are route templates
  (`/users/:id`); normalisation lives in `services/ingest/internal/norm` and in each SDK. Rules: [data-model.md](data-model.md).

## SDKs never crash the host

- Every SDK entry point swallows its own errors and counts them internally (AGENTS rule 5 asks for an internal metric;
  how far each SDK exports it differs, see the per-SDK docs); network I/O is async,
  batched, with backoff and bounded buffers; a missing/invalid DSN turns the SDK into a no-op. See [sdk.md](sdk.md).

## PII

- SDKs do not collect headers, bodies or query-param values by default; masking happens in the SDK before data leaves
  the process (session replay masks inputs/text by default). Servers never log secrets, keys or raw request bodies.

## Secrets and configuration

- Services read config only from env (`TT_*`, via `services/pkg/config`); SDKs from `TROOPER_*` / DSN. No secrets in
  code, fixtures, docs examples or commits. Dev defaults match `deploy/docker-compose.yml` (`trooper:trooper`).
- Production guards fail closed (`TT_ENV=production`, e.g. `services/pkg/secrets`).
- LLM model IDs come from config, never hard-coded.

## UI and charts

- Design tokens only (`apps/web/src/styles/tokens.css`), single y-axis, fixed `--series-1..8` order, `--status-*` only
  for state with icon + label. See [design-system.md](design-system.md).

## Commits, PRs and gates

- Conventional Commits with the area as scope: `feat(web): ...`, `fix(ingest): ...`, `docs(ai): ...`, `chore(sdk-js): ...`.
- Before committing or opening a PR run `make check` at the repo root; it must end with `check OK`. It is the same set
  of gates as `.github/workflows/ci.yml`: gofmt + `go vet` + unit tests for every `services/go.work` module, web
  `tsc -b` + `vite build`, JS SDK build + vitest, actionlint. Never skip or remove a gate to get green.
- SDKs other than `sdks/js` (and `tools/trooper-cli`) are **not** in `make check`/CI; run their own build/test commands
  (see their `.ai/testing.md`) when you touch them. Their release workflows (`.github/workflows/release-*.yml`) are
  the last line of defence, not a substitute.
- Workflows: pin third-party actions by full commit SHA with a version comment (as in `ci.yml`), minimal `permissions:`.
