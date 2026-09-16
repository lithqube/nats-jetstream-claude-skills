# Streams

Streams are the persistence layer in JetStream. A stream captures messages published to one or more subjects and stores them according to configurable retention and limit policies.

## Stream Configuration Reference

| Field | Description | Default |
|-------|-------------|---------|
| `Name` | Unique stream identifier (alphanumeric, dash, underscore) | required |
| `Subjects` | List of subjects to capture (supports wildcards) | required |
| `Retention` | When to discard messages: `LimitsPolicy`, `InterestPolicy`, `WorkQueuePolicy` | `LimitsPolicy` |
| `Storage` | `FileStorage` (persistent) or `MemoryStorage` (fast, volatile) | `FileStorage` |
| `Replicas` | Number of replicas in cluster (1, 3, or 5) | `1` |
| `MaxMsgs` | Max number of messages in stream (-1 = unlimited) | `-1` |
| `MaxBytes` | Max total bytes in stream (-1 = unlimited) | `-1` |
| `MaxAge` | Max age of messages (0 = unlimited) | `0` |
| `MaxMsgSize` | Max size per message (-1 = unlimited) | `-1` |
| `MaxMsgsPerSubject` | Max messages per subject (-1 = unlimited) | `-1` |
| `Discard` | When limits reached: `DiscardOld` or `DiscardNew` | `DiscardOld` |
| `DuplicateWindow` | Time window for publish deduplication via `Nats-Msg-Id` header | `2m` |
| `AllowRollup` | Allow `Nats-Rollup` header to purge prior messages | `false` |
| `DenyDelete` | Prevent message deletion from stream | `false` |
| `DenyPurge` | Prevent stream purge operations | `false` |
| `AllowDirect` | Enable direct get for KV-like access patterns | `false` |

## Retention Policies

### LimitsPolicy (default)

Messages are kept until limits (MaxMsgs, MaxBytes, MaxAge) are reached. Old messages are discarded based on `Discard` policy. Use for event logs, audit trails, and general pub/sub.

```go
&nats.StreamConfig{
    Name:      "EVENTS",
    Subjects:  []string{"events.>"},
    Retention: nats.LimitsPolicy,
    MaxAge:    30 * 24 * time.Hour, // 30 days
    Storage:   nats.FileStorage,
    Replicas:  3,
}
```

### InterestPolicy

Messages are kept only while there are active consumers. Once all consumers have acknowledged a message, it is removed. Use for transient notifications where unsubscribed data is worthless.

```go
&nats.StreamConfig{
    Name:      "NOTIFICATIONS",
    Subjects:  []string{"notify.>"},
    Retention: nats.InterestPolicy,
    Storage:   nats.FileStorage,
    Replicas:  3,
}
```

### WorkQueuePolicy

Each message is delivered to exactly one consumer and removed upon acknowledgment. Use for job queues, task distribution, and command processing.

```go
&nats.StreamConfig{
    Name:      "TASKS",
    Subjects:  []string{"tasks.>"},
    Retention: nats.WorkQueuePolicy,
    Storage:   nats.FileStorage,
    Replicas:  3,
}
```

## Storage Types

### FileStorage
- Persistent across server restarts
- Uses disk I/O — throughput depends on disk speed
- Use for all production streams
- Supports compression (enabled per server config)

### MemoryStorage
- Lost on server restart
- Extremely fast — no disk I/O
- Use for ephemeral caches, real-time aggregations, or development
- Counts against JetStream memory limits (not storage limits)

## Subject Namespace Design

Use hierarchical subjects with dot-separated tokens. Wildcards enable flexible consumer filtering.

### Wildcards

- `*` matches exactly one token: `orders.*` matches `orders.created` but not `orders.us.created`
- `>` matches one or more tokens: `orders.>` matches `orders.created` and `orders.us.created`

**Capture with `>`, not `*`, unless you truly mean one token.** A stream whose subject is `orders.*` silently does **not** capture `orders.us.created` — the publish still succeeds (it goes nowhere in JetStream), so the producer's outbox row or retry loop spins forever with no error. This is a common, quiet production bug; when in doubt, a stream should own `domain.>`.

### Recommended patterns

```
{domain}.{entity}.{event}
  orders.created
  orders.updated
  orders.cancelled

{scope}.{scope_id}.{aggregate}.{event}     // tenant/partition as a LOGICAL token
  tenant.acme.orders.created
  tenant.globex.orders.shipped
```

### Subject design is a contract (production conventions)

Subjects are baked into stream config, so treat their shape as an interface that is expensive to change:

- **Verb tense carries meaning.** Past tense = a fact that happened (`orders.created`, `payment.completed`); `.requested` = a command; `.failed` / `.rejected` / `.approved` = terminal states. Consumers filter on these, so keep the convention consistent.
- **Never encode physical topology in a subject** (`orders.node1.…`, `orders.eu-datacenter.…`). A logical tenant/region partition is fine; a server or datacenter name isn't — it bakes your deployment into the namespace and breaks the day you migrate. A *logical* region used for routing is acceptable; a physical one is not.
- **Never put a schema version in the subject** (`orders.created.v2`). Versioned subjects change stream config and break tenant-wide replay (`nats stream view … --subject 'tenant.<id>.>'`). Put the version in a header (`X-Schema-Version`) and/or a payload field instead, and evolve payloads additively.

### Stream subject grouping

```go
// One stream captures an entire domain
&nats.StreamConfig{
    Name:     "ORDERS",
    Subjects: []string{"orders.>"},
}

// Consumers filter to specific events
// Consumer A: orders.created
// Consumer B: orders.shipped
// Consumer C: orders.> (all order events)
```

## Stream Mirroring and Sourcing

### Mirror
A mirror stream is a read-only replica of another stream. Use for cross-cluster replication or creating read replicas.

```go
&nats.StreamConfig{
    Name: "ORDERS_MIRROR",
    Mirror: &nats.StreamSource{
        Name: "ORDERS",
    },
    Storage:  nats.FileStorage,
    Replicas: 3,
}
```

### Source
A stream can source messages from one or more other streams, combining them into a single stream. Use for aggregation.

```go
&nats.StreamConfig{
    Name: "ALL_EVENTS",
    Sources: []*nats.StreamSource{
        {Name: "ORDERS"},
        {Name: "PAYMENTS"},
        {Name: "SHIPPING"},
    },
    Storage:  nats.FileStorage,
    Replicas: 3,
}
```

## Discard Policies

### DiscardOld (default)
When limits are reached, the oldest messages are removed to make room. Best for rolling windows and logs.

### DiscardNew
When limits are reached, new publishes are rejected with an error. Best when you need backpressure — the publisher knows the stream is full and can decide what to do.

```go
// Bounded queue that rejects when full
&nats.StreamConfig{
    Name:      "BOUNDED_QUEUE",
    Subjects:  []string{"jobs.>"},
    Retention: nats.WorkQueuePolicy,
    MaxMsgs:   10000,
    Discard:   nats.DiscardNew,
    Storage:   nats.FileStorage,
    Replicas:  3,
}
```
