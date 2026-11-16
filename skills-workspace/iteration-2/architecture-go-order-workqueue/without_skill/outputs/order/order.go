package order

import (
	"context"
	"errors"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// Order is the unit of work produced by the API and processed by exactly one worker.
type Order struct {
	ID          string    `json:"id"`
	CustomerID  string    `json:"customer_id"`
	AmountCents int64     `json:"amount_cents"`
	CreatedAt   time.Time `json:"created_at"`
}

func (o Order) Validate() error {
	if o.ID == "" {
		return errors.New("order: empty id")
	}
	if o.AmountCents <= 0 {
		return errors.New("order: amount must be positive")
	}
	return nil
}

const (
	StreamName   = "ORDERS"
	ConsumerName = "order-workers"
	SubjectAll   = "orders.>"
	SubjectNew   = "orders.created"
)

// Setup declares the work-queue stream and the single shared durable pull
// consumer. Both calls are idempotent (CreateOrUpdate), so every producer and
// worker process can safely run this on startup.
func Setup(ctx context.Context, js jetstream.JetStream) (jetstream.Consumer, error) {
	_, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:       StreamName,
		Subjects:   []string{SubjectAll},
		Retention:  jetstream.WorkQueuePolicy, // deliver each msg once, drop it on ack
		Storage:    jetstream.FileStorage,     // survive a server restart
		MaxAge:     24 * time.Hour,            // safety valve for stuck messages
		Duplicates: 2 * time.Minute,           // dedup window for idempotent publish
	})
	if err != nil {
		return nil, err
	}

	// One durable consumer, many worker instances pulling from it: the server
	// hands each message to exactly one worker. AckWait + MaxDeliver + BackOff
	// give automatic retries with backoff on failure.
	return js.CreateOrUpdateConsumer(ctx, StreamName, jetstream.ConsumerConfig{
		Durable:       ConsumerName,
		FilterSubject: SubjectAll,
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       30 * time.Second, // redeliver if not acked in time (e.g. worker crash)
		MaxDeliver:    5,                // give up after 5 attempts
		BackOff:       []time.Duration{time.Second, 5 * time.Second, 15 * time.Second, 30 * time.Second},
		MaxAckPending: 1000, // cap in-flight work across all workers
	})
}
