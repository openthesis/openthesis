#!/bin/sh
# verify_snapshot.sh - compact + defrag the cluster, verify DB size.
# Runs last after all workloads.

NODES="${ETCD_NODES:-127.0.0.1:12379,127.0.0.1:22379,127.0.0.1:32379}"

echo "=== Pre-snapshot cluster state ==="
for node in $(echo "$NODES" | tr ',' ' '); do
    echo "--- $node ---"
    wget -q -O - \
        --header="Content-Type: application/json" \
        --post-data='{}' \
        "http://${node}/v3/maintenance/status" 2>/dev/null || echo "  unreachable"
done

echo ""
echo "=== Running compact + defrag via driver ==="
/opt/openthesis/bin/etcd-driver snapshot

echo ""
echo "=== Post-snapshot cluster state ==="
for node in $(echo "$NODES" | tr ',' ' '); do
    echo "--- $node ---"
    wget -q -O - \
        --header="Content-Type: application/json" \
        --post-data='{}' \
        "http://${node}/v3/maintenance/status" 2>/dev/null || echo "  unreachable"
done

echo "=== Snapshot check complete ==="
