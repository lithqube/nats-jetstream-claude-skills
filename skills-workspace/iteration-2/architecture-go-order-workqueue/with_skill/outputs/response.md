# Order processing on JetStream — design notes

An order web API drops each order into a JetStream **work queue**; a pool of
identical workers competes for them so each order is processed by exactly one
worker, with bounded retries on failure and nowhere for a failed order to
silently vanish.

```
POST /orders ─▶ cmd/api ──▶ Stream ORDERS (WorkQueuePolicy) ──▶ durable pull consumer "order-workers"
                                                                   ├─ cmd/worker #1
                                                                   ├─ cmd/worker #2   (exactly one gets each order)
                                                                   └─ cmd/worker #3
                            MaxDeliver exhausted ─▶ advisory ─▶ ORDERS_ADVISORY ─▶ cmd/dlq ─▶ ORDERS_DLQ
```

## Files

| File | Role |
|------|------|
| `orders/orders.go` | `Order` type + `EnsureTopology` — the one definition of streams and the consumer |
| `cmd/api/` | HTTP producer: `POST /orders`, idempotent publish |
| `cmd/worker/` | Worker instance: fetch → process → ack/nak; run N copies |
| `cmd/dlq/` | Dead-letter router: durably consumes failure advisories |
| `orders/orders_test.go` | Offline self-check (round-trip + advisory-subject scoping) |

## Why these choices

**Stream = `WorkQueuePolicy`.** This is the setting that makes "exactly one of
several workers" true: a message is held until acked and then deleted, and the
stream permits only one consumer per subject. A single durable pull consumer
(`order-workers`) is shared by every worker process, so JetStream load-balances
orders across them. Add or remove worker instances freely — no config changes.

**Pull, not push.** Pull consumers give natural backpressure: a worker fetches a
batch only when it is ready for more, and `MaxAckPending: 100` caps total
in-flight orders across the whole pool. Push would make the server fire orders at
whatever rate and rely on flow control to not overwhelm a slow worker.

**Retries = `MaxDeliver: 5` + `BackOff`.** A failing worker calls `msg.Nak()`;
JetStream redelivers on the backoff schedule (5s, 30s, 2m, 10m). `AckWait: 30s`
means a crashed worker's in-flight order is redelivered after 30s even without a
nak. Retries are bounded — we do not loop forever on a poison order.

**Backpressure to the producer = `DiscardNew`.** When the queue hits its bound
the server rejects new publishes, so `cmd/api` returns `503 retry later` instead
of letting an unbounded backlog build. `MaxAge: 24h` expires stale orders.

**Idempotent publish = `Nats-Msg-Id`.** The API sets the message id to the order
id, so a client that retries `POST /orders` (timeout, double-click) does not
enqueue the order twice — the stream's duplicate window collapses it. The window
(5m) should be at least as long as your slowest client/publish retry.

**Synchronous publish.** The API returns `202 Accepted` only after the stream has
durably stored the order, so a `2xx` is a real durability promise, not fire-and-
forget.

## The part the work-queue pattern forces you to build: the DLQ

JetStream has **no built-in dead-letter queue**. When an order exhausts
`MaxDeliver`, the server stops redelivering and publishes an *advisory* — and on
a `WorkQueuePolicy` stream the order also just *sits there* occupying the queue,
because no other consumer can take it. `MaxDeliver` alone therefore means "retry,
then stop, and tell no one" — unacceptable for orders.

`cmd/dlq` closes that gap:

1. Failure advisories are captured into their own stream (`ORDERS_ADVISORY`) so
   none are lost across a restart (raw advisories are fire-and-forget).
2. A durable consumer reads them, fetches the stuck order by its stream sequence
   (still present because MaxDeliver does not delete it), republishes it to
   `ORDERS_DLQ` with failure metadata headers, **then deletes it from `ORDERS`**
   to unblock the work queue.
3. The advisory is acked only *after* the DLQ write succeeds — a crash mid-route
   redelivers the advisory rather than losing the order. The DLQ publish carries
   an `Nats-Msg-Id` derived from the original sequence, so a redelivered advisory
   cannot double-write.

We subscribe to exactly `MAX_DELIVERIES.ORDERS.>` and `MSG_TERMINATED.ORDERS.>`,
never `$JS.EVENT.ADVISORY.>` (that firehose also carries an advisory on every
JetStream API call).

## Consumer-side exactly-once (what `process()` must guarantee)

Delivery is **at-least-once**: an order can be redelivered after a crash or ack
timeout. We ack only after the work is durably done, so the design is correct
*if* `process()` is idempotent. In production make it so with an inbox table:
in one DB transaction apply the effect and
`INSERT ... ON CONFLICT (order_id) DO NOTHING`, commit, then `Ack()`. That pairs
with the publish-side dedup above to give effective exactly-once.

## Running (needs a JetStream-enabled NATS server)

```sh
nats-server -js                      # or point NATS_URL at your cluster
go run ./cmd/api                     # producer on :8080
go run ./cmd/worker                  # start several of these
go run ./cmd/dlq                     # one DLQ router
curl -XPOST localhost:8080/orders -d '{"id":"ord-1","customer":"acme","amount":42.5}'
```

`Replicas: 3` in the configs assumes a 3-node cluster; drop to `1` for a
single-node dev server.
