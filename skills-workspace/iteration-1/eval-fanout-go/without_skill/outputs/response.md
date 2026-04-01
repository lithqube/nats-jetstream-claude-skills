# NATS JetStream Fan-Out Pattern for Order Processing in Go

## Stream Design Rationale

When multiple independent services need to process every message, JetStream's **durable push consumers** (or pull consumers with separate durable names) give each service its own cursor into the stream. This is fundamentally different from a queue group (which load-balances across instances of the *same* service). The key design decisions are:

1. **One stream, three consumers** — a single `ORDERS` stream holds all order events. Each of the three services (payment, inventory, notifications) gets its own named durable consumer. JetStream tracks each consumer's position independently, so all three receive every message.

2. **Subject hierarchy** — publishing to `orders.created` (and potentially `orders.updated`, `orders.cancelled`) lets consumers filter by subject if needed. All subjects matching `orders.>` land in the same stream.

3. **Retention policy** — `LimitsPolicy` (the default) keeps messages according to age/size limits, giving replay capability if a service restarts or falls behind.

4. **Ack policy** — `AckExplicit` on all consumers. A service only advances its cursor after successfully processing the message, preventing data loss on crashes.

5. **Delivery policy** — `DeliverAllPolicy` so that if a consumer is created after messages already exist, it starts from the beginning. Use `DeliverNewPolicy` for production if you only care about future messages.

6. **Max deliver / backoff** — set `MaxDeliver` to bound retry loops and provide an exponential backoff slice to avoid hammering downstream systems on repeated failures.

---

## Stream Configuration

```go
// Stream: ORDERS
// Captures all subjects matching orders.>
nats.StreamConfig{
    Name:       "ORDERS",
    Subjects:   []string{"orders.>"},
    Storage:    nats.FileStorage,       // survives server restarts
    Replicas:   1,                      // increase for clustering
    Retention:  nats.LimitsPolicy,
    MaxAge:     24 * time.Hour * 7,     // keep 7 days
    MaxMsgs:    -1,                     // unlimited count
    MaxBytes:   -1,                     // unlimited bytes
    Discard:    nats.DiscardOld,
    Duplicates: 5 * time.Minute,        // idempotency window
}
```

---

## Consumer Configurations

Each service gets a **durable pull consumer**. Pull consumers are preferred in production because they allow the application to control concurrency and apply back-pressure naturally.

### Payment Service Consumer

```go
nats.ConsumerConfig{
    Durable:        "payment-service",
    FilterSubject:  "orders.>",
    AckPolicy:      nats.AckExplicitPolicy,
    AckWait:        30 * time.Second,   // payment processing can be slow
    MaxDeliver:     5,
    BackOff:        []time.Duration{
        1 * time.Second,
        5 * time.Second,
        15 * time.Second,
        30 * time.Second,
    },
    DeliverPolicy:  nats.DeliverAllPolicy,
    ReplayPolicy:   nats.ReplayInstantPolicy,
    MaxAckPending:  50,                 // limit in-flight for payment
}
```

### Inventory Service Consumer

```go
nats.ConsumerConfig{
    Durable:        "inventory-service",
    FilterSubject:  "orders.>",
    AckPolicy:      nats.AckExplicitPolicy,
    AckWait:        10 * time.Second,
    MaxDeliver:     5,
    BackOff:        []time.Duration{
        500 * time.Millisecond,
        2 * time.Second,
        10 * time.Second,
        30 * time.Second,
    },
    DeliverPolicy:  nats.DeliverAllPolicy,
    ReplayPolicy:   nats.ReplayInstantPolicy,
    MaxAckPending:  200,                // inventory lookups are fast
}
```

### Notifications Service Consumer

