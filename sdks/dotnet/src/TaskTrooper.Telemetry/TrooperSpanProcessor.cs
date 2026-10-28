using System.Diagnostics;
using OpenTelemetry;

namespace TaskTrooper.Telemetry;

/// <summary>
/// Applies the request-scoped conventions:
/// <list type="bullet">
/// <item><c>trooper.entry.route</c> is copied from the local root (entry) activity onto every child activity.</item>
/// <item><c>trooper.caller</c> is read from the <c>tt.svc</c> baggage item of entry spans.</item>
/// </list>
/// Never throws.
/// </summary>
public sealed class TrooperSpanProcessor : BaseProcessor<Activity>
{
    public override void OnStart(Activity activity)
    {
        try
        {
            var root = TrooperSpan.LocalRoot(activity);
            if (ReferenceEquals(root, activity))
            {
                if (TrooperSpan.IsEntryKind(activity) && activity.GetTagItem(TrooperAttributes.Caller) is null)
                {
                    var caller = activity.GetBaggageItem(TrooperAttributes.CallerBaggageKey);
                    if (!string.IsNullOrEmpty(caller)) activity.SetTag(TrooperAttributes.Caller, caller);
                }

                if (TrooperSpan.IsEntryKind(activity)) TrooperProfilingHooks.EntryStarted?.Invoke(activity);
                return;
            }

            var entry = TrooperSpan.EntryRouteOf(root);
            if (entry is not null) activity.SetTag(TrooperAttributes.EntryRoute, entry);
        }
        catch (Exception e)
        {
            TrooperSelfDiagnostics.Error("processor.start", e);
        }
    }

    public override void OnEnd(Activity activity)
    {
        try
        {
            if (activity.GetTagItem(TrooperAttributes.EntryRoute) is not null)
            {
                NotifyProfiler(activity);
                return;
            }

            var root = TrooperSpan.LocalRoot(activity);
            if (ReferenceEquals(root, activity) && activity.Kind == ActivityKind.Server &&
                activity.GetTagItem(TrooperAttributes.Route) is null && activity.GetTagItem("http.route") is string httpRoute)
            {
                activity.SetTag(TrooperAttributes.Route, TrooperRoute.Normalize(httpRoute));
            }

            // child started before the route was known (or the root itself): fill now
            var entry = TrooperSpan.EntryRouteOf(root);
            if (entry is not null) activity.SetTag(TrooperAttributes.EntryRoute, entry);
            NotifyProfiler(activity);
        }
        catch (Exception e)
        {
            TrooperSelfDiagnostics.Error("processor.end", e);
        }
    }

    private static void NotifyProfiler(Activity activity)
    {
        if (TrooperProfilingHooks.EntryStopped is { } stopped && activity.Parent is null && TrooperSpan.IsEntryKind(activity))
            stopped(activity);
    }
}

/// <summary>Helpers for integrations that mark entry spans.</summary>
public static class TrooperSpan
{
    /// <summary>Top-most in-process ancestor of <paramref name="activity"/>.</summary>
    public static Activity LocalRoot(Activity activity)
    {
        var current = activity;
        for (var depth = 0; current.Parent is { } parent && depth < 1024; depth++) current = parent;
        return current;
    }

    public static bool IsEntryKind(Activity activity) =>
        activity.Kind is ActivityKind.Server or ActivityKind.Consumer;

    /// <summary>Entry route of a local root: explicit entry route, else route (server) or process (consumer).</summary>
    public static string? EntryRouteOf(Activity root) =>
        root.GetTagItem(TrooperAttributes.EntryRoute) as string
        ?? root.GetTagItem(TrooperAttributes.Route) as string
        ?? root.GetTagItem(TrooperAttributes.Process) as string
        ?? (root.Kind == ActivityKind.Consumer ? root.GetTagItem("messaging.consumer.group.name") as string : null);

    /// <summary>Marks a server entry span with its normalized route (also its entry route).</summary>
    public static void SetRoute(Activity? activity, string route)
    {
        if (activity is null) return;
        activity.SetTag(TrooperAttributes.Route, route);
        activity.SetTag(TrooperAttributes.EntryRoute, route);
    }

    /// <summary>Marks a consumer entry span with its process name (also its entry route).</summary>
    public static void SetProcess(Activity? activity, string process)
    {
        if (activity is null) return;
        activity.SetTag(TrooperAttributes.Process, process);
        activity.SetTag(TrooperAttributes.EntryRoute, process);
    }

    /// <summary>Flags the current (or given) consumer span as dead-lettered.</summary>
    public static void MarkDeadLettered(Activity? activity = null) =>
        (activity ?? Activity.Current)?.SetTag(TrooperAttributes.ConsumerDlq, true);
}
