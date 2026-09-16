# Exposing a Nuxt app as a discoverable, streaming, human-in-the-loop AI agent on NATS

Great use case. You want three things layered on top of `nuxt-nats`:

1. **Discoverability** — other services can find your agent on the bus without knowing its address ahead of time.
2. **Token streaming** — the agent streams partial tokens back to the caller as they are generated, instead of one big final reply.
3. **Human-in-the-loop (HITL)** — before the agent does anything destructive, it pauses and asks *your* Nuxt UI (the human) to confirm, and only proceeds on approval.

NATS gives you clean primitives for all three:

- **Discoverability** → the NATS **Services API** (a.k.a. "micro"). A service advertises a name, version, and endpoints, and responds to the built-in `$SRV.PING`, `$SRV.INFO`, and `$SRV.STATS` discovery subjects. Any client can enumerate services on the bus.
- **Streaming** → the caller creates a unique **inbox** subject, subscribes to it, and the agent publishes many chunk messages to that inbox followed by a "done" sentinel. This is a request/many-replies pattern rather than a single request/reply.
- **HITL** → the agent issues its *own* request to a `confirm.*` subject that your Nuxt front end is listening on, and blocks (with a timeout) until a human answers approve/deny.

Below is a complete, working implementation. It assumes `nuxt-nats` exposes a connected NATS client on the Nitro server side (`useNats()` / `hooks` — adjust the import to match your module version) and that you have an LLM client available. I use the official `nats` JS client APIs (`jetstream`/core + `Svc`/services) so the code is portable even if your `nuxt-nats` wrapper differs slightly.

---

## 1. Design / subject layout

```
agents.nuxt-app.chat        <- main endpoint (request comes in here)
_INBOX.<nuid>               <- per-request reply inbox (agent streams chunks here)
confirm.<sessionId>         <- agent asks the human; Nuxt UI answers
$SRV.PING / $SRV.INFO       <- automatic discovery (provided by the services API)
```

Message envelope (JSON) for streamed chunks so the caller can tell tokens, control, and errors apart:

```jsonc
// token chunk
{ "type": "token", "data": "Hello" }
// the agent needs a human decision (surfaced to caller for transparency)
{ "type": "await_confirm", "action": "delete all invoices", "confirmId": "confirm.abc123" }
// terminal messages
{ "type": "done", "data": "…full text…" }
{ "type": "error", "error": "user denied destructive action" }
```

---

## 2. The agent (Nitro server plugin)

Create `server/plugins/agent.ts`. Nuxt/Nitro loads `server/plugins/*` once at startup, which is the right place to register a long-lived NATS service.

```ts
// server/plugins/agent.ts
import { JSONCodec, headers as natsHeaders } from 'nats'
// import { Svc } from '@nats-io/services'   // or nats built-in services API
// import { useNats } from '#nats'           // however your nuxt-nats exposes the connection

const jc = JSONCodec()

// ---- pretend LLM: replace with your real streaming client ----
async function* streamLLM(prompt: string): AsyncGenerator<string> {
  for (const tok of `Working on: ${prompt} ... done`.split(' ')) {
    await new Promise(r => setTimeout(r, 40))
    yield tok + ' '
  }
}

// Heuristic: does this request intend a destructive action?
function destructiveActionOf(prompt: string): string | null {
  if (/\b(delete|drop|wipe|purge|remove all|reset)\b/i.test(prompt)) {
    return prompt.trim()
  }
  return null
}

export default defineNitroPlugin(async () => {
  const nc = await useNats()               // connected NATS client from nuxt-nats

  // Register a DISCOVERABLE service. Name + version show up in $SRV.INFO.
  const svc = await nc.services.add({
    name: 'nuxt-app',
    version: '1.0.0',
    description: 'Nuxt AI agent with streaming + human-in-the-loop',
    metadata: { 'agent.kind': 'assistant', 'agent.streaming': 'true' },
  })

  const grp = svc.addGroup('agents')       // -> subjects under agents.*

  grp.addEndpoint('chat', {
    subject: 'agents.nuxt-app.chat',
    handler: async (err, msg) => {
      if (err) return

      // The caller MUST set msg.reply to a unique inbox it is subscribed to.
      const replyTo = msg.reply
      if (!replyTo) return

      const { prompt, sessionId } = jc.decode(msg.data) as {
        prompt: string
        sessionId: string
      }

      const emit = (obj: unknown) => nc.publish(replyTo, jc.encode(obj))

      try {
        // ---- HUMAN-IN-THE-LOOP GATE ----
        const dangerous = destructiveActionOf(prompt)
        if (dangerous) {
          const confirmId = `confirm.${sessionId}`
          // Tell the caller we are pausing for a human (transparency).
          emit({ type: 'await_confirm', action: dangerous, confirmId })

          // Ask the human. This is a REQUEST the Nuxt UI answers.
          let approved = false
          try {
            const answer = await nc.request(
              confirmId,
              jc.encode({ action: dangerous }),
              { timeout: 60_000 },           // wait up to 60s for a human
            )
            approved = (jc.decode(answer.data) as { approved: boolean }).approved
          } catch {
            approved = false                 // timeout => treat as denial (fail safe)
          }

          if (!approved) {
            emit({ type: 'error', error: 'destructive action not confirmed by human' })
            return
          }
          // ...only now would you actually perform the destructive op...
        }

        // ---- STREAM TOKENS ----
        let full = ''
        for await (const token of streamLLM(prompt)) {
          full += token
          emit({ type: 'token', data: token })
        }
        emit({ type: 'done', data: full })
      } catch (e: any) {
        emit({ type: 'error', error: String(e?.message ?? e) })
      }
    },
  })

  console.log('nuxt-app agent registered on agents.nuxt-app.chat')
})
```

