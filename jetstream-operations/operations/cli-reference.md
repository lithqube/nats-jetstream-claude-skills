# NATS CLI Reference

The `nats` CLI is the primary tool for managing and debugging JetStream. Install via:

```bash
# macOS
brew install nats-io/nats-tools/nats

# Linux
curl -sf https://binaries.nats.dev/nats-io/natscli/nats@latest | sh

# Docker (nats-box)
docker run --rm -it --network host natsio/nats-box
```

## Connection Context

```bash
# Set default server
nats context add local --server nats://localhost:4222 --select

# Switch between contexts
nats context select production

# List contexts
nats context ls

# One-off connection to different server
nats stream ls --server nats://prod-1:4222
```

## Stream Commands

```bash
# List all streams
nats stream ls

# Create a stream interactively
nats stream add

# Create a stream non-interactively
nats stream add ORDERS \
  --subjects "orders.>" \
  --retention limits \
  --storage file \
  --replicas 3 \
  --max-age 30d \
  --max-bytes 5G \
  --discard old \
  --dupe-window 2m

# Stream details
nats stream info ORDERS
nats stream info ORDERS --json | jq .

# Stream report (all streams overview)
nats stream report

# View messages in stream (caution: can be noisy)
nats stream view ORDERS --last 10

# Get specific message by sequence
nats stream get ORDERS 42

# Edit stream configuration
nats stream edit ORDERS --max-age 14d
nats stream edit ORDERS --max-bytes 10G

# Purge stream
nats stream purge ORDERS
nats stream purge ORDERS --subject "orders.cancelled"
nats stream purge ORDERS --seq 1000000  # keep messages from this seq onward

# Delete stream (destructive)
nats stream rm ORDERS

# Backup/restore
nats stream backup ORDERS /path/to/backup
nats stream restore ORDERS /path/to/backup
```

## Consumer Commands

```bash
# List consumers for a stream
nats consumer ls ORDERS

# Create consumer interactively
nats consumer add ORDERS

# Create consumer non-interactively
nats consumer add ORDERS order-processor \
  --ack explicit \
  --wait 30s \
  --max-deliver 5 \
  --max-pending 1000 \
  --filter "orders.>" \
  --deliver all \
  --pull

# Consumer details
nats consumer info ORDERS order-processor
nats consumer info ORDERS order-processor --json | jq .

# Consumer report
nats consumer report ORDERS

# Manually fetch next message (for debugging)
nats consumer next ORDERS order-processor
nats consumer next ORDERS order-processor --count 5

# Edit consumer
nats consumer edit ORDERS order-processor --max-pending 5000

# Delete consumer
nats consumer rm ORDERS order-processor
```

## Publish and Subscribe (Testing)

```bash
# Publish a message
nats pub orders.created '{"id":"test-123"}'

# Publish with headers
nats pub orders.created '{"id":"test-123"}' \
  --header "Nats-Msg-Id:test-123" \
  --header "Content-Type:application/json"

# Publish from file
nats pub orders.created --file /path/to/payload.json

# Subscribe to subjects
nats sub "orders.>"
nats sub "orders.>" --count 10  # stop after 10 messages

# Request-reply
nats request "orders.validate" '{"id":"test-123"}'

# JetStream subscribe (using consumer)
nats consumer next ORDERS order-processor --count 10
```

## Server Commands

```bash
# Server health check
nats server check connection
nats server check jetstream

# JetStream cluster report
nats server report jetstream

# Connection report
nats server report connections

# Server list (cluster)
nats server ls

# Server info
nats server info

# Ping all servers
nats server ping
```

## Account Commands

```bash
# Account information (JetStream limits and usage)
nats account info

# Output includes:
# - JetStream tier: memory, storage limits and usage
# - Stream count vs max
# - Consumer count vs max
```

## Common Diagnostic Workflows

### "Why is my consumer not receiving messages?"

```bash
# 1. Check stream has messages
nats stream info ORDERS

# 2. Check consumer state
nats consumer info ORDERS order-processor

# 3. Look at:
#    - Num Pending (messages waiting) — if 0, nothing to deliver
#    - Num Ack Pending (in-flight) — if == MaxAckPending, consumer is blocked
#    - Filter Subject — does it match published subjects?

# 4. Try manually fetching
nats consumer next ORDERS order-processor
```

### "Why is my consumer falling behind?"

```bash
# 1. Overview of all consumers
nats consumer report ORDERS

# 2. Check pending growth rate
watch -n5 'nats consumer report ORDERS'

# 3. If Num Ack Pending is at limit, increase it
nats consumer edit ORDERS order-processor --max-pending 5000

# 4. If processing is slow, add more workers
# (scale the application, not NATS config)
```

### "Is my cluster healthy?"

```bash
# 1. JetStream cluster status
nats server report jetstream

# 2. Check all streams have leaders
nats stream report

# 3. Check routes
nats server report connections

# 4. HTTP endpoints
curl http://localhost:8222/healthz
curl http://localhost:8222/routez | jq '.routes | length'
curl http://localhost:8222/jsz | jq '{streams: .streams, consumers: .consumers, memory: .memory, storage: .storage}'
```

### "What's consuming the most resources?"

```bash
# Storage by stream
nats stream report

# Connections by account
nats server report connections --sort msgs_from

# JetStream resource usage
nats account info
```
