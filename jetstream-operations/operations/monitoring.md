# Monitoring and Observability

## NATS Monitoring Endpoints

NATS server exposes HTTP monitoring on port 8222 (configurable via `http: port`):

| Endpoint | Description |
|----------|-------------|
| `/healthz` | Health check (200 if healthy) |
| `/healthz?js-enabled-only=true` | Health check requiring JetStream |
| `/varz` | Server stats (connections, memory, CPU) |
| `/jsz` | JetStream stats (streams, consumers, storage) |
| `/connz` | Client connection details |
| `/routez` | Cluster route information |
| `/gatewayz` | Gateway connections |
| `/leafz` | Leaf node connections |
| `/subsz` | Subscription details |
| `/accountz` | Account information |

```bash
# Quick health check
curl http://localhost:8222/healthz

# JetStream overview
curl http://localhost:8222/jsz | jq .

# Detailed JetStream with streams and consumers
curl "http://localhost:8222/jsz?streams=true&consumers=true&config=true" | jq .
```

## Prometheus Metrics

### Setup with nats-exporter

The NATS Prometheus exporter (`prometheus-nats-exporter`) scrapes monitoring endpoints and exposes Prometheus metrics.

```bash
# Run alongside NATS server
prometheus-nats-exporter -varz -jsz -connz -port 7777 http://localhost:8222
```

Or use the Docker sidecar:

```yaml
services:
  nats-exporter:
    image: natsio/prometheus-nats-exporter:latest
    command: ["-varz", "-jsz", "-connz", "-port", "7777", "http://nats:8222"]
    ports:
      - "7777:7777"
```

### Key Metrics

**Server Health**

| Metric | Meaning | Alert When |
|--------|---------|------------|
| `gnatsd_varz_connections` | Active client connections | > 80% of max_connections |
| `gnatsd_varz_mem` | Server memory usage (bytes) | > 80% of allocated |
| `gnatsd_varz_cpu` | CPU usage percentage | Sustained > 80% |
| `gnatsd_varz_slow_consumers` | Cumulative slow consumer count | Increasing |

**JetStream Health**

| Metric | Meaning | Alert When |
|--------|---------|------------|
| `gnatsd_varz_jetstream_stats_memory` | JetStream memory used | > 80% of max_mem |
| `gnatsd_varz_jetstream_stats_storage` | JetStream disk used | > 80% of max_file |
| `gnatsd_varz_jetstream_stats_streams` | Total stream count | Unexpected change |
| `gnatsd_varz_jetstream_stats_consumers` | Total consumer count | Unexpected change |

**Per-Stream (from /jsz)**

| Metric | Meaning | Alert When |
|--------|---------|------------|
| `stream_msgs` | Messages in stream | Growing unbounded |
| `stream_bytes` | Bytes in stream | > 80% of MaxBytes |
| `consumer_num_pending` | Undelivered messages | Growing (consumer lag) |
| `consumer_num_ack_pending` | Delivered but unacked | Equals MaxAckPending |
| `consumer_num_redelivered` | Redelivery count | High rate = processing failures |

## JetStream Advisory Subjects

NATS publishes real-time events on advisory subjects. Subscribe to these for operational alerting.

### Advisory Subject Hierarchy

```
$JS.EVENT.ADVISORY.STREAM.CREATED.{stream}
$JS.EVENT.ADVISORY.STREAM.DELETED.{stream}
$JS.EVENT.ADVISORY.STREAM.UPDATED.{stream}
$JS.EVENT.ADVISORY.STREAM.SNAPSHOT_CREATE.{stream}
$JS.EVENT.ADVISORY.STREAM.SNAPSHOT_COMPLETE.{stream}
$JS.EVENT.ADVISORY.STREAM.LEADER_ELECTED.{stream}
$JS.EVENT.ADVISORY.STREAM.QUORUM_LOST.{stream}

$JS.EVENT.ADVISORY.CONSUMER.CREATED.{stream}.{consumer}
$JS.EVENT.ADVISORY.CONSUMER.DELETED.{stream}.{consumer}
$JS.EVENT.ADVISORY.CONSUMER.MAX_DELIVERIES.{stream}.{consumer}

$JS.EVENT.ADVISORY.API                    # all API calls
```

