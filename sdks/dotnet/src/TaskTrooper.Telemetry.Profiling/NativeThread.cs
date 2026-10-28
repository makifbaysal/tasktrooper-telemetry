using System.Runtime.InteropServices;

namespace TaskTrooper.Telemetry.Profiling;

/// <summary>
/// OS thread id of the current thread — the id EventPipe stamps on events (Linux <c>gettid</c>, Windows
/// <c>GetCurrentThreadId</c>, macOS <c>pthread_threadid_np</c>). Cached per thread; −1 when unavailable.
/// </summary>
internal static class NativeThread
{
    [ThreadStatic] private static int cached;

    public static int CurrentOsThreadId()
    {
        var id = cached;
        if (id != 0) return id;
        id = Query();
        cached = id == 0 ? -1 : id;
        return cached;
    }

    private static int Query()
    {
        try
        {
            if (OperatingSystem.IsLinux()) return LinuxTid();
            if (OperatingSystem.IsWindows()) return unchecked((int)GetCurrentThreadId());
            if (OperatingSystem.IsMacOS() && pthread_threadid_np(IntPtr.Zero, out var tid) == 0) return unchecked((int)tid);
        }
        catch
        {
            // fall through: no route attribution
        }

        return -1;
    }

    private static int LinuxTid()
    {
        try
        {
            return gettid();
        }
        catch (Exception e) when (e is EntryPointNotFoundException or DllNotFoundException)
        {
            // glibc < 2.30: /proc/thread-self → "<pid>/task/<tid>"
            var target = new FileInfo("/proc/thread-self").LinkTarget;
            var slash = target?.LastIndexOf('/') ?? -1;
            return slash >= 0 && int.TryParse(target![(slash + 1)..], out var tid) ? tid : -1;
        }
    }

    [DllImport("libc", EntryPoint = "gettid")]
    private static extern int gettid();

    [DllImport("kernel32.dll")]
    private static extern uint GetCurrentThreadId();

    [DllImport("libc")]
    private static extern int pthread_threadid_np(IntPtr thread, out ulong threadId);
}
