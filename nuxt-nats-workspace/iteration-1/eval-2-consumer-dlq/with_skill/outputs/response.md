# Your durable pull consumer isn't firing in prod — here's what's actually wrong

"NATS connected fine" and "the consumer is running" are two different things in a `nuxt-nats` app. The connection succeeding tells you the client reached the broker; it tells you nothing about whether your durable consumer ever registered. In every case I've seen, a durable that logs a healthy connection but receives zero messages is one of the four things below — and the first one accounts for the large majority.

## 1. The worker gate: `NUXT_NATS_WORKERS=true` (check this first)

`nuxt-nats` **does not start durable consumers unless `NUXT_NATS_WORKERS=true`.** This is deliberate. Long-lived pull loops must not run on serverless/edge, so without the flag the app boots, connects, publishes, and serves ephemeral (SSE) consumers just fine — and silently skips every `defineNatsConsumer`, logging a skip warning (which is easy to miss under a healthy "NATS connected" line).

This is almost certainly your problem. In production the recommended topology is two roles from the same build:

- a **publisher** (your web/API dynos, workers off) that only calls `jsPublish` — fine on serverless/edge,
- a separate **persistent worker process** (workers on) that runs the pull loops.

```bash
# The worker process — the ONLY place durable consumers run
NUXT_NATS_WORKERS=true node .output/server/index.mjs
```

Look in your worker's boot logs for the skip warning. If your `orders.created` consumer only ever ran locally (where you probably had the flag set in a `.env`) and never in the prod worker, that's the whole bug. Set the flag on the worker role and it will start consuming.

## 2. Where the consumer is declared: it must be a Nitro server plugin

`defineNatsConsumer` has to be registered inside a **Nitro server plugin** — `server/plugins/**`. A very common trap: people put worker code in `server/workers/*.ts` because the name reads well. **Nitro does not auto-scan `server/workers/`**, so a consumer declared there never registers and never logs an error. Same outcome — zero messages, healthy connection.

Move it to `server/plugins/`.

## 3. The SSR / Nitro lifecycle race (a bare `useJetStream()` at plugin top level)

Nitro calls server plugins in order but **does not await async ones**. So if your consumer plugin does a bare `useJetStream()` at the top level, it can run while the JetStream client is still `undefined`, the call throws into an un-awaited async context, and the registration is lost for the entire process lifetime — nothing recovers it. This one reproduces as "durable consumers die at every restart even though the broker connects within a second."

`defineNatsConsumer` itself already polls for readiness internally, so if you just use it directly you're fine. The danger is hand-rolled code around it. If you need the client for anything else at plugin scope, use the non-throwing accessor and wait for readiness — never a bare `useJetStream()`:

```ts
const js = useJetStreamIfAvailable() // returns null until the connection is ready
if (!js) { /* not ready yet — poll / await, don't throw */ }
```

## 4. Config drift: the durable's live filter may not include `orders.created`

This is the subtle one, and it's specific to how `nuxt-nats` binds durables. **`defineNatsConsumer` binds an existing durable; it does not enforce your config.** By default the `filterSubjects` / `ackPolicy` / `ackWait` / `maxDeliver` you pass in code are **only** used when *creating* a durable (`provision: 'startup'`, which you should not use in prod). When it binds an existing durable, **the live durable's config on the broker wins** — anything you pass that disagrees is reported, not applied.

So if the durable that actually exists on your prod broker was created (by your IaC / `nats` CLI) with a `filter_subject` of, say, `orders.*.created` or `orders.shipped`, your handler will bind successfully, log nothing wrong, and receive nothing — because the broker-side filter doesn't match `orders.created`. Your code says one thing; the broker says another; the broker wins.

Two things to verify against the **real broker**, not your config file:

```bash
nats consumer info ORDERS order-processor
# check: Filter Subject(s) actually includes orders.created
# check: the consumer is a PULL consumer (Delivery: pull)
# check: Num Pending / Unprocessed — are messages even landing in the stream for this subject?

nats stream info ORDERS
# confirm the stream's subjects cover orders.created and messages are arriving
```

While you're there, two related failure modes:

- **A durable's config is immutable.** If someone changed the filter/`ackWait`/`deliver_policy` and redeployed, creation fails with `consumer name already in use with different configuration` and the *old* durable keeps holding the subject while lag grows. Fix by recreating: `nats consumer rm ORDERS order-processor -f` (note `-f`, not `--force`), then let IaC recreate it.
- The only thing that catches this drift automatically is an **integration test against a real broker** asserting the live consumer config equals your declared one. Write that test — config drift is invisible otherwise.

