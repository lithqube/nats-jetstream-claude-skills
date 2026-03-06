# Go Producer Example

```go
nc, _ := nats.Connect(nats.DefaultURL)
js, _ := nc.JetStream()

js.Publish("orders.created", []byte("order123"))
```
