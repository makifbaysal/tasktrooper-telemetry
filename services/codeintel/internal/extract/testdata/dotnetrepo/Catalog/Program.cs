using Catalog.Endpoints;
using Microsoft.EntityFrameworkCore;
using StackExchange.Redis;

var builder = WebApplication.CreateBuilder(args);
builder.Services.AddDbContext<CatalogDb>(o => o.UseSqlServer(builder.Configuration.GetConnectionString("db")));
builder.Services.AddSingleton(ConnectionMultiplexer.Connect("localhost"));
var app = builder.Build();

app.MapGet("/healthz", () => "ok");

var api = app.MapGroup("/api");
var v1 = api.MapGroup("/v1")
    .WithTags("v1")
    .RequireAuthorization();

v1.MapGet("/products/{id:int}", GetProduct);
v1.MapPost("/products", async (Product p, CatalogDb db) =>
{
    db.Add(p);
    await db.SaveChangesAsync();
    return Results.Created($"/api/v1/products/{p.Id}", p);
});

app.MapGroup("/admin").RequireAuthorization("admin").MapDelete("/cache/{*key}", (string key) => Results.NoContent());
app.MapMethods("/ping", new[] { "GET", "HEAD" }, () => "pong");
app.MapGroup("/orders").MapOrderEndpoints();
app.MapHealthChecks("/ready");

var verbatim = @"app.MapGet(""/verbatim-string"", () => 1);";
var raw = """
    app.MapGet("/raw-string", () => 2);
    """;
/* app.MapGet("/block-commented", () => 3); */
// app.MapGet("/line-commented", () => 4);
var interp = $"{app.MapGet("/in-interpolation", () => 5)}";

app.Run();

static IResult GetProduct(int id, CatalogDb db) => Results.Ok(db.Find<Product>(id));
