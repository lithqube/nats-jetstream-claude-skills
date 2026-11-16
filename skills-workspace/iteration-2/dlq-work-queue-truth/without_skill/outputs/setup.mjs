// Creates the work-queue stream, a separate DLQ stream, and the worker consumer.
// Run once. Idempotent-ish: re-running throws if they already exist — that's fine.
//
// npm i @nats-io/transport-node @nats-io/jetstream
import { connect } from "@nats-io/transport-node";
import { jetstreamManager } from "@nats-io/jetstream";

const MAX_PROCESS_ATTEMPTS = 5;

const nc = await connect({ servers: process.env.NATS_URL ?? "localhost:4222" });
const jsm = await jetstreamManager(nc);

// Work queue: WorkQueuePolicy means a message is removed once it is acked/termed
// and no consumer still has interest in it. Only ONE consumer may bind per subject.
await jsm.streams.add({
  name: "WORK",
  subjects: ["work.>"],
  retention: "workqueue",
});

// DLQ is a SEPARATE stream (normal limits retention) so failed messages survive
// after WORK deletes the original. Never point the DLQ subject at the WORK stream.
await jsm.streams.add({
  name: "WORK_DLQ",
  subjects: ["dlq.>"],
  retention: "limits",
  max_age: 14 * 24 * 60 * 60 * 1_000_000_000, // 14d in ns; tune to your inspect/replay window
});

// max_deliver is 6, one more than our business limit of 5. JetStream silently drops
// a message once max_deliver is hit, so we give ourselves a spare delivery as a safety
// net in case the worker crashes during attempt 5 before it can republish to the DLQ.
await jsm.consumers.add("WORK", {
  durable_name: "worker",
  ack_policy: "explicit",
  max_deliver: MAX_PROCESS_ATTEMPTS + 1,
  ack_wait: 30 * 1_000_000_000, // 30s; a crashed/timed-out attempt counts as a delivery
});

console.log("streams WORK + WORK_DLQ and consumer 'worker' ready");
await nc.drain();
