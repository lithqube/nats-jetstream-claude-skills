# Work Queue Pattern

Use JetStream as a distributed job queue where each message is processed by exactly one worker with explicit acknowledgment, retry logic, and dead letter handling.

## Architecture

```
                                    ┌─ Worker A
Producer ──> Stream (WorkQueue) ──> ├─ Worker B  ──> Pull Consumer "processor"
                                    └─ Worker C
```

Each message is delivered to exactly one worker. Failed messages are retried with backoff up to `MaxDeliver`, after which **the server discards the message and publishes an advisory**.

There is no dead-letter queue in NATS. Not in any released server, and not in 2.15-RC: `consumer.go` has no `dead_letter` field and no ADR proposes one. `MaxDeliver` without the advisory-consuming machinery below is not "retry then dead-letter", it is "retry then silently drop".

## Stream Configuration

```go
stream, _ := js.AddStream(&nats.StreamConfig{
    Name:      "TASKS",
    Subjects:  []string{"tasks.>"},
    Retention: nats.WorkQueuePolicy,  // remove message once acked
    Storage:   nats.FileStorage,
    Replicas:  3,
    MaxMsgs:   100000,                // bound the queue
    Discard:   nats.DiscardNew,       // reject publishes when full (backpressure)
    MaxAge:    24 * time.Hour,        // expire stale tasks
})
```

Key settings:
- `WorkQueuePolicy` — message is deleted once acknowledged
- `DiscardNew` — gives backpressure to producers when queue is full
- `MaxAge` — prevents indefinitely old tasks from accumulating

## Consumer Configuration

```go
consumer, _ := js.AddConsumer("TASKS", &nats.ConsumerConfig{
    Durable:       "processor",
    AckPolicy:     nats.AckExplicit,
    AckWait:       30 * time.Second,
    MaxDeliver:    5,
    MaxAckPending: 100,               // per-worker concurrency limit
    Backoff: []time.Duration{
        5 * time.Second,
        30 * time.Second,
        2 * time.Minute,
        10 * time.Minute,
    },
})
```

## Worker Implementation

```go
sub, _ := js.PullSubscribe("tasks.>", "processor")

for {
    msgs, err := sub.Fetch(10, nats.MaxWait(5*time.Second))
    if err != nil {
        if err == nats.ErrTimeout {
            continue // no messages available
        }
        log.Printf("fetch error: %v", err)
        continue
    }

    for _, msg := range msgs {
        // Check delivery count for dead letter handling
        meta, _ := msg.Metadata()
        if meta.NumDelivered > 3 {
            log.Printf("message %d delivered %d times, will reach MaxDeliver soon",
                meta.Sequence.Stream, meta.NumDelivered)
        }

        err := processTask(msg.Data)
        if err != nil {
            log.Printf("processing failed: %v", err)
            // Nak with delay for transient errors
            msg.NakWithDelay(5 * time.Second)
            continue
        }

        msg.Ack()
    }
}
```

## Dead Letter Queue (DLQ)

When a message exceeds `MaxDeliver`, JetStream publishes an advisory to `$JS.EVENT.ADVISORY.CONSUMER.MAX_DELIVERIES.{stream}.{consumer}`. You build the DLQ yourself by consuming it.

**Advisories are core NATS, not JetStream.** They are fire-and-forget, so anything published while your subscriber is restarting, redeploying or partitioned is gone permanently. The `nc.Subscribe` below is the simplest illustration, and it is the wrong shape for anything that must not lose a job: it makes the failure path the only ephemeral part of an otherwise durable design.

For production, capture the advisories into a stream and consume THAT durably:

```go
js.AddStream(&nats.StreamConfig{
    Name: "JS_ADVISORY",
    Subjects: []string{
        "$JS.EVENT.ADVISORY.CONSUMER.MAX_DELIVERIES.>",
        "$JS.EVENT.ADVISORY.CONSUMER.MSG_TERMINATED.>",
    },
    Retention: nats.LimitsPolicy,
    MaxAge:    30 * 24 * time.Hour,
    Storage:   nats.FileStorage,
})
```

Then run a durable pull consumer over `JS_ADVISORY` with the handler body shown below. Two further notes:

