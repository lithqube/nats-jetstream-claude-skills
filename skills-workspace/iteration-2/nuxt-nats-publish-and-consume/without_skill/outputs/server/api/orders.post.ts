import { useJetStream } from '../utils/jetstream'

// POST /api/orders -> publish an order.created event to JetStream.
export default defineEventHandler(async (event) => {
  const body = await readBody<{ items?: unknown[]; total?: number }>(event)

  const order = {
    id: crypto.randomUUID(),
    items: body?.items ?? [],
    total: body?.total ?? 0,
    createdAt: new Date().toISOString(),
  }

  const js = await useJetStream()
  // msgID makes the publish idempotent if the client retries.
  const ack = await js.publish(
    'order.created',
    JSON.stringify(order),
    { msgID: order.id },
  )

  return { order, stream: ack.stream, seq: ack.seq }
})
