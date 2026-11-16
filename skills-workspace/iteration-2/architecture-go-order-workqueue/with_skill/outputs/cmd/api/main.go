// Command api is the web-facing producer. It accepts orders over HTTP and
// publishes each one to the ORDERS work queue with an idempotency id, so a
// client retry of POST /orders does not enqueue the order twice.
package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
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

	http.HandleFunc("POST /orders", handleCreate(js))
	log.Println("order API listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

func handleCreate(js nats.JetStreamContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var o orders.Order
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&o); err != nil {
			http.Error(w, "invalid order json", http.StatusBadRequest)
			return
		}
		if o.ID == "" || o.Amount <= 0 {
			http.Error(w, "order requires non-empty id and positive amount", http.StatusBadRequest)
			return
		}

		data, _ := json.Marshal(o)
		// Synchronous publish: we only tell the client "accepted" once the
		// stream has durably stored the order. Nats-Msg-Id makes a retry of
		// this same call a no-op inside the stream's duplicate window.
		_, err := js.PublishMsg(&nats.Msg{
			Subject: orders.SubjectNew,
			Data:    data,
			Header:  nats.Header{nats.MsgIdHdr: []string{o.ID}},
		}, nats.AckWait(5*time.Second))
		if err != nil {
			// Any publish failure (incl. a full DiscardNew queue applying
			// backpressure) means we did not accept the order. Tell the client
			// to retry rather than returning a 500 it will treat as fatal.
			log.Printf("publish order %s: %v", o.ID, err)
			http.Error(w, "could not enqueue order, retry later", http.StatusServiceUnavailable)
			return
		}

		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "accepted", "id": o.ID})
	}
}

func natsURL() string {
	if u := os.Getenv("NATS_URL"); u != "" {
		return u
	}
	return nats.DefaultURL
}
