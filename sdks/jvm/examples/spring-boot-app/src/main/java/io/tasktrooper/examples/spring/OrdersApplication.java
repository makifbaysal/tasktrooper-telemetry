package io.tasktrooper.examples.spring;

import org.springframework.boot.SpringApplication;
import org.springframework.boot.autoconfigure.SpringBootApplication;
import org.springframework.kafka.annotation.KafkaListener;
import org.springframework.stereotype.Component;
import org.springframework.web.bind.annotation.GetMapping;
import org.springframework.web.bind.annotation.PathVariable;
import org.springframework.web.bind.annotation.RestController;

@SpringBootApplication
public class OrdersApplication {
    public static void main(String[] args) {
        SpringApplication.run(OrdersApplication.class, args);
    }

    @RestController
    static class OrdersController {
        @GetMapping("/v1/orders/{id}")
        String order(@PathVariable String id) {
            return "{\"id\":\"" + id + "\"}"; // span: trooper.route=/v1/orders/:id
        }
    }

    /** Consumer span gets trooper.process=order-projector (+ trooper.consumer.attempt on redelivery). */
    @Component
    static class OrderProjector {
        @KafkaListener(id = "order-projector", topics = "orders.created", autoStartup = "${app.kafka.enabled:false}")
        void onOrderCreated(String payload) {
            // project the order
        }
    }
}
