/*
 * openthesis.c - OpenThesis C99 SDK implementation.
 *
 * Design notes:
 *  - No malloc: all buffers are fixed-size on the stack or in static storage.
 *  - Thread-safe: single static pthread_mutex_t guards the output file and all
 *    shared state (writer, ever_since table, counter table, PRNG).
 *  - Output file opened once (lazy init) from $OPENTHESIS_OUTPUT_DIR/sdk.jsonl.
 *    If the env var is not set the file descriptor is -1 and every write is silently skipped.
 *  - JSON is built with snprintf; strings are escaped via json_escape().
 *  - Callsite ID: FNV-1a hash of "file:line", low 4 bytes, 8-char hex.
 *  - ever_since table: open-addressing hash table, 256 slots, FNV-1a key.
 *  - counter table:    same structure, stores int64_t accumulator per name.
 */

#define _POSIX_C_SOURCE 200112L  /* for pthread, fileno */

#include "openthesis.h"

#include <errno.h>
#include <fcntl.h>
#include <inttypes.h>
#include <pthread.h>
#include <stddef.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>
#include <unistd.h>

/* Internal constants */

#define OT_JSON_BUF     4096   /* max output line length                  */
#define OT_STR_BUF      2048   /* max escaped-string scratch buffer       */
#define OT_HT_SIZE      256    /* hash table capacity (must be power of 2) */
#define OT_NAME_MAX     128    /* max length of a name/message key stored  */

/* Mutex + writer fd */

static pthread_mutex_t g_mu = PTHREAD_MUTEX_INITIALIZER;
static int             g_fd  = -2;   /* -2 = not yet initialised, -1 = no-op */

/* Open (or decide to skip) the output file.  Must be called under g_mu. */
static void writer_init_locked(void) {
    if (g_fd != -2) return;   /* already done */

    const char *dir = getenv("OPENTHESIS_OUTPUT_DIR");
    if (!dir || dir[0] == '\0') {
        g_fd = -1;
        return;
    }

    char path[512];
    snprintf(path, sizeof(path), "%s/sdk.jsonl", dir);
    g_fd = open(path, O_WRONLY | O_CREAT | O_APPEND, 0644);
    if (g_fd < 0)
        g_fd = -1;   /* failed to open → silent no-op */
}

/* Write a NUL-terminated line + newline to the output file.
 * Must be called under g_mu. */
static void write_line(const char *buf) {
    if (g_fd < 0) return;
    size_t n = strlen(buf);
    /* write the JSON content */
    while (n > 0) {
        ssize_t w = write(g_fd, buf, n);
        if (w <= 0) return;
        buf += w;
        n   -= (size_t)w;
    }
    /* trailing newline */
    char nl = '\n';
    (void)write(g_fd, &nl, 1);
}

/* JSON string escaping */

/*
 * Copy src into out (NUL-terminated, at most outsz-1 chars written) with
 * JSON escaping: \, ", \n, \r, \t and other control characters (\uXXXX).
 */
static void json_escape(char *out, size_t outsz, const char *in) {
    if (!out || outsz == 0) return;
    size_t i = 0;
    const unsigned char *s = (const unsigned char *)in;

    if (!s) { out[0] = '\0'; return; }

    while (*s && i + 7 < outsz) {   /* leave room for longest escape + NUL */
        unsigned char c = *s++;
        if (c == '"')  { out[i++] = '\\'; out[i++] = '"';  }
        else if (c == '\\') { out[i++] = '\\'; out[i++] = '\\'; }
        else if (c == '\n') { out[i++] = '\\'; out[i++] = 'n';  }
        else if (c == '\r') { out[i++] = '\\'; out[i++] = 'r';  }
        else if (c == '\t') { out[i++] = '\\'; out[i++] = 't';  }
        else if (c < 0x20) {
            /* \uXXXX */
            i += (size_t)snprintf(out + i, outsz - i, "\\u%04x", (unsigned)c);
        } else {
            out[i++] = (char)c;
        }
    }
    out[i] = '\0';
}

/* FNV-1a helpers */

