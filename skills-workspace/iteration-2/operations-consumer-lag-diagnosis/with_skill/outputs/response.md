# Diagnosing a JetStream consumer that's falling behind

Your symptom — **Num Pending climbing** while **Num Ack Pending is pinned at its max** — is the single most diagnostic signal JetStream gives you. It almost never means "the stream is too fast." It means your consumer is **blocked**: every in-flight slot is full of messages that were delivered but never acked, so JetStream refuses to deliver anything new. Pending climbs because delivery has stopped, not because processing merely slowed.

So the real question is **not** "how do I make it go faster" — it's **"why have acks stopped flowing?"** Bumping `MaxAckPending` without answering that just gives you a bigger bucket that fills up in a few more minutes.

Below is the walkthrough: confirm the state, find the root cause, apply the matching fix, verify, then add an alert so it pages you before it pins next time.

Replace `ORDERS` / `order-processor` with your stream and consumer throughout. A one-shot helper that runs the read-only diagnosis steps is in `diagnose.sh`.

---

## Step 1 — Confirm the state (read-only)

```bash
nats consumer report ORDERS
nats consumer info ORDERS order-processor
```

From `consumer info`, read these four fields together — they tell the whole story:

| Field | What you're seeing | What it means |
|---|---|---|
| `Num Ack Pending` **==** `Max Ack Pending` | in-flight slots full | **consumer is blocked** — no new delivery until acks come in |
| `Num Pending` climbing | backlog in the stream growing | nothing is draining |
| `Num Redelivered` high / climbing | same messages delivered again and again | acks are timing out → processing is stuck or `AckWait` too short |
| `Ack Floor` not advancing | the "everything below here is acked" marker frozen | confirms acks have stalled, not just slowed |

The deciding split is **`Num Redelivered`**:

- **Redeliveries climbing** → messages are being handed out, the worker takes too long (or dies) before acking, `AckWait` fires, they get redelivered, and the slots never free. This is a **stuck/slow/crashing worker** or **`AckWait` too short**. → Step 2.
- **Redeliveries flat, Ack Floor frozen, Num Delivered not moving** → the worker isn't fetching/acking at all. The process may be down, deadlocked, disconnected, or bound to the wrong consumer. → Step 3.

Note the current `Max Ack Pending`, `AckWait`, and `Num Pending` values now so you can prove the fix worked in Step 6.

---

## Step 2 — Root cause A: acks time out (redeliveries climbing)

### 2a. Is `AckWait` shorter than real processing time?

```bash
nats consumer info ORDERS order-processor | grep -i ack
```

If a message takes, say, 45s to process (slow DB write, external API) but `AckWait` is 30s, JetStream redelivers it before the worker finishes. The worker then re-does work it already started, falls further behind, and every slot churns on redeliveries — classic pin.

**Fix — give processing enough time:**

```bash
nats consumer edit ORDERS order-processor --ack-wait=2m
```

And in long-running handlers, send `InProgress` to reset the timer instead of guessing one big number:

```go
msg.InProgress()   // heartbeat: "still working, don't redeliver"
// ... long work ...
msg.Ack()
```

### 2b. Is one poison message burning every slot?

If the same few sequences dominate `Num Redelivered`, a message that always fails processing is being retried forever, holding its slot each time.

```bash
# Find how many times things are being redelivered and cap it
nats consumer info ORDERS order-processor | grep -i deliver
```

If `Max Deliver` is unlimited (`-1`), a permanently-failing message never leaves the slot. Cap it and route failures aside so they stop occupying in-flight capacity:

```bash
nats consumer edit ORDERS order-processor --max-deliver=5
```

With a delivery cap, exhausted messages are dropped from the consumer and a `MAX_DELIVERIES` advisory is published. **Advisories are fire-and-forget** — if you can't afford to lose those, consume them durably (see Step 7) rather than relying on a bare subscription. In code, `msg.Term()` a message you know will never succeed to free its slot immediately instead of waiting out the retries.

### 2c. Processing genuinely too slow for the arrival rate

If acks are flowing but just slower than publishes, you're throughput-bound, not blocked-bound — but a too-small `MaxAckPending` makes it worse by under-feeding the workers. Address the throughput (Step 4) and size `MaxAckPending` to match (Step 5).

---

## Step 3 — Root cause B: the worker isn't acking at all (redeliveries flat)

If `Num Ack Pending` is pinned but `Num Redelivered` is **not** climbing and `Ack Floor` is frozen, the messages were delivered once and nothing is happening to them. The consumer is bound but the worker side is dead or absent.

**Check it's actually a worker problem and not a config/binding problem:**

```bash
# Is anything pulling? Try fetching one yourself.
nats consumer next ORDERS order-processor --count=1
```

- If **you** get a message instantly, the consumer is healthy and has work — your application workers are the problem: process down, deadlocked, stuck on a blocking call (DB connection pool exhausted, external call with no timeout), or crash-looping after fetch but before ack. Check app logs and liveness; restart/scale the workers. Messages already delivered will redeliver after `AckWait` once a live worker is back.
- If `consumer next` **hangs with `Num Pending > 0`**, deliveries really are blocked — you're back to Step 2 (all slots full) or the server/cluster side (Step 8).

**Also rule out a silent binding failure** — this reads exactly like "fell behind" but is really "never bound":

```bash
nats consumer info ORDERS order-processor
```

A durable's core config (`AckPolicy`, `DeliverPolicy`, `FilterSubject`) is immutable server-side. If a deploy tried to (re)create the durable with different settings, JetStream rejects it, the **old** durable keeps holding the subject, and its `Num Pending` grows unbounded while your new code never attaches. If this started right after a deploy, suspect this first. Fix is delete + recreate:

