// Command worker is one instance of the order processor. Run as many copies as
// you like: they all bind the same durable pull consumer, so JetStream hands
// each order to exactly one of them and redelivers (with backoff) on failure.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"example.com/order-workqueue/orders"
	"github.com/nats-io/nats.go"
)

func main() {
	nc, err := nats.Connect(natsURL(),
		nats.RetryOnFailedConnect(true),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(2*time.Second),
	)
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	defer nc.Drain()

	js, err := nc.JetStream()
	if err != nil {
		log.Fatalf("jetstream: %v", err)
	}
	if err := orders.EnsureTopology(js); err != nil {
		log.Fatalf("topology: %v", err)
	}

	sub, err := js.PullSubscribe("", orders.ConsumerName, nats.Bind(orders.StreamName, orders.ConsumerName))
	if err != nil {
		log.Fatalf("subscribe: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log.Println("worker started")
	for ctx.Err() == nil {
		msgs, err := sub.Fetch(10, nats.MaxWait(5*time.Second))
		if err != nil {
			if errors.Is(err, nats.ErrTimeout) {
				continue // no work right now
			}
			log.Printf("fetch: %v", err)
			continue
		}
		for _, msg := range msgs {
			handle(msg)
		}
	}
	log.Println("worker shutting down")
}

func handle(msg *nats.Msg) {
	var o orders.Order
	if err := json.Unmarshal(msg.Data, &o); err != nil {
		// Poison payload: never going to parse, so stop redelivering it. The
		// Term advisory lets the DLQ router record it.
		log.Printf("unparseable order, terminating: %v", err)
		_ = msg.Term()
		return
	}

	if err := process(o); err != nil {
		// Transient failure: negative-ack so JetStream redelivers on the
		// configured backoff schedule, up to MaxDeliver.
		log.Printf("order %s failed, will retry: %v", o.ID, err)
		_ = msg.Nak()
		return
	}

	// Ack only after the work is durably done. If we crash before this, the
	// order is redelivered — so process() must be idempotent (see response.md).
	_ = msg.Ack()
	log.Printf("order %s processed", o.ID)
}

// process does the real work. Replace the body with your fulfilment logic.
// It must be idempotent: the same order may be delivered again after a crash
// or ack timeout.
func process(o orders.Order) error {
	// e.g. charge payment, reserve inventory, write to the orders DB...
	return nil
}

func natsURL() string {
	if u := os.Getenv("NATS_URL"); u != "" {
		return u
	}
	return nats.DefaultURL
}
