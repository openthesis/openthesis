#!/bin/sh

# Runs concurrently with other workloads.
# Tests that watch events are delivered under fault injection.

exec env ETCD_ITERATIONS=10 /opt/openthesis/bin/etcd-driver watch
