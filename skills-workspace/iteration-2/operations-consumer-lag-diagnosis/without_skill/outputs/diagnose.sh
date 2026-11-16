#!/usr/bin/env bash
# diagnose.sh — one-shot triage for a JetStream consumer falling behind.
# Usage: ./diagnose.sh STREAM CONSUMER
# Reads two snapshots 10s apart and tells you whether you're throughput-bound
# or in a redelivery storm. Read-only: it does not ack, edit, or delete anything.
set -euo pipefail

STREAM="${1:?usage: diagnose.sh STREAM CONSUMER}"
CONSUMER="${2:?usage: diagnose.sh STREAM CONSUMER}"

fields() {
  nats consumer info "$STREAM" "$CONSUMER" \
    | grep -E 'Ack Wait|Max Ack Pending|Max Deliver|Acknowledgment Policy|Num Pending|Num Ack Pending|Num Redelivered|Acknowledgment floor|Last Delivered' \
    || true
}

num() { # pull the first integer out of a labeled line, else 0
  nats consumer info "$STREAM" "$CONSUMER" | awk -F: "/$1/ {gsub(/[^0-9]/,\"\",\$2); print \$2; exit}"
}

echo "=== consumer config + state (snapshot 1) ==="
fields

r1="$(num 'Num Redelivered')"; r1="${r1:-0}"
echo
echo "sampling redelivery rate over 10s..."
sleep 10
r2="$(num 'Num Redelivered')"; r2="${r2:-0}"

echo
echo "=== verdict ==="
delta=$(( r2 - r1 ))
echo "Num Redelivered: ${r1} -> ${r2} (delta ${delta} over 10s)"
if [ "$delta" -gt 0 ]; then
  echo "REDELIVERY STORM (scenario 2): acks are timing out before completion."
  echo "  -> Raise AckWait, send in-progress acks for long jobs, and TERM the poison message."
  echo "  -> Find it:  nats consumer next $STREAM $CONSUMER --count=5 --no-ack"
else
  echo "THROUGHPUT-BOUND (scenario 1): consumer is acking but too slow."
  echo "  -> Add more worker instances first; raise --max-pending only if workers can use it;"
  echo "     then look at the downstream dependency (DB/API) per message."
fi
echo
echo "Watch recovery:  watch -n 5 \"nats consumer report $STREAM\""
