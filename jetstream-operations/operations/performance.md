# Performance Tuning

## Publish Throughput

### Async Publish

Synchronous publish waits for server ack on each message. Async publish batches messages and checks acks asynchronously.

```go
// Sync: ~10k msgs/sec (round-trip per message)
ack, err := js.Publish("orders.created", data)

// Async: ~100k+ msgs/sec (batched, non-blocking)
_, err := js.PublishAsync("orders.created", data)
// Check completion later
<-js.PublishAsyncComplete()
```

Set `PublishAsyncMaxPending` to control buffer size:

```go
js, _ := nc.JetStream(nats.PublishAsyncMaxPending(4096))
```

### Publish Batching Guidelines

| Scenario | Strategy |
|----------|----------|
| Low volume (< 1k/s) | Sync publish is fine |
| Medium volume (1k-50k/s) | Async publish, buffer 256-1024 |
| High volume (> 50k/s) | Async publish, buffer 4096+, consider multiple connections |

### Multiple Publisher Connections

For very high throughput, use multiple NATS connections to parallelize publishing across CPU cores:

```go
// Each connection uses its own goroutine/thread
for i := 0; i < runtime.NumCPU(); i++ {
    nc, _ := nats.Connect(url)
    js, _ := nc.JetStream(nats.PublishAsyncMaxPending(1024))
    go publishWorker(js, messages)
}
```

## Consumer Throughput

### Fetch Batch Size

The most impactful consumer tuning parameter. Larger batches reduce round-trips.

```go
// Slow: 1 message at a time
msgs, _ := sub.Fetch(1)

// Better: batch of 100
msgs, _ := sub.Fetch(100, nats.MaxWait(5*time.Second))

// High throughput: batch of 500-1000
msgs, _ := sub.Fetch(500, nats.MaxWait(5*time.Second))
```

### Fetch Batch Guidelines

| Processing Time Per Message | Recommended Batch |
|---------------------------|-------------------|
| < 1ms (simple transforms) | 500-1000 |
| 1-10ms (database writes) | 100-500 |
| 10-100ms (API calls) | 10-50 |
| > 100ms (heavy processing) | 1-10 |

### MaxAckPending Tuning

`MaxAckPending` limits in-flight (unacknowledged) messages per consumer. Too low = underutilization. Too high = memory pressure during failures.

```bash
# Check current utilization
nats consumer info ORDERS processor
# If Num Ack Pending consistently equals Max Ack Pending, increase it

nats consumer edit ORDERS processor --max-pending=5000
```

Guidelines:
- Default: 1000
- Fast consumers with small messages: 5000-20000
- Slow consumers or large messages: 100-500
- Single-threaded consumer: match to batch size

### Parallel Workers

Scale horizontally by adding workers to the same pull consumer:

```go
for i := 0; i < numWorkers; i++ {
    go func(workerID int) {
        sub, _ := js.PullSubscribe("orders.>", "processor")
        for {
            msgs, _ := sub.Fetch(100, nats.MaxWait(5*time.Second))
            for _, msg := range msgs {
                process(msg)
                msg.Ack()
            }
        }
    }(i)
}
```

Worker count guidelines:
- CPU-bound processing: match to CPU cores
- I/O-bound processing (DB, HTTP): 2-4x CPU cores
- Mixed: benchmark and adjust

## Storage Optimization

### File vs Memory Storage

| Aspect | FileStorage | MemoryStorage |
|--------|------------|---------------|
| Throughput | ~100k-500k msgs/s | ~500k-2M msgs/s |
| Latency | 0.1-1ms (SSD) | < 0.05ms |
| Durability | Survives restart | Lost on restart |
| Capacity | Limited by disk | Limited by RAM |

### Disk Performance

- **NVMe SSD**: Required for > 100k msgs/s sustained writes
- **Filesystem**: ext4 or xfs, mount with `noatime`
- **I/O Scheduler**: `none` for NVMe, `mq-deadline` for SATA SSD
- **Dedicated volume**: Never share JetStream storage with OS

### Message Size Impact

Smaller messages = higher throughput. If messages are large:

- Compress payloads client-side before publishing
- Use message references (publish URL/key, not full payload)
- Consider chunking for messages > 1MB

### Retention Policy Impact on Disk I/O

- `LimitsPolicy`: Periodic cleanup of expired messages
- `WorkQueuePolicy`: Immediate deletion on ack — highest write amplification
- `InterestPolicy`: Delete when all consumers ack — moderate write amplification

## Subject Cardinality

High subject cardinality (millions of unique subjects) impacts:
- Stream index size and memory usage
- Consumer filter matching performance
- `MaxMsgsPerSubject` tracking overhead

Guidelines:
- < 10k unique subjects per stream: no concerns
- 10k-100k: monitor memory usage, set appropriate limits
- > 100k: consider splitting into multiple streams or redesigning subject hierarchy

## OS and Network Tuning

### File Descriptors

```bash
# Check current limits
ulimit -n

# Set for NATS process (systemd)
[Service]
LimitNOFILE=1048576
```

Each connection uses 1 fd. Each stream uses additional fds for storage files.

### TCP Tuning

```bash
# /etc/sysctl.conf
net.core.rmem_max = 16777216
net.core.wmem_max = 16777216
net.ipv4.tcp_rmem = 4096 87380 16777216
net.ipv4.tcp_wmem = 4096 87380 16777216
net.core.somaxconn = 65535
net.ipv4.tcp_max_syn_backlog = 65535
```

### NATS Server Tuning

```conf
# nats-server.conf
max_connections: 64000
max_payload: 8MB
max_pending: 64MB

# Write deadline for slow clients
write_deadline: "10s"
```

## Benchmarking

### Built-in Benchmark

```bash
# Publish benchmark
nats bench orders.bench --pub 1 --msgs 1000000 --size 256

# Publish + Subscribe
nats bench orders.bench --pub 1 --sub 1 --msgs 1000000 --size 256

# JetStream publish
nats bench orders.bench --pub 1 --msgs 1000000 --size 256 --js

# JetStream with multiple publishers and subscribers
nats bench orders.bench --pub 4 --sub 4 --msgs 1000000 --size 256 --js
```

### Interpreting Results

```
Pub stats: 487,329 msgs/sec ~ 118.98 MB/sec
Sub stats: 487,329 msgs/sec ~ 118.98 MB/sec
```

Expected ranges on modern hardware (NVMe SSD, 10 Gbps):
- Core NATS: 1-10M msgs/sec
- JetStream R1 file: 100k-500k msgs/sec
- JetStream R3 file: 50k-200k msgs/sec
- JetStream R1 memory: 500k-2M msgs/sec
