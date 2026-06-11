# Pattern: Meta-Agent (discover → fan-out → merge)

A meta-agent coordinates a fleet it doesn't know in advance: it discovers the agents currently on the bus, prompts some or all of them, and merges what comes back. This is the orchestrator shape the protocol is built for.

Three building blocks: **discovery**, **fan-out with streaming merge**, and **liveness tracking**. Plus **human-in-the-loop** when an agent asks a mid-stream question.

## 1. Discover the fleet

Never hardcode endpoints — the population changes as agents start and stop. Use `$SRV` discovery (the caller SDK wraps this in `discover()`):

- `$SRV.PING.agents` → who's out there right now.
- `$SRV.INFO.agents` → full info including the actual `prompt`/`status` endpoint subjects and metadata (`agent`, `owner`, `name`, capabilities like `max_payload`/`attachments_ok`).

Filter the returned set by metadata to target a subset — e.g. "every agent whose `agent` is a coding harness," or "all agents under `owner=team-search`."

```ts
const agents = new Agents({ nc });
const all = await agents.discover();
const coders = all.filter(a => ["cc", "opencode", "pi"].includes(a.agent));
```

## 2. Fan-out and merge streams

Prompt the targeted agents concurrently and merge their chunk streams. Each agent's reply is an async iterator of typed chunks; tag each by source so the merge stays attributable.

```ts
async function fanOut(targets, promptText) {
  const results = new Map(); // agentKey -> accumulated text
  await Promise.all(targets.map(async (agent) => {
    let buf = "";
    for await (const msg of await agent.prompt(promptText)) {
      if (msg.type === "response") buf += msg.text;
      else if (msg.type === "query") await handleQuery(agent, msg); // see §4
      // ignore unknown chunk types — forward-compat is required
    }
    results.set(`${agent.agent}/${agent.owner}/${agent.name}`, buf);
  }));
  return results;
}
```

Merge strategies, pick per use case:

- **Collect-all** — return every agent's answer keyed by identity (comparison, voting, ensemble).
- **First-wins / race** — take the fastest complete reply, cancel the rest (latency-sensitive).
- **Quorum** — wait for K of N, useful when agents may be slow or offline.

Because prompts load-balance across instances of the same agent via the `agents` queue group, "fan out to every *kind* of agent" and "fan out to every *instance*" are different intents — the first prompts one instance per agent identity; the second requires addressing instances individually via `$SRV.INFO.agents.{instance_id}`.

## 3. Track liveness without polling

Agents beacon to `agents.hb.{agent}.{owner}.{name}` (~every 30s). Subscribe with a wildcard and mark an agent offline after 3× its advertised `interval_s` with no beat. This keeps your target set fresh without re-running discovery on every prompt.

```ts
const sub = nc.subscribe("agents.hb.*.*.*");
(async () => {
  for await (const m of sub) {
    const hb = JSON.parse(sc.decode(m.data));
    liveness.set(`${hb.agent}/${hb.owner}/${hb.name}`, { lastSeen: hb.ts, intervalS: hb.interval_s });
  }
})();
```

To bootstrap liveness for an agent you just discovered (without waiting for its next beat), hit its `status` request/reply endpoint — it returns the same JSON shape as a heartbeat.

## 4. Human-in-the-loop (mid-stream queries)

An agent can pause and ask a question via a `query` chunk while its stream stays open:

```json
{ "type": "query",
  "data": { "id": "…", "reply_subject": "_INBOX.…", "prompt": "Confirm deletion of 200 files?" } }
```

The meta-agent routes this to a human (or an automated policy), then publishes the answer **once** to `reply_subject`. The agent resumes streaming. Don't treat `query` chunks as response text — branch on `type`.

```ts
async function handleQuery(agent, msg) {
  const answer = await askHuman(msg.data.prompt);   // or apply a policy
  nc.publish(msg.data.reply_subject, sc.encode(answer));
}
```

## Robustness checklist

- **Tolerate offline agents** — discovery is a snapshot; an agent may die mid-fan-out. Treat a missing terminator past a timeout as a failed branch, not a hang.
- **Respect capabilities** — check `max_payload` / `attachments_ok` from discovery metadata before sending large or attachment-bearing prompts; oversize sends earn a `400`.
- **Honor the terminator** — a branch is done only at the zero-byte, header-less message. Don't finalize merges on the first lull.
- **Ignore unknown chunk types** and preserve unknown fields — required for forward-compat as 0.x evolves.
- **Handle error headers** — a branch may terminate with `Nats-Service-Error-Code` (e.g. `429` with `retry_after_s`); back off and optionally retry on another instance.