```go
nats.ConsumerConfig{
    Durable:        "notifications-service",
    FilterSubject:  "orders.>",
    AckPolicy:      nats.AckExplicitPolicy,
    AckWait:        10 * time.Second,
    MaxDeliver:     3,                  // fewer retries; notification loss is acceptable
    BackOff:        []time.Duration{
        1 * time.Second,
        5 * time.Second,
        15 * time.Second,
    },
    DeliverPolicy:  nats.DeliverAllPolicy,
    ReplayPolicy:   nats.ReplayInstantPolicy,
    MaxAckPending:  500,                // notifications can fan out quickly
}
```

---

## Working Go Code

### Project layout

```
order-fanout/
├── go.mod
├── cmd/
│   ├── producer/      main.go
│   ├── payment/       main.go
│   ├── inventory/     main.go
│   └── notifications/ main.go
└── internal/
    ├── jetstream/     setup.go
    └── model/         order.go
```

### go.mod

```go
module github.com/example/order-fanout

go 1.22

require github.com/nats-io/nats.go v1.37.0
```

### internal/model/order.go

```go
package model

import "time"

type Order struct {
    ID        string    `json:"id"`
    CustomerID string   `json:"customer_id"`
    Items     []Item    `json:"items"`
    Total     float64   `json:"total"`
    CreatedAt time.Time `json:"created_at"`
}

type Item struct {
    SKU      string  `json:"sku"`
    Quantity int     `json:"quantity"`
    Price    float64 `json:"price"`
}
```

### internal/jetstream/setup.go

```go
package jetstream

import (
    "errors"
    "fmt"
    "time"

    "github.com/nats-io/nats.go"
)

const StreamName = "ORDERS"

// EnsureStream creates the ORDERS stream if it does not already exist,
// or leaves it unchanged if it does. Safe to call at startup from every service.
func EnsureStream(js nats.JetStreamContext) error {
    _, err := js.StreamInfo(StreamName)
    if err == nil {
        return nil // already exists
    }
    if !errors.Is(err, nats.ErrStreamNotFound) {
        return fmt.Errorf("stream info: %w", err)
    }

    _, err = js.AddStream(&nats.StreamConfig{
        Name:       StreamName,
        Subjects:   []string{"orders.>"},
        Storage:    nats.FileStorage,
        Replicas:   1,
        Retention:  nats.LimitsPolicy,
        MaxAge:     7 * 24 * time.Hour,
        MaxMsgs:    -1,
        MaxBytes:   -1,
        Discard:    nats.DiscardOld,
        Duplicates: 5 * time.Minute,
    })
    if err != nil {
        return fmt.Errorf("add stream: %w", err)
    }
    return nil
}

// EnsureConsumer creates a durable pull consumer if it does not exist.
func EnsureConsumer(js nats.JetStreamContext, cfg *nats.ConsumerConfig) error {
    _, err := js.ConsumerInfo(StreamName, cfg.Durable)
    if err == nil {
        return nil // already exists
    }
    if !errors.Is(err, nats.ErrConsumerNotFound) {
        return fmt.Errorf("consumer info: %w", err)
    }
    _, err = js.AddConsumer(StreamName, cfg)
    if err != nil {
        return fmt.Errorf("add consumer: %w", err)
    }
    return nil
}
```

### cmd/producer/main.go

```go
package main

import (
    "encoding/json"
    "fmt"
    "log"
    "time"

    "github.com/nats-io/nats.go"
    "github.com/google/uuid"

    js "github.com/example/order-fanout/internal/jetstream"
    "github.com/example/order-fanout/internal/model"
)

func main() {
    nc, err := nats.Connect(nats.DefaultURL)
    if err != nil {
        log.Fatalf("connect: %v", err)
    }
    defer nc.Drain()

    jsc, err := nc.JetStream()
    if err != nil {
        log.Fatalf("jetstream: %v", err)
    }

    if err := js.EnsureStream(jsc); err != nil {
        log.Fatalf("ensure stream: %v", err)
    }

    // Simulate publishing 5 orders
    for i := 1; i <= 5; i++ {
        order := model.Order{
            ID:         uuid.NewString(),
            CustomerID: fmt.Sprintf("cust-%d", i),
            Items: []model.Item{
                {SKU: "SKU-001", Quantity: 2, Price: 19.99},
            },
            Total:     39.98,
            CreatedAt: time.Now(),
        }

        payload, err := json.Marshal(order)
        if err != nil {
            log.Printf("marshal order %d: %v", i, err)
            continue
        }

        // Publish with a message ID for deduplication
        ack, err := jsc.Publish("orders.created", payload,
            nats.MsgId(order.ID),
        )
        if err != nil {
            log.Printf("publish order %d: %v", i, err)
            continue
        }
        log.Printf("published order %s -> stream seq %d", order.ID, ack.Sequence)

        time.Sleep(500 * time.Millisecond)
    }
}
```

