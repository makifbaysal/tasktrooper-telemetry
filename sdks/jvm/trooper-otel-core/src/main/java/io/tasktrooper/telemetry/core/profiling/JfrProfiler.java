package io.tasktrooper.telemetry.core.profiling;

import io.tasktrooper.telemetry.core.SdkErrors;
import java.time.Instant;
import java.util.ArrayList;
import java.util.List;
import java.util.Map;
import java.util.function.Consumer;
import jdk.jfr.FlightRecorder;
import jdk.jfr.consumer.RecordedEvent;
import jdk.jfr.consumer.RecordedFrame;
import jdk.jfr.consumer.RecordedMethod;
import jdk.jfr.consumer.RecordedStackTrace;
import jdk.jfr.consumer.RecordedThread;
import jdk.jfr.consumer.RecordingStream;

/**
 * The only class that touches {@code jdk.jfr} (so runtimes without the module fail in one guarded place). Runs a
 * continuous JFR {@link RecordingStream} with low-overhead settings and hands one window of folded profiles to the
 * sink every period:
 *
 * <ul>
 *   <li>{@code jdk.ExecutionSample} every {@link ProfilerSettings#cpuInterval()} → {@code cpu}, value = period ns</li>
 *   <li>{@code jdk.ObjectAllocationSample} (throttled to 100/s) → {@code alloc_space}, value = weight (bytes)</li>
 *   <li>{@code jdk.JavaMonitorEnter} and {@code jdk.ThreadPark} above {@link ProfilerSettings#lockThreshold()} →
 *       {@code lock}, value = duration ns; parks only count inside requests (idle pool threads park all day)</li>
 * </ul>
 */
final class JfrProfiler {
    private static final String EXECUTION_SAMPLE = "jdk.ExecutionSample";
    private static final String ALLOCATION_SAMPLE = "jdk.ObjectAllocationSample";
    private static final String MONITOR_ENTER = "jdk.JavaMonitorEnter";
    private static final String THREAD_PARK = "jdk.ThreadPark";

    /** One finished period. */
    record Window(long startMs, long endMs, Map<ProfileType, FoldedProfile> profiles) {}

    private final ProfilerSettings settings;
    private final Consumer<Window> sink;
    private final SampleAggregator aggregator = new SampleAggregator(FoldedProfile.DEFAULT_MAX_STACKS);
    private final long cpuValue;
    private volatile RecordingStream stream;
    private volatile boolean closed;
    private long windowStartMs;

    JfrProfiler(ProfilerSettings settings, Consumer<Window> sink) {
        this.settings = settings;
        this.sink = sink;
        this.cpuValue = settings.cpuInterval().toNanos();
    }

    static boolean available() {
        return FlightRecorder.isAvailable();
    }

    /** Blocks the calling (daemon) thread until {@link #close()}. */
    void run() {
        try (RecordingStream rs = new RecordingStream()) {
            stream = rs;
            if (closed) return;
            rs.setOrdered(false);
            rs.setMaxAge(java.time.Duration.ofSeconds(30));
            rs.setMaxSize(32L * 1024 * 1024);
            if (settings.types().contains(ProfileType.CPU)) {
                rs.enable(EXECUTION_SAMPLE).withPeriod(settings.cpuInterval()).withStackTrace();
                rs.onEvent(EXECUTION_SAMPLE, e -> sample(ProfileType.CPU, e, true, cpuValue, false));
            }
            if (settings.types().contains(ProfileType.ALLOC_SPACE)) {
                rs.enable(ALLOCATION_SAMPLE).with("throttle", "100/s").withStackTrace();
                rs.onEvent(ALLOCATION_SAMPLE, e -> sample(ProfileType.ALLOC_SPACE, e, false, e.getLong("weight"), false));
            }
            if (settings.types().contains(ProfileType.LOCK)) {
                rs.enable(MONITOR_ENTER).withThreshold(settings.lockThreshold()).withStackTrace();
                rs.enable(THREAD_PARK).withThreshold(settings.lockThreshold()).withStackTrace();
                rs.onEvent(MONITOR_ENTER, e -> sample(ProfileType.LOCK, e, false, e.getDuration().toNanos(), false));
                rs.onEvent(THREAD_PARK, e -> sample(ProfileType.LOCK, e, false, e.getDuration().toNanos(), true));
            }
            rs.onFlush(this::maybeRotate);
            rs.onError(t -> SdkErrors.record("profiler.jfr", t));
            windowStartMs = System.currentTimeMillis();
            rs.start();
        }
    }

    void close() {
        closed = true;
        RecordingStream rs = stream;
        if (rs != null) {
            try {
                rs.close();
            } catch (Throwable t) {
                SdkErrors.record("profiler.jfr", t);
            }
        }
    }

    private void sample(ProfileType type, RecordedEvent e, boolean sampledThread, long value, boolean onlyInRequest) {
        try {
            RecordedStackTrace st = e.getStackTrace();
            if (st == null || value <= 0) return;
            List<RecordedFrame> frames = st.getFrames(); // leaf first
            List<String> names = new ArrayList<>(frames.size());
            for (int i = frames.size() - 1; i >= 0; i--) {
                RecordedMethod m = frames.get(i).getMethod();
                if (m == null || m.getType() == null) continue;
                names.add(FoldedProfile.frame(m.getType().getName(), m.getName()));
            }
            if (names.isEmpty()) return;
            RecordedThread t = sampledThread ? e.getThread("sampledThread") : e.getThread();
            long tid = t == null ? -1 : t.getJavaThreadId();
            Instant ts = e.getStartTime();
            long epochNanos = ts.getEpochSecond() * 1_000_000_000L + ts.getNano();
            aggregator.add(type, tid, epochNanos, FoldedProfile.stack(names, st.isTruncated()), value, onlyInRequest);
        } catch (Throwable t) {
            SdkErrors.record("profiler.jfr", t);
        }
    }

    private void maybeRotate() {
        try {
            long now = System.currentTimeMillis();
            if (now - windowStartMs < settings.period().toMillis()) return;
            Map<ProfileType, FoldedProfile> profiles = aggregator.drain();
            long start = windowStartMs;
            windowStartMs = now;
            ProfileRoutes.prune();
            if (!profiles.isEmpty()) sink.accept(new Window(start, now, profiles));
        } catch (Throwable t) {
            SdkErrors.record("profiler", t);
        }
    }
}
