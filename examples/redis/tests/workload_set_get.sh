#!/bin/sh

# Runs concurrently with other workloads.
# Exercises MOVED/ASK redirect handling and data durability under faults.

exec /opt/openthesis/bin/redis-driver set-get
