#!/bin/sh
# settle_converge.sh - verify the cluster converged after all workloads.
# Runs after workloads.
# Checks cluster_state:ok on every node and confirms 16384 slots are assigned.

NODES="${REDIS_NODES:-127.0.0.1:7001,127.0.0.1:7002,127.0.0.1:7003,127.0.0.1:7004,127.0.0.1:7005,127.0.0.1:7006}"
REDIS_CLI=/opt/openthesis/bin/redis-cli

echo "=== Checking cluster convergence on all nodes ==="
all_ok=1
for node in $(echo "$NODES" | tr ',' ' '); do
    host=$(echo "$node" | cut -d: -f1)
    port=$(echo "$node" | cut -d: -f2)

    INFO=$($REDIS_CLI -h "$host" -p "$port" CLUSTER INFO 2>/dev/null || echo "")
    if [ -z "$INFO" ]; then
        echo "  $node: unreachable"
        all_ok=0
        continue
    fi

    STATE=$(echo "$INFO" | grep "^cluster_state:" | tr -d '\r' | cut -d: -f2)
    SLOTS=$(echo "$INFO" | grep "^cluster_slots_assigned:" | tr -d '\r' | cut -d: -f2)
    SIZE=$(echo "$INFO" | grep "^cluster_size:" | tr -d '\r' | cut -d: -f2)
    KNOWN=$(echo "$INFO" | grep "^cluster_known_nodes:" | tr -d '\r' | cut -d: -f2)

    echo "  $node: state=${STATE} slots=${SLOTS} size=${SIZE} known_nodes=${KNOWN}"

    if [ "$STATE" != "ok" ] || [ "$SLOTS" != "16384" ]; then
        all_ok=0
    fi
done

echo ""
echo "=== Running final check via driver ==="
/opt/openthesis/bin/redis-driver check

echo ""
if [ "$all_ok" = "1" ]; then
    echo "=== Cluster converged: all nodes ok, 16384 slots assigned ==="
else
    echo "=== WARNING: some nodes degraded - check assertions above ==="
fi
