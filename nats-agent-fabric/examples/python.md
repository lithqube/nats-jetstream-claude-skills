# Python Examples

Using `synadia-ai-agents` (caller) and `synadia-ai-agent-service` (host). Beta — `synadia-ai-agent-service` 0.4.1 (May 2026), depends on `synadia-ai-agents>=0.6`. Requires Python ≥3.11 (3.11–3.13) and a reachable NATS server. Strong fit for LLM/AI agent use cases.

> APIs are 0.x and may drift — verify the installed version. The **wire protocol** (`concepts/protocol.md`) is the stable contract; if a symbol here differs from the installed package, honor the protocol and adjust.

## Install

```bash
pip install synadia-ai-agents          # caller / meta-agent
pip install synadia-ai-agent-service   # host / agent
```

## Host: serve an agent

The host SDK handles micro registration (service name `agents`), the mandatory `ack` chunk, the queue group, heartbeats, and the zero-byte terminator. You supply the `on_prompt` handler.

```python
import asyncio
import nats
from synadia_ai.agent_service import AgentService, PromptStream
from synadia_ai.agents import Envelope


async def echo(envelope: Envelope, stream: PromptStream) -> None:
    # envelope.prompt is the user text
    await stream.send(f"echo: {envelope.prompt}")
    # multiple sends stream multiple `response` chunks (e.g. token streaming)


async def main() -> None:
    nc = await nats.connect(servers="nats://127.0.0.1:4222")
    service = AgentService(
        agent="demo",            # -> agents.prompt.demo.alice.worker-1
        owner="alice",
        session_name="worker-1",
        nc=nc,
        description="demo echo agent",
    )
    service.on_prompt(echo)
    await service.start()
    try:
        await asyncio.Event().wait()     # run until cancelled
    finally:
        await service.stop()


if __name__ == "__main__":
    asyncio.run(main())
```

### Wrapping an LLM (streaming tokens)

```python
async def llm_agent(envelope: Envelope, stream: PromptStream) -> None:
    async for token in model.stream(envelope.prompt):   # your model client
        await stream.send(token)                         # one `response` chunk per token
```

Pair this with a local model (Ollama) or a hosted one (OpenRouter) — the protocol is model-agnostic; it only cares that you stream `response` chunks and let the SDK emit the terminator.

## Caller: discover and prompt

```python
import asyncio
import nats
from synadia_ai.agents import Agents, ResponseChunk, QueryChunk


async def main() -> None:
    nc = await nats.connect("nats://127.0.0.1:4222")
    agents = Agents(nc=nc)

    found = await agents.discover()          # $SRV.PING/INFO.agents
    if not found:
        raise RuntimeError("no agents on the fabric")
    agent = found[0]

    async for msg in agent.prompt("hello"):
        if isinstance(msg, ResponseChunk):
            print(msg.text, end="", flush=True)
        elif isinstance(msg, QueryChunk):
            # human-in-the-loop: answer once on the provided reply subject
            await nc.publish(msg.reply_subject, b"yes")
        # ignore unknown chunk types — forward-compat is required


if __name__ == "__main__":
    asyncio.run(main())
```

## Caller: fan out to a fleet and merge

```python
async def fan_out(agents_client, prompt_text, predicate):
    fleet = [a for a in await agents_client.discover() if predicate(a)]

    async def ask(agent):
        text = ""
        async for msg in agent.prompt(prompt_text):
            if isinstance(msg, ResponseChunk):
                text += msg.text
        return f"{agent.agent}/{agent.owner}/{agent.name}", text

    return dict(await asyncio.gather(*(ask(a) for a in fleet)))


# usage:
# results = await fan_out(agents, "Summarize the incident",
#                         predicate=lambda a: a.owner == "team-search")
```

See `patterns/meta-agent.md` for merge strategies and heartbeat-based liveness tracking.

## Notes

- **Identity → subject:** `agent` / `owner` / `session_name` produce `agents.prompt.{agent}.{owner}.{name}`. Lowercase `a–z 0–9 - _`, never leading `$`.
- **Scaling:** run multiple host processes with the same `agent`/`owner` (distinct `session_name`) — the `agents` queue group load-balances prompts across them.
- **Exact chunk class names** (`ResponseChunk`/`QueryChunk`/status) may vary by version — if an import fails, check the chunk `type` field per `concepts/protocol.md` and branch on that instead.
- **Cross-language interop is tested** — a Python caller can prompt a TypeScript host and vice versa, because both speak the same wire protocol.
