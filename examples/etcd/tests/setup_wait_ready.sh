#!/bin/sh
# setup_wait_ready.sh - wait for all 3 etcd nodes to be healthy.
# Runs once before any driver scripts.
# Note: uses POSIX sh and wget (busybox-compatible) inside minimal initramfs.

NODES="${ETCD_NODES:-127.0.0.1:12379,127.0.0.1:22379,127.0.0.1:32379}"

echo "Waiting for etcd nodes to be healthy..."
for node in $(echo "$NODES" | tr ',' ' '); do
    i=0
    while [ $i -lt 30 ]; do
        STATUS=$(wget -q -O - "http://${node}/health" 2>/dev/null || echo '{}')
        if echo "$STATUS" | grep -q '"health":"true"'; then
            echo "  $node healthy"
            break
        fi
        sleep 1
        i=$((i+1))
    done
    if [ $i -eq 30 ]; then
        echo "  WARNING: $node did not become healthy within 30s"
    fi
done

echo "etcd cluster ready check complete"
