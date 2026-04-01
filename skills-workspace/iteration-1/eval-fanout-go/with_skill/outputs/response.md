# Order Processing System with NATS JetStream — Fanout Design

## Stream Design Rationale

This is a classic **fanout pattern**: one event (an order) must be independently processed by three services (payment, inventory, notifications). Each service must receive every order, maintain its own position, and process at its own pace.

Key design decisions:

1. **One stream, three durable consumers** — the `ORDERS` stream captures all order events. Each service gets its own durable pull consumer with independent delivery state. One consumer's failure or slowness does not affect the others.

2. **LimitsPolicy retention** — orders have business value beyond delivery. A 7-day window gives services time to catch up after downtime and provides an audit trail. `InterestPolicy` would be risky here: if all three consumers are registered but one falls behind and loses its consumer binding, messages could be deleted before it processes them.

3. **Pull consumers over push** — pull consumers provide natural backpressure. Payment processing in particular may be slow (external API calls); pull lets it fetch only what it can handle. All three services can also scale horizontally by adding workers that fetch from the same durable consumer.

4. **`orders.created` subject** — published as `orders.created`. The stream captures `orders.>` so future event types (`orders.updated`, `orders.cancelled`) are automatically included. Consumer `FilterSubject` can be narrowed per service if needed.

5. **Deduplication via `Nats-Msg-Id`** — the API publisher sets the order ID as the message ID header. Within the 2-minute `DuplicateWindow`, retried publishes are safely deduplicated by JetStream.

6. **`MaxDeliver` with exponential backoff** — services have different retry tolerances. Payment uses tighter limits (5 retries, longer backoff) since duplicate charges are dangerous. Notifications use fewer retries with faster backoff. All consumers use `AckExplicit` — no implicit acks in production.

---

## Stream Config

```go
&nats.StreamConfig{
    Name:     "ORDERS",
    Subjects: []string{"orders.>"},

    // Retain messages for 7 days regardless of consumer state
    Retention: nats.LimitsPolicy,
    MaxAge:    7 * 24 * time.Hour,
    MaxBytes:  10 * 1024 * 1024 * 1024, // 10 GB cap

    // Durable, replicated storage for production
    Storage:  nats.FileStorage,
    Replicas: 3,

    // Drop oldest messages when limits are reached
    Discard: nats.DiscardOld,

    // Deduplicate publishes within a 2-minute window using Nats-Msg-Id
    DuplicateWindow: 2 * time.Minute,
}
```

---

## Consumer Configs

### Payment Service

Processes all order events. Strict retry limit and exponential backoff — avoids hammering a downstream payment provider and prevents double-charge risk on repeated delivery.

```go
&nats.ConsumerConfig{
    Durable:       "payment-service",
    FilterSubject: "orders.>",
    AckPolicy:     nats.AckExplicit,
    DeliverPolicy: nats.DeliverAllPolicy,
    AckWait:       45 * time.Second, // payment calls can be slow
    MaxDeliver:    5,
    MaxAckPending: 100, // conservative — payment is expensive
    Backoff: []time.Duration{
        5 * time.Second,
        30 * time.Second,
        2 * time.Minute,
        10 * time.Minute,
    },
}
```

### Inventory Service

Processes all order events. Faster ack wait since inventory updates are typically quick local database writes.

```go
&nats.ConsumerConfig{
    Durable:       "inventory-service",
    FilterSubject: "orders.>",
    AckPolicy:     nats.AckExplicit,
    DeliverPolicy: nats.DeliverAllPolicy,
    AckWait:       30 * time.Second,
    MaxDeliver:    7,
    MaxAckPending: 500,
    Backoff: []time.Duration{
        2 * time.Second,
        10 * time.Second,
        30 * time.Second,
        2 * time.Minute,
    },
}
```

### Notifications Service

Processes order events for customer-facing notifications. Fewer retries — a missed notification is acceptable, but a flood of duplicate notifications is not.

