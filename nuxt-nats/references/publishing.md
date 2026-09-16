# Publishing with nuxt-nats

Two publish functions, auto-imported in `server/`.

## `jsPublish` — durable JetStream publish

```ts
await jsPublish('orders.created', { id: '123', total: 99.99 }, {
  msgId: '123',                 // becomes the Nats-Msg-Id dedup key (deduped within the stream's duplicate_window)
  headers: { 'X-Trace-Id': traceId },
})
```

- Serializes the object (JSON) and encodes it for you — you pass a plain object, not bytes.
- **`msgId` is the idempotency lever.** It sets `Nats-Msg-Id`; a re-publish of the same id inside the stream's `duplicate_window` is deduped by the broker. Set it to a deterministic id per logical event (e.g. `orders.created.{orderId}` or the row id) so a retried publish can't double-book. `msgId` is applied last, so a caller can't accidentally override the dedup key through `headers`.
- Does **client-side retry** (default 3 attempts, exponential backoff) because a JetStream publish can time out under load. It returns the `PubAck`, so you can read `ack.duplicate` to see if the broker deduped it.
- Also stamps `X-Trace-Id` / `X-Correlation-Id` when you pass them.

### Deterministic msgId + duplicate_window is the exactly-once-publish story

The pattern both production apps use: a **transactional outbox**. The relay publishes *inside* the same DB transaction that marks the outbox row sent, so a crash after publish but before commit re-runs and re-publishes — and the broker's `Nats-Msg-Id` dedup absorbs the duplicate. For that to hold, the stream's `duplicateWindow` must be **at least as long as your retry / outbox-drain interval** (set it explicitly; the NATS default is only 2 minutes). This gives you at-least-once delivery with idempotent effects, which is the honest exactly-once you can actually build on NATS.

## `corePublish` — fire-and-forget core NATS

```ts
corePublish('metrics.pageview', { path: '/home' })   // no PubAck, no dedup, not persisted
```

Use only for ephemeral, loss-tolerant signals (metrics, presence). It does **not** go through JetStream, so there is no persistence, no dedup, and no delivery guarantee.

## Typed subjects (`NatsEvents`)

Augment the `NatsEvents` interface so every `jsPublish` subject and payload is type-checked. Put the declaration in a `.d.ts` under `server/` (or `shared/` if app code needs the types too), and it **must** start with the `import type` line or the augmentation silently doesn't apply:

```ts
// server/nats-events.d.ts
import type {} from 'nuxt-nats'

declare module 'nuxt-nats' {
  interface NatsEvents {
    'orders.created': { id: string; total: number }
    'orders.cancelled': { id: string; reason: string }
  }
}
```

```ts
await jsPublish('orders.created', { id: '123', total: 99.99 })  // ✅
await jsPublish('orders.created', { id: '123', foo: 'bar' })    // ✗ type error
```

## Publisher discipline (from production)

- **One writer per subject family.** Both real systems make a single relay the sole publisher of the money/domain subjects to avoid dual-writes (write to the DB *or* to NATS, never both from two places). Everything else enqueues to the outbox.
- **Put IDs on the wire, not payloads.** Money/PII-sensitive systems publish IDs only and let consumers re-derive details from the DB; logs carry counts and trace ids, never payloads.
- **Schema version goes in a header/payload field, never in the subject.** `document.uploaded.v2` bakes the version into stream config and breaks tenant-wide replay; use an `X-Schema-Version` header + `schema_version` field instead.
