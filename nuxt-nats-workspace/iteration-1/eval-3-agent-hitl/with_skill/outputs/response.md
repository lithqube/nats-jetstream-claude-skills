# Exposing a Nuxt app as a discoverable AI agent on NATS (streaming + human-in-the-loop)

With `nuxt-nats` you host an agent with **`defineNatsAgent`** and call agents with **`useAgents`**. Both sit on the **Synadia Agent Protocol**, so any service — Nuxt, plain Node, Python, Go — that speaks the protocol can discover and prompt your agent. You get three things for free from the wrapper:

- **Discovery** — the agent registers as a NATS micro-service (`$SRV.PING/INFO.agents`), so callers find it without hard-coding a subject.
- **Token streaming** — every `response.send(...)` is one `response` chunk on the wire; the caller iterates them as they arrive.
- **Mid-stream human-in-the-loop** — `response.ask(question, { timeoutMs })` emits a `query` chunk and blocks until the caller answers, so you can gate destructive work on a confirmation.

Two things to internalize before the code:

1. **It's server-only and gated.** Agents (like consumers) run **only when `NUXT_NATS_WORKERS=true`**. A serverless/edge deployment can *publish* but must not host a long-lived agent — run the agent in a separate persistent worker process. If your agent "never registers", check this env var first.
2. **The method is `ask`, the chunk is `query`.** The host calls `response.ask(...)`; the *caller* receives a chunk whose `type` is `"query"` and replies once. Don't look for a `response.query()` — it doesn't exist.

---

## 1. Configure the module

```ts
// nuxt.config.ts
export default defineNuxtConfig({
  modules: ['nuxt-nats'],
  nats: {
    servers: ['nats://localhost:4222'], // prod: override via NUXT_NATS_SERVERS (all cluster nodes)
    // auth is injected via env in prod: NUXT_NATS_USER_JWT + NUXT_NATS_NKEY_SEED
    // No `streams:` block needed — the agent fabric is request/reply + micro-service
    // discovery, not a JetStream stream.
  },
})
```

The agent host needs the worker gate on. Run the process that hosts the agent with:

```bash
NUXT_NATS_WORKERS=true \
NUXT_NATS_SERVERS=nats://localhost:4222 \
node .output/server/index.mjs
```

---

## 2. Host the agent — `server/plugins/support-agent.ts`

