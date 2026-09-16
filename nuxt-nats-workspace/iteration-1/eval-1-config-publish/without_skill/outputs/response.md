# Adding NATS to a Nuxt 4 app: cluster + JWT/NKey auth + type-safe, deduplicated publish

Below is an end-to-end setup for a Nuxt 4 app using the `nuxt-nats` module to connect to a 3-node production cluster with JWT + NKey (decentralized) authentication, plus a server API route that publishes an `orders.created` event to JetStream with type safety and deduplication.

The important design points:

- **Secrets stay out of the repo.** The user JWT and the NKey seed are read from environment variables via Nuxt's `runtimeConfig`, which maps to `NUXT_*` env vars at runtime.
- **Cluster connectivity** is handled by passing all 3 seed servers. The NATS client discovers the rest of the cluster topology automatically and reconnects/fails over between nodes.
- **Deduplication** is a JetStream feature: you set a `Nats-Msg-Id` on publish (via the `msgID` option), and the stream is configured with a duplicate-detection window. Within that window, JetStream drops any message with a repeated ID.
- **Type safety** comes from a shared TypeScript interface plus a typed JSON codec.

---

## 1. Install

```bash
# the Nuxt module + the official NATS JS client
npm install nuxt-nats
npm install nats
```

> Note: `nuxt-nats` wraps the official `nats.js` client. Depending on the module version, it may re-export the client for you; installing `nats` directly guarantees you have the JetStream API and authenticators available on the server side. All the JetStream/publish logic below uses the underlying `nats` client, which is the stable, well-documented surface — so this works whether or not your module version exposes a convenience helper.

---

## 2. Environment variables (never commit these)

Put these in your deployment environment (or a local, git-ignored `.env`):

```bash
# .env  (add .env to .gitignore)

# The three cluster nodes (TLS in production)
NUXT_NATS_SERVERS="tls://node1.nats.prod.internal:4222,tls://node2.nats.prod.internal:4222,tls://node3.nats.prod.internal:4222"

# Decentralized auth material — user JWT + NKey seed (the account/user seed, starts with 'S')
NUXT_NATS_USER_JWT="eyJ0eXAiOiJKV1QiLCJhbGciOiJlZDI1NTE5LW5rZXkifQ..."
NUXT_NATS_NKEY_SEED="SUAEXAMPLEDONOTCOMMITSEEDGOESHERE..."
```

Because these keys sit under `runtimeConfig` (see below) and are **not** prefixed for the `public` block, they are only ever available on the server — they are never shipped to the browser bundle.

---

## 3. `nuxt.config.ts`

```ts
export default defineNuxtConfig({
  modules: ['nuxt-nats'],

  // Server-only secrets. These are overridden at runtime by the matching
  // NUXT_* env vars, so nothing sensitive lives in the repo.
  runtimeConfig: {
    nats: {
      // "servers" can be a comma-separated string; we split it in the plugin/route.
      servers: '',      // <- filled by NUXT_NATS_SERVERS
      userJwt: '',      // <- filled by NUXT_NATS_USER_JWT
      nkeySeed: '',     // <- filled by NUXT_NATS_NKEY_SEED
    },
  },

  // Optional: module-level options. Many nuxt-nats versions accept a `nats`
  // block here for a default connection. Keep auth OUT of here (it belongs in
  // runtimeConfig/env) — use this only for non-secret defaults if you want the
  // module to auto-connect. If your version doesn't support programmatic
  // authenticators via config, prefer the explicit server-side connection in
  // section 4 below.
  nats: {
    // name shown in `nats server report connections`
    name: 'nuxt-orders-service',
  },
})
```

> Why not put the JWT/seed in the `nats:` module block? Two reasons: (1) module options are static config and are the wrong place for secrets, and (2) NKey/JWT auth requires a programmatic **authenticator** (a signing callback), which is cleanest to build on the server with the values from `runtimeConfig`. That's what the next section does.

---

## 4. A shared, cached JetStream connection (server utility)

Create a server utility so every API route reuses **one** connection instead of reconnecting per request.

