using System.Diagnostics;
using System.Text;
using Confluent.Kafka;
using Headers = Confluent.Kafka.Headers;
using OpenTelemetry;
using OpenTelemetry.Trace;
using RabbitMQ.Client;
using RabbitMQ.Client.Events;
using TaskTrooper.Telemetry.Kafka;
using TaskTrooper.Telemetry.MassTransit;
using TaskTrooper.Telemetry.RabbitMQ;

namespace TaskTrooper.Telemetry.Tests;

public sealed class MessagingTests : IDisposable
{
    private static readonly ActivitySource Child = new("Test.Messaging");
    private readonly List<Activity> exported = new();
    private readonly TracerProvider provider;

    public MessagingTests()
    {
        provider = Sdk.CreateTracerProviderBuilder()
            .AddTaskTrooperKafka()
            .AddSource("Test.Messaging", "MassTransit", TrooperRabbitMQ.SubscriberSource)
            .SetSampler(new TrooperSampler(1.0))
            .AddProcessor(new TrooperSpanProcessor())
            .AddProcessor(new TrooperMassTransitProcessor(new TrooperMassTransitOptions()))
            .AddProcessor(new TrooperRabbitMQProcessor(new TrooperRabbitMQOptions()))
            .AddInMemoryExporter(exported)
            .Build()!;
    }

    public void Dispose() => provider.Dispose();

    private static ConsumeResult<string, string> FakeResult(Headers headers) => new()
    {
        Topic = "orders.created",
        Partition = new Partition(3),
        Offset = new Offset(1234),
        Message = new Message<string, string> { Key = "k1", Value = "{}", Headers = headers },
    };

    [Fact]
    public async Task Kafka_consumer_helper_creates_entry_span_with_trooper_attributes()
    {
        var headers = new Headers
        {
            { "traceparent", Encoding.UTF8.GetBytes("00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01") },
            { "baggage", Encoding.UTF8.GetBytes("tt.svc=orders-api") },
            { TrooperKafka.AttemptHeader, Encoding.UTF8.GetBytes("3") },
        };

        await FakeResult(headers).ProcessAsync("billing-workers", _ =>
        {
            using var db = Child.StartActivity("insert invoice", ActivityKind.Client);
            return Task.CompletedTask;
        });

        var consumer = exported.Single(a => a.Kind == ActivityKind.Consumer);
        Assert.Equal("process orders.created", consumer.DisplayName);
        Assert.Equal("4bf92f3577b34da6a3ce929d0e0e4736", consumer.TraceId.ToHexString());
        Assert.Equal("00f067aa0ba902b7", consumer.ParentSpanId.ToHexString());
        Assert.Equal("kafka", consumer.Tag("messaging.system"));
        Assert.Equal("orders.created", consumer.Tag("messaging.destination.name"));
        Assert.Equal("billing-workers", consumer.Tag("messaging.consumer.group.name"));
        Assert.Equal("3", consumer.Tag("messaging.destination.partition.id"));
        Assert.Equal(1234L, consumer.Tag("messaging.kafka.offset"));
        Assert.Equal("billing-workers", consumer.Tag(TrooperAttributes.Process));
        Assert.Equal("billing-workers", consumer.Tag(TrooperAttributes.EntryRoute));
        Assert.Equal(3, consumer.Tag(TrooperAttributes.ConsumerAttempt));
        Assert.Equal("orders-api", consumer.Tag(TrooperAttributes.Caller));
        Assert.Equal(1.0, consumer.Tag(TrooperAttributes.SampleRate));

        var db = exported.Single(a => a.DisplayName == "insert invoice");
        Assert.Equal(consumer.SpanId, db.ParentSpanId);
        Assert.Equal("billing-workers", db.Tag(TrooperAttributes.EntryRoute));
    }

