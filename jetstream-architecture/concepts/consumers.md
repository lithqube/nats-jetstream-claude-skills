# Consumers

Consumers are the mechanism for reading messages from a JetStream stream. Each consumer tracks its position in the stream independently and can be configured for different delivery semantics.

## Pull vs Push Consumers

| Aspect | Pull Consumer | Push Consumer |
|--------|--------------|---------------|
| Flow control | Client requests batches — natural backpressure | Server pushes — needs `MaxAckPending` for flow control |
| Scaling | Multiple workers fetch from same consumer (load balanced) | Each subscription gets all messages (fanout) |
| Use case | Worker queues, batch processing, rate-limited consumers | Real-time event listeners, notifications |
| Reconnect | Stateless — just fetch again | Needs `DeliverSubject` resubscription |
| Horizontal scaling | Add more workers calling Fetch() | Use queue groups on deliver subject |

**Decision rule**: Default to pull consumers. Use push only when you need real-time delivery with minimal latency and the consumer can keep up with the message rate.

## Durable vs Ephemeral Consumers

### Durable Consumer
- Has a `Durable` name — survives client disconnection
- Server tracks acknowledgment state persistently
- Required for any consumer that must resume from where it left off
- Use for all production consumers

### Ephemeral Consumer
- No `Durable` name — destroyed when the last subscription disconnects (after `InactiveThreshold`)
- Use for ad-hoc queries, debugging, or temporary subscriptions
- Default `InactiveThreshold` is 5 seconds

## Consumer Configuration Reference

| Field | Description | Default |
|-------|-------------|---------|
| `Durable` | Consumer name for persistence (omit for ephemeral) | none |
| `FilterSubject` | Receive only messages matching this subject | all subjects |
| `FilterSubjects` | Receive messages matching any of these subjects | all subjects |
| `AckPolicy` | `AckExplicit`, `AckAll`, `AckNone` | `AckExplicit` |
| `AckWait` | Time to wait for ack before redelivery | `30s` |
| `MaxDeliver` | Max redelivery attempts (-1 = unlimited) | `-1` |
| `DeliverPolicy` | Starting point: `DeliverAll`, `DeliverLast`, `DeliverLastPerSubject`, `DeliverNew`, `DeliverByStartSequence`, `DeliverByStartTime` | `DeliverAll` |
| `ReplayPolicy` | Replay speed: `ReplayInstant` or `ReplayOriginal` | `ReplayInstant` |
| `MaxAckPending` | Max unacknowledged messages in flight | `1000` |
| `MaxWaiting` | Max pending pull requests (pull consumers only) | `512` |
| `Backoff` | Custom redelivery backoff durations | none |
| `InactiveThreshold` | Time before ephemeral consumer is deleted | `5s` |
| `NumReplicas` | Consumer replica count (0 = inherit from stream) | `0` |
| `MemStorage` | Store consumer state in memory (even if stream is file) | `false` |
| `Metadata` | Key-value metadata for consumer | none |

## Ack Policies

### AckExplicit (recommended)
Each message must be individually acknowledged. Unacknowledged messages are redelivered after `AckWait`. Use for all production workloads.

```go
// Ack — message processed successfully
msg.Ack()

// Nak — request immediate redelivery
msg.Nak()

// NakWithDelay — request redelivery after delay
msg.NakWithDelay(5 * time.Second)

// Term — permanently give up on this message (won't be redelivered)
msg.Term()

// InProgress — extend the ack deadline (for long-running processing)
msg.InProgress()
```

### AckAll
Acknowledging message N implicitly acknowledges all messages up to N. Lower overhead but risks reprocessing on failure. Use only when messages are processed sequentially and reprocessing is acceptable.

### AckNone
No acknowledgment required. Messages are considered delivered once sent. Use only for best-effort delivery where loss is acceptable (metrics, logs).

## Deliver Policies

| Policy | Behavior | Use Case |
|--------|----------|----------|
| `DeliverAll` | Start from first message in stream | Full replay, new consumers that need history |
| `DeliverLast` | Start from last message in stream | Get current state, late-joining subscribers |
| `DeliverLastPerSubject` | Start from last message per subject | Materialized views, current state per entity |
| `DeliverNew` | Start from messages published after consumer creation | Real-time only, no history needed |
| `DeliverByStartSequence` | Start from a specific stream sequence number | Resume from known position |
| `DeliverByStartTime` | Start from a specific timestamp | Replay from a point in time |

## Ordered Consumers

An ordered consumer guarantees messages arrive in order with no gaps. If the server detects a gap (due to reconnection), it automatically recreates the consumer from the last known sequence.

- Always ephemeral (no durable name)
- `AckPolicy: AckNone`
- `MaxDeliver: 1`
- `MaxAckPending: 1` (or flow control)
- Use for read-only replay, event sourcing projections, or snapshotting

```go
sub, _ := js.SubscribeSync("events.>",
    nats.OrderedConsumer(),
)
```

## Consumer Groups (Load Balancing)

### Pull consumer (recommended)
Multiple workers call `Fetch()` on the same durable pull consumer. JetStream distributes messages across workers automatically.

```
Worker A ─┐
Worker B ─┼─ Fetch() ──> Pull Consumer "WORKER" ──> Stream "TASKS"
Worker C ─┘
```

### Push consumer with queue group
Set the `DeliverSubject` to a queue group subject. Multiple subscribers share the message load.

```go
&nats.ConsumerConfig{
    Durable:        "event-processors",
    DeliverSubject: "deliver.events",
    DeliverGroup:   "processors",   // queue group name
    AckPolicy:      nats.AckExplicit,
}
```

## Backoff Strategy

Configure custom redelivery intervals for failed messages:

```go
&nats.ConsumerConfig{
    Durable:    "order-processor",
    AckPolicy:  nats.AckExplicit,
    MaxDeliver: 5,
    Backoff: []time.Duration{
        2 * time.Second,   // 1st retry
        10 * time.Second,  // 2nd retry
        30 * time.Second,  // 3rd retry
        2 * time.Minute,   // 4th retry
    },
}
```

When `Backoff` is set, `AckWait` applies only to the first delivery. Subsequent redeliveries use the backoff schedule. If there are more retries than backoff entries, the last entry is repeated.
