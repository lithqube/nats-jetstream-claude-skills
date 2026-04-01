---
name: jetstream-deployment
description: Use this skill whenever users need to deploy, configure, or secure a NATS JetStream server or cluster — including nats-server.conf configuration, Kubernetes Helm charts, StatefulSets, Docker Compose, multi-region super-clusters with gateways, leaf nodes, TLS/mTLS setup, NKey/JWT authentication, subject-level authorization, or infrastructure sizing. Use this skill even for general "how do I run NATS in production?" questions. Do NOT use for stream/consumer design or code examples (use jetstream-architecture) or troubleshooting/monitoring (use jetstream-operations).
---

# JetStream Deployment

Deploy and configure NATS JetStream clusters including server configuration, Kubernetes, Docker, clustering, security, and multi-region setups.

For stream/consumer design or application code, defer to the `jetstream-architecture` skill.
For troubleshooting, monitoring, or performance tuning, defer to the `jetstream-operations` skill.

## Reference Files

Read these files when they're relevant — don't load all of them upfront:

- `deployment/cluster.md` — single-node dev config, 3-node production cluster, sizing guidelines, multi-region super-clusters with gateways, leaf nodes for edge deployments. Read for any clustering or topology question.
- `deployment/docker.md` — Docker single-node and 3-node cluster Docker Compose configs with health checks. Read when the user wants Docker or local development setups.
- `deployment/kubernetes.md` — Helm chart values, StatefulSet config, PVCs, pod anti-affinity, health probes, NetworkPolicy, Prometheus setup. Read for any Kubernetes deployment question.
- `deployment/security.md` — TLS/mTLS config, NKey auth, JWT/accounts with nsc, subject-level permissions, multi-tenant account isolation. Read whenever security, auth, or TLS comes up.

## Workflow

Step 1: Determine deployment target — Kubernetes, Docker, bare metal, or cloud managed.

Step 2: Size the cluster — number of nodes, CPU, memory, disk based on throughput and retention needs.

Step 3: Configure nats-server — JetStream storage, cluster routes, and domain settings.

Step 4: Set up infrastructure — Helm chart, Docker Compose, or systemd units.

Step 5: Configure security — TLS certificates, authentication method, subject-level authorization.

Step 6: Validate deployment — health checks, cluster connectivity, JetStream readiness.

## Core Principles

- Minimum 3 nodes for production JetStream clusters (odd number for leader election)
- Use dedicated storage volumes for JetStream data — never share with OS or logs
- Set explicit resource limits (max_mem, max_file) per server for JetStream
- Always enable TLS for production — at minimum server TLS, ideally mTLS
- Use NKey or JWT-based auth in production — avoid plain tokens
- Configure liveness and readiness probes — NATS supports health monitoring on port 8222
- Set `connect_retries` in cluster routes for resilient bootstrapping
- Use gateways for multi-region — not cluster routes across WAN
- Pin NATS server versions — don't use `latest` tags in production
