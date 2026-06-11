# Synadia Agent Protocol — Wire Contract (spec v0.3)

This is the interoperability contract. Any agent or caller — SDK-based or hand-rolled — must honor it exactly, because the whole value proposition is that a caller can talk to an agent it has never seen. Get the subjects, the envelope, the chunk format, and the stream terminator right; everything else is implementation detail.

Spec source: <https://github.com/synadia-ai/synadia-agent-sdk-docs> (`core-protocol.md`).

## Table of contents

- Subject hierarchy & the four verbs
- Service registration (the discovery anchor)
- Identity tokens
- Request envelope
- Response streaming & chunk types
- Stream termination (the critical rule)
- Mid-stream queries (human-in-the-loop)
- Heartbeats & the status endpoint
- Discovery via `$SRV`
- Errors
- Versioning

## Subject hierarchy & the four verbs

A verb-first namespace rooted at `agents`:

```
agents.{verb}.{agent}.{owner}.{name}
```

| Verb          | Default subject                              | Purpose                                            |
|---------------|----------------------------------------------|----------------------------------------------------|
| `prompt`      | `agents.prompt.{agent}.{owner}.{name}`       | **Required** request endpoint                      |
| `hb`          | `agents.hb.{agent}.{owner}.{name}`           | Liveness beacon — **subject is protocol-fixed**    |
| `status`      | `agents.status.{agent}.{owner}.{name}`       | On-demand status request/reply                     |
| `attachments` | `agents.attachments.{agent}.{owner}.{name}`  | Reserved for future chunked uploads                |

Example concrete subjects for an agent `claude-code`, owner `aconnolly`, name/session `synadia-com-2`:

```
agents.prompt.cc.aconnolly.synadia-com-2
agents.hb.cc.aconnolly.synadia-com-2
agents.status.cc.aconnolly.synadia-com-2
```

> **Do not build these subjects from identity in caller code.** The heartbeat subject is the *only* one you may construct directly. For `prompt`/`status`, learn the actual subject from the discovery (`$SRV.INFO.agents`) response and address what it gives you. This indirection is deliberate — it lets agents relocate endpoints without breaking callers.

## Service registration (the discovery anchor)

Every agent MUST register as a NATS micro-service (`@nats-io/services`, the Python equivalent, or `micro` in `nats.go`) with:

- **Service name** = `"agents"` — this exact string is the discovery filter. Wrong name = invisible to callers.
- **Queue group on the `prompt` endpoint** = `"agents"` — enables automatic load-balancing across instances of the same agent.
- **Metadata** advertised on the service / endpoint:

```json
{
  "agent": "claude-code",
  "owner": "aconnolly",
  "session": "synadia-com-2",
  "protocol_version": "0.3"
}
```

The `prompt` endpoint additionally advertises its capabilities, e.g.:

```json
{ "max_payload": "1MB", "attachments_ok": true }
```

Callers should pre-flight-validate payload size and attachment use against these before sending.

## Identity tokens

`agent`, `owner`, `name` (and optional `session`) compose both the subject namespace and the discovery metadata.

- Use `a–z 0–9 - _`. Tokens MUST NOT begin with `$`.
- `agent` — canonical harness identifier (`claude-code`, `pi`, `hermes`, your service name).
- `owner` — operator/account.
- `name` — instance/session name distinguishing two instances of the same agent under the same owner.
- `session` — present in metadata when the agent is session-aware.

## Request envelope

Two accepted forms. If the first byte is `{`, it's parsed as JSON; otherwise it's treated as a plain-text prompt.

**Plain-text shorthand** (equivalent to `{"prompt": "..."}`):

```
summarize the attached report
```

**JSON envelope:**

```json
{
  "prompt": "summarize the attached report",
  "attachments": [
    { "filename": "report.pdf", "content": "<base64>" }
  ]
}
```

- `prompt` is required and non-empty; missing/empty → status `400`.
- `attachments` is valid only when the endpoint advertised `"attachments_ok": true`.
- Attachment `content` is standard **padded base64 (RFC 4648 §4, not URL-safe)**.

