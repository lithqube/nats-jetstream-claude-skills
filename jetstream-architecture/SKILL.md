---
name: jetstream-architecture
description: Use this skill when users ask about designing NATS JetStream streams, subject hierarchies, consumer patterns, delivery guarantees, retention policies, or JetStream data modeling. Activate for questions about stream configuration, consumer types (pull vs push), ack policies, work queue vs fanout patterns, or JetStream code examples in Go, JavaScript, or Python.
---

# JetStream Architecture

Design NATS JetStream streams, subjects, and consumers for event streaming and worker queue architectures.

## When to Use

Activate this skill when users ask about:

- Designing JetStream stream configurations
- Subject namespace and hierarchy design
- Choosing between pull and push consumers
- Consumer configuration (ack policies, deliver policies, replay)
- Retention policies (limits, interest, work queue)
- Messaging patterns (fanout, work queue, request-reply)
- Delivery guarantees (at-most-once, at-least-once, exactly-once)
- JetStream code examples in Go, JavaScript, or Python
- Stream mirroring and sourcing
- Idempotent message processing

Do NOT activate for:
- Deployment, clustering, or Kubernetes questions (use jetstream-deployment)
- Troubleshooting, monitoring, or performance tuning (use jetstream-operations)

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
