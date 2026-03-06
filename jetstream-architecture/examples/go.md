# Go Examples

Complete JetStream examples using `github.com/nats-io/nats.go`.

## Connection and JetStream Context

```go
package main

import (
    "log"
    "time"

    "github.com/nats-io/nats.go"
)

func main() {
    // Connect with reconnect and error handling
    nc, err := nats.Connect("nats://localhost:4222",
        nats.RetryOnFailedConnect(true),
        nats.MaxReconnects(-1),
        nats.ReconnectWait(2*time.Second),
        nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
            log.Printf("disconnected: %v", err)
        }),
        nats.ReconnectHandler(func(_ *nats.Conn) {
            log.Println("reconnected")
        }),
        nats.ErrorHandler(func(_ *nats.Conn, _ *nats.Subscription, err error) {
            log.Printf("nats error: %v", err)
        }),
    )
    if err != nil {
        log.Fatal(err)
    }
    defer nc.Drain()

    js, err := nc.JetStream(
        nats.PublishAsyncMaxPending(256),
    )
    if err != nil {
        log.Fatal(err)
    }
}
```

## Stream Creation

```go
func createStream(js nats.JetStreamContext) error {
    _, err := js.AddStream(&nats.StreamConfig{
        Name:     "ORDERS",
        Subjects: []string{"orders.>"},

        // Retention
        Retention: nats.LimitsPolicy,
        MaxAge:    30 * 24 * time.Hour,
        MaxBytes:  5 * 1024 * 1024 * 1024, // 5 GB
        MaxMsgs:   -1,

        // Storage
        Storage:  nats.FileStorage,
        Replicas: 3,

        // Behavior
        Discard:         nats.DiscardOld,
        DuplicateWindow: 2 * time.Minute,

        MaxMsgsPerSubject: 1000,
    })
    return err
}
```

## Publishing

```go
// Synchronous publish
func publishSync(js nats.JetStreamContext) {
    ack, err := js.Publish("orders.created", []byte(`{"id":"ord-123","total":99.99}`))
    if err != nil {
        log.Printf("publish failed: %v", err)
        return
    }
    log.Printf("published to stream=%s seq=%d", ack.Stream, ack.Sequence)
}

// Publish with deduplication
func publishIdempotent(js nats.JetStreamContext, orderID string, data []byte) error {
    _, err := js.PublishMsg(&nats.Msg{
        Subject: "orders.created",
        Data:    data,
        Header:  nats.Header{"Nats-Msg-Id": []string{orderID}},
    })
    return err
}

// Async publish for higher throughput
func publishAsync(js nats.JetStreamContext, orders [][]byte) {
    for _, data := range orders {
        _, err := js.PublishAsync("orders.created", data)
        if err != nil {
            log.Printf("async publish error: %v", err)
        }
    }

    select {
    case <-js.PublishAsyncComplete():
        log.Println("all messages acknowledged")
    case <-time.After(10 * time.Second):
        log.Println("timeout waiting for acks")
    }
}
```

## Pull Consumer

```go
func pullConsumer(js nats.JetStreamContext) {
    // Create durable pull consumer
    _, err := js.AddConsumer("ORDERS", &nats.ConsumerConfig{
        Durable:       "order-processor",
        AckPolicy:     nats.AckExplicit,
        AckWait:       30 * time.Second,
        MaxDeliver:    5,
        MaxAckPending: 1000,
        FilterSubject: "orders.>",
        Backoff: []time.Duration{
            2 * time.Second,
            10 * time.Second,
            60 * time.Second,
            5 * time.Minute,
        },
    })
    if err != nil {
        log.Fatal(err)
    }

    sub, err := js.PullSubscribe("orders.>", "order-processor")
    if err != nil {
        log.Fatal(err)
    }

    for {
        msgs, err := sub.Fetch(100, nats.MaxWait(5*time.Second))
        if err != nil {
            if err == nats.ErrTimeout {
                continue
            }
            log.Printf("fetch error: %v", err)
            continue
        }

        for _, msg := range msgs {
            if err := handleOrder(msg); err != nil {
                log.Printf("error: %v", err)
                msg.NakWithDelay(5 * time.Second)
                continue
            }
            msg.Ack()
        }
    }
}

func handleOrder(msg *nats.Msg) error {
    meta, _ := msg.Metadata()
    log.Printf("processing seq=%d subject=%s attempt=%d",
        meta.Sequence.Stream, msg.Subject, meta.NumDelivered)
    return nil
}
```

## Push Consumer

```go
func pushConsumer(js nats.JetStreamContext) {
    sub, err := js.QueueSubscribe(
        "orders.>",
        "order-handlers",
        func(msg *nats.Msg) {
            meta, _ := msg.Metadata()
            log.Printf("received seq=%d", meta.Sequence.Stream)

            if err := processOrder(msg.Data); err != nil {
                msg.Nak()
                return
            }
            msg.Ack()
        },
        nats.Durable("order-handler"),
        nats.AckExplicit(),
        nats.MaxAckPending(500),
        nats.AckWait(30*time.Second),
        nats.MaxDeliver(5),
        nats.DeliverNew(),
    )
    if err != nil {
        log.Fatal(err)
    }
    defer sub.Unsubscribe()
}
```

## Graceful Shutdown

```go
func gracefulShutdown(nc *nats.Conn) {
    if err := nc.Drain(); err != nil {
        log.Printf("drain error: %v", err)
    }
}
```
