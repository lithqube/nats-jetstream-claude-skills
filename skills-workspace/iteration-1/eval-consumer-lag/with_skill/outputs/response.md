# Diagnosing a Growing Num Pending on a JetStream Consumer

When `nats consumer report` shows Num Pending growing despite workers running, the consumer is receiving messages slower than they are being published. The cause is almost always one of four things: MaxAckPending is saturated, AckWait is too short causing redeliveries, fetch batch size is too small, or the workers themselves are too slow. Here is how to work through each possibility systematically.

---

## Step 1: Get a Baseline

```bash
# Overview of all consumers on the stream
nats consumer report ORDERS

# Full details on the specific consumer
nats consumer info ORDERS order-processor

# Watch the numbers change in real time (every 5 seconds)
watch -n5 'nats consumer report ORDERS'
```

Also confirm the stream itself is healthy:

```bash
nats stream info ORDERS
nats server report jetstream
```

---

## Num Pending vs Num Ack Pending — What They Mean

These two counters measure different things and point to different problems:

| Counter | What it counts | What it tells you |
|---|---|---|
| **Num Pending** | Messages in the stream not yet delivered to this consumer | The backlog — how far behind the consumer is |
| **Num Ack Pending** | Messages delivered to workers but not yet acknowledged | How many in-flight messages the consumer is holding |

**Key diagnostic rule:** If `Num Ack Pending` equals `Max Ack Pending`, the consumer is blocked — NATS will not deliver any more messages until workers acknowledge the ones already in flight. This is the single most common cause of growing Num Pending when workers appear to be running.

---

## Step 2: Check if MaxAckPending Is Saturated

```bash
nats consumer info ORDERS order-processor
```

Look at the output:

```
Num Ack Pending:  1000
Max Ack Pending:  1000   # <-- consumer is blocked if these match
Num Pending:      84321  # <-- growing because no new deliveries happen
```

If these two values are equal (or nearly equal), this is your primary bottleneck right now.

---

## Root Causes and Fixes

### Cause 1: MaxAckPending Too Low (Most Common)

**Symptom:** `Num Ack Pending == Max Ack Pending` in the consumer info output.

Workers are processing messages, but the in-flight window is full, so NATS stops delivering more. The backlog grows even though workers are not idle — they are just never given new work fast enough.

**Fix:** Increase MaxAckPending to allow more messages in flight at once.

```bash
nats consumer edit ORDERS order-processor --max-pending 5000
```

Guidelines for sizing:
- Default is 1000, which is often too small for high-throughput consumers
- Fast consumers with small messages: 5000–20000
- Slow consumers or large messages: 100–500
- Match to roughly (worker count) x (fetch batch size)

After the edit, watch whether Num Pending starts shrinking:

```bash
watch -n5 'nats consumer report ORDERS'
```

---

### Cause 2: AckWait Too Short — Messages Are Redelivering

**Symptom:** `Num Redelivered` is high in the consumer info. Num Pending stays high or oscillates.

If `AckWait` is shorter than the time your workers need to process a message, NATS redelivers the message before the worker finishes. This wastes capacity: the worker processes the message, acks it, but NATS has already moved on to a redelivery — wasting slots and causing double-processing.

```bash
# Check AckWait and Num Redelivered
nats consumer info ORDERS order-processor | grep -i -E "ack wait|redelivered"
```

**Fix:** Extend AckWait to exceed your worst-case processing time.

```bash
nats consumer edit ORDERS order-processor --ack-wait 120s
```

Alternatively, have workers send an in-progress signal for long-running jobs. In Go this looks like:

```go
msg.InProgress()  // resets the ack timer, prevents premature redelivery
// ... do the slow work ...
msg.Ack()
```

---

### Cause 3: Fetch Batch Size Too Small

**Symptom:** MaxAckPending is not saturated. Workers are running but spending a lot of time waiting for messages rather than processing them.

Each `Fetch()` call is a round-trip to the NATS server. Fetching one message at a time means workers spend most of their time waiting for network round-trips rather than processing.

