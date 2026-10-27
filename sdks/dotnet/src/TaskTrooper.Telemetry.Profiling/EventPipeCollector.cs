using System.Collections.Concurrent;
using System.Diagnostics;
using System.Diagnostics.Tracing;
using Microsoft.Diagnostics.NETCore.Client;
using Microsoft.Diagnostics.Tracing;
using Microsoft.Diagnostics.Tracing.Etlx;
using Microsoft.Diagnostics.Tracing.EventPipe;
using Microsoft.Diagnostics.Tracing.Parsers.Clr;

namespace TaskTrooper.Telemetry.Profiling;

/// <summary>Profiles of one window, one per type.</summary>
internal sealed record ProfileWindow(long StartMs, long EndMs, IReadOnlyDictionary<ProfileType, FoldedProfile> Profiles);

/// <summary>
/// One profiling window: an in-process EventPipe session (<see cref="DiagnosticsClient"/> on our own PID) streamed through
/// TraceEvent's real-time <see cref="TraceLog"/>, which resolves stack addresses to method names (a short rundown
/// session at the start names methods compiled earlier; JIT/Loader events name the ones compiled during the window).
/// <list type="bullet">
/// <item><c>Microsoft-DotNETCore-SampleProfiler</c>: thread samples (~1 kHz) → <c>cpu</c> (managed-code samples) and
/// <c>wall</c> (all samples), each weighted by the time since that thread's previous sample (≤ 10 ms).</item>
/// <item><c>Microsoft-Windows-DotNETRuntime</c> GC (0x1, verbose) → <c>GCAllocationTick</c> → <c>alloc_space</c> (bytes);
/// Contention (0x4000) → <c>ContentionStart</c> stack + <c>ContentionStop</c> <c>DurationNs</c> → <c>lock</c>.</item>
/// </list>
/// Samples of the profiler's own threads are skipped. Events run on a dedicated thread; never on application threads.
/// </summary>
internal sealed class EventPipeCollector
{
    internal const string SampleProfilerProvider = "Microsoft-DotNETCore-SampleProfiler";
    internal const string RuntimeProvider = "Microsoft-Windows-DotNETRuntime";
    internal const long GcKeyword = 0x1;
    internal const long LoaderKeyword = 0x8;
    internal const long JitKeyword = 0x10;
    internal const long ContentionKeyword = 0x4000;

    private const double NominalSampleMs = 1.0;
    private const double MaxSampleMs = 10.0;
    private const int MaxStackWalk = 1024;

    private readonly int processId;
    private readonly IReadOnlySet<ProfileType> types;
    private readonly int bufferSizeMb;
    private readonly ConcurrentDictionary<int, byte> ownThreads;

    public EventPipeCollector(int processId, IReadOnlySet<ProfileType> types, int bufferSizeMb, ConcurrentDictionary<int, byte> ownThreads)
    {
        this.processId = processId;
        this.types = types;
        this.bufferSizeMb = Math.Clamp(bufferSizeMb, 4, 1024);
        this.ownThreads = ownThreads;
    }

    internal static IReadOnlyList<EventPipeProvider> Providers(IReadOnlySet<ProfileType> types)
    {
        var list = new List<EventPipeProvider>();
        if (types.Contains(ProfileType.Cpu) || types.Contains(ProfileType.Wall))
            list.Add(new EventPipeProvider(SampleProfilerProvider, EventLevel.Informational));
        var keywords = LoaderKeyword | JitKeyword; // MethodLoadVerbose: names of methods compiled during the window
        if (types.Contains(ProfileType.AllocSpace)) keywords |= GcKeyword;
        if (types.Contains(ProfileType.Lock)) keywords |= ContentionKeyword;
        list.Add(new EventPipeProvider(RuntimeProvider, EventLevel.Verbose, keywords));
        return list;
    }

