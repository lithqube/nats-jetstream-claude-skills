import { jetstreamManager, AckPolicy } from '@nats-io/jetstream'
import { useJetStream } from '../utils/jetstream'

// @ts-expect-error auto-imported by nuxt-nats
const conn = () => useNats()

// Durable background consumer for order.created events.
// Nitro runs this plugin once at server startup.
export default defineNitroPlugin(async (nitroApp) => {
  const nc = conn()
  const durable = 'order-processor'

  const js = await useJetStream() // ensures the ORDERS stream exists

  // Create the durable pull consumer once (idempotent).
  const jsm = await jetstreamManager(nc)
  try {
    await jsm.consumers.add('ORDERS', {
      durable_name: durable,
      ack_policy: AckPolicy.Explicit,
      filter_subject: 'order.created',
    })
  } catch {
    // already exists
  }

  const consumer = await js.consumers.get('ORDERS', durable)
  const messages = await consumer.consume()

  // Background loop — don't await, so startup isn't blocked.
  ;(async () => {
    for await (const m of messages) {
      try {
        const order = m.json()
        await processOrder(order)
        m.ack()
      } catch (err) {
        console.error('order.created processing failed, will redeliver:', err)
        m.nak() // negative ack -> JetStream redelivers
      }
    }
  })().catch((err) => console.error('consumer loop stopped:', err))

  // Stop cleanly on server shutdown.
  nitroApp.hooks.hook('close', () => messages.stop())
})

async function processOrder(order: unknown) {
  // ... your side effects: send email, reserve inventory, etc.
  console.log('processing order', order)
}
