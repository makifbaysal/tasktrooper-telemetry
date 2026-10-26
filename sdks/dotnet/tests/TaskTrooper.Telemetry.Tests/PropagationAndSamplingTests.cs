using System.Diagnostics;
using Microsoft.Extensions.DependencyInjection;
using OpenTelemetry;
using OpenTelemetry.Trace;

namespace TaskTrooper.Telemetry.Tests;

public class PropagationAndSamplingTests
{
    private static readonly ActivitySource Source = new("Test.Prop");

    [Fact]
    public async Task Outgoing_http_calls_carry_tt_svc_baggage_and_traceparent()
    {
        using var downstream = new CaptureServer();
        var services = new ServiceCollection();
        services.AddTaskTrooper(o => o.ServiceName = "checkout-api").AddSources("Test.Prop");
        await using var sp = services.BuildServiceProvider();
        _ = sp.GetRequiredService<TracerProvider>();

        using var http = new HttpClient();
        using (Source.StartActivity("handler", ActivityKind.Server))
        {
            Baggage.SetBaggage("tt.svc", "upstream-should-be-replaced");
            await http.GetAsync(downstream.BaseUrl + "/inventory/1");
        }

        var req = await downstream.WaitForAsync("/inventory/1");
        Assert.NotNull(req.Header("traceparent"));
        Assert.Equal("checkout-api", TrooperPropagation.CallerFromBaggageHeader(req.Header("baggage")));
    }

    [Fact]
    public void Sampler_marks_entry_spans_with_sample_rate()
    {
        var sampler = new TrooperSampler(0.5);
        var traceId = ActivityTraceId.CreateRandom();

        var root = sampler.ShouldSample(new SamplingParameters(default, traceId, "GET /x", ActivityKind.Server));
        var remoteParent = new ActivityContext(traceId, ActivitySpanId.CreateRandom(), ActivityTraceFlags.Recorded, isRemote: true);
        var localParent = new ActivityContext(traceId, ActivitySpanId.CreateRandom(), ActivityTraceFlags.Recorded, isRemote: false);
        var consumer = sampler.ShouldSample(new SamplingParameters(remoteParent, traceId, "process orders", ActivityKind.Consumer));
        var child = sampler.ShouldSample(new SamplingParameters(localParent, traceId, "db", ActivityKind.Client));

        if (root.Decision == SamplingDecision.RecordAndSample)
            Assert.Contains(root.Attributes, kv => kv.Key == TrooperAttributes.SampleRate && (double)kv.Value == 0.5);
        Assert.Equal(SamplingDecision.RecordAndSample, consumer.Decision);
        Assert.Contains(consumer.Attributes, kv => kv.Key == TrooperAttributes.SampleRate && (double)kv.Value == 0.5);
        Assert.Equal(SamplingDecision.RecordAndSample, child.Decision);
        Assert.DoesNotContain(child.Attributes ?? [], kv => kv.Key == TrooperAttributes.SampleRate);
    }

    [Fact]
    public void Sampler_ratio_is_applied_to_new_traces()
    {
        var sampler = new TrooperSampler(0.1);
        var sampled = Enumerable.Range(0, 5000).Count(_ =>
            sampler.ShouldSample(new SamplingParameters(default, ActivityTraceId.CreateRandom(), "x", ActivityKind.Server)).Decision
            == SamplingDecision.RecordAndSample);
        Assert.InRange(sampled, 300, 700);
        Assert.Equal(0, Enumerable.Range(0, 100).Count(_ =>
            new TrooperSampler(0).ShouldSample(new SamplingParameters(default, ActivityTraceId.CreateRandom(), "x", ActivityKind.Server)).Decision
            == SamplingDecision.RecordAndSample));
    }

    [Fact]
    public void Processor_reads_caller_from_activity_baggage_and_never_throws()
    {
        var exported = new List<Activity>();
        using var provider = Sdk.CreateTracerProviderBuilder()
            .AddSource("Test.Prop")
            .AddProcessor(new TrooperSpanProcessor())
            .AddInMemoryExporter(exported)
            .Build();

        var parent = new ActivityContext(ActivityTraceId.CreateRandom(), ActivitySpanId.CreateRandom(), ActivityTraceFlags.Recorded, isRemote: true);
        var server = Source.CreateActivity("GET /x", ActivityKind.Server, parent)!;
        server.AddBaggage("tt.svc", "web-bff");
        server.Start();
        server.Stop();

        Assert.Equal("web-bff", exported.Single().Tag(TrooperAttributes.Caller));
    }
}
