# Building a meta-agent in TypeScript: discover the fleet, fan out one prompt, collect every reply

What you're describing is the canonical **meta-agent / orchestrator** shape that the Synadia Agent Protocol for NATS is built for: you don't know the agents in advance, you discover whoever is on the bus *right now*, send them all the same prompt, and merge the streamed replies keyed by who said what so you can compare.

Below is the protocol mental model you need (so the code makes sense and stays correct as the SDK drifts), then complete, runnable TypeScript.

---

## How discovery and fan-out actually work

Two things make this tractable:

1. **Agents are ordinary NATS micro-services.** Every compliant agent registers under the *exact* service name `agents`. That single shared name is the discovery filter — you enumerate the fleet with standard NATS service discovery (`$SRV.PING.agents` / `$SRV.INFO.agents`), not a custom mechanism. The caller SDK wraps this as `discover()`.

2. **A reply is a stream of typed JSON chunks, not a single response.** Each chunk is `{ "type": ..., "data": ... }`. The types you care about:
   - `status` — the stream **always** opens with `{"type":"status","data":"ack"}`.
   - `response` — the actual answer text; it may arrive across many chunks (token streaming). `data` is either a string or `{ text, attachments }`.
   - `query` — the agent is pausing to ask *you* a question mid-stream (human-in-the-loop). Don't treat it as answer text.
   - **The stream ends with a zero-byte, header-less NATS message.** That terminator — not a lull, not the first pause — is how you know a given agent is done. The SDK surfaces this as the async iterator simply completing.

Three rules that keep a meta-agent correct:

- **Discovery first, never hardcode endpoints.** The population changes as agents start/stop. Let `discover()` hand you agent handles; don't build `agents.prompt.*` subjects yourself.
- **Ignore unknown chunk types and preserve unknown fields.** The 0.x protocol will add chunk types; a caller that throws on an unrecognized `type` is non-compliant. Branch only on the types you handle, drop the rest.
- **A branch is done only at the terminator.** Treat a missing terminator past a timeout as a *failed branch*, not a hang — discovery is a snapshot and an agent can die mid-fan-out.

For a "compare the responses" use case, the merge strategy is **collect-all**: keep every agent's full answer, keyed by identity (`agent/owner/name`).

> **Version note:** the protocol is spec **v0.3** and the npm SDKs (`@synadia-ai/agents` caller, `@synadia-ai/agent-service` host) are around **0.5.x** as of mid-2026 — the 0.x line is explicitly unstable. The **wire protocol above is the durable contract**; if a method name in this code differs from what's in your installed package, honor the protocol and adjust the call. Pin and verify your installed version.

---

## Install

```bash
npm i @synadia-ai/agents @nats-io/transport-node
```

`@synadia-ai/agents` is the **caller** SDK (you're building a caller/orchestrator, not hosting an agent, so you don't need `@synadia-ai/agent-service`). Requires Node ≥ 20 (or Bun ≥ 1.2) and a reachable NATS server.

---

## The meta-agent

This discovers the fleet, filters to the coding agents, fans the same prompt out concurrently, accumulates each streamed reply, and returns a comparison map. It tolerates offline/slow agents via a per-branch timeout and handles error-terminated streams.

