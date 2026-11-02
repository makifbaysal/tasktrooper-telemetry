package com.acme.inventory;

import io.quarkus.scheduler.Scheduled;
import jakarta.ws.rs.*;
import org.eclipse.microprofile.reactive.messaging.Incoming;
import io.quarkus.hibernate.orm.panache.PanacheRepository;

@Path("/stock")
@Produces(MediaType.APPLICATION_JSON)
public class StockResource {
    @GET
    public List<Stock> all() {
        return List.of();
    }

    @GET
    @Path("/{sku}")
    public Stock one(@PathParam("sku") String sku) {
        return null;
    }

    @POST
    @Path("{sku}/reserve")
    public void reserve(@PathParam("sku") String sku) {
    }

    @Path("/sub")
    public SubResource sub() {
        return new SubResource();
    }

    @Incoming("stock-updates")
    public void onUpdate(String msg) {
    }

    @Scheduled(every = "10s")
    void refresh() {
    }

    @Scheduled(cron = "0 15 10 * * ?")
    void cleanup() {
    }
}
