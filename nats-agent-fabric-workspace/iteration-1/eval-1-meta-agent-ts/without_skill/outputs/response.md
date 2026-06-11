# Building a Meta-Agent on NATS: Discover Agents and Fan Out a Prompt

Great use case. A "meta-agent" that discovers peers and broadcasts a prompt to all of them is a classic **service discovery + scatter/gather** pattern. NATS is a perfect fit for it because it gives you request/reply, wildcards, and the new **micro service** primitives that Synadia's agents SDK builds on top of.

Below I'll walk through:

1. The architecture (how discovery and fan-out work on NATS)
2. A discovery + fan-out implementation using the NATS micro framework (which the Synadia agents SDK is built on)
3. A version using the Synadia agents SDK's higher-level client
4. Production hardening notes

> A quick, honest caveat: Synadia's agent SDK is brand new and the exact class/method names move around between releases. I'll show you the canonical patterns and the underlying NATS primitives that the SDK wraps. If a symbol name differs in your installed version, the NATS-core version (Approach A) will always work, because the SDK ultimately rides on these same subjects and the `nats` / `@nats-io/services` packages. Check `node_modules/@synadia/...` or `npm view` for the exact exports you have.

---

## 1. How discovery and fan-out work on NATS

There are two distinct problems:

**Discovery** — "who is out there?" NATS services (the `micro` framework, which agent SDKs use under the hood) automatically respond to a set of well-known **`$SRV` subjects**:

| Subject | Returns |
| --- | --- |
| `$SRV.PING` | Every service replies with its id/name/version |
| `$SRV.INFO` | Full service info incl. endpoint subjects |
| `$SRV.STATS` | Per-service request stats |
| `$SRV.PING.<name>` | Only services with that name |

You discover agents by publishing a request to `$SRV.PING` and collecting **all** replies within a time window. This is the scatter/gather idiom: you can't know in advance how many will answer, so you collect for N milliseconds or until you have "enough."

**Fan-out** — once you know the agents (or even before), you send the prompt. Two strategies:

- **Targeted fan-out (recommended):** discover each agent's request subject, then send a normal `request()` to each one and await each reply. You get a clean 1:1 mapping of agent → response, with per-agent timeouts.
- **Broadcast fan-out:** publish once to a shared subject with a unique reply inbox, and gather every reply that arrives within a window. Fewer messages, but you don't know up front how many to expect.

I'll show both.

---

## 2. Approach A — NATS micro (`@nats-io/services`)

This is the most robust approach and works regardless of which SDK version you have, because every NATS-based agent service answers `$SRV` discovery.

### Install

```bash
npm install @nats-io/nats-core @nats-io/services @nats-io/transport-node
# (older single-package layout: npm install nats)
```

### `meta-agent.ts`

