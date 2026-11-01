# .NET SDK — testing

Cross-cutting test rules: [`.ai/testing.md`](../../../.ai/testing.md). Behaviour under test is specified in
[`.ai/sdk.md`](../../../.ai/sdk.md) and [`.ai/contracts.md`](../../../.ai/contracts.md).

## Gates — be honest about them

- **Not in PR CI, not in `make check`.** `.github/workflows/ci.yml` and the root `Makefile` cover Go services,
  `apps/web` and `sdks/js` only. Nothing builds or tests `sdks/dotnet` on a PR.
- **Only gate:** [`.github/workflows/release-dotnet.yml`](../../../.github/workflows/release-dotnet.yml) on a
  `dotnet-vX.Y.Z` tag runs `dotnet restore` → `dotnet build -c Release` → `dotnet test -c Release --no-build` (SDK
  10.0.x) before packing. A red test blocks the release, which is late. So:
- **Run `./build.sh` (or `dotnet test`) locally before every PR that touches `sdks/dotnet`.** Say in the PR that you did.

## Commands

```bash
cd sdks/dotnet
./build.sh            # Docker (mcr.microsoft.com/dotnet/sdk:10.0): restore + Release build + test — no local dotnet needed
./build.sh build      # build only
./build.sh pack       # NuGet packages → artifacts/ (release dry run)
./build.sh shell      # interactive container

# with a local .NET 10 SDK (same steps as the release workflow)
dotnet test TaskTrooper.sln -c Release
dotnet test tests/TaskTrooper.Telemetry.Tests --filter "FullyQualifiedName~DsnAndRouteTests"
```

The Release build compiles all three library TFMs (net8.0/9.0/10.0); tests run on net10.0 only.

## Stack

- **xUnit 2** (`xunit`, `xunit.runner.visualstudio`, `Microsoft.NET.Test.Sdk`); `Xunit` is a global `using` in each test
  `.csproj`. Plain `Assert.*` — no FluentAssertions/Shouldly.
- **No mocking library** (no Moq/NSubstitute). Use real pipelines and fakes:
  - `OpenTelemetry.Exporter.InMemory` with a real `ServiceCollection` + `AddTaskTrooper(...)` to capture `Activity`s.
  - `CaptureServer` ([`TestUtil.cs`](../tests/TaskTrooper.Telemetry.Tests/TestUtil.cs)): `HttpListener` on a free
    `127.0.0.1` port as fake ingest / downstream; `WaitForAsync(path)` polls with a timeout. The profiling tests link the
    same file (`<Compile Include="..\TaskTrooper.Telemetry.Tests\TestUtil.cs" />`) — reuse it, don't copy it.
  - ASP.NET Core via `Microsoft.AspNetCore.TestHost` (`AspNetCoreTests`); M.E.AI via its real `ActivitySource`
    (`LlmTests`); Kafka/RabbitMQ via header objects and handlers, no broker (`MessagingTests`).
- Internals are reachable through `InternalsVisibleTo` (`TaskTrooper.Telemetry.Tests`, `...Profiling.Tests`). Test the
  internal type directly (`TrooperDsn`, `TrooperRoute`, `TrooperRetryHandler`) rather than widening visibility.

## Layout and style

- Test projects: `tests/<Project>.Tests/`, `net10.0`, `IsPackable=false`, `IsTestProject=true`; add to
  `TaskTrooper.sln`. Namespace `<Project>.Tests`.
- Files group a behaviour area, `<Area>Tests.cs` (`DsnAndRouteTests`, `PropagationAndSamplingTests`, `ExporterTests`).
- Method names are sentence-style snake case: `Parses_valid_dsn`, `Normalizes_route_templates`.
- Table-driven: `[Theory]` + `[InlineData]` for parsing/normalization tables (see `DsnAndRouteTests`). Route/DSN rows
  must match the shared contract tables used by the other SDKs.
- **Parallelization is off** for `TaskTrooper.Telemetry.Tests` (`[assembly: CollectionBehavior(DisableTestParallelization = true)]`
  in `TestUtil.cs`) because `ActivitySource` listeners and meters are process-global. Keep tests independent anyway:
  build a fresh `ServiceProvider` per test and dispose it.
- Speed up retry tests with the internal `TaskTrooperOptions.RetryBaseDelay`, not real 1s+ waits.
- No external network, Docker or brokers inside tests; poll with a deadline instead of fixed `Task.Delay` sleeps.

## What every change needs

| Change | Test |
|---|---|
| New option / env var / DSN rule | option-resolution or `DsnAndRouteTests` case |
| Route normalization | `[InlineData]` row in `Normalizes_route_templates` (and the same row in the other SDKs' tables) |
| New processor / middleware / consumer hook | happy path **and** a failure path proving the host call still succeeds (user exceptions rethrown unchanged) |
| Exporter / retry / payload change | `CaptureServer` test asserting the request (path, headers, retry on 429/5xx, drop on other 4xx) |
| GenAI / LLM | `LlmTests`, incl. content stripped when `LlmCaptureContent = false` |
| Profiling | `TaskTrooper.Telemetry.Profiling.Tests` (folded format, uploader, route attribution) |

Samples (`samples/MinimalApi`, `KafkaWorker`, `ProfilingOverhead`) are built by the solution but are not tests;
`ProfilingOverhead` is a manual overhead check (`dotnet run -c Release --project samples/ProfilingOverhead -- 30 4`).
