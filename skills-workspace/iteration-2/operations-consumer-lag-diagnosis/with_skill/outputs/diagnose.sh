#!/usr/bin/env bash
# Read-only JetStream consumer-lag diagnosis. Runs the Step 1-3 inspection
# from response.md; changes nothing. Usage: ./diagnose.sh STREAM CONSUMER
set -euo pipefail

STREAM="${1:?usage: diagnose.sh STREAM CONSUMER}"
CONSUMER="${2:?usage: diagnose.sh STREAM CONSUMER}"

echo "== consumer report =="
nats consumer report "$STREAM"

echo; echo "== consumer info: the four fields that decide the diagnosis =="
# Num Ack Pending == Max Ack Pending -> blocked; Num Redelivered splits cause A vs B
nats consumer info "$STREAM" "$CONSUMER" \
  | grep -iE 'ack pending|max ack|num pending|ack floor|redelivered|ack wait|max deliver' || true

echo; echo "== stream backlog =="
nats stream info "$STREAM" | grep -iE 'messages|last seq' || true

echo; echo "== cluster health (only matters if consumer looks healthy but delivery still stalled) =="
nats server report jetstream || true

cat <<'EOF'

Next:
  Num Ack Pending == Max Ack Pending   -> consumer is BLOCKED, not merely slow
  Num Redelivered climbing             -> cause A: AckWait too short / poison msg  (response.md Step 2)
  Num Redelivered flat, Ack Floor stuck-> cause B: worker down or durable unbound  (response.md Step 3)
EOF
