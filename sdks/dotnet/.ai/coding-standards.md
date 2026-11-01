# .NET SDK — coding standards

Scope: `sdks/dotnet` (`TaskTrooper.sln`). Cross-cutting rules live in the repo root:
[`.ai/coding-standards.md`](../../../.ai/coding-standards.md), SDK design in [`.ai/sdk.md`](../../../.ai/sdk.md),
wire/attribute/env contracts in [`.ai/contracts.md`](../../../.ai/contracts.md). This file only adds what is .NET-specific.

## Build settings (from [`Directory.Build.props`](../Directory.Build.props))

- Libraries multi-target **`net8.0;net9.0;net10.0`** via `$(LibraryTargetFrameworks)`. New `src/` projects use
  `<TargetFrameworks>$(LibraryTargetFrameworks)</TargetFrameworks>`, never a hard-coded list. Tests/samples: `net10.0`.
  Don't use APIs missing from net8.0 without `#if`.
- `LangVersion=latest`, **`Nullable=enable`**, `ImplicitUsings=enable` everywhere. Annotate every public API
  (`string?`, `Action<TaskTrooperOptions>? configure = null`); no `!` suppressions to silence real nulls.
- `TreatWarningsAsErrors=false`; `NoWarn` only `NU5104` (beta OTel packages in opt-in projects) and `CS1591`.
  Don't add new warnings or new `NoWarn` entries. No analyzer packages or `.editorconfig` are configured.
- `GenerateDocumentationFile=true`: public members get `/// <summary>`; mention env var and default like
  [`TaskTrooperOptions.cs`](../src/TaskTrooper.Telemetry/TaskTrooperOptions.cs) does.
- Packaging metadata, Source Link, symbols, deterministic build are central. A `.csproj` sets only `Description`,
  extra `PackageTags`, references and `InternalsVisibleTo`.

## Package versions (central package management)

- **All versions in [`Directory.Packages.props`](../Directory.Packages.props)**; `.csproj` uses
  `<PackageReference Include="X" />` without `Version`.
  - Do: add `<PackageVersion Include="Foo" Version="1.2.3" />` there, then reference it.
  - Don't: `<PackageReference Include="Foo" Version="1.2.3" />`.
- OpenTelemetry stable packages share `$(OTelVersion)`, beta instrumentations `$(OTelBetaVersion)`; bump together.
- Beta / heavy deps (EF Core, Redis, Kafka, RabbitMQ, EventPipe) only in their opt-in package, never in core
  `TaskTrooper.Telemetry`.

## Projects and naming

| Project | Contents |
|---|---|
| `TaskTrooper.Telemetry` | core distro: options, DSN, sampler, span processor, propagation, retry handler, GenAI, heartbeat |
| `TaskTrooper.Telemetry.AspNetCore` / `.EntityFrameworkCore` / `.Redis` | instrumentation add-ons |
| `TaskTrooper.Telemetry.Kafka` / `.RabbitMQ` / `.MassTransit` | consumer entry spans, producer propagation |
| `TaskTrooper.Telemetry.Profiling` | EventPipe continuous profiling (hosted service + uploader) |

- Namespace = project name (`TaskTrooper.Telemetry.Kafka`), file-scoped (`namespace X;`).
- **DI entry points live in the framework namespace** so users need no extra `using`:
  `namespace Microsoft.Extensions.DependencyInjection;` for `IServiceCollection` extensions,
  `namespace OpenTelemetry;` for `OpenTelemetryBuilder` extensions. Static classes named `TaskTrooper*Extensions`.
- Public surface: `AddTaskTrooper(...)` → `ITaskTrooperBuilder`, chainable `.AddSources/.AddMeters/.Add<Integration>()`.
  New integrations extend `ITaskTrooperBuilder` and return it.
- Types: public API prefixed `TaskTrooper*`, internals `Trooper*`. Default to `internal sealed`; make public only
  what users call. Cross-project access via `InternalsVisibleTo` (listed in each `.csproj`), not by widening visibility.
- Fields: camelCase without underscore, assigned with `this.` (`this.maxRetries = …`). Constants PascalCase.
- Guard public arguments with `ArgumentNullException.ThrowIfNull` (programmer error, at configuration time only).

## Configuration

- Resolution order (later wins): env `TROOPER_*` → `IConfiguration` section `TaskTrooper` → code delegate
  (`TaskTrooperOptionsSetup`). Env: `TROOPER_DSN`, `TROOPER_SERVICE` (or `OTEL_SERVICE_NAME`), `TROOPER_VERSION`,
  `TROOPER_ENVIRONMENT`, `TROOPER_SAMPLE_RATE`, `TROOPER_ENABLED`, `TROOPER_LLM_CAPTURE_CONTENT`.
- Parse numbers with `CultureInfo.InvariantCulture`. New option → property on `TaskTrooperOptions` + env mapping +
  contracts.md, in the same PR. Test-only knobs are `internal` (e.g. `RetryBaseDelay`).
- No DSN → nothing is exported; never throw for a missing/invalid DSN (record `dsn.invalid`).

## Never crash the host

- Every processor / sampler / propagator / middleware / consumer hook body:
  ```csharp
  try { … }
  catch (Exception e) { TrooperSelfDiagnostics.Error("processor.end", e); }
  ```
  `TrooperSelfDiagnostics` counts `trooper.sdk.errors{component}` on meter `TaskTrooper.Telemetry` (contracts §4) and
  only writes `Debug.WriteLine`. It never throws.
- Component names are dotted lower-case `<area>.<step>`: `processor.start`, `kafka.consume`, `aspnetcore.route`,
  `propagation.inject`. Reuse an existing one when the place is the same.
- Wrapped user work (Kafka handler, producer, LLM call) records the error on the `Activity` and **rethrows the user's
  exception unchanged**.
- No `ILogger` or `Console` output from the SDK.

## Async and network

- Export runs on OTel's batch exporter thread. Retries are in `TrooperRetryHandler` (429/5xx/network, exponential
  1s→60s full jitter, honours `Retry-After`, other 4xx dropped) — reuse it, don't add another retry loop.
- Library `await`s use `.ConfigureAwait(false)`; accept and pass through `CancellationToken`.
- No sync-over-async on request threads (the only blocking `Send` override is on the exporter thread).
- Background work has an owner that stops it: an `IHostedService` (`TrooperProfilerHostedService`), a named
  background thread (`IsBackground = true`, `trooper-profiler`) or a disposable `Timer` (`TrooperHeartbeat`).
  No unowned fire-and-forget `Task.Run`.
- Classes are `sealed` (or `static`); there is no inheritance-based extension point.

## PII and cardinality

- No headers, bodies or query strings by default; LLM content only when `LlmCaptureContent` is `true`.
- `trooper.route` is the route template via `TrooperRoute.Normalize` (`/orders/{id:int}` → `/orders/:id`).
- No user/request ids in metric tags.

## Docs and commits

- New public API or option → update [`README.md`](../README.md), [`.ai/sdk.md`](../../../.ai/sdk.md),
  [`.ai/contracts.md`](../../../.ai/contracts.md) in the same PR. English only; Conventional Commits
  (`feat(sdk-dotnet): …`).