### cmd/payment/main.go

```go
package main

import (
    "context"
    "encoding/json"
    "log"
    "os"
    "os/signal"
    "syscall"
    "time"

    "github.com/nats-io/nats.go"

    js "github.com/example/order-fanout/internal/jetstream"
    "github.com/example/order-fanout/internal/model"
)

const durableName = "payment-service"

func main() {
    nc, err := nats.Connect(nats.DefaultURL)
    if err != nil {
        log.Fatalf("connect: %v", err)
    }
    defer nc.Drain()

    jsc, err := nc.JetStream()
    if err != nil {
        log.Fatalf("jetstream: %v", err)
    }

    if err := js.EnsureStream(jsc); err != nil {
        log.Fatalf("ensure stream: %v", err)
    }

    cfg := &nats.ConsumerConfig{
        Durable:       durableName,
        FilterSubject: "orders.>",
        AckPolicy:     nats.AckExplicitPolicy,
        AckWait:       30 * time.Second,
        MaxDeliver:    5,
        BackOff: []time.Duration{
            1 * time.Second,
            5 * time.Second,
            15 * time.Second,
            30 * time.Second,
        },
        DeliverPolicy: nats.DeliverAllPolicy,
        ReplayPolicy:  nats.ReplayInstantPolicy,
        MaxAckPending: 50,
    }
    if err := js.EnsureConsumer(jsc, cfg); err != nil {
        log.Fatalf("ensure consumer: %v", err)
    }

    sub, err := jsc.PullSubscribe("orders.>", durableName,
        nats.Bind(js.StreamName, durableName),
    )
    if err != nil {
        log.Fatalf("pull subscribe: %v", err)
    }

    ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
    defer cancel()

    log.Println("payment service: waiting for orders...")

    for {
        select {
        case <-ctx.Done():
            log.Println("payment service: shutting down")
            return
        default:
        }

        msgs, err := sub.Fetch(10, nats.MaxWait(2*time.Second))
        if err != nil {
            // ErrTimeout just means no messages arrived in the window — not an error
            if err == nats.ErrTimeout {
                continue
            }
            log.Printf("fetch: %v", err)
            continue
        }

        for _, msg := range msgs {
            processPayment(msg)
        }
    }
}

func processPayment(msg *nats.Msg) {
    var order model.Order
    if err := json.Unmarshal(msg.Data, &order); err != nil {
        log.Printf("payment: unmarshal error: %v — nacking", err)
        _ = msg.Term() // permanently reject malformed messages
        return
    }

    log.Printf("payment: processing order %s, total $%.2f", order.ID, order.Total)

    // --- real payment logic goes here ---
    // e.g., call Stripe, update DB, etc.
    time.Sleep(100 * time.Millisecond) // simulate work

    if err := msg.Ack(); err != nil {
        log.Printf("payment: ack error: %v", err)
    }
    log.Printf("payment: order %s payment processed OK", order.ID)
}
```

### cmd/inventory/main.go

