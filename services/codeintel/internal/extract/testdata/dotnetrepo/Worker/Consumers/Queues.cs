using RabbitMQ.Client;
using Azure.Messaging.ServiceBus;

namespace Worker.Consumers;

public class EmailListener
{
    public void Start(IModel channel)
    {
        channel.QueueDeclare(queue: "emails", durable: true);
        var consumer = new EventingBasicConsumer(channel);
        channel.BasicConsume(queue: "emails", autoAck: false, consumer: consumer);
    }
}

public class ShipmentListener
{
    public async Task Start(ServiceBusClient client)
    {
        var processor = client.CreateProcessor("shipments", "billing", new ServiceBusProcessorOptions());
        var single = client.CreateProcessor("returns");
        await processor.StartProcessingAsync();
    }
}
