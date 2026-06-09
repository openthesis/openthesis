#!/bin/sh

# Runs concurrently with other workloads.
# Tests leader election and slot ownership transfer under fault injection.

exec /opt/openthesis/bin/redis-driver failover
