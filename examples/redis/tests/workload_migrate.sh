#!/bin/sh

# Runs in parallel.
# A separate set-get pass under concurrent fault injection; migration itself is
# triggered by the orchestrator's fault injector (slot resharding is not scripted here
# because redis-cli --cluster reshard requires interactive input without --cluster-yes
# support for incremental migration). This script exercises the MOVED/ASK redirect
# path that naturally fires during any in-progress migration the orchestrator induces.

exec /opt/openthesis/bin/redis-driver set-get
