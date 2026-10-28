package io.tasktrooper.telemetry.core.profiling;

import java.util.ArrayList;
import java.util.EnumSet;
import java.util.List;
import java.util.Locale;
import java.util.Set;

/** Profile types the JVM profiler produces (contracts.md §24 type / unit names). */
public enum ProfileType {
    /** {@code jdk.ExecutionSample}; value = samples × sampling period. */
    CPU("cpu", "nanoseconds"),
    /** {@code jdk.ObjectAllocationSample}; value = sample weight (estimated allocated bytes). */
    ALLOC_SPACE("alloc_space", "bytes"),
    /** {@code jdk.JavaMonitorEnter} + {@code jdk.ThreadPark} (inside requests) above the threshold; value = wait time. */
    LOCK("lock", "nanoseconds");

    /** Used when {@code TROOPER_PROFILING_TYPES} is not set. */
    public static final Set<ProfileType> DEFAULTS = EnumSet.allOf(ProfileType.class);

    private final String wireName;
    private final String unit;

    ProfileType(String wireName, String unit) {
        this.wireName = wireName;
        this.unit = unit;
    }

    public String wireName() {
        return wireName;
    }

    public String unit() {
        return unit;
    }

    /**
     * Maps configured type names to the supported subset. Types the JVM profiler can't produce ({@code inuse_space},
     * {@code wall}, {@code goroutines} …) are skipped and returned in {@code unsupported}. Empty input → defaults.
     */
    public static Set<ProfileType> parse(List<String> names, List<String> unsupported) {
        if (names == null || names.isEmpty()) return EnumSet.copyOf(DEFAULTS);
        Set<ProfileType> out = EnumSet.noneOf(ProfileType.class);
        List<String> skipped = unsupported != null ? unsupported : new ArrayList<>();
        for (String n : names) {
            String k = n == null ? "" : n.trim().toLowerCase(Locale.ROOT);
            boolean found = false;
            for (ProfileType t : values()) {
                if (t.wireName.equals(k)) {
                    out.add(t);
                    found = true;
                }
            }
            if (!found && !k.isEmpty()) skipped.add(k);
        }
        return out;
    }
}