`server/utils/nats.ts`:

```ts
import {
  connect,
  jwtAuthenticator,
  JSONCodec,
  type NatsConnection,
  type JetStreamClient,
  type JetStreamManager,
} from 'nats'

// ---- Type-safe event contracts ----
export interface OrderCreated {
  orderId: string
  customerId: string
  totalCents: number
  currency: 'USD' | 'EUR' | 'GBP'
  createdAt: string // ISO-8601
}

// One codec instance, typed to our event.
export const orderCreatedCodec = JSONCodec<OrderCreated>()

let nc: NatsConnection | null = null

export async function getNats(): Promise<NatsConnection> {
  if (nc && !nc.isClosed()) return nc

  const cfg = useRuntimeConfig().nats

  const servers = cfg.servers
    .split(',')
    .map((s) => s.trim())
    .filter(Boolean)

  if (!servers.length) throw new Error('NATS: no servers configured (NUXT_NATS_SERVERS)')
  if (!cfg.userJwt || !cfg.nkeySeed) {
    throw new Error('NATS: missing JWT/NKey credentials (NUXT_NATS_USER_JWT / NUXT_NATS_NKEY_SEED)')
  }

  // JWT + NKey authenticator. The seed is used to sign the server nonce;
  // the JWT identifies the user/account. Seed must be a Uint8Array of the
  // ASCII characters (NOT decoded).
  const authenticator = jwtAuthenticator(
    cfg.userJwt,
    new TextEncoder().encode(cfg.nkeySeed),
  )

  nc = await connect({
    servers,                 // all 3 nodes -> automatic cluster discovery & failover
    authenticator,
    name: 'nuxt-orders-service',
    maxReconnectAttempts: -1, // retry forever
    reconnectTimeWait: 2000,  // 2s between attempts
    // tls: {}                // present TLS certs here if the cluster requires mTLS
  })

  // Surface connection lifecycle in logs (optional but recommended).
  ;(async () => {
    for await (const status of nc!.status()) {
      // status.type: 'disconnect' | 'reconnect' | 'update' | 'ldm' | 'error' ...
      console.log(`[nats] ${status.type}`, status.data ?? '')
    }
  })().catch(() => {})

  return nc
}

export async function getJetStream(): Promise<JetStreamClient> {
  const conn = await getNats()
  return conn.jetstream()
}

// Ensure the ORDERS stream exists with a duplicate-detection window.
// Run this once at boot (or manage the stream via ops/CLI in production).
export async function ensureOrdersStream(): Promise<void> {
  const conn = await getNats()
  const jsm: JetStreamManager = await conn.jetstreamManager()

  const streamName = 'ORDERS'
  const subjects = ['orders.>']

  try {
    await jsm.streams.info(streamName)
    // Stream exists; you could update() it here if config drifted.
  } catch {
    await jsm.streams.add({
      name: streamName,
      subjects,
      // 2-minute dedup window: repeated Nats-Msg-Id within this window is dropped.
      duplicate_window: 2 * 60 * 1_000_000_000, // nanoseconds
      // retention/limits are workload-specific; tune for production.
    })
  }
}
```

Key auth detail: `jwtAuthenticator(jwt, seed)` takes the **user JWT** and the **NKey seed**. The client never sends the seed over the wire — it uses it locally to sign the server's nonce, proving possession of the key. This is the standard decentralized (operator/account/user) auth model.

---

## 5. Initialize the stream at server startup (optional)

`server/plugins/nats.ts`:

```ts
export default defineNitroPlugin(async () => {
  try {
    await ensureOrdersStream()
    console.log('[nats] ORDERS stream ready')
  } catch (err) {
    // Don't crash boot on a transient NATS outage; the route will retry.
    console.error('[nats] stream init failed', err)
  }
})
```

In a strict production setup you often provision streams out-of-band (Terraform, the `nats` CLI, or a migration job) rather than from the app. Either is fine — just make sure the `duplicate_window` is set on the stream, because that's what makes deduplication work.

---

## 6. The publish route

`server/api/orders.post.ts`:

