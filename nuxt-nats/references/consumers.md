# Consuming with nuxt-nats

Three consumer shapes: durable pull workers (`defineNatsConsumer`), a synthesized dead-letter consumer (`defineDeadLetterConsumer`), and request-scoped ephemeral consumers for SSE (`useEphemeralConsumer`).

## The worker gate — read this first

**Consumers and agents run only when `NUXT_NATS_WORKERS=true`.** Without it the app boots, publishes, creates ephemeral consumers — and starts no durable consumers, logging a skip warning. This is deliberate: long-lived pull loops must not run on serverless/edge. If a consumer "never fires", check this before anything else.

Recommended topology: a serverless/edge **publisher** (workers off) plus a separate persistent **worker process** (workers on):

```bash
NUXT_NATS_WORKERS=true node .output/server/index.mjs
```

## `defineNatsConsumer` — durable pull worker

Register inside a Nitro **server plugin** (`server/plugins/**`). Note: `server/workers/*.ts` are **not** auto-scanned by Nitro — a consumer declared there silently never registers.

```ts
export default defineNitroPlugin(() => {
  defineNatsConsumer({
    stream: 'ORDERS',
    durable: 'order-processor',        // binds an EXISTING durable (its config lives in IaC)
    deadLetterSubject: 'dlq.orders.created',
    async handler(data, msg) {
      // handler returns normally => the message is acked
      // handler throws          => msg.nak(backoff) with the configured backoff
      await processOrder(data)
    },
  })
})
```

- **Binds, does not enforce.** By default it binds the durable as it exists on the broker; `filterSubjects`/`ackPolicy`/`ackWait`/`maxDeliver` you pass are only used when *creating* a durable (`provision: 'startup'`). The live durable's config wins — a value you pass that disagrees is reported, not applied.
- The module's consume loop uses `consume({ max_messages: 1, idle_heartbeat: 5000 })` for clean per-message error isolation and stale-subscription detection, sends `msg.working()` heartbeats every `ackWait/2` for slow handlers, and on a throw calls `msg.nak(backoff[…])`.

### Manual ack control inside a handler

When you need finer control than return-acks-throw-naks, act on `msg` directly. The rules that matter:

- **`msg.nak(delayMs)` for a transient failure** (retry later). A **bare `msg.nak()` redelivers immediately** and can burn all `maxDeliver` attempts in milliseconds — always pass a delay for "not ready yet" (e.g. `msg.nak(30_000)`).
- **`msg.term(reason)` for a poison message** you will never process (malformed, missing a required header). `term()` stops redelivery and records the reason (client 3.4+); `nak`-ing it would just loop it to `maxDeliver`.
- **`msg.ack()` to drop** a message you can't use but don't want redelivered (e.g. an unparseable body you've logged).
- Guard idempotency yourself: JetStream is at-least-once. Keep a processed/inbox table keyed `(Nats-Msg-Id, handler)` — `INSERT ... ON CONFLICT DO NOTHING` — and skip if already processed. Reject a message with **no `Nats-Msg-Id`** via `term()` (you can't dedup it, and a redelivery can never acquire the header, so nak would loop forever).

## Dead-letter handling — there is no DLQ in NATS

`maxDeliver` means **retry then stop**, not "route to a dead-letter queue." After the limit the server stops redelivering and publishes a `$JS.EVENT.ADVISORY.CONSUMER.MAX_DELIVERIES` advisory; the message stays in the stream but nothing hands it to you. A comment claiming "NATS routes to DLQ after max_deliver" is wrong — a real production codebase shipped exactly that comment with no advisory listener, silently dropping messages after 5 attempts.

Build the DLQ yourself with `defineDeadLetterConsumer`, which does it correctly:

```ts
export default defineNitroPlugin(() => {
  defineDeadLetterConsumer({
    stream: 'JS_ADVISORY',           // a stream that CAPTURES the advisory subjects (durable, not core-sub)
    durable: 'dlq-router',
    async handler(event) {
      // event has the original subject, delivery count, and recovers the original
      // message by sequence via jsm.streams.getMessage(); an aged-out message is null (expected).
      await recordDeadLetter(event)
    },
  })
})
```

Two mistakes it avoids and you should too:
1. **Don't subscribe with core NATS** to the advisory subjects — advisories are fire-and-forget, so anything published while your subscriber restarts or is partitioned is gone. Capture the advisory subjects into a **JetStream stream** and consume that durably.
2. **Don't use the account-wide `advisories()` firehose** (`$JS.EVENT.ADVISORY.>`) — it includes an audit advisory on *every* JetStream API call. Capture only `…CONSUMER.MAX_DELIVERIES.>` and `…CONSUMER.MSG_TERMINATED.>`.

Give the DLQ consumer **no** `deadLetterSubject` of its own — a failing dead-letter handler must not dead-letter itself into a loop.

## `useEphemeralConsumer` — SSE / request-scoped

For an API route that streams matching events to the browser and then goes away:

```ts
export default defineEventHandler(async (event) => {
  const handle = await useEphemeralConsumer({
    stream: 'ORDERS',
    filterSubjects: ['orders.*.shipped'],
    timeoutMs: 30_000,
    onMessage: (data) => sendSSE(event, data),
  })
  // handle cleans up on timeout and on client disconnect automatically
})
```

It creates an **ordered, ephemeral** consumer scoped to the request (no durable state), with timeout and disconnect cleanup and per-message error isolation. This is the right tool for "wait for the one event that answers this request" — never create a durable per request.
