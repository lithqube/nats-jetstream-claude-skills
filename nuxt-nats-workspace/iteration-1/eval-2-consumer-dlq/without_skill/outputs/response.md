# Debugging a durable pull consumer that receives nothing (+ retry & DLQ)

Short version: "NATS connected" only proves you have a **core NATS** connection. It says nothing about whether **JetStream** has a stream that captures `orders.created`, whether your **publisher is actually storing** messages, whether your **consumer's filter subject matches**, or whether anything is **calling `fetch`/`consume`** to pull. A durable pull consumer is 100% passive — if no loop is pulling, it sits there forever, silent, with a healthy-looking connection.

Let me walk through the likely causes in the order they bite people, then give you a production-ready consumer with transient retry and a real dead-letter path.

---

## 1. Why "connected fine" but no messages

### (a) The publisher is using core NATS, not JetStream
This is the #1 cause. These two look almost identical but behave completely differently:

```ts
nc.publish('orders.created', payload)   // core NATS: fire-and-forget, NOT stored
await js.publish('orders.created', payload) // JetStream: stored, returns an ack with seq/stream
```

If the producer calls `nc.publish(...)` (or a framework helper that does), the message is broadcast to any *core* subscriber that happens to be connected at that instant and is **never persisted**. A pull consumer reads from the *stream*, so it sees nothing. 

**Check:** the publish call must go through the JetStream client (`js.publish`) and you should be inspecting the returned `PubAck` (it has `stream`, `seq`, `duplicate`). If you don't get a PubAck, it wasn't stored.

```bash
# On the server, confirm messages are actually landing in the stream:
nats stream info ORDERS
nats stream view ORDERS        # or: nats stream get ORDERS <seq>
```

If `messages: 0` after your app publishes, the problem is upstream of the consumer — fix the publisher first.

### (b) No stream captures the subject
JetStream storage only exists if a **stream** is configured with a subject filter that matches `orders.created`. If nobody created the stream (dev worked because someone ran `nats stream add` once on their laptop, prod is a fresh server), publishes either error or — worse — silently fall back to core.

```bash
nats stream ls
nats stream find orders.created   # which stream, if any, owns this subject
```

**Fix:** create the stream idempotently at boot (code below). Make sure its `subjects` includes `orders.created` (or `orders.>`).

