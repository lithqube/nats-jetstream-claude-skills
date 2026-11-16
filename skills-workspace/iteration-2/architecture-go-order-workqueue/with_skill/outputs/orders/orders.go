// Package orders holds the shared domain type and the JetStream topology for
// the order-processing work queue: one stream, one competing-consumer, plus
// the advisory + dead-letter streams that stop a failed order from vanishing.
package orders

import (
	"time"

	"github.com/nats-io/nats.go"
)

// Topology names. Keep them in one place so the API, worker and DLQ router
// cannot drift.
const (
	StreamName   = "ORDERS"
	SubjectAll   = "orders.>"
	SubjectNew   = "orders.created"
	ConsumerName = "order-workers"

	DLQStreamName = "ORDERS_DLQ"
	DLQSubject    = "dlq.orders.failed"

	AdvisoryStreamName = "ORDERS_ADVISORY"
	// Only the two advisories we act on — not the $JS.EVENT.ADVISORY.> firehose,
	// which also carries a per-API-call audit advisory.
	advMaxDeliveries = "$JS.EVENT.ADVISORY.CONSUMER.MAX_DELIVERIES." + StreamName + ".>"
	advMsgTerminated = "$JS.EVENT.ADVISORY.CONSUMER.MSG_TERMINATED." + StreamName + ".>"
	AdvisoryConsumer = "dlq-router"
)

// Order is the message payload. ID is the idempotency key: it becomes the
// Nats-Msg-Id on publish and the row key in a consumer's inbox table.
type Order struct {
	ID       string  `json:"id"`
	Customer string  `json:"customer"`
	Amount   float64 `json:"amount"`
}

// EnsureTopology provisions every stream and the shared consumer. It is
// idempotent — AddStream/AddConsumer with an unchanged config is a no-op — so
// every process can call it on startup without coordination.
func EnsureTopology(js nats.JetStreamContext) error {
	// Work queue: each order is delivered to exactly one worker and removed on
	// ack. DiscardNew turns a full queue into publish-side backpressure instead
	// of silently dropping the newest orders.
	if _, err := js.AddStream(&nats.StreamConfig{
		Name:      StreamName,
		Subjects:  []string{SubjectAll},
		Retention: nats.WorkQueuePolicy,
		Storage:   nats.FileStorage,
		Replicas:  3,
		MaxMsgs:   1_000_000,
		Discard:   nats.DiscardNew,
		MaxAge:    24 * time.Hour,
		// Collapse duplicate publishes (web-API retries) within the window.
		// Size this >= your slowest publish-retry interval.
		Duplicates: 5 * time.Minute,
	}); err != nil {
		return err
	}

	// One durable pull consumer shared by every worker instance == competing
	// consumers == exactly-one-worker-per-order. AckExplicit + MaxDeliver +
	// Backoff give bounded retries on failure.
	if _, err := js.AddConsumer(StreamName, &nats.ConsumerConfig{
		Durable:       ConsumerName,
		AckPolicy:     nats.AckExplicitPolicy,
		AckWait:       30 * time.Second,
		MaxDeliver:    5,
		MaxAckPending: 100, // in-flight cap, shared across all workers
		BackOff: []time.Duration{
			5 * time.Second,
			30 * time.Second,
			2 * time.Minute,
			10 * time.Minute,
		},
	}); err != nil {
		return err
	}

	// Durable capture of the max-deliveries / terminated advisories. Without
	// this stream the advisory is fire-and-forget and a restart loses it.
	if _, err := js.AddStream(&nats.StreamConfig{
		Name:      AdvisoryStreamName,
		Subjects:  []string{advMaxDeliveries, advMsgTerminated},
		Retention: nats.LimitsPolicy,
		Storage:   nats.FileStorage,
		Replicas:  3,
		MaxAge:    30 * 24 * time.Hour,
	}); err != nil {
		return err
	}

	// Dead-letter store for orders that exhausted their retries.
	if _, err := js.AddStream(&nats.StreamConfig{
		Name:      DLQStreamName,
		Subjects:  []string{DLQSubject},
		Retention: nats.LimitsPolicy,
		Storage:   nats.FileStorage,
		Replicas:  3,
		MaxAge:    30 * 24 * time.Hour,
	}); err != nil {
		return err
	}

	return nil
}
