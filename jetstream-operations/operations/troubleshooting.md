# Troubleshooting

## Messages Not Delivering

### Symptom: Consumer receives no messages

**Check 1: Subject filter mismatch**

```bash
# See what subjects the stream captures
nats stream info ORDERS

# See what the consumer filters
nats consumer info ORDERS order-processor
```

Common mistake: stream captures `orders.>` but consumer filters `order.>` (missing 's').

**Check 2: Consumer is paused or has no pending**

```bash
nats consumer info ORDERS order-processor
```

Look at:
- `Num Pending` — messages waiting to be delivered. If 0, no new messages match the filter.
- `Num Ack Pending` — messages delivered but not acked. If equal to `MaxAckPending`, consumer is blocked.
- `Num Redelivered` — high count means messages are failing processing.

**Check 3: All messages hit MaxDeliver**

```bash
nats consumer info ORDERS order-processor
```

If `Num Pending: 0` and `Ack Floor` equals stream last sequence, all messages have been processed or exhausted retries. Check dead letter queue or advisory subjects.

**Check 4: Stream has no messages**

```bash
nats stream info ORDERS
```

If `Messages: 0`, either nothing was published or retention policy removed messages.

### Symptom: Messages delivered but not processing

**Check ack timeout**: If `AckWait` is too short for processing time, messages get redelivered before the worker finishes.

```bash
# Current ack wait
nats consumer info ORDERS order-processor | grep -i ack

# Fix: increase AckWait or use InProgress() in code
nats consumer edit ORDERS order-processor --ack-wait=60s
```

**Check MaxAckPending**: If all `MaxAckPending` slots are used, no new messages are delivered.

```bash
# If Num Ack Pending == Max Ack Pending, consumer is blocked
nats consumer info ORDERS order-processor
```

Fix: increase `MaxAckPending` or fix slow consumers.

## Consumer Lag

### Diagnosis

```bash
# Quick overview of all consumers
nats consumer report ORDERS

# Output shows:
# Consumer  Num Pending  Num Ack Pending  Last Delivered  Ack Floor
```

**Num Pending** = messages in stream not yet delivered to consumer
**Num Ack Pending** = messages delivered but awaiting ack

### Root Causes and Fixes

**Slow consumer processing**
- Increase worker count (add more instances calling Fetch())
- Increase fetch batch size
- Optimize processing logic (database queries, external calls)

**MaxAckPending too low**
```bash
nats consumer edit ORDERS order-processor --max-pending=5000
```

**Fetch batch too small**
```go
// Instead of
msgs, _ := sub.Fetch(1)
// Use
msgs, _ := sub.Fetch(100, nats.MaxWait(5*time.Second))
```

**AckWait too short causing redeliveries**
```bash
nats consumer edit ORDERS order-processor --ack-wait=120s
```

## Stream Full

### Symptom: Publish returns error

```
nats: maximum bytes exceeded
nats: maximum messages exceeded
```

### Diagnosis

```bash
nats stream info ORDERS

# Look at:
# Config: MaxBytes, MaxMsgs, MaxAge, Discard
# State: Messages, Bytes, First Seq, Last Seq
```

### Fixes

**If using DiscardNew** (backpressure mode):
- Increase stream limits: `nats stream edit ORDERS --max-bytes=10G`
- Speed up consumers so messages get removed faster (WorkQueuePolicy)
- Reduce retention: `nats stream edit ORDERS --max-age=7d`

**If using DiscardOld** (default):
- Messages shouldn't be rejected — old ones are auto-removed
- If still seeing errors, check `MaxMsgsPerSubject` limit

**Emergency: purge stale data**
```bash
# Purge all messages
nats stream purge ORDERS

# Purge messages on specific subject
nats stream purge ORDERS --subject="orders.cancelled"

# Purge messages older than a sequence
nats stream purge ORDERS --seq=1000000
```

## Cluster Issues

### Leader Election Problems

```bash
# Check cluster state
nats server report jetstream

# Check stream leader
nats stream info ORDERS --json | jq '.cluster'
```

If a stream shows no leader:
- Check if enough replicas are online (need majority: 2/3 or 3/5)
- Check cluster routes: `curl http://localhost:8222/routez`
- Check server logs for "JetStream cluster peer" errors

### Split Brain

Symptoms: different nodes report different stream states.

```bash
# Compare stream state across nodes
nats stream info ORDERS --server=nats://node-1:4222
nats stream info ORDERS --server=nats://node-2:4222
nats stream info ORDERS --server=nats://node-3:4222
```

Fix: Usually self-heals when network connectivity is restored. If not, the minority partition's state is discarded.

### R1 Streams Losing Data on Restart

R1 (single replica) streams have no redundancy. If the node restarts, data in memory streams is lost.

Fix: Use `Replicas: 3` for production streams. R1 is for development only.

## Client Disconnections

### Diagnosis

```bash
# Check client connections
curl http://localhost:8222/connz?subs=true | jq '.connections | length'

# Check for slow consumers being dropped
curl http://localhost:8222/connz | jq '.connections[] | select(.slow_consumer == true)'
```

### Slow Consumer Drops

NATS drops connections that can't keep up. Signs in logs:
```
Slow Consumer Detected
```

Fixes:
- Use JetStream (not core NATS) for guaranteed delivery
- Increase client pending limits: `nats.PendingLimits(100000, 100*1024*1024)`
- Process messages faster or add more consumers

### Reconnection Strategy

Ensure clients handle reconnection properly:

```go
nc, _ := nats.Connect(url,
    nats.MaxReconnects(-1),           // unlimited
    nats.ReconnectWait(2*time.Second),
    nats.ReconnectBufSize(50*1024*1024), // 50MB buffer during reconnect
    nats.RetryOnFailedConnect(true),
)
```

After reconnection, JetStream pull consumers resume automatically on next `Fetch()`. Push consumers resubscribe automatically if using durable names.

## Common nats CLI Diagnostic Commands

```bash
# Server health
nats server check jetstream
nats server report jetstream
nats server report connections

# Stream inspection
nats stream ls
nats stream info ORDERS
nats stream report
nats stream view ORDERS           # view recent messages (careful in production)

# Consumer inspection
nats consumer ls ORDERS
nats consumer info ORDERS order-processor
nats consumer report ORDERS
nats consumer next ORDERS order-processor  # manually fetch next message

# Publish/subscribe testing
nats pub orders.test "hello"
nats sub "orders.>"

# Account info
nats account info
```