static uint32_t fnv1a32(const char *s, int extra_int) {
    uint32_t h = 2166136261u;
    for (; *s; s++) {
        h ^= (uint8_t)*s;
        h *= 16777619u;
    }
    /* mix in the integer (used for callsite hashing "file:line") */
    if (extra_int >= 0) {
        h ^= (uint32_t)(extra_int & 0xffff);
        h *= 16777619u;
        h ^= (uint32_t)((extra_int >> 16) & 0xffff);
        h *= 16777619u;
    }
    return h;
}

static uint64_t fnv1a64(const char *s) {
    uint64_t h = 14695981039346656037ULL;
    for (; *s; s++) {
        h ^= (uint8_t)*s;
        h *= 1099511628211ULL;
    }
    return h;
}

/* Callsite ID: FNV-1a32 of "file:line", formatted as 8-char hex */

static void callsite_id(char out[9], const char *file, int line) {
    uint32_t h = fnv1a32(file, line);
    snprintf(out, 9, "%08x", h);
}

/* ever_since hash table: stores "armed" state per message key.
 * Once armed, a false condition is reported as a violation. */

typedef struct {
    uint64_t key_hash;              /* 0 = empty slot */
    char     key[OT_NAME_MAX];
    int      armed;
} ot_es_slot_t;

static ot_es_slot_t g_es_table[OT_HT_SIZE];

/* Returns pointer to slot for key (inserts empty slot if not found). */
static ot_es_slot_t *es_find(const char *key) {
    uint64_t h = fnv1a64(key);
    if (h == 0) h = 1;   /* reserve 0 for empty */
    size_t idx = (size_t)(h & (OT_HT_SIZE - 1));
    for (size_t probe = 0; probe < OT_HT_SIZE; probe++) {
        ot_es_slot_t *slot = &g_es_table[(idx + probe) & (OT_HT_SIZE - 1)];
        if (slot->key_hash == 0) {
            /* empty - claim it */
            slot->key_hash = h;
            strncpy(slot->key, key, OT_NAME_MAX - 1);
            slot->key[OT_NAME_MAX - 1] = '\0';
            slot->armed = 0;
            return slot;
        }
        if (slot->key_hash == h && strncmp(slot->key, key, OT_NAME_MAX) == 0)
            return slot;
    }
    return NULL;   /* table full (shouldn't happen in practice) */
}

/* counter hash table - Accumulates int64_t counters per name key. */

typedef struct {
    uint64_t key_hash;
    char     key[OT_NAME_MAX];
    int64_t  value;
} ot_ctr_slot_t;

static ot_ctr_slot_t g_ctr_table[OT_HT_SIZE];

static ot_ctr_slot_t *ctr_find(const char *key) {
    uint64_t h = fnv1a64(key);
    if (h == 0) h = 1;
    size_t idx = (size_t)(h & (OT_HT_SIZE - 1));
    for (size_t probe = 0; probe < OT_HT_SIZE; probe++) {
        ot_ctr_slot_t *slot = &g_ctr_table[(idx + probe) & (OT_HT_SIZE - 1)];
        if (slot->key_hash == 0) {
            slot->key_hash = h;
            strncpy(slot->key, key, OT_NAME_MAX - 1);
            slot->key[OT_NAME_MAX - 1] = '\0';
            slot->value = 0;
            return slot;
        }
        if (slot->key_hash == h && strncmp(slot->key, key, OT_NAME_MAX) == 0)
            return slot;
    }
    return NULL;
}

/* declared-assertion tracking: emits a hit:false record on first encounter
 * of each (assertType, message) pair, keyed on FNV-1a64. */

typedef struct {
    uint64_t key_hash;   /* 0 = empty */
} ot_decl_slot_t;

static ot_decl_slot_t g_decl_table[OT_HT_SIZE];

/* Returns 1 if this is the first time we see this key (and records it),
 * 0 if already seen. */
static int decl_first_time(uint64_t h) {
    if (h == 0) h = 1;
    size_t idx = (size_t)(h & (OT_HT_SIZE - 1));
    for (size_t probe = 0; probe < OT_HT_SIZE; probe++) {
        ot_decl_slot_t *slot = &g_decl_table[(idx + probe) & (OT_HT_SIZE - 1)];
        if (slot->key_hash == 0) {
            slot->key_hash = h;
            return 1;
        }
        if (slot->key_hash == h)
            return 0;
    }
    return 0;   /* table full - skip declaration */
}

/* SplitMix64 PRNG */