```ts
// meta-agent.ts
import { connect, type NatsConnection } from "@nats-io/transport-node";
import { Agents } from "@synadia-ai/agents";

// Which harnesses count as "coding agents". Adjust to your fleet's `agent` tokens.
const CODING_AGENTS = ["cc", "claude-code", "opencode", "pi", "aider", "codex"];

// Per-agent cap so one stuck agent can't hang the whole fan-out.
const PER_AGENT_TIMEOUT_MS = 60_000;

interface AgentResult {
  key: string;              // agent/owner/name — stable identity for comparison
  agent: string;
  text: string;             // accumulated response
  ok: boolean;
  error?: string;           // populated on error-terminated / timed-out branches
  elapsedMs: number;
}

async function main() {
  const nc: NatsConnection = await connect({
    servers: process.env.NATS_URL ?? "nats://localhost:4222",
  });

  try {
    const agents = new Agents({ nc });

    // 1. DISCOVER — who is on the bus right now ($SRV.PING/INFO.agents under the hood).
    const all = await agents.discover();
    console.log(`Discovered ${all.length} agent(s) on the fabric.`);

    // 2. FILTER — target the coding harnesses. Filter on discovery metadata,
    //    never on hand-built subjects. You could also filter by owner, capability, etc.
    const targets = all.filter((a) => CODING_AGENTS.includes(a.agent));
    if (targets.length === 0) {
      console.log("No coding agents found. Live agents were:",
        all.map((a) => `${a.agent}/${a.owner}/${a.name}`).join(", ") || "(none)");
      return;
    }
    console.log(
      `Fanning out to ${targets.length} coding agent(s):`,
      targets.map((a) => `${a.agent}/${a.owner}/${a.name}`).join(", "),
    );

    const prompt =
      "In 3 sentences, explain the tradeoffs between optimistic and pessimistic locking.";

    // 3. FAN OUT — prompt every target concurrently, collect-all.
    const results = await fanOutCollectAll(nc, targets, prompt);

    // 4. COMPARE — every reply keyed by identity, side by side.
    for (const r of results) {
      console.log(`\n=== ${r.key} ${r.ok ? `(${r.elapsedMs}ms)` : `FAILED: ${r.error}`} ===`);
      console.log(r.ok ? r.text.trim() : "");
    }
  } finally {
    await nc.drain();
  }
}

/**
 * Send the same prompt to every target and accumulate each streamed reply.
 * Each branch is independent: one agent failing or timing out does not abort the others.
 */
async function fanOutCollectAll(
  nc: NatsConnection,
  targets: Awaited<ReturnType<Agents["discover"]>>,
  promptText: string,
): Promise<AgentResult[]> {
  return Promise.all(
    targets.map((agent) => promptOne(nc, agent, promptText)),
  );
}

async function promptOne(
  nc: NatsConnection,
  agent: Awaited<ReturnType<Agents["discover"]>>[number],
  promptText: string,
): Promise<AgentResult> {
  const key = `${agent.agent}/${agent.owner}/${agent.name}`;
  const started = Date.now();
  let text = "";

  try {
    // Race the stream consumption against a timeout so a stuck agent
    // (no terminator) becomes a failed branch, not a hang.
    await withTimeout(
      (async () => {
        for await (const msg of await agent.prompt(promptText)) {
          switch (msg.type) {
            case "response":
              // `data` is a string or { text }; the SDK normalizes to msg.text.
              text += msg.text;
              break;
            case "query":
              // The agent is asking a mid-stream question. For an unattended
              // meta-agent, apply a policy. Answer EXACTLY ONCE on reply_subject.
              nc.publish(
                msg.data.reply_subject,
                new TextEncoder().encode(autoAnswer(msg.data.prompt)),
              );
              break;
            // Any other chunk type (incl. `status` ack and future additions):
            // ignore it. Forward-compatibility is required by the protocol.
          }
        }
        // Loop completes ONLY on the zero-byte, header-less terminator — done.
      })(),
      PER_AGENT_TIMEOUT_MS,
    );

    return { key, agent: agent.agent, text, ok: true, elapsedMs: Date.now() - started };
  } catch (err) {
    // Error-terminated stream (e.g. 429 with retry_after_s) or timeout lands here.
    return {
      key,
      agent: agent.agent,
      text,
      ok: false,
      error: err instanceof Error ? err.message : String(err),
      elapsedMs: Date.now() - started,
    };
  }
}

// Unattended policy for mid-stream queries. Replace with a real human prompt
// (or a stricter default) if your coding agents ask destructive-action questions.
function autoAnswer(_question: string): string {
  return "no";
}

function withTimeout<T>(p: Promise<T>, ms: number): Promise<T> {
  return Promise.race([
    p,
    new Promise<T>((_, reject) =>
      setTimeout(() => reject(new Error(`timed out after ${ms}ms`)), ms).unref?.(),
    ),
  ]);
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
```

Run it:

```bash
NATS_URL=nats://localhost:4222 npx tsx meta-agent.ts
```

---

## Verify the fabric from the CLI first

Before running the orchestrator, confirm agents are actually registered and discoverable — this is exactly what `discover()` does under the hood:

```bash
# Enumerate every agent currently on the bus
nats req '$SRV.PING.agents' '' --replies 0

# Full service info, including each agent's real prompt-endpoint subject + metadata
nats req '$SRV.INFO.agents' ''
```

If `$SRV.PING.agents` returns nothing, no compliant agents are registered (check that each host registered under the service name `agents` exactly), and the meta-agent has nothing to fan out to.

---

## Notable design points

- **Filter on discovery metadata, not subjects.** I filtered by the `agent` token (the canonical harness id). You can target just as easily by `owner` (e.g. one team's agents) or by advertised capabilities. Discovery hands you `agent`/`owner`/`name` plus the real endpoint subject — address what it gives you; don't reconstruct `agents.prompt.*` yourself.

- **One instance per agent identity vs. every instance.** Because instances of the same agent share the `agents` queue group, `agent.prompt(...)` load-balances to *one* instance per agent identity — which is what you want for "compare different coding agents." If you instead wanted to hit every running *instance* of the same agent, you'd address them individually via `$SRV.INFO.agents.{instance_id}`.

- **Collect-all vs. other merges.** This returns every answer for comparison. For latency-sensitive work you'd switch to **first-wins/race** (take the fastest complete reply, cancel the rest) or **quorum** (wait for K of N — useful when some agents are slow or offline).

- **Liveness (optional, for long-running orchestrators).** `discover()` is a point-in-time snapshot. If your meta-agent is long-lived, subscribe to the heartbeat wildcard `agents.hb.*.*.*` and mark an agent offline after 3× its advertised `interval_s` with no beat, instead of re-discovering on every prompt:

  ```ts
  const liveness = new Map<string, { lastSeen: string; intervalS: number }>();
  const sub = nc.subscribe("agents.hb.*.*.*");
  (async () => {
    for await (const m of sub) {
      const hb = JSON.parse(new TextDecoder().decode(m.data));
      liveness.set(`${hb.agent}/${hb.owner}/${hb.name}`,
        { lastSeen: hb.ts, intervalS: hb.interval_s });
    }
  })();
  ```

  To bootstrap liveness for an agent you just discovered without waiting for its next beat, hit its `status` request/reply endpoint — it returns the same JSON shape as a heartbeat.

- **Multi-tenancy is free.** If you need to isolate teams/tenants, do it with **NATS accounts**, not application logic — agents in different accounts simply don't see each other on `$SRV.*`.

That's the whole loop: `discover()` → filter by metadata → `prompt()` each concurrently → accumulate `response` chunks until each stream's terminator → compare by identity.