```bash
nats consumer rm ORDERS order-processor -f    # -f, not --force
# then redeploy / re-run your provisioner to recreate with the intended config
```

(Related: `must use pull subscribe to bind to pull based consumer` means two components disagree on push vs pull type — pick one owner per durable.)

---

## Step 4 — Speed up draining (once root cause is addressed)

If the consumer is healthy but genuinely under-provisioned for the load:

**Bigger fetch batches** — the highest-impact knob; fewer round-trips per message:

```go
// Slow: one at a time
msgs, _ := sub.Fetch(1)
// Better:
msgs, _ := sub.Fetch(100, nats.MaxWait(5*time.Second))
```

Batch size by per-message cost: `<1ms` → 500–1000; `1–10ms` (DB writes) → 100–500; `10–100ms` (API calls) → 10–50; `>100ms` → 1–10.

**More parallel workers** on the same pull consumer (horizontal scale):

```go
for i := 0; i < numWorkers; i++ {
    go func() {
        sub, _ := js.PullSubscribe("orders.>", "order-processor")
        for {
            msgs, _ := sub.Fetch(100, nats.MaxWait(5*time.Second))
            for _, m := range msgs { process(m); m.Ack() }
        }
    }()
}
```

Worker count: CPU-bound → ~= cores; I/O-bound (DB/HTTP) → 2–4× cores.

---

## Step 5 — Resize MaxAckPending (last, not first)

Only after Steps 2–4: if acks now flow and workers are keeping up, a too-low `MaxAckPending` caps throughput by starving them of in-flight work.

```bash
nats consumer edit ORDERS order-processor --max-pending=5000
```

Sizing: default 1000; fast consumers, small messages → 5000–20000; slow consumers or large messages → keep it **low** (100–500) so a failure doesn't leave thousands of messages in limbo. A single-threaded consumer should match its fetch batch size.

> Raising `MaxAckPending` while the real problem is Step 2 or 3 just delays the next pin and increases redelivery blast radius when it does. Fix the ack path first.

---

## Step 6 — Verify the fix

Watch the two numbers that were wrong actually recover:

```bash
# Re-run every couple of seconds and watch the trend
watch -n 2 'nats consumer report ORDERS'
```

You want to see, over a minute or two:

- `Num Ack Pending` drop **below** `Max Ack Pending` (slots freeing)
- `Ack Floor` **advancing** (acks landing)
- `Num Pending` **falling** (backlog draining)
- `Num Redelivered` flattening (no more ack timeouts)

Cross-check the backlog is shrinking against the stream, not just the consumer:

```bash
nats stream info ORDERS | grep -iE 'messages|last seq'
```

If `Num Pending` is still climbing after the fix, the drain rate is still below the publish rate — go back and add workers / batch size (Step 4), or investigate the server side (Step 8).

---

## Step 7 — Prevent recurrence: alert on pending, not publish rate

The skill's rule: **alert on consumer pending, not publish rate** — publish rate looks healthy right up until the consumer pins. Set a Prometheus alert on the consumer's pending counts:

```yaml
# Alert when a consumer's backlog grows for 10 minutes straight
- alert: JetStreamConsumerLag
  expr: jetstream_consumer_num_pending > 10000
  for: 10m
  labels: { severity: warning }
  annotations:
    summary: "Consumer {{ $labels.consumer_name }} backlog climbing"

# Alert the moment in-flight slots pin — this is the real "blocked" signal
- alert: JetStreamConsumerAckPinned
  expr: jetstream_consumer_num_ack_pending >= jetstream_consumer_max_ack_pending
  for: 5m
  labels: { severity: critical }
  annotations:
    summary: "Consumer {{ $labels.consumer_name }} ack-pinned — delivery stalled"
```

For the failure classes above, consume the advisories **durably** (don't rely on a bare subscription — advisories are fire-and-forget). Subscribe to the specific subjects you care about, not the `$JS.EVENT.ADVISORY.>` firehose:

```bash
# Max-deliveries exhausted (your poison-message / DLQ signal)
nats sub '$JS.EVENT.ADVISORY.CONSUMER.MAX_DELIVERIES.ORDERS.order-processor'

# Explicitly terminated messages
nats sub '$JS.EVENT.ADVISORY.CONSUMER.MSG_TERMINATED.ORDERS.order-processor'
```

---

## Step 8 — If none of the above: check the server/cluster

If the consumer and workers look healthy but delivery is still stalled, the stream may have no leader or a lagging replica:

```bash
nats server report jetstream           # cluster-wide JetStream health
nats stream info ORDERS --json | jq '.cluster'   # leader + replica state
```

No leader → not enough replicas online for a majority (need 2/3 or 3/5). Check routes (`curl http://localhost:8222/routez`) and server logs for peer errors. Always read the NATS server logs — they surface slow-consumer and peer warnings before the failure is visible in `consumer report`.

---

## TL;DR decision path

1. `Num Ack Pending == Max Ack Pending` → consumer is **blocked**, not slow.
2. **Redeliveries climbing?** → `AckWait` too short (raise it / `InProgress()`) or poison message (cap `MaxDeliver`, `Term()`).
3. **Redeliveries flat, Ack Floor frozen?** → worker is down/stuck, or a config-drift deploy left the durable unbound (`consumer next` to tell which; delete+recreate if drifted).
4. Only then: add workers / bigger fetch batches, and size `MaxAckPending` to match.
5. Verify `Ack Floor` advances and `Num Pending` falls.
6. Alert on `num_pending` and `num_ack_pending`, not publish rate.
