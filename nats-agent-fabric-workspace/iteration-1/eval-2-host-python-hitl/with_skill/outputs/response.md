# Exposing a Python LLM agent on NATS with the Synadia agent host SDK

What you're describing is exactly the shape the **Synadia Agent Protocol for NATS** is built for: an agent that other services can *discover* (without knowing its address in advance), *prompt*, get a *token-by-token* streamed answer from, and that can *pause mid-stream to ask a human* before doing something destructive.

You'll use the **host SDK** — `synadia-ai-agent-service` (PyPI) — which embeds an `AgentService` in your process. You supply an `on_prompt` handler; the SDK handles all the protocol plumbing: registering your agent as a NATS micro-service named `agents`, joining the `agents` queue group, emitting the mandatory `ack` chunk, publishing heartbeats, and writing the zero-byte stream terminator. Your three requirements map cleanly onto protocol features:

| Requirement | How the protocol delivers it |
|---|---|
| Other services **discover** the agent | You register as micro-service name `agents`; callers find you via `$SRV.PING.agents` / `$SRV.INFO.agents`. No endpoint sharing needed. |
| **Stream the answer token by token** | Each `await stream.send(token)` emits one `response` chunk; the SDK terminates the stream when your handler returns. |
| **Pause and ask a human** before destructive actions | Emit a `query` chunk mid-stream; the caller routes it to a human and publishes the answer back; your handler resumes. |

> **Version note.** The protocol spec is **v0.3** and the 0.x SDK line is explicitly unstable. As of mid-2026, `synadia-ai-agent-service` is at **0.4.1** (depends on `synadia-ai-agents>=0.6`), needs **Python 3.11–3.13**, and a reachable NATS server. Treat exact class/method names below as *current-but-verify* — confirm against your installed version. The **wire protocol** (subjects, the `ack`-first rule, chunk types, the zero-byte terminator) is the stable contract; if a symbol drifts, honor the protocol and adjust the call.

---

## 1. Install and the moving parts

```bash
pip install synadia-ai-agent-service   # the host SDK (you build a host)
# (callers that prompt you would install synadia-ai-agents — not needed to host)
```

Three things you'll touch:

- **`AgentService`** — registers your agent and runs it. You give it identity tokens and a NATS connection.
- **`on_prompt` handler** — `async (envelope, stream) -> None`. `envelope.prompt` is the user's text; `stream.send(...)` emits `response` chunks.
- **A mid-stream query** — the mechanism to pause and ask the caller (your human) a yes/no question. Below it's wrapped in a small `confirm()` helper.

**Identity** is the tuple `agent` / `owner` / `name` (+ optional `session`). These compose both your subject namespace (`agents.prompt.{agent}.{owner}.{name}`) and your discovery metadata, so pick stable, lowercase tokens (`a–z 0–9 - _`, never leading `$`). For an LLM assistant owned by `acme`, running as instance `prod-1`, that's `agent="llm-assistant"`, `owner="acme"`, `name="prod-1"`.

---

## 2. Minimal host (discovery + token streaming)

Start here to prove discovery and streaming work, then add HITL in §3. This wraps any streaming model client — swap `model.stream(...)` for your Anthropic/OpenAI/Ollama/etc. streaming call.

```python
import asyncio
import nats
from synadia_ai.agent_service import AgentService, PromptStream
from synadia_ai.agents import Envelope

# --- your LLM client (replace with whatever you use) -------------------------
class Model:
    async def stream(self, prompt: str):
        # yield text deltas as they arrive from your model
        for tok in ("Here ", "is ", "the ", "answer."):
            await asyncio.sleep(0.05)
            yield tok

model = Model()
# -----------------------------------------------------------------------------


async def on_prompt(envelope: Envelope, stream: PromptStream) -> None:
    # envelope.prompt is the caller's text.
    # Each send() emits one `response` chunk -> the caller sees tokens live.
    async for token in model.stream(envelope.prompt):
        await stream.send(token)
    # When this coroutine returns, the SDK writes the zero-byte terminator
    # that tells the caller the stream is complete.


async def main() -> None:
    nc = await nats.connect("nats://127.0.0.1:4222")
    service = AgentService(
        agent="llm-assistant",     # -> agents.prompt.llm-assistant.acme.prod-1
        owner="acme",
        session_name="prod-1",
        nc=nc,
        description="LLM assistant; streams tokens, asks before destructive ops",
    )
    service.on_prompt(on_prompt)
    await service.start()          # registers as micro-service `agents`, starts heartbeats
    try:
        await asyncio.Event().wait()   # run until cancelled
    finally:
        await service.stop()


if __name__ == "__main__":
    asyncio.run(main())
```

