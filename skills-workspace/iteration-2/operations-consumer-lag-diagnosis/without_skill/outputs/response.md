# Diagnosing a JetStream consumer that is falling behind

Your symptoms tell a specific story. Let me name it before we touch anything, because it changes what the fix is.

- **Num Ack Pending pinned at the max** means the consumer is holding `MaxAckPending` messages in flight that it has delivered but not yet acked.
- Once ack-pending hits that ceiling, **JetStream stops delivering new messages to this consumer** until some get acked. This is backpressure working as designed.
- Because delivery is throttled, **Num Pending climbs**: new messages pile up in the stream faster than your consumer drains them.

So the stream is not the problem. The consumer (or whatever it depends on) is the bottleneck, and `MaxAckPending` is the valve that's now fully closed. The whole job of this walkthrough is to find out **why acks aren't coming back fast enough**, and there are only a few possibilities:

1. The consumer is healthy but genuinely too slow (CPU, slow downstream, not enough workers).
2. Acks are timing out and messages are being **redelivered** — a redelivery storm, often from one poison message or an `AckWait` shorter than your processing time.
3. A consumer app bug: it crashed, stopped acking, double-acks, or leaks in-flight messages.

The diagnosis below separates these three in about five minutes.

---

## Step 0 — Set your shell variables

```bash
export STREAM=ORDERS          # your stream
export CONSUMER=order-worker  # your durable consumer
```

