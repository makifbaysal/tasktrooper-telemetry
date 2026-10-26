package io.tasktrooper.telemetry.quarkus;

import static org.assertj.core.api.Assertions.assertThat;

import io.opentelemetry.context.Context;
import io.smallrye.config.PropertiesConfigSource;
import io.smallrye.config.SmallRyeConfig;
import io.smallrye.config.SmallRyeConfigBuilder;
import jakarta.ws.rs.GET;
import jakarta.ws.rs.Path;
import java.util.HashMap;
import java.util.Map;
import org.junit.jupiter.api.Test;

class QuarkusMappingTest {

    private static SmallRyeConfig config(Map<String, String> userProps) {
        return new SmallRyeConfigBuilder()
                .withSources(new PropertiesConfigSource(userProps, "application.properties", 250))
                .withSources(new TrooperConfigSourceFactory())
                .build();
    }

    @Test
    void mapsDsnToQuarkusOtel() {
        SmallRyeConfig c = config(Map.of(
                "trooper.dsn", "http://tt_dev_ingest_key@localhost:4318/1",
                "quarkus.application.name", "inventory",
                "trooper.environment", "dev",
                "trooper.sample-rate", "0.5"));
        assertThat(c.getRawValue("quarkus.otel.exporter.otlp.endpoint")).isEqualTo("http://localhost:4318");
        assertThat(c.getRawValue("quarkus.otel.exporter.otlp.protocol")).isEqualTo("http/protobuf");
        assertThat(c.getRawValue("quarkus.otel.exporter.otlp.headers")).isEqualTo("x-trooper-key=tt_dev_ingest_key");
        assertThat(c.getRawValue("quarkus.otel.service.name")).isEqualTo("inventory");
        assertThat(c.getRawValue("quarkus.otel.resource.attributes")).isEqualTo("deployment.environment.name=dev");
        assertThat(c.getRawValue("quarkus.otel.traces.sampler.arg")).isEqualTo("0.5");
        assertThat(c.getRawValue("quarkus.otel.propagators")).isEqualTo("tracecontext,trooper");
    }

    @Test
    void explicitQuarkusSettingsWin() {
        Map<String, String> props = new HashMap<>();
        props.put("trooper.dsn", "http://k@localhost:4318/1");
        props.put("quarkus.otel.exporter.otlp.endpoint", "http://collector:4318");
        SmallRyeConfig c = config(props);
        assertThat(c.getRawValue("quarkus.otel.exporter.otlp.endpoint")).isEqualTo("http://collector:4318");
        assertThat(c.getRawValue("quarkus.otel.exporter.otlp.headers")).isEqualTo("x-trooper-key=k");
    }

    @Test
    void producesSamplerFromConfig() {
        SmallRyeConfig c = config(Map.of("trooper.sample-rate", "0.25"));
        assertThat(new TrooperOtelProducers().trooperSampler(c).getDescription()).contains("rate=0.25");
        assertThat(new TrooperOtelProducers().trooperSpanProcessor()).isNotNull();
    }

    @Test
    void propagatorAddsServiceBaggage() {
        Map<String, String> headers = new HashMap<>();
        io.tasktrooper.telemetry.core.TrooperBaggagePropagator.create("inventory").inject(Context.root(), headers, Map::put);
        assertThat(headers).containsEntry("baggage", "tt.svc=inventory");
        assertThat(new TrooperPropagatorProvider().getName()).isEqualTo("trooper");
    }

    @Path("/v1/orders")
    static class OrdersResource {
        @GET
        @Path("{id}")
        public String get() { return ""; }

        @GET
        public String list() { return ""; }
    }

    @Test
    void jaxRsRouteTemplate() throws Exception {
        assertThat(TrooperRouteFilter.template("/", OrdersResource.class, OrdersResource.class.getMethod("get"))).isEqualTo("/v1/orders/:id");
        assertThat(TrooperRouteFilter.template("/api/", OrdersResource.class, OrdersResource.class.getMethod("get"))).isEqualTo("/api/v1/orders/:id");
        assertThat(TrooperRouteFilter.template(null, OrdersResource.class, OrdersResource.class.getMethod("list"))).isEqualTo("/v1/orders");
    }
}