Key points:

- **Discoverability is automatic.** By registering through `nc.services.add(...)`, the runtime answers `$SRV.PING`, `$SRV.INFO.nuxt-app`, and `$SRV.STATS.nuxt-app`. No extra code.
- **Streaming uses the caller's reply inbox.** We publish many messages to `msg.reply`, ending with a `done` (or `error`) sentinel. NATS core is fire-and-forget, so the sentinel is how the caller knows to stop.
- **HITL is a nested request.** The agent itself becomes a *client* of `confirm.<sessionId>`, which your own Nuxt UI serves. Timeout → denial keeps it fail-safe.

> If you need the stream to survive disconnects/replays (at-least-once), publish chunks into a **JetStream** stream keyed by request id instead of to a core inbox, and have the caller consume with an ephemeral consumer. Core inboxes (shown here) are the simplest and lowest-latency choice for live token streaming.

---

## 3. The human side (Nuxt UI answers the confirmation)

Your front end needs to *listen* for confirmation requests and let a person click Approve/Deny. Do this on the server (bridge to the browser over WebSocket/SSE) or, if `nuxt-nats` exposes a browser client over WebSocket, directly in a composable.

```ts
// server/plugins/confirm-listener.ts  (or a composable if you connect from the browser)
export default defineNitroPlugin(async () => {
  const nc = await useNats()
  const jc = JSONCodec()

  // Wildcard: catch any session's confirmation request.
  const sub = nc.subscribe('confirm.*')
  ;(async () => {
    for await (const msg of sub) {
      const { action } = jc.decode(msg.data) as { action: string }

      // Push to the browser (WS/SSE) and await the human's click.
      const approved = await askHumanInBrowser(action)   // resolves true/false

      // Reply directly to the agent's pending request.
      msg.respond(jc.encode({ approved }))
    }
  })()
})
```

`askHumanInBrowser` is your app's own mechanism — e.g. emit over a WebSocket to a modal, resolve the promise when the user clicks. The important part is `msg.respond(...)`: that unblocks the agent's `nc.request()`.

---

## 4. How another service calls the agent

Any service on the bus — Go, Python, another Node app — can (a) discover the agent and (b) call it while consuming the token stream.

### Discover it

```bash
# Using the nats CLI
nats micro ls              # lists services, incl. nuxt-app@1.0.0
nats micro info nuxt-app   # shows endpoints, subjects, metadata
```

Programmatically (Node):

```ts
import { connect, JSONCodec } from 'nats'
const nc = await connect({ servers: 'nats://localhost:4222' })
const jc = JSONCodec()

// PING discovery: every service replies within a short window.
const found: any[] = []
const inbox = nc.subscribe('_INBOX.discover', {
  callback: (_e, m) => found.push(jc.decode(m.data)),
})
nc.publish('$SRV.PING', new Uint8Array(0), { reply: '_INBOX.discover' })
await new Promise(r => setTimeout(r, 500))
inbox.unsubscribe()
console.log('services on the bus:', found.map(s => `${s.name}@${s.version}`))
```

