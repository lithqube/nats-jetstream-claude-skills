// Worker: pulls orders from the shared durable consumer and processes them.
// Run as many instances as you like; the server gives each order to exactly one.
package main

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"order-workqueue/order"
)

func main() {
	nc, err := nats.Connect(nats.DefaultURL)
	if err != nil {
		log.Fatal(err)
	}
	defer nc.Drain()

	js, err := jetstream.New(nc)
	if err != nil {
		log.Fatal(err)
	}
	cons, err := order.Setup(context.Background(), js)
	if err != nil {
		log.Fatal(err)
	}

	cc, err := cons.Consume(func(msg jetstream.Msg) {
		var o order.Order
		if err := json.Unmarshal(msg.Data(), &o); err != nil {
			// Unparseable message will never succeed: terminate so it is not
			// retried until MaxDeliver is exhausted.
			log.Printf("bad message, terminating: %v", err)
			msg.Term()
			return
		}

		if err := process(o); err != nil {
			// Nak triggers redelivery after the consumer's configured backoff.
			// After MaxDeliver attempts the server stops redelivering.
			log.Printf("order %s failed, will retry: %v", o.ID, err)
			msg.Nak()
			return
		}
		if err := msg.Ack(); err != nil {
			log.Printf("ack failed for order %s: %v", o.ID, err)
			return
		}
		log.Printf("order %s processed", o.ID)
	})
	if err != nil {
		log.Fatal(err)
	}
	defer cc.Stop()

	log.Println("worker consuming; ctrl-c to stop")
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
}

// process does the real work for one order. Return an error to trigger a retry.
// Make this idempotent: a message can be redelivered (e.g. after a crash
// between the side effect and the ack).
func process(o order.Order) error {
	// charge payment, reserve inventory, write to DB, ...
	return nil
}