    [Fact]
    public async Task Kafka_consumer_helper_records_errors_dlq_and_custom_process_name()
    {
        var ex = await Assert.ThrowsAsync<InvalidOperationException>(() =>
            FakeResult(new Headers()).ProcessAsync("g1", _ =>
            {
                TrooperKafka.MarkDeadLettered();
                throw new InvalidOperationException("bad payload");
            }, processName: "invoice-writer", attempt: 2));
        Assert.Equal("bad payload", ex.Message);

        var consumer = exported.Single(a => a.Kind == ActivityKind.Consumer);
        Assert.Equal(default, consumer.ParentSpanId); // no traceparent → new root
        Assert.Equal("invoice-writer", consumer.Tag(TrooperAttributes.Process));
        Assert.Equal("g1", consumer.Tag("messaging.consumer.group.name"));
        Assert.Equal(2, consumer.Tag(TrooperAttributes.ConsumerAttempt));
        Assert.Equal(true, consumer.Tag(TrooperAttributes.ConsumerDlq));
        Assert.Equal(ActivityStatusCode.Error, consumer.Status);
        Assert.Contains(consumer.Events, e => e.Name == "exception");
    }

    [Fact]
    public void Kafka_inject_writes_traceparent_and_caller_baggage()
    {
        TrooperPropagation.ServiceName = "orders-api";
        var headers = new Headers();
        using (var a = Child.StartActivity("send", ActivityKind.Producer))
        {
            TrooperKafka.Inject(headers, a);
            Assert.True(headers.TryGetLastBytes("traceparent", out var tp));
            Assert.Contains(a!.TraceId.ToHexString(), Encoding.UTF8.GetString(tp));
        }

        Assert.True(headers.TryGetLastBytes("baggage", out var bg));
        Assert.Equal("orders-api", TrooperPropagation.CallerFromBaggageHeader(Encoding.UTF8.GetString(bg)));
    }

    [Fact]
    public void MassTransit_receive_span_gets_process_from_queue()
    {
        using var mt = new ActivitySource("MassTransit");
        using (var receive = mt.StartActivity("orders-queue receive", ActivityKind.Consumer, default(ActivityContext),
                   new[] { new KeyValuePair<string, object?>("messaging.masstransit.input_address", "rabbitmq://localhost/orders-queue") }))
        using (mt.StartActivity("orders-queue process", ActivityKind.Consumer))
        {
            Assert.NotNull(receive);
        }

        var receiveSpan = exported.Single(a => a.DisplayName == "orders-queue receive");
        Assert.Equal("orders-queue", receiveSpan.Tag(TrooperAttributes.Process));
        Assert.Equal("orders-queue", exported.Single(a => a.DisplayName == "orders-queue process").Tag(TrooperAttributes.EntryRoute));
        Assert.Null(exported.Single(a => a.DisplayName == "orders-queue process").Tag(TrooperAttributes.Process));
    }

    [Fact]
    public void RabbitMQ_attempt_and_caller_from_delivery_headers()
    {
        var props = new BasicProperties
        {
            Headers = new Dictionary<string, object?>
            {
                ["x-delivery-count"] = 2L,
                ["baggage"] = Encoding.UTF8.GetBytes("tt.svc=orders-api"),
            },
        };
        var delivery = new BasicDeliverEventArgs("ctag", 1, true, "orders", "orders.created", props, ReadOnlyMemory<byte>.Empty);

        using var rmq = new ActivitySource(TrooperRabbitMQ.SubscriberSource);
        using (var a = rmq.StartActivity("orders.created deliver", ActivityKind.Consumer, default(ActivityContext),
                   new[] { new KeyValuePair<string, object?>("messaging.rabbitmq.destination.routing_key", "orders.created") }))
        {
            Assert.Equal("orders.created", a!.Tag(TrooperAttributes.Process)); // default from routing key
            TrooperRabbitMQ.Enrich(delivery, queue: "billing.orders", activity: a);
        }

        var span = exported.Single(a => a.Source.Name == TrooperRabbitMQ.SubscriberSource);
        Assert.Equal("billing.orders", span.Tag(TrooperAttributes.Process));
        Assert.Equal("billing.orders", span.Tag(TrooperAttributes.EntryRoute));
        Assert.Equal(3, span.Tag(TrooperAttributes.ConsumerAttempt));
        Assert.Equal("orders-api", span.Tag(TrooperAttributes.Caller));

        var redelivered = new BasicDeliverEventArgs("c", 1, true, "x", "y", new BasicProperties(), ReadOnlyMemory<byte>.Empty);
        Assert.Equal(2, TrooperRabbitMQ.Attempt(redelivered));
    }
}
