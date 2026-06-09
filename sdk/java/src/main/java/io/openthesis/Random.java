package io.openthesis;

import java.util.concurrent.locks.ReentrantLock;

public class Random {
    private static final ReentrantLock LOCK = new ReentrantLock();
    private static volatile Random INSTANCE = null;

    private long state;

    public Random() {
        String seed = System.getenv("OPENTHESIS_SEED");
        if (seed != null && !seed.isEmpty()) {
            try {
                this.state = Long.parseUnsignedLong(seed);
                return;
            } catch (NumberFormatException e) {
                // fall through
            }
        }
        this.state = System.nanoTime() ^ (long) System.identityHashCode(this);
    }

    public Random(long seed) {
        this.state = seed;
    }

    public long nextLong() {
        state += 0x9e3779b97f4a7c15L;
        long z = state;
        z = (z ^ (z >>> 30)) * 0xbf58476d1ce4e5b9L;
        z = (z ^ (z >>> 27)) * 0x94d049bb133111ebL;
        return z ^ (z >>> 31);
    }

    public long nextLong(long bound) {
        if (bound <= 0) throw new IllegalArgumentException("bound must be positive");
        long raw = nextLong();
        long result = (raw & Long.MAX_VALUE) % bound;
        return result;
    }

    public int nextInt(int bound) {
        if (bound <= 0) throw new IllegalArgumentException("bound must be positive");
        return (int) ((nextLong() & Integer.MAX_VALUE) % bound);
    }

    public boolean nextBoolean() {
        return (nextLong() & 1L) == 0L;
    }

    public double nextDouble() {
        return (nextLong() >>> 11) * (1.0 / (1L << 53));
    }

    public byte[] getBytes(int n) {
        byte[] buf = new byte[n];
        for (int i = 0; i < n; ) {
            long val = nextLong();
            for (int j = 0; j < 8 && i < n; j++, i++) {
                buf[i] = (byte) (val >>> (j * 8));
            }
        }
        return buf;
    }

    @SafeVarargs
    public final <T> T choose(T... choices) {
        if (choices.length == 0) throw new IllegalArgumentException("choices must not be empty");
        if (choices.length == 1) return choices[0];
        int idx = nextInt(choices.length);
        emitBranchPoint(idx, choices.length, "choose");
        return choices[idx];
    }

    public <T> T choose(java.util.List<T> choices) {
        if (choices.isEmpty()) throw new IllegalArgumentException("choices must not be empty");
        if (choices.size() == 1) return choices.get(0);
        int idx = nextInt(choices.size());
        emitBranchPoint(idx, choices.size(), "choose");
        return choices.get(idx);
    }

    private void emitBranchPoint(int chosenIndex, int totalChoices, String valueType) {
        String dir = System.getenv("OPENTHESIS_OUTPUT_DIR");
        if (dir == null || dir.isEmpty()) {
            dir = System.getenv("ANTITHESIS_OUTPUT_DIR"); // backward compat
        }
        if (dir == null || dir.isEmpty()) return;
        StringBuilder sb = new StringBuilder();
        sb.append("{\"openthesis_random_choice\":{");
        sb.append("\"chosen_index\":").append(chosenIndex).append(",");
        sb.append("\"total_choices\":").append(totalChoices).append(",");
        sb.append("\"name\":\"\",");
        sb.append("\"value_type\":").append(Assert.jsonStr(valueType));
        sb.append("}}");
        SdkWriter.emit(sb.toString());
    }

    public static Random global() {
        if (INSTANCE == null) {
            LOCK.lock();
            try {
                if (INSTANCE == null) {
                    INSTANCE = new Random();
                }
            } finally {
                LOCK.unlock();
            }
        }
        return INSTANCE;
    }

    public static long getRandomLong() {
        LOCK.lock();
        try {
            return global().nextLong();
        } finally {
            LOCK.unlock();
        }
    }

    public static byte[] getRandomBytes(int n) {
        LOCK.lock();
        try {
            return global().getBytes(n);
        } finally {
            LOCK.unlock();
        }
    }
}
