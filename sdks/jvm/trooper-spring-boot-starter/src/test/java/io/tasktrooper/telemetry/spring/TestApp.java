package io.tasktrooper.telemetry.spring;

import io.opentelemetry.api.OpenTelemetry;
import org.apache.kafka.clients.consumer.ConsumerRecord;
import org.springframework.boot.autoconfigure.SpringBootApplication;
import org.springframework.kafka.annotation.KafkaListener;
import org.springframework.stereotype.Component;
import org.springframework.web.bind.annotation.GetMapping;
import org.springframework.web.bind.annotation.PathVariable;
import org.springframework.web.bind.annotation.RestController;

@SpringBootApplication
public class TestApp {

    @RestController
    static class OrdersController {
        private final OpenTelemetry otel;

        OrdersController(OpenTelemetry otel) {
            this.otel = otel;
        }

        @GetMapping("/v1/orders/{id}")
        String order(@PathVariable String id) {
            otel.getTracer("test").spanBuilder("load-order").startSpan().end();
            return "order " + id;
        }
    }

    @Component
    public static class OrderProjector {
        @KafkaListener(id = "order-projector", topics = "orders.created", autoStartup = "false")
        public void onOrder(ConsumerRecord<String, String> record) {
            // what a retry-topic publish from this listener would write
            lastRetryHeaders = TrooperRetryHeaders.setNextAttempt(new org.apache.kafka.common.header.internals.RecordHeaders());
        }

        public static volatile org.apache.kafka.common.header.Headers lastRetryHeaders;
    }
}
