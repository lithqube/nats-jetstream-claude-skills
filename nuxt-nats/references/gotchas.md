# nuxt-nats Production Gotchas

The non-obvious failures, drawn from the module's own field notes and two production deployments. Read this before shipping.

## SSR / Nitro lifecycle race — the one that bites hardest

**Nitro (nitropack 2) calls server plugins in order without awaiting async ones.** So a plugin that runs after the nuxt-nats connection plugin executes while the connection/JetStream client is still `undefined`. A bare `useJetStream()` at plugin top level throws into an un-awaited async context, the registration is lost for the whole process lifetime, and nothing recovers it.

- Confirmed live on staging: durable consumers died at every restart despite the broker connecting within a second.
- **Fix:** never call `useJetStream()` synchronously at plugin top level. Use `useJetStreamIfAvailable()` (returns `null` until ready) and poll/await readiness before registering, or register inside the module's readiness hook. The module's own consumers poll `getJetStream()` every 250ms; agents poll the connection.
- The module publishes the JS client **only after stream provisioning completes**, so a boot-time consumer never races its own stream's creation.

## Reconnect storm — status fires per attempt, not per recovery

The client emits a `reconnect` status **per retry attempt**, not once per recovery — a single outage has produced thousands of events (one report: ~2400). If you wire an `onReconnect` side effect (cache warm, re-subscribe, alert), gate it on an actual `disconnect → reconnect` transition (a `_wasDisconnected` flag), or you'll fire it thousands of times per blip. Auth failures surface in the status stream too: messages containing `Authorization` / `Permissions Violation` usually mean an expired or under-permissioned JWT, not a network problem.

## Nitro externals are required

All `@nats-io/*` and `@synadia-ai/*` packages must be marked external in the Nitro build (`nitroConfig.externals.external`) — the module does this, but if you fork or hand-roll config, know that `@nats-io/transport-node` uses Node's `net` module and Rollup bundling severs the `net.Socket` prototype chain, failing at runtime with `Cannot read properties of undefined`.

## Graceful shutdown ordering

Nitro's `close` hook is unreliable (nitro#4015), so the module's real shutdown path is `SIGTERM`/`SIGINT` handlers. The order is deliberate and each step is error-isolated: **stop agents → close agents → stop consumers → `nc.drain()`**. Consumers are stopped *before* the drain so acks don't race the closing connection. If you add your own teardown, drain last.

## Cluster connection: pass every node

Pass **all** cluster nodes to `servers` (comma-separated in `NUXT_NATS_SERVERS`; the module splits it). Passing a comma-joined URL as a *single* list entry works against single-node staging and then fails against a real 3-node cluster with an error like `Port could not be cast to integer value as '4222,nats:'` — a bug that hides in staging and only appears in production. Make your staging topology match prod (a cluster, even if small) so this surfaces before release.

## Config drift is invisible without a live-broker test

Because a stream/consumer's real config lives on the broker (`provision: 'never'`) while your `nuxt.config` holds a *description* of it, the two drift silently. `defineNatsConsumer` binds the live durable and does not push your declared `filterSubjects`/`ackPolicy`. The only thing that catches a mismatch is an **integration test against a real broker** that asserts the live stream/consumer config equals the declared one. Write that test.

Related operational failure: a durable consumer's config is **immutable**. Changing `ackWait`/`deliver_policy`/filter and redeploying fails with `consumer name already in use with different configuration` (or `configuration requests deliver policy to be 2, but consumer's value is 0`), the old durable keeps holding the subject, and lag grows unbounded. Fix by removing and recreating: `nats consumer rm <stream> <durable> -f` (note: `-f`, the CLI does not accept `--force` here), then redeploy.

## Testing: real broker via Testcontainers, no mocks

Integration tests run a **real `nats:<pinned>-alpine` with JetStream** via `@testcontainers/nats`, wired into the module's singletons through a test-only setter — no NATS mocks. Run integration suites single-fork so the container is shared. Unit tests mock the connection singleton module (kept import-free of `nitropack/runtime` on purpose) rather than the connection plugin. For JWT auth, build a full operator→account→user chain with `@nats-io/jwt` against a `resolver: MEMORY` + `resolver_preload` server config as a runnable fixture.

## Version hygiene

- The module is pre-1.0 (0.1.x beta/alpha) and its internals shift between releases — pin an exact version and read the CHANGELOG on upgrade; several documented workarounds exist precisely to bridge beta-to-beta behavior changes.
- Pin the **NATS server** to an exact patch, never a floating minor. 2.10 is EOL; don't run staging on a newer server (2.14) than production (2.10) — you'd be testing against a different server than you ship.
