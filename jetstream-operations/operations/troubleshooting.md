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

## Consumer Won't Start After a Config Change

### Symptom: deploy fails, or the consumer silently stops receiving

```
consumer name already in use with different configuration
# or, more cryptically:
configuration requests deliver policy to be 2, but consumer's value is 0
```

A durable consumer's core config — `AckPolicy`, `DeliverPolicy`, `FilterSubject(s)`, `ack_wait` in some client versions — is **immutable server-side state**. When your code (or IaC) creates a durable that already exists with different settings, JetStream rejects the create. The old durable keeps holding the subject and its `Num Pending` grows unbounded while nothing consumes — so this reads like "consumer fell behind" but is really "consumer never bound."

This is one of the most common recurring production failures. It bites especially hard with a wrapper that *binds* an existing durable rather than reconciling it: the app's declared filter/ack settings are never pushed, so config and broker drift silently until the day someone changes both.

**Fix — delete and recreate** (there is no in-place change for these fields):

```bash
nats consumer rm ORDERS order-processor -f   # note: -f, the CLI does not accept --force here
# then redeploy / re-run your provisioner so the durable is recreated with the new config
```

**Prevent it:** keep durable config in one authoritative place (an idempotent `streams.sh` or Helm), and add an integration test against a real broker that asserts the live consumer's config equals the declared one. Config that lives in two places (app config + provisioner) drifts invisibly.

### Related: "must use pull subscribe to bind to pull based consumer"

You get this when two things disagree about the consumer *type*. A common cause: one component (e.g. a connector like Benthos/Bento) auto-creates the durable as a **push** consumer, and another binds it with `pull_subscribe`. Pick one type per durable; if a connector owns a durable, don't also provision it as pull.

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

### Symptom: works in staging, crashes connecting to the production cluster

```
nats: Port could not be cast to integer value as '4222,nats-2:4222,nats-3'
```

The client is being handed **all cluster nodes as one comma-joined string in a single list slot** instead of one entry per node. A single-node staging environment has only one URL, so the bug is invisible there and only appears against the multi-node production cluster — a nasty "worked in staging" failure. Split the server list into separate entries:

```go
// WRONG: one element containing commas
nats.Connect("nats://nats-1:4222,nats-2:4222,nats-3:4222") // ok — Connect parses this string

// but in code that builds a LIST (e.g. from an env var), split it:
servers := strings.Split(os.Getenv("NATS_SERVERS"), ",")   // []string{"nats://nats-1:4222", ...}
nats.Connect(strings.Join(servers, ","))
```

In clients that take an array (nats-py `servers=[...]`, the JS client's `servers`), pass a real array of one URL per node — never a single element with commas in it. **Make staging a (small) cluster too**, so this surfaces before production.

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
