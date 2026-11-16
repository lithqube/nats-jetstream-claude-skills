// Replay: drains WORK_DLQ and republishes each message back to its original work.>
// subject, then acks it (removing it from the DLQ). Run after you've fixed the bug.
//
// Add a durable DLQ consumer on first run; re-runs reuse it.
// npm i @nats-io/transport-node @nats-io/jetstream
import { connect } from "@nats-io/transport-node";
import { jetstream, jetstreamManager } from "@nats-io/jetstream";

const nc = await connect({ servers: process.env.NATS_URL ?? "localhost:4222" });
const jsm = await jetstreamManager(nc);
const js = jetstream(nc);

await jsm.consumers.add("WORK_DLQ", {
  durable_name: "replayer",
  ack_policy: "explicit",
  ack_wait: 30 * 1_000_000_000,
}).catch(() => {}); // already exists -> ignore

const c = await js.consumers.get("WORK_DLQ", "replayer");
let replayed = 0;

// Pull in batches; stop when the DLQ is empty (no message within the expiry window).
const iter = await c.fetch({ max_messages: 500, expires: 2000 });
for await (const m of iter) {
  const original = m.headers?.get("Nats-DLQ-Original-Subject");
  if (!original) {
    console.warn(`dlq msg seq ${m.info.streamSequence} has no original subject, skipping`);
    m.nak();
    continue;
  }
  await js.publish(original, m.data); // back onto the WORK stream for reprocessing
  m.ack(); // removes it from the DLQ
  replayed++;
}

console.log(`replayed ${replayed} message(s) back to WORK`);
await nc.drain();
