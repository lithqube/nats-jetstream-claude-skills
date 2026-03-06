# JavaScript Examples

Complete JetStream examples using `nats.js` (npm package `nats`).

## Connection and JetStream Client

```javascript
import { connect, AckPolicy, DeliverPolicy, RetentionPolicy,
         StorageType, DiscardPolicy, StringCodec, JSONCodec } from "nats";

const sc = StringCodec();
const jc = JSONCodec();

async function main() {
  const nc = await connect({
    servers: "nats://localhost:4222",
    reconnect: true,
    maxReconnectAttempts: -1,
    reconnectTimeWait: 2000,
  });

  console.log(`connected to ${nc.getServer()}`);

  (async () => {
    for await (const s of nc.status()) {
      console.log(`${s.type}: ${s.data}`);
    }
  })().then();

  const js = nc.jetstream();
  const jsm = await nc.jetstreamManager();

  // ... use js and jsm ...

  await nc.drain();
}
```

## Stream Creation

```javascript
async function createStream(jsm) {
  await jsm.streams.add({
    name: "ORDERS",
    subjects: ["orders.>"],

    retention: RetentionPolicy.Limits,
    max_age: 30 * 24 * 60 * 60 * 1_000_000_000, // 30 days in nanos
    max_bytes: 5 * 1024 * 1024 * 1024,
    max_msgs: -1,

    storage: StorageType.File,
    num_replicas: 3,

    discard: DiscardPolicy.Old,
    duplicate_window: 2 * 60 * 1_000_000_000,

    max_msgs_per_subject: 1000,
  });
}
```

## Publishing

```javascript
// Simple publish
async function publishOrder(js, order) {
  const ack = await js.publish("orders.created", jc.encode(order));
  console.log(`published seq=${ack.seq} stream=${ack.stream}`);
}

// Publish with deduplication
async function publishIdempotent(js, orderID, order) {
  const h = nats.headers();
  h.set("Nats-Msg-Id", orderID);

  return await js.publish("orders.created", jc.encode(order), { headers: h });
}

// Publish with retry
async function publishWithRetry(js, subject, data, maxRetries = 3) {
  for (let i = 0; i < maxRetries; i++) {
    try {
      return await js.publish(subject, jc.encode(data));
    } catch (err) {
      console.error(`publish attempt ${i + 1} failed: ${err.message}`);
      if (i === maxRetries - 1) throw err;
      await new Promise((r) => setTimeout(r, 1000 * (i + 1)));
    }
  }
}
```

## Pull Consumer

```javascript
async function pullConsumer(js) {
  const consumer = await js.consumers.get("ORDERS", "order-processor");

  // Batch fetch
  while (true) {
    const messages = await consumer.fetch({ max_messages: 100, expires: 5000 });

    for await (const msg of messages) {
      try {
        const order = jc.decode(msg.data);
        console.log(`processing order ${order.id} seq=${msg.seq}`);
        await processOrder(order);
        msg.ack();
      } catch (err) {
        console.error(`processing failed: ${err.message}`);
        msg.nak(5000);
      }
    }
  }
}

// Continuous consumption
async function pullConsumerContinuous(js) {
  const consumer = await js.consumers.get("ORDERS", "order-processor");
  const messages = await consumer.consume({ max_messages: 100 });

  for await (const msg of messages) {
    try {
      const order = jc.decode(msg.data);
      await processOrder(order);
      msg.ack();
    } catch (err) {
      console.error(`error: ${err.message}`);
      msg.nak(5000);
    }
  }
}
```

## Consumer Creation

```javascript
async function createConsumer(jsm) {
  await jsm.consumers.add("ORDERS", {
    durable_name: "order-processor",
    ack_policy: AckPolicy.Explicit,
    ack_wait: 30 * 1_000_000_000,
    max_deliver: 5,
    max_ack_pending: 1000,
    filter_subject: "orders.>",
    deliver_policy: DeliverPolicy.All,
    backoff: [
      2 * 1_000_000_000,
      10 * 1_000_000_000,
      60 * 1_000_000_000,
      300 * 1_000_000_000,
    ],
  });
}
```

## Ordered Consumer (Replay)

```javascript
async function orderedConsumer(js) {
  const consumer = await js.consumers.get("ORDERS", {
    ordered_consumer: true,
    filter_subjects: ["orders.>"],
  });

  const messages = await consumer.consume();
  for await (const msg of messages) {
    const order = jc.decode(msg.data);
    console.log(`seq=${msg.seq} subject=${msg.subject}`, order);
  }
}
```

## Stream Management

```javascript
async function streamInfo(jsm) {
  const streams = await jsm.streams.list().next();
  for (const si of streams) {
    console.log(`stream: ${si.config.name} msgs=${si.state.messages}`);
  }

  const info = await jsm.streams.info("ORDERS");
  console.log(`ORDERS: ${info.state.messages} messages`);

  await jsm.streams.purge("ORDERS");
  await jsm.streams.purge("ORDERS", { filter: "orders.cancelled" });
}
```

## Graceful Shutdown

```javascript
process.on("SIGINT", async () => {
  await nc.drain();
  process.exit(0);
});

process.on("SIGTERM", async () => {
  await nc.drain();
  process.exit(0);
});
```