- **Do not subscribe to `$JS.EVENT.ADVISORY.>` to catch these.** That is the whole-account firehose, and it includes an API audit advisory the server publishes on *every* JetStream API response, success or failure. Filter to the two subjects you actually want.
- **Also capture `MSG_TERMINATED`.** It fires on an explicit `msg.Term()` and, unlike `max_deliver`, carries `consumer_seq` and a `reason` field when the client supplied one. The `max_deliver` advisory has **no `consumer_seq`**; assuming symmetry puts a hole in your audit trail.

The illustration below uses a plain subscription for brevity:

```go
// Create a dead letter stream
js.AddStream(&nats.StreamConfig{
    Name:     "TASKS_DLQ",
    Subjects: []string{"dlq.tasks.>"},
    Retention: nats.LimitsPolicy,
    MaxAge:    30 * 24 * time.Hour, // keep dead letters for 30 days
    Storage:   nats.FileStorage,
    Replicas:  3,
})

// Subscribe to max delivery advisories
nc.Subscribe(
    "$JS.EVENT.ADVISORY.CONSUMER.MAX_DELIVERIES.TASKS.processor",
    func(msg *nats.Msg) {
        // Parse the advisory to get the original message details
        var advisory struct {
            Stream   string `json:"stream"`
            Consumer string `json:"consumer"`
            StreamSeq uint64 `json:"stream_seq"`
        }
        json.Unmarshal(msg.Data, &advisory)

        // Fetch the original message by sequence
        raw, _ := js.GetMsg("TASKS", advisory.StreamSeq)

        // Publish to dead letter stream with metadata
        headers := nats.Header{}
        headers.Set("Original-Stream", advisory.Stream)
        headers.Set("Original-Seq", fmt.Sprintf("%d", advisory.StreamSeq))
        headers.Set("Failure-Reason", "max-deliveries-exceeded")

        js.PublishMsg(&nats.Msg{
            Subject: "dlq.tasks.failed",
            Data:    raw.Data,
            Header:  headers,
        })
    },
)
```

## Exactly-Once Processing

JetStream provides publish-side deduplication via `Nats-Msg-Id` header. For consumer-side exactly-once, implement idempotent processing:

```go
// Publish with deduplication ID
js.PublishMsg(&nats.Msg{
    Subject: "tasks.process",
    Data:    taskData,
    Header:  nats.Header{"Nats-Msg-Id": []string{taskID}},
})

// Consumer-side idempotency
func processTask(msg *nats.Msg) error {
    taskID := extractTaskID(msg)

    // Check if already processed (use database, Redis, etc.)
    if alreadyProcessed(taskID) {
        msg.Ack() // ack to remove from queue, but skip processing
        return nil
    }

    // Process and mark as done atomically
    err := processAndMarkDone(taskID, msg.Data)
    if err != nil {
        return err
    }

    msg.Ack()
    return nil
}
```

## Priority Queues

JetStream doesn't have native priority. Implement with multiple subjects and weighted consumption:

```go
// Producers publish to priority subjects
js.Publish("tasks.high.resize", highPriorityTask)
js.Publish("tasks.normal.resize", normalPriorityTask)
js.Publish("tasks.low.resize", lowPriorityTask)

// Worker fetches high priority first
func worker(js nats.JetStreamContext) {
    highSub, _ := js.PullSubscribe("tasks.high.>", "processor-high")
    normalSub, _ := js.PullSubscribe("tasks.normal.>", "processor-normal")
    lowSub, _ := js.PullSubscribe("tasks.low.>", "processor-low")

    for {
        // Try high priority first
        if msgs, err := highSub.Fetch(10, nats.MaxWait(100*time.Millisecond)); err == nil {
            processBatch(msgs)
            continue
        }
        // Then normal
        if msgs, err := normalSub.Fetch(10, nats.MaxWait(100*time.Millisecond)); err == nil {
            processBatch(msgs)
            continue
        }
        // Then low priority with longer wait
        if msgs, err := lowSub.Fetch(10, nats.MaxWait(2*time.Second)); err == nil {
            processBatch(msgs)
        }
    }
}
```