### Critical Advisories to Monitor

```bash
# Subscribe to all advisories
nats sub '$JS.EVENT.ADVISORY.>'

# Max deliveries exhausted (dead letters)
nats sub '$JS.EVENT.ADVISORY.CONSUMER.MAX_DELIVERIES.>'

# Quorum lost (cluster issue)
nats sub '$JS.EVENT.ADVISORY.STREAM.QUORUM_LOST.>'

# Leader changes (may indicate instability)
nats sub '$JS.EVENT.ADVISORY.STREAM.LEADER_ELECTED.>'
```

### Programmatic Advisory Handling

```go
// Monitor max delivery failures (dead letter events)
nc.Subscribe("$JS.EVENT.ADVISORY.CONSUMER.MAX_DELIVERIES.>", func(msg *nats.Msg) {
    var advisory struct {
        Stream   string `json:"stream"`
        Consumer string `json:"consumer"`
        StreamSeq uint64 `json:"stream_seq"`
    }
    json.Unmarshal(msg.Data, &advisory)

    log.Printf("ALERT: message %d in %s exceeded max deliveries for %s",
        advisory.StreamSeq, advisory.Stream, advisory.Consumer)

    // Send to alerting system (PagerDuty, Slack, etc.)
    alertMaxDeliveryExceeded(advisory)
})

// Monitor quorum loss
nc.Subscribe("$JS.EVENT.ADVISORY.STREAM.QUORUM_LOST.>", func(msg *nats.Msg) {
    log.Printf("CRITICAL: Stream quorum lost: %s", string(msg.Data))
    alertQuorumLost(msg.Data)
})
```

## Grafana Dashboard

### Recommended Panels

**Overview Row:**
- Total connections (gauge)
- Total streams (gauge)
- Total consumers (gauge)
- JetStream memory usage % (gauge)
- JetStream storage usage % (gauge)

**Throughput Row:**
- Messages published/sec (graph, rate)
- Messages consumed/sec (graph, rate)
- Bytes in/out per second (graph, rate)

**Consumer Health Row:**
- Consumer pending by stream (stacked graph)
- Consumer ack pending by stream (stacked graph)
- Redelivery rate (graph)
- Max deliveries exceeded events (counter)

**Cluster Row:**
- Leader elections over time (graph)
- Route connections status (table)
- Peer lag (graph)

## nats CLI Monitoring Commands

```bash
# Real-time stream report (refreshes every second)
watch -n1 'nats stream report'

# Consumer report for specific stream
watch -n1 'nats consumer report ORDERS'

# Server-wide JetStream report
nats server report jetstream

# Connection report
nats server report connections

# Account usage
nats account info
```

## Alerting Rules (Prometheus)

```yaml
groups:
  - name: nats-jetstream
    rules:
      - alert: NATSJetStreamStorageHigh
        expr: gnatsd_varz_jetstream_stats_storage / gnatsd_varz_jetstream_config_max_storage > 0.8
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: "JetStream storage above 80%"

      - alert: NATSJetStreamMemoryHigh
        expr: gnatsd_varz_jetstream_stats_memory / gnatsd_varz_jetstream_config_max_memory > 0.8
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: "JetStream memory above 80%"

      - alert: NATSConnectionsHigh
        expr: gnatsd_varz_connections / gnatsd_varz_max_connections > 0.8
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: "NATS connections above 80% of max"

      - alert: NATSSlowConsumers
        expr: rate(gnatsd_varz_slow_consumers[5m]) > 0
        for: 2m
        labels:
          severity: critical
        annotations:
          summary: "Slow consumers detected"
```
