package io.openthesis;

import java.io.*;
import java.nio.file.*;
import java.util.concurrent.locks.ReentrantLock;

class SdkWriter {
    private static final ReentrantLock LOCK = new ReentrantLock();
    private static volatile BufferedWriter writer = null;
    private static volatile boolean initialized = false;

    static void emit(String json) {
        LOCK.lock();
        try {
            if (!initialized) {
                writer = openWriter();
                initialized = true;
            }
            if (writer == null) return;
            writer.write(json);
            writer.newLine();
            writer.flush();
        } catch (IOException e) {
            // Best-effort: swallow
        } finally {
            LOCK.unlock();
        }
    }

    private static BufferedWriter openWriter() {
        String dir = System.getenv("OPENTHESIS_OUTPUT_DIR");
        if (dir == null || dir.isEmpty()) {
            dir = System.getenv("ANTITHESIS_OUTPUT_DIR"); // backward compat
        }
        if (dir == null || dir.isEmpty()) {
            String local = System.getenv("OPENTHESIS_SDK_LOCAL_OUTPUT");
            if (local == null || local.isEmpty()) {
                local = System.getenv("ANTITHESIS_SDK_LOCAL_OUTPUT"); // backward compat
            }
            if (local == null || local.isEmpty()) return null;
            try {
                return new BufferedWriter(new FileWriter(local, true));
            } catch (IOException e) {
                return null;
            }
        }
        try {
            Path p = Paths.get(dir, "sdk.jsonl");
            return new BufferedWriter(new FileWriter(p.toFile(), true));
        } catch (IOException e) {
            return null;
        }
    }
}