static uint64_t g_prng_state = 0;
static int      g_prng_inited = 0;

static void prng_init_locked(void) {
    if (g_prng_inited) return;
    g_prng_inited = 1;

    const char *seed_str = getenv("OPENTHESIS_SEED");
    if (seed_str && seed_str[0] != '\0') {
        char *end;
        unsigned long long v = strtoull(seed_str, &end, 10);
        if (end != seed_str) {
            g_prng_state = (uint64_t)v;
            return;
        }
    }
    /* fallback: time-based seed (non-deterministic, outside VM only) */
    g_prng_state = (uint64_t)time(NULL);
    if (g_prng_state == 0) g_prng_state = 0x9E3779B97F4A7C15ULL;
}

static uint64_t splitmix64_next(void) {
    g_prng_state += 0x9e3779b97f4a7c15ULL;
    uint64_t z = g_prng_state;
    z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9ULL;
    z = (z ^ (z >> 27)) * 0x94d049bb133111ebULL;
    return z ^ (z >> 31);
}

/* Core assertion emitter */

/*
 * Build and write an openthesis_assert JSON line.
 *
 * hit=0 for the declaration record, hit=1 for the live evaluation.
 */
static void emit_assert(int hit, int condition,
                        const char *message, const char *assert_type,
                        int must_hit, const char *details_json,
                        const char *file, int line) {
    char esc_msg[OT_STR_BUF];
    char esc_det[OT_STR_BUF];
    char esc_file[OT_STR_BUF];
    char id[9];
    char buf[OT_JSON_BUF];

    json_escape(esc_msg,  sizeof(esc_msg),  message     ? message     : "");
    json_escape(esc_file, sizeof(esc_file), file        ? file        : "unknown");
    callsite_id(id, file ? file : "unknown", line);

    /* details: use raw JSON if provided and non-empty, else null */
    int has_details = (details_json && details_json[0] != '\0');
    if (has_details) {
        /* the caller is responsible for valid JSON; we copy it verbatim */
        strncpy(esc_det, details_json, sizeof(esc_det) - 1);
        esc_det[sizeof(esc_det) - 1] = '\0';
    }

    int n;
    if (hit) {
        if (has_details) {
            n = snprintf(buf, sizeof(buf),
                "{\"openthesis_assert\":{"
                "\"hit\":true,"
                "\"condition\":%s,"
                "\"message\":\"%s\","
                "\"assert_type\":\"%s\","
                "\"must_hit\":%s,"
                "\"id\":\"%s\","
                "\"details\":%s,"
                "\"location\":{\"file\":\"%s\",\"line\":%d}"
                "}}",
                condition  ? "true" : "false",
                esc_msg,
                assert_type,
                must_hit   ? "true" : "false",
                id,
                esc_det,
                esc_file, line);
        } else {
            n = snprintf(buf, sizeof(buf),
                "{\"openthesis_assert\":{"
                "\"hit\":true,"
                "\"condition\":%s,"
                "\"message\":\"%s\","
                "\"assert_type\":\"%s\","
                "\"must_hit\":%s,"
                "\"id\":\"%s\","
                "\"location\":{\"file\":\"%s\",\"line\":%d}"
                "}}",
                condition  ? "true" : "false",
                esc_msg,
                assert_type,
                must_hit   ? "true" : "false",
                id,
                esc_file, line);
        }
    } else {
        /* declaration record: hit=false, condition=false, no details */
        n = snprintf(buf, sizeof(buf),
            "{\"openthesis_assert\":{"
            "\"hit\":false,"
            "\"condition\":false,"
            "\"message\":\"%s\","
            "\"assert_type\":\"%s\","
            "\"must_hit\":%s,"
            "\"id\":\"%s\","
            "\"location\":{\"file\":\"%s\",\"line\":%d}"
            "}}",
            esc_msg,
            assert_type,
            must_hit ? "true" : "false",
            id,
            esc_file, line);
    }

    (void)n;   /* truncation is acceptable; the line is still valid JSON up to OT_JSON_BUF-1 */
    write_line(buf);
}

