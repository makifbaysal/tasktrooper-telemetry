package com.acme.orders;

import static org.springframework.web.reactive.function.server.RequestPredicates.GET;
import static org.springframework.web.reactive.function.server.RequestPredicates.POST;
import static org.springframework.web.reactive.function.server.RouterFunctions.route;

import org.springframework.web.reactive.function.server.RouterFunction;

@Configuration
public class RouterConfig {
    @Bean
    public RouterFunction<ServerResponse> routes(PingHandler h) {
        return route(GET("/fn/ping"), h::ping)
            .andRoute(POST("/fn/echo"), h::echo);
    }
}
