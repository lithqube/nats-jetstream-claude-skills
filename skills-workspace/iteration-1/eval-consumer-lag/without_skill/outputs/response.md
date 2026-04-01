# Diagnosing and Fixing JetStream Consumer Lag

## Quick Summary

When `nats consumer report` shows Num Pending growing continuously, your consumer is receiving messages slower than they are being published. This is a throughput mismatch. Work through the diagnostic commands below in order, then apply the appropriate fix.

---

## Step 1: Establish Baseline Metrics

Run these commands first to understand the current state.

### Consumer report (continuous watch)
```bash
nats consumer report <stream> <consumer> --watch
```
Observe whether Num Pending is growing, stable, or shrinking. Also note:
- **Num Ack Pending** — messages delivered but not yet acknowledged
- **Num Redelivered** — messages being retried (indicates ack failures)
- **Num Waiting** — fetch requests waiting (pull consumers only)

### Stream info
```bash
nats stream info <stream>
```
Note the current message count, subject filters, and retention policy.

### Consumer info (detailed)
```bash
nats consumer info <stream> <consumer>
```
Key fields to inspect:
- `Ack Wait` — how long before unacked messages are redelivered
- `Max Ack Pending` — the in-flight cap; if workers hit this, delivery stalls
- `Max Deliver` — max redelivery attempts before a message is skipped
- `Filter Subject` — confirm it matches what your workers expect

---

## Step 2: Check Delivery Stalls

### Check if Max Ack Pending is blocking delivery
```bash
nats consumer info <stream> <consumer> | grep -E "Ack Pending|Max Ack Pending"
```
If `Num Ack Pending` equals `Max Ack Pending`, JetStream has paused delivery. No new messages will be pushed/fetched until workers ack existing ones.

### Check for redelivery storms
```bash
nats consumer report <stream> <consumer>
```
If `Num Redelivered` is high and growing, your workers are receiving messages but failing to ack them before `Ack Wait` expires. This consumes worker capacity with retries rather than new work.

### Check for stalled push consumers (push-based only)
```bash
nats consumer ls <stream>
```
Look for consumers with no active subscriptions. A push consumer with no subscribers will accumulate pending messages indefinitely.

---

## Step 3: Measure Worker Throughput

### Subscribe and measure delivery rate (test consumer)
```bash
nats sub --count=1000 --context <your-context> <subject>
```

### Check server metrics for the consumer
```bash
nats server report jetstream
```
Look at per-server message rates to confirm NATS server itself is not the bottleneck.

### Check server resource usage
```bash
nats server info
```
```bash
nats server report connections
```

---

## Step 4: Inspect the Stream for Backlog Size

```bash
nats stream info <stream> --json | jq '{messages: .state.messages, bytes: .state.bytes, first_seq: .state.first_seq, last_seq: .state.last_seq}'
```
Calculate the lag depth: `last_seq - consumer_delivered_seq`. If this is in the millions, catching up will take time even after the fix is applied.

---

## Step 5: Check NATS Server Health

```bash
nats server check jetstream
```
```bash
nats server check stream --stream <stream>
```
```bash
nats server check consumer --stream <stream> --consumer <consumer>
```
These commands return explicit WARN/CRIT/OK statuses and are the fastest way to spot server-side issues (storage pressure, cluster split, leader election).

---

## Most Likely Root Causes

### 1. Max Ack Pending Cap Reached (Most Common)

**Symptom:** `Num Ack Pending == Max Ack Pending`, delivery has stopped entirely, workers appear idle.

**Cause:** Workers are taking too long to process and ack messages, or they are crashing before acking. Once the in-flight cap is hit, JetStream stops delivering new messages.

**Fix:**
```bash
# Increase the cap to unblock delivery immediately
nats consumer edit <stream> <consumer> --max-pending 10000

# Also extend ack wait if processing is legitimately slow
nats consumer edit <stream> <consumer> --ack-wait 120s
```
Long-term: fix the workers to ack faster, or scale out more worker instances.

---

### 2. Ack Wait Too Short — Redelivery Storm

**Symptom:** `Num Redelivered` is large and growing. Workers seem busy but Num Pending still grows.

