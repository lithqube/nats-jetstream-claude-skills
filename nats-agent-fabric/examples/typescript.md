# TypeScript / Node / Bun Examples

Using `@synadia-ai/agents` (caller) and `@synadia-ai/agent-service` (host). Reference implementation; most mature SDK (npm ~0.5.2). Requires Node ≥20 or Bun ≥1.2 and a reachable NATS server.

> APIs are 0.x and may drift — verify the installed version. The **wire protocol** (`concepts/protocol.md`) is the stable contract; if a method name here differs from the installed package, honor the protocol and adjust the call.

## Install

```bash
# Caller / meta-agent
npm i @synadia-ai/agents @nats-io/transport-node
# Host / agent
npm i @synadia-ai/agent-service @nats-io/transport-node
```

## Host: serve an agent

The host SDK handles micro registration (service name `agents`), the mandatory `ack` chunk, the queue group, heartbeats, and the zero-byte terminator. You supply `onPrompt`.

```ts
import { connect } from "@nats-io/transport-node";
import { AgentService } from "@synadia-ai/agent-service";

const nc = await connect({ servers: "nats://localhost:4222" });

const service = new AgentService({
  nc,
  agent: "echo",     // canonical harness id  -> agents.prompt.echo.demo.main
  owner: "demo",
  name: "main",
  description: "demo echo agent",
});

// An agent is just: prompt -> streamed reply.
service.onPrompt(async (envelope, response) => {
  // envelope.prompt is the user text; envelope.attachments if attachments_ok
  await response.send(`echo: ${envelope.prompt}`);
  // multiple sends stream multiple `response` chunks (e.g. token streaming)
});

await service.start();
console.log("agent up — discover with: nats req '$SRV.PING.agents' ''");
// keep the process alive; await service.stop() on shutdown
```

### Wrapping an LLM (streaming tokens)

```ts
service.onPrompt(async (envelope, response) => {
  const stream = await llm.stream(envelope.prompt);     // your model client
  for await (const token of stream) {
    await response.send(token);                          // one `response` chunk per token
  }
});
```

### Asking the caller a question mid-stream (human-in-the-loop)

The host emits a mid-stream question with `response.ask(prompt, { timeoutMs })` — it sends a `query` chunk and resolves with the caller's single reply. (The chunk the *caller* receives has `type: "query"`; the *host* method that emits it is `ask`, not `query`.)

```ts
service.onPrompt(async (envelope, response) => {
  if (isDestructive(envelope.prompt)) {
    const answer = await response.ask("Confirm deletion of 200 files? (yes/no)", { timeoutMs: 15_000 });
    if (answer.trim().toLowerCase() !== "yes") {
      await response.send("Aborted.");
      return;
    }
  }
  await response.send(doWork(envelope.prompt));
});
```

> If the caller never answers, `ask` rejects on timeout — default-deny (treat it as "no") for destructive actions rather than letting the handler hang.

## Caller: discover and prompt

```ts
import { connect } from "@nats-io/transport-node";
import { Agents } from "@synadia-ai/agents";

const nc = await connect({ servers: "nats://localhost:4222" });
const agents = new Agents({ nc });

const [agent] = await agents.discover();          // $SRV.PING/INFO.agents under the hood
if (!agent) throw new Error("no agents on the fabric");

for await (const msg of await agent.prompt("hello")) {
  if (msg.type === "response") process.stdout.write(msg.text);
  else if (msg.type === "query") {
    // human-in-the-loop: answer once on the provided reply subject
    nc.publish(msg.data.reply_subject, new TextEncoder().encode("yes"));
  }
  // ignore unknown chunk types — forward-compat is required
}
```

## Caller: fan out to a fleet and merge

```ts
const agents = new Agents({ nc });
const all = await agents.discover();
const coders = all.filter(a => ["cc", "opencode", "pi"].includes(a.agent));

const answers = await Promise.all(coders.map(async (agent) => {
  let text = "";
  for await (const msg of await agent.prompt("Refactor this function for clarity")) {
    if (msg.type === "response") text += msg.text;
  }
  return { agent: `${agent.agent}/${agent.owner}/${agent.name}`, text };
}));

for (const a of answers) console.log(`\n=== ${a.agent} ===\n${a.text}`);
```

See `patterns/meta-agent.md` for merge strategies (collect-all / race / quorum) and liveness tracking.

## Notes

- **Identity → subject:** `agent`/`owner`/`name` produce `agents.prompt.{agent}.{owner}.{name}`. Keep tokens lowercase `a–z 0–9 - _`, never leading `$`.
- **Scaling:** start multiple host processes with the same `agent`/`owner` (different `name`) — the `agents` queue group load-balances prompts across them automatically.
- **Attachments:** advertise `attachments_ok` on the host and send `{ filename, content }` with base64 (padded, not URL-safe) from the caller.
- **Don't construct prompt subjects by hand** in caller code — let `discover()` give you the agent handle; the SDK addresses the discovered subject.
