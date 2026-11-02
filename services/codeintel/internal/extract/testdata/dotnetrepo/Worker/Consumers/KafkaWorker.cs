using Confluent.Kafka;
using Microsoft.Extensions.Hosting;

namespace Worker.Consumers;

public sealed class PaymentsWorker : BackgroundService
{
    protected override Task ExecuteAsync(CancellationToken ct)
    {
        var config = new ConsumerConfig { BootstrapServers = "kafka:9092", GroupId = "billing-workers" };
        using var consumer = new ConsumerBuilder<string, string>(config).Build();
        consumer.Subscribe(new[] { "payments.captured", "payments.failed" });
        while (!ct.IsCancellationRequested)
        {
            var result = consumer.Consume(ct);
        }
        return Task.CompletedTask;
    }
}

public sealed class HeartbeatService : BackgroundService
{
    protected override async Task ExecuteAsync(CancellationToken ct)
    {
        while (!ct.IsCancellationRequested) await Task.Delay(1000, ct);
    }
}

public sealed class SqsPoller(IAmazonSQS sqs) : BackgroundService
{
    protected override async Task ExecuteAsync(CancellationToken ct)
    {
        var req = new ReceiveMessageRequest { QueueUrl = "https://sqs.eu-west-1.amazonaws.com/123456789012/refunds" };
        var res = await sqs.ReceiveMessageAsync(req, ct);
    }
}
