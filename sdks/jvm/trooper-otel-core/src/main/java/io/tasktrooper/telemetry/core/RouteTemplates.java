package io.tasktrooper.telemetry.core;

/**
 * Route normalization shared by all TaskTrooper SDKs and the writer (contracts.md §4, "Route normalizasyonu"):
 *
 * <ul>
 *   <li>path parameters {@code {id}}, {@code {id:regex}}, {@code {id?}}, {@code <id>}, {@code <int:id>}, {@code :id}
 *       → {@code :id}</li>
 *   <li>catch-alls {@code {p...}}, {@code {*p}}, {@code *p} → {@code *p}</li>
 *   <li>raw id-like segments (numbers, UUIDs, hex, ULIDs, long opaque tokens) → {@code :id}</li>
 *   <li>trailing slash and empty segments dropped; gRPC routes are the full method name ({@link #grpc})</li>
 * </ul>
 */
public final class RouteTemplates {
    private RouteTemplates() {}

    public static String normalize(String template) {
        if (template == null) {
            return null;
        }
        String t = template.trim();
        int q = indexOfAny(t, '?', '#');
        if (q >= 0 && !insideBraces(t, q)) {
            t = t.substring(0, q);
        }
        if (t.isEmpty()) {
            return null;
        }
        StringBuilder out = new StringBuilder(t.length());
        int i = 0;
        int n = t.length();
        while (i < n) {
            // one segment per iteration
            while (i < n && t.charAt(i) == '/') i++;
            if (i >= n) break;
            int end = segmentEnd(t, i);
            String seg = normalizeSegment(t.substring(i, end));
            if (!seg.isEmpty()) {
                out.append('/').append(seg);
            }
            i = end;
        }
        return out.length() == 0 ? "/" : out.toString();
    }

    /** gRPC route = full method name: {@code /pkg.Service/Method}. Accepts {@code (service, method)} or a full name. */
    public static String grpc(String service, String method) {
        if (method == null || method.isBlank()) {
            return null;
        }
        String m = method.trim();
        if (m.indexOf('/') >= 0) {
            return m.startsWith("/") ? m : "/" + m;
        }
        if (service == null || service.isBlank()) {
            return null;
        }
        String s = service.trim();
        return (s.startsWith("/") ? "" : "/") + s + "/" + m;
    }

    /** Joins a class-level and method-level path (JAX-RS style) and normalizes the result. */
    public static String join(String base, String sub) {
        String a = base == null ? "" : base.trim();
        String b = sub == null ? "" : sub.trim();
        if (a.endsWith("/")) a = a.substring(0, a.length() - 1);
        if (!b.isEmpty() && !b.startsWith("/")) b = "/" + b;
        String joined = a + b;
        return normalize(joined.isEmpty() ? "/" : joined);
    }

    /** End of the segment starting at {@code start}: next '/' outside braces / angle brackets. */
    private static int segmentEnd(String t, int start) {
        int depth = 0;
        boolean angle = false;
        for (int j = start; j < t.length(); j++) {
            char c = t.charAt(j);
            if (c == '{') depth++;
            else if (c == '}' && depth > 0) depth--;
            else if (c == '<' && depth == 0) angle = true;
            else if (c == '>' && depth == 0) angle = false;
            else if (c == '/' && depth == 0 && !angle) return j;
        }
        return t.length();
    }

    static String normalizeSegment(String seg) {
        if (seg.indexOf('{') >= 0 || seg.indexOf('<') >= 0) {
            return replaceParams(seg);
        }
        if (seg.startsWith(":")) {
            String name = stripOptional(seg.substring(1));
            return ":" + (name.isEmpty() ? "param" : name);
        }
        if (seg.startsWith("*") && seg.length() > 1 && !seg.startsWith("**")) {
            return seg; // named catch-all *p
        }
        return isId(seg) ? ":id" : seg;
    }

