import { jetstream, jetstreamManager } from '@nats-io/jetstream'
import type { JetStreamClient } from '@nats-io/jetstream'

// nuxt-nats exposes the server-side NatsConnection as a Nitro server util.
// If your nuxt-nats version names it differently, this is the one line to change.
// @ts-expect-error auto-imported by nuxt-nats
const conn = () => useNats()

let ready: Promise<JetStreamClient> | undefined

// Ensure the ORDERS stream exists once, then hand back the JetStream client.
// Idempotent: add() throws if it already exists, so we fall back to update/get.
export function useJetStream(): Promise<JetStreamClient> {
  ready ??= (async () => {
    const nc = conn()
    const jsm = await jetstreamManager(nc)
    const cfg = { name: 'ORDERS', subjects: ['order.>'] }
    try {
      await jsm.streams.add(cfg)
    } catch {
      await jsm.streams.info(cfg.name) // already there; leave its config alone
    }
    return jetstream(nc)
  })()
  return ready
}
