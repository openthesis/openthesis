#!/bin/sh
# settle_check.sh - verify final row count after all workloads.
# Runs after workloads complete.

echo "=== Final integrity check ==="
/opt/openthesis/bin/pg-driver check
echo "=== Check complete ==="