```ts
import { connect, NatsConnection, JSONCodec, Msg } from "@nats-io/transport-node";

// ---- Types ---------------------------------------------------------------

interface ServicePingResponse {
  name: string;
  id: string;
  version: string;
  metadata?: Record<string, string>;
}

interface ServiceInfoResponse extends ServicePingResponse {
  description?: string;
  endpoints: Array<{
    name: string;
    subject: string;       // the subject you send requests to
    metadata?: Record<string, string>;
  }>;
}

interface DiscoveredAgent {
  name: string;
  id: string;
  version: string;
  /** subject to send the prompt to */
  promptSubject: string;
}

interface AgentResult {
  agent: DiscoveredAgent;
  ok: boolean;
  response?: string;
  error?: string;
  latencyMs: number;
}

const jc = JSONCodec();

// ---- Discovery -----------------------------------------------------------

/**
 * Scatter a $SRV.INFO request and gather every reply within `windowMs`.
 * We use INFO (not PING) because it tells us each service's endpoint subjects,
 * which is what we need to actually send a prompt.
 */
async function discoverAgents(
  nc: NatsConnection,
  opts: { windowMs?: number; endpointName?: string } = {},
): Promise<DiscoveredAgent[]> {
  const windowMs = opts.windowMs ?? 1000;
  const inbox = nc.createInbox();
  const agents: DiscoveredAgent[] = [];
  const seen = new Set<string>();

  // Subscribe to the reply inbox BEFORE publishing so we don't miss fast repliers.
  const sub = nc.subscribe(inbox);

  const collecting = (async () => {
    for await (const m of sub) {
      try {
        const info = jc.decode(m.data) as ServiceInfoResponse;
        if (seen.has(info.id)) continue;
        seen.add(info.id);

        // Pick the endpoint that handles prompts. Convention: an endpoint named
        // "prompt" / "chat" / "ask", else just take the first endpoint.
        const ep =
          info.endpoints?.find((e) =>
            ["prompt", "chat", "ask", "complete"].includes(e.name),
          ) ?? info.endpoints?.[0];

        if (!ep) continue;

        agents.push({
          name: info.name,
          id: info.id,
          version: info.version,
          promptSubject: ep.subject,
        });
      } catch {
        // ignore malformed replies
      }
    }
  })();

  // Fire the discovery request to the well-known service-info subject.
  // Append `.<name>` to filter to a specific agent family if you want.
  const subject = opts.endpointName ? `$SRV.INFO.${opts.endpointName}` : "$SRV.INFO";
  nc.publish(subject, new Uint8Array(0), { reply: inbox });

  // Collect for the window, then stop.
  await new Promise((r) => setTimeout(r, windowMs));
  sub.unsubscribe();
  await collecting;

  return agents;
}

// ---- Fan-out -------------------------------------------------------------

/**
 * Send the same prompt to every discovered agent in parallel and gather results.
 * Each request has its own timeout so one slow/dead agent can't stall the batch.
 */
async function fanOutPrompt(
  nc: NatsConnection,
  agents: DiscoveredAgent[],
  prompt: string,
  perAgentTimeoutMs = 30_000,
): Promise<AgentResult[]> {
  const tasks = agents.map(async (agent): Promise<AgentResult> => {
    const start = Date.now();
    try {
      const reply = await nc.request(
        agent.promptSubject,
        jc.encode({ prompt }),
        { timeout: perAgentTimeoutMs },
      );

      // Agents may reply as JSON {response: "..."} or as raw text.
      let response: string;
      try {
        const decoded = jc.decode(reply.data) as { response?: string; text?: string };
        response = decoded.response ?? decoded.text ?? new TextDecoder().decode(reply.data);
      } catch {
        response = new TextDecoder().decode(reply.data);
      }

      return { agent, ok: true, response, latencyMs: Date.now() - start };
    } catch (err) {
      return {
        agent,
        ok: false,
        error: err instanceof Error ? err.message : String(err),
        latencyMs: Date.now() - start,
      };
    }
  });

  // allSettled is implicit here since each task catches its own errors.
  return Promise.all(tasks);
}

// ---- Main ----------------------------------------------------------------

async function main() {
  const nc = await connect({
    servers: process.env.NATS_URL ?? "nats://localhost:4222",
    // token / creds / user-pass as appropriate:
    // token: process.env.NATS_TOKEN,
    // authenticator: credsAuthenticator(await fs.readFile(process.env.NATS_CREDS!)),
  });

  try {
    console.log("Discovering agents on the bus...");
    const agents = await discoverAgents(nc, { windowMs: 1500 });

    if (agents.length === 0) {
      console.log("No agents responded. Are any running and answering $SRV.INFO?");
      return;
    }

    console.log(`Found ${agents.length} agent(s):`);
    for (const a of agents) {
      console.log(`  • ${a.name} (v${a.version}, id=${a.id}) -> ${a.promptSubject}`);
    }

    const prompt = "Summarize the tradeoffs of optimistic vs pessimistic locking.";
    console.log(`\nFanning out prompt to ${agents.length} agent(s)...\n`);

    const results = await fanOutPrompt(nc, agents, prompt);

    for (const r of results) {
      console.log("─".repeat(60));
      console.log(`${r.agent.name} (${r.latencyMs}ms): ${r.ok ? "OK" : "ERROR"}`);
      console.log(r.ok ? r.response : r.error);
    }
  } finally {
    // drain() flushes outstanding messages, then closes cleanly.
    await nc.drain();
  }
}

main().catch((e) => {
  console.error("Fatal:", e);
  process.exit(1);
});
```

### Why this works

- **No central registry needed.** Discovery is emergent — every agent that speaks the NATS micro protocol announces itself when pinged. New agents joining the bus are found on the next discovery cycle automatically.
- **Scatter/gather window.** Because you can't know the agent count ahead of time, you subscribe to an inbox, broadcast the request, and collect replies for a fixed window (`windowMs`). 1–2 seconds is plenty on a healthy cluster.
- **Per-agent isolation.** Each prompt request has its own timeout, so a hung agent costs you `perAgentTimeoutMs`, not the whole batch.

---

## 3. Approach B — Synadia agents SDK (higher-level)

