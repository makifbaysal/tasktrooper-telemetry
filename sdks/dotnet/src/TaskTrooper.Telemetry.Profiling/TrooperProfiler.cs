using System.Collections.Concurrent;
using Microsoft.Diagnostics.NETCore.Client;
using Microsoft.Extensions.Logging;
using Microsoft.Extensions.Logging.Abstractions;

namespace TaskTrooper.Telemetry.Profiling;

/// <summary>
/// Continuous profiler for .NET (ADR-022, contracts §24): every <see cref="TrooperProfilingOptions.Period"/> an in-process
/// EventPipe window of <see cref="TrooperProfilingOptions.Duration"/> is recorded and uploaded as one folded profile per
/// type to <c>/v1/profiles</c>. Normally started by the hosted service of <c>AddTaskTrooperProfiling()</c>.
///
/// <para>One profiler per process. Runs on a dedicated background thread. When the diagnostics IPC endpoint is disabled
/// (<c>DOTNET_EnableDiagnostics=0</c>, <c>DOTNET_EnableDiagnostics_IPC=0</c>), the platform has no EventPipe (browser,
/// mobile) or no DSN is configured, <see cref="Start"/> logs once and returns <c>false</c>. Never throws.</para>
/// </summary>
public sealed class TrooperProfiler : IDisposable
{
    private const int MaxConsecutiveFailures = 3;
    private static int running;

    private readonly TrooperProfilingOptions options;
    private readonly TaskTrooperOptions trooper;
    private readonly ILogger logger;
    private readonly ConcurrentDictionary<int, byte> ownThreads = new();
    private readonly CancellationTokenSource stop = new();
    private Thread? thread;
    private ProfileUploader? uploader;
    private int started;
    private int ownsSlot;

    public TrooperProfiler(TrooperProfilingOptions options, TaskTrooperOptions trooper, ILogger? logger = null)
    {
        this.options = options ?? new TrooperProfilingOptions();
        this.trooper = trooper ?? new TaskTrooperOptions();
        this.logger = logger ?? NullLogger.Instance;
    }

    /// <summary>True while the profiling thread runs.</summary>
    public bool IsRunning => thread is { IsAlive: true };

    /// <summary>Number of windows recorded (tests / diagnostics).</summary>
    public int Windows { get; private set; }

    /// <summary>Whether EventPipe can be used in this process; <paramref name="reason"/> explains why not.</summary>
    public static bool IsSupported(out string? reason)
    {
        reason = null;
        if (OperatingSystem.IsBrowser() || OperatingSystem.IsWasi() || OperatingSystem.IsIOS() || OperatingSystem.IsTvOS() ||
            OperatingSystem.IsAndroid() || OperatingSystem.IsMacCatalyst())
        {
            reason = "EventPipe diagnostics IPC is not available on this platform";
            return false;
        }

        foreach (var prefix in new[] { "DOTNET_", "COMPlus_" })
        {
            foreach (var name in new[] { "EnableDiagnostics", "EnableDiagnostics_IPC" })
            {
                if (Environment.GetEnvironmentVariable(prefix + name)?.Trim() == "0")
                {
                    reason = $"diagnostics are disabled ({prefix}{name}=0)";
                    return false;
                }
            }
        }

        return true;
    }

