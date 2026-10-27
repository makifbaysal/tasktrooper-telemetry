using System.Diagnostics;
using TaskTrooper.Telemetry.Profiling;

namespace TaskTrooper.Telemetry.Profiling.Tests;

public class ProfileRoutesTests
{
    [Fact]
    public async Task Entry_follows_the_execution_context_to_other_threads()
    {
        ProfileRoutes.Enable();
        try
        {
            ProfileRoutes.BeginRecording(Stopwatch.GetTimestamp());
            var entry = new ProfileRoutes.Entry("/v1/orders/:id", "GET");
            var (tidIn, tsIn, tidOut, tsOut) = await RunRequest(entry);

            Assert.True(tidIn > 0);
            Assert.Same(entry, ProfileRoutes.Lookup(tidIn, tsIn));
            Assert.Equal(("/v1/orders/:id", "GET"), ProfileRoutes.Lookup(tidIn, tsIn)!.Resolve());
            // the same pool thread later ran code outside of the request
            if (tidOut == tidIn) Assert.Null(ProfileRoutes.Lookup(tidOut, tsOut));
            Assert.Null(ProfileRoutes.Lookup(987_654_321, tsIn));

            ProfileRoutes.EndRecording();
            ProfileRoutes.Clear();
            Assert.Null(ProfileRoutes.Lookup(tidIn, tsIn));
        }
        finally
        {
            ProfileRoutes.Disable();
        }
    }

    private static async Task<(int, long, int, long)> RunRequest(ProfileRoutes.Entry entry)
    {
        ProfileRoutes.SetCurrent(entry);
        await Task.Yield();
        var (tidIn, tsIn) = await Task.Run(async () =>
        {
            await Task.Delay(10);
            return (NativeThread.CurrentOsThreadId(), Stopwatch.GetTimestamp());
        });
        ProfileRoutes.SetCurrent(null);
        var (tidOut, tsOut) = await Task.Run(() => (NativeThread.CurrentOsThreadId(), Stopwatch.GetTimestamp()));
        return (tidIn, tsIn, tidOut, tsOut);
    }

    [Fact]
    public void Threads_register_again_after_the_profiler_restarts()
    {
        Exception? error = null;
        var t = new Thread(() =>
        {
            try
            {
                ProfileRoutes.Enable();
                ProfileRoutes.SetCurrent(new ProfileRoutes.Entry("/first", "GET"));
                ProfileRoutes.SetCurrent(null);
                ProfileRoutes.Disable();

                ProfileRoutes.Enable();
                ProfileRoutes.BeginRecording(Stopwatch.GetTimestamp());
                var second = new ProfileRoutes.Entry("/second", "GET");
                ProfileRoutes.SetCurrent(second);
                Assert.Same(second, ProfileRoutes.Lookup(NativeThread.CurrentOsThreadId(), Stopwatch.GetTimestamp()));
                ProfileRoutes.SetCurrent(null);
            }
            catch (Exception e)
            {
                error = e;
            }
            finally
            {
                ProfileRoutes.Disable();
            }
        });
        t.Start();
        t.Join();
        Assert.Null(error);
    }

    [Fact]
    public void Entry_route_is_read_lazily_from_the_activity_and_frozen_at_stop()
    {
        using var activity = new Activity("GET").SetIdFormat(ActivityIdFormat.W3C);
        activity.Start();
        var entry = new ProfileRoutes.Entry(activity);
        Assert.Equal((null, null), entry.Resolve());

        activity.SetTag("http.request.method", "GET");
        TrooperSpan.SetRoute(activity, "/v1/items/:id");
        Assert.Equal(("/v1/items/:id", "GET"), entry.Resolve());

        entry.Freeze();
        activity.SetTag(TrooperAttributes.EntryRoute, "/changed");
        Assert.Equal(("/v1/items/:id", "GET"), entry.Resolve());
    }

    [Fact]
    public void Os_thread_id_is_stable_per_thread_and_distinct_across_threads()
    {
        var a = NativeThread.CurrentOsThreadId();
        Assert.Equal(a, NativeThread.CurrentOsThreadId());
        var b = 0;
        var t = new Thread(() => b = NativeThread.CurrentOsThreadId());
        t.Start();
        t.Join();
        Assert.True(a > 0 && b > 0);
        Assert.NotEqual(a, b);
    }
}
