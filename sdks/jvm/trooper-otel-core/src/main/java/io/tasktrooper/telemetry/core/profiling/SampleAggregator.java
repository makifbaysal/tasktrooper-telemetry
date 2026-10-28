package io.tasktrooper.telemetry.core.profiling;

import java.util.EnumMap;
import java.util.HashMap;
import java.util.IdentityHashMap;
import java.util.Map;

/**
 * Collects samples of one profile period and attributes them to endpoints via {@link ProfileRoutes}. Routes are
 * resolved only when the period is drained, so requests whose route became known late are still labelled.
 * Not thread-safe (fed by the single JFR stream thread).
 */
final class SampleAggregator {
    private static final Object NO_ENTRY = new Object();

    private final int maxStacks;
    private final Map<ProfileType, Map<Object, Map<String, long[]>>> byType = new EnumMap<>(ProfileType.class);
    private final Map<ProfileType, int[]> distinct = new EnumMap<>(ProfileType.class);

    SampleAggregator(int maxStacks) {
        this.maxStacks = maxStacks;
    }

    /**
     * Adds a sample taken on {@code threadId} at {@code epochNanos}.
     *
     * @param onlyInRequest drop the sample unless the thread was serving a request (used for {@code jdk.ThreadPark},
     *     where idle pool threads would otherwise dominate)
     * @return false when the sample was dropped
     */
    boolean add(ProfileType type, long threadId, long epochNanos, String stack, long value, boolean onlyInRequest) {
        if (value <= 0 || stack == null) return false;
        ProfileRoutes.Entry entry = threadId >= 0 ? ProfileRoutes.lookup(threadId, epochNanos) : null;
        if (onlyInRequest && entry == null) return false;
        Map<String, long[]> stacks = byType
                .computeIfAbsent(type, k -> new IdentityHashMap<>())
                .computeIfAbsent(entry == null ? NO_ENTRY : entry, k -> new HashMap<>());
        long[] cell = stacks.get(stack);
        if (cell == null) {
            int[] n = distinct.computeIfAbsent(type, k -> new int[1]);
            if (n[0] >= maxStacks) {
                stack = FoldedProfile.OTHER;
                cell = stacks.get(stack);
            }
            if (cell == null) {
                cell = new long[1];
                stacks.put(stack, cell);
                n[0]++;
            }
        }
        cell[0] += value;
        return true;
    }

    /** Builds one folded profile per type and resets the aggregator. */
    Map<ProfileType, FoldedProfile> drain() {
        Map<ProfileType, FoldedProfile> out = new EnumMap<>(ProfileType.class);
        for (Map.Entry<ProfileType, Map<Object, Map<String, long[]>>> t : byType.entrySet()) {
            FoldedProfile p = new FoldedProfile(t.getKey(), maxStacks);
            for (Map.Entry<Object, Map<String, long[]>> e : t.getValue().entrySet()) {
                String route = null;
                String method = null;
                if (e.getKey() instanceof ProfileRoutes.Entry entry) {
                    String[] rm = entry.resolve();
                    route = rm[0];
                    method = rm[1];
                }
                for (Map.Entry<String, long[]> s : e.getValue().entrySet()) {
                    p.add(route, method, s.getKey(), s.getValue()[0]);
                }
            }
            if (!p.isEmpty()) out.put(t.getKey(), p);
        }
        byType.clear();
        distinct.clear();
        return out;
    }
}