    /// <summary>Starts the profiling thread. Returns false when disabled, unsupported, without DSN or already running.</summary>
    public bool Start()
    {
        try
        {
            if (Interlocked.Exchange(ref started, 1) == 1) return IsRunning;
            if (!trooper.Enabled) return false;
            if (trooper.ParsedDsn is not { } dsn)
            {
                logger.LogInformation("TaskTrooper: profiling enabled but no valid DSN (TROOPER_DSN); profiler not started");
                return false;
            }

            if (!IsSupported(out var reason))
            {
                logger.LogInformation("TaskTrooper: profiling enabled but {Reason}; profiler not started", reason);
                return false;
            }

            if (options.Types.Count == 0) return false;
            if (Interlocked.CompareExchange(ref running, 1, 0) != 0)
            {
                logger.LogInformation("TaskTrooper: a profiler is already running in this process");
                return false;
            }

            ownsSlot = 1;

            var metadata = new ProfileMetadata(
                trooper.ServiceName ?? "unknown_service:dotnet",
                trooper.Environment,
                trooper.ServiceVersion,
                string.IsNullOrWhiteSpace(options.Host) ? Environment.MachineName : options.Host);
            uploader = new ProfileUploader(dsn, metadata, trooper.ExportTimeout, options.UploadRetryBaseDelay, options.UploadHandler);
            ProfileRoutes.Enable();
            thread = new Thread(Run) { IsBackground = true, Name = "trooper-profiler" };
            thread.Start();
            return true;
        }
        catch (Exception e)
        {
            TrooperSelfDiagnostics.Error("profiler", e);
            Release();
            return false;
        }
    }

    /// <summary>Stops the profiler; the current window is cut short and uploaded. Waits at most <paramref name="timeout"/>.</summary>
    public void Stop(TimeSpan? timeout = null)
    {
        try
        {
            stop.Cancel();
            thread?.Join(timeout ?? TimeSpan.FromSeconds(10));
        }
        catch (Exception e)
        {
            TrooperSelfDiagnostics.Error("profiler", e);
        }
    }

    public void Dispose() => Stop();

    private void Run()
    {
        var tid = NativeThread.CurrentOsThreadId();
        ownThreads[tid] = 0;
        try
        {
            if (stop.Token.WaitHandle.WaitOne(options.StartDelay < TimeSpan.Zero ? TimeSpan.Zero : options.StartDelay)) return;
            var collector = new EventPipeCollector(options.ProcessId, new HashSet<ProfileType>(options.Types), options.BufferSizeMb, ownThreads);
            var failures = 0;
            var next = DateTime.UtcNow;
            while (!stop.IsCancellationRequested)
            {
                next += options.Period <= TimeSpan.Zero ? TimeSpan.FromSeconds(60) : options.Period;
                try
                {
                    var window = collector.Collect(options.EffectiveDuration, stop.Token);
                    Windows++;
                    failures = 0;
                    Upload(window);
                }
                catch (DiagnosticsClientException e)
                {
                    // IPC endpoint missing / refused (DOTNET_EnableDiagnostics=0 set some other way, sandbox, …): give up
                    logger.LogInformation("TaskTrooper: EventPipe is not available ({Message}); profiler stopped", e.Message);
                    TrooperSelfDiagnostics.Error("profiler", e);
                    return;
                }
                catch (Exception e)
                {
                    TrooperSelfDiagnostics.Error("profiler", e);
                    if (++failures >= MaxConsecutiveFailures)
                    {
                        logger.LogInformation(e, "TaskTrooper: profiler failed {Count} times in a row; stopped", failures);
                        return;
                    }
                }

                var wait = next - DateTime.UtcNow;
                if (wait < TimeSpan.Zero)
                {
                    next = DateTime.UtcNow; // fell behind (slow upload / long window): no catch-up burst
                    wait = TimeSpan.Zero;
                }

                if (stop.Token.WaitHandle.WaitOne(wait)) return;
            }
        }
        catch (Exception e)
        {
            TrooperSelfDiagnostics.Error("profiler", e);
        }
        finally
        {
            ownThreads.TryRemove(tid, out _);
            Release();
        }
    }

    private void Upload(ProfileWindow window)
    {
        if (window.EndMs - window.StartMs < 1_000) return; // cut short at shutdown: not worth a profile
        foreach (var profile in window.Profiles.Values)
            uploader?.Upload(profile, window.StartMs, window.EndMs);
    }

    private void Release()
    {
        if (Interlocked.Exchange(ref ownsSlot, 0) == 0) return;
        try
        {
            ProfileRoutes.Disable();
            uploader?.Dispose();
        }
        catch
        {
            // ignore
        }
        finally
        {
            Interlocked.Exchange(ref running, 0);
        }
    }
}