---

# The retry + dead-letter requirement — and the thing everyone gets wrong

> **NATS has no dead-letter queue.** `maxDeliver` means *retry then stop*, not "route to a DLQ."

After the delivery limit, the server stops redelivering and publishes a `$JS.EVENT.ADVISORY.CONSUMER.MAX_DELIVERIES` advisory. The message stays in the stream but **nothing hands it to you**. If you (or a comment in your codebase) assume "NATS routes to `dlq.*` after `max_deliver`," you are silently dropping every poison message after N attempts. A real production codebase shipped exactly that wrong comment with no advisory listener and lost messages for months.

So dead-lettering is two separate pieces:

1. **Retry / poison handling inside the handler** (below).
2. **A real DLQ you build yourself** by durably capturing the advisory subjects (further below).

## Retry vs. dead-letter, decided in the handler

The handler's default contract in `nuxt-nats` is: **return = ack, throw = nak with backoff.** That's enough for simple transient retries. But to distinguish *retry later* from *give up forever*, act on `msg` directly. The rules that matter:

- **`msg.nak(delayMs)` for a transient failure** — retry later. Critically, **always pass a delay.** A bare `msg.nak()` redelivers *immediately* and can burn all your `maxDeliver` attempts in milliseconds, which defeats the point of retrying a transient outage.
- **`msg.term(reason)` for a poison message** you will never process (malformed body, missing required header). `term()` stops redelivery immediately and records the reason. Naking a poison message just loops it to `maxDeliver` for no reason.
- **`msg.ack()` to drop** a message you can't use but don't want redelivered (e.g. a body you've logged and given up on).
- **Guard idempotency yourself** — JetStream is at-least-once, so retries *will* redeliver. Keep an inbox/processed table keyed on `(Nats-Msg-Id, handler)` with `INSERT ... ON CONFLICT DO NOTHING`, and skip if already processed. A message with **no `Nats-Msg-Id`** can't be deduped and a redelivery can never gain the header, so `term()` it rather than nak (nak would loop forever).

## What your consumer should look like

```ts
// server/plugins/order-worker.ts
// Runs ONLY when NUXT_NATS_WORKERS=true. MUST live under server/plugins/.
export default defineNitroPlugin(() => {
  defineNatsConsumer({
    stream: 'ORDERS',
    durable: 'order-processor',        // binds the EXISTING durable; its real config lives in IaC
    // deadLetterSubject is a label used by your DLQ tooling; it does NOT make NATS auto-route.
    deadLetterSubject: 'dlq.orders.created',

    async handler(data, msg) {
      // 1) Reject un-dedupable messages permanently — a redelivery can never gain the header.
      const msgId = msg.headers?.get('Nats-Msg-Id')
      if (!msgId) {
        msg.term('missing Nats-Msg-Id')      // poison: stop redelivery, record reason
        return
      }

      // 2) Idempotency guard — JetStream is at-least-once.
      const fresh = await claimOnce(msgId, 'order-processor') // INSERT ... ON CONFLICT DO NOTHING
      if (!fresh) {
        msg.ack()                            // already processed — drop the duplicate
        return
      }

      // 3) Validate. Structurally broken payloads are poison, not transient.
      let order
      try {
        order = parseOrder(data)             // throws on malformed/invalid shape
      } catch (err) {
        msg.term(`unparseable order: ${(err as Error).message}`)
        return
      }

      // 4) Do the work. Distinguish transient from permanent failures.
      try {
        await processOrder(order)
        msg.ack()                            // success
      } catch (err) {
        if (isTransient(err)) {
          // DB blip, downstream 503, timeout — retry LATER (never a bare nak()).
          msg.nak(30_000)                    // 30s backoff; server retries up to maxDeliver
        } else {
          // Business-rule rejection we'll never recover from → poison.
          msg.term(`permanent failure: ${(err as Error).message}`)
        }
      }
    },
  })
})
```

`maxDeliver` and `ackWait` are set on the **durable in your IaC**, not here (remember: binding doesn't enforce them). A sane starting point is `maxDeliver: 5` with a backoff schedule and an `ackWait` comfortably longer than your slowest handler run.

## The actual DLQ — capture advisories into a stream and consume them durably

Because the message stays in the source stream and only an *advisory* is emitted, build the DLQ by consuming those advisories durably. `nuxt-nats` gives you `defineDeadLetterConsumer` which does it correctly:

```ts
// server/plugins/dlq-router.ts  (also gated behind NUXT_NATS_WORKERS=true)
export default defineNitroPlugin(() => {
  defineDeadLetterConsumer({
    stream: 'JS_ADVISORY',        // a stream that CAPTURES the advisory subjects (durable, not core-sub)
    durable: 'dlq-router',
    // NOTE: give the DLQ consumer NO deadLetterSubject of its own —
    // a failing dead-letter handler must never dead-letter itself into a loop.
    async handler(event) {
      // event carries the original subject + delivery count, and recovers the original
      // message by sequence via jsm.streams.getMessage(). An aged-out message is null (expected).
      await recordDeadLetter(event)   // persist to your DLQ table / alert / ops queue
    },
  })
})
```

Two mistakes this avoids — and you must avoid them too if you hand-roll it:

1. **Don't subscribe to advisories with core NATS.** Advisories are fire-and-forget; anything published while your subscriber restarts or is network-partitioned is gone forever. You need JetStream durability, which means capturing the advisory subjects into a **stream** and consuming that stream with a durable.
2. **Don't use the account-wide advisory firehose** `$JS.EVENT.ADVISORY.>` — it fires an audit advisory on *every* JetStream API call and will bury you. Capture only the two you care about:
   - `$JS.EVENT.ADVISORY.CONSUMER.MAX_DELIVERIES.>`
   - `$JS.EVENT.ADVISORY.CONSUMER.MSG_TERMINATED.>`

The `JS_ADVISORY` stream that captures those subjects is server-side state — provision it in IaC alongside `ORDERS`, with `provision: 'never'` in the app config:

```bash
# streams.sh (idempotent, owned by IaC — NOT by racing app replicas)
nats stream add JS_ADVISORY \
  --subjects '$JS.EVENT.ADVISORY.CONSUMER.MAX_DELIVERIES.>,$JS.EVENT.ADVISORY.CONSUMER.MSG_TERMINATED.>' \
  --storage file --retention limits --max-age 720h --defaults

nats consumer add JS_ADVISORY dlq-router --pull --ack explicit --defaults
```

```ts
// nuxt.config.ts — describe streams; let IaC own them in prod
export default defineNuxtConfig({
  modules: ['nuxt-nats'],
  nats: {
    servers: ['nats://localhost:4222'],  // override in prod via NUXT_NATS_SERVERS (all cluster nodes)
    streams: [
      { name: 'ORDERS',      subjects: ['orders.>'],                         provision: 'never' },
      { name: 'JS_ADVISORY', subjects: ['$JS.EVENT.ADVISORY.CONSUMER.MAX_DELIVERIES.>',
                                        '$JS.EVENT.ADVISORY.CONSUMER.MSG_TERMINATED.>'], provision: 'never' },
    ],
  },
})
```

---

# TL;DR checklist for the "connected but not receiving" symptom

1. **Is `NUXT_NATS_WORKERS=true` on the worker process?** Almost always the answer. Publishers don't run consumers by design.
2. **Is the consumer under `server/plugins/`?** `server/workers/` is not auto-scanned — it registers nothing.
3. **Any bare `useJetStream()` at plugin top level?** The SSR race silently loses the registration. Use `useJetStreamIfAvailable()` and wait for readiness.
4. **Does the *live* durable's `filter_subject` actually include `orders.created`, and is it a pull consumer?** Binding doesn't enforce your code's config — check the broker with `nats consumer info ORDERS order-processor`. Recreate with `nats consumer rm ORDERS order-processor -f` if it drifted.
5. **For dead-lettering:** `maxDeliver` is *retry then stop*, not auto-DLQ. Use `msg.nak(delayMs)` for transient / `msg.term(reason)` for poison in the handler, and build the real DLQ with `defineDeadLetterConsumer` capturing the `MAX_DELIVERIES` / `MSG_TERMINATED` advisories from a durable stream — never a core-NATS subscribe, never the `$JS.EVENT.ADVISORY.>` firehose.

Once (1)–(4) are fixed the consumer will start receiving; (5) makes its failures durable and observable instead of silently dropped.
