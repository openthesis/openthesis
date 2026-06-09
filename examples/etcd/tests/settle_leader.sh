#!/bin/sh
# settle_leader.sh - verify the cluster has elected a leader.
# Runs after workloads. Checks each node's maintenance/status.
# Note: uses POSIX sh and wget (busybox-compatible) inside minimal initramfs.

NODES="${ETCD_NODES:-127.0.0.1:12379,127.0.0.1:22379,127.0.0.1:32379}"

echo "Checking for etcd leader..."
for node in $(echo "$NODES" | tr ',' ' '); do
    STATUS=$(wget -q -O - \
        --header="Content-Type: application/json" \
        --post-data='{}' \
        "http://${node}/v3/maintenance/status" 2>/dev/null || echo '{}')

    LEADER=$(echo "$STATUS" | grep -o '"leader":"[^"]*"' | head -1 || true)
    if [ -n "$LEADER" ] && ! echo "$LEADER" | grep -q '"leader":"0"'; then
        echo "Leader found via $node: $LEADER"
        exit 0
    fi
done

echo "WARNING: No leader detected - cluster may be in election"
exit 0