```go
package main

import (
    "context"
    "encoding/json"
    "log"
    "os"
    "os/signal"
    "syscall"
    "time"

    "github.com/nats-io/nats.go"

    js "github.com/example/order-fanout/internal/jetstream"
    "github.com/example/order-fanout/internal/model"
)

const durableName = "inventory-service"

func main() {
    nc, err := nats.Connect(nats.DefaultURL)
    if err != nil {
        log.Fatalf("connect: %v", err)
    }
    defer nc.Drain()

    jsc, err := nc.JetStream()
    if err != nil {
        log.Fatalf("jetstream: %v", err)
    }

    if err := js.EnsureStream(jsc); err != nil {
        log.Fatalf("ensure stream: %v", err)
    }

    cfg := &nats.ConsumerConfig{
        Durable:       durableName,
        FilterSubject: "orders.>",
        AckPolicy:     nats.AckExplicitPolicy,
        AckWait:       10 * time.Second,
        MaxDeliver:    5,
        BackOff: []time.Duration{
            500 * time.Millisecond,
            2 * time.Second,
            10 * time.Second,
            30 * time.Second,
        },
        DeliverPolicy: nats.DeliverAllPolicy,
        ReplayPolicy:  nats.ReplayInstantPolicy,
        MaxAckPending: 200,
    }
    if err := js.EnsureConsumer(jsc, cfg); err != nil {
        log.Fatalf("ensure consumer: %v", err)
    }

    sub, err := jsc.PullSubscribe("orders.>", durableName,
        nats.Bind(js.StreamName, durableName),
    )
    if err != nil {
        log.Fatalf("pull subscribe: %v", err)
    }

    ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
    defer cancel()

    log.Println("inventory service: waiting for orders...")

    for {
        select {
        case <-ctx.Done():
            log.Println("inventory service: shutting down")
            return
        default:
        }

        msgs, err := sub.Fetch(25, nats.MaxWait(2*time.Second))
        if err != nil {
            if err == nats.ErrTimeout {
                continue
            }
            log.Printf("fetch: %v", err)
            continue
        }

        for _, msg := range msgs {
            processInventory(msg)
        }
    }
}

func processInventory(msg *nats.Msg) {
    var order model.Order
    if err := json.Unmarshal(msg.Data, &order); err != nil {
        log.Printf("inventory: unmarshal error: %v — terminating", err)
        _ = msg.Term()
        return
    }

    log.Printf("inventory: reserving stock for order %s (%d line items)", order.ID, len(order.Items))

    for _, item := range order.Items {
        // --- real inventory reservation logic goes here ---
        log.Printf("inventory:   reserving %d x %s", item.Quantity, item.SKU)
    }
    time.Sleep(30 * time.Millisecond) // simulate DB write

    if err := msg.Ack(); err != nil {
        log.Printf("inventory: ack error: %v", err)
    }
    log.Printf("inventory: order %s stock reserved OK", order.ID)
}
```

### cmd/notifications/main.go