/* Combined init + declaration-check + emit, under the global lock. */
static void assert_emit(int condition,
                        const char *message, const char *assert_type,
                        int must_hit, const char *details_json,
                        const char *file, int line) {
    pthread_mutex_lock(&g_mu);

    writer_init_locked();

    if (g_fd >= 0) {
        /* Build declaration key: FNV-1a64 of assertType + ":" + message */
        char decl_key[OT_NAME_MAX * 2];
        snprintf(decl_key, sizeof(decl_key), "%s:%s",
                 assert_type ? assert_type : "",
                 message     ? message     : "");
        uint64_t dk = fnv1a64(decl_key);

        if (decl_first_time(dk))
            emit_assert(0, 0, message, assert_type, must_hit, NULL, file, line);

        emit_assert(1, condition, message, assert_type, must_hit, details_json, file, line);
    }

    pthread_mutex_unlock(&g_mu);
}

/* Public assertion functions */

void ot_always_at(int condition, const char *message, const char *details_json,
                  const char *file, int line) {
    assert_emit(condition, message, "always", 1, details_json, file, line);
}

void ot_always_or_unreachable_at(int condition, const char *message,
                                  const char *details_json,
                                  const char *file, int line) {
    assert_emit(condition, message, "always_or_unreachable", 0, details_json, file, line);
}

void ot_sometimes_at(int condition, const char *message, const char *details_json,
                     const char *file, int line) {
    assert_emit(condition, message, "sometimes", 1, details_json, file, line);
}

void ot_reachable_at(const char *message, const char *details_json,
                     const char *file, int line) {
    assert_emit(1, message, "reachable", 1, details_json, file, line);
}

void ot_unreachable_at(const char *message, const char *details_json,
                       const char *file, int line) {
    assert_emit(0, message, "unreachable", 0, details_json, file, line);
}

void ot_ever_since_at(int condition, const char *message, const char *details_json,
                      const char *file, int line) {
    pthread_mutex_lock(&g_mu);
    writer_init_locked();

    int effective = condition;
    if (g_fd >= 0) {
        ot_es_slot_t *slot = es_find(message ? message : "");
        if (slot) {
            if (condition)
                slot->armed = 1;
            /* if armed but condition is now false → violation (effective=0) */
            /* if not yet armed → pass (condition may be false before first arm) */
            effective = condition || !slot->armed;
        }

        char decl_key[OT_NAME_MAX * 2];
        snprintf(decl_key, sizeof(decl_key), "ever_since:%s",
                 message ? message : "");
        uint64_t dk = fnv1a64(decl_key);
        if (decl_first_time(dk))
            emit_assert(0, 0, message, "ever_since", 1, NULL, file, line);

        emit_assert(1, effective, message, "ever_since", 1,
                    details_json, file, line);
    }

    pthread_mutex_unlock(&g_mu);
}

/* Comparison assertion helpers: build details JSON with operand values. */
static void assert_comparison(int64_t left, int64_t right, int passed,
                               const char *message, const char *assert_type,
                               const char *details_json,
                               const char *file, int line) {
    char det[OT_JSON_BUF];
    snprintf(det, sizeof(det), "{\"left\":%lld,\"right\":%lld}", (long long)left, (long long)right);
    (void)details_json; /* user details ignored; operands take priority */
    assert_emit(passed, message, assert_type, 1, det, file, line);
}

void ot_always_greater_than_at(int64_t left, int64_t right, const char *message,
                                const char *details_json, const char *file, int line) {
    assert_comparison(left, right, left > right, message, "always", details_json, file, line);
}
void ot_sometimes_greater_than_at(int64_t left, int64_t right, const char *message,
                                   const char *details_json, const char *file, int line) {
    assert_comparison(left, right, left > right, message, "sometimes", details_json, file, line);
}
void ot_always_equal_at(int64_t left, int64_t right, const char *message,
                         const char *details_json, const char *file, int line) {
    assert_comparison(left, right, left == right, message, "always", details_json, file, line);
}
void ot_sometimes_equal_at(int64_t left, int64_t right, const char *message,
                            const char *details_json, const char *file, int line) {
    assert_comparison(left, right, left == right, message, "sometimes", details_json, file, line);
}

void ot_sometimes_each_at(const char *label, const char *key,
                           const char *details_json, const char *file, int line) {
    /* Emit a "sometimes" assertion keyed by (label, key). Passes when reached. */
    char msg[OT_NAME_MAX * 2];
    snprintf(msg, sizeof(msg), "%s:%s", label ? label : "", key ? key : "");
    ot_sometimes_at(1, msg, details_json, file, line);
}

