package io.tasktrooper.telemetry.core.profiling;

import io.tasktrooper.telemetry.core.SdkErrors;
import java.time.Instant;
import java.util.ArrayDeque;
import java.util.Iterator;
import java.util.Map;
import java.util.concurrent.ConcurrentHashMap;
import java.util.function.Supplier;

/**
 * Thread → entry route timeline used to attribute profile samples to endpoints (contracts.md §24, folded
 * {@code # trooper.route=… trooper.method=…} sections).
 *
 * <p>The span processor reports entry spans (local-root SERVER / CONSUMER) and child spans that run on another thread
 * than their entry span. Each report opens an interval {@code [start, end]} on the current thread pointing to the
 * request's {@link Entry}; a sample taken on thread {@code T} at time {@code t} belongs to the newest interval of
 * {@code T} that contains {@code t}. The route is resolved lazily (frameworks set {@code http.route} after the server
 * span started) and frozen when the entry span ends.
 *
 * <p>Only active while the profiler runs ({@link #isActive()} is a single volatile read otherwise). Bounded: at most
 * {@value #MAX_PER_THREAD} intervals per thread, ended intervals are kept {@value #RETENTION_SECONDS} s (JFR delivers
 * events about a second late). Never throws.
 */
public final class ProfileRoutes {
    static final int MAX_PER_THREAD = 2048;
    static final int RETENTION_SECONDS = 15;
    private static final long RETENTION_NANOS = RETENTION_SECONDS * 1_000_000_000L;
    private static final int MAX_OPEN = 100_000;

    private static volatile boolean active;
    private static final Map<String, Open> OPEN = new ConcurrentHashMap<>();
    private static final Map<Long, Timeline> TIMELINES = new ConcurrentHashMap<>();

    private ProfileRoutes() {}

    /** True while a profiler is running; span processors skip all bookkeeping otherwise. */
    public static boolean isActive() {
        return active;
    }

    static void setActive(boolean on) {
        active = on;
        if (!on) {
            OPEN.clear();
            TIMELINES.clear();
        }
    }

    /**
     * An entry span (local root SERVER / CONSUMER) started on the current thread.
     *
     * @param spanKey unique span key (trace id + span id)
     * @param resolver returns {@code {route, method}} of the entry span (either may be null)
     */
    public static void entryStarted(String spanKey, Supplier<String[]> resolver) {
        if (!active) return;
        try {
            if (OPEN.size() > MAX_OPEN) OPEN.clear(); // leak guard for spans that are never ended
            Entry entry = new Entry(resolver);
            OPEN.put(spanKey, new Open(entry, open(entry), true));
        } catch (Throwable t) {
            SdkErrors.record("profiler", t);
        }
    }

    /** A child span started; if it runs on another thread than its entry span, that thread works for the entry too. */
    public static void childStarted(String spanKey, String rootKey) {
        if (!active) return;
        try {
            Open root = OPEN.get(rootKey);
            if (root == null) return;
            if (root.interval.threadId == currentThreadId()) return;
            OPEN.put(spanKey, new Open(root.entry, open(root.entry), false));
        } catch (Throwable t) {
            SdkErrors.record("profiler", t);
        }
    }

    /** Called from {@code onEnding}: an entry span's route is final now. */
    public static void spanEnding(String spanKey) {
        if (!active) return;
        try {
            Open o = OPEN.get(spanKey);
            if (o != null && o.root) o.entry.freeze();
        } catch (Throwable t) {
            SdkErrors.record("profiler", t);
        }
    }

    /** Called from {@code onEnd}: closes the span's interval. */
    public static void spanEnded(String spanKey) {
        if (!active) return;
        try {
            Open o = OPEN.remove(spanKey);
            if (o != null) {
                if (o.root) o.entry.freeze();
                o.interval.end = nowNanos();
            }
        } catch (Throwable t) {
            SdkErrors.record("profiler", t);
        }
    }

    /** Entry whose interval on {@code threadId} contains {@code epochNanos}, or null. */
    static Entry lookup(long threadId, long epochNanos) {
        Timeline tl = TIMELINES.get(threadId);
        return tl == null ? null : tl.find(epochNanos);
    }

    /** Drops timelines of threads that have been idle longer than the retention (called once per profile period). */
    static void prune() {
        long cutoff = nowNanos() - RETENTION_NANOS;
        TIMELINES.values().removeIf(tl -> tl.prune(cutoff));
    }

    static int openSpans() {
        return OPEN.size();
    }

    private static Interval open(Entry entry) {
        long tid = currentThreadId();
        Interval interval = new Interval(tid, nowNanos(), entry);
        TIMELINES.computeIfAbsent(tid, k -> new Timeline()).add(interval);
        return interval;
    }

    @SuppressWarnings("deprecation") // Thread.getId(): threadId() needs JDK 19
    private static long currentThreadId() {
        return Thread.currentThread().getId();
    }

    /** Wall clock in epoch nanoseconds (same base as JFR event timestamps). */
    static long nowNanos() {
        Instant now = Instant.now();
        return now.getEpochSecond() * 1_000_000_000L + now.getNano();
    }

    /** The request (entry span) a set of intervals belongs to. */
    public static final class Entry {
        private volatile Supplier<String[]> live;
        private volatile String route;
        private volatile String method;

        Entry(Supplier<String[]> live) {
            this.live = live;
        }

        /** Test / manual constructor with a known route. */
        public Entry(String route, String method) {
            this.route = route;
            this.method = method;
        }

        void freeze() {
            Supplier<String[]> s = live;
            if (s == null) return;
            String[] rm = safe(s);
            route = rm[0];
            method = rm[1];
            live = null; // drop the span reference
        }

        /** {@code {route, method}}; still-open entries are resolved from the live span. */
        String[] resolve() {
            Supplier<String[]> s = live;
            if (s != null) return safe(s);
            return new String[] {route, method};
        }

        private static String[] safe(Supplier<String[]> s) {
            try {
                String[] rm = s.get();
                if (rm != null && rm.length >= 2) return rm;
            } catch (Throwable t) {
                SdkErrors.record("profiler", t);
            }
            return new String[2];
        }
    }

    static final class Interval {
        final long threadId;
        final long start;
        volatile long end = Long.MAX_VALUE;
        final Entry entry;

        Interval(long threadId, long start, Entry entry) {
            this.threadId = threadId;
            this.start = start;
            this.entry = entry;
        }
    }

    /** Intervals of one thread in start order. */
    static final class Timeline {
        private final ArrayDeque<Interval> intervals = new ArrayDeque<>();

        synchronized void add(Interval i) {
            intervals.addLast(i);
            long cutoff = i.start - RETENTION_NANOS;
            while (intervals.size() > MAX_PER_THREAD
                    || (!intervals.isEmpty() && intervals.peekFirst().end < cutoff)) {
                intervals.pollFirst();
            }
        }

        synchronized Entry find(long t) {
            Iterator<Interval> it = intervals.descendingIterator();
            int scanned = 0;
            while (it.hasNext() && scanned++ < MAX_PER_THREAD) {
                Interval i = it.next();
                if (i.start <= t && t <= i.end) return i.entry;
            }
            return null;
        }

        /** Prunes ended intervals before {@code cutoff}; true when the timeline is empty afterwards. */
        synchronized boolean prune(long cutoff) {
            intervals.removeIf(i -> i.end < cutoff);
            return intervals.isEmpty();
        }
    }

    private record Open(Entry entry, Interval interval, boolean root) {}
}
