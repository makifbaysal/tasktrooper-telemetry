using OpenTelemetry;
using System.Diagnostics;
using Microsoft.AspNetCore.Builder;
using Microsoft.AspNetCore.Hosting;
using Microsoft.AspNetCore.Http;
using Microsoft.AspNetCore.Http.Features;
using Microsoft.Extensions.DependencyInjection;
using Microsoft.Extensions.DependencyInjection.Extensions;
using OpenTelemetry.Metrics;
using OpenTelemetry.Trace;
using TaskTrooper.Telemetry.AspNetCore;

namespace TaskTrooper.Telemetry;

public sealed class TrooperAspNetCoreOptions
{
    /// <summary>Return false to skip tracing a request (e.g. health probes). Default: trace everything.</summary>
    public Func<HttpContext, bool>? Filter { get; set; }

    /// <summary>Record exceptions as span events (default true).</summary>
    public bool RecordException { get; set; } = true;
}

public static class TaskTrooperAspNetCoreExtensions
{
    /// <summary>
    /// ASP.NET Core server spans (incl. gRPC services) enriched with <c>trooper.route</c> (route template, <c>{id}</c> → <c>:id</c>),
    /// <c>trooper.entry.route</c>, <c>trooper.caller</c> and exception events; plus ASP.NET Core / Kestrel metrics.
    /// </summary>
    public static ITaskTrooperBuilder AddAspNetCore(this ITaskTrooperBuilder builder, Action<TrooperAspNetCoreOptions>? configure = null)
    {
        var options = new TrooperAspNetCoreOptions();
        configure?.Invoke(options);

        builder.OpenTelemetry
            .WithTracing(t => t.AddAspNetCoreInstrumentation(o =>
            {
                o.RecordException = options.RecordException;
                if (options.Filter is { } filter) o.Filter = filter;
                o.EnrichWithHttpRequest = TrooperAspNetCoreEnricher.OnRequest;
                o.EnrichWithHttpResponse = TrooperAspNetCoreEnricher.OnResponse;
            }))
            .WithMetrics(m => m.AddAspNetCoreInstrumentation());

        builder.Services.TryAddEnumerable(ServiceDescriptor.Singleton<IStartupFilter, TrooperRouteStartupFilter>());
        return builder;
    }
}

internal static class TrooperAspNetCoreEnricher
{
    public static void OnRequest(Activity activity, HttpRequest request)
    {
        try
        {
            if (activity.GetTagItem(TrooperAttributes.Caller) is not null) return;
            var caller = TrooperPropagation.CallerFromBaggageHeader(request.Headers["baggage"].ToString());
            if (caller is not null) activity.SetTag(TrooperAttributes.Caller, caller);
        }
        catch (Exception e)
        {
            TrooperSelfDiagnostics.Error("aspnetcore.request", e);
        }
    }

    public static void OnResponse(Activity activity, HttpResponse response)
    {
        try
        {
            if (activity.GetTagItem(TrooperAttributes.Route) is null) SetRoute(activity, response.HttpContext);
        }
        catch (Exception e)
        {
            TrooperSelfDiagnostics.Error("aspnetcore.response", e);
        }
    }

    public static void SetRoute(Activity? activity, HttpContext context)
    {
        if (activity is null) return;
        var route = TrooperRouteTemplate.FromEndpoint(context.GetEndpoint());
        if (route is not null) TrooperSpan.SetRoute(activity, route);
    }
}

/// <summary>
/// Sets the route on the server span as soon as routing has matched an endpoint (DiagnosticSource event
/// <c>Microsoft.AspNetCore.Routing.EndpointMatched</c>), so child spans started by the handler inherit
/// <c>trooper.entry.route</c>.
/// </summary>
internal sealed class TrooperRouteStartupFilter : IStartupFilter, IObserver<KeyValuePair<string, object?>>, IDisposable
{
    private const string EndpointMatched = "Microsoft.AspNetCore.Routing.EndpointMatched";
    private IDisposable? subscription;

    public Action<IApplicationBuilder> Configure(Action<IApplicationBuilder> next) => app =>
    {
        try
        {
            if (subscription is null && app.ApplicationServices.GetService<DiagnosticListener>() is { } listener)
                subscription = listener.Subscribe(this, name => name == EndpointMatched);
        }
        catch (Exception e)
        {
            TrooperSelfDiagnostics.Error("aspnetcore.subscribe", e);
        }

        next(app);
    };

    public void OnNext(KeyValuePair<string, object?> value)
    {
        try
        {
            if (value.Key != EndpointMatched || value.Value is not HttpContext context) return;
            var activity = context.Features.Get<IHttpActivityFeature>()?.Activity ?? Activity.Current;
            TrooperAspNetCoreEnricher.SetRoute(activity, context);
        }
        catch (Exception e)
        {
            TrooperSelfDiagnostics.Error("aspnetcore.route", e);
        }
    }

    public void OnCompleted() { }

    public void OnError(Exception error) { }

    public void Dispose() => subscription?.Dispose();
}