Register the agent in a **Nitro server plugin**. The module handles the SSR/connection-readiness race for you (agents poll the connection until it's up), and the SDK emits the mandatory leading `ack` chunk and the trailing zero-byte terminator — you only write `onPrompt`.

```ts
// server/plugins/support-agent.ts
export default defineNitroPlugin(() => {
  defineNatsAgent({
    agent: 'support',        // ┐ identity tuple ->
    owner: 'acme',           // ├ subject agents.prompt.support.acme.main
    name: 'main',            // ┘ and discovery metadata
    // Optional: heartbeatIntervalS, attachmentsOk, maxPayload,
    //           extraMetadata, extraEndpoints, subjectToken

    async onPrompt(envelope, response) {
      const prompt = envelope.prompt

      // ---- Human-in-the-loop: confirm BEFORE any destructive action ----
      if (isDestructive(prompt)) {
        let answer: string
        try {
          answer = await response.ask(
            `This will run a destructive action:\n  "${prompt}"\nConfirm? (yes/no)`,
            { timeoutMs: 15_000 },
          )
        } catch {
          // ask() REJECTS on timeout. Default-deny for destructive work:
          // treat "no answer" as "no" rather than hanging the handler.
          await response.send('\nNo confirmation received — aborted.')
          return
        }
        if (answer.trim().toLowerCase() !== 'yes') {
          await response.send('\nAborted by user.')
          return
        }
      }

      // ---- Stream tokens back: one response.send() == one `response` chunk ----
      const stream = await runModel(prompt) // your LLM / tool call, yields tokens
      for await (const token of stream) {
        await response.send(token)          // caller sees chunk.type === 'response'
      }
      // No explicit "done" — returning from onPrompt closes the stream;
      // the SDK sends the zero-byte terminator for you.
    },
  })
})

// --- helpers (your logic) ---
function isDestructive(prompt: string): boolean {
  return /\b(delete|drop|purge|refund|deploy|wipe)\b/i.test(prompt)
}

async function* runModel(prompt: string): AsyncGenerator<string> {
  // Replace with your real model/tool stream. Demo: stream word-by-word.
  for (const word of `Working on: ${prompt}`.split(' ')) {
    await new Promise((r) => setTimeout(r, 40))
    yield word + ' '
  }
}
```

Key points:

- **Streaming** is just calling `response.send()` repeatedly. Each call is a discrete chunk on the wire, so the caller renders tokens live.
- **The confirmation gate runs mid-stream.** `response.ask()` pauses your handler, emits a `query` chunk, and resolves with the caller's single reply. Anything you `send` after a `yes` streams normally.
- **Default-deny on timeout.** For destructive actions, a caller that never answers must be treated as "no". `ask()` *rejects* on timeout, so wrap it in `try/catch` and abort — never let the handler hang.

---

## 3. Call the agent from another Nuxt service — `useAgents()`

If the *caller* is also a `nuxt-nats` app, use the auto-imported `useAgents()`. It's cached per process (one shared heartbeat subscription no matter how many callers), so `discover()` is cheap.

```ts
// server/api/ask.post.ts  (a different Nuxt service)
export default defineEventHandler(async (event) => {
  const { prompt } = await readBody(event)
  const agents = useAgents()

  // Discover, then target the one you want by its metadata.
  const found = await agents.discover()
  const agent = found.find((a) => a.agent === 'support' && a.owner === 'acme')
  if (!agent) throw createError({ statusCode: 503, statusMessage: 'support agent offline' })

  let text = ''
  for await (const chunk of agent.prompt(prompt)) {
    if (chunk.type === 'response') {
      text += chunk.text              // token stream
    } else if (chunk.type === 'query') {
      // Human-in-the-loop question from the host. Here we auto-approve;
      // in a real app you'd surface chunk.text to a human and relay their reply.
      await chunk.reply('yes')
    }
    // chunk.type is 'response' | 'status' | 'query' — ignore unknown types (forward-compat)
  }

  return { answer: text }
})
```

Don't build the `agents.prompt.*` subject by hand — `discover()` hands you an addressed agent handle. Filter the discovery results by `agent`/`owner`/`name` to target a subset or fan out to a fleet.

### Streaming HITL through to your own UI (SSE)

Because the caller sits in a Nitro route, you can relay tokens straight to the browser over Server-Sent Events, and forward a `query` chunk to the user as a "needs confirmation" event:

```ts
// server/api/ask-stream.get.ts
export default defineEventHandler(async (event) => {
  const prompt = getQuery(event).prompt as string
  const agents = useAgents()
  const [agent] = await agents.discover()

  const es = createEventStream(event)
  ;(async () => {
    for await (const chunk of agent.prompt(prompt)) {
      if (chunk.type === 'response') {
        await es.push({ event: 'token', data: chunk.text })
      } else if (chunk.type === 'query') {
        // Surface the confirmation prompt to the browser; a real UI would
        // collect the user's answer and call chunk.reply(...) with it.
        await es.push({ event: 'confirm', data: chunk.text })
        await chunk.reply('yes') // replace with the human's actual answer
      }
    }
    await es.push({ event: 'done', data: '' })
    await es.close()
  })()

  return es.send()
})
```

---

## 4. Call it from a non-Nuxt service — `@synadia-ai/agents`

Any service that speaks the protocol works. Here's a plain Node caller (no Nuxt) using the underlying SDK directly. This is what a Python or Go service does too — same wire protocol, same `query` chunk with a `reply_subject`.

```ts
import { connect } from '@nats-io/transport-node'
import { Agents } from '@synadia-ai/agents'

const nc = await connect({ servers: 'nats://localhost:4222' })
const agents = new Agents({ nc })

const [agent] = await agents.discover() // $SRV.PING/INFO.agents under the hood
if (!agent) throw new Error('no agents on the fabric')

for await (const msg of await agent.prompt('please delete the stale invoices')) {
  if (msg.type === 'response') {
    process.stdout.write(msg.text)       // live token stream
  } else if (msg.type === 'query') {
    // Human-in-the-loop: the host asked a question. Answer ONCE on the
    // reply subject it provided. Here we'd prompt a real human.
    const humanAnswer = await promptHumanSomehow(msg.data.prompt) // 'yes' | 'no'
    nc.publish(msg.data.reply_subject, new TextEncoder().encode(humanAnswer))
  }
  // ignore unknown chunk types — forward-compatibility is required
}

await nc.drain()
```

The low-level shape shows exactly how HITL works on the wire: the host's `response.ask()` emits a `query` chunk carrying a **`reply_subject`**; the caller publishes the human's answer once to that subject; the host's `ask()` promise resolves with it. If the caller never publishes, the host's `ask()` rejects on its `timeoutMs` and (in the code above) aborts.

---

## How the pieces line up

```
  Caller service                         Host (NUXT_NATS_WORKERS=true)
  ------------                           -----------------------------
  discover()  ──$SRV.PING/INFO.agents──▶ defineNatsAgent registers as a micro-service
  prompt(txt) ──agents.prompt.support.acme.main──▶ onPrompt(envelope, response)
        ◀──── ack chunk (auto) ─────────────────
        ◀──── response chunks ────────────  response.send(token)   ← streaming
        ◀──── query chunk (reply_subject) ─  response.ask(...)      ← HITL, blocks
  reply 'yes' ──▶ reply_subject ───────────▶ ask() resolves 'yes'
        ◀──── response chunks ────────────  response.send(result)  ← work proceeds
        ◀──── zero-byte terminator (auto) ─  onPrompt returns
```

---

## Gotchas worth knowing

- **`NUXT_NATS_WORKERS=true` is mandatory on the host.** Without it the plugin registers nothing and no agent appears in `discover()`. This is the single most common "my agent doesn't work" cause. Recommended topology: serverless/edge publishers + a separate persistent worker process that hosts the agent.
- **Server-only.** There are no browser composables — you can't `useAgents()` in a Vue component (ADR-002). Host and call from `server/` (plugins, API routes, Nitro tasks). To reach the browser, bridge through an SSE route as shown above.
- **`ask` vs `query`.** Host emits with `response.ask(prompt, { timeoutMs })`; caller receives `chunk.type === 'query'` and replies once. There is no `response.query()`.
- **Default-deny on timeout for destructive actions.** `ask()` rejects when the caller doesn't answer in time — catch it and treat as "no". Never leave a destructive path hanging on an unanswered confirmation.
- **Answer a `query` exactly once**, on the provided reply subject. Ignore unknown chunk types so you stay forward-compatible as the 0.x SDK evolves.
- **Scale by running more hosts.** Start multiple host processes with the same `agent`/`owner` (different `name`); the `agents` queue group load-balances prompts across them automatically.
- **The SDK is young (0.x, pinned `^0.5.2`).** `nuxt-nats` keeps the wrapper intentionally thin. For deeper protocol detail (chunk types, discovery internals, meta-agent fan-out/merge, liveness), read the `nats-agent-fabric` skill. For ordinary event/worker plumbing, prefer `jsPublish` + `defineNatsConsumer` — reserve the agent surface for genuinely agentic work like this.
