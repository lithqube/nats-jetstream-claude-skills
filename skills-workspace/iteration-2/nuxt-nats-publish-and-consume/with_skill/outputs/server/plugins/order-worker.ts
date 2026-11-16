// Durable background consumer. Runs ONLY when NUXT_NATS_WORKERS=true, so run
// the worker process with:  NUXT_NATS_WORKERS=true node .output/server/index.mjs
// Must live under server/plugins/** — server/workers/* is NOT auto-scanned by Nitro.
export default defineNitroPlugin(() => {
  defineNatsConsumer({
    stream: 'ORDERS',
    durable: 'order-processor', // binds an EXISTING durable; its config lives in IaC
    async handler(data, msg) {
      // JetStream is at-least-once: guard idempotency yourself (e.g. an inbox
      // table keyed on msg.headers Nats-Msg-Id) before causing side effects.
      await processOrder(data)
      // return normally => ack. throw => nak with backoff (auto-retry then stop
      // at maxDeliver; NATS has no DLQ — use defineDeadLetterConsumer if needed).
    },
  })
})

declare function processOrder(data: { id: string; total: number }): Promise<void>
