// Worker: processes work.> messages. On the 5th failed attempt it copies the message
// (payload + metadata) to the DLQ and term()s it, so nothing is silently lost.
//
// npm i @nats-io/transport-node @nats-io/jetstream @nats-io/nats-core
import { connect } from "@nats-io/transport-node";
import { jetstream } from "@nats-io/jetstream";
import { headers } from "@nats-io/nats-core";

const MAX_PROCESS_ATTEMPTS = 5;

const nc = await connect({ servers: process.env.NATS_URL ?? "localhost:4222" });
const js = jetstream(nc);

// Replace with your real work.
async function process(m) {
  // throw new Error("boom") to exercise the retry -> DLQ path
  console.log(`processing ${m.subject} (attempt ${m.info.deliveryCount})`);
}

async function toDLQ(m, err) {
  const h = headers();
  h.set("Nats-DLQ-Original-Subject", m.subject);
  h.set("Nats-DLQ-Stream-Seq", String(m.info.streamSequence));
  h.set("Nats-DLQ-Attempts", String(m.info.deliveryCount));
  h.set("Nats-DLQ-Error", String(err?.message ?? err).slice(0, 512));
  h.set("Nats-DLQ-Failed-At", new Date().toISOString());
  // Preserve original headers under a prefix so replay can restore them if needed.
  await js.publish(`dlq.${m.subject}`, m.data, { headers: h });
}

const c = await js.consumers.get("WORK", "worker");
const messages = await c.consume();
console.log("worker consuming work.> ...");

for await (const m of messages) {
  try {
    await process(m);
    m.ack();
  } catch (err) {
    // deliveryCount counts THIS delivery (starts at 1). Republish on the final
    // business attempt rather than nak-ing, because after max_deliver JetStream
    // drops the message with no payload left to capture.
    if (m.info.deliveryCount >= MAX_PROCESS_ATTEMPTS) {
      await toDLQ(m, err);
      m.term(); // done retrying; WorkQueue removes it from WORK
      console.warn(`-> DLQ ${m.subject} after ${m.info.deliveryCount} attempts: ${err?.message}`);
    } else {
      m.nak(); // retry later (optionally m.nak(delayMs) for backoff)
    }
  }
}