    /// <summary>
    /// Runs one window of <paramref name="duration"/> (shorter when <paramref name="cancel"/> fires).
    /// Throws <see cref="DiagnosticsClientException"/> when the diagnostics IPC endpoint is not available.
    /// </summary>
    public ProfileWindow Collect(TimeSpan duration, CancellationToken cancel)
    {
        var client = new DiagnosticsClient(processId);
        var session = client.StartEventPipeSession(Providers(types), requestRundown: false, circularBufferMB: bufferSizeMb);
        var startMs = DateTimeOffset.UtcNow.ToUnixTimeMilliseconds();
        var sink = new Sink(types);
        Thread? reader = null;
        TraceLogEventSource? source = null;
        try
        {
            ProfileRoutes.BeginRecording(Stopwatch.GetTimestamp());
            source = TraceLog.CreateFromEventPipeSession(session, TraceLog.EventPipeRundownConfiguration.Enable(client));
            Subscribe(source, sink);

            var src = source;
            reader = new Thread(() =>
            {
                var tid = NativeThread.CurrentOsThreadId();
                ownThreads[tid] = 0;
                try
                {
                    src.Process();
                }
                catch (Exception e)
                {
                    TrooperSelfDiagnostics.Error("profiler", e);
                }
                finally
                {
                    ownThreads.TryRemove(tid, out _);
                }
            })
            {
                IsBackground = true,
                Name = "trooper-profiler-events",
            };
            reader.Start();
            cancel.WaitHandle.WaitOne(duration);
        }
        finally
        {
            var endMs = DateTimeOffset.UtcNow.ToUnixTimeMilliseconds();
            sink.EndMs = endMs;
            try
            {
                session.Stop(); // flushes; Process() returns at end of stream
            }
            catch (Exception e)
            {
                TrooperSelfDiagnostics.Error("profiler", e);
            }

            if (reader is not null && !reader.Join(TimeSpan.FromSeconds(30)))
                TrooperSelfDiagnostics.Error("profiler"); // stream did not end; dispose below breaks it

            ProfileRoutes.EndRecording();
            try
            {
                source?.Dispose();
                session.Dispose();
            }
            catch (Exception e)
            {
                TrooperSelfDiagnostics.Error("profiler", e);
            }

            ProfileRoutes.Clear();
        }

        if (sink.Errors > 0) TrooperSelfDiagnostics.Error("profiler");
        return new ProfileWindow(startMs, sink.EndMs, sink.Profiles);
    }

    private void Subscribe(TraceLogEventSource source, Sink sink)
    {
        if (types.Contains(ProfileType.Cpu) || types.Contains(ProfileType.Wall))
        {
            var parser = new SampleProfilerTraceEventParser(source);
            parser.ThreadSample += d => Guard(sink, () => OnSample(sink, d));
        }

        if (types.Contains(ProfileType.AllocSpace))
            source.Clr.GCAllocationTick += d => Guard(sink, () => OnAllocation(sink, d));

        if (types.Contains(ProfileType.Lock))
        {
            source.Clr.ContentionStart += d => Guard(sink, () => OnContentionStart(sink, d));
            source.Clr.ContentionStop += d => Guard(sink, () => OnContentionStop(sink, d));
        }
    }

    private static void Guard(Sink sink, Action a)
    {
        try
        {
            a();
        }
        catch
        {
            sink.Errors++; // counted once per window, never per event
        }
    }

    private void OnSample(Sink sink, ClrThreadSampleTraceData d)
    {
        var tid = d.ThreadID;
        if (ownThreads.ContainsKey(tid)) return;
        var now = d.TimeStampRelativeMSec;
        var weightMs = sink.LastSample.TryGetValue(tid, out var last) ? Math.Clamp(now - last, 0, MaxSampleMs) : NominalSampleMs;
        sink.LastSample[tid] = now;
        var ns = (long)(weightMs * 1_000_000);
        if (ns <= 0) return;

        var managed = d.Type == ClrThreadSampleType.Managed;
        if (!managed && !sink.Wall) return; // cpu only counts managed code
        var stack = sink.Fold(d.CallStack());
        var (route, method) = sink.Route(tid, Qpc(d));
        if (managed) sink.Add(ProfileType.Cpu, route, method, stack, ns);
        sink.Add(ProfileType.Wall, route, method, stack, ns);
    }

    private void OnAllocation(Sink sink, GCAllocationTickTraceData d)
    {
        if (ownThreads.ContainsKey(d.ThreadID)) return;
        var bytes = d.AllocationAmount64 > 0 ? d.AllocationAmount64 : d.AllocationAmount;
        if (bytes <= 0) return;
        var (route, method) = sink.Route(d.ThreadID, Qpc(d));
        sink.Add(ProfileType.AllocSpace, route, method, sink.Fold(d.CallStack()), bytes);
    }

    private void OnContentionStart(Sink sink, ContentionStartTraceData d)
    {
        if (ownThreads.ContainsKey(d.ThreadID)) return;
        var (route, method) = sink.Route(d.ThreadID, Qpc(d));
        sink.Contention[d.ThreadID] = new PendingContention(sink.Fold(d.CallStack()), route, method, d.TimeStampRelativeMSec);
    }