**Fix:** Increase the batch size in your worker code.

```go
// Slow: round-trip for every single message
msgs, _ := sub.Fetch(1)

// Better: amortize round-trips over many messages
msgs, _ := sub.Fetch(100, nats.MaxWait(5*time.Second))

// High throughput (simple, fast processing):
msgs, _ := sub.Fetch(500, nats.MaxWait(5*time.Second))
```

Recommended batch sizes by processing time per message:

| Processing Time | Recommended Batch |
|---|---|
| < 1ms | 500–1000 |
| 1–10ms | 100–500 |
| 10–100ms | 10–50 |
| > 100ms | 1–10 |

---

### Cause 4: Not Enough Worker Instances

**Symptom:** MaxAckPending is not saturated, batch sizes are reasonable, but the publish rate is simply higher than what your workers can consume.

Workers are processing as fast as they can individually but there are not enough of them.

**Fix:** Scale out workers. Multiple instances can all call `Fetch()` on the same pull consumer — NATS distributes messages across them.

```go
// Each goroutine (or process) is an independent worker on the same consumer
for i := 0; i < numWorkers; i++ {
    go func() {
        sub, _ := js.PullSubscribe("orders.>", "order-processor")
        for {
            msgs, _ := sub.Fetch(100, nats.MaxWait(5*time.Second))
            for _, msg := range msgs {
                process(msg)
                msg.Ack()
            }
        }
    }()
}
```

Worker count guidelines:
- CPU-bound work: match worker count to CPU cores
- I/O-bound work (database writes, HTTP calls): 2–4x CPU cores

---

### Cause 5: Cluster or Leader Issue

**Symptom:** Num Pending is growing and Num Ack Pending is low — messages are not being delivered at all, not just slowly.

Check if the stream or consumer has lost its cluster leader:

```bash
nats server report jetstream
nats stream info ORDERS --json | jq '.cluster'
```

If a stream shows no leader, check that a majority of replicas are reachable (2 of 3, or 3 of 5). NATS will not serve reads or writes without a quorum.

---

## Step 3: Verify the Fix

After applying changes, confirm Num Pending is shrinking, not just stabilizing:

```bash
# Watch for improvement
watch -n5 'nats consumer report ORDERS'

# Confirm consumer config was applied
nats consumer info ORDERS order-processor
```

You should see Num Pending decrease at a faster rate than messages are being published. If it is decreasing slowly, combine fixes (for example, increase both MaxAckPending and worker count together).

---

## Step 4: Prevent Recurrence

Set up monitoring so you catch this before it becomes an hour-long backlog:

```bash
# The NATS HTTP endpoint exposes consumer pending counts
curl http://localhost:8222/jsz?consumers=true | jq '.account_details[].stream_detail[].consumer_detail[] | {name: .name, num_pending: .num_pending, num_ack_pending: .num_ack_pending}'
```

For Prometheus alerting, add a rule that fires when Num Pending exceeds a threshold for more than a few minutes:

```yaml
- alert: JetStreamConsumerLag
  expr: nats_consumer_num_pending{consumer="order-processor"} > 10000
  for: 5m
  annotations:
    summary: "Consumer order-processor is falling behind ({{ $value }} pending)"
```

Subscribe to JetStream advisories to get real-time operational events including slow consumer signals:

```bash
nats sub '$JS.EVENT.ADVISORY.>'
```

---

## Quick Reference: Most Likely Causes by Symptom

| What you see | Most likely cause | First fix |
|---|---|---|
| Num Ack Pending == Max Ack Pending | MaxAckPending too low | `nats consumer edit ... --max-pending 5000` |
| Num Redelivered is high | AckWait too short | `nats consumer edit ... --ack-wait 120s` |
| Num Ack Pending is low, Num Pending growing | Not enough workers or batch too small | Scale workers or increase fetch batch |
| Num Pending growing, no deliveries at all | Cluster leader missing | `nats server report jetstream` |
