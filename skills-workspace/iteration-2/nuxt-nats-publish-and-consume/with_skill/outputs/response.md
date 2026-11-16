# Publish `order.created` and consume it durably with nuxt-nats

Four files. Everything is server-side (`server/`) — nuxt-nats is a Nitro module with no browser composables.

## 1. `nuxt.config.ts`

Register the module and **describe** the `ORDERS` stream (subjects `orders.>`). `provision: 'never'` means the app never creates or edits the stream — that's IaC's job (an idempotent `streams.sh` / Helm), so rolling-deploy replicas don't race on stream config. In prod, point `NUXT_NATS_SERVERS` at all cluster nodes and inject auth via `NUXT_NATS_USER_JWT` + `NUXT_NATS_NKEY_SEED`.

## 2. `server/api/orders.post.ts` — publish

`jsPublish('orders.created', body, { msgId: body.id })`. You pass a plain object (it serializes for you) and get the `PubAck` back. `msgId` sets `Nats-Msg-Id`; a retried publish of the same order id is deduped by the broker inside the stream's `duplicate_window`. Use a deterministic id per logical event. For exactly-once effects, pair this with a transactional outbox and set the stream's `duplicateWindow` at least as long as your retry/drain interval.

## 3. `server/plugins/order-worker.ts` — durable background consumer

`defineNatsConsumer({ stream: 'ORDERS', durable: 'order-processor', handler })` inside a Nitro server plugin. Two things that bite people:

- **The worker gate.** Consumers run **only when `NUXT_NATS_WORKERS=true`**. The publisher (API route) runs fine without it; the long-lived pull loop must not run on serverless/edge. Recommended topology is a serverless publisher plus a separate persistent worker process: `NUXT_NATS_WORKERS=true node .output/server/index.mjs`. If the consumer "never fires", check this first.
- **It must live in `server/plugins/**`.** `server/workers/*.ts` is not auto-scanned by Nitro and silently never registers.

Handler contract: **return = ack, throw = nak** with backoff. `defineNatsConsumer` *binds* the existing durable — `ackPolicy`/`maxDeliver`/`filterSubjects` you pass are only applied when a durable is being *created* (`provision: 'startup'`), so the live durable's IaC config wins.

Idempotency is your job: JetStream is at-least-once, so guard side effects (e.g. an inbox table keyed on `Nats-Msg-Id`) before processing.

## 4. `server/nats-events.d.ts` — typed subjects

Augment `NatsEvents` so every `jsPublish` subject and payload is type-checked. The leading `import type {} from 'nuxt-nats'` line is required or the augmentation silently does nothing.

## Notes / caveats

- NATS has **no** dead-letter queue. `maxDeliver` = retry then *stop*, not "route to a DLQ"; after the limit the message just sits in the stream. If you need a DLQ, add `defineDeadLetterConsumer` against a stream that captures the `MAX_DELIVERIES` / `MSG_TERMINATED` advisory subjects.
- `processOrder` is declared but not implemented — it's your business logic.
