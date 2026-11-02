using MassTransit;

namespace Worker.Consumers;

public class OrderCreatedConsumer : IConsumer<OrderCreated>, IConsumer<Batch<OrderCancelled>>
{
    public Task Consume(ConsumeContext<OrderCreated> context) => Task.CompletedTask;

    public Task Consume(ConsumeContext<Batch<OrderCancelled>> context) => Task.CompletedTask;
}