### (c) The consumer's filter subject doesn't match
A durable consumer has a `filter_subject` (or `filter_subjects`). If it was created as `orders.*.created`, `orders.created.>`, or bound to a different stream than the one receiving traffic, it will match nothing. Subject tokens are exact and dot-delimited — `orders.created` is not matched by `orders.*` (that's a single trailing token) and *is* matched by `orders.>`.

```bash
nats consumer info ORDERS orders-worker
# check: Filter Subject, plus "Unprocessed Messages" / "Outstanding Acks"
```

### (d) Nothing is pulling
Pull consumers deliver only when the client asks. If your Nuxt code creates the consumer but the `consume()` / `fetch()` loop:
- lives in a plugin that only runs in `dev`,
- threw during startup and was swallowed,
- or never got wired up,

then the consumer is healthy and idle. `nats consumer info` will show a growing "Unprocessed Messages" count with zero deliveries — a dead giveaway that the server has messages ready but nobody is fetching.

### (e) DeliverPolicy / stale ack floor
If the durable was first created with `DeliverPolicy: new` **before** the messages were published, or its ack floor is already past them, it won't redeliver old messages. For a brand-new durable that should drain the backlog, use `DeliverPolicy: all`. (Once a durable exists, this is fixed — changing the config later doesn't rewind it; you'd reset or recreate the consumer.)

### (f) Nuxt/Nitro deployment reality — the subtle production-only killer
This is the one that explains "works in dev, silent in prod." A JetStream pull consumer needs a **long-lived process** holding an open connection and running the consume loop. That is fundamentally incompatible with **serverless / edge** deployment targets:

- On Vercel, Netlify Functions, Cloudflare Workers, or any Lambda-style preset, each request spins up an ephemeral invocation. Your `server/plugins/nats.ts` may run, open a connection, and then the instance is frozen/torn down between requests. The consume loop never gets sustained runtime, so nothing is processed. Dev (`nuxt dev`, a persistent Node process) hides this completely.
- The fix is to run the consumer in a **persistent Node server** — `nitro.preset: 'node-server'` (or a dedicated worker process/container), not a serverless function. In many production setups the cleanest architecture is to **not** run the consumer inside the Nuxt request server at all: run it as a separate long-running worker (its own container/systemd/PM2 process) that imports the same handler. That also means scaling your web tier doesn't accidentally spawn N competing consumers.

Also make sure the consumer is started exactly once. A Nuxt **server plugin** (`server/plugins/`) runs once per Nitro instance, which is what you want — but if you accidentally create/start it inside an API route or middleware, you'll get a new consumer or duplicate loops per request.

### Quick triage checklist
```bash
nats stream ls                       # does a stream exist?
nats stream info ORDERS              # messages > 0? subjects include orders.created?
nats consumer info ORDERS orders-worker  # unprocessed > 0 but no delivery => nobody pulling
nats consumer next ORDERS orders-worker  # manually pull one; if this works, your app loop is the bug
```
If `nats consumer next` hands you a message but your app doesn't, the server side is fine and the bug is in your client loop or deployment target (points d/f).

---

## 2. Retry & dead-letter — what NATS does and does *not* give you

**JetStream has no built-in dead-letter queue.** There is no `--dlq` flag. What it gives you are the primitives to build one:

- **`ack_wait`** — how long the server waits for an ack before it considers the message in-flight-timed-out and redelivers.
- **`max_deliver`** — the maximum number of delivery attempts. After this, the server **stops redelivering and drops the message from that consumer's view**. If you did nothing else, poison messages would silently vanish after N tries.
- **`nak` (negative ack)** — "I couldn't process this, redeliver it." You can pass a **delay** for backoff: `m.nak(5000)`.
- **`backoff`** — a per-attempt delay array on the consumer config (e.g. `[1s, 5s, 30s]`), so redelivery timing escalates automatically instead of hammering.
- **`term`** — "this message is permanently unprocessable, do **not** redeliver." This is what you call for a poison message *after* you've copied it to your DLQ.
- **`in_progress` (`working()`)** — "still working, reset my ack_wait timer" for long jobs.

### The DLQ pattern
There is no automatic hand-off, so **you** build it:

1. Give the consumer a bounded `max_deliver` and a `backoff` schedule.
2. In your handler, distinguish **transient** failures (DB blip, timeout, 503 from a downstream) from **poison** (bad schema, business-rule violation that will never succeed).
3. Transient → `nak(delay)` (or just throw and let `ack_wait`/backoff redeliver). Let it retry up to `max_deliver`.
4. Poison, **or** the last attempt (`deliveryCount >= max_deliver`) → **publish the message to a DLQ stream** (`orders.created.dlq`) with headers capturing the reason and original metadata, then **`term()`** it so it never comes back.
5. Create a separate **DLQ stream** on `orders.created.dlq` (or `dlq.>`) so those poison messages are durably retained for inspection/replay. Alert on its depth.

Relying on `max_deliver` alone is a trap: once it's exceeded, the message is gone and you have no record of what failed. Always copy-to-DLQ *before* you term or before the final attempt lapses.

You can also monitor the advisory `$JS.EVENT.ADVISORY.CONSUMER.MAX_DELIVERIES.>` as a safety net to catch anything that hit max_deliver without being explicitly DLQ'd, but doing the DLQ publish inline (as below) gives you the payload, not just a notification.

---

## 3. Production-ready consumer

Using `nats.js` (the `nats` npm package, v2 API). Structured as a Nuxt server plugin, but the `startOrdersConsumer` function is deployment-agnostic — you can equally import it into a standalone worker entrypoint.

### `server/utils/nats.ts` — connection + topology (idempotent)

```ts
import {
  connect,
  type NatsConnection,
  RetentionPolicy,
  DiscardPolicy,
  AckPolicy,
  DeliverPolicy,
  type ConsumerConfig,
} from 'nats'

let nc: NatsConnection | null = null

export async function getNats(): Promise<NatsConnection> {
  if (nc && !nc.isClosed()) return nc

  nc = await connect({
    servers: process.env.NATS_URL ?? 'nats://localhost:4222',
    name: 'nuxt-orders-service',
    // Survive network blips instead of dying silently:
    maxReconnectAttempts: -1,      // reconnect forever
    reconnectTimeWait: 2000,
    waitOnFirstConnect: true,
  })

  // Log lifecycle so a "connected but idle" state is visible in prod logs.
  ;(async () => {
    for await (const s of nc!.status()) {
      console.log(`[nats] ${s.type}: ${JSON.stringify(s.data ?? '')}`)
    }
  })().catch(() => {})

  return nc
}

/**
 * Create the main stream and the DLQ stream if they don't exist.
 * Safe to call on every boot.
 */
export async function ensureTopology(nc: NatsConnection) {
  const jsm = await nc.jetstreamManager()

  // Main stream — MUST include the subject your producer publishes to.
  await upsertStream(jsm, {
    name: 'ORDERS',
    subjects: ['orders.>'],
    retention: RetentionPolicy.Limits,
    discard: DiscardPolicy.Old,
    max_age: 7 * 24 * 60 * 60 * 1_000_000_000, // 7d in ns
  })

  // Dead-letter stream — durable home for poison messages.
  await upsertStream(jsm, {
    name: 'ORDERS_DLQ',
    subjects: ['dlq.orders.>'],
    retention: RetentionPolicy.Limits,
    discard: DiscardPolicy.Old,
    max_age: 30 * 24 * 60 * 60 * 1_000_000_000, // keep failures longer: 30d
  })
}

async function upsertStream(jsm: any, cfg: any) {
  try {
    await jsm.streams.info(cfg.name)
    await jsm.streams.update(cfg.name, cfg) // keep config in sync
  } catch {
    await jsm.streams.add(cfg)
  }
}

/** Durable pull consumer config for orders.created. */
export function ordersConsumerConfig(): Partial<ConsumerConfig> {
  return {
    durable_name: 'orders-worker',
    filter_subject: 'orders.created',      // EXACT match to what you publish
    ack_policy: AckPolicy.Explicit,        // required for pull + retry
    deliver_policy: DeliverPolicy.All,     // drain the backlog on first run
    ack_wait: 30 * 1_000_000_000,          // 30s to ack before redelivery (ns)
    max_deliver: 5,                        // 5 attempts, then we DLQ + term
    max_ack_pending: 100,                  // in-flight cap / backpressure
    // Escalating backoff between redeliveries (ns). length ~ max_deliver-1.
    backoff: [
      1 * 1_000_000_000,
      5 * 1_000_000_000,
      15 * 1_000_000_000,
      30 * 1_000_000_000,
    ],
  }
}
```

### `server/utils/orders-consumer.ts` — consume loop with retry + DLQ

```ts
import {
  headers as natsHeaders,
  type NatsConnection,
  type JsMsg,
} from 'nats'
import { ensureTopology, ordersConsumerConfig } from './nats'

// Throw this from your handler when the message can never succeed.
export class PoisonMessageError extends Error {}

const MAX_DELIVER = 5

export async function startOrdersConsumer(nc: NatsConnection) {
  await ensureTopology(nc)

  const jsm = await nc.jetstreamManager()
  const js = nc.jetstream()

  // Idempotently create/update the durable consumer.
  const cfg = ordersConsumerConfig()
  try {
    await jsm.consumers.info('ORDERS', cfg.durable_name!)
  } catch {
    await jsm.consumers.add('ORDERS', cfg)
  }

  const consumer = await js.consumers.get('ORDERS', cfg.durable_name!)

  // Long-lived pull loop. `consume()` keeps requesting messages.
  const iter = await consumer.consume({ max_messages: 50 })
  console.log('[orders] consumer started, pulling orders.created')

  ;(async () => {
    for await (const m of iter) {
      await handleWithRetryAndDlq(js, m)
    }
  })().catch((err) => {
    console.error('[orders] consume loop crashed:', err)
    // In prod: trigger a restart of this worker (process exit + supervisor).
  })
}

async function handleWithRetryAndDlq(js: any, m: JsMsg) {
  const attempt = m.info.redeliveryCount // 1 on first delivery
  try {
    await processOrder(m)
    m.ack()
  } catch (err: any) {
    const isPoison = err instanceof PoisonMessageError
    const isLastAttempt = attempt >= MAX_DELIVER

    if (isPoison || isLastAttempt) {
      // ---- Dead-letter it: copy to DLQ stream, THEN term ----
      await deadLetter(js, m, err, attempt, isPoison ? 'poison' : 'max_deliver')
      m.term() // never redeliver
      console.warn(
        `[orders] dead-lettered seq=${m.seq} attempt=${attempt} reason=${err?.message}`,
      )
    } else {
      // ---- Transient: negative-ack; server redelivers per backoff ----
      // (You can also just `return` and let ack_wait expire, but nak is faster.)
      m.nak()
      console.warn(
        `[orders] transient failure seq=${m.seq} attempt=${attempt}, will retry: ${err?.message}`,
      )
    }
  }
}

async function deadLetter(
  js: any,
  m: JsMsg,
  err: unknown,
  attempt: number,
  reason: string,
) {
  const h = natsHeaders()
  h.set('x-dlq-reason', reason)
  h.set('x-dlq-error', String((err as Error)?.message ?? err))
  h.set('x-dlq-origin-subject', m.subject)
  h.set('x-dlq-origin-seq', String(m.seq))
  h.set('x-dlq-attempts', String(attempt))
  h.set('x-dlq-ts', new Date().toISOString())

  // Publish the ORIGINAL payload to the DLQ stream. Await the ack so we
  // don't term() a message we failed to persist.
  await js.publish(`dlq.${m.subject}`, m.data, { headers: h })
}

/** Your real business logic. */
async function processOrder(m: JsMsg) {
  const order = m.json<{ id: string }>() // throws on bad JSON

  if (!order?.id) {
    // Bad shape will never succeed -> poison, straight to DLQ.
    throw new PoisonMessageError('order.id missing')
  }

  // For long work, periodically extend the ack deadline:
  // m.working()

  await doTheActualWork(order) // throws => treated as transient, retried
}

async function doTheActualWork(order: { id: string }) {
  // ... your side effects: DB write, downstream call, etc.
}
```

### `server/plugins/nats.ts` — start it once per Nitro instance

```ts
import { getNats } from '../utils/nats'
import { startOrdersConsumer } from '../utils/orders-consumer'

export default defineNitroPlugin(async () => {
  // IMPORTANT: only start the consumer where a persistent process exists.
  // Skip on serverless/edge builds, or (better) run the consumer in a
  // dedicated worker and never in the request server.
  if (process.env.RUN_CONSUMERS !== 'true') return

  try {
    const nc = await getNats()
    await startOrdersConsumer(nc)
  } catch (err) {
    console.error('[nats] failed to start orders consumer', err)
  }
})
```

And in `nuxt.config.ts`, make sure you're on a persistent server preset:

```ts
export default defineNuxtConfig({
  nitro: {
    preset: 'node-server', // NOT vercel/netlify/edge for a long-lived consumer
  },
})
```

Set `RUN_CONSUMERS=true` only on the worker/server that should host the consumer, so scaling your web tier doesn't spawn duplicate loops.

---

## 4. Publisher side (so messages actually land)

For completeness — publish through JetStream and check the ack. Add a dedupe header for idempotency (JetStream dedupes on `Nats-Msg-Id` within the stream's `duplicate_window`):

```ts
import { headers as natsHeaders } from 'nats'
import { getNats } from '../utils/nats'

export async function publishOrderCreated(order: { id: string }) {
  const nc = await getNats()
  const js = nc.jetstream()

  const h = natsHeaders()
  h.set('Nats-Msg-Id', order.id) // idempotent publish / exactly-once storage

  const ack = await js.publish(
    'orders.created',
    JSON.stringify(order),
    { headers: h },
  )
  if (!ack.stream) throw new Error('order.created was not stored in JetStream')
  return ack // { stream: 'ORDERS', seq, duplicate }
}
```

---

## 5. Summary

**Why it's silent:** almost certainly one of — the producer uses core `nc.publish` instead of `js.publish`; no stream captures `orders.created`; the consumer's `filter_subject` doesn't match; nothing is calling `consume()`/`fetch()`; or (the classic prod-only case) you deployed to a serverless/edge target where the long-lived pull loop can't run. Diagnose with `nats stream info` (messages > 0?) and `nats consumer info` (unprocessed piling up with no delivery = nobody pulling).

**Retry:** set `ack_wait` + `max_deliver` + a `backoff` array; `nak()` transient failures so the server redelivers on the backoff schedule.

**DLQ:** NATS has none built in. Build it — on the final attempt or a poison error, **publish the message to a dedicated DLQ stream**, then **`term()`** so it's not redelivered. Retain and alert on the DLQ stream so failures are visible and replayable.
