---
name: jetstream-architecture
description: Use this skill whenever users are designing, modeling, or writing code for NATS JetStream — including stream configuration, subject namespace design, consumer types (pull vs push), ack policies, retention policies, delivery guarantees, messaging patterns (fanout, work queue, request-reply), idempotent publishing, exactly-once semantics, or JetStream code examples in Go, JavaScript, or Python. Use this skill even when the user doesn't say "JetStream" explicitly — if they're asking how to build a message queue, event stream, or worker system on NATS, this skill applies. Do NOT use for deployment/clustering/Kubernetes questions (use jetstream-deployment) or troubleshooting/monitoring (use jetstream-operations).
---

# JetStream Architecture

Design NATS JetStream streams, subjects, and consumers for event streaming and worker queue architectures.

For deployment/clustering/Kubernetes questions, defer to the `jetstream-deployment` skill.
For troubleshooting, monitoring, or performance tuning, defer to the `jetstream-operations` skill.

## Reference Files

Read these files when they're relevant to the user's question — don't load all of them upfront, just the ones you need:

- `concepts/streams.md` — stream config fields, retention policies, storage types, subject namespaces, mirroring/sourcing. Read when configuring a stream.
- `concepts/consumers.md` — pull vs push comparison, ack policies, deliver policies, consumer groups, ordered consumers, backoff. Read when designing consumers.
- `patterns/fanout.md` — fanout pattern with multiple independent consumers, LimitsPolicy vs InterestPolicy, scaling. Read when the user needs multiple services consuming the same events.
- `patterns/work-queue.md` — work queue with competing consumers, DLQ, deduplication, priority queues. Read when the user needs task distribution or job processing.
- `examples/go.md` — complete Go examples using nats.go. Read when the user wants Go code.
- `examples/python.md` — complete Python examples using nats-py. Read when the user wants Python code.
- `examples/javascript.md` — complete JavaScript/Node.js examples using nats.js. Read when the user wants JavaScript or TypeScript code.

## Workflow

Step 1: Gather requirements — what data is being produced, who consumes it, what are the durability and ordering needs.

Step 2: Design the subject namespace — use hierarchical subjects with wildcards for flexible routing.

Step 3: Configure the stream — choose retention policy, storage type, replicas, limits, and discard policy.

Step 4: Select consumer strategy — pull for worker queues, push for event listeners, ordered for replay.

Step 5: Configure delivery guarantees — ack policy, max deliver, ack wait, backoff, dead letter handling.

Step 6: Provide implementation code — working examples with proper error handling in the user's language.

## Core Principles

- Use hierarchical subjects: `{domain}.{entity}.{event}` (e.g., `orders.us-east.created`)
- Prefer one stream per bounded context with multiple subjects over many single-subject streams
- Use pull consumers for worker queues — they allow backpressure and batch processing
- Use push consumers for real-time event listeners that need immediate delivery
- Always use `AckExplicit` in production — never rely on implicit acks
- Set `MaxDeliver` with a dead letter strategy — don't retry forever
- Use `DuplicateWindow` for publish-side deduplication (default 2 minutes)
- Set `Replicas: 3` for production streams — R1 is only for development
- Prefer `FileStorage` for durability — use `MemoryStorage` only for ephemeral/cache streams
- Design for idempotent consumers — messages may be redelivered after ack timeout
