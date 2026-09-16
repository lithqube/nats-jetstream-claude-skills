# JavaScript / TypeScript Examples

Complete JetStream examples using the **modular `@nats-io/*` v3 client** (`@nats-io/transport-node`, `@nats-io/jetstream`, `@nats-io/nats-core`, and `@nats-io/kv` / `@nats-io/obj` where needed).

> **Use the modular client, not the legacy monolithic `nats` package.** The v3 packages are the current, maintained line; production Nuxt/Node code (e.g. lithqube's `nuxt-nats` module) uses them exclusively. The v2 → v3 migration removed things you will still see in old blog posts and Stack Overflow answers:
> - `StringCodec` / `JSONCodec` are **gone** — encode with `TextEncoder`, decode with `msg.json()` / `msg.string()`.
> - `js.subscribe()` (push subscription) and `js.fetch()` are **gone** — use `js.consumers.get(...).consume()` / `.fetch()`.
> - JetStream is no longer a method on the connection: `nc.jetstream()` → `jetstream(nc)`, `nc.jetstreamManager()` → `jetstreamManager(nc)` (imported from `@nats-io/jetstream`).
>
> Install: `npm i @nats-io/transport-node @nats-io/jetstream` (add `@nats-io/kv`, `@nats-io/obj`, `@nats-io/services` as needed).

## Durations are nanoseconds — convert explicitly

Every JetStream duration in config (`ack_wait`, `backoff`, `max_age`, `duplicate_window`, `idle_heartbeat` in some APIs) is an **integer count of nanoseconds**. Passing a millisecond value by mistake sets a duration a million times too short — a 30 000 that you meant as 30 s becomes 30 µs, so the consumer redelivers almost immediately. Make the unit impossible to get wrong with a helper:

```javascript
const ms = (n) => n * 1_000_000;          // ms  -> ns
const sec = (n) => n * 1_000_000_000;     // s   -> ns
const days = (n) => n * 24 * 60 * 60 * 1_000_000_000;
```

## Connection and JetStream Client

```javascript
import { connect } from "@nats-io/transport-node";
import { jetstream, jetstreamManager } from "@nats-io/jetstream";

async function main() {
  const nc = await connect({
    servers: "nats://localhost:4222",       // comma-separated string or array for a cluster — pass ALL nodes
    maxReconnectAttempts: -1,               // reconnect forever
    reconnectTimeWait: 2000,
  });

  console.log(`connected to ${nc.getServer()}`);

  // Watch connection status. NOTE: the client emits a `reconnect` status per retry
  // ATTEMPT, not once per recovery — a single outage can produce thousands of them.
  // Gate any "we reconnected" side effect on an actual disconnect->reconnect transition.
  (async () => {
    for await (const s of nc.status()) {
      console.log(`${s.type}: ${s.data}`);
    }
  })();

  const js = jetstream(nc);                 // was nc.jetstream()
  const jsm = await jetstreamManager(nc);   // was nc.jetstreamManager()

  // ... use js and jsm ...

  await nc.drain();                         // flush in-flight, then close
}
```

## Encoding and decoding (no codecs)

```javascript
const enc = (obj) => new TextEncoder().encode(JSON.stringify(obj));

// On the receive side, decode straight off the message:
//   const order = msg.json();     // JSON.parse of the payload
//   const text  = msg.string();   // UTF-8 string
```

## Stream Creation

```javascript
import { RetentionPolicy, StorageType, DiscardPolicy } from "@nats-io/jetstream";

async function createStream(jsm) {
  await jsm.streams.add({
    name: "ORDERS",
    subjects: ["orders.>"],                 // use '>' (multi-token), not 'orders.*'
                                            // 'orders.*' matches ONE token, so orders.us.created
                                            // would silently never be captured

    retention: RetentionPolicy.Limits,
    max_age: days(30),
    max_bytes: 5 * 1024 * 1024 * 1024,      // ALWAYS bound a production stream (age and/or bytes)
    max_msgs: -1,

    storage: StorageType.File,
    num_replicas: 3,                        // 3 for production; 1 only on a single non-clustered node

    discard: DiscardPolicy.Old,

    // Publish-side dedup. The default when unset is 2 MINUTES — usually too short.
    // Set it to at least your longest publish-retry / outbox-drain window so a
    // re-publish of the same Nats-Msg-Id is actually caught.
    duplicate_window: sec(120),

    max_msgs_per_subject: 1000,
  });
}
```

## Publishing

```javascript
// Simple publish
async function publishOrder(js, order) {
  const ack = await js.publish("orders.created", enc(order));
  console.log(`published seq=${ack.seq} stream=${ack.stream}`);
}

// Idempotent publish — the msgID becomes the Nats-Msg-Id dedup key, deduped
// within the stream's duplicate_window.
async function publishIdempotent(js, orderID, order) {
  return js.publish("orders.created", enc(order), { msgID: orderID });
}

// Publish with client-side retry (JetStream publish can time out under load)
async function publishWithRetry(js, subject, data, msgID, maxRetries = 3) {
  for (let i = 0; i < maxRetries; i++) {
    try {
      return await js.publish(subject, enc(data), { msgID });
    } catch (err) {
      if (i === maxRetries - 1) throw err;
      await new Promise((r) => setTimeout(r, 200 * 2 ** i)); // exponential backoff
    }
  }
}
```

## Consumer Creation (durable)

```javascript
import { AckPolicy, DeliverPolicy } from "@nats-io/jetstream";

async function createConsumer(jsm) {
  await jsm.consumers.add("ORDERS", {
    durable_name: "order-processor",
    ack_policy: AckPolicy.Explicit,         // never AckNone/AckAll in production
    ack_wait: sec(30),                      // nanoseconds — use the helper
    max_deliver: 5,                         // retry up to 5 times, then STOP (see note below)
    max_ack_pending: 1000,                  // per-consumer in-flight cap / backpressure
    filter_subject: "orders.created",       // single filter; use filter_subjects: [...] for several
    deliver_policy: DeliverPolicy.All,
    backoff: [sec(2), sec(10), sec(60), sec(300)],
  });
}
```

> **`max_deliver` is "retry then stop", not "retry then dead-letter".** After the limit the server stops redelivering to this consumer and publishes a `$JS.EVENT.ADVISORY.CONSUMER.MAX_DELIVERIES` advisory; the message stays in the stream but nothing hands it to you. There is no built-in DLQ — see `patterns/work-queue.md` for the advisory-capture pattern that builds one.
>
> A consumer's **filter and ack policy are fixed at creation**. Binding to it later with `js.consumers.get()` cannot change them; to change a durable's filter you delete and recreate it.

## Pull Consumer — continuous (`consume`)

```javascript
async function pullConsumerContinuous(js) {
  const consumer = await js.consumers.get("ORDERS", "order-processor");

  // idle_heartbeat lets the client detect a silently dead server-side subscription.
  const iter = await consumer.consume({ max_messages: 100, idle_heartbeat: sec(5) });

  for await (const msg of iter) {
    try {
      const order = msg.json();             // decode straight off the message
      await processOrder(order);
      msg.ack();
    } catch (err) {
      console.error(`processing failed: ${err.message}`);
      // nak(delayMs) delays redelivery. A BARE nak() redelivers immediately and can
      // burn all max_deliver attempts in milliseconds — always pass a delay for a
      // transient failure (retry later), and reserve term() for poison messages.
      msg.nak(5000);
    }
  }
}
```

## Pull Consumer — batch (`fetch`)

```javascript
async function pullConsumerBatch(js) {
  const consumer = await js.consumers.get("ORDERS", "order-processor");

  while (true) {
    const batch = await consumer.fetch({ max_messages: 100, expires: 5000 }); // expires: ms
    let received = 0;
    for await (const msg of batch) {
      received++;
      try {
        await processOrder(msg.json());
        msg.ack();
      } catch {
        msg.nak(5000);
      }
    }
    if (received === 0) continue; // no messages this window; loop (or break for a one-shot drain)
  }
}
```

## Slow handlers, poison messages, and idempotency

```javascript
async function robustHandler(js) {
  const consumer = await js.consumers.get("ORDERS", "order-processor");
  const iter = await consumer.consume({ max_messages: 1, idle_heartbeat: sec(5) });

  for await (const msg of iter) {
    // Idempotency: JetStream is at-least-once. Redelivery after an ack timeout is
    // normal, so make effects idempotent — e.g. an inbox/processed table keyed by
    // (Nats-Msg-Id, consumer), or upserts — rather than assuming exactly-once.
    const id = msg.headers?.get("Nats-Msg-Id");
    if (!id) { msg.term("missing Nats-Msg-Id"); continue; } // can't dedup it; don't loop it

    // For a long job, send working() heartbeats so ack_wait doesn't expire mid-processing
    // and trigger a spurious redelivery.
    const beat = setInterval(() => msg.working(), 15_000);
    try {
      if (await alreadyProcessed(id)) { msg.ack(); continue; } // idempotent skip
      await processOrder(msg.json());
      msg.ack();
    } catch (err) {
      if (isPermanent(err)) msg.term(err.message); // poison: stop retrying (client 3.4+ carries the reason)
      else msg.nak(30_000);                        // transient: retry after 30s
    } finally {
      clearInterval(beat);
    }
  }
}
```

## Ordered Consumer (ephemeral, for replay)

```javascript
async function orderedConsumer(js) {
  // No durable name + an options object => an ordered, ephemeral consumer.
  const consumer = await js.consumers.get("ORDERS", {
    filter_subjects: ["orders.>"],
    deliver_policy: DeliverPolicy.All,
  });

  const iter = await consumer.consume();
  for await (const msg of iter) {
    console.log(`seq=${msg.seq} subject=${msg.subject}`, msg.json());
  }
  // Ordered consumers are ephemeral; they vanish when you stop consuming.
}
```

## Key-Value (`@nats-io/kv`)

```javascript
import { Kvm } from "@nats-io/kv";

async function kvExample(nc) {
  const kvm = new Kvm(nc);
  const kv = await kvm.create("config");                 // .open("config") if it already exists

  await kv.put("feature.flags", enc({ beta: true }));
  const e = await kv.get("feature.flags");
  if (e) console.log(e.json(), "rev", e.revision, "op", e.operation);

  // Optimistic concurrency: update only if the revision still matches; a stale
  // revision throws, so two writers can't silently clobber each other.
  await kv.update("feature.flags", enc({ beta: false }), e.revision);
}
```

## Object Store (`@nats-io/obj`)

```javascript
import { Objm } from "@nats-io/obj";

async function objExample(nc, buf /* a Node Buffer */) {
  const objm = new Objm(nc);
  const os = await objm.create("assets");

  // GOTCHA: put() takes a Web ReadableStream<Uint8Array>, NOT a Node Buffer.
  const body = new ReadableStream({
    start(c) { c.enqueue(new Uint8Array(buf)); c.close(); },
  });
  await os.put({ name: "logo.png", description: "brand" }, body);

  const res = await os.get("logo.png");                  // res.data is a ReadableStream
  console.log("stored", res?.info.size, "bytes");
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

## Detecting "already exists" / "not found" by error code

The v3 client surfaces JetStream API errors as a typed error with a numeric `.code` (the older `err.api_error.err_code` shape is gone). Match the code, not the message text:

```javascript
import { JetStreamApiError } from "@nats-io/jetstream";

async function ensureStream(jsm, cfg) {
  try {
    await jsm.streams.add(cfg);
  } catch (err) {
    if (err instanceof JetStreamApiError && err.code === 10058) return; // stream name already in use
    throw err; // a permissions/transport blip must NOT be swallowed as "already exists"
  }
}
```

## Graceful Shutdown

```javascript
// Stop your consume loops first, THEN drain — draining while a consumer is still
// pulling races acks against the closing connection.
async function shutdown(nc, stopConsumers) {
  await stopConsumers();   // break your `for await` loops / close iterators
  await nc.drain();        // flush in-flight messages and acks, then close
}

for (const sig of ["SIGINT", "SIGTERM"]) {
  process.on(sig, () => shutdown(nc, stopConsumers).then(() => process.exit(0)));
}
```
