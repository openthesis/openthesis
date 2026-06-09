/*
 * openthesis.h - OpenThesis C99 SDK, single-header public API.
 *
 * Wire format is identical to the Go SDK:
 *   {"openthesis_assert":   {...}}
 *   {"openthesis_guidance": {...}}
 *   {"openthesis_setup_complete": {...}}
 *   {"openthesis_send_event": {...}}
 *   {"openthesis_teardown": {...}}
 *   {"openthesis_stop_faults": {...}}
 *
 * Output: $OPENTHESIS_OUTPUT_DIR/sdk.jsonl.
 * If the env var is not set, all functions are silent no-ops.
 *
 * Thread-safety: all public functions are thread-safe (single static mutex).
 * No malloc on any hot path; fixed-size stack buffers are used throughout.
 */

#ifndef OPENTHESIS_H
#define OPENTHESIS_H

#ifdef __cplusplus
extern "C" {
#endif

#include <stdint.h>

/* Assertions (low-level, with explicit file/line): */

void ot_always_at(int condition, const char *message, const char *details_json,
                  const char *file, int line);
void ot_always_or_unreachable_at(int condition, const char *message, const char *details_json,
                                  const char *file, int line);
void ot_sometimes_at(int condition, const char *message, const char *details_json,
                     const char *file, int line);
void ot_reachable_at(const char *message, const char *details_json,
                     const char *file, int line);
void ot_unreachable_at(const char *message, const char *details_json,
                       const char *file, int line);
void ot_ever_since_at(int condition, const char *message, const char *details_json,
                      const char *file, int line);
void ot_always_greater_than_at(int64_t left, int64_t right, const char *message,
                                const char *details_json, const char *file, int line);
void ot_sometimes_greater_than_at(int64_t left, int64_t right, const char *message,
                                   const char *details_json, const char *file, int line);
void ot_always_equal_at(int64_t left, int64_t right, const char *message,
                         const char *details_json, const char *file, int line);
void ot_sometimes_equal_at(int64_t left, int64_t right, const char *message,
                            const char *details_json, const char *file, int line);
void ot_sometimes_each_at(const char *label, const char *key,
                           const char *details_json, const char *file, int line);
/* names[] and values[] are parallel arrays of length count.
 * The assertion passes when all values[] entries are non-zero simultaneously. */
void ot_sometimes_all_at(const char *message,
                          const char **names, const int *values, int count,
                          const char *details_json, const char *file, int line);

/* Assertions (convenience wrappers - uses "unknown"/0 for location): */
 * Prefer the OT_* macros below so the real callsite is captured.

void ot_always(int condition, const char *message, const char *details_json);
void ot_always_or_unreachable(int condition, const char *message, const char *details_json);
void ot_sometimes(int condition, const char *message, const char *details_json);
void ot_reachable(const char *message, const char *details_json);
void ot_unreachable(const char *message, const char *details_json);
void ot_ever_since(int condition, const char *message, const char *details_json);
void ot_always_greater_than(int64_t left, int64_t right, const char *message, const char *details_json);
void ot_sometimes_greater_than(int64_t left, int64_t right, const char *message, const char *details_json);
void ot_always_equal(int64_t left, int64_t right, const char *message, const char *details_json);
void ot_sometimes_equal(int64_t left, int64_t right, const char *message, const char *details_json);
void ot_sometimes_each(const char *label, const char *key, const char *details_json);
void ot_sometimes_all(const char *message,
                      const char **names, const int *values, int count,
                      const char *details_json);

/* Assertion macros - capture real __FILE__ / __LINE__: */

#define OT_ALWAYS(cond, msg, det) \
    ot_always_at((cond), (msg), (det), __FILE__, __LINE__)

#define OT_ALWAYS_OR_UNREACHABLE(cond, msg, det) \
    ot_always_or_unreachable_at((cond), (msg), (det), __FILE__, __LINE__)

#define OT_SOMETIMES(cond, msg, det) \
    ot_sometimes_at((cond), (msg), (det), __FILE__, __LINE__)

#define OT_REACHABLE(msg, det) \
    ot_reachable_at((msg), (det), __FILE__, __LINE__)

#define OT_UNREACHABLE(msg, det) \
    ot_unreachable_at((msg), (det), __FILE__, __LINE__)

#define OT_EVER_SINCE(cond, msg, det) \
    ot_ever_since_at((cond), (msg), (det), __FILE__, __LINE__)

#define OT_ALWAYS_GREATER_THAN(left, right, msg, det) \
    ot_always_greater_than_at((left), (right), (msg), (det), __FILE__, __LINE__)

#define OT_SOMETIMES_GREATER_THAN(left, right, msg, det) \
    ot_sometimes_greater_than_at((left), (right), (msg), (det), __FILE__, __LINE__)

#define OT_ALWAYS_EQUAL(left, right, msg, det) \
    ot_always_equal_at((left), (right), (msg), (det), __FILE__, __LINE__)

#define OT_SOMETIMES_EQUAL(left, right, msg, det) \
    ot_sometimes_equal_at((left), (right), (msg), (det), __FILE__, __LINE__)

#define OT_SOMETIMES_EACH(label, key, det) \
    ot_sometimes_each_at((label), (key), (det), __FILE__, __LINE__)

#define OT_SOMETIMES_ALL(msg, names, values, count, det) \
    ot_sometimes_all_at((msg), (names), (values), (count), (det), __FILE__, __LINE__)

/* Guidance: */

/* Report a value to maximize. Engine prioritizes states where this value
 * reaches new highs (IJON_MAX equivalent). */
void ot_maximize_int(const char *name, int64_t value);

/* Report a state-space coordinate. Each unique value is new coverage
 * (IJON_SET equivalent). */
void ot_explore(const char *name, int64_t value);

/* Two-dimensional explore: packs (a, b) into a single int64. */
void ot_explore_pair(const char *name, int64_t a, int64_t b);

/* Hash state string to int64 and call ot_explore(). */
void ot_track_state(const char *name, const char *state);

/* Accumulate a running counter and call ot_maximize_int() with the total. */
void ot_track_counter(const char *name, int64_t delta);

/* Lifecycle: */

/* Signal that the SUT is ready. Platform begins test execution after this. */
void ot_setup_complete(const char *details_json);

/* Emit a custom named lifecycle event. */
void ot_send_event(const char *event_name, const char *details_json);

/* Signal graceful shutdown. */
void ot_teardown(const char *details_json);

/* Request a fault-injection quiet period of duration_seconds. */
void ot_stop_faults(double duration_seconds);

/* Random (SplitMix64, seeded from OPENTHESIS_SEED or time(NULL)): */

/* Return a uniformly random uint64. */
uint64_t ot_random_uint64(void);

/* Return a uniformly random int in [lo, hi] inclusive. */
int ot_random_int(int lo, int hi);

#ifdef __cplusplus
} /* extern "C" */
#endif

#endif /* OPENTHESIS_H */
