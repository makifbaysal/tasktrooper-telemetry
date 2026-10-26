using System.Diagnostics;
using Microsoft.AspNetCore.Builder;
using Microsoft.AspNetCore.Hosting;
using Microsoft.AspNetCore.Http;
using Microsoft.AspNetCore.Mvc;
using Microsoft.AspNetCore.TestHost;
using Microsoft.Extensions.DependencyInjection;
using OpenTelemetry;
using OpenTelemetry.Trace;

namespace TaskTrooper.Telemetry.Tests;

[ApiController]
[Route("api/users")]
public class UsersController : ControllerBase
{
    [HttpGet("{userId:int}/orders/{orderId}")]
    public IActionResult Get(int userId, string orderId)
    {
        using var child = AspNetCoreTests.ChildSource.StartActivity("load-orders");
        return Ok(new { userId, orderId });
    }
}

public class ReportsController : Controller
{
    public IActionResult Show(int id) => Content("report " + id);
}

public sealed class AspNetCoreTests : IAsyncLifetime
{
    public static readonly ActivitySource ChildSource = new("Test.Child");

    private readonly List<Activity> exported = new();
    private WebApplication app = null!;
    private HttpClient client = null!;

    public async Task InitializeAsync()
    {
        var builder = WebApplication.CreateBuilder();
        builder.WebHost.UseTestServer();
        builder.Services.AddControllersWithViews().AddApplicationPart(typeof(UsersController).Assembly);
        builder.Services
            .AddTaskTrooper(o =>
            {
                o.ServiceName = "orders-api";
                o.Dsn = null; // in-memory only
            })
            .AddAspNetCore()
            .AddSources("Test.Child")
            .OpenTelemetry.WithTracing(t => t.AddInMemoryExporter(exported));

        app = builder.Build();
        app.MapGet("/orders/{id:int}", (int id) =>
        {
            using var child = ChildSource.StartActivity("db-call", ActivityKind.Client);
            using var grandChild = ChildSource.StartActivity("serialize");
            return Results.Ok(id);
        });
        app.MapGet("/boom/{kind}", (string kind) =>
        {
            throw new InvalidOperationException("boom " + kind);
#pragma warning disable CS0162
            return Results.Ok();
#pragma warning restore CS0162
        });
        app.MapControllers();
        app.MapControllerRoute("default", "{controller=Home}/{action=Index}/{id?}");
        await app.StartAsync();
        client = app.GetTestClient();
    }

    public async Task DisposeAsync()
    {
        await app.DisposeAsync();
    }

    private async Task<List<Activity>> TraceOf(Func<Task> request, string displayNameContains)
    {
        await request();
        return await exported.WaitForAsync(list => list.Any(a => a.Kind == ActivityKind.Server && a.DisplayName.Contains(displayNameContains)));
    }

    [Fact]
    public async Task Minimal_api_route_template_is_captured_and_normalized()
    {
        var spans = await TraceOf(() => client.GetAsync("/orders/42"), "/orders");
        var server = spans.Single(a => a.Kind == ActivityKind.Server);
        Assert.Equal("/orders/:id", server.Tag(TrooperAttributes.Route));
        Assert.Equal("/orders/:id", server.Tag(TrooperAttributes.EntryRoute));
        Assert.Equal(1.0, server.Tag(TrooperAttributes.SampleRate));
    }

    [Fact]
    public async Task Entry_route_is_copied_to_all_child_activities()
    {
        var spans = await TraceOf(() => client.GetAsync("/orders/7"), "/orders");
        var server = spans.Single(a => a.Kind == ActivityKind.Server);
        var children = spans.Where(a => a.Source.Name == "Test.Child").ToList();
        Assert.Equal(2, children.Count);
        Assert.All(children, c =>
        {
            Assert.Equal(server.TraceId, c.TraceId);
            Assert.Equal("/orders/:id", c.Tag(TrooperAttributes.EntryRoute));
            Assert.Null(c.Tag(TrooperAttributes.SampleRate)); // only entry spans carry the sample rate
        });
    }

    [Fact]
    public async Task Attribute_routed_controller_route_is_normalized()
    {
        var spans = await TraceOf(() => client.GetAsync("/api/users/5/orders/abc"), "api/users");
        var server = spans.Single(a => a.Kind == ActivityKind.Server);
        Assert.Equal("/api/users/:userId/orders/:orderId", server.Tag(TrooperAttributes.Route));
        Assert.Equal("/api/users/:userId/orders/:orderId", spans.Single(a => a.DisplayName == "load-orders").Tag(TrooperAttributes.EntryRoute));
    }

    [Fact]
    public async Task Conventional_mvc_route_substitutes_controller_and_action()
    {
        var spans = await TraceOf(() => client.GetAsync("/Reports/Show/3"), "GET");
        var server = spans.Single(a => a.Kind == ActivityKind.Server);
        Assert.Equal("/Reports/Show/:id", server.Tag(TrooperAttributes.Route));
    }

    [Fact]
    public async Task Caller_is_read_from_baggage()
    {
        var request = new HttpRequestMessage(HttpMethod.Get, "/orders/1");
        request.Headers.Add("traceparent", "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01");
        request.Headers.Add("baggage", "tt.svc=checkout-api,foo=bar");
        var spans = await TraceOf(() => client.SendAsync(request), "/orders");
        var server = spans.Single(a => a.Kind == ActivityKind.Server);
        Assert.Equal("checkout-api", server.Tag(TrooperAttributes.Caller));
        Assert.Equal("0af7651916cd43dd8448eb211c80319c", server.TraceId.ToHexString());
        Assert.Equal(1.0, server.Tag(TrooperAttributes.SampleRate));
    }

    [Fact]
    public async Task Exceptions_are_recorded_on_the_server_span()
    {
        var spans = await TraceOf(async () =>
        {
            try { await client.GetAsync("/boom/x"); }
            catch (InvalidOperationException) { } // TestServer rethrows unhandled exceptions
        }, "/boom");
        var server = spans.Single(a => a.Kind == ActivityKind.Server);
        Assert.Equal("/boom/:kind", server.Tag(TrooperAttributes.Route));
        Assert.Equal(ActivityStatusCode.Error, server.Status);
        Assert.Contains(server.Events, e => e.Name == "exception");
    }
}
