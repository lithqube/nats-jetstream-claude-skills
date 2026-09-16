# nuxt-nats Configuration

The module reads its config from the `nats` key in `nuxt.config.ts`. Every field is overridable at runtime via `NUXT_NATS_*` env vars (Nuxt runtimeConfig), which is how you keep secrets out of the repo.

## Module options (`nats:`)

```ts
nats: {
  servers?: string[]            // TCP URLs; default ['nats://localhost:4222']. In prod pass ALL cluster nodes.
  wsServers?: string[]          // WebSocket URLs; used when transport is 'ws' (falls back to servers if empty)
  transport?: 'auto' | 'tcp' | 'ws'   // default 'auto' (WS on Bun, TCP otherwise)

  // Auth — set exactly ONE method (see priority chain below)
  token?: string
  user?: string
  pass?: string
  nkeySeed?: string             // the SEED STRING itself (starts with 'S'), NOT a file path
  userJwt?: string              // a JWT string; pair with nkeySeed for the JWT+NKey chain

  tls?: { caFile?: string; certFile?: string; keyFile?: string }   // caFile = server TLS; +cert/key = mTLS

  maxReconnectAttempts?: number // default -1 (reconnect forever)

  jsDomain?: string             // JetStream domain (multi-tenant / leaf-node setups)
  jsApiPrefix?: string          // JetStream API prefix

  streams?: StreamDefinition[]     // see below
  consumers?: ConsumerDefinition[] // declarative alternative to defineNatsConsumer()

  health?: { enabled?: boolean; endpoint?: string }  // default enabled at /api/_nats/health
}
```

### StreamDefinition

```ts
{ name, subjects,
  retention?: 'limits' | 'workqueue' | 'interest',
  storage?: 'file' | 'memory',
  replicas?: number,
  maxAge?: string,            // Go-style duration string, e.g. '90d', '30m' — parsed to nanoseconds
  maxBytes?: number,
  duplicateWindow?: string,   // e.g. '2h' — SET THIS; unset relies on the NATS 2-minute default
  provision?: 'startup' | 'update' | 'never' }
```

### ConsumerDefinition

```ts
{ stream, durable,
  filterSubjects?: string[],
  ackPolicy?: 'explicit' | 'none' | 'all',
  ackWait?: number,           // milliseconds (module converts to ns for you)
  maxDeliver?: number,
  backoff?: number[],         // milliseconds
  deadLetterSubject?: string,
  provision?: 'startup' | 'never',
  handler?: string }          // path to a handler module when declared in config
```

## Environment variables

The env-var names changed in the nuxt-nats cutover — a real migration footgun (running an old plugin alongside the new module attempts nkey-only auth against a JWT chain):

| Env var | Overrides |
|---|---|
| `NUXT_NATS_SERVERS` | `servers` — a **comma-separated string of all nodes**; the module splits it |
| `NUXT_NATS_USER_JWT` | `userJwt` (was `NUXT_NATS_JWT`) |
| `NUXT_NATS_NKEY_SEED` | `nkeySeed` (kept its name) |
| `NUXT_NATS_WORKERS` | must be `true` for consumers/agents to run |

## Auth priority chain

Exactly one method is applied; first match wins (`buildConnectionOptions`):

1. `userJwt` **and** `nkeySeed` → `jwtAuthenticator(userJwt, encode(nkeySeed))` — the full JWT+NKey chain (production).
2. `userJwt` alone → `jwtAuthenticator(userJwt)`.
3. `nkeySeed` alone → `nkeyAuthenticator(encode(nkeySeed))`.
4. `token`, then `user`/`pass`, then anonymous.

Setting more than one method is a **silent misconfiguration** — pick one. `nkeyAuthenticator`/`jwtAuthenticator` come from `@nats-io/nats-core`, not `@nats-io/nkeys`.

The module **validates the JWT at boot**: it decodes the payload, errors if `exp` has passed, and warns when less than 24h remains — a cheap way to catch the "creds expired over the weekend" outage. Keep the NKey seed in a secrets manager (both real deployments pull it from Infisical at runtime); it is the only true secret in the chain.

## Provisioning stance

`provision` decides who creates streams/consumers:

- **`'never'` (production default):** the app only *connects and uses*; an out-of-band idempotent provisioner (a `streams.sh` using the `nats` CLI, or Helm/IaC) owns the topology. This avoids N replicas racing to create the same stream on a rolling deploy.
- **`'startup'`:** the module creates from the declared config on boot — convenient for dev, but every replica races.
- **`'update'`:** also issues `streams.update()`, which races even harder on rolling deploys and can flap subjects.

Treat the `streams:` block under `provision: 'never'` as a **description** of a stream another process owns, kept in config for documentation and typing — not as the thing that creates it. Whoever creates the stream first wins its `replicas`/`maxAge`/`duplicateWindow`; a later `update` from another replica only reconciles subjects.
