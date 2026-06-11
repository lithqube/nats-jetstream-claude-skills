# Exposing a Python LLM Agent over NATS with the Synadia Agent Host SDK

You want four things, all at once:

1. **Discovery** — other services can find your agent without hard‑coding addresses.
2. **Prompt → answer over NATS** — request/response over the message bus rather than HTTP.
3. **Token streaming** — the answer comes back incrementally, token by token, not as one blob.
4. **Human‑in‑the‑loop (HITL)** — the agent can suspend itself, ask a human to approve a destructive action, and resume only after it gets a yes.

The Synadia **agent host** (the `agentp`/agent‑host runtime that Synadia ships for hosting agents on NATS) gives you primitives for all four. Below is a complete, runnable pattern. Because the host SDK's surface is still evolving, I show the canonical agent‑host pattern **and** the equivalent built directly on the NATS Python client (`nats-py`) so you have a fallback that works no matter which SDK version you have pinned. The two are interoperable — the host SDK is essentially a thin, opinionated wrapper over the same NATS subjects.

---

## Mental model

Think of the agent host as a small framework that:

- **Registers a "service"** on NATS using the NATS *services API* (the `$SRV.*` micro framework). This is what makes your agent *discoverable*: any client can send `$SRV.PING`, `$SRV.INFO`, `$SRV.STATS` and the host answers, advertising your agent's name, version, description, and the subjects it listens on.
- **Subscribes to a request subject** (e.g. `agent.my-llm-agent.prompt`) where callers send prompts.
- **Streams tokens back** by publishing many messages to a per‑request `reply` (inbox) subject as the LLM generates them, terminating with a sentinel/“done” message.
- **Pauses for HITL** by publishing a *confirmation request* to a well‑known subject and doing a NATS *request* that blocks (with a timeout) until a human responder approves or denies.

Everything is plain NATS subjects under the hood, which is why discovery, streaming, and HITL all compose cleanly.

---

## 1. Install

```bash
pip install nats-py        # NATS Python client (provides micro/services API + JetStream)
# If you have the Synadia host SDK available in your environment, also:
# pip install synadia-agent-host    # name varies by distribution; the wrapper is optional
```

You also need a running NATS server. For HITL you'll want JetStream enabled so pending‑approval requests survive a restart:

```bash
nats-server -js
```

---

## 2. The agent: discoverable, streaming, HITL‑aware