    private static void OnContentionStop(Sink sink, ContentionStopTraceData d)
    {
        if (!sink.Contention.Remove(d.ThreadID, out var start)) return;
        double ns;
        try
        {
            ns = d.DurationNs; // .NET 8+ (ContentionStop_V1)
        }
        catch
        {
            ns = 0;
        }

        if (ns <= 0) ns = (d.TimeStampRelativeMSec - start.StartMs) * 1_000_000;
        sink.Add(ProfileType.Lock, start.Route, start.Method, start.Stack, (long)ns);
    }

#pragma warning disable CS0618 // "discouraged": the raw QPC is what Stopwatch.GetTimestamp() returns, needed for route lookup
    private static long Qpc(TraceEvent d) => d.TimeStampQPC;
#pragma warning restore CS0618

    private sealed record PendingContention(string Stack, string? Route, string? Method, double StartMs);

    /// <summary>Per-window aggregation state; only touched by the event thread.</summary>
    private sealed class Sink
    {
        private readonly Dictionary<ProfileType, FoldedProfile> profiles = new();
        private readonly Dictionary<CallStackIndex, string> stackCache = new();
        private readonly Dictionary<CodeAddressIndex, string> frameCache = new();
        private readonly List<string> frames = new(64);
        private bool? clockOk;

        public Sink(IReadOnlySet<ProfileType> types)
        {
            foreach (var t in types) profiles[t] = new FoldedProfile(t);
            Wall = types.Contains(ProfileType.Wall);
        }

        public bool Wall { get; }

        public int Errors { get; set; }

        public long EndMs { get; set; }

        public Dictionary<int, double> LastSample { get; } = new();

        public Dictionary<int, PendingContention> Contention { get; } = new();

        public IReadOnlyDictionary<ProfileType, FoldedProfile> Profiles => profiles;

        public void Add(ProfileType type, string? route, string? method, string stack, long value)
        {
            if (profiles.TryGetValue(type, out var p)) p.Add(route, method, stack, value);
        }

        public (string? Route, string? Method) Route(int osThreadId, long qpc)
        {
            if (!ProfileRoutes.Enabled) return (null, null);
            // EventPipe and Stopwatch share QPC / CLOCK_MONOTONIC; verify once per window and give up on a mismatch
            clockOk ??= Math.Abs(qpc - Stopwatch.GetTimestamp()) < Stopwatch.Frequency * 600;
            if (clockOk != true) return (null, null);
            ProfileRoutes.Processed(qpc);
            return ProfileRoutes.Lookup(osThreadId, qpc)?.Resolve() ?? (null, null);
        }

        /// <summary>Folded stack (root first) of a TraceLog call stack; cached per stack index.</summary>
        public string Fold(TraceCallStack? leaf)
        {
            if (leaf is null) return FrameNames.Unknown;
            var index = leaf.CallStackIndex;
            if (stackCache.TryGetValue(index, out var cached)) return cached;

            frames.Clear();
            var truncated = false;
            for (var c = leaf; c is not null; c = c.Caller)
            {
                if (frames.Count >= MaxStackWalk)
                {
                    truncated = true;
                    break;
                }

                var frame = Frame(c.CodeAddress);
                // consecutive unknown frames of one module collapse into one
                if (frames.Count > 0 && frame[0] == '[' && frames[^1] == frame) continue;
                frames.Add(frame);
            }

            frames.Reverse();
            if (truncated && frames.Count <= FoldedProfile.MaxDepth) frames.Insert(0, FoldedProfile.Truncated);
            var stack = frames.Count == 0 ? FrameNames.Unknown : FoldedProfile.Stack(frames);
            stackCache[index] = stack;
            return stack;
        }

        private string Frame(TraceCodeAddress address)
        {
            var index = address.CodeAddressIndex;
            if (frameCache.TryGetValue(index, out var cached)) return cached;
            var method = address.Method;
            var name = method is not null && !string.IsNullOrEmpty(method.FullMethodName)
                ? FrameNames.Method(method.FullMethodName)
                : string.IsNullOrEmpty(address.FullMethodName) ? FrameNames.UnknownFrame(address.ModuleName) : FrameNames.Method(address.FullMethodName);
            frameCache[index] = name;
            return name;
        }
    }
}
