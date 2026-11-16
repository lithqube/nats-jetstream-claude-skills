# Order processing on JetStream — work queue

## Why this shape

Your requirement is "process each order by **exactly one** of several workers, with retries." That's the textbook **work-queue** pattern, and JetStream gives it to you directly:

- **Stream** with `Retention: WorkQueuePolicy`. A message stays in the stream until it is acked, then it is deleted. A work-queue stream also forbids overlapping consumers on the same subject, which is exactly the "exactly one consumer" guarantee you want.
- **One durable pull consumer** (`order-workers`) shared by every worker instance. You do **not** create a consumer per worker. All workers pull from the same durable consumer, and the server hands any given message to exactly one of them. Scaling = running more worker processes; no config change.
- **Explicit ack + `MaxDeliver` + `BackOff`** give you retries for free. A worker that `Nak`s (or crashes without acking within `AckWait`) causes the server to redeliver after the configured backoff, up to `MaxDeliver` attempts.

Pull (not push) is the right choice here: pull consumers load-balance naturally across instances and apply backpressure (a worker only fetches when it has capacity), whereas a push consumer with a queue group is the older, harder-to-flow-control approach.

## The pieces

| File | Role |
|------|------|
| `order/order.go` | `Order` type + `Setup()` — declares the stream and the shared consumer (idempotent, run on every startup) |
| `order/order_test.go` | Self-check for `Order.Validate` (runs without a server) |
| `cmd/producer/main.go` | Web API: `POST /orders` validates and publishes to `orders.created` |
| `cmd/worker/main.go` | Pulls, processes, acks; `Nak` to retry, `Term` for poison messages |

Uses the modern `github.com/nats-io/nats.go/jetstream` package (the `nats.JetStreamContext` API is legacy).

## Key design points

- **Subjects:** stream captures `orders.>`; producer publishes `orders.created`. The wildcard leaves room for other order events later without touching the stream.
- **Idempotent publish:** the producer sets a message ID (`WithMsgID(o.ID)`). Combined with the stream's `Duplicates: 2m` window, a retried HTTP request with the same order ID is deduplicated server-side instead of enqueuing a second copy.
- **Retries with backoff:** `MaxDeliver: 5` with `BackOff: [1s, 5s, 15s, 30s]`. `AckWait: 30s` means a crashed worker's in-flight message is redelivered to another worker after 30s.
- **Idempotent processing:** `process()` must be idempotent — a message can be redelivered after a worker does the side effect but dies before acking. This is inherent to at-least-once delivery, not optional.
- **Poison messages:** an unparseable message is `Term`'d so it isn't retried pointlessly. For business failures the worker `Nak`s and lets the retry policy run.

## Running it (when you have a server)

```
nats-server -js
go run ./cmd/worker      # start 2-3 of these
go run ./cmd/producer
curl -X POST localhost:8080/orders -d '{"id":"o-1","customer_id":"c-9","amount_cents":4200}'
```

## What I did not build (and the one risk to know)

- **No dead-letter queue.** After `MaxDeliver` the server stops redelivering and emits a `$JS.EVENT.ADVISORY.CONSUMER.MAX_DELIVERIES` advisory, but in a *work-queue* stream the message is **not** auto-removed and is **not** moved anywhere — it just stops being delivered and will linger until `MaxAge` (24h) expires it. If you need to inspect failed orders, add a second normal stream that sources those advisories, or switch the main stream to `Limits` retention plus a DLQ. I left this out because you asked to keep it focused; wire it in before production if lost orders must be recoverable.
- No auth/TLS, no graceful in-flight drain tuning, no metrics, no `go.sum` (run `go mod tidy`). The nats.go version in `go.mod` is a known-good pin; `go mod tidy` will resolve the exact patch. Code is not compiled/tested here (no toolchain run requested) beyond the validation self-check, which is server-free.