(Adjust to your names. `nats stream ls` and `nats consumer ls $STREAM` if you're unsure.)

---

## Step 1 — Read the full consumer state

`consumer report` gives the summary; `consumer info` gives the fields that actually tell you which scenario you're in.

```bash
nats consumer info $STREAM $CONSUMER
```

Look at these fields specifically:

| Field | What to read from it |
|---|---|
| `Acknowledgment Policy` | Should be `Explicit` for a work-queue/scaling consumer. `None` or `All` behaves very differently. |
| `Ack Wait` | How long JetStream waits for an ack before redelivering. Compare this to how long one message actually takes to process. |
| `Max Ack Pending` | The ceiling you're pinned against. |
| `Max Deliver` | After this many attempts a message is dropped (or goes to your DLQ if you set one up). `-1` = infinite redelivery. |
| `Num Pending` | Backlog not yet delivered. |
| `Num Ack Pending` | In-flight, awaiting ack (pinned = at `Max Ack Pending`). |
| `Num Redelivered` | **The tell.** High or climbing = redelivery storm (scenario 2). Near zero = genuinely slow (scenario 1). |
| `Last Delivered` vs `Acknowledgment floor` | A large, growing gap means acks are stuck behind one or more old messages. |

---

## Step 2 — Decide which scenario you're in

Run `consumer info` twice, ~10s apart, and watch `Num Redelivered` and `Num Ack Pending`.

```bash
nats consumer info $STREAM $CONSUMER | grep -E 'Redelivered|Ack Pending|Pending Messages|Acknowledgment floor'
sleep 10
nats consumer info $STREAM $CONSUMER | grep -E 'Redelivered|Ack Pending|Pending Messages|Acknowledgment floor'
```

- **`Num Redelivered` is low and roughly flat** → **Scenario 1: genuinely too slow.** The consumer is acking, just not fast enough. Go to Fix A.
- **`Num Redelivered` is climbing fast** → **Scenario 2: redelivery storm.** Messages are timing out before they're acked and coming back around, re-consuming your ack-pending budget with the same messages. Go to Step 3, then Fix B.
- **`Num Ack Pending` is pinned but the app logs show nothing being processed** → **Scenario 3: stuck/crashed consumer.** The in-flight messages will sit until `AckWait` expires. Go to Fix C.

---

## Step 3 — (Scenario 2) Find the poison message

A redelivery storm is usually one message that always fails. Inspect what's being redelivered without acking it:

```bash
# Pull a batch WITHOUT acking so you can look at it (Explicit-ack consumers only).
# --no-ack means these won't be acked; they'll be redelivered after AckWait.
nats consumer next $STREAM $CONSUMER --count=5 --no-ack
```

Check the headers on each message — `Nats-Delivery-Count` (or the `--trace`/verbose output) tells you how many times a message has been attempted. A message with a delivery count far above the others is your poison pill.

You can also read the oldest pending message directly by sequence:

```bash
nats stream get $STREAM --last-by-subject="orders.>"   # or
nats stream view $STREAM                                # paginate through the stream
```

Cross-check against your consumer logs for the subject/ID that keeps erroring.

---

## Fixes

### Fix A — Consumer is healthy but too slow (scale the drain rate)

The levers, roughly in order of "do this first":

1. **Add more worker instances.** For a pull consumer, every process that calls `fetch`/`consume` on the same durable shares the load. Doubling workers roughly doubles drain rate until the downstream (DB/API) becomes the limit. This is the real fix for a backlog; everything else just adjusts buffers.

2. **Raise `MaxAckPending` — only if your workers can actually use the extra in-flight slots.** If you have 50 concurrent workers but `MaxAckPending=100`, you may be starving them. If you have 4 workers, raising it to 10000 just buffers more and fixes nothing.

   ```bash
   nats consumer edit $STREAM $CONSUMER --max-pending=5000
   ```

   (`MaxAckPending` is editable live on modern nats-server; no recreate needed.)

3. **Increase the pull batch size** so workers aren't round-tripping for one message at a time. This is a client-side setting (`fetch(batch=N)` / `PullMaxMessages`), not a `nats` CLI flag — check your consumer code.

4. **Fix the downstream.** If each message does a slow DB write or external API call, that is your true ceiling. Batch the writes, add a connection pool, or cache. No NATS tuning beats a 200ms-per-message dependency.

### Fix B — Redelivery storm (stop the churn)

1. **If `AckWait` is shorter than your real processing time, raise it.** This is the single most common cause: the message is still being worked when `AckWait` fires, JetStream redelivers it, and now the same message is in flight twice.

   ```bash
   # Processing takes ~25s? Give AckWait real headroom.
   nats consumer edit $STREAM $CONSUMER --wait=60s
   ```

2. **For legitimately long jobs, send in-progress acks** from the client so `AckWait` keeps resetting while you work (`msg.InProgress()` / `AckProgress` / `msg.inProgress()` depending on language). This lets you keep `AckWait` short without false redeliveries.

3. **Add backoff** so a failing message doesn't hammer back instantly:

   ```bash
   nats consumer edit $STREAM $CONSUMER --backoff=linear --backoff-steps=5 --backoff-min=1s --backoff-max=1m
   ```

4. **Cap redelivery and route poison messages to a DLQ.** Set a finite `MaxDeliver` and have your app `TERM` a message it knows will never succeed (TERM stops redelivery immediately, unlike NAK). Pair it with the max-deliver advisory to capture terminated messages:

   ```bash
   nats consumer edit $STREAM $CONSUMER --max-deliver=5
   # Watch terminated/exhausted messages so none are silently lost:
   nats stream report
   nats sub '$JS.EVENT.ADVISORY.CONSUMER.MAX_DELIVERIES.>'
   ```

   In the app: on an unrecoverable error call `msg.Term()` instead of letting it NAK and loop.

### Fix C — Consumer stuck or crashed

- Confirm whether workers are alive and connected:

  ```bash
  nats consumer info $STREAM $CONSUMER | grep -E 'Active Interest|Delivered'
  nats server report connections   # are your worker connections present?
  ```

- If workers died mid-message, those in-flight messages won't ack and will redeliver only after `AckWait`. Restart the workers; set a sane `AckWait` so recovery doesn't take forever.
- If the app is deadlocked (acquired a message, blocked on a lock/DB), that's an app bug — the fix is in your code, not in NATS. The signature is: ack-pending pinned, `Num Redelivered` only ticking up once per `AckWait` interval, zero throughput in app logs.

---

## Step 4 — Confirm recovery

After applying a fix, watch the backlog actually drain:

```bash
# Re-run the report every few seconds and watch Unprocessed/Pending fall.
watch -n 5 "nats consumer report $STREAM"
```

You want to see **Num Pending trending down** and **Num Ack Pending no longer pinned** at the ceiling. If ack-pending is still pinned but redelivered is flat, you're throughput-bound — go back to Fix A step 1 (more workers / faster downstream). If redelivered is still climbing, the poison message is still in the stream — go back to Step 3.

---

## One-line triage summary

> `Num Ack Pending` at max is backpressure, not the disease. Check `Num Redelivered`: flat → you're too slow (add workers / fix downstream); climbing → acks are timing out (raise `AckWait`, use in-progress acks, and TERM the poison message to a DLQ).

A `diagnose.sh` helper that gathers all of the above in one shot is in this folder.
