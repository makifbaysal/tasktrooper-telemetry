package io.tasktrooper.telemetry.core.profiling;

import java.util.ArrayList;
import java.util.Comparator;
import java.util.HashMap;
import java.util.List;
import java.util.Map;
import java.util.regex.Pattern;

/**
 * One profile of one type in the folded wire format (contracts.md §24):
 *
 * <pre>
 * frame1;frame2;…;frameN value          ← no endpoint
 * # trooper.route=/v1/orders/:id trooper.method=GET
 * frame1;…;frameN value                 ← samples taken while serving GET /v1/orders/:id
 * </pre>
 *
 * Root frame first, leaf last, value in the type's unit. Stacks deeper than {@value #MAX_DEPTH} keep the leaf side and
 * start with {@code [truncated]}; more than {@code maxStacks} distinct stacks fold the rest into {@code [other]}.
 * Not thread-safe.
 */
public final class FoldedProfile {
    public static final int MAX_DEPTH = 128;
    public static final int DEFAULT_MAX_STACKS = 10_000;
    public static final String TRUNCATED = "[truncated]";
    public static final String OTHER = "[other]";

    private static final Pattern LAMBDA = Pattern.compile("\\$\\$Lambda[$/][0-9a-fA-Fx/]+");
    private static final Pattern HIDDEN = Pattern.compile("/0x[0-9a-fA-F]+");
    private static final Pattern PROXY = Pattern.compile("\\$Proxy\\d+");
    private static final Pattern CGLIB = Pattern.compile("\\$\\$(SpringCGLIB|EnhancerBySpringCGLIB|FastClassBySpringCGLIB|EnhancerByCGLIB)\\$\\$[0-9a-fA-F]+");
    private static final Pattern ACCESSOR = Pattern.compile("(GeneratedMethodAccessor|GeneratedConstructorAccessor|GeneratedSerializationConstructorAccessor)\\d+");

    private final ProfileType type;
    private final int maxStacks;
    private final Map<Section, Map<String, long[]>> sections = new HashMap<>();
    private int stacks;
    private long total;

    public FoldedProfile(ProfileType type) {
        this(type, DEFAULT_MAX_STACKS);
    }

    public FoldedProfile(ProfileType type, int maxStacks) {
        this.type = type;
        this.maxStacks = Math.max(1, maxStacks);
    }

    public ProfileType type() {
        return type;
    }

    /** Adds {@code value} to {@code stack} under the endpoint ({@code route} null = no endpoint). Zero / negative values are ignored. */
    public void add(String route, String method, String stack, long value) {
        if (value <= 0 || stack == null || stack.isEmpty()) return;
        Section section = route == null || route.isBlank() ? Section.NONE : new Section(clean(route), method == null ? null : clean(method));
        Map<String, long[]> m = sections.computeIfAbsent(section, k -> new HashMap<>());
        long[] cell = m.get(stack);
        if (cell == null) {
            if (stacks >= maxStacks) {
                stack = OTHER;
                cell = m.get(OTHER);
            }
            if (cell == null) {
                cell = new long[1];
                m.put(stack, cell);
                stacks++;
            }
        }
        cell[0] += value;
        total += value;
    }

    public boolean isEmpty() {
        return total == 0;
    }

    public long total() {
        return total;
    }

    public int stackCount() {
        return stacks;
    }

    /** Folded text; samples without endpoint first, then one {@code # trooper.route=…} section per endpoint. */
    public String render() {
        StringBuilder sb = new StringBuilder(Math.max(256, stacks * 64));
        List<Section> keys = new ArrayList<>(sections.keySet());
        keys.sort(Comparator.comparing((Section s) -> s == Section.NONE ? "" : "\u0001" + s.route)
                .thenComparing(s -> s.method == null ? "" : s.method));
        for (Section s : keys) {
            Map<String, long[]> m = sections.get(s);
            if (m.isEmpty()) continue;
            if (s != Section.NONE) {
                sb.append("# trooper.route=").append(s.route);
                if (s.method != null && !s.method.isEmpty()) sb.append(" trooper.method=").append(s.method);
                sb.append('\n');
            }
            m.entrySet().stream()
                    .sorted(Map.Entry.comparingByKey())
                    .forEach(e -> sb.append(e.getKey()).append(' ').append(e.getValue()[0]).append('\n'));
        }
        return sb.toString();
    }

    /**
     * Joins frames (root first) into a folded stack. Keeps the {@value #MAX_DEPTH} leaf-most frames; when frames were
     * dropped here or by the recorder ({@code truncated}), the root side starts with {@code [truncated]}.
     */
    public static String stack(List<String> rootFirst, boolean truncated) {
        int n = rootFirst.size();
        int from = 0;
        if (n > MAX_DEPTH || (truncated && n >= MAX_DEPTH)) {
            from = n - (MAX_DEPTH - 1);
            truncated = true;
        }
        StringBuilder sb = new StringBuilder(n * 32);
        if (truncated) sb.append(TRUNCATED);
        for (int i = from; i < n; i++) {
            if (sb.length() > 0) sb.append(';');
            sb.append(clean(rootFirst.get(i)));
        }
        return sb.toString();
    }

    /**
     * JVM frame name {@code com.acme.Foo.bar} (no file / line). Generated class names are made stable across restarts:
     * {@code Foo$$Lambda/0x…} → {@code Foo$$Lambda}, {@code $Proxy12} → {@code $Proxy}, CGLIB hashes dropped.
     */
    public static String frame(String className, String methodName) {
        String c = className == null ? "?" : className;
        if (c.indexOf('$') >= 0 || c.indexOf('/') >= 0 || c.startsWith("jdk.internal.reflect.Generated")) {
            c = LAMBDA.matcher(c).replaceAll("\\$\\$Lambda");
            c = HIDDEN.matcher(c).replaceAll("");
            c = PROXY.matcher(c).replaceAll("\\$Proxy");
            c = CGLIB.matcher(c).replaceAll("\\$\\$$1");
            c = ACCESSOR.matcher(c).replaceAll("$1");
        }
        return c + '.' + (methodName == null ? "?" : methodName);
    }

    /** Frame / label text may not contain the separators of the format. */
    static String clean(String s) {
        if (s.indexOf(';') < 0 && s.indexOf('\n') < 0 && s.indexOf('\r') < 0 && s.indexOf(' ') < 0 && s.indexOf('\t') < 0) {
            return s;
        }
        return s.replace(';', ':').replace('\n', ' ').replace('\r', ' ').replace(' ', '_').replace('\t', '_');
    }

    private record Section(String route, String method) {
        static final Section NONE = new Section("", null);
    }
}
