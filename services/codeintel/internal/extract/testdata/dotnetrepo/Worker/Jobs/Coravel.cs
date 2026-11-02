using Coravel;

namespace Worker.Jobs;

public static class CoravelSetup
{
    public static void Use(IServiceProvider services)
    {
        services.UseScheduler(scheduler =>
        {
            scheduler.Schedule<SendDailyReport>().DailyAt(8, 30);
            scheduler.Schedule<PruneSessions>().Cron("0 */6 * * *");
        });
    }
}