```go
&nats.ConsumerConfig{
    Durable:       "notifications-service",
    FilterSubject: "orders.>",
    AckPolicy:     nats.AckExplicit,
    DeliverPolicy: nats.DeliverAllPolicy,
    AckWait:       15 * time.Second,
    MaxDeliver:    3,
    MaxAckPending: 1000,
    Backoff: []time.Duration{
        3 * time.Second,
        15 * time.Second,
    },
}
```

---

## Working Go Code

### `main.go` — Stream and Consumer Setup

```go
package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/nats-io/nats.go"
)

func main() {
	natsURL := os.Getenv("NATS_URL")
	if natsURL == "" {
		natsURL = nats.DefaultURL
	}

	nc, err := nats.Connect(natsURL,
		nats.RetryOnFailedConnect(true),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(2*time.Second),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			log.Printf("disconnected: %v", err)
		}),
		nats.ReconnectHandler(func(_ *nats.Conn) {
			log.Println("reconnected to NATS")
		}),
		nats.ErrorHandler(func(_ *nats.Conn, _ *nats.Subscription, err error) {
			log.Printf("nats error: %v", err)
		}),
	)
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	defer nc.Drain()

	js, err := nc.JetStream(nats.PublishAsyncMaxPending(256))
	if err != nil {
		log.Fatalf("jetstream: %v", err)
	}

	if err := setupStream(js); err != nil {
		log.Fatalf("setup stream: %v", err)
	}
	if err := setupConsumers(js); err != nil {
		log.Fatalf("setup consumers: %v", err)
	}

	// Start each service worker in its own goroutine
	go runPaymentWorker(js)
	go runInventoryWorker(js)
	go runNotificationsWorker(js)

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("shutting down")
}

func setupStream(js nats.JetStreamContext) error {
	cfg := &nats.StreamConfig{
		Name:     "ORDERS",
		Subjects: []string{"orders.>"},

		Retention: nats.LimitsPolicy,
		MaxAge:    7 * 24 * time.Hour,
		MaxBytes:  10 * 1024 * 1024 * 1024,

		Storage:  nats.FileStorage,
		Replicas: 3,

		Discard:         nats.DiscardOld,
		DuplicateWindow: 2 * time.Minute,
	}

	// AddStream is idempotent if config is unchanged; use UpdateStream if you need to modify
	_, err := js.AddStream(cfg)
	if err != nil {
		// Stream may already exist with the same config — treat as non-fatal
		if err != nats.ErrStreamNameAlreadyInUse {
			return err
		}
	}
	log.Println("stream ORDERS ready")
	return nil
}

func setupConsumers(js nats.JetStreamContext) error {
	consumers := []struct {
		name   string
		config *nats.ConsumerConfig
	}{
		{
			name: "payment-service",
			config: &nats.ConsumerConfig{
				Durable:       "payment-service",
				FilterSubject: "orders.>",
				AckPolicy:     nats.AckExplicit,
				DeliverPolicy: nats.DeliverAllPolicy,
				AckWait:       45 * time.Second,
				MaxDeliver:    5,
				MaxAckPending: 100,
				Backoff: []time.Duration{
					5 * time.Second,
					30 * time.Second,
					2 * time.Minute,
					10 * time.Minute,
				},
			},
		},
		{
			name: "inventory-service",
			config: &nats.ConsumerConfig{
				Durable:       "inventory-service",
				FilterSubject: "orders.>",
				AckPolicy:     nats.AckExplicit,
				DeliverPolicy: nats.DeliverAllPolicy,
				AckWait:       30 * time.Second,
				MaxDeliver:    7,
				MaxAckPending: 500,
				Backoff: []time.Duration{
					2 * time.Second,
					10 * time.Second,
					30 * time.Second,
					2 * time.Minute,
				},
			},
		},
		{
			name: "notifications-service",
			config: &nats.ConsumerConfig{
				Durable:       "notifications-service",
				FilterSubject: "orders.>",
				AckPolicy:     nats.AckExplicit,
				DeliverPolicy: nats.DeliverAllPolicy,
				AckWait:       15 * time.Second,
				MaxDeliver:    3,
				MaxAckPending: 1000,
				Backoff: []time.Duration{
					3 * time.Second,
					15 * time.Second,
				},
			},
		},
	}

	for _, c := range consumers {
		if _, err := js.AddConsumer("ORDERS", c.config); err != nil {
			if err != nats.ErrConsumerNameAlreadyInUse {
				return fmt.Errorf("add consumer %s: %w", c.name, err)
			}
		}
		log.Printf("consumer %s ready", c.name)
	}
	return nil
}
```