/* Convenience wrappers (file="unknown", line=0) */
void ot_always(int condition, const char *message, const char *details_json) {
    ot_always_at(condition, message, details_json, "unknown", 0);
}
void ot_always_or_unreachable(int condition, const char *message, const char *details_json) {
    ot_always_or_unreachable_at(condition, message, details_json, "unknown", 0);
}
void ot_sometimes(int condition, const char *message, const char *details_json) {
    ot_sometimes_at(condition, message, details_json, "unknown", 0);
}
void ot_reachable(const char *message, const char *details_json) {
    ot_reachable_at(message, details_json, "unknown", 0);
}
void ot_unreachable(const char *message, const char *details_json) {
    ot_unreachable_at(message, details_json, "unknown", 0);
}
void ot_ever_since(int condition, const char *message, const char *details_json) {
    ot_ever_since_at(condition, message, details_json, "unknown", 0);
}
void ot_always_greater_than(int64_t left, int64_t right, const char *message, const char *details_json) {
    ot_always_greater_than_at(left, right, message, details_json, "unknown", 0);
}
void ot_sometimes_greater_than(int64_t left, int64_t right, const char *message, const char *details_json) {
    ot_sometimes_greater_than_at(left, right, message, details_json, "unknown", 0);
}
void ot_always_equal(int64_t left, int64_t right, const char *message, const char *details_json) {
    ot_always_equal_at(left, right, message, details_json, "unknown", 0);
}
void ot_sometimes_equal(int64_t left, int64_t right, const char *message, const char *details_json) {
    ot_sometimes_equal_at(left, right, message, details_json, "unknown", 0);
}
void ot_sometimes_each(const char *label, const char *key, const char *details_json) {
    ot_sometimes_each_at(label, key, details_json, "unknown", 0);
}

void ot_sometimes_all_at(const char *message,
                          const char **names, const int *values, int count,
                          const char *details_json, const char *file, int line) {
    int satisfied = 0;
    for (int i = 0; i < count; i++) {
        if (values[i]) satisfied++;
    }
    int all_true = (count > 0 && satisfied == count);

    /* Build sub_goals JSON: {"name1":true,"name2":false,...} */
    char sub[OT_JSON_BUF];
    int pos = 0;
    pos += snprintf(sub + pos, sizeof(sub) - (size_t)pos, "{");
    for (int i = 0; i < count && pos < (int)sizeof(sub) - 32; i++) {
        char esc[OT_STR_BUF];
        json_escape(esc, sizeof(esc), names[i] ? names[i] : "");
        pos += snprintf(sub + pos, sizeof(sub) - (size_t)pos,
                        "%s\"%s\":%s", i ? "," : "", esc, values[i] ? "true" : "false");
    }
    if (pos < (int)sizeof(sub) - 2) sub[pos++] = '}';
    sub[pos] = '\0';

    char det[OT_JSON_BUF];
    snprintf(det, sizeof(det),
             "{\"sub_goals\":%s,\"satisfied_count\":%d,\"total_count\":%d}",
             sub, satisfied, count);

    assert_emit(all_true, message, "sometimes_all", 1, det, file, line);
    (void)details_json; /* caller details merged into det above */
}

void ot_sometimes_all(const char *message,
                      const char **names, const int *values, int count,
                      const char *details_json) {
    ot_sometimes_all_at(message, names, values, count, details_json, "unknown", 0);
}

/* Guidance emitter */

static void emit_guidance(const char *guidance_type, const char *name, int64_t value) {
    char esc_name[OT_STR_BUF];
    char buf[OT_JSON_BUF];

    json_escape(esc_name, sizeof(esc_name), name ? name : "");

    snprintf(buf, sizeof(buf),
        "{\"openthesis_guidance\":{"
        "\"guidance_type\":\"%s\","
        "\"name\":\"%s\","
        "\"value\":%" PRId64
        "}}",
        guidance_type, esc_name, value);

    write_line(buf);
}

void ot_maximize_int(const char *name, int64_t value) {
    pthread_mutex_lock(&g_mu);
    writer_init_locked();
    if (g_fd >= 0) emit_guidance("maximize", name, value);
    pthread_mutex_unlock(&g_mu);
}

