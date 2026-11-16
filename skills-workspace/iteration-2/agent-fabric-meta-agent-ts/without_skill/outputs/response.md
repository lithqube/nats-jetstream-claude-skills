# Meta-agent over the Synadia agent fabric

A meta-agent that **discovers** every AI agent registered on NATS, **fans** one
prompt out to all of them in parallel, and **merges** their replies.

## Files
- `meta-agent.ts` — discovery + fan-out + merge, plus a `main()` runner.
- `meta-agent.test.ts` — assert-based self-check for the pure `mergeReplies` step.

## Flow
1. **Connect** to NATS (`@nats-io/transport-node`).
2. **Discover** — `AgentClient.discover()` queries the `agents.*` fabric and
   returns the agents currently advertising themselves. Each registered agent
   is a NATS service, so discovery is a request/reply over that namespace; no
   static list is hard-coded.
3. **Fan-out** — one `client.prompt(id, prompt)` per agent, all in flight at
   once via `Promise.all`. Each call is wrapped in try/catch and has a per-agent
   timeout, so one slow or dead agent can't stall or sink the whole batch.
4. **Merge** — successful replies are rolled up into one Markdown block;
   failures are kept in the structured `replies[]` with their error, but left
   out of the roll-up and the `answered` count.

## Run (nothing here needs to; for reference)
```
NATS_URL=nats://localhost:4222 tsx meta-agent.ts "Summarize NATS in one sentence."
tsx meta-agent.test.ts   # -> ok
```

## Assumptions
Written from general knowledge of the Synadia Agents SDK, not from its docs. The
shape used — `new AgentClient(nc)`, `.discover()` returning `AgentInfo[]`, and
`.prompt(id, text, { timeout })` returning `{ text }` — is the expected surface;
exact export names / method signatures in `@synadia-ai/agents` may differ and
should be checked against the installed version.

---
Skipped: streaming replies, heartbeat/liveness filtering of discovered agents,
and retries — not needed to show discovery + fan-out + merge.
Risk: the SDK's real API names may not match the above, so this is a shape to
adapt, not drop-in code; `discover()` is a point-in-time snapshot, so agents
that register a moment later are missed.
