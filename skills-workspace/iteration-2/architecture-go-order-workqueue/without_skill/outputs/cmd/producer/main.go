// Producer: a web API that enqueues orders onto the JetStream work queue.
package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"

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
	if _, err := order.Setup(context.Background(), js); err != nil {
		log.Fatal(err)
	}

	http.HandleFunc("POST /orders", func(w http.ResponseWriter, r *http.Request) {
		var o order.Order
		if err := json.NewDecoder(r.Body).Decode(&o); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		o.CreatedAt = time.Now().UTC()
		if err := o.Validate(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		data, _ := json.Marshal(o)

		// WithMsgID makes the publish idempotent within the stream's dedup
		// window: a retried HTTP request with the same order ID is deduplicated
		// by the server instead of enqueuing a second copy.
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if _, err := js.Publish(ctx, order.SubjectNew, data, jetstream.WithMsgID(o.ID)); err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	})

	log.Println("producer API listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}
