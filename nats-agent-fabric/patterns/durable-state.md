# Pattern: Durable Agent State & Handoff (JetStream + KV)

The Synadia Agent Protocol transport (v0.3) is **stateless** — micro request/reply over the `agents.>` subjects. Nothing in the core protocol persists a conversation. When agents need to **remember** across prompts, **resume** a session, or **hand off** work to another agent, you add a durable layer with NATS **JetStream** and **KV**.

This is roadmap-aligned: Synadia describes JetStream and KV as "available for future durable state and handoff." Today you wire it yourself; design it deliberately.

> For the mechanics of streams, consumers, KV buckets, and retention, use the **`jetstream-architecture`** skill — this file covers only the agent-specific shape. For sizing the servers, use **`jetstream-deployment`**.

## When you actually need it

Don't add state reflexively — stateless agents are simpler and the protocol is happy without it. Reach for durability when:

- An agent must recall earlier turns of the same `session` (conversational memory).
- A long task must survive an agent restart or move to another instance (resumability).
- One agent finishes part of a task and another picks it up (handoff).
- You want a replayable, auditable record of agent work beyond the live message tap.

## KV for agent memory

A KV bucket keyed by session is the simplest durable memory. Key by the identity tuple so memory is naturally partitioned per agent/owner/session.

```
bucket:  agent_memory
key:     {owner}.{agent}.{session}        # e.g. team-search.cc.synadia-com-2
value:   serialized conversation state / scratchpad
```

- KV is itself a JetStream stream under the hood, so you get history, revisions, and TTL.
- Use **optimistic concurrency** (update-with-revision) when two instances of the same agent might write the same session — the loser retries, avoiding lost updates.
- Set a **TTL** so abandoned sessions expire instead of accumulating.

In a host `onPrompt` handler: load the session's memory at the start, append the new turn, persist before (or as) you stream the reply.

## JetStream for handoff & durable tasks

For handoff and resumable work, model tasks as messages on a JetStream stream rather than KV values:

- **Subjects:** `agenttasks.{owner}.{stage}` — e.g. `agenttasks.team-search.research`, `…​.summarize`. A stage-shaped namespace lets each agent role consume its stage.
- **Retention:** `WorkQueuePolicy` when each task should be handled exactly once and then removed (classic handoff); `LimitsPolicy`/`InterestPolicy` when multiple roles observe the same task.
- **Consumers:** one durable pull consumer per agent role; `AckExplicit`; `MaxDeliver` + a dead-letter subject so a poisoned task doesn't loop forever.
- **Idempotency:** include a task id and use the publish dedup window (`Nats-Msg-Id`) so a retried handoff doesn't duplicate work.

Flow: agent A finishes its stage and publishes the next task to `agenttasks.{owner}.next-stage`; agent B's durable consumer picks it up, possibly on a different host or after a restart. The prompt/response fabric stays the live channel; JetStream carries the durable work items between agents.

## Capturing the fabric for audit/replay

Because every prompt and response is a NATS message, you can source the `agents.>` subjects into a JetStream stream to get a durable, replayable audit log of all agent traffic — without changing any agent. Use a `LimitsPolicy` stream with a retention window sized to your compliance needs. This is observability/governance, not part of an agent's own state.

## Design checklist

- Key memory by `{owner}.{agent}.{session}` so it's partitioned and discoverable.
- Choose KV (latest-state memory) vs. a JetStream stream (ordered task/event log, handoff) deliberately — they solve different problems.
- Set TTLs/retention; agent sessions are often short-lived and shouldn't accumulate forever.
- Make task processing idempotent — handoffs get retried.
- Keep the durable layer **optional and behind the stateless transport** — an agent that loses its store should still answer prompts (degraded, not down).
- Put each tenant's state in its **own account/streams** so isolation matches the fabric's account boundaries.