### Call it and consume the stream

Because the agent replies to a unique inbox with many messages, the caller sets up its own inbox subscription (not a plain `request`, which only waits for one reply):

```ts
import { connect, JSONCodec, createInbox } from 'nats'

const nc = await connect({ servers: 'nats://localhost:4222' })
const jc = JSONCodec()

async function callAgent(prompt: string, onToken: (t: string) => void) {
  const inbox = createInbox()                 // unique _INBOX.<nuid>
  const sub = nc.subscribe(inbox)

  nc.publish('agents.nuxt-app.chat', jc.encode({ prompt, sessionId: 'sess-42' }), {
    reply: inbox,                             // tell the agent where to stream
  })

  for await (const msg of sub) {
    const evt = jc.decode(msg.data) as any
    switch (evt.type) {
      case 'token':
        onToken(evt.data)                     // stream to your own UI/logs
        break
      case 'await_confirm':
        console.log(`(agent paused for human approval of: ${evt.action})`)
        break                                 // just informational for the caller
      case 'done':
        sub.unsubscribe()
        return evt.data                       // full text
      case 'error':
        sub.unsubscribe()
        throw new Error(evt.error)
    }
  }
}

const result = await callAgent('summarize today\'s orders', t => process.stdout.write(t))
console.log('\nFINAL:', result)
```

### Same call from Python

```python
import asyncio, json
import nats
from nats.aio.client import Client

async def main():
    nc = await nats.connect("nats://localhost:4222")
    inbox = nc.new_inbox()
    sub = await nc.subscribe(inbox)

    await nc.publish(
        "agents.nuxt-app.chat",
        json.dumps({"prompt": "delete all invoices", "sessionId": "sess-99"}).encode(),
        reply=inbox,
    )

    full = ""
    async for msg in sub.messages:
        evt = json.loads(msg.data)
        if evt["type"] == "token":
            full += evt["data"]; print(evt["data"], end="", flush=True)
        elif evt["type"] == "await_confirm":
            print(f"\n[agent waiting on human: {evt['action']}]")
        elif evt["type"] == "done":
            await sub.unsubscribe(); break
        elif evt["type"] == "error":
            await sub.unsubscribe(); raise RuntimeError(evt["error"])
    print("\nDONE:", full)

asyncio.run(main())
```

When the prompt is destructive (`delete all invoices`), the caller sees an `await_confirm` event, the agent blocks on `confirm.sess-99`, your Nuxt UI pops a modal, and the run continues or aborts based on the human's click.

---

## 5. End-to-end flow

```
caller ──publish(agents.nuxt-app.chat, reply=_INBOX.x)──▶ agent
                                                          │  destructive? 
                                                          ├─▶ request(confirm.sess) ──▶ Nuxt UI ──▶ human clicks
                                                          │◀── respond({approved})
agent ──publish token,token,…,done──▶ _INBOX.x ──▶ caller streams to its UI
```

---

## 6. Practical notes

- **Timeouts everywhere.** The HITL `request` has a 60s timeout that defaults to *deny*. Give the caller its own overall deadline too (drain the inbox sub after N seconds of silence).
- **One inbox per request.** `createInbox()` / `nc.new_inbox()` ensures streams for concurrent calls don't collide.
- **Backpressure / large streams.** For long generations or unreliable networks, switch the token transport to JetStream so chunks are persisted and replayable; keep the HITL request on core NATS (it's inherently interactive).
- **Auth & scoping.** Put the agent under an account/subject-permission scheme so only authorized callers can publish to `agents.nuxt-app.chat`, and only your UI can answer `confirm.*`.
- **Health/stats.** `nats micro stats nuxt-app` gives request counts, errors, and latencies for free — useful for dashboards.
- **Graceful shutdown.** Call `svc.stop()` on Nitro `close` so the service deregisters cleanly.

This gives you a Nuxt app that is discoverable as a first-class NATS service, streams tokens back over a per-call inbox, and refuses to do anything destructive until a human explicitly approves it.
