# NATS JetStream Claude Skills

Production-ready [Claude custom skills](https://docs.anthropic.com/en/docs/agents-and-tools/claude-code/custom-skills) for designing, deploying, and operating **NATS JetStream** messaging systems.

These skills give Claude deep knowledge of JetStream architecture patterns, production deployment configurations, and operational troubleshooting — enabling it to assist with real-world NATS projects.

## Skills

### jetstream-architecture

Design JetStream streams, subjects, and consumers. Covers:

- Stream configuration (retention policies, storage types, limits, discard policies)
- Consumer design (pull vs push, ack policies, deliver policies, backoff)
- Subject namespace design with wildcards
- Messaging patterns: fanout, work queues, priority queues, dead letter queues
- Code examples in **Go**, **JavaScript**, and **Python** with error handling
- Idempotent publishing and exactly-once processing strategies

### jetstream-deployment

Deploy and configure NATS JetStream infrastructure. Covers:

- nats-server.conf for single-node and 3-node clusters
- Kubernetes deployment with Helm charts, StatefulSets, PVCs, and NetworkPolicies
- Docker Compose for development and local cluster testing
- Multi-region super-clusters with gateways
- Leaf nodes for edge deployments
- TLS/mTLS, NKey auth, JWT/accounts, and subject-level authorization
- Cluster sizing guidelines

### jetstream-operations

Troubleshoot, monitor, and tune JetStream. Covers:

- Diagnosing message delivery failures, consumer lag, and stream capacity issues
- Performance tuning (publish batching, fetch sizing, MaxAckPending, OS tuning)
- Prometheus metrics, Grafana dashboards, and alerting rules
- JetStream advisory subjects for real-time operational events
- nats CLI reference with common diagnostic workflows
- Cluster health checks and leader election troubleshooting

## Installation

Copy the skill directories into your project's `.claude/skills/` directory:

```bash
# Project-level (recommended)
mkdir -p .claude/skills
cp -r jetstream-architecture .claude/skills/
cp -r jetstream-deployment .claude/skills/
cp -r jetstream-operations .claude/skills/
```

## File Structure

```
jetstream-architecture/
  SKILL.md              # Skill definition and triggers
  concepts/
    streams.md          # Stream configuration reference
    consumers.md        # Consumer types and configuration
  patterns/
    fanout.md           # Fanout pattern with examples
    work-queue.md       # Work queue with DLQ and idempotency
  examples/
    go.md               # Go examples (nats.go)
    javascript.md       # JavaScript examples (nats.js)
    python.md           # Python examples (nats-py)

jetstream-deployment/
  SKILL.md
  deployment/
    cluster.md          # Server config, clustering, multi-region, leaf nodes
    kubernetes.md       # Helm charts, StatefulSets, health checks, NetworkPolicy
    docker.md           # Docker Compose for dev and local clusters
    security.md         # TLS, authentication, authorization

jetstream-operations/
  SKILL.md
  operations/
    troubleshooting.md  # Common issues and diagnosis
    performance.md      # Throughput tuning and benchmarking
    monitoring.md       # Prometheus, Grafana, advisory subjects, alerting
    cli-reference.md    # nats CLI commands and diagnostic workflows
```

## License

MIT
