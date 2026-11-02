package com.acme.shipping;

import io.micronaut.configuration.kafka.annotation.KafkaListener;
import io.micronaut.configuration.kafka.annotation.Topic;
import io.micronaut.http.annotation.*;

@Controller("/shipments")
public class ShipmentController {
    @Get("/{id}")
    public Shipment get(String id) {
        return null;
    }

    @Post
    public Shipment create(@Body Shipment s) {
        return s;
    }
}

@KafkaListener(groupId = "shipping-tracker")
class TrackingListener {
    @Topic("tracking.events")
    void on(String e) {
    }
}
