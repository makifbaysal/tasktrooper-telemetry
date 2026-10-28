using System.Collections.Concurrent;
using System.Diagnostics;

namespace TaskTrooper.Telemetry.Profiling;

/// <summary>
/// Thread → entry span timeline used to put samples under <c># trooper.route=… trooper.method=…</c> sections (contracts §24).
///
/// <para>When an entry span (local root SERVER / CONSUMER, reported by the core <c>TrooperSpanProcessor</c>) starts, its
/// <see cref="Entry"/> is stored in an <see cref="AsyncLocal{T}"/>; it flows with the request's ExecutionContext across
/// <c>await</c>s, <c>Task.Run</c> and thread-pool hops. The AsyncLocal's change callback fires whenever a thread starts or
/// stops running code of a request, so each OS thread gets a timeline <c>(timestamp, entry)</c>; a sample on thread
/// <c>T</c> at time <c>t</c> belongs to the last transition of <c>T</c> at or before <c>t</c>. Timestamps are
/// <see cref="Stopwatch.GetTimestamp"/> — the clock EventPipe uses (QPC / CLOCK_MONOTONIC).</para>
///
/// <para>Outside a profiling window only the current entry per thread is tracked (one field write per switch);
/// timelines are recorded during windows, pruned behind the profiler's processing watermark and cleared afterwards.
/// Bounded, never throws.</para>
/// </summary>
internal static class ProfileRoutes
{
    internal const int MaxTransitionsPerThread = 1 << 20;
    private const int PruneEvery = 4_096;
    private const string EntryProperty = "trooper.profiler.entry";

    private static readonly AsyncLocal<Entry?> Current = new(OnChanged);
    private static readonly ConcurrentDictionary<int, ThreadTimeline> Timelines = new();
    [ThreadStatic] private static ThreadTimeline? local;
    private static volatile bool enabled;
    private static volatile bool recording;
    private static long watermark; // QPC of the newest event the profiler has processed

    /// <summary>Lets threads drop transitions older than events already processed (minus a margin for out-of-order events).</summary>
    public static void Processed(long timestamp)
    {
        if (timestamp > Volatile.Read(ref watermark)) Volatile.Write(ref watermark, timestamp);
    }

    public static bool Enabled => enabled;

    public static bool Recording => recording;

    /// <summary>Installs the core span hooks.</summary>
    public static void Enable()
    {
        enabled = true;
        TrooperProfilingHooks.EntryStarted = EntryStarted;
        TrooperProfilingHooks.EntryStopped = EntryStopped;
    }

    public static void Disable()
    {
        TrooperProfilingHooks.EntryStarted = null;
        TrooperProfilingHooks.EntryStopped = null;
        enabled = false;
        recording = false;
        foreach (var tl in Timelines.Values) tl.Removed = true; // threads re-register when profiling is enabled again
        Timelines.Clear();
    }

    /// <summary>Starts recording transitions; every known thread is seeded with its current entry at <paramref name="timestamp"/>.</summary>
    public static void BeginRecording(long timestamp)
    {
        Volatile.Write(ref watermark, 0);
        foreach (var tl in Timelines.Values) tl.Reset(timestamp);
        recording = true;
    }

    /// <summary>Stops recording; timelines stay readable until <see cref="Clear"/>.</summary>
    public static void EndRecording() => recording = false;

    /// <summary>Drops recorded transitions and the timelines of idle threads.</summary>
    public static void Clear()
    {
        foreach (var (tid, tl) in Timelines)
        {
            tl.Reset(null);
            if (tl.Current is null && Timelines.TryRemove(new KeyValuePair<int, ThreadTimeline>(tid, tl))) tl.Removed = true;
        }
    }

    /// <summary>Entry whose code ran on OS thread <paramref name="osThreadId"/> at <paramref name="timestamp"/>, or null.</summary>
    public static Entry? Lookup(int osThreadId, long timestamp) =>
        Timelines.TryGetValue(osThreadId, out var tl) ? tl.Find(timestamp) : null;

    /// <summary>Marks the current ExecutionContext as working for <paramref name="entry"/> (tests / manual use).</summary>
    internal static void SetCurrent(Entry? entry) => Current.Value = entry;

