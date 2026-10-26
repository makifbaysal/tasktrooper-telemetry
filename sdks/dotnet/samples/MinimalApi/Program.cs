using TaskTrooper.Telemetry;

var builder = WebApplication.CreateBuilder(args);

// DSN from TROOPER_DSN or appsettings "TaskTrooper:Dsn"; local stack: http://tt_dev_ingest_key@localhost:4318/1
builder.Services
    .AddTaskTrooper(o => o.ServiceName = "orders-api")
    .AddAspNetCore();

builder.Services.AddHttpClient("inventory", c => c.BaseAddress = new Uri("http://localhost:5081"));

var app = builder.Build();

app.MapGet("/v1/orders/{id:int}", async (int id, IHttpClientFactory http, ILogger<Program> log) =>
{
    log.LogInformation("loading order {OrderId}", id);
    // outgoing call carries traceparent + baggage tt.svc=orders-api → callee records trooper.caller
    try { await http.CreateClient("inventory").GetAsync($"/v1/stock/{id}"); } catch (HttpRequestException) { }
    return Results.Ok(new { id, status = "shipped" });
});

app.MapPost("/v1/orders", () => Results.Created("/v1/orders/1", new { id = 1 }));
app.MapGet("/v1/fail", () => { throw new InvalidOperationException("demo failure"); });

app.Run();