### `producer.go` — API Order Publisher

```go
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/nats-io/nats.go"
)

type Order struct {
	ID         string    `json:"id"`
	CustomerID string    `json:"customer_id"`
	Items      []Item    `json:"items"`
	TotalCents int64     `json:"total_cents"`
	CreatedAt  time.Time `json:"created_at"`
}

type Item struct {
	SKU      string `json:"sku"`
	Quantity int    `json:"quantity"`
	Price    int64  `json:"price_cents"`
}

// PublishOrder publishes an order to the ORDERS stream.
// The order ID is used as the Nats-Msg-Id for deduplication within the
// DuplicateWindow (2 minutes). Retrying a failed publish with the same
// order ID is safe.
func PublishOrder(js nats.JetStreamContext, order Order) error {
	data, err := json.Marshal(order)
	if err != nil {
		return fmt.Errorf("marshal order: %w", err)
	}

	msg := &nats.Msg{
		Subject: "orders.created",
		Data:    data,
		Header:  nats.Header{"Nats-Msg-Id": []string{order.ID}},
	}

	ack, err := js.PublishMsg(msg)
	if err != nil {
		return fmt.Errorf("publish order %s: %w", order.ID, err)
	}

	log.Printf("published order=%s stream=%s seq=%d", order.ID, ack.Stream, ack.Sequence)
	return nil
}
```

### `payment_worker.go` — Payment Service Consumer

```go
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/nats-io/nats.go"
)

func runPaymentWorker(js nats.JetStreamContext) {
	sub, err := js.PullSubscribe("orders.>", "payment-service")
	if err != nil {
		log.Fatalf("payment worker subscribe: %v", err)
	}
	defer sub.Unsubscribe()

	log.Println("payment worker started")

	for {
		msgs, err := sub.Fetch(10, nats.MaxWait(5*time.Second))
		if err != nil {
			if err == nats.ErrTimeout {
				continue // no messages available, loop back
			}
			log.Printf("payment fetch error: %v", err)
			time.Sleep(1 * time.Second)
			continue
		}

		for _, msg := range msgs {
			if err := processPayment(msg); err != nil {
				log.Printf("payment processing failed: %v", err)
				// NakWithDelay triggers the Backoff schedule configured on the consumer.
				// For payment, we never want to hammer the payment provider.
				msg.NakWithDelay(5 * time.Second)
				continue
			}
			msg.Ack()
		}
	}
}

func processPayment(msg *nats.Msg) error {
	meta, err := msg.Metadata()
	if err != nil {
		// Malformed metadata — term the message so it is never redelivered
		msg.Term()
		return fmt.Errorf("bad metadata: %w", err)
	}

	log.Printf("payment: processing seq=%d subject=%s attempt=%d/%d",
		meta.Sequence.Stream, msg.Subject, meta.NumDelivered, meta.NumPending)

	var order Order
	if err := json.Unmarshal(msg.Data, &order); err != nil {
		msg.Term() // unparseable — permanent failure
		return fmt.Errorf("unmarshal order: %w", err)
	}

	// Signal to NATS that processing is still in progress.
	// Call this before any long-running external API call to prevent
	// the message from being redelivered due to AckWait expiry.
	msg.InProgress()

	// TODO: call payment provider
	log.Printf("payment: charged order=%s total=%d cents", order.ID, order.TotalCents)
	return nil
}
```

### `inventory_worker.go` — Inventory Service Consumer