    private static void EntryStarted(Activity activity)
    {
        if (!enabled) return;
        try
        {
            var entry = new Entry(activity);
            activity.SetCustomProperty(EntryProperty, entry);
            Current.Value = entry;
        }
        catch (Exception e)
        {
            TrooperSelfDiagnostics.Error("profiler", e);
        }
    }

    private static void EntryStopped(Activity activity)
    {
        try
        {
            if (activity.GetCustomProperty(EntryProperty) is not Entry entry) return;
            entry.Freeze();
            activity.SetCustomProperty(EntryProperty, null);
            if (ReferenceEquals(Current.Value, entry)) Current.Value = null;
        }
        catch (Exception e)
        {
            TrooperSelfDiagnostics.Error("profiler", e);
        }
    }

    private static void OnChanged(AsyncLocalValueChangedArgs<Entry?> args)
    {
        if (!enabled) return;
        try
        {
            var tl = local;
            if (tl is null || tl.Removed)
            {
                var tid = NativeThread.CurrentOsThreadId();
                if (tid <= 0) return;
                tl ??= local = new ThreadTimeline();
                tl.Removed = false;
                Timelines[tid] = tl; // OS ids are reused: a new thread replaces the dead one's timeline
            }

            tl.Current = args.CurrentValue;
            if (recording) tl.Add(Stopwatch.GetTimestamp(), args.CurrentValue);
        }
        catch
        {
            // never throw from an ExecutionContext switch
        }
    }

    /// <summary>The request (entry span) a thread works for. Route and method are read lazily (frameworks set the route late).</summary>
    internal sealed class Entry
    {
        private Activity? activity;
        private volatile bool frozen;
        private string? route;
        private string? method;

        public Entry(Activity activity) => this.activity = activity;

        /// <summary>Test / manual constructor with a known route.</summary>
        public Entry(string? route, string? method)
        {
            this.route = route;
            this.method = method;
            frozen = true;
        }

        public (string? Route, string? Method) Resolve()
        {
            if (frozen) return (route, method);
            var a = activity;
            return a is null ? (route, method) : Read(a);
        }

        public void Freeze()
        {
            var a = activity;
            if (a is not null) (route, method) = Read(a);
            frozen = true;
            activity = null; // drop the Activity reference
        }

        private static (string?, string?) Read(Activity a)
        {
            try
            {
                return (TrooperSpan.EntryRouteOf(a),
                    a.GetTagItem("http.request.method") as string ?? a.GetTagItem("http.method") as string);
            }
            catch
            {
                return (null, null);
            }
        }
    }

    /// <summary>Transitions of one OS thread in time order. Written by the owner thread, read by the profiler thread.</summary>
    private sealed class ThreadTimeline
    {
        private readonly List<(long Ts, Entry? Entry)> transitions = new();
        public volatile Entry? Current;
        public volatile bool Removed;

        public void Add(long ts, Entry? entry)
        {
            lock (transitions)
            {
                if (transitions.Count >= PruneEvery && transitions.Count % PruneEvery == 0) Prune();
                if (transitions.Count >= MaxTransitionsPerThread) transitions.RemoveRange(0, transitions.Count / 2); // hard bound
                transitions.Add((ts, entry));
            }
        }

        /// <summary>Drops transitions before the processed watermark (minus 2 s), keeping the one in effect at the cutoff.</summary>
        private void Prune()
        {
            var mark = Volatile.Read(ref watermark);
            if (mark == 0) return;
            var cutoff = mark - 2 * Stopwatch.Frequency;
            var keepFrom = IndexAtOrBefore(cutoff);
            if (keepFrom > 0) transitions.RemoveRange(0, keepFrom);
        }

        private int IndexAtOrBefore(long ts)
        {
            int lo = 0, hi = transitions.Count - 1, found = -1;
            while (lo <= hi)
            {
                var mid = (lo + hi) >>> 1;
                if (transitions[mid].Ts <= ts)
                {
                    found = mid;
                    lo = mid + 1;
                }
                else
                {
                    hi = mid - 1;
                }
            }

            return found;
        }

        public void Reset(long? seedTimestamp)
        {
            lock (transitions)
            {
                transitions.Clear();
                if (seedTimestamp is { } ts) transitions.Add((ts, Current));
            }
        }

        public Entry? Find(long ts)
        {
            lock (transitions)
            {
                var found = IndexAtOrBefore(ts);
                return found >= 0 ? transitions[found].Entry : null;
            }
        }
    }
}
