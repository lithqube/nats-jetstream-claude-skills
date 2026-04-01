---
name: jetstream-operations
description: Use this skill whenever users are operating, troubleshooting, or monitoring a running NATS JetStream system — including diagnosing consumer lag, messages not delivering, stream-full errors, performance tuning, Prometheus metrics, Grafana dashboards, alerting rules, JetStream advisory subjects, nats CLI usage, cluster health and leader election issues, or client connection/reconnection problems. Use this skill when something is broken or slow, or when the user wants to observe their system. Do NOT use for designing new streams/consumers (use jetstream-architecture) or deploying/configuring infrastructure (use jetstream-deployment).
---

# JetStream Operations

Troubleshoot, monitor, and tune NATS JetStream including consumer lag, delivery failures, performance optimization, and observability.

For designing new streams or consumers, defer to the `jetstream-architecture` skill.
For deploying or configuring NATS infrastructure, defer to the `jetstream-deployment` skill.

## Reference Files

Read these files when they're relevant — don't load all of them upfront:

- `operations/troubleshooting.md` — step-by-step diagnosis for messages not delivering, consumer lag, stream-full errors, cluster split-brain, and client disconnections. Read whenever something is broken.
- `operations/performance.md` — publish throughput tuning, fetch batch sizing, MaxAckPending, parallel workers, FileStorage vs MemoryStorage, OS/TCP tuning, built-in benchmark commands. Read for performance or throughput questions.
- `operations/monitoring.md` — NATS HTTP endpoints, Prometheus metrics, nats-exporter setup, advisory subjects, Grafana dashboard recommendations, Prometheus alert rules. Read for observability and alerting questions.
- `operations/cli-reference.md` — full nats CLI reference for streams, consumers, pub/sub, server commands, and diagnostic workflows. Read when the user needs specific CLI commands.

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
