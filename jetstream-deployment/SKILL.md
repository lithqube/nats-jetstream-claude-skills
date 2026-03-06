---
name: jetstream-deployment
description: Use this skill when users ask about deploying NATS JetStream, configuring nats-server, Kubernetes NATS Helm charts, Docker Compose setups, clustering, multi-region gateways, leaf nodes, TLS, authentication, authorization, or infrastructure sizing for NATS.
---

# JetStream Deployment

Deploy and configure NATS JetStream clusters including server configuration, Kubernetes, Docker, clustering, security, and multi-region setups.

## When to Use

Activate this skill when users ask about:

- NATS server configuration (nats-server.conf)
- JetStream cluster setup and topology
- Kubernetes deployment (Helm charts, StatefulSets, operators)
- Docker and Docker Compose setups
- Multi-region / super-cluster with gateways
- Leaf node configuration
- TLS and mTLS setup
- Authentication (tokens, NKeys, JWT/accounts)
- Authorization and subject permissions
- Infrastructure sizing (CPU, memory, disk)
- High availability and fault tolerance

Do NOT activate for:
- Stream/consumer design or code examples (use jetstream-architecture)
- Troubleshooting, monitoring, or performance tuning (use jetstream-operations)

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