The moment `start()` returns, any caller on the bus can find you:

```bash
nats req '$SRV.PING.agents' '' --replies 0   # enumerate every agent, including yours
nats req '$SRV.INFO.agents' ''               # full info incl. your prompt endpoint subject
```

Callers learn your `prompt` subject from that `$SRV.INFO` response — they never construct it from your identity. That indirection is deliberate: it lets you relocate endpoints without breaking anyone.

---

## 3. Human-in-the-loop: pause before destructive actions

This is the core of your requirement. Before your agent does anything destructive (delete files, drop a table, send money, etc.), it emits a **`query` chunk** instead of a `response` chunk. A `query` carries an `id`, a `reply_subject`, and a `prompt`:

```json
{ "type": "query",
  "data": { "id": "a8f1c2e4-…", "reply_subject": "_INBOX.Xj7…", "prompt": "Confirm deletion of 200 files? (yes/no)" } }
```

The stream **stays open**. The caller routes this to a human (or an approval policy), publishes the answer **once** to `reply_subject`, and your handler resumes. The host SDK typically exposes this as a single awaitable on the stream (`stream.query(...)`/`stream.ask(...)`) that allocates the inbox, emits the chunk, and waits for the one reply. If your installed version names it differently, the protocol-level fallback is in §5 — same wire behavior either way.

```python
import asyncio
import nats
from synadia_ai.agent_service import AgentService, PromptStream
from synadia_ai.agents import Envelope

DESTRUCTIVE_TIMEOUT_S = 120  # how long a human has to approve


async def confirm(stream: PromptStream, question: str) -> bool:
    """Pause the stream and ask the caller's human to approve. Returns True on 'yes'."""
    try:
        # Emits a `query` chunk and waits for the single reply on its reply_subject.
        answer = await asyncio.wait_for(stream.query(question), timeout=DESTRUCTIVE_TIMEOUT_S)
    except asyncio.TimeoutError:
        return False   # no answer in time -> treat as "not approved"
    return answer.strip().lower() in ("y", "yes", "approve", "confirm")


async def on_prompt(envelope: Envelope, stream: PromptStream) -> None:
    # 1) Stream the model's reasoning/plan as normal response tokens.
    async for token in model.stream(envelope.prompt):
        await stream.send(token)

    # 2) When the agent decides to do something destructive, gate it on a human.
    if plan_includes_destructive_step(envelope.prompt):  # your own policy check
        approved = await confirm(
            stream,
            "This will permanently delete 200 files. Approve? (yes/no)",
        )
        if not approved:
            await stream.send("\n[aborted — destructive action not approved]")
            return
        # approved: now perform the action, streaming progress as response chunks
        async for token in perform_destructive_action():
            await stream.send(token)
    # handler returns -> SDK emits the zero-byte terminator


async def main() -> None:
    nc = await nats.connect("nats://127.0.0.1:4222")
    service = AgentService(
        agent="llm-assistant",
        owner="acme",
        session_name="prod-1",
        nc=nc,
        description="LLM assistant with human approval on destructive ops",
    )
    service.on_prompt(on_prompt)
    await service.start()
    try:
        await asyncio.Event().wait()
    finally:
        await service.stop()


if __name__ == "__main__":
    asyncio.run(main())
```

### What the caller sees (for context)

The caller distinguishes a `query` from a `response` by the chunk `type` and answers it once. A caller using `synadia-ai-agents` looks like this — it's *not* part of your host, but it shows the other half of the HITL handshake so the contract is clear:

```python
from synadia_ai.agents import Agents, ResponseChunk, QueryChunk

found = await agents.discover()           # finds your agent via $SRV
agent = next(a for a in found if a.agent == "llm-assistant")

async for msg in agent.prompt("clean up the temp directory"):
    if isinstance(msg, ResponseChunk):
        print(msg.text, end="", flush=True)        # tokens, live
    elif isinstance(msg, QueryChunk):
        decision = input(f"\n[agent asks] {msg.prompt} ")   # route to a human
        await nc.publish(msg.reply_subject, decision.encode())
    # ignore unknown chunk types — forward-compat is required
```

