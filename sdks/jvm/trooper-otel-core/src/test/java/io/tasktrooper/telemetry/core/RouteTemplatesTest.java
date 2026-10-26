package io.tasktrooper.telemetry.core;

import static org.assertj.core.api.Assertions.assertThat;

import org.junit.jupiter.params.ParameterizedTest;
import org.junit.jupiter.params.provider.CsvSource;

/** contracts.md §4 "Route normalizasyonu" — same table for every SDK and the writer. */
class RouteTemplatesTest {
    @ParameterizedTest(name = "{0} -> {1}")
    @CsvSource(delimiter = '|', value = {
        "/users/{id}                       | /users/:id",
        "/users/{id:[0-9]+}                | /users/:id",
        "/users/{id:[0-9]{3}}/x            | /users/:id/x",
        "/users/<id>                       | /users/:id",
        "/users/<int:id>                   | /users/:id",
        "/users/:id                        | /users/:id",
        "/users/{id?}                      | /users/:id",
        "/files/{path...}                  | /files/*path",
        "/files/{*path}                    | /files/*path",
        "/files/*path                      | /files/*path",
        "/static/**                        | /static/**",
        "/exact/{$}                        | /exact",
        "/v1/orders/123                    | /v1/orders/:id",
        "/users/550e8400-e29b-41d4-a716-446655440000 | /users/:id",
        "/objects/5f8d0d55b54764421b7156c9 | /objects/:id",
        "/events/01ARZ3NDEKTSV4RRFFQ69G5FAV | /events/:id",
        "/files/deadbeef                   | /files/deadbeef",
        "/invoices/INV-2024-000123         | /invoices/:id",
        "/products/red-shoe                | /products/red-shoe",
        "/api/v2/feed                      | /api/v2/feed",
        "/a//b/                            | /a/b",
        "/v1/orders/{id}?x=1               | /v1/orders/:id",
        "items/                            | /items",
        "/                                 | /",
    })
    void normalize(String in, String want) {
        assertThat(RouteTemplates.normalize(in)).isEqualTo(want);
    }

    @ParameterizedTest(name = "{0} {1} -> {2}")
    @CsvSource(delimiter = '|', value = {
        "pkg.OrderService | GetOrder                  | /pkg.OrderService/GetOrder",
        "                 | pkg.OrderService/GetOrder | /pkg.OrderService/GetOrder",
        "x                | /pkg.S/M                  | /pkg.S/M",
    })
    void grpc(String service, String method, String want) {
        assertThat(RouteTemplates.grpc(service, method)).isEqualTo(want);
    }
}
