---
name: jetstream-operations
description: Use this skill when users ask about troubleshooting NATS JetStream, diagnosing consumer lag, message delivery failures, performance tuning, monitoring, observability, Prometheus metrics, nats CLI commands, or operational issues with JetStream streams and consumers.
---

# JetStream Operations

Troubleshoot, monitor, and tune NATS JetStream including consumer lag, delivery failures, performance optimization, and observability.

## When to Use

Activate this skill when users ask about:

- JetStream troubleshooting (messages not delivering, consumer lag, stream full)
- Consumer lag diagnosis and resolution
- Message delivery failures and retries
- Performance tuning (throughput, latency)
- Monitoring and alerting (Prometheus, Grafana)
- NATS advisory subjects and events
- nats CLI commands for inspection and debugging
- Cluster health and leader election issues
- Client connection problems and reconnection
- Stream and consumer operational management (purge, edit, delete)

Do NOT activate for:
- Designing new streams or consumers (use jetstream-architecture)
- Deploying or configuring NATS infrastructure (use jetstream-deployment)

## Workflow

Step 1: Identify symptoms — what is the user observing? (lag, missing messages, errors, slow throughput)

Step 2: Inspect current state — use `nats` CLI to examine streams, consumers, and server status.

Step 3: Diagnose root cause — match symptoms to known patterns (ack timeout, filter mismatch, resource limits).

Step 4: Apply fix — provide specific configuration changes or code fixes.

Step 5: Verify resolution — confirm the fix with CLI commands and metrics.

Step 6: Set up monitoring — recommend metrics, alerts, and advisory subscriptions to prevent recurrence.

## Core Principles

- Always start diagnosis with `nats stream report` and `nats consumer report`
- Check `num_ack_pending` first when consumers appear stuck — it's the most common bottleneck
- Monitor JetStream advisory subjects (`$JS.EVENT.ADVISORY.>`) for real-time operational events
- Use `nats server report jetstream` to check cluster-wide JetStream health
- Set alerts on consumer pending count, not just publish rate
- Prefer `nats` CLI over raw API calls for operational tasks
- Always check the NATS server logs — they surface warnings before failures
- When in doubt, compare stream sequence numbers with consumer ack floor