That's the full loop: the human's `yes`/`no` flows back on `reply_subject`, your `confirm()` returns, and the stream continues.

---

## 4. Why this satisfies all three requirements

- **Discoverable** — `AgentService` registers as micro-service name **`agents`** and joins the **`agents` queue group** on the `prompt` endpoint. Callers enumerate the fleet with `$SRV.PING.agents` and read your actual endpoint subject + metadata from `$SRV.INFO.agents`. You inherit NATS accounts (multi-tenant isolation — including who can even *see* you in discovery), cloud-to-edge reach, and a message-level audit trail because every prompt/response is a NATS message on `agents.>`.
- **Token streaming** — every `stream.send(token)` is one `response` chunk; the protocol mandates the stream open with `{"type":"status","data":"ack"}` and close with a zero-byte, header-less terminator — both handled by the SDK. The caller consumes chunks as an async iterator and renders tokens as they land.
- **Human-in-the-loop** — the `query` chunk pauses *without* ending the stream; the caller answers once on `reply_subject`; you resume. This is a first-class protocol verb, not a side channel, so any compliant caller (even one in TypeScript) can satisfy your approval prompt.

**Scale-out is free:** run several host processes with the same `agent`/`owner` and distinct `session_name`s — the `agents` queue group load-balances prompts across them automatically. No router, no config.

---

## 5. Protocol fallback (if the SDK symbol names differ)

Because the SDK is 0.x, the exact helper for a mid-stream query may be named `query` / `ask` / `prompt_caller` depending on your version, and the chunk classes may be `ResponseChunk`/`QueryChunk` or plain dicts. The **wire protocol is the stable contract** — branch on the chunk `type` field. If `stream.query(...)` doesn't exist, you can emit the query and await the reply yourself:

```python
import uuid

async def confirm_raw(nc, stream, question: str) -> bool:
    inbox = nc.new_inbox()                     # unique _INBOX.* reply subject
    sub = await nc.subscribe(inbox)
    # emit a raw `query` chunk through the stream's low-level send if available,
    # otherwise publish to the reply subject the SDK gave your handler:
    await stream.send_chunk({                   # type/name may vary; see your version
        "type": "query",
        "data": {"id": str(uuid.uuid4()), "reply_subject": inbox, "prompt": question},
    })
    try:
        reply = await asyncio.wait_for(sub.next_msg(), timeout=DESTRUCTIVE_TIMEOUT_S)
        return reply.data.decode().strip().lower() in ("y", "yes")
    except asyncio.TimeoutError:
        return False
    finally:
        await sub.unsubscribe()
```

Key invariants to preserve no matter the API surface:

- Service name **must** be `agents`; the `prompt` endpoint **must** use queue group `agents`.
- The stream **must** start with `{"type":"status","data":"ack"}` (SDK does this) and **must** end with a **zero-byte message and no headers** (SDK does this when your handler returns or raises).
- A `query` keeps the stream **open**; the caller replies exactly **once** to `reply_subject`.
- Callers must ignore unknown chunk types — so you're free to add new types later without breaking them.

---

## 6. Practical hardening

- **Default-deny on no answer.** Time out the `confirm()` and treat silence as *not approved* (shown above). A destructive action should never proceed because nobody answered.
- **Gate on a real policy, not a keyword.** `plan_includes_destructive_step(...)` should reflect your agent's actual tool/plan (e.g. the tool call it's about to make), not a substring of the user's prompt.
- **Heartbeats are automatic.** The SDK beacons to `agents.hb.{agent}.{owner}.{name}` (~30s); callers mark you offline after 3× the interval. Nothing to do unless you want to customize the interval.
- **Multi-tenancy via NATS accounts**, not app logic. Put different owners/tenants in different NATS accounts so the bus enforces isolation. Configuring accounts/TLS/authz on the servers is a deployment concern — see the `jetstream-deployment` guidance.
- **Durable memory/session handoff is optional and on the roadmap** (JetStream + KV). The v0.3 transport here is stateless micro request/reply; only reach for streams/KV if your agent needs to *remember* across prompts or hand a task to another agent.
- **Confirm your installed version** before shipping (`pip show synadia-ai-agent-service`). If a method name differs from this guide, the §5 protocol fallback gets you the same wire behavior.
