package io.tasktrooper.examples.quarkus;

import jakarta.ws.rs.GET;
import jakarta.ws.rs.Path;
import jakarta.ws.rs.PathParam;

@Path("/v1/items")
public class InventoryResource {
    @GET
    @Path("{sku}")
    public String item(@PathParam("sku") String sku) {
        return sku; // server span: trooper.route=/v1/items/:sku
    }
}
