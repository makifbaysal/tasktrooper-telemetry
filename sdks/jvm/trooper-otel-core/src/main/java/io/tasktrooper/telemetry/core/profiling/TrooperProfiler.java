package io.tasktrooper.telemetry.core.profiling;

import io.tasktrooper.telemetry.core.SdkErrors;
import java.util.Map;
import java.util.concurrent.ArrayBlockingQueue;
import java.util.concurrent.ThreadPoolExecutor;
import java.util.concurrent.TimeUnit;
import java.util.function.BooleanSupplier;
import java.util.function.Consumer;
import java.util.logging.Level;
import java.util.logging.Logger;

/**
 * Continuous profiler for the JVM (ADR-022, contracts.md §24): JFR, no native agent, JDK 17+. Off by default; enabled
 * with {@code TROOPER_PROFILING=true} / {@code -Dtrooper.profiling.enabled=true} (javaagent, {@code TrooperSdk}) or
 * {@code trooper.profiling.enabled: true} (Spring Boot starter) — the SDK calls {@link #start} once the OTel resource
 * is known.
 *
 * <p>One profiler per JVM. JFR runs on a daemon thread; uploads run on another daemon thread. When JFR is not
 * available (JRE without {@code jdk.jfr}, GraalVM native image, {@code -XX:-FlightRecorder} …) {@link #start} logs once
 * and returns {@code false}. Never throws.
 */
public final class TrooperProfiler {
    private static final Logger LOG = Logger.getLogger(TrooperProfiler.class.getName());
    private static final Object LOCK = new Object();
    private static Session session;

    /** Test hook: JFR availability check. */
    static volatile BooleanSupplier jfrCheck = TrooperProfiler::defaultJfrCheck;

    private TrooperProfiler() {}

    /** Starts the profiler (no-op if already running). @return whether a profiler is running afterwards */
    public static boolean start(ProfilerSettings settings) {
        try {
            synchronized (LOCK) {
                if (session != null) return true;
                if (settings == null || settings.endpoint() == null || settings.endpoint().isBlank()) {
                    LOG.log(Level.INFO, "TaskTrooper: profiling enabled but no ingest endpoint (TROOPER_DSN); profiler not started");
                    return false;
                }
                if (!jfrCheck.getAsBoolean()) {
                    LOG.log(Level.INFO, "TaskTrooper: profiling enabled but Java Flight Recorder is not available on this runtime; profiler not started");
                    return false;
                }
                session = new Session(settings);
                session.start();
                LOG.log(Level.FINE, "TaskTrooper: profiler starting " + settings);
                return true;
            }
        } catch (Throwable t) {
            SdkErrors.record("profiler", t);
            LOG.log(Level.FINE, "TaskTrooper: profiler failed to start", t);
            return false;
        }
    }

    /** Stops the profiler and the route bookkeeping. Never throws. */
    public static void stop() {
        try {
            synchronized (LOCK) {
                if (session != null) {
                    session.stop();
                    session = null;
                }
            }
        } catch (Throwable t) {
            SdkErrors.record("profiler", t);
        }
    }

    public static boolean isRunning() {
        synchronized (LOCK) {
            return session != null;
        }
    }

    /** Whether Java Flight Recorder can be used on this runtime. */
    public static boolean jfrAvailable() {
        try {
            return jfrCheck.getAsBoolean();
        } catch (Throwable t) {
            return false;
        }
    }

    private static boolean defaultJfrCheck() {
        try {
            Class.forName("jdk.jfr.FlightRecorder", false, TrooperProfiler.class.getClassLoader());
            return JfrProfiler.available();
        } catch (Throwable t) { // ClassNotFoundException, NoClassDefFoundError, UnsupportedOperationException …
            return false;
        }
    }

    private static final class Session {
        private final ProfilerSettings settings;
        private final ProfileUploader uploader;
        private final ThreadPoolExecutor uploads;
        private volatile JfrProfiler jfr;
        private volatile boolean stopped;
        private Thread thread;

        Session(ProfilerSettings settings) {
            this.settings = settings;
            this.uploader = new ProfileUploader(settings);
            // at most a few windows waiting; the oldest is dropped (profiles are not queued, contracts.md §24)
            this.uploads = new ThreadPoolExecutor(1, 1, 30, TimeUnit.SECONDS, new ArrayBlockingQueue<>(4),
                    r -> daemon(r, "trooper-profiler-upload"), new ThreadPoolExecutor.DiscardOldestPolicy());
            this.uploads.allowCoreThreadTimeOut(true);
        }

        void start() {
            thread = daemon(this::run, "trooper-profiler");
            thread.start();
        }

        private void run() {
            try {
                long delay = settings.startDelay().toMillis();
                if (delay > 0) Thread.sleep(delay);
                if (stopped) return;
                Consumer<JfrProfiler.Window> sink = w -> uploads.execute(() -> upload(w));
                JfrProfiler p = new JfrProfiler(settings, sink);
                jfr = p;
                ProfileRoutes.setActive(true);
                if (stopped) return;
                p.run();
            } catch (InterruptedException e) {
                Thread.currentThread().interrupt();
            } catch (Throwable t) {
                SdkErrors.record("profiler.jfr", t);
                LOG.log(Level.INFO, "TaskTrooper: profiler stopped: " + t);
            } finally {
                ProfileRoutes.setActive(false);
            }
        }

        private void upload(JfrProfiler.Window w) {
            for (Map.Entry<ProfileType, FoldedProfile> e : w.profiles().entrySet()) {
                uploader.upload(e.getValue(), w.startMs(), w.endMs());
            }
        }

        void stop() {
            stopped = true;
            JfrProfiler p = jfr;
            if (p != null) p.close();
            if (thread != null) thread.interrupt();
            uploads.shutdown();
            ProfileRoutes.setActive(false);
        }

        private static Thread daemon(Runnable r, String name) {
            Thread t = new Thread(r, name);
            t.setDaemon(true);
            return t;
        }
    }
}
