// Command dlq is the dead-letter router. JetStream has no built-in DLQ: when an
// order exhausts MaxDeliver (or a worker Term()s it) the server only publishes
// an advisory and then stops. On a WorkQueue stream the order also stays put
// and blocks nothing can take it, so this process must move it out and delete
// it. We consume the advisories durably (from ORDERS_ADVISORY) so none are lost
// across restarts.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"time"

	"example.com/order-workqueue/orders"
	"github.com/nats-io/nats.go"
)

// advisory is the subset of the MAX_DELIVERIES / MSG_TERMINATED advisory we use.
type advisory struct {
	Type      string `json:"type"`
	Stream    string `json:"stream"`
	Consumer  string `json:"consumer"`
	StreamSeq uint64 `json:"stream_seq"`
	Reason    string `json:"reason"` // set on terminated only
}

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

	if _, err := js.AddConsumer(orders.AdvisoryStreamName, &nats.ConsumerConfig{
		Durable:       orders.AdvisoryConsumer,
		AckPolicy:     nats.AckExplicitPolicy,
		AckWait:       30 * time.Second,
		MaxDeliver:    5,
		DeliverPolicy: nats.DeliverAllPolicy,
	}); err != nil {
		log.Fatalf("advisory consumer: %v", err)
	}

	sub, err := js.PullSubscribe("", orders.AdvisoryConsumer,
		nats.Bind(orders.AdvisoryStreamName, orders.AdvisoryConsumer))
	if err != nil {
		log.Fatalf("subscribe: %v", err)
	}

	log.Println("dlq router started")
	for {
		msgs, err := sub.Fetch(10, nats.MaxWait(30*time.Second))
		if err != nil {
			if errors.Is(err, nats.ErrTimeout) {
				continue
			}
			log.Printf("fetch: %v", err)
			continue
		}
		for _, m := range msgs {
			route(js, m)
		}
	}
}

func route(js nats.JetStreamContext, m *nats.Msg) {
	var adv advisory
	if err := json.Unmarshal(m.Data, &adv); err != nil {
		log.Printf("unparseable advisory, dropping: %v", err)
		_ = m.Term()
		return
	}

	// MaxDeliver keeps the order in the stream, so we can still fetch it by seq.
	raw, err := js.GetMsg(orders.StreamName, adv.StreamSeq)
	if err != nil {
		// Already gone (aged out). Nothing to move; record and ack.
		log.Printf("order seq=%d already gone (%s): %v", adv.StreamSeq, adv.Type, err)
		_ = m.Ack()
		return
	}

	hdr := nats.Header{}
	hdr.Set("Original-Stream", adv.Stream)
	hdr.Set("Original-Consumer", adv.Consumer)
	hdr.Set("Original-Seq", fmt.Sprintf("%d", adv.StreamSeq))
	hdr.Set("Failure-Reason", adv.Type) // max_deliver or terminated
	if adv.Reason != "" {
		hdr.Set("Term-Reason", adv.Reason)
	}
	// Reuse the original seq as the DLQ idempotency id, so a redelivered
	// advisory cannot write the same dead order twice.
	hdr.Set(nats.MsgIdHdr, fmt.Sprintf("%s-%d", orders.StreamName, adv.StreamSeq))

	if _, err := js.PublishMsg(&nats.Msg{Subject: orders.DLQSubject, Data: raw.Data, Header: hdr}); err != nil {
		log.Printf("dlq publish seq=%d failed, will retry: %v", adv.StreamSeq, err)
		_ = m.Nak()
		return
	}

	// On a WorkQueue stream the failed order is still occupying the queue.
	// Delete it now that it is safely in the DLQ, otherwise it lingers until
	// MaxAge. Safe to repeat: a second delete of the same seq just errors.
	if err := js.DeleteMsg(orders.StreamName, adv.StreamSeq); err != nil {
		log.Printf("delete seq=%d from %s: %v", adv.StreamSeq, orders.StreamName, err)
	}

	_ = m.Ack() // ack only after the DLQ write succeeded
	log.Printf("order seq=%d moved to DLQ (%s)", adv.StreamSeq, adv.Type)
}

func natsURL() string {
	if u := os.Getenv("NATS_URL"); u != "" {
		return u
	}
	return nats.DefaultURL
}