## Response streaming & chunk types

The agent streams typed chunks to the reply subject. Each chunk is a JSON object:

```json
{ "type": "<type>", "data": <value> }
```

| `type`     | `data`                                              | Notes                                              |
|------------|-----------------------------------------------------|----------------------------------------------------|
| `status`   | a lifecycle string; v0.x defines `"ack"`            | **First chunk MUST be** `{"type":"status","data":"ack"}` |
| `response` | a string, **or** `{ "text": ..., "attachments": [...] }` | The actual answer; may arrive in many chunks (token streaming) |
| `query`    | `{ id, reply_subject, prompt }`                     | Mid-stream question back to the caller (see below) |

**Forward-compatibility (required):** callers MUST silently ignore unknown chunk `type`s and preserve unknown fields. The protocol will add types within 0.x; a caller that throws on an unknown type is non-compliant.

## Stream termination (the critical rule)

> Every stream — success or error — ends with a **zero-byte message carrying no NATS headers**. This single, uniform end-of-stream signal is how a caller knows the agent is done, regardless of outcome.

For an **error-terminated** stream: send a message bearing the error headers (below), then send the empty, header-less terminator.

A correct success stream therefore looks like:

```
1. {"type":"status","data":"ack"}        # mandatory first chunk
2. {"type":"response","data":"Here's "}  # zero or more response chunks
3. {"type":"response","data":"the answer."}
4. <zero-byte message, no headers>        # terminator
```

## Mid-stream queries (human-in-the-loop)

An agent can pause and ask the caller a question without ending the stream:

```json
{
  "type": "query",
  "data": {
    "id": "a8f1c2e4-...",
    "reply_subject": "_INBOX.Xj7k9Q2pA",
    "prompt": "Confirm deletion of 200 files? (yes/no)"
  }
}
```

The caller publishes its answer **once** to `reply_subject`, and the agent continues streaming. Build callers to detect `query` chunks and route them to a human (or an automated policy) rather than treating them as response text.

## Heartbeats & the status endpoint

Agents publish a liveness beacon periodically to the fixed subject `agents.hb.{agent}.{owner}.{name}`:

```json
{
  "agent": "claude-code",
  "owner": "aconnolly",
  "session": "synadia-com-2",
  "instance_id": "VMKS6MHK71PCPWGY38A7N5",
  "ts": "2026-04-28T14:23:01Z",
  "interval_s": 30
}
```

- **Recommended interval:** 30s. **Offline threshold:** 3× interval since the last beat.
- The `status` request/reply endpoint replies with the **same JSON shape** as a heartbeat, so a caller can bootstrap liveness immediately instead of waiting for the next beacon.

## Discovery via `$SRV`

Standard NATS micro discovery — no custom mechanism:

- `$SRV.PING.agents` — every compliant agent responds once; use to enumerate the fleet.
- `$SRV.INFO.agents` — full service info (multi-response), including endpoint subjects and metadata.
- `$SRV.INFO.agents.{instance_id}` — info for one specific instance.

From the CLI:

```bash
nats req '$SRV.PING.agents' '' --replies 0      # enumerate all agents
nats req '$SRV.INFO.agents' ''                  # full info incl. endpoint subjects
```

Callers MUST learn endpoint subjects from these responses and MUST NOT construct them from identity alone (heartbeat subject excepted).

## Errors

Errors travel as NATS micro-service **headers**, optionally with a JSON body:

```
Nats-Service-Error-Code: 429
Nats-Service-Error: rate limited
```

```json
{ "error": "rate_limited", "message": "rate limited", "retry_after_s": 5 }
```

Status taxonomy: `400` malformed request · `401` auth required · `403` forbidden · `404` not found · `409` conflict · `429` rate limited · `500` internal error.

An error stream sends the error-headered message, then the zero-byte terminator.

## Versioning

`protocol_version` is a `"MAJOR.MINOR"` string (e.g. `"0.3"`). The 0.x line is explicitly unstable — pin and verify. Design for additive change: tolerate new chunk types and new metadata fields.