void ot_explore(const char *name, int64_t value) {
    pthread_mutex_lock(&g_mu);
    writer_init_locked();
    if (g_fd >= 0) emit_guidance("explore", name, value);
    pthread_mutex_unlock(&g_mu);
}

void ot_explore_pair(const char *name, int64_t a, int64_t b) {
    int64_t combined = (a & 0x00000000FFFFFFFFLL) |
                       ((b & 0x00000000FFFFFFFFLL) << 32);
    ot_explore(name, combined);
}

void ot_track_state(const char *name, const char *state) {
    uint64_t h = fnv1a64(state ? state : "");
    ot_explore(name, (int64_t)h);
}

void ot_track_counter(const char *name, int64_t delta) {
    pthread_mutex_lock(&g_mu);
    writer_init_locked();

    if (g_fd >= 0) {
        ot_ctr_slot_t *slot = ctr_find(name ? name : "");
        if (slot) {
            slot->value += delta;
            emit_guidance("maximize", name, slot->value);
        }
    }

    pthread_mutex_unlock(&g_mu);
}

/* Lifecycle emitters */

/*
 * Emit a generic lifecycle envelope of the form:
 *   {"<key>": {"status": "<status>", "details": <details_json_or_null>}}
 */
static void emit_lifecycle_status(const char *key, const char *status,
                                   const char *details_json) {
    char buf[OT_JSON_BUF];
    int has_details = (details_json && details_json[0] != '\0');

    if (has_details) {
        snprintf(buf, sizeof(buf),
            "{\"%s\":{\"status\":\"%s\",\"details\":%s}}",
            key, status, details_json);
    } else {
        snprintf(buf, sizeof(buf),
            "{\"%s\":{\"status\":\"%s\",\"details\":null}}",
            key, status);
    }
    write_line(buf);
}

void ot_setup_complete(const char *details_json) {
    pthread_mutex_lock(&g_mu);
    writer_init_locked();
    if (g_fd >= 0)
        emit_lifecycle_status("openthesis_setup_complete", "complete", details_json);
    pthread_mutex_unlock(&g_mu);
}

void ot_send_event(const char *event_name, const char *details_json) {
    char esc_name[OT_STR_BUF];
    char buf[OT_JSON_BUF];

    json_escape(esc_name, sizeof(esc_name), event_name ? event_name : "");

    pthread_mutex_lock(&g_mu);
    writer_init_locked();

    if (g_fd >= 0) {
        int has_details = (details_json && details_json[0] != '\0');
        if (has_details) {
            snprintf(buf, sizeof(buf),
                "{\"openthesis_send_event\":{"
                "\"event_name\":\"%s\","
                "\"details\":%s"
                "}}",
                esc_name, details_json);
        } else {
            snprintf(buf, sizeof(buf),
                "{\"openthesis_send_event\":{"
                "\"event_name\":\"%s\","
                "\"details\":null"
                "}}",
                esc_name);
        }
        write_line(buf);
    }

    pthread_mutex_unlock(&g_mu);
}

void ot_teardown(const char *details_json) {
    pthread_mutex_lock(&g_mu);
    writer_init_locked();
    if (g_fd >= 0)
        emit_lifecycle_status("openthesis_teardown", "complete", details_json);
    pthread_mutex_unlock(&g_mu);
}

void ot_stop_faults(double duration_seconds) {
    char buf[OT_JSON_BUF];
    pthread_mutex_lock(&g_mu);
    writer_init_locked();

    if (g_fd >= 0) {
        snprintf(buf, sizeof(buf),
            "{\"openthesis_stop_faults\":{"
            "\"duration_seconds\":%g,"
            "\"status\":\"requested\""
            "}}",
            duration_seconds);
        write_line(buf);
    }

    pthread_mutex_unlock(&g_mu);
}

/* Random */

uint64_t ot_random_uint64(void) {
    pthread_mutex_lock(&g_mu);
    prng_init_locked();
    uint64_t v = splitmix64_next();
    pthread_mutex_unlock(&g_mu);
    return v;
}

int ot_random_int(int lo, int hi) {
    if (lo >= hi) return lo;
    uint64_t range = (uint64_t)((int64_t)hi - (int64_t)lo + 1);
    uint64_t r = ot_random_uint64();
    return lo + (int)(r % range);
}