```go
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/nats-io/nats.go"
)

func runInventoryWorker(js nats.JetStreamContext) {
	sub, err := js.PullSubscribe("orders.>", "inventory-service")
	if err != nil {
		log.Fatalf("inventory worker subscribe: %v", err)
	}
	defer sub.Unsubscribe()

	log.Println("inventory worker started")

	for {
		msgs, err := sub.Fetch(50, nats.MaxWait(5*time.Second))
		if err != nil {
			if err == nats.ErrTimeout {
				continue
			}
			log.Printf("inventory fetch error: %v", err)
			time.Sleep(1 * time.Second)
			continue
		}

		for _, msg := range msgs {
			if err := processInventory(msg); err != nil {
				log.Printf("inventory processing failed: %v", err)
				msg.Nak() // use consumer Backoff schedule for retry timing
				continue
			}
			msg.Ack()
		}
	}
}

func processInventory(msg *nats.Msg) error {
	meta, err := msg.Metadata()
	if err != nil {
		msg.Term()
		return fmt.Errorf("bad metadata: %w", err)
	}

	log.Printf("inventory: processing seq=%d subject=%s attempt=%d",
		meta.Sequence.Stream, msg.Subject, meta.NumDelivered)

	var order Order
	if err := json.Unmarshal(msg.Data, &order); err != nil {
		msg.Term()
		return fmt.Errorf("unmarshal order: %w", err)
	}

	// TODO: decrement stock for each item
	for _, item := range order.Items {
		log.Printf("inventory: reserve sku=%s qty=%d for order=%s",
			item.SKU, item.Quantity, order.ID)
	}
	return nil
}
```

### `notifications_worker.go` — Notifications Service Consumer

```go
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/nats-io/nats.go"
)

func runNotificationsWorker(js nats.JetStreamContext) {
	sub, err := js.PullSubscribe("orders.>", "notifications-service")
	if err != nil {
		log.Fatalf("notifications worker subscribe: %v", err)
	}
	defer sub.Unsubscribe()

	log.Println("notifications worker started")

	for {
		msgs, err := sub.Fetch(100, nats.MaxWait(5*time.Second))
		if err != nil {
			if err == nats.ErrTimeout {
				continue
			}
			log.Printf("notifications fetch error: %v", err)
			time.Sleep(1 * time.Second)
			continue
		}

		for _, msg := range msgs {
			if err := processNotification(msg); err != nil {
				log.Printf("notification processing failed: %v", err)
				msg.Nak()
				continue
			}
			msg.Ack()
		}
	}
}

func processNotification(msg *nats.Msg) error {
	meta, err := msg.Metadata()
	if err != nil {
		msg.Term()
		return fmt.Errorf("bad metadata: %w", err)
	}

	log.Printf("notifications: processing seq=%d subject=%s attempt=%d",
		meta.Sequence.Stream, msg.Subject, meta.NumDelivered)

	var order Order
	if err := json.Unmarshal(msg.Data, &order); err != nil {
		msg.Term()
		return fmt.Errorf("unmarshal order: %w", err)
	}

	// TODO: send confirmation email / push notification
	log.Printf("notifications: sending confirmation to customer=%s for order=%s",
		order.CustomerID, order.ID)
	return nil
}
```

---

## Scaling Horizontally

Each service can scale by adding more worker goroutines (or processes) fetching from the same durable consumer. JetStream distributes messages across all active fetchers automatically:

```go
// Scale payment workers to 3 goroutines
numPaymentWorkers := 3
for i := 0; i < numPaymentWorkers; i++ {
    go runPaymentWorker(js)
}
```

No configuration changes are needed — all workers share the `payment-service` durable consumer and JetStream ensures each message is delivered to only one of them.

---

## Key Design Summary

| Concern | Decision |
|---|---|
| Pattern | Fanout — each service processes every message independently |
| Stream retention | `LimitsPolicy` with 7-day `MaxAge` |
| Storage | `FileStorage`, `Replicas: 3` |
| Consumer type | Pull consumers — backpressure, horizontal scaling |
| Ack policy | `AckExplicit` on all consumers |
| Deduplication | `Nats-Msg-Id` header with 2-minute `DuplicateWindow` |
| Retry | `MaxDeliver` + `Backoff` per consumer, sized to each service's risk tolerance |
| Permanent failures | `msg.Term()` for unrecoverable errors (bad JSON, bad metadata) |
| Long operations | `msg.InProgress()` before external calls to prevent false redelivery |
