#!/bin/sh

# Runs concurrently with other workloads.
set -euo pipefail

exec env ETCD_ITERATIONS=20 /opt/openthesis/bin/etcd-driver read
