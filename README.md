# NATS JetStream Claude Skills

Production-ready [Claude custom skills](https://docs.anthropic.com/en/docs/agents-and-tools/claude-code/custom-skills) for designing, deploying, and operating **NATS JetStream** messaging systems.

These skills give Claude deep knowledge of JetStream architecture patterns, production deployment configurations, and operational troubleshooting — enabling it to assist with real-world NATS projects.

## Skills

### jetstream-architecture

Design JetStream streams, subjects, and consumers. Covers:

- Stream configuration (retention policies, storage types, limits, discard policies)
- Consumer design (pull vs push, ack policies, deliver policies, backoff)
- Subject namespace design: wildcards, the `>` vs `*` capture pitfall, and subject-as-contract conventions (verb tense, no topology/version in subjects)
- Messaging patterns: fanout, work queues, priority queues, and dead-letter handling via advisory capture — NATS has **no** built-in DLQ (`MaxDeliver` is retry-then-stop)
- Server feature-gating by `api.level` and the version support policy (per-message TTL, priority groups, atomic batches, message scheduling, 2.12 strict mode)
- Code examples in **Go** (`nats.go`), **JavaScript/TypeScript** (modular `@nats-io/*` v3), and **Python** (`nats-py`) with error handling
- Idempotent publishing and the honest exactly-once: at-least-once delivery + an inbox/dedup table

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

### nats-agent-fabric

Build AI agents that discover and prompt each other over NATS using the **Synadia Agent Protocol** and **Synadia Agents SDK**. Covers:

- The wire protocol (spec v0.3): `agents.*` subject hierarchy, the four verbs, service registration, request envelope, response chunk streaming, stream termination, heartbeats, `$SRV` discovery, errors
- Host SDK (serve an agent) and caller SDK (discover/orchestrate) — `@synadia-ai/agents` / `@synadia-ai/agent-service` (TS) and `synadia-ai-agents` / `synadia-ai-agent-service` (Python)
- Meta-agent pattern: discover → fan-out → merge, liveness tracking, human-in-the-loop mid-stream queries
- Protocol-compliant agents in **Go** (no SDK yet) over `nats.go` micro — the template for any language without an SDK
- Durable agent memory and handoff with JetStream + KV
- Examples in **TypeScript**, **Python**, and **Go (protocol-level)**

> Built on the NATS Services API (micro), not core JetStream — JetStream/KV are the optional durable-state layer behind the fabric. Cross-referenced from all three JetStream skills.

### nuxt-nats (moved)

The skill for the **`nuxt-nats` Nuxt 4 / Nitro module** now lives with the module, so it is versioned with the API it describes and checked against the source in CI: [`skills/nuxt-nats`](https://github.com/lithqube/nuxt-nats/tree/main/skills/nuxt-nats). Install it with:

```bash
npx giget@latest gh:lithqube/nuxt-nats/skills/nuxt-nats .claude/skills/nuxt-nats
```

The earlier copy here taught several signatures that did not match the module (for example `handler(data, msg)`, and that a returning handler acks), so it was removed rather than updated.

## Installation

Copy the skill directories into your project's `.claude/skills/` directory:

```bash
# Project-level (recommended)
mkdir -p .claude/skills
cp -r jetstream-architecture .claude/skills/
cp -r jetstream-deployment .claude/skills/
cp -r jetstream-operations .claude/skills/
cp -r nats-agent-fabric .claude/skills/
```

## File Structure

```
jetstream-architecture/
  SKILL.md              # Skill definition and triggers
  concepts/
    streams.md          # Stream config, subject-design-as-contract, > vs * capture
    consumers.md        # Consumer types and configuration
    server-features.md  # Feature gating by api.level, version support policy, strict mode
  patterns/
    fanout.md           # Fanout pattern with examples
    work-queue.md       # Work queue, advisory-capture DLQ, exactly-once inbox table
  examples/
    go.md               # Go examples (nats.go)
    javascript.md       # JS/TS examples (modular @nats-io/* v3 client)
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

nats-agent-fabric/
  SKILL.md              # Skill definition, triggers, workflow, principles
  concepts/
    protocol.md         # Synadia Agent Protocol wire contract (v0.3)
    architecture.md     # Host vs caller SDK, meta-agent shape, where JS/KV fit
  patterns/
    meta-agent.md       # Discover → fan-out → merge, liveness, human-in-the-loop
    durable-state.md    # JetStream + KV for agent memory and handoff
  examples/
    typescript.md       # @synadia-ai/agents + @synadia-ai/agent-service
    python.md           # synadia-ai-agents + synadia-ai-agent-service
    protocol-go.md      # Protocol-compliant agent in Go over nats.go micro

```

## Evaluations

Each skill is benchmarked with the skill-creator eval loop: realistic test prompts run **with the skill vs. a no-skill baseline**, graded against objective assertions. Prompts live in `evals/evals.json` (and per-skill `*/evals/evals.json`); graded runs, timing, and `benchmark.json`/`benchmark.md` land in the matching `*-workspace/` directory.

Recent results (with skill → baseline pass rate): `nats-agent-fabric` 100% → 43% — baselines typically reach for the legacy `nats` package or hand-roll the raw client and miss the module/protocol specifics the skills encode.

## License

MIT