```python
import asyncio
import json
import os
import uuid

import nats
from nats.aio.msg import Msg
from nats.micro import add_service          # NATS "micro"/services API → discovery
from nats.micro.request import Request

# --- Your LLM. Swap in whatever you actually use (Anthropic, OpenAI, local, etc.) ---
# The only contract the host cares about is: given a prompt, yield text chunks.
class LLM:
    async def stream(self, prompt: str):
        """Async-generate the answer token by token."""
        # Example with the Anthropic SDK (async streaming):
        #
        #   from anthropic import AsyncAnthropic
        #   client = AsyncAnthropic()
        #   async with client.messages.stream(
        #       model="claude-3-7-sonnet-latest",
        #       max_tokens=1024,
        #       messages=[{"role": "user", "content": prompt}],
        #   ) as stream:
        #       async for text in stream.text_stream:
        #           yield text
        #
        # Stub so this file runs standalone:
        for word in f"Echoing your prompt: {prompt}".split():
            await asyncio.sleep(0.05)
            yield word + " "


# Subjects we use. Keep them namespaced per agent so many agents can share one NATS.
AGENT_NAME = "my-llm-agent"
PROMPT_SUBJECT = f"agent.{AGENT_NAME}.prompt"          # callers send prompts here
CONFIRM_SUBJECT = f"agent.{AGENT_NAME}.confirm"        # HITL approvals flow here


class AgentHost:
    def __init__(self, nc, llm: LLM):
        self.nc = nc
        self.llm = llm

    # ----- HITL: pause and ask a human before destructive actions -----
    async def request_human_confirmation(self, action: dict, timeout: float = 300.0) -> bool:
        """
        Publish a confirmation request and BLOCK until a human approves/denies.
        Returns True if approved. Uses NATS request/reply so the call naturally
        suspends the agent's coroutine until an answer arrives or it times out.
        """
        payload = {
            "id": str(uuid.uuid4()),
            "agent": AGENT_NAME,
            "action": action,                 # e.g. {"type": "delete", "target": "prod-db"}
            "prompt": f"Approve destructive action: {action}?",
        }
        try:
            # A human-facing service (UI, Slack bot, CLI) subscribes to CONFIRM_SUBJECT,
            # shows the request to an operator, and replies with {"approved": bool}.
            resp = await self.nc.request(
                CONFIRM_SUBJECT,
                json.dumps(payload).encode(),
                timeout=timeout,
            )
            decision = json.loads(resp.data)
            return bool(decision.get("approved", False))
        except asyncio.TimeoutError:
            # No human answered in time → fail safe (deny).
            return False

    async def is_destructive(self, prompt: str) -> bool:
        # Replace with your real policy / tool-call inspection.
        return any(k in prompt.lower() for k in ("delete", "drop", "rm ", "wipe", "destroy"))

    # ----- Streaming prompt handler -----
    async def handle_prompt(self, req: Request):
        data = json.loads(req.data())
        prompt = data["prompt"]
        # Per-request streaming subject. Caller subscribes to it before sending,
        # OR we just stream to req.reply (the auto-generated inbox).
        reply = data.get("stream_to") or req.msg.reply

        # 1) HITL gate
        if await self.is_destructive(prompt):
            await self.nc.publish(
                reply, json.dumps({"type": "awaiting_confirmation"}).encode()
            )
            approved = await self.request_human_confirmation(
                {"type": "destructive", "prompt": prompt}
            )
            if not approved:
                await self.nc.publish(
                    reply,
                    json.dumps({"type": "denied",
                                "message": "Human declined the destructive action."}).encode(),
                )
                await self.nc.publish(reply, json.dumps({"type": "done"}).encode())
                # Also ack the original request so the caller's request() resolves.
                await req.respond(json.dumps({"status": "denied"}).encode())
                return

        # 2) Stream tokens
        async for token in self.llm.stream(prompt):
            await self.nc.publish(
                reply, json.dumps({"type": "token", "text": token}).encode()
            )
        await self.nc.publish(reply, json.dumps({"type": "done"}).encode())

        # 3) Acknowledge the request itself (so a plain request() also gets a final reply)
        await req.respond(json.dumps({"status": "complete"}).encode())


async def main():
    nc = await nats.connect(os.getenv("NATS_URL", "nats://localhost:4222"))
    host = AgentHost(nc, LLM())

    # --- Discovery: register as a NATS micro service ---
    # This auto-wires $SRV.PING / $SRV.INFO / $SRV.STATS so other services can
    # discover this agent (name, version, description, endpoints) without config.
    svc = await add_service(
        nc,
        name=AGENT_NAME,
        version="1.0.0",
        description="LLM agent: streaming prompts over NATS with human-in-the-loop.",
    )
    group = svc.add_group(f"agent.{AGENT_NAME}")
    await group.add_endpoint("prompt", handler=host.handle_prompt)

    print(f"{AGENT_NAME} is up. Listening on {PROMPT_SUBJECT}")
    print("Discover it with:  nats micro info " + AGENT_NAME)
    await asyncio.Event().wait()   # run forever


if __name__ == "__main__":
    asyncio.run(main())
```

> If you are using the Synadia **agent host SDK** wrapper, the registration/streaming/HITL pieces are exposed as decorators/helpers (names vary by version), roughly:
>
> ```python
> from synadia.agent_host import AgentHost, hitl
>
> host = AgentHost(name="my-llm-agent", version="1.0.0",
>                  description="LLM agent over NATS")
>
> @host.prompt_handler()
> async def handle(ctx, prompt: str):
>     if await is_destructive(prompt):
>         # ctx.confirm() suspends until a human approves; raises if denied.
>         await ctx.confirm(action={"type": "destructive", "prompt": prompt})
>     async for token in llm.stream(prompt):
>         await ctx.emit_token(token)     # streams back to the caller
>
> await host.run()   # registers discovery endpoints + starts serving
> ```
>
> The wrapper does exactly what the explicit version above does: it registers the micro service for discovery, turns `emit_token` into per‑request publishes on the reply inbox, and turns `ctx.confirm()` into a blocking NATS request to a confirmation subject. If your installed SDK uses slightly different names (`@host.endpoint`, `ctx.stream(...)`, `ctx.require_approval(...)`), keep the *shape* and adjust the call names.

---

## 3. A client that discovers the agent and consumes the token stream

