# Dead-letter queue for a NATS JetStream work queue

## The one thing to know first

**NATS JetStream has no native dead-letter queue.** There is no `dead_letter`
config field, in any released server including 2.15. `MaxDeliver: 5` on its own
means *"try 5 times, then stop redelivering and go quiet"* — the message is not
moved anywhere, and nothing tells your application it gave up.

What NATS actually gives you are three primitives, and a real DLQ is built by
gluing them together:

1. A **`MAX_DELIVERIES` advisory** the server publishes when a message exhausts
   `MaxDeliver`.
2. **`GetMsg(stream, seq)`** — the failed message is *not* deleted (it was never
   acked), so you can still fetch it by sequence.
3. **`DeleteMsg(stream, seq)`** — to remove the original once you've copied it.

This solution wires those together. Set `MaxDeliver: 5` and the "fail 5 times"
requirement is met by the server; the DLQ, inspect, and replay are code you run.

## Files

| File | Role |
|------|------|
| `main.go` | Everything, as subcommands (see below) |
| `main_test.go` | Self-check for the idempotent-move logic (`go test`, no server needed) |
| `go.mod` | `github.com/nats-io/nats.go` |

Run order:

```
go run . setup        # once — creates streams + consumers (idempotent)
go run . worker       # long-lived — your task processor
go run . dlq-router   # long-lived — the DLQ engine
go run . inspect      # on demand — list what's in the DLQ
go run . replay all   # on demand — re-inject dead letters (or: replay 3 7)
```

## How it works

```
producer → TASKS (WorkQueuePolicy) → worker "processor" (MaxDeliver: 5)
                                           │ 5th failure
                                           ▼
                 $JS.EVENT.ADVISORY.CONSUMER.MAX_DELIVERIES.TASKS.processor
                                           │ (captured durably)
                                 JS_ADVISORY stream → dlq-router
                                           │ GetMsg + copy + DeleteMsg
                                           ▼
                           TASKS_DLQ (LimitsPolicy) ──inspect / replay──┐
                                           ▲                            │
                                           └──── replay re-publishes ───┘
                                                 back to tasks.*
```

### Why each non-obvious choice is there

- **Advisories are captured into a durable stream (`JS_ADVISORY`), not
  subscribed to directly.** Advisories are fire-and-forget core-NATS messages.
  A plain `nc.Subscribe` to the advisory subject loses every failure that lands
  while the router is restarting or partitioned — i.e. it makes the *failure
  path* the only non-durable part of an otherwise durable system. Capturing them
  into a stream and consuming that durably is the only shape that doesn't drop
  jobs.

- **The router deletes the original from `TASKS` after copying it.** On a
  `WorkQueuePolicy` stream a message that hit `MaxDeliver` is never acked, so it
  *stays in the stream and blocks the queue* — no other consumer can take it.
  Copying it to the DLQ without deleting the original leaves the work queue
  jammed. The move = copy **then** delete.

- **The move is idempotent.** If the router crashes after the DLQ write but
  before the delete, the advisory redelivers and it writes again — so the DLQ
  write carries `Nats-Msg-Id: TASKS-<seq>` and the DLQ stream has a
  `DuplicateWindow`, which collapses the duplicate. This is the logic
  `main_test.go` guards.

- **Provenance headers travel with the dead letter** (`Original-Stream`,
  `Original-Subject`, `Original-Seq`, `Failure-Reason`, and the app's original
  `Nats-Msg-Id`). `inspect` reads them; `replay` needs `Original-Subject` to know
  where to send the message back, and preserves the original `Nats-Msg-Id` so a
  consumer-side idempotency/inbox table still dedups after a replay.

- **The worker uses `Nak`, never `Term`, on failure.** `Term()` permanently
  drops a message *without* firing the `MAX_DELIVERIES` advisory, which would
  silently bypass the DLQ. Let the server reach `MaxDeliver` on its own.

## What this does and doesn't guarantee

- **Delivery is at-least-once, not exactly-once.** A message can be redelivered
  (and a dead letter can be replayed). Make `processTask` idempotent — ideally
  with an inbox table keyed on `(Nats-Msg-Id, handler)`, committing the effect
  *before* acking. "Exactly-once on NATS" is really at-least-once + idempotent
  effects.
- **`MaxDeliver: 5` counts the first delivery plus 4 retries.** With the 4-entry
  `BackOff` schedule, that's attempts at 0s, 5s, 30s, 2m, 10m.
- **Replay clears the dead letter from the DLQ after re-injecting it**, so
  re-running `replay all` won't double-submit. If you'd rather keep an audit
  copy, remove the `DeleteMsg` in `replayOne` and track a "replayed" marker
  instead.
- `Replicas: 1` here is for a single-node dev server — use `3` in production (as
  the skill's core principles state).

## Production hardening not included here (deliberately out of scope)

- Also capturing `MSG_TERMINATED` advisories for messages your own code `Term`s
  — the `JS_ADVISORY` stream already subscribes to that subject, but `worker`
  never calls `Term`, so nothing exercises it. Wire it in if you add explicit
  poison-message rejection.
- Metrics/alerting on DLQ depth (`inspect` shows it; a real system should alarm
  on it).
- Authz for the `$JS.EVENT.ADVISORY.>` subjects and the admin `replay` command.
```
