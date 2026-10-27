using System.IO.Compression;
using System.Text;
using System.Web;
using TaskTrooper.Telemetry.Profiling;
using TaskTrooper.Telemetry.Tests;

namespace TaskTrooper.Telemetry.Profiling.Tests;

public class UploaderTests
{
    private static readonly ProfileMetadata Meta = new("orders-api", "prod", "1.2.3", "web-1");

    private static FoldedProfile Profile(ProfileType type = ProfileType.Cpu)
    {
        var p = new FoldedProfile(type);
        p.Add("/v1/orders", "GET", "Main;Handle", 1_000_000);
        return p;
    }

    private static ProfileUploader Uploader(CaptureServer server) =>
        new(TrooperDsn.Parse($"http://secret@127.0.0.1:{server.Port}/7"), Meta, TimeSpan.FromSeconds(5), TimeSpan.FromMilliseconds(200));

    internal static string Gunzip(byte[] body)
    {
        using var gz = new GZipStream(new MemoryStream(body), CompressionMode.Decompress);
        using var r = new StreamReader(gz, Encoding.UTF8);
        return r.ReadToEnd();
    }

    [Fact]
    public async Task Posts_gzipped_folded_profile_with_metadata_query_and_key()
    {
        using var server = new CaptureServer();
        using var uploader = Uploader(server);
        Assert.Equal(ProfileUploader.Result.Sent, uploader.Upload(Profile(ProfileType.AllocSpace), 1_000, 11_000));

        var req = await server.WaitForAsync("/v1/profiles");
        Assert.Equal("POST", req.Method);
        Assert.Equal("secret", req.Header("x-trooper-key"));
        Assert.Equal("gzip", req.Header("content-encoding"));
        Assert.StartsWith("text/plain", req.Header("content-type"));
        var q = HttpUtility.ParseQueryString(req.Query);
        Assert.Equal("folded", q["format"]);
        Assert.Equal("alloc_space", q["type"]);
        Assert.Equal("bytes", q["unit"]);
        Assert.Equal("orders-api", q["service"]);
        Assert.Equal("prod", q["env"]);
        Assert.Equal("1.2.3", q["version"]);
        Assert.Equal("web-1", q["host"]);
        Assert.Equal("dotnet", q["runtime"]);
        Assert.Equal("1000", q["start"]);
        Assert.Equal("11000", q["end"]);
        Assert.Equal("# trooper.route=/v1/orders trooper.method=GET\nMain;Handle 1000000\n", Gunzip(req.Body));
    }

    [Fact]
    public void Url_omits_empty_metadata_and_keeps_base_path()
    {
        var url = ProfileUploader.Url(TrooperDsn.Parse("https://k@ingest.example.com/base/3").SignalEndpoint("v1/profiles"),
            new ProfileMetadata("svc a", null, "", "h"), ProfileType.Lock, 1, 2);
        Assert.Equal("https://ingest.example.com/base/v1/profiles?format=folded&type=lock&unit=nanoseconds&service=svc%20a&host=h&runtime=dotnet&start=1&end=2",
            url.AbsoluteUri);
    }

    [Theory]
    [InlineData(429)]
    [InlineData(503)]
    public void Retryable_status_drops_the_profile_and_backs_off(int status)
    {
        using var server = new CaptureServer(_ => status);
        using var uploader = Uploader(server);
        Assert.Equal(ProfileUploader.Result.Dropped, uploader.Upload(Profile(), 1, 2));
        Assert.True(uploader.BackoffRemaining > TimeSpan.Zero);
        Assert.Equal(ProfileUploader.Result.SkippedBackoff, uploader.Upload(Profile(), 2, 3));
        Assert.Single(server.ForPath("/v1/profiles"));

        Thread.Sleep(uploader.BackoffRemaining + TimeSpan.FromMilliseconds(50));
        Assert.Equal(ProfileUploader.Result.Dropped, uploader.Upload(Profile(), 3, 4));
        Assert.Equal(2, server.ForPath("/v1/profiles").Count());
    }

    [Fact]
    public void Other_4xx_drops_only_that_profile()
    {
        using var server = new CaptureServer(_ => 400);
        using var uploader = Uploader(server);
        Assert.Equal(ProfileUploader.Result.Dropped, uploader.Upload(Profile(), 1, 2));
        Assert.Equal(TimeSpan.Zero, uploader.BackoffRemaining);
        Assert.Equal(ProfileUploader.Result.Dropped, uploader.Upload(Profile(), 2, 3));
        Assert.Equal(2, server.ForPath("/v1/profiles").Count());
    }

    [Fact]
    public void Network_errors_never_throw_and_back_off()
    {
        int port;
        using (var s = new CaptureServer()) port = s.Port; // closed now
        using var uploader = new ProfileUploader(TrooperDsn.Parse($"http://k@127.0.0.1:{port}/1"), Meta, TimeSpan.FromSeconds(2), TimeSpan.FromMilliseconds(100));
        Assert.Equal(ProfileUploader.Result.Dropped, uploader.Upload(Profile(), 1, 2));
        Assert.True(uploader.BackoffRemaining > TimeSpan.Zero);
        Assert.Equal(ProfileUploader.Result.Empty, uploader.Upload(new FoldedProfile(ProfileType.Cpu), 1, 2));
    }
}
