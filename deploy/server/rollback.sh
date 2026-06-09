#!/usr/bin/env bash
#
# Usage: ./deploy/rollback.sh <USER>@<IP>
#
set -euo pipefail

if [[ $# -lt 1 ]]; then
    echo "Usage: $0 <USER>@<IP>"
    exit 1
fi

TARGET="$1"
IP="$(echo "$TARGET" | cut -d@ -f2)"

echo "==> Rolling back openthesis on $IP"

ssh "$TARGET" bash <<'REMOTE'
set -euo pipefail

PREV="${HOME}/.openthesis/bin/openthesis.prev"
CURRENT="${HOME}/.openthesis/bin/openthesis"

if [[ ! -f "$PREV" ]]; then
    echo "!!! No previous binary found at $PREV"
    exit 1
fi

mv "$CURRENT" "${CURRENT}.bad"
mv "$PREV" "$CURRENT"
mv "${CURRENT}.bad" "$PREV"

echo "--- Rollback complete ---"
"$HOME/.openthesis/bin/openthesis" --version 2>/dev/null || true

REMOTE

echo "==> Rollback complete"
