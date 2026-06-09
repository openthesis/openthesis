package io.openthesis;

import java.util.Collections;
import java.util.HashSet;
import java.util.Map;
import java.util.Set;
import java.util.concurrent.ConcurrentHashMap;

public class Assert {
    private static final Set<String> declared = Collections.synchronizedSet(new HashSet<>());
    private static final Map<String, Boolean> everSinceArmed = new ConcurrentHashMap<>();

    public static void always(boolean condition, String message, Map<String, Object> details) {
        emit(condition, message, "always", true, details, 2);
    }

    public static void alwaysOrUnreachable(boolean condition, String message, Map<String, Object> details) {
        emit(condition, message, "always_or_unreachable", false, details, 2);
    }

    public static void sometimes(boolean condition, String message, Map<String, Object> details) {
        emit(condition, message, "sometimes", true, details, 2);
    }

    public static void reachable(String message, Map<String, Object> details) {
        emit(true, message, "reachable", true, details, 2);
    }

    public static void unreachable(String message, Map<String, Object> details) {
        emit(false, message, "unreachable", false, details, 2);
    }

    public static void everSince(boolean condition, String message, Map<String, Object> details) {
        String key = "ever_since:" + message;
        if (condition) {
            everSinceArmed.put(key, true);
        }
        boolean armed = everSinceArmed.containsKey(key);
        boolean effectiveCondition = condition || !armed;
        emit(effectiveCondition, message, "ever_since", true, details, 2);
    }

    public static void alwaysGreaterThan(long left, long right, String message, Map<String, Object> details) {
        Map<String, Object> d = details != null ? new java.util.HashMap<>(details) : new java.util.HashMap<>();
        d.put("left_value", left);
        d.put("right_value", right);
        emit(left > right, message, "always", true, d, 2);
    }

    public static void sometimesGreaterThan(long left, long right, String message, Map<String, Object> details) {
        Map<String, Object> d = details != null ? new java.util.HashMap<>(details) : new java.util.HashMap<>();
        d.put("left_value", left);
        d.put("right_value", right);
        emit(left > right, message, "sometimes", true, d, 2);
    }

    public static void alwaysEqual(long left, long right, String message, Map<String, Object> details) {
        Map<String, Object> d = details != null ? new java.util.HashMap<>(details) : new java.util.HashMap<>();
        d.put("left_value", left);
        d.put("right_value", right);
        emit(left == right, message, "always", true, d, 2);
    }

    public static void sometimesEqual(long left, long right, String message, Map<String, Object> details) {
        Map<String, Object> d = details != null ? new java.util.HashMap<>(details) : new java.util.HashMap<>();
        d.put("left_value", left);
        d.put("right_value", right);
        emit(left == right, message, "sometimes", true, d, 2);
    }

    public static void sometimesEach(String label, String key, Map<String, Object> details) {
        emit(true, label + ":" + key, "sometimes", true, details, 2);
    }

    public static void sometimesAll(String message, Map<String, Boolean> namedBools, Map<String, Object> details) {
        int satisfied = 0;
        int total = namedBools != null ? namedBools.size() : 0;
        if (namedBools != null) {
            for (boolean v : namedBools.values()) {
                if (v) satisfied++;
            }
        }
        boolean allTrue = total > 0 && satisfied == total;
        Map<String, Object> d = details != null ? new java.util.HashMap<>(details) : new java.util.HashMap<>();
        d.put("sub_goals", namedBools);
        d.put("satisfied_count", satisfied);
        d.put("total_count", total);
        emit(allTrue, message, "sometimes_all", true, d, 2);
    }

    private static void emit(boolean condition, String message, String assertType, boolean mustHit,
                              Map<String, Object> details, int skipFrames) {
        StackTraceElement[] stack = new Throwable().getStackTrace();
        StackTraceElement caller = stack.length > skipFrames ? stack[skipFrames] : stack[stack.length - 1];
        String file = caller.getFileName() != null ? caller.getFileName() : "unknown";
        int line = caller.getLineNumber();
        String id = callsiteId(file, line);

        String declKey = assertType + ":" + message;
        if (declared.add(declKey)) {
            SdkWriter.emit(buildJson(false, false, message, assertType, mustHit, id, null, file, line));
        }
        SdkWriter.emit(buildJson(true, condition, message, assertType, mustHit, id, details, file, line));
    }

    private static String callsiteId(String file, int line) {
        try {
            byte[] input = (file + ":" + line).getBytes(java.nio.charset.StandardCharsets.UTF_8);
            java.security.MessageDigest md = java.security.MessageDigest.getInstance("SHA-256");
            byte[] hash = md.digest(input);
            return String.format("%02x%02x%02x%02x", hash[0], hash[1], hash[2], hash[3]);
        } catch (java.security.NoSuchAlgorithmException e) {
            return String.format("%08x", ((long) (file + ":" + line).hashCode()) & 0xFFFFFFFFL);
        }
    }

    private static String buildJson(boolean hit, boolean condition, String message, String assertType,
                                    boolean mustHit, String id, Map<String, Object> details,
                                    String file, int line) {
        StringBuilder sb = new StringBuilder();
        sb.append("{\"openthesis_assert\":{");
        sb.append("\"hit\":").append(hit).append(",");
        sb.append("\"condition\":").append(condition).append(",");
        sb.append("\"message\":").append(jsonStr(message)).append(",");
        sb.append("\"assert_type\":").append(jsonStr(assertType)).append(",");
        sb.append("\"must_hit\":").append(mustHit).append(",");
        sb.append("\"id\":").append(jsonStr(id));
        if (details != null && !details.isEmpty()) {
            sb.append(",\"details\":").append(mapToJson(details));
        }
        sb.append(",\"location\":{\"file\":").append(jsonStr(file)).append(",\"line\":").append(line).append("}");
        sb.append("}}");
        return sb.toString();
    }

    static String jsonStr(String s) {
        if (s == null) return "null";
        StringBuilder sb = new StringBuilder("\"");
        for (int i = 0; i < s.length(); i++) {
            char c = s.charAt(i);
            switch (c) {
                case '\\': sb.append("\\\\"); break;
                case '"':  sb.append("\\\""); break;
                case '\n': sb.append("\\n");  break;
                case '\r': sb.append("\\r");  break;
                case '\t': sb.append("\\t");  break;
                default:
                    if (c < 0x20) {
                        sb.append(String.format("\\u%04x", (int) c));
                    } else {
                        sb.append(c);
                    }
            }
        }
        sb.append("\"");
        return sb.toString();
    }

    static String mapToJson(Map<String, Object> m) {
        if (m == null || m.isEmpty()) return "{}";
        StringBuilder sb = new StringBuilder("{");
        boolean first = true;
        for (Map.Entry<String, Object> entry : m.entrySet()) {
            if (!first) sb.append(",");
            first = false;
            sb.append(jsonStr(entry.getKey())).append(":").append(valueToJson(entry.getValue()));
        }
        sb.append("}");
        return sb.toString();
    }

    @SuppressWarnings("unchecked")
    static String valueToJson(Object v) {
        if (v == null) return "null";
        if (v instanceof Boolean || v instanceof Number) return v.toString();
        if (v instanceof String) return jsonStr((String) v);
        if (v instanceof Map) {
            try {
                return mapToJson((Map<String, Object>) v);
            } catch (ClassCastException e) {
                return jsonStr(v.toString());
            }
        }
        return jsonStr(v.toString());
    }
}
