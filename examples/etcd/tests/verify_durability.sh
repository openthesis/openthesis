#!/bin/sh
# verify_durability.sh - final linearizability check.
# Runs last. Performs a write-then-read-all-nodes check via the driver.
# Note: uses POSIX sh and wget (busybox-compatible) inside minimal initramfs.

NODES="${ETCD_NODES:-127.0.0.1:12379,127.0.0.1:22379,127.0.0.1:32379}"

echo "=== Final etcd cluster state ==="
for node in $(echo "$NODES" | tr ',' ' '); do
    echo "--- $node ---"
    wget -q -O - \
        --header="Content-Type: application/json" \
        --post-data='{}' \
        "http://${node}/v3/maintenance/status" 2>/dev/null || echo "  unreachable"
done

echo ""
echo "=== Running final linearizability check ==="
/opt/openthesis/bin/etcd-driver check

echo "=== Final durability check complete ==="