Synadia's agents SDK wraps the same primitives but gives you typed agent handles and a discovery client so you don't hand-roll the `$SRV` plumbing. The shape (names may vary by version) looks like this:

```ts
import { connect } from "@nats-io/transport-node";
import { AgentClient } from "@synadia/agents"; // verify the exact export in your version

async function main() {
  const nc = await connect({ servers: process.env.NATS_URL ?? "nats://localhost:4222" });

  // The SDK exposes a client bound to the connection.
  const client = new AgentClient(nc);

  // 1) Discovery — the SDK fans out a ping under the hood and returns handles.
  //    `list()` / `discover()` collects responders within a window.
  const agents = await client.discover({ timeout: 1500 });
  console.log(`Discovered ${agents.length} agents:`, agents.map((a) => a.name));

  // 2) Fan-out — send the same prompt to each agent in parallel.
  const prompt = "Summarize the tradeoffs of optimistic vs pessimistic locking.";

  const results = await Promise.all(
    agents.map(async (agent) => {
      try {
        const res = await agent.prompt(prompt, { timeout: 30_000 });
        return { name: agent.name, ok: true as const, response: res.text ?? res };
      } catch (err) {
        return {
          name: agent.name,
          ok: false as const,
          error: err instanceof Error ? err.message : String(err),
        };
      }
    }),
  );

  for (const r of results) {
    console.log("─".repeat(60));
    console.log(`${r.name}: ${r.ok ? r.response : "ERROR: " + r.error}`);
  }

  await nc.drain();
}

main().catch(console.error);
```

The key methods to look for in your installed SDK version:

- A **discovery** call — often `discover()`, `list()`, or `find()` on a client — that takes a timeout/window and returns agent handles.
- A **prompt/invoke** call on each handle — `agent.prompt()`, `agent.ask()`, `agent.invoke()`, or `agent.run()`.

If your SDK lacks one of these, drop down to Approach A for that part. Mixing is fine — they share a connection and the same wire protocol.

> **Finding the exact API:** run `npm ls @synadia/agents`, then peek at the type definitions:
> ```bash
> ls node_modules/@synadia/agents/dist
> cat node_modules/@synadia/agents/dist/index.d.ts   # or use your editor's go-to-definition
> ```
> The `.d.ts` files are the ground truth for method names in your version.

---

## 4. Production hardening

A few things worth adding once the basic flow works:

**Two-phase discovery for liveness.** `$SRV.INFO` tells you who *registered*, but a service can register and then hang. If you want only *responsive* agents, do a quick `$SRV.PING` round first (cheaper) and only fan out to those that answered.

**Cache discovery, refresh periodically.** Don't re-discover on every prompt. Discover once, cache the agent list, and refresh on an interval (e.g. every 30s) or when a fan-out request to a cached agent times out.

**Filter by metadata.** Agents can advertise `metadata` (model, capabilities, team). Use it to fan out only to relevant agents:

```ts
const codingAgents = agents.filter(
  (a) => a.metadata?.role === "coder" || a.name.includes("coder"),
);
```

If your agents set a metadata tag like `{ kind: "coding-agent" }`, you can also narrow discovery at the source by pinging `$SRV.PING.<service-name>` when they share a name.

**Bounded concurrency.** `Promise.all` over hundreds of agents will open hundreds of in-flight requests at once. If your fleet is large, batch with a small concurrency pool (e.g. `p-limit`) to avoid flooding the bus.

**Use `Promise.allSettled` semantics.** The code above catches per-task errors so a single failure never rejects the whole batch — that's exactly what you want for a comparison harness where partial results are still useful.

**Correlation + tracing.** Stamp each fan-out with a batch id (e.g. in the payload or a header) so you can correlate responses, especially if you later switch to the broadcast-inbox style.

**Drain on shutdown.** Always `await nc.drain()` (not just `close()`) so in-flight replies are flushed before the process exits.

---

## Which approach should you use?

- **Start with Approach A (NATS micro / `$SRV`).** It's vendor-neutral, always works, and you fully understand the wire. Great for a comparison harness because you control discovery windows and per-agent timeouts precisely.
- **Layer in Approach B (Synadia SDK)** once you've confirmed the exact method names in your installed version — it removes boilerplate and gives you typed handles.

Both ride the same subjects, so you can prototype with A and migrate piece by piece. The core mental model — *broadcast a discovery request, gather replies in a window, then fan out targeted prompts with per-agent timeouts and gather with `allSettled` semantics* — is the same either way.
