# Agent Fabric Architecture

How the pieces fit together, so you can decide what to build before writing code.

## Two sides: host SDK and caller SDK

The SDK is split by role. Most projects use exactly one side; orchestrators that are themselves agents use both.

| Side             | You're building…                                  | TS package                 | Python package                 |
|------------------|---------------------------------------------------|----------------------------|--------------------------------|
| **Host / agent** | an agent that registers and answers prompts       | `@synadia-ai/agent-service`| `synadia-ai-agent-service`     |
| **Caller / client** | a meta-agent/app that discovers and prompts agents | `@synadia-ai/agents`     | `synadia-ai-agents`            |

- **Host SDK** embeds an `AgentService` in your harness: you register as service name `agents`, supply an `onPrompt` handler, and the SDK handles micro registration, the `ack` chunk, the queue group, heartbeats, and the stream terminator.
- **Caller SDK** gives you an `Agents` client: `discover()` the fleet, bind to an agent, `prompt()` it, and iterate typed chunks. It also tracks liveness from heartbeats and pre-flight-validates payloads.

Mental model: **an agent is a function from a prompt to a streamed reply.** The SDK is the plumbing around that function; only the body differs between an echo agent, an LLM agent, and a full coding harness.

## Meta-agent vs worker

The protocol is shaped for *many processes, many agents, none known to the caller in advance* — the opposite of "one process, one known endpoint."

- A **worker agent** does the work (runs an LLM, a tool loop, a coding harness). It hosts.
- A **meta-agent** coordinates other agents: discovers them, fans a prompt out, merges responses, tracks who's alive. It calls — and, if it also answers prompts itself, it hosts too.

This is **A2A-style coordination**. It is *not* MCP: MCP gives one agent its tools/context; this gives a bus full of agents a way to find and talk to each other. The two compose — an MCP-equipped agent can be exposed on the fabric.

## Pre-built plugins (zero-code hosting)

The `agents/` directory of the SDK repo ships thin shims that expose existing harnesses on the fabric with no code: `claude-code` (token `cc`), `opencode`, `pi`, `hermes`, `deerflow` (`df`), `flue`, `openclaw` (`oc`), `open-agent`. If the user just wants to put Claude Code or another supported harness on the bus, point them at the matching plugin rather than writing a host from scratch.

## Load-balancing & scaling

Because the `prompt` endpoint registers on the NATS queue group `agents`, running N instances of the same agent (same `agent`/`owner`, different `name`/`instance_id`) automatically load-balances prompts across them — no router, no config. This is plain NATS queue-group semantics; scale out by starting more instances.

To address a *specific* instance rather than any-of-N, use the instance-scoped discovery subject `$SRV.INFO.agents.{instance_id}` and the endpoint subject it returns.

## Identity & multi-tenancy

Identity is the tuple `agent` / `owner` / `name` (+ `session`). It composes both the subject namespace and the discovery metadata, so choose stable, lowercase tokens early.

Multi-tenancy and isolation come from **NATS accounts**, inherited from the bus — not from application code. Put different tenants/owners in different accounts and the fabric enforces separation, including which agents can even see each other in discovery. For how to configure accounts, TLS, and authn/authz on the servers, defer to the `jetstream-deployment` skill.

Every prompt and response is a NATS message, so the fabric gives you a natural **audit trail** — tap or persist the `agents.>` subjects (a good JetStream capture point).

## Where JetStream and KV fit

The transport is the NATS **Services API (micro)** — stateless request/reply + queue groups. Core JetStream is **not** required for v0.3.

JetStream and KV are the **roadmap layer for durable state and session handoff**: persistent agent memory, resumable sessions, handing a task from one agent to another. They're "available for future durable state," not part of the stateless transport contract. When agents need to *remember* or *hand off*, see `patterns/durable-state.md`, and design the underlying streams/buckets with the `jetstream-architecture` skill.

## What ships today vs. what's coming

- **Today:** connectivity, identity, discovery, streaming, heartbeats, audit trail; TS + Python SDKs; protocol v0.3.
- **Coming:** durable state/handoff (JetStream + KV), richer agent-to-agent capability negotiation, zero-trust security (class/instance identity, dynamic permissions). **Go SDK is planned.**

Treat anything beyond the v0.3 transport as subject to change, and pin package versions.