**Cause:** Workers receive a message, begin processing, but take longer than `Ack Wait`. JetStream redelivers the message to another worker (or the same one), doubling the work. This cascades.

**Fix:**
```bash
# Set ack wait to comfortably exceed your worst-case processing time
nats consumer edit <stream> <consumer> --ack-wait 300s
```
In your worker code, use in-progress acking (a "working" signal) to reset the ack timer mid-processing:
```go
// NATS Go client example
msg.InProgress()  // resets ack wait timer, call periodically during long processing
```

---

### 3. Too Few Workers / Insufficient Parallelism

**Symptom:** Num Pending grows steadily, no redelivery storm, ack pending is well below the cap. Workers are simply too slow.

**Fix options:**
- Scale out: add more worker processes or pods
- For pull consumers, increase the batch size and fetch concurrency:
  ```bash
  # In code — fetch more messages per pull request
  msgs, err := sub.Fetch(100, nats.MaxWait(5*time.Second))
  ```
- For push consumers, increase `--max-pending` and add subscriber instances on the same queue group

---

### 4. Push Consumer with No Active Subscribers

**Symptom:** Num Pending grows but no workers appear to be receiving anything. Consumer has a delivery subject but no active subscriptions.

**Cause:** Workers crashed or were not started, or the subject filter/delivery subject has a typo.

**Fix:**
```bash
# Verify active subscription count
nats consumer info <stream> <consumer> | grep "Active"

# Restart workers; confirm they subscribe to the correct delivery subject
# Check subject matches exactly — NATS subjects are case-sensitive
```

---

### 5. Pull Consumer — Workers Not Fetching Aggressively Enough

**Symptom:** `Num Waiting` is 0 or very low for a pull consumer. Workers are not issuing fetch requests fast enough.

**Fix:** Tune the fetch loop in your worker:
```go
// Increase batch size and reduce wait time between fetches
msgs, _ := sub.Fetch(500, nats.MaxWait(2*time.Second))
```
Or use the newer `nats.Subscribe` with `ConsumeContext` (NATS.go v2) which handles the fetch loop automatically.

---

### 6. Single-Threaded or Blocking Worker Logic

**Symptom:** Workers are running but processing serially. Each message blocks the next.

**Fix:** Process messages concurrently in your worker:
```go
// Process in goroutines; ack after processing is complete
go func(msg *nats.Msg) {
    process(msg)
    msg.Ack()
}(msg)
```
Be careful not to exceed `Max Ack Pending` with unbounded goroutines — use a semaphore.

---

### 7. NATS Server or Storage Under Pressure

**Symptom:** `nats server check jetstream` returns WARN or CRIT. Slow ack rates server-wide.

**Cause:** Disk I/O saturation (file storage), memory pressure, or cluster leader issues.

**Fix:**
```bash
# Check JetStream storage usage
nats server report jetstream

# If a cluster, check who the stream leader is
nats stream info <stream> --json | jq '.cluster'

# Force leader election if a node is degraded
nats stream leader-stepdown <stream>
```

---

## Remediation Priority Order

1. Run `nats consumer info` and `nats server check jetstream` first.
2. If `Num Ack Pending == Max Ack Pending` → increase `--max-pending` and `--ack-wait` immediately.
3. If `Num Redelivered` is high → increase `--ack-wait` and add in-progress acking to workers.
4. If metrics look healthy but pending grows → scale out workers or increase fetch concurrency.
5. If server checks fail → address infrastructure issues (disk, memory, cluster health).

---

## Reference: Key Consumer Edit Commands

```bash
# Extend ack wait
nats consumer edit <stream> <consumer> --ack-wait 300s

# Increase max ack pending (in-flight cap)
nats consumer edit <stream> <consumer> --max-pending 10000

# Increase max deliver (retry limit)
nats consumer edit <stream> <consumer> --max-deliver 10

# View all editable fields
nats consumer edit --help
```

> Note: Some consumer configuration fields (like deliver policy and filter subject) cannot be changed on an existing consumer. You may need to delete and recreate the consumer if fundamental settings need to change. Ensure your workers handle the restart gracefully.
