#!/bin/sh

# Runs concurrently with other workloads.
# Tests that CAS txn semantics hold under concurrent writes and fault injection.

exec env ETCD_ITERATIONS=15 /opt/openthesis/bin/etcd-driver txn
