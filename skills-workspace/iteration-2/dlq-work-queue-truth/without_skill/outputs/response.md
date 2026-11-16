# Dead-letter queue for a NATS JetStream work queue

## The one thing to know first

**JetStream has no built-in dead-letter queue.** `max_deliver` only makes the server
*stop redelivering* a message after N attempts. With a WorkQueue stream that message is
then removed and its payload is gone. There is no automatic DLQ to catch it.

So a "DLQ" in JetStream is a pattern you build: on the final failed attempt, the worker
copies the message to a separate stream itself, *then* stops it. That is what these files do.

## What's here

| File | Role |
|------|------|
| `setup.mjs`  | Creates `WORK` (workqueue), a separate `WORK_DLQ` stream, and the `worker` consumer with `max_deliver` = 6. Run once. |
| `worker.mjs` | Consumes `work.>`. On the 5th failed attempt, publishes the message to `dlq.<subject>` with metadata headers, then `term()`s it. |
| `replay.mjs` | Drains `WORK_DLQ` and republishes each message to its original subject, then acks it. Run after fixing the bug. |

Client: the modular `@nats-io/*` v3 packages. Same logic ports directly to Go
(`jetstream` package, `msg.Metadata().NumDelivered`) or the Python client.

## Why these choices

- **Separate DLQ stream, not a DLQ subject on `WORK`.** `WORK` has WorkQueue retention, so a
  message there is deleted once acked/termed. The DLQ must outlive the original, so it's its
  own `limits`-retention stream.
- **Republish on the last attempt, don't rely on the advisory.** NATS emits a
  `$JS.EVENT.ADVISORY.CONSUMER.MAX_DELIVERIES.WORK.worker` advisory when `max_deliver` is hit,
  but it carries only the stream sequence — **not the payload**. You'd have to fetch the
  message by sequence before WorkQueue deletes it: a race. Republishing while the payload is
  in hand is simpler and lossless.
- **`max_deliver` = 6, business limit = 5.** The server drops a message the instant
  `max_deliver` is reached. If the worker crashes *during* attempt 5 before it can republish,
  that spare 6th delivery is the safety net that still routes it to the DLQ.

## Inspect the DLQ (no code needed)

```bash
nats stream info WORK_DLQ            # how many are parked
nats stream view WORK_DLQ           # page through them
nats stream get  WORK_DLQ <seq>     # one message, with the Nats-DLQ-* headers
nats consumer report WORK           # see num_pending / redelivered on the live queue
```

Each DLQ message carries headers: `Nats-DLQ-Original-Subject`, `Nats-DLQ-Attempts`,
`Nats-DLQ-Error`, `Nats-DLQ-Failed-At`, `Nats-DLQ-Stream-Seq`.

## Replay

```bash
node replay.mjs     # moves everything in WORK_DLQ back onto WORK
```

To replay selectively, filter on `Nats-DLQ-Original-Subject` or the error header inside the loop.

## Caveats worth knowing

- **Poison-message loop.** A message that always fails will go WORK → DLQ → (replay) → WORK →
  DLQ again. Fix the cause before replaying, or the replayer just refills the DLQ.
- **Replay resets the attempt count.** A replayed message starts again at attempt 1. That's
  usually what you want; if not, carry a persistent attempt count in a header.
- **Ordering is not preserved** across the DLQ round-trip.
- **`ack_wait` matters.** A slow handler that exceeds `ack_wait` (30s here) counts as a failed
  delivery and burns a retry. Size it above your real processing time.
- **One consumer per WorkQueue subject.** You can scale the `worker` consumer horizontally
  (many instances pulling the same durable), but you can't add a second *differently-filtered*
  consumer over the same subjects on a WorkQueue stream.
