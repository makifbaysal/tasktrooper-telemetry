using System.Diagnostics;
using System.Diagnostics.Metrics;

namespace TaskTrooper.Telemetry;

/// <summary>Internal telemetry of the SDK itself. SDK errors are swallowed and counted here (AGENTS.md rule 5).</summary>
internal static class TrooperSelfDiagnostics
{
    public const string MeterName = "TaskTrooper.Telemetry";

    private static readonly Meter Meter = new(MeterName, typeof(TrooperSelfDiagnostics).Assembly.GetName().Version?.ToString());
    private static readonly Counter<long> Errors = Meter.CreateCounter<long>("trooper.sdk.errors", description: "Errors swallowed by the TaskTrooper SDK");
    private static readonly Counter<long> Retries = Meter.CreateCounter<long>("trooper.sdk.export.retries", description: "OTLP export retries");

    public static void Error(string component, Exception? e = null)
    {
        try
        {
            Errors.Add(1, new KeyValuePair<string, object?>("component", component));
            Debug.WriteLine($"[TaskTrooper] {component}: {e}");
        }
        catch
        {
            // never throw from diagnostics
        }
    }

    public static void Retry(int status) =>
        Retries.Add(1, new KeyValuePair<string, object?>("http.response.status_code", status));
}
