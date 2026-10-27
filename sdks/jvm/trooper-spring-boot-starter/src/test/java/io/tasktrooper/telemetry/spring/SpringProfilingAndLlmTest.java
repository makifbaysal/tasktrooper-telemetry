package io.tasktrooper.telemetry.spring;

import static org.assertj.core.api.Assertions.assertThat;

import io.opentelemetry.api.OpenTelemetry;
import io.tasktrooper.telemetry.core.profiling.TrooperProfiler;
import io.tasktrooper.telemetry.llm.TrooperLlm;
import org.junit.jupiter.api.AfterAll;
import org.junit.jupiter.api.Test;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.boot.test.context.SpringBootTest;
import org.springframework.core.env.Environment;

/** {@code trooper.profiling.enabled} / {@code trooper.llm.capture-content} in application config. */
@SpringBootTest(classes = TestApp.class, properties = {
        "trooper.dsn=http://tt_dev_ingest_key@127.0.0.1:1/1",
        "trooper.service=profiled-api",
        "trooper.profiling.enabled=true",
        "trooper.llm.capture-content=true",
        "otel.traces.exporter=none",
        "otel.metrics.exporter=none",
        "otel.logs.exporter=none",
        "spring.kafka.bootstrap-servers=localhost:1",
})
class SpringProfilingAndLlmTest {
    @Autowired Environment environment;
    @Autowired OpenTelemetry otel;

    @AfterAll
    static void cleanUp() {
        TrooperProfiler.stop();
        TrooperLlm.setCaptureContent(null);
    }

    @Test
    void profilerStartsAndContentCaptureIsConfigured() {
        assertThat(otel).isNotNull();
        assertThat(TrooperProfiler.isRunning()).as("JFR profiler session").isTrue();
        assertThat(TrooperLlm.captureContent()).isTrue();
        assertThat(environment.getProperty("otel.instrumentation.genai.capture-message-content")).isEqualTo("true");
    }
}
