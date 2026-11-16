# Publish + durably consume `order.created` with nuxt-nats

Three pieces: module config, a server route that publishes, and a Nitro
plugin that runs a durable background consumer. JetStream gives you the
durability (the stream persists events; the durable consumer remembers its
position across restarts).

## Files

- `nuxt.config.ts` — register `nuxt-nats`, point it at the server, name the stream/durable.
- `server/utils/jetstream.ts` — ensures the `ORDERS` stream exists, hands back the JetStream client (shared by route + consumer).
- `server/api/orders.post.ts` — `POST /api/orders` publishes `order.created`.
- `server/plugins/order-consumer.ts` — durable pull consumer, started once at Nitro boot.

## 1. nuxt.config.ts

Registers the module and opens the connection on the Nitro (server) side.
`nats.servers` is the only thing you must set per-environment.

## 2. Publish (server API route)

```ts
const js = await useJetStream()
await js.publish('order.created', JSON.stringify(order), { msgID: order.id })
```

- Publish to **JetStream** (`js.publish`), not core NATS (`nc.publish`), or the
  event is fire-and-forget and never lands in the stream.
- `msgID` dedupes retries inside the stream's dedup window — cheap idempotency.
- The returned ack carries `stream`/`seq` so the caller knows it was persisted.

## 3. Durable consumer (Nitro plugin)

Nitro runs `server/plugins/*` once at startup, which is where a long-lived
consumer belongs (not in a request handler). The consumer is:

- **Durable** (`durable_name: 'order-processor'`): survives restarts and keeps
  its ack position, so no events are missed or reprocessed on redeploy.
- **Explicit ack**: `m.ack()` on success, `m.nak()` on failure → JetStream
  redelivers. Without explicit ack a crash mid-processing would lose the event.
- **Filtered** to `order.created` so adding more `order.*` subjects later won't
  feed this worker.
- Stopped cleanly on the Nitro `close` hook.

The consume loop runs detached (not awaited) so it doesn't block server startup.

## Notes / assumptions

- Uses the modular **@nats-io v3** client (`@nats-io/jetstream`). Install:
  `npm i @nats-io/jetstream` (the transport comes via nuxt-nats).
- Stream config: `{ name: 'ORDERS', subjects: ['order.>'] }` — one stream for
  the whole `order.*` namespace. `useJetStream()` creates it on first use; in
  production you may prefer to provision streams out-of-band and drop the
  `streams.add`.
- Single-instance assumption: one durable consumer. If you run multiple server
  replicas, they'll all attach to the same durable and share the work (that's
  fine and usually what you want for a work queue).
