# Streams

Streams persist messages in JetStream.

Example configuration:

```go
js.AddStream(&nats.StreamConfig{
    Name: "EVENTS",
    Subjects: []string{"events.*"},
    Storage: nats.FileStorage,
    Replicas: 3,
})
```
