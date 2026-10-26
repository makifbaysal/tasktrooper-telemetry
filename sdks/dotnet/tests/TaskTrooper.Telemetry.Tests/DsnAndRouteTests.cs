namespace TaskTrooper.Telemetry.Tests;

public class DsnAndRouteTests
{
    [Theory]
    [InlineData("https://abc123@ingest.trooper.dev/42", "https://ingest.trooper.dev/", "abc123", "42")]
    [InlineData("http://tt_dev_ingest_key@localhost:4318/1", "http://localhost:4318/", "tt_dev_ingest_key", "1")]
    [InlineData("https://k@example.com:8443/otel/ingest/7", "https://example.com:8443/otel/ingest/", "k", "7")]
    [InlineData("  https://k%40x@example.com/9  ", "https://example.com/", "k@x", "9")]
    public void Parses_valid_dsn(string dsn, string endpoint, string key, string project)
    {
        Assert.True(TrooperDsn.TryParse(dsn, out var d));
        Assert.Equal(endpoint, d.Endpoint.AbsoluteUri);
        Assert.Equal(key, d.Key);
        Assert.Equal(project, d.ProjectId);
    }

    [Theory]
    [InlineData(null)]
    [InlineData("")]
    [InlineData("not a url")]
    [InlineData("https://ingest.trooper.dev/1")] // no key
    [InlineData("https://key@ingest.trooper.dev")] // no project
    [InlineData("ftp://key@ingest.trooper.dev/1")]
    public void Rejects_invalid_dsn(string? dsn)
    {
        Assert.False(TrooperDsn.TryParse(dsn, out _));
        if (dsn is not null) Assert.Throws<FormatException>(() => TrooperDsn.Parse(dsn));
    }

    [Fact]
    public void Builds_signal_endpoints()
    {
        var d = TrooperDsn.Parse("https://k@example.com:8443/base/7");
        Assert.Equal("https://example.com:8443/base/v1/traces", d.SignalEndpoint("v1/traces").AbsoluteUri);
        Assert.Equal("https://example.com:8443/base/v1/logs", d.SignalEndpoint("/v1/logs").AbsoluteUri);
    }

    [Theory]
    [InlineData("api/orders/{id}", "/api/orders/:id")]
    [InlineData("/api/orders/{id:int}", "/api/orders/:id")]
    [InlineData("/users/{userId:guid}/items/{itemId?}", "/users/:userId/items/:itemId")]
    [InlineData("{controller=Home}/{action=Index}/{id?}", "/:controller/:action/:id")]
    [InlineData("/files/{**path}", "/files/*path")]
    [InlineData("/files/{*path:nonfile}", "/files/*path")]
    [InlineData("/v1/{lang:regex(^[a-z]+$)}/x", "/v1/:lang/x")]
    [InlineData("", "/")]
    [InlineData("/health/", "/health")]
    public void Normalizes_route_templates(string template, string expected) =>
        Assert.Equal(expected, TrooperRoute.Normalize(template));

    [Theory]
    [InlineData("tt.svc=checkout-api", "checkout-api")]
    [InlineData("userId=1, tt.svc = orders%20api ;prop=1,other=x", "orders api")]
    [InlineData("other=x", null)]
    [InlineData("tt.svc=", null)]
    [InlineData(null, null)]
    public void Reads_caller_from_baggage_header(string? header, string? expected) =>
        Assert.Equal(expected, TrooperPropagation.CallerFromBaggageHeader(header));
}
