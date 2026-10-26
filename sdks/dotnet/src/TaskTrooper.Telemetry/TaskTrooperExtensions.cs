using Microsoft.Extensions.DependencyInjection;
using Microsoft.Extensions.Options;
using OpenTelemetry.Exporter;
using OpenTelemetry.Logs;
using OpenTelemetry.Metrics;
using OpenTelemetry.Resources;
using OpenTelemetry.Trace;
using TaskTrooper.Telemetry;

namespace OpenTelemetry;

public static class TaskTrooperOpenTelemetryBuilderExtensions
{
    /// <summary>ActivitySources enabled by default; they only produce spans when the library is in use.</summary>
    internal static readonly string[] DefaultSources =
    {
        "TaskTrooper.Telemetry*", // our messaging helpers
        "Npgsql",                 // Npgsql 8+
        "RabbitMQ.Client.*",      // RabbitMQ.Client 7+
        "MassTransit",            // MassTransit 8+
    };

    internal static readonly string[] DefaultMeters =
    {
        TrooperSelfDiagnostics.MeterName,
        "System.Runtime",         // .NET 9+ runtime metrics
        "Npgsql",
        "MassTransit",
    };

    /// <summary>
    /// Configures an existing OpenTelemetry builder as the TaskTrooper distribution: resource, sampler,
    /// trooper.* processors, HttpClient + SqlClient instrumentation and OTLP/HTTP export of traces, metrics and logs
    /// to the DSN endpoint with the <c>x-trooper-key</c> header.
    /// </summary>
    public static ITaskTrooperBuilder UseTaskTrooper(this IOpenTelemetryBuilder builder, Action<TaskTrooperOptions>? configure = null)
    {
        ArgumentNullException.ThrowIfNull(builder);
        var services = builder.Services;
        if (services.Any(d => d.ServiceType == typeof(TaskTrooperMarker)))
        {
            if (configure is not null) services.Configure(configure);
            return new TaskTrooperBuilder(builder);
        }

        services.AddSingleton<TaskTrooperMarker>();
        services.AddSingleton(sp => new TaskTrooperOptionsSetup(sp));
        services.AddSingleton<IConfigureOptions<TaskTrooperOptions>>(sp => sp.GetRequiredService<TaskTrooperOptionsSetup>());
        services.AddSingleton<IPostConfigureOptions<TaskTrooperOptions>>(sp => sp.GetRequiredService<TaskTrooperOptionsSetup>());
        if (configure is not null) services.Configure(configure); // after env/config so code wins

        builder.ConfigureResource(r => r.AddDetector(sp => new TrooperResourceDetector(Options(sp))));

        builder.WithTracing(tracing =>
        {
            tracing
                .AddSource(DefaultSources)
                .AddHttpClientInstrumentation(o => o.RecordException = true)
                .AddSqlClientInstrumentation()
                .SetSampler(sp => new TrooperSampler(Options(sp).SampleRate))
                .AddProcessor(new TrooperSpanProcessor());

            ((IDeferredTracerProviderBuilder)tracing).Configure((sp, b) =>
            {
                var o = Options(sp);
                TrooperPropagation.ServiceName = o.ServiceName;
                Sdk.SetDefaultTextMapPropagator(new TrooperTextMapPropagator());
                if (o.Sources.Count > 0) b.AddSource(o.Sources.ToArray());
                if (Exporter(o, "v1/traces") is { } exporterOptions && o.EnableTraces)
                    b.AddProcessor(new BatchActivityExportProcessor(new OtlpTraceExporter(exporterOptions)));
            });
        });

        builder.WithMetrics(metrics =>
        {
            metrics.AddHttpClientInstrumentation().AddMeter(DefaultMeters);
            ((IDeferredMeterProviderBuilder)metrics).Configure((sp, b) =>
            {
                var o = Options(sp);
                if (o.Meters.Count > 0) b.AddMeter(o.Meters.ToArray());
                if (Exporter(o, "v1/metrics") is { } exporterOptions && o.EnableMetrics)
                    b.AddReader(new PeriodicExportingMetricReader(new OtlpMetricExporter(exporterOptions), exportIntervalMilliseconds: 30_000));
            });
        });

        builder.WithLogging(
            logging =>
            {
                ((IDeferredLoggerProviderBuilder)logging).Configure((sp, b) =>
                {
                    var o = Options(sp);
                    if (Exporter(o, "v1/logs") is { } exporterOptions && o.EnableLogs)
                        b.AddProcessor(new BatchLogRecordExportProcessor(new OtlpLogExporter(exporterOptions)));
                });
            },
            o =>
            {
                o.IncludeFormattedMessage = true;
                o.IncludeScopes = true;
            });

        return new TaskTrooperBuilder(builder);
    }

    private static TaskTrooperOptions Options(IServiceProvider sp) => sp.GetRequiredService<IOptions<TaskTrooperOptions>>().Value;

    /// <summary>OTLP/HTTP protobuf exporter options for one signal, or null when export is disabled / DSN missing.</summary>
    internal static OtlpExporterOptions? Exporter(TaskTrooperOptions o, string signalPath)
    {
        try
        {
            if (!o.Enabled) return null;
            var dsn = o.ParsedDsn;
            if (dsn is null)
            {
                if (!string.IsNullOrWhiteSpace(o.Dsn)) TrooperSelfDiagnostics.Error("dsn.invalid");
                return null;
            }

            var attemptTimeout = o.ExportTimeout;
            var retries = o.MaxExportRetries;
            var baseDelay = o.RetryBaseDelay;
            return new OtlpExporterOptions
            {
                Protocol = OtlpExportProtocol.HttpProtobuf,
                Endpoint = dsn.SignalEndpoint(signalPath),
                Headers = "x-trooper-key=" + dsn.Key,
                // overall budget incl. backoff; each attempt is bounded by ExportTimeout in the retry handler
                TimeoutMilliseconds = (int)Math.Min(int.MaxValue, attemptTimeout.TotalMilliseconds * (retries + 1) + 60_000 * Math.Min(retries, 3)),
                HttpClientFactory = () => new HttpClient(
                    new TrooperRetryHandler(new SocketsHttpHandler { PooledConnectionLifetime = TimeSpan.FromMinutes(5) }, retries, baseDelay, attemptTimeout))
                {
                    Timeout = Timeout.InfiniteTimeSpan,
                },
            };
        }
        catch (Exception e)
        {
            TrooperSelfDiagnostics.Error("exporter.options", e);
            return null;
        }
    }
}

internal sealed class TaskTrooperMarker;
