---
name: nuxt-nats
description: Use this skill whenever working in a Nuxt 4 / Nitro project that uses the nuxt-nats module to talk to NATS JetStream — configuring the module in nuxt.config (the `nats:` key), publishing events with jsPublish or corePublish, registering durable pull consumers with defineNatsConsumer, building a dead-letter consumer with defineDeadLetterConsumer, streaming to the browser with useEphemeralConsumer (SSE), using KV/Object Store via useKV/useObj, exposing or calling AI agents with defineNatsAgent/useAgents, typing subjects via the NatsEvents interface, or the NUXT_NATS_WORKERS worker gate. Trigger on any of those symbols, on the env vars NUXT_NATS_SERVERS / NUXT_NATS_USER_JWT / NUXT_NATS_NKEY_SEED / NUXT_NATS_WORKERS, or when a Nuxt app needs server-side NATS messaging. Use this even if the user just says "add NATS to my Nuxt app". For raw @nats-io/* client code outside Nuxt use jetstream-architecture; for server/cluster deployment use jetstream-deployment; for the Synadia agent protocol details use nats-agent-fabric.
---

# nuxt-nats

`nuxt-nats` is a **Nuxt 4 / Nitro server-side module** (config key `nats`) that owns the NATS JetStream connection lifecycle and gives you auto-imported server utilities for publishing, consuming, KV/Object Store, and the Synadia agent fabric. It wraps the modular `@nats-io/*` v3 client — you almost never call `@nats-io/*` directly in a nuxt-nats app.

It is **server-only** (Nitro): there are no browser composables (ADR-002). Publish and consume from `server/` — plugins, API routes, and Nitro tasks.

## What it gives you (auto-imported in `server/`)

| Utility | Purpose |
|---|---|
| `useNats()` / `useJetStream()` / `useJetStreamManager()` | raw connection / JS client / JSM, when you need to drop down |
| `useJetStreamIfAvailable()` | non-throwing `useJetStream()` — returns `null` before the connection is ready (see SSR lifecycle) |
| `jsPublish(subject, data, opts?)` | durable JetStream publish, typed via `NatsEvents`, with `msgId` dedup + client retry |
| `corePublish(subject, payload)` | fire-and-forget core NATS publish (no PubAck) |
| `defineNatsConsumer(opts)` | register a durable **pull** consumer (worker) |
| `defineDeadLetterConsumer(opts)` | build a DLQ by consuming max-deliver / terminated advisories from a stream |
| `useEphemeralConsumer(opts)` | request-scoped ordered ephemeral consumer (SSE) |
| `useKV(bucket, opts?)` / `useObj(bucket, opts?)` | KV bucket / Object Store, cached per process |
| `defineNatsAgent(opts)` / `useAgents()` | host or call AI agents on the Synadia Agent Protocol |

## Reference files

Read the one you need — don't load them all:

- `references/configuration.md` — the full `nats:` module option reference, env-var overrides, the auth priority chain, TLS, JetStream domain/prefix, and the provisioning stance (`provision`). Read when setting up or debugging module config.
- `references/publishing.md` — `jsPublish` vs `corePublish`, the `NatsEvents` typed-subject augmentation, `msgId` dedup, and publish retry. Read when producing events.
- `references/consumers.md` — `defineNatsConsumer`, the `NUXT_NATS_WORKERS` gate, ack/nak/term handling, `defineDeadLetterConsumer`, and `useEphemeralConsumer` for SSE. Read when consuming.
- `references/kv-object.md` — `useKV` / `useObj`, and their real gotchas (Object Store wants a `ReadableStream`, KV TTL units). Read for state/blob storage.
- `references/agents.md` — `defineNatsAgent` / `useAgents` on the Synadia Agent Protocol. Read when building an agent; defer to the `nats-agent-fabric` skill for the wire protocol itself.
- `references/gotchas.md` — the SSR/Nitro lifecycle race, reconnect-storm status semantics, Nitro externals, provisioning races, and testing with Testcontainers. Read this before shipping to production; it's where the non-obvious failures live.

## Minimal setup

```ts
// nuxt.config.ts
export default defineNuxtConfig({
  modules: ['nuxt-nats'],
  nats: {
    servers: ['nats://localhost:4222'],   // override in prod via NUXT_NATS_SERVERS (all cluster nodes)
    // auth is normally injected via env: NUXT_NATS_USER_JWT + NUXT_NATS_NKEY_SEED
    streams: [                             // DESCRIBE streams; do not let the app own them in prod
      { name: 'ORDERS', subjects: ['orders.>'], provision: 'never' },
    ],
  },
})
```

```ts
// server/api/orders.post.ts — publish
export default defineEventHandler(async (event) => {
  const body = await readBody(event)
  await jsPublish('orders.created', body, { msgId: body.id })  // msgId => Nats-Msg-Id dedup
  return { ok: true }
})
```

```ts
// server/plugins/order-worker.ts — consume (runs only when NUXT_NATS_WORKERS=true)
export default defineNitroPlugin(() => {
  defineNatsConsumer({
    stream: 'ORDERS',
    durable: 'order-processor',       // binds an EXISTING durable by default (IaC owns its config)
    async handler(data, msg) {
      await process(data)
      // handler return = ack; throw = nak with backoff; see references/consumers.md
    },
  })
})
```

## Core principles

- **Server-side only.** Publish/consume from `server/`. There are no client composables; don't try to `useNats()` in a Vue component.
- **The worker gate is deliberate.** Consumers and agents run **only when `NUXT_NATS_WORKERS=true`**. Serverless/edge deployments publish fine but must not run long-lived pull loops. The recommended topology is a serverless publisher + a separate persistent worker process. If your consumer "never fires", check this first.
- **`provision: 'never'` in production.** A stream/consumer config is server-side state that belongs in IaC (an idempotent `streams.sh` or Helm), not in N racing app replicas. `provision: 'startup'` is for dev; `'update'` races on rolling deploys. The `streams:` block in config is best read as *documentation* of a stream some other process owns.
- **`defineNatsConsumer` binds; it does not enforce.** By default it binds an existing durable — `filterSubjects`/`ackPolicy` you pass are **not** sent to the broker unless the durable is being created (`provision: 'startup'`). The live durable's config wins; a drift test against a real broker is the only thing that catches a mismatch.
- **NATS has no dead-letter queue.** `maxDeliver` = retry then *stop*, not "route to a DLQ". Use `defineDeadLetterConsumer()` (advisory capture) if you need one — see `references/consumers.md`. Never write a comment claiming redelivery auto-routes to `dlq.*`; it doesn't.
- **Type your subjects.** Augment `NatsEvents` in a `server/**/*.d.ts` so every `jsPublish` is checked. See `references/publishing.md`.
- **Respect the SSR lifecycle race.** Nitro doesn't await async server plugins in order, so the connection may not be ready when a later plugin runs. Use `useJetStreamIfAvailable()` / the module's readiness wait, never a bare `useJetStream()` at plugin top level. See `references/gotchas.md`.
