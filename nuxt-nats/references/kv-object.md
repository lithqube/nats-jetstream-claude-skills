# KV and Object Store with nuxt-nats

Both are auto-imported and cached per process (one handle per bucket name).

## `useKV` — Key-Value

```ts
const kv = await useKV('sessions')          // creates with opts, or opens if it exists

await kv.put('user:123', { lastSeen: Date.now() })
const entry = await kv.get('user:123')
if (entry) {
  const value = entry.json<{ lastSeen: number }>()
  console.log(value, 'rev', entry.revision, 'op', entry.operation) // operation 'PUT' | 'DEL' | 'PURGE'
}

// Optimistic concurrency: update only if the revision still matches.
// A stale revision throws, so two writers can't silently clobber each other.
await kv.update('user:123', { lastSeen: Date.now() }, entry.revision)
```

Notes and gotchas:
- **KV TTL is in different units across the ecosystem.** In `@nats-io/kv` (what nuxt-nats uses) the bucket `ttl` is **milliseconds**. In `nats-py` it's **float seconds**. In the `nats` CLI it's a duration string (`--ttl 72h`). Passing seconds where milliseconds are expected makes keys effectively never expire; passing ms where seconds are expected expires them almost immediately.
- **Per-key TTL and expiry markers need server support** (2.11+, bucket created with a marker TTL). Without a marker TTL an expiring key vanishes with **no watcher event**, which silently breaks cache-invalidation designs that watch the bucket. See the `jetstream-architecture` skill's `concepts/server-features.md`.
- KV keys must match `[-/_=.a-zA-Z0-9]+` — canonicalize before use (e.g. replace `:` if you key by things containing it, or accept it since `:` is not in the set — canonicalize deliberately).
- `entry.operation === 'DEL'` / `'PURGE'` are tombstones, not values — check before reading `.json()`.

## `useObj` — Object Store

```ts
const os = await useObj('uploads')

// GOTCHA: put() takes a Web ReadableStream<Uint8Array>, NOT a Node Buffer.
const body = new ReadableStream<Uint8Array>({
  start(c) { c.enqueue(new Uint8Array(buf)); c.close() },
})
await os.put({ name: 'invoice.pdf', description: 'user upload' }, body)

const res = await os.get('invoice.pdf')     // res.data is a ReadableStream, res.info has size/metadata
```

- **The Buffer→ReadableStream wrap is the #1 mistake** — passing a Node `Buffer` directly fails. `get()` likewise returns a stream (`res.data`), not a Buffer; pipe it through your Nitro handler.
- Use `max_chunk_size` in the options to control chunking for large blobs.
- Object Store is a fine blob store for moderate needs (both real apps use it for PDFs), but treat it as a stepping stone — one production system tracks a planned migration to S3 for scale.

## When NOT to reach for these

Plenty of production NATS systems use **neither** KV nor Object Store — if a Postgres table or your existing blob store already fits, a NATS bucket adds an extra failure domain for no gain. Reach for KV when you specifically want a replicated, watchable, TTL'd key space colocated with your streams, and Object Store when you want blobs on the same bus without standing up S3 yet.
