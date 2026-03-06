# Fanout Pattern

A single published event is delivered to multiple independent consumers, each processing the message for a different purpose.

## Architecture

```
                    ┌─ Consumer: "audit-log"     ──> Audit Service
Producer ──> Stream ├─ Consumer: "notifications" ──> Notification Service
                    ├─ Consumer: "analytics"     ──> Analytics Service
                    └─ Consumer: "search-index"  ──> Search Indexer
```

Each consumer maintains its own position in the stream and processes messages independently. One consumer's failure does not affect others.

## When to Use

- Multiple services need to react to the same events
- Each service processes events at its own pace
- Services have different uptime requirements
- You need to add new consumers without modifying producers

## Stream Configuration

Use `LimitsPolicy` or `InterestPolicy` retention:

```go
// LimitsPolicy: keeps messages for a time window regardless of consumers
stream, _ := js.AddStream(&nats.StreamConfig{
    Name:     "ORDERS",
    Subjects: []string{"orders.>"},
    Retention: nats.LimitsPolicy,
    MaxAge:    7 * 24 * time.Hour, // 7-day retention
    Storage:   nats.FileStorage,
    Replicas:  3,
})
```

```go
// InterestPolicy: removes messages once ALL active consumers have acked
stream, _ := js.AddStream(&nats.StreamConfig{
    Name:      "NOTIFICATIONS",
    Subjects:  []string{"notify.>"},
    Retention: nats.InterestPolicy,
    Storage:   nats.FileStorage,
    Replicas:  3,
})
```

**Do not** use `WorkQueuePolicy` for fanout — it delivers each message to only one consumer.

## Consumer Configuration

Each consumer is an independent durable consumer filtering the subjects it cares about:

```go
// Audit service — processes all order events
js.AddConsumer("ORDERS", &nats.ConsumerConfig{
    Durable:       "audit-log",
    AckPolicy:     nats.AckExplicit,
    DeliverPolicy: nats.DeliverAllPolicy,
    FilterSubject: "orders.>",
    MaxAckPending: 1000,
    AckWait:       30 * time.Second,
    MaxDeliver:    5,
})

// Notification service — only cares about created and cancelled
js.AddConsumer("ORDERS", &nats.ConsumerConfig{
    Durable:       "notifications",
    AckPolicy:     nats.AckExplicit,
    DeliverPolicy: nats.DeliverNewPolicy,
    FilterSubjects: []string{"orders.created", "orders.cancelled"},
    MaxAckPending: 500,
    AckWait:       10 * time.Second,
    MaxDeliver:    3,
})

// Analytics — processes everything but can tolerate reprocessing
js.AddConsumer("ORDERS", &nats.ConsumerConfig{
    Durable:       "analytics",
    AckPolicy:     nats.AckExplicit,
    DeliverPolicy: nats.DeliverAllPolicy,
    FilterSubject: "orders.>",
    MaxAckPending: 5000,
    AckWait:       60 * time.Second,
    MaxDeliver:    10,
})
```

## Adding a New Consumer

New consumers can be added at any time without modifying producers or existing consumers:

```go
// New search indexer joins — replays all historical messages
js.AddConsumer("ORDERS", &nats.ConsumerConfig{
    Durable:       "search-index",
    AckPolicy:     nats.AckExplicit,
    DeliverPolicy: nats.DeliverAllPolicy, // replay from beginning
    FilterSubject: "orders.>",
    MaxAckPending: 2000,
})
```

Use `DeliverAllPolicy` to replay history or `DeliverNewPolicy` to start from now.

## Scaling Individual Consumers

If one consumer (e.g., analytics) needs more throughput, add workers to that specific consumer using pull consumers:

```go
// Multiple analytics workers fetching from the same pull consumer
for i := 0; i < numWorkers; i++ {
    go func() {
        sub, _ := js.PullSubscribe("orders.>", "analytics")
        for {
            msgs, _ := sub.Fetch(100, nats.MaxWait(5*time.Second))
            for _, msg := range msgs {
                processAnalytics(msg)
                msg.Ack()
            }
        }
    }()
}
```