    private static String replaceParams(String seg) {
        StringBuilder out = new StringBuilder(seg.length());
        int i = 0;
        while (i < seg.length()) {
            char c = seg.charAt(i);
            if (c == '{' || c == '<') {
                char close = c == '{' ? '}' : '>';
                int depth = 1;
                int j = i + 1;
                while (j < seg.length() && depth > 0) {
                    char d = seg.charAt(j);
                    if (d == c) depth++;
                    else if (d == close) depth--;
                    j++;
                }
                if (depth != 0) { // unbalanced: keep as is
                    out.append(seg, i, seg.length());
                    break;
                }
                String inner = seg.substring(i + 1, j - 1).trim();
                out.append(param(inner, c == '<'));
                i = j;
            } else {
                out.append(c);
                i++;
            }
        }
        return out.toString();
    }

    /** {@code id}, {@code id:regex}, {@code p...}, {@code *p}, {@code $} (brace) or {@code int:id} (angle). */
    private static String param(String inner, boolean angle) {
        String name;
        if (angle) {
            int colon = inner.lastIndexOf(':'); // Flask/Werkzeug: <converter:name>
            name = colon >= 0 ? inner.substring(colon + 1) : inner;
        } else {
            int colon = inner.indexOf(':'); // {name:regex}
            name = colon >= 0 ? inner.substring(0, colon) : inner;
        }
        name = name.trim();
        if (name.equals("$")) {
            return ""; // net/http end anchor
        }
        boolean catchAll = false;
        if (name.endsWith("...")) {
            catchAll = true;
            name = name.substring(0, name.length() - 3);
        }
        if (name.startsWith("**")) {
            catchAll = true;
            name = name.substring(2);
        } else if (name.startsWith("*")) {
            catchAll = true;
            name = name.substring(1);
        }
        name = stripOptional(name);
        if (catchAll) {
            return "*" + name;
        }
        return ":" + (name.isEmpty() ? "param" : name);
    }

    private static String stripOptional(String name) {
        String s = name.trim();
        while (s.endsWith("?") || s.endsWith("+")) s = s.substring(0, s.length() - 1);
        return s;
    }

    private static int indexOfAny(String s, char a, char b) {
        for (int i = 0; i < s.length(); i++) {
            char c = s.charAt(i);
            if (c == a || c == b) return i;
        }
        return -1;
    }

    private static boolean insideBraces(String s, int pos) {
        int depth = 0;
        for (int i = 0; i < pos; i++) {
            char c = s.charAt(i);
            if (c == '{') depth++;
            else if (c == '}' && depth > 0) depth--;
        }
        return depth > 0;
    }

    /** Same rules as the writer ({@code services/ingest/internal/norm.IsID}). */
    static boolean isId(String s) {
        int n = s.length();
        if (n == 0) {
            return false;
        }
        int digits = 0, hexLetters = 0, letters = 0, other = 0;
        for (int i = 0; i < n; i++) {
            char c = s.charAt(i);
            if (c >= '0' && c <= '9') digits++;
            else if ((c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) hexLetters++;
            else if ((c >= 'g' && c <= 'z') || (c >= 'G' && c <= 'Z')) letters++;
            else other++;
        }
        if (digits == n) return true;
        if (n == 36 && isUuid(s)) return true;
        if (letters == 0 && other == 0 && n >= 8 && digits > 0) return true;
        if (n == 26 && other == 0 && digits > 0 && isUlid(s)) return true;
        if (n >= 20 && digits >= 4 && digits + hexLetters + letters >= n - 2) return true;
        return digits >= 6 && digits * 2 >= n;
    }

    private static boolean isUuid(String s) {
        for (int i = 0; i < 36; i++) {
            char c = s.charAt(i);
            if (i == 8 || i == 13 || i == 18 || i == 23) {
                if (c != '-') return false;
            } else if (!((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F'))) {
                return false;
            }
        }
        return true;
    }

    private static boolean isUlid(String s) {
        for (int i = 0; i < s.length(); i++) {
            char c = Character.toUpperCase(s.charAt(i));
            boolean ok = (c >= '0' && c <= '9') || (c >= 'A' && c <= 'Z' && c != 'I' && c != 'L' && c != 'O' && c != 'U');
            if (!ok) return false;
        }
        return true;
    }
}