```ts
import { headers } from 'nats'
import { getJetStream, orderCreatedCodec, type OrderCreated } from '~/server/utils/nats'

export default defineEventHandler(async (event) => {
  const body = await readBody<Partial<OrderCreated>>(event)

  // --- validate / build a fully-typed payload ---
  if (!body?.orderId || !body?.customerId || typeof body?.totalCents !== 'number') {
    throw createError({ statusCode: 400, statusMessage: 'orderId, customerId, totalCents are required' })
  }

  const payload: OrderCreated = {
    orderId: body.orderId,
    customerId: body.customerId,
    totalCents: body.totalCents,
    currency: body.currency ?? 'USD',
    createdAt: new Date().toISOString(),
  }

  const js = await getJetStream()

  // Deduplication: the msgID becomes the "Nats-Msg-Id" header. JetStream drops
  // duplicates that arrive within the stream's duplicate_window. Use a stable,
  // business-meaningful ID (the order id) so a retried publish is idempotent.
  const ack = await js.publish(
    'orders.created',
    orderCreatedCodec.encode(payload),
    {
      msgID: payload.orderId,   // <- idempotency / dedup key
      timeout: 5000,
      // Optional safety: only accept if published to the ORDERS stream.
      expect: { streamName: 'ORDERS' },
    },
  )

  return {
    ok: true,
    stream: ack.stream,
    seq: ack.seq,
    // true when JetStream recognized this msgID as a duplicate and did NOT
    // store a new copy — the publish is still a success from your side.
    duplicate: ack.duplicate === true,
  }
})
```

### How the deduplication behaves

- First `POST` with `orderId: "A-1001"` → stored, `duplicate: false`, a new stream sequence is returned.
- A retry (network blip, client re-send) with the **same** `orderId` inside the 2-minute window → JetStream returns an ack with `duplicate: true` and does **not** store a second copy. Your consumers see the event exactly once.
- The same `orderId` after the window has elapsed → treated as new. So size `duplicate_window` to be comfortably larger than your worst-case retry/replay horizon.

Using `orderId` (a real business key) as the `msgID` is what makes the publish *idempotent* — safe to call more than once.

---

## 7. Consuming (for completeness)

A durable pull consumer to process the events elsewhere:

```ts
import { AckPolicy } from 'nats'
import { getNats, orderCreatedCodec } from '~/server/utils/nats'

export async function consumeOrders() {
  const nc = await getNats()
  const js = nc.jetstream()

  const c = await js.consumers.get('ORDERS', {
    // create/get a durable consumer bound to orders.created
    durable_name: 'orders-worker',
    ack_policy: AckPolicy.Explicit,
    filter_subject: 'orders.created',
  } as any)

  const messages = await c.consume()
  for await (const m of messages) {
    const order = orderCreatedCodec.decode(m.data) // typed as OrderCreated
    try {
      // ... handle order ...
      m.ack()
    } catch {
      m.nak() // redeliver later
    }
  }
}
```

---

## Summary / checklist

- **Cluster**: pass all 3 `tls://` node URLs to `servers`; the client auto-discovers and fails over.
- **Auth**: `jwtAuthenticator(userJwt, seed)` with both values pulled from `runtimeConfig` → `NUXT_NATS_*` env vars. Secrets never touch the repo or the client bundle (they live under the private `runtimeConfig`, not `runtimeConfig.public`).
- **Type safety**: a shared `OrderCreated` interface + `JSONCodec<OrderCreated>()` used on both publish and consume.
- **Deduplication**: stream configured with a `duplicate_window`; publish with `msgID: orderId` so retries are idempotent and JetStream reports `duplicate: true` on repeats.
- **Connection reuse**: a `server/utils/nats.ts` singleton so routes share one authenticated connection.

> Version note: `nuxt-nats`'s exact config surface and helper names can vary between releases. The connection/auth/JetStream code above targets the stable official `nats` client API, so it works regardless of the module version. Check your installed module's docs if you want to use its auto-injected connection helper instead of the explicit `getNats()` utility — but keep the JWT/NKey values in `runtimeConfig`/env either way.
