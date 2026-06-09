#!/bin/sh
# setup_wait_cluster.sh - wait for all 6 Redis nodes, then form the cluster.
# Runs once before any driver scripts.
# Uses POSIX sh and redis-cli (busybox-compatible environment).

NODES="${REDIS_NODES:-127.0.0.1:7001,127.0.0.1:7002,127.0.0.1:7003,127.0.0.1:7004,127.0.0.1:7005,127.0.0.1:7006}"
REDIS_CLI=/opt/openthesis/bin/redis-cli

echo "=== Waiting for all Redis nodes to accept connections ==="
for node in $(echo "$NODES" | tr ',' ' '); do
    host=$(echo "$node" | cut -d: -f1)
    port=$(echo "$node" | cut -d: -f2)
    i=0
    while [ $i -lt 60 ]; do
        if $REDIS_CLI -h "$host" -p "$port" PING 2>/dev/null | grep -q "PONG"; then
            echo "  $node: PONG"
            break
        fi
        sleep 1
        i=$((i+1))
    done
    if [ $i -eq 60 ]; then
        echo "  WARNING: $node did not respond within 60s"
    fi
done

echo ""
echo "=== Forming Redis Cluster ==="
# redis-cli --cluster create expects all 6 addrs; --cluster-replicas 1 = 3 master + 3 replica
$REDIS_CLI --cluster create \
    127.0.0.1:7001 \
    127.0.0.1:7002 \
    127.0.0.1:7003 \
    127.0.0.1:7004 \
    127.0.0.1:7005 \
    127.0.0.1:7006 \
    --cluster-replicas 1 \
    --cluster-yes 2>&1 || {
        echo "  WARNING: cluster create returned non-zero (may already be formed)"
    }

echo ""
echo "=== Waiting for cluster_state:ok ==="
i=0
while [ $i -lt 30 ]; do
    INFO=$($REDIS_CLI -h 127.0.0.1 -p 7001 CLUSTER INFO 2>/dev/null || echo "")
    STATE=$(echo "$INFO" | grep "^cluster_state:" | tr -d '\r' | cut -d: -f2)
    SLOTS=$(echo "$INFO" | grep "^cluster_slots_assigned:" | tr -d '\r' | cut -d: -f2)
    if [ "$STATE" = "ok" ] && [ "$SLOTS" = "16384" ]; then
        echo "  cluster_state:ok, 16384 slots assigned"
        break
    fi
    echo "  state=${STATE:-unknown} slots=${SLOTS:-0} - waiting..."
    sleep 2
    i=$((i+1))
done

if [ $i -eq 30 ]; then
    echo "  WARNING: cluster did not reach ok state within 60s"
fi

echo ""
echo "=== Redis Cluster topology ==="
$REDIS_CLI -h 127.0.0.1 -p 7001 CLUSTER NODES 2>/dev/null || true

echo "=== Redis Cluster ready ==="
