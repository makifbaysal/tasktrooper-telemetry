package com.acme.orders;

import org.springframework.amqp.rabbit.annotation.RabbitListener;
import org.springframework.jms.annotation.JmsListener;
import org.springframework.kafka.annotation.KafkaListener;
import org.springframework.scheduling.annotation.Scheduled;
import io.awspring.cloud.sqs.annotation.SqsListener;

@Component
public class OrderEvents {
    private static final String CREATED = "orders.created";

    @KafkaListener(
        topics = {CREATED, "orders.updated"},
        groupId = "order-projector")
    public void project(String event) {
    }

    @RabbitListener(queues = "emails")
    public void email(String msg) {
    }

    @JmsListener(destination = "invoices")
    public void invoice(String msg) {
    }

    @SqsListener("refunds-queue")
    public void refund(String msg) {
    }

    @Scheduled(cron = "0 0 2 * * *")
    public void nightlyReport() {
    }

    @Scheduled(fixedRate = 5000)
    public void heartbeat() {
    }
}
