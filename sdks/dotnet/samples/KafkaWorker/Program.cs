using Confluent.Kafka;
using TaskTrooper.Telemetry;
using TaskTrooper.Telemetry.Kafka;

var builder = Host.CreateApplicationBuilder(args);
builder.Services.AddTaskTrooper(o => o.ServiceName = "billing-worker").AddKafka();
builder.Services.AddHostedService<OrdersConsumer>();
builder.Build().Run();

internal sealed class OrdersConsumer(ILogger<OrdersConsumer> log, IConfiguration config) : BackgroundService
{
    private const string GroupId = "billing-workers";
    private const string Topic = "orders.created";

    protected override Task ExecuteAsync(CancellationToken stoppingToken) => Task.Run(async () =>
    {
        var brokers = config["KAFKA_BROKERS"] ?? "localhost:19092";
        using var consumer = new ConsumerBuilder<string, string>(new ConsumerConfig
        {
            BootstrapServers = brokers,
            GroupId = GroupId,
            EnableAutoCommit = false,
            AutoOffsetReset = AutoOffsetReset.Earliest,
        }).Build();
        using var dlq = new ProducerBuilder<string, string>(new ProducerConfig { BootstrapServers = brokers }).Build();
        consumer.Subscribe(Topic);

        while (!stoppingToken.IsCancellationRequested)
        {
            ConsumeResult<string, string> result;
            try { result = consumer.Consume(stoppingToken); }
            catch (OperationCanceledException) { break; }
            catch (ConsumeException e) { log.LogWarning(e, "consume failed"); continue; }

            try
            {
                // consumer entry span: trooper.process=billing-workers, topic/partition/offset, attempt, caller
                await result.ProcessAsync(GroupId, HandleAsync);
            }
            catch (Exception e)
            {
                log.LogError(e, "giving up on offset {Offset}", result.Offset.Value);
                TrooperKafka.MarkDeadLettered();
                await dlq.ProduceTracedAsync(Topic + ".dlq", result.Message, stoppingToken);
            }

            consumer.Commit(result);
        }

        consumer.Close();
    }, stoppingToken);

    private Task HandleAsync(ConsumeResult<string, string> message)
    {
        log.LogInformation("invoicing order {Key}", message.Message.Key); // exported as an OTLP log with trace id
        return Task.CompletedTask;
    }
}
