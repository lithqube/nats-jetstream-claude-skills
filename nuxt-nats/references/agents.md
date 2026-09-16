# Agent Fabric with nuxt-nats

`nuxt-nats` ships a thin wrapper over the **Synadia Agent Protocol** (`@synadia-ai/agent-service` host + `@synadia-ai/agents` caller, pinned at `^0.5.2` and kept intentionally thin because the SDK is 0.x and unstable). For the wire protocol itself — subjects, chunk types, the ack-first / zero-byte-terminator rules, discovery — read the **`nats-agent-fabric`** skill. This file is only the nuxt-nats surface.

Like consumers, agents run **only when `NUXT_NATS_WORKERS=true`**.

## Host an agent — `defineNatsAgent`

Register in a Nitro server plugin:

```ts
export default defineNitroPlugin(() => {
  defineNatsAgent({
    agent: 'echo',
    owner: 'demo',
    name: 'main',                 // identity tuple -> subject agents.prompt.echo.demo.main
    // heartbeatIntervalS, attachmentsOk, maxPayload, extraMetadata, extraEndpoints, subjectToken all optional
    async onPrompt(envelope, response) {
      // stream tokens back
      for (const word of envelope.prompt.split(' ')) {
        await response.send(word + ' ')
      }
    },
  })
})
```

### Human-in-the-loop: `response.ask`, not `response.query`

The host asks the caller a mid-stream question with **`response.ask(prompt, { timeoutMs })`** — it emits a `query` chunk and resolves with the caller's single reply. (The method is `ask`; `query` is the chunk *type* the caller receives.)

```ts
async onPrompt(envelope, response) {
  if (isDestructive(envelope.prompt)) {
    const answer = await response.ask(`Confirm: "${envelope.prompt}"? (yes/no)`, { timeoutMs: 15_000 })
    if (answer.trim().toLowerCase() !== 'yes') { await response.send('Aborted.'); return }
  }
  await response.send(doWork(envelope.prompt))
}
```

Default-deny on timeout for destructive actions: if the caller never answers, `ask` rejects — treat that as "no" rather than hanging the handler. The SDK emits the mandatory leading `ack` chunk and the zero-byte terminator for you.

## Call agents — `useAgents`

```ts
const agents = useAgents()               // process-wide cached: one heartbeat subscription for the app
const [agent] = await agents.discover()  // $SRV.PING/INFO.agents under the hood
for await (const chunk of agent.prompt('hello')) {
  if (chunk.type === 'response') process.stdout.write(chunk.text)
  // chunk.type is 'response' | 'status' | 'query'
}
```

`useAgents()` is cached per process so you hold a single heartbeat subscription no matter how many callers you have. Filter `discover()` results by the `agent`/`owner` metadata to target a subset, and see the `nats-agent-fabric` skill's `patterns/meta-agent.md` for fan-out/merge and liveness.

## Reality check

Both surveyed production apps that use nuxt-nats for messaging do **not** run agents — the agent surface ships in the module but is younger and 0.x. Use it for genuinely agentic work (exposing a harness on the bus, orchestrating a fleet); for ordinary event/worker plumbing, stick to `jsPublish` + `defineNatsConsumer`.
