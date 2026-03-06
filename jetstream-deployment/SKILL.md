---
name: jetstream-deployment
description: Use this skill when users ask about NATS JetStream including streams, consumers, event streaming, worker queues, or troubleshooting JetStream message delivery.
---

# Jetstream Deployment

This skill provides guidance for **Deploying and configuring NATS JetStream clusters including Kubernetes, clustering, and multi-region setups.**.

## When to Use

Activate this skill when users ask about:

- NATS JetStream streams
- consumer configuration
- event streaming architectures
- worker queues
- JetStream troubleshooting
- JetStream deployment

## Workflow

Step 1: Identify the messaging pattern (event streaming, queue, RPC).

Step 2: Determine appropriate JetStream subjects.

Step 3: Configure a stream.

Step 4: Select consumer type (push or pull).

Step 5: Configure delivery guarantees and retries.

Step 6: Provide example implementation code.

## Core Principles

- Prefer pull consumers for worker queues.
- Prefer push consumers for event listeners.
- Use replicas=3 for production reliability.
- Prefer fewer streams with multiple subjects.