```python
import asyncio, json, uuid
import nats

async def main():
    nc = await nats.connect("nats://localhost:4222")

    # --- Discover available agents (services API) ---
    # $SRV.PING returns one message per running service instance.
    info = await nc.request("$SRV.INFO.my-llm-agent", b"", timeout=2)
    print("Discovered:", json.loads(info.data)["name"])

    # --- Subscribe to a per-request stream subject first ---
    stream_subj = f"agent.stream.{uuid.uuid4().hex}"
    sub = await nc.subscribe(stream_subj)

    # --- Send the prompt, telling the agent where to stream ---
    await nc.publish(
        "agent.my-llm-agent.prompt",
        json.dumps({"prompt": "Summarize the NATS services API", "stream_to": stream_subj}).encode(),
    )

    # --- Consume tokens until 'done' ---
    async for msg in sub.messages:
        evt = json.loads(msg.data)
        if evt["type"] == "token":
            print(evt["text"], end="", flush=True)
        elif evt["type"] == "awaiting_confirmation":
            print("\n[agent is waiting on human approval...]")
        elif evt["type"] == "denied":
            print("\n[denied]:", evt["message"]); break
        elif evt["type"] == "done":
            print("\n[done]"); break

    await nc.drain()

asyncio.run(main())
```

---

## 4. The human approver (the "in‑the‑loop" part)

This is any process a person watches — a CLI, a Slack bot, a web dashboard. It subscribes to the confirmation subject, surfaces the request to a human, and replies with the decision:

```python
import asyncio, json
import nats

async def main():
    nc = await nats.connect("nats://localhost:4222")

    async def on_confirm(msg):
        req = json.loads(msg.data)
        print(f"\nAPPROVAL NEEDED: {req['prompt']}")
        answer = input("Approve? [y/N] ").strip().lower() == "y"
        await msg.respond(json.dumps({"approved": answer}).encode())

    await nc.subscribe("agent.my-llm-agent.confirm", cb=on_confirm)
    print("Approver running. Waiting for confirmation requests...")
    await asyncio.Event().wait()

asyncio.run(main())
```

Because the agent used `nc.request(...)` with a timeout, its coroutine is genuinely *suspended* at the confirmation point — it consumes no LLM tokens and takes no destructive action until a human replies (or it times out and fails safe to "denied").

---

## How each requirement is satisfied

| Requirement | Mechanism |
|---|---|
| **Discovery** | The agent registers via the NATS **services (micro) API**. Other services find it with `$SRV.PING` / `$SRV.INFO` (or `nats micro info my-llm-agent`) — no hard‑coded addresses, and it works across a cluster. |
| **Prompt → answer over NATS** | Callers publish to `agent.my-llm-agent.prompt`. Subject‑based addressing means any number of instances can load‑balance the same subject via a queue group. |
| **Token streaming** | The agent publishes one message per token to a per‑request reply/inbox subject, ending with a `{"type":"done"}` sentinel. The client subscribes first, then reads the stream. This is the idiomatic NATS streaming pattern (many publishes on one inbox). |
| **Human‑in‑the‑loop** | Before any destructive action the agent does a blocking `nc.request()` to `agent.my-llm-agent.confirm`. A human‑facing responder replies `{"approved": true/false}`. A timeout fails safe to "deny". |

---

## Production hardening notes

- **Use JetStream for durable HITL.** Plain request/reply confirmations are lost if the approver is offline. Publish confirmation requests to a **JetStream stream** with a durable consumer so a pending approval survives restarts, and correlate the agent's resume on the request `id`. This turns "pause for a human" from best‑effort into durable workflow state.
- **Scale out with queue groups.** Register multiple agent instances on the same subject in a queue group; NATS load‑balances prompts across them. Discovery (`$SRV.PING`) will then return one entry per instance, which is how you see health/throughput per replica.
- **Backpressure / large answers.** Token‑per‑message is simple but chatty. For very high token rates, batch a few tokens per message, or stream into a JetStream subject the client tails.
- **Timeouts & cancellation.** Give `request_human_confirmation` a sane timeout (e.g. 5 min) and publish a `cancelled` event so the client UI can stop spinning. Consider a separate `agent.my-llm-agent.cancel` subject so a caller can abort an in‑flight generation.
- **AuthN/AuthZ.** Use NATS accounts and subject‑level permissions so only authorized callers can publish to `agent.*.prompt`, and only your approver service can reply on `agent.*.confirm`. This is essential given the HITL approvals gate destructive operations.
- **Structured events.** Keep the streamed envelope (`{"type": ...}`) stable and versioned — `token`, `awaiting_confirmation`, `denied`, `error`, `done` — so multiple client UIs can interoperate.

This gives you a discoverable, streaming, human‑gated LLM agent that speaks pure NATS, whether you drive it through the Synadia agent host SDK wrapper or the raw `nats-py` client underneath it.
