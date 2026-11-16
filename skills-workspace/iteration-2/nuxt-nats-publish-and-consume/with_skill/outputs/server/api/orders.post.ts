export default defineEventHandler(async (event) => {
  const body = await readBody<{ id: string; total: number }>(event)

  // msgId => Nats-Msg-Id: a retried publish of the same order id is deduped by
  // the broker within the stream's duplicate_window. Use a deterministic id.
  const ack = await jsPublish('orders.created', body, { msgId: body.id })

  return { ok: true, duplicate: ack.duplicate }
})
