# Adding NATS to Nuxt 4 with `nuxt-nats`: cluster + JWT/NKey auth + type-safe deduplicated publish

Here's the full setup for connecting to a 3-node production cluster with JWT+NKey auth (secrets from env) and publishing a type-safe, deduplicated `orders.created` event from a server API route.

The key design decisions baked into this answer:

- **Secrets never touch the repo.** The `nats:` block in `nuxt.config` holds only non-secret defaults and a *description* of your stream. The JWT and NKey seed are injected at runtime via `NUXT_NATS_*` env vars.
- **The app connects; it does not own the stream.** In production you use `provision: 'never'` so N app replicas don't race to create the `ORDERS` stream on a rolling deploy. An out-of-band provisioner (a `streams.sh` or Helm/IaC) owns the topology.
- **Deduplication is done with `msgId`**, which sets the `Nats-Msg-Id` header. The broker dedupes re-publishes of the same id within the stream's duplicate window.

---

## 1. `nuxt.config.ts`

```ts
// nuxt.config.ts
export default defineNuxtConfig({
  modules: ['nuxt-nats'],

  nats: {
    // Dev default only. In production this is OVERRIDDEN by NUXT_NATS_SERVERS,
    // which must list ALL THREE cluster nodes (see env section below).
    servers: ['nats://localhost:4222'],

    // Auth: DO NOT put userJwt / nkeySeed here. They are injected at runtime
    // from env (NUXT_NATS_USER_JWT + NUXT_NATS_NKEY_SEED) so secrets stay out
    // of the repo. When both are present the module uses the full JWT+NKey
    // chain: jwtAuthenticator(userJwt, encode(nkeySeed)).

    // DESCRIBE the stream for documentation + typing. Do NOT let the app own it
    // in production: provision: 'never' means the app only connects and uses.
    // An idempotent out-of-band provisioner (streams.sh / Helm) creates it.
    streams: [
      {
        name: 'ORDERS',
        subjects: ['orders.>'],
        // The following fields are documentation under provision: 'never' —
        // whichever process creates the stream first wins these values.
        retention: 'limits',
        storage: 'file',
        replicas: 3,               // match your 3-node cluster
        maxAge: '30d',
        // Dedup horizon. MUST be >= your publish-retry / outbox-drain interval,
        // or a retried publish outside this window will double-book.
        // The NATS default is only 2 minutes — set it explicitly.
        duplicateWindow: '2h',
        provision: 'never',
      },
    ],
  },
})
```

Why `provision: 'never'`: a stream's config is server-side state. Letting each app replica create it on boot (`'startup'`) makes every replica race; `'update'` races even harder on rolling deploys and can flap subjects. Treat the `streams:` block here as a *description* of a stream another process owns, kept for documentation and typing.

---

## 2. Environment variables (the secrets live here, not in the repo)

Set these in your deployment's secret store / runtime env — never commit them. The module reads them via Nuxt `runtimeConfig` and they override the `nats:` block above.

```bash
# ALL THREE cluster nodes, comma-separated. The module splits on commas.
NUXT_NATS_SERVERS="nats://node1.prod:4222,nats://node2.prod:4222,nats://node3.prod:4222"

# JWT+NKey auth chain. Both present => jwtAuthenticator(userJwt, encode(nkeySeed)).
NUXT_NATS_USER_JWT="eyJ0eXAiOiJKV1QiLCJhbGci..."   # the user JWT string
NUXT_NATS_NKEY_SEED="SUAExample...THE_SEED_STRING"  # the SEED STRING itself (starts with 'S'), not a file path
```

Important cluster gotcha: pass **all** nodes as separate comma-separated entries. A comma-joined URL passed as a *single* list entry works against single-node staging and then fails against a real 3-node cluster with an error like `Port could not be cast to integer value as '4222,nats:'`. `NUXT_NATS_SERVERS` is the right lever because the module splits the comma-separated string into a proper list for you.

Notes on the auth chain:

