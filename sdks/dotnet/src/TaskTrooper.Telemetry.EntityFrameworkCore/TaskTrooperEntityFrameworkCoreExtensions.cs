using OpenTelemetry;
using OpenTelemetry.Trace;

namespace TaskTrooper.Telemetry;

public static class TaskTrooperEntityFrameworkCoreExtensions
{
    /// <summary>
    /// EF Core command spans (db.system, db.namespace, server.address). Statement text is captured as <c>db.query.text</c>
    /// only for parameterized SQL (literals are normalized by the writer). Not needed for Npgsql/SqlClient-only apps,
    /// which are covered by the core package.
    /// </summary>
    public static ITaskTrooperBuilder AddEntityFrameworkCore(this ITaskTrooperBuilder builder)
    {
        builder.OpenTelemetry.WithTracing(t => t.AddEntityFrameworkCoreInstrumentation());
        return builder;
    }
}
