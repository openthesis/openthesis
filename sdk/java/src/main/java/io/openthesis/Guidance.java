package io.openthesis;

import java.util.Map;
import java.util.concurrent.ConcurrentHashMap;

public class Guidance {
    private static final Map<String, Long> counters = new ConcurrentHashMap<>();

    public static void maximizeInt(String name, long value) {
        emit("maximize", name, value);
    }

    public static void explore(String name, long value) {
        emit("explore", name, value);
    }

    public static void explorePair(String name, long a, long b) {
        long combined = (a & 0xFFFFFFFFL) | ((b & 0xFFFFFFFFL) << 32);
        emit("explore", name, combined);
    }

    public static void trackState(String name, String state) {
        long h = -3750763034362895579L; // FNV-1a offset basis: 14695981039346656037 as signed long
        byte[] bytes = state.getBytes(java.nio.charset.StandardCharsets.UTF_8);
        for (byte b : bytes) {
            h ^= (b & 0xFFL);
            h *= 1099511628211L;
        }
        emit("explore", name, h);
    }

    public static void trackCounter(String name, long delta) {
        long total = counters.merge(name, delta, Long::sum);
        emit("maximize", name, total);
    }

    private static void emit(String guidanceType, String name, long value) {
        StringBuilder sb = new StringBuilder();
        sb.append("{\"openthesis_guidance\":{");
        sb.append("\"guidance_type\":").append(Assert.jsonStr(guidanceType)).append(",");
        sb.append("\"name\":").append(Assert.jsonStr(name)).append(",");
        sb.append("\"value\":").append(value);
        sb.append("}}");
        SdkWriter.emit(sb.toString());
    }
}
