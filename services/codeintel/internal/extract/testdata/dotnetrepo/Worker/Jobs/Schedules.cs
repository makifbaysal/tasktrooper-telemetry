using Hangfire;
using Quartz;

namespace Worker.Jobs;

public static class Schedules
{
    public static void Register(IServiceCollectionQuartzConfigurator q, IRecurringJobManager jobs)
    {
        RecurringJob.AddOrUpdate("nightly-invoices", () => Console.WriteLine("invoices"), Cron.Daily);
        RecurringJob.AddOrUpdate<CleanupJob>("cleanup", x => x.Run(), "*/5 * * * *");
        jobs.AddOrUpdate("weekly-report", () => Report.Send(), Cron.Weekly(DayOfWeek.Friday, 17));
        RecurringJob.AddOrUpdate<Reindexer>(x => x.Reindex(), Cron.Hourly(15));

        var jobKey = new JobKey("SendEmailJob");
        q.AddJob<SendEmailJob>(o => o.WithIdentity(jobKey));
        q.AddTrigger(o => o.ForJob(jobKey).WithIdentity("SendEmailJob-trigger").WithCronSchedule("0 0/5 * * * ?"));
        q.AddTrigger(o => o.WithIdentity("purge-trigger").WithCronSchedule("0 0 3 * * ?"));

        var cache = new System.Collections.Concurrent.ConcurrentDictionary<string, int>();
        cache.AddOrUpdate("k", 1, (k, v) => v + 1);
    }
}