- The env var was renamed in the module cutover: it is `NUXT_NATS_USER_JWT` (formerly `NUXT_NATS_JWT`). `NUXT_NATS_NKEY_SEED` kept its name.
- `nkeySeed` must be the **seed string itself** (starts with `S`), not a path to a `.nk`/creds file.
- Set exactly **one** auth method. Because both `userJwt` and `nkeySeed` are present, the module applies the full JWT+NKey chain (priority #1). Adding a `token` or `user`/`pass` on top would be a silent misconfiguration.
- The NKey seed is the only true secret in the chain — keep it in a secrets manager (Infisical, Vault, etc.) and inject at runtime.
- The module validates the JWT at boot: it decodes the payload, errors if `exp` has passed, and warns when under 24h remains — cheap protection against a "creds expired over the weekend" outage.

---

## 3. Type your subjects (`NatsEvents`)

Augment the `NatsEvents` interface so every `jsPublish` subject and payload is type-checked at compile time. Put it in a `.d.ts` under `server/`.

The file **must** start with the `import type {} from 'nuxt-nats'` line, or the module augmentation silently doesn't apply.

```ts
// server/nats-events.d.ts
import type {} from 'nuxt-nats'

declare module 'nuxt-nats' {
  interface NatsEvents {
    'orders.created': { id: string; total: number; currency: string }
    // add more subjects as you introduce them:
    // 'orders.cancelled': { id: string; reason: string }
  }
}
```

Now `jsPublish('orders.created', ...)` is checked against this shape:

```ts
await jsPublish('orders.created', { id: 'o_123', total: 99.99, currency: 'USD' }) // ✅
await jsPublish('orders.created', { id: 'o_123', foo: 'bar' })                     // ✗ type error
```

---

## 4. Publish from a server API route

`nuxt-nats` is **server-side only** (Nitro) — there are no browser composables. Publish from `server/` (API routes, plugins, or Nitro tasks). `jsPublish` is auto-imported, so no import line is needed.

```ts
// server/api/orders.post.ts
export default defineEventHandler(async (event) => {
  const body = await readBody<{ id: string; total: number; currency: string }>(event)

  // Durable JetStream publish, type-checked against NatsEvents.
  //   - msgId sets Nats-Msg-Id => broker deduplicates a re-publish of the same
  //     id within the stream's duplicate_window (2h, set in nuxt.config).
  //   - Use a deterministic id per logical event so a retried publish can't
  //     double-book. The order id is a natural key here.
  //   - jsPublish also does client-side retry (default 3, exponential backoff)
  //     and returns the PubAck.
  const ack = await jsPublish('orders.created', {
    id: body.id,
    total: body.total,
    currency: body.currency,
  }, {
    msgId: body.id,                          // dedup key
    headers: { 'X-Trace-Id': event.context.traceId ?? '' },
  })

  // ack.duplicate is true if the broker recognized this msgId as a duplicate
  // (i.e. it was already stored within the duplicate window).
  return { ok: true, seq: ack.seq, stream: ack.stream, duplicate: ack.duplicate }
})
```

### What `jsPublish` gives you here

- **Serialization for free.** You pass a plain object; the module JSON-encodes it to bytes.
- **`msgId` is the idempotency lever.** It sets `Nats-Msg-Id`; a re-publish of the same id inside the stream's `duplicate_window` is deduped by the broker. `msgId` is applied last, so a caller can never accidentally override the dedup key through `headers`.
- **Client-side retry.** A JetStream publish can time out under load, so `jsPublish` retries (default 3 attempts, exponential backoff) and returns the `PubAck`. Read `ack.duplicate` to see whether the broker deduped.
- **Header stamping.** `X-Trace-Id` / `X-Correlation-Id` are stamped when you pass them.

### Deterministic `msgId` + `duplicate_window` = the honest exactly-once story

Dedup only holds if the stream's `duplicateWindow` (set to `2h` above) is **at least as long as your retry / outbox-drain interval**. If you later add a transactional outbox — publish inside the same DB transaction that marks the outbox row sent, so a crash after publish but before commit re-runs and re-publishes — the broker's `Nats-Msg-Id` dedup absorbs the duplicate. That gives you at-least-once delivery with idempotent effects, which is the real exactly-once you can build on NATS.

---

## Production checklist / pitfalls to avoid

- **Publishing works on serverless/edge; consuming does not (by default).** Consumers and agents run only when `NUXT_NATS_WORKERS=true`. A publisher-only API route needs nothing extra. The recommended topology is a serverless publisher + a separate persistent worker process for consumers.
- **Don't call `useJetStream()` synchronously at plugin top level.** Nitro doesn't await async server plugins in order, so the connection may not be ready when a later plugin runs — a bare `useJetStream()` there throws into an un-awaited context and the registration is lost for the whole process lifetime. Inside an API request handler (like above) the connection is ready, so `jsPublish` is fine. In plugins, use `useJetStreamIfAvailable()` and wait for readiness.
- **Set `duplicateWindow` explicitly.** The NATS default is 2 minutes; a retry or outbox drain slower than that defeats deduplication.
- **Match staging to prod.** Run a (small) cluster in staging so the single-entry-`servers` bug surfaces before release, and pin the NATS server to an exact patch (2.10 is EOL — don't run a newer server in staging than in prod).
- **Config drift is invisible without a live-broker test.** Under `provision: 'never'`, the real stream config lives on the broker while `nuxt.config` holds a description; they drift silently. Write an integration test (real broker via Testcontainers) that asserts the live stream config equals the declared one.
- **One writer per subject family.** Make a single relay the sole publisher of the `orders.*` money/domain subjects to avoid dual-writes. Prefer putting IDs on the wire and letting consumers re-derive details from the DB; keep schema version in a header/field (`X-Schema-Version`), never in the subject.