```go
package main

import (
    "context"
    "encoding/json"
    "log"
    "os"
    "os/signal"
    "syscall"
    "time"

    "github.com/nats-io/nats.go"

    js "github.com/example/order-fanout/internal/jetstream"
    "github.com/example/order-fanout/internal/model"
)

const durableName = "notifications-service"

func main() {
    nc, err := nats.Connect(nats.DefaultURL)
    if err != nil {
        log.Fatalf("connect: %v", err)
    }
    defer nc.Drain()

    jsc, err := nc.JetStream()
    if err != nil {
        log.Fatalf("jetstream: %v", err)
    }

    if err := js.EnsureStream(jsc); err != nil {
        log.Fatalf("ensure stream: %v", err)
    }

    cfg := &nats.ConsumerConfig{
        Durable:       durableName,
        FilterSubject: "orders.>",
        AckPolicy:     nats.AckExplicitPolicy,
        AckWait:       10 * time.Second,
        MaxDeliver:    3,
        BackOff: []time.Duration{
            1 * time.Second,
            5 * time.Second,
            15 * time.Second,
        },
        DeliverPolicy: nats.DeliverAllPolicy,
        ReplayPolicy:  nats.ReplayInstantPolicy,
        MaxAckPending: 500,
    }
    if err := js.EnsureConsumer(jsc, cfg); err != nil {
        log.Fatalf("ensure consumer: %v", err)
    }

    sub, err := jsc.PullSubscribe("orders.>", durableName,
        nats.Bind(js.StreamName, durableName),
    )
    if err != nil {
        log.Fatalf("pull subscribe: %v", err)
    }

    ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
    defer cancel()

    log.Println("notifications service: waiting for orders...")

    for {
        select {
        case <-ctx.Done():
            log.Println("notifications service: shutting down")
            return
        default:
        }

        msgs, err := sub.Fetch(50, nats.MaxWait(2*time.Second))
        if err != nil {
            if err == nats.ErrTimeout {
                continue
            }
            log.Printf("fetch: %v", err)
            continue
        }

        for _, msg := range msgs {
            processNotification(msg)
        }
    }
}

func processNotification(msg *nats.Msg) {
    var order model.Order
    if err := json.Unmarshal(msg.Data, &order); err != nil {
        log.Printf("notifications: unmarshal error: %v — terminating", err)
        _ = msg.Term()
        return
    }

    log.Printf("notifications: sending confirmation to customer %s for order %s",
        order.CustomerID, order.ID)

    // --- real notification logic goes here ---
    // e.g., send email via SendGrid, push via FCM, etc.
    time.Sleep(20 * time.Millisecond)

    if err := msg.Ack(); err != nil {
        log.Printf("notifications: ack error: %v", err)
    }
    log.Printf("notifications: order %s notification sent OK", order.ID)
}
```

---

## How the Fan-Out Works

```
API
 │
 │  Publish("orders.created", payload)
 ▼
┌─────────────────────────────────┐
│  Stream: ORDERS                 │
│  Subjects: orders.>             │
│  Storage: File                  │
│                                 │
│  msg 1, msg 2, msg 3, ...       │
└──────┬──────────┬──────────┬───┘
       │          │          │
       ▼          ▼          ▼
  payment-   inventory-  notifications-
  service    service     service
  (cursor)   (cursor)    (cursor)
```

Each consumer maintains its own independent sequence pointer. Acknowledging a message on the `payment-service` consumer has no effect on the `inventory-service` cursor — both must independently ack the same stream message.

---

## Scaling Within a Single Service

To scale a single service horizontally (e.g., run 3 payment pods), use a **queue group** on top of the durable consumer. Replace `PullSubscribe` with:

```go
// All instances sharing the same queue group compete for messages,
// but still independent from inventory/notifications consumers.
sub, err := jsc.QueueSubscribeSync("orders.>", "payment-workers",
    nats.Bind(js.StreamName, durableName),
    nats.Durable(durableName),
)
```

Or with pull consumers, simply have multiple goroutines/pods all call `sub.Fetch()` on the same subscription — JetStream dispatches each message to only one fetcher within that consumer.

---

## Key Operational Notes

| Concern | Recommendation |
|---|---|
| Message ordering | JetStream delivers per-subject ordering by default. If strict global ordering is required, use a single-partition stream and MaxAckPending=1. |
| At-least-once vs exactly-once | Use `nats.MsgId()` on publish + `Duplicates` window on the stream for publisher deduplication. Consumer-side idempotency must be implemented in business logic. |
| Dead-letter handling | When `MaxDeliver` is exhausted JetStream stops retrying. Add a separate consumer with `DeliverPolicy: nats.DeliverAllPolicy` filtered to the same subjects as an audit log, or inspect via `nats consumer report`. |
| Graceful shutdown | Call `nc.Drain()` (not `nc.Close()`) — it flushes pending acks before closing. |
| NATS CLI inspection | `nats stream info ORDERS`, `nats consumer info ORDERS payment-service`, `nats consumer report ORDERS` |
