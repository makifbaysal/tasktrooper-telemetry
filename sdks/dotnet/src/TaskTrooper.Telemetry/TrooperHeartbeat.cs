using System.Diagnostics.Metrics;
using System.Runtime.CompilerServices;
using OpenTelemetry.Metrics;

namespace TaskTrooper.Telemetry;

/// <summary>
/// <c>trooper.sdk.heartbeat</c> (gauge, 1, no attributes; meter <c>TaskTrooper.Telemetry</c>): presence signal so a
/// service without traffic still shows up in the Services list (.ai/contracts.md §4). Identity comes from the resource.
/// Only exported when a TaskTrooper metric reader exists (metrics enabled + DSN).
/// </summary>
internal static class TrooperHeartbeat
{
    public const string Name = "trooper.sdk.heartbeat";

    /// <summary>Delay of the one-off early collect that brings the first heartbeat in within 10 s (internal for tests).</summary>
    internal static TimeSpan EarlyExportDelay = TimeSpan.FromSeconds(5);

    private static readonly object Gate = new();
    private static ObservableGauge<int>? gauge;

    // keeps each one-shot timer alive exactly as long as its reader
    private static readonly ConditionalWeakTable<MetricReader, Timer> Timers = new();

    /// <summary>Creates the gauge on the SDK's own meter (once per process). Never throws.</summary>
    public static void Ensure()
    {
        try
        {
            lock (Gate)
            {
                gauge ??= TrooperSelfDiagnostics.Meter.CreateObservableGauge(Name, () => 1, unit: "1", description: "Presence signal of a running TaskTrooper SDK");
            }
        }
        catch (Exception e)
        {
            TrooperSelfDiagnostics.Error("heartbeat", e);
        }
    }

    /// <summary>
    /// Schedules one <see cref="MetricReader.Collect"/> after <see cref="EarlyExportDelay"/> on the thread pool (no
    /// dedicated thread, never blocks startup) when the export interval would delay the first heartbeat past 10 s.
    /// </summary>
    public static void ScheduleEarlyExport(MetricReader reader, int exportIntervalMs)
    {
        if (exportIntervalMs <= 10_000) return;
        try
        {
            var timer = new Timer(
                static state =>
                {
                    try
                    {
                        ((MetricReader)state!).Collect();
                    }
                    catch (Exception e)
                    {
                        TrooperSelfDiagnostics.Error("heartbeat", e);
                    }
                },
                reader,
                EarlyExportDelay,
                Timeout.InfiniteTimeSpan);
            Timers.AddOrUpdate(reader, timer);
        }
        catch (Exception e)
        {
            TrooperSelfDiagnostics.Error("heartbeat", e);
        }
    }
}
