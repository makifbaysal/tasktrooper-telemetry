package io.tasktrooper.telemetry.core.profiling;

import static org.assertj.core.api.Assertions.assertThat;

import io.opentelemetry.api.trace.Span;
import io.opentelemetry.api.trace.SpanKind;
import io.opentelemetry.api.trace.Tracer;
import io.opentelemetry.context.Context;
import io.opentelemetry.context.Scope;
import io.opentelemetry.sdk.trace.SdkTracerProvider;
import io.tasktrooper.telemetry.core.TrooperSpanProcessor;
import java.util.Map;
import java.util.concurrent.atomic.AtomicLong;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;

/** Samples are attributed to the endpoint whose entry span was running on the sampled thread. */
class ProfileRoutesTest {
    private SdkTracerProvider provider;
    private Tracer tracer;

    @BeforeEach
    void setUp() {
        ProfileRoutes.setActive(true);
        provider = SdkTracerProvider.builder().addSpanProcessor(new TrooperSpanProcessor()).build();
        tracer = provider.get("test");
    }

    @AfterEach
    void tearDown() {
        provider.close();
        ProfileRoutes.setActive(false);
    }

    @SuppressWarnings("deprecation")
    private static long tid() {
        return Thread.currentThread().getId();
    }

    @Test
    void samplesInsideAnEntrySpanGetItsRouteSection() {
        SampleAggregator agg = new SampleAggregator(FoldedProfile.DEFAULT_MAX_STACKS);
        long before = ProfileRoutes.nowNanos();

        Span server = tracer.spanBuilder("GET").setSpanKind(SpanKind.SERVER)
                .setAttribute("http.request.method", "GET").startSpan();
        long inside;
        try (Scope ignored = server.makeCurrent()) {
            inside = ProfileRoutes.nowNanos();
            // route known only after routing, as with real frameworks
            server.setAttribute("http.route", "/v1/orders/{id}");
            // still open: resolved from the live span
            assertThat(ProfileRoutes.lookup(tid(), inside).resolve()).containsExactly("/v1/orders/:id", "GET");
        }
        server.end();
        long after = ProfileRoutes.nowNanos() + 1;

        assertThat(agg.add(ProfileType.CPU, tid(), inside, "a.Http.serve;a.Orders.get", 20, false)).isTrue();
        assertThat(agg.add(ProfileType.CPU, tid(), before - 1, "a.Idle.run", 20, false)).isTrue();
        assertThat(agg.add(ProfileType.CPU, tid(), after, "a.Idle.run", 20, false)).isTrue();
        // parks only count while serving a request
        assertThat(agg.add(ProfileType.LOCK, tid(), after, "a.Pool.take;j.Unsafe.park", 5, true)).isFalse();
        assertThat(agg.add(ProfileType.LOCK, tid(), inside, "a.Orders.get;j.Unsafe.park", 5, true)).isTrue();

        Map<ProfileType, FoldedProfile> out = agg.drain();
        assertThat(out.get(ProfileType.CPU).render()).isEqualTo("""
                a.Idle.run 40
                # trooper.route=/v1/orders/:id trooper.method=GET
                a.Http.serve;a.Orders.get 20
                """);
        assertThat(out.get(ProfileType.LOCK).render())
                .isEqualTo("# trooper.route=/v1/orders/:id trooper.method=GET\na.Orders.get;j.Unsafe.park 5\n");
        assertThat(ProfileRoutes.openSpans()).isZero();
        assertThat(agg.drain()).isEmpty();
    }

    @Test
    void childSpanOnAnotherThreadAttributesThatThread() throws Exception {
        Span consumer = tracer.spanBuilder("orders process").setSpanKind(SpanKind.CONSUMER)
                .setAttribute("messaging.consumer.group.name", "billing").startSpan();
        Context ctx = Context.current().with(consumer);
        AtomicLong workerTid = new AtomicLong();
        AtomicLong workerTime = new AtomicLong();
        Thread worker = new Thread(() -> {
            workerTid.set(tid());
            Span child = tracer.spanBuilder("compute").setParent(ctx).startSpan();
            workerTime.set(ProfileRoutes.nowNanos());
            child.end();
        });
        worker.start();
        worker.join();
        consumer.end();

        SampleAggregator agg = new SampleAggregator(100);
        agg.add(ProfileType.CPU, workerTid.get(), workerTime.get(), "w.Worker.compute", 7, false);
        assertThat(agg.drain().get(ProfileType.CPU).render())
                .isEqualTo("# trooper.route=billing trooper.method=CONSUME\nw.Worker.compute 7\n");
    }

    @Test
    void newestIntervalWinsAndInactiveRegistryIsFree() {
        ProfileRoutes.Entry a = new ProfileRoutes.Entry("/a", "GET");
        ProfileRoutes.Timeline tl = new ProfileRoutes.Timeline();
        ProfileRoutes.Interval outer = new ProfileRoutes.Interval(1, 100, a);
        outer.end = 1000;
        ProfileRoutes.Interval inner = new ProfileRoutes.Interval(1, 200, new ProfileRoutes.Entry("/b", "POST"));
        inner.end = 300;
        tl.add(outer);
        tl.add(inner);
        assertThat(tl.find(250).resolve()).containsExactly("/b", "POST");
        assertThat(tl.find(500).resolve()).containsExactly("/a", "GET");
        assertThat(tl.find(2000)).isNull();

        ProfileRoutes.setActive(false);
        Span s = tracer.spanBuilder("GET").setSpanKind(SpanKind.SERVER).startSpan();
        assertThat(ProfileRoutes.openSpans()).isZero();
        s.end();
    }
}
