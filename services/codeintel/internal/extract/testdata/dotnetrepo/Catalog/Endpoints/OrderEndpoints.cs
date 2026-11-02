namespace Catalog.Endpoints;

public static class OrderEndpoints
{
    public static RouteGroupBuilder MapOrderEndpoints(this RouteGroupBuilder group)
    {
        group.MapGet("/{id}", (int id) => Results.Ok(id));
        var items = group.MapGroup("/{orderId}/items");
        items.MapPut("/{itemId}", (int orderId, int itemId) => Results.Ok());
        return group;
    }
}
