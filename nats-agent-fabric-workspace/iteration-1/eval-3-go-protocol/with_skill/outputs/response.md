# Making a Go agent compliant with the Synadia Agent Protocol

Short version: there is **no Synadia Agents Go SDK yet** (it's on the roadmap). But you don't need one. The Agent Protocol is a *wire contract*, and the SDKs (TypeScript `@synadia-ai/agent-service`, Python `synadia-ai-agent-service`) are just convenience wrappers around it. If your Go service speaks the wire protocol correctly, a TypeScript orchestrator will discover and prompt it **transparently** — it has no idea (and doesn't care) that there's no SDK behind it.

You implement the protocol directly over the `micro` package in `nats.go`. That's it.

> Spec note: this targets protocol **v0.3**. The 0.x line is explicitly unstable, so pin your NATS client versions and re-verify the protocol version string when you upgrade. The rules below are the durable parts — service name, queue group, the ack chunk, and the zero-byte terminator are what make interop work.

---

## What "compliant" actually means

For the TS orchestrator to find your Go service and stream from it, your agent must do exactly these six things:

1. **Register a `micro` service named `agents`** — the literal string `agents`. This is the discovery filter callers use (`$SRV.PING.agents`). Wrong name → invisible.
2. **Serve a `prompt` endpoint on NATS queue group `agents`** — the queue group is what load-balances prompts across multiple instances of your agent.
3. **Advertise identity metadata**: `agent`, `owner`, `name`/`session`, `protocol_version`.
4. **Emit a mandatory first chunk** `{"type":"status","data":"ack"}` on every reply, then stream `response` chunks.
5. **Terminate every stream** — success *or* error — with a **zero-byte message that has no NATS headers**. This is the single uniform "I'm done" signal.
6. **Beacon heartbeats** to `agents.hb.{agent}.{owner}.{name}` (~30s) so the orchestrator can track liveness.

A `status` request/reply endpoint (item 3.5) is recommended too — it lets a caller bootstrap your liveness immediately instead of waiting up to 30s for the next heartbeat.

### The subject namespace

Everything hangs off a verb-first namespace rooted at `agents`:

```
agents.{verb}.{agent}.{owner}.{name}
```

| Verb     | Subject                                  | Who uses it                              |
|----------|------------------------------------------|------------------------------------------|
| `prompt` | `agents.prompt.{agent}.{owner}.{name}`   | **Required.** Callers send prompts here. |
| `hb`     | `agents.hb.{agent}.{owner}.{name}`       | You publish heartbeats here (fixed subject). |
| `status` | `agents.status.{agent}.{owner}.{name}`   | On-demand liveness request/reply.        |

Pick stable, lowercase identity tokens (`a–z 0–9 - _`, never starting with `$`):
- `agent` — your harness/service identifier, e.g. `go-research`
- `owner` — operator/account, e.g. `acme`
- `name` — instance/session name to distinguish two instances of the same agent

> **Important asymmetry:** *you* (the host) build your own endpoint subjects from your identity — that's fine. But *callers* must NOT construct `prompt`/`status` subjects from identity; they learn them from `$SRV.INFO.agents`. The heartbeat subject is the one exception both sides may build directly. You don't have to worry about the caller side — the TS SDK handles discovery correctly already.

---

## The request envelope

Your `prompt` handler receives one of two forms. If the first byte is `{`, parse it as JSON; otherwise treat the whole payload as a plain-text prompt.

```json
{
  "prompt": "summarize the attached report",
  "attachments": [
    { "filename": "report.pdf", "content": "<padded-base64>" }
  ]
}
```

- `prompt` is required and non-empty; missing/empty → respond with status `400`.
- `attachments` is only valid if your endpoint advertised `"attachments_ok": "true"`. (Use padded base64, RFC 4648 §4 — not URL-safe.)

## The response stream

You publish typed JSON chunks to the request's reply subject:

```json
{ "type": "<type>", "data": <value> }
```

| `type`     | `data`                                                  | Notes |
|------------|---------------------------------------------------------|-------|
| `status`   | lifecycle string; v0.x defines `"ack"`                  | **First chunk MUST be** `{"type":"status","data":"ack"}` |
| `response` | a string, or `{ "text": ..., "attachments": [...] }`    | The answer; emit many for token streaming |
| `query`    | `{ id, reply_subject, prompt }`                          | Optional mid-stream question back to caller (human-in-the-loop) |

A correct success stream looks like:

```
1. {"type":"status","data":"ack"}          ← mandatory first chunk
2. {"type":"response","data":"Here's "}     ← zero or more response chunks
3. {"type":"response","data":"the answer."}
4. <zero-byte message, no headers>          ← terminator
```

> **The single most common compliance bug:** forgetting the terminator, or sending it with headers attached. The caller blocks (or times out) waiting for a zero-byte, header-less message. Make sure *every* return path from your handler — including errors and early returns — ends with `nc.Publish(reply, nil)`.

Errors travel as NATS micro headers, then the terminator:

```
Nats-Service-Error-Code: 429
Nats-Service-Error: rate limited
```

---

## Install

```bash
go get github.com/nats-io/nats.go
go get github.com/nats-io/nats.go/micro
```

---

## Full working agent

This is production-shaped: it registers the service, serves `prompt` (with streaming) on the `agents` queue group, serves a `status` endpoint, beacons heartbeats, handles each prompt in its own goroutine, and drains cleanly on shutdown.

A key detail: `micro`'s normal `req.Respond(...)` sends **one** reply. The Agent Protocol streams **many** messages to the reply subject and then a terminator — so we publish to `req.Reply()` ourselves rather than calling `req.Respond`.

```go
package main

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/micro"
)

// ---- Identity. Choose stable, lowercase tokens; never start with '$'. ----
const (
	agentID  = "go-research"
	ownerID  = "acme"
	nameID   = "main" // instance/session name
	protoVer = "0.3"
)

// instanceID identifies THIS process; useful in heartbeats and $SRV.INFO.<id>.
var instanceID = uuid.NewString()

// chunk is the on-the-wire response envelope.
type chunk struct {
	Type string      `json:"type"`
	Data interface{} `json:"data"`
}

// requestEnvelope is the JSON form of an inbound prompt.
type requestEnvelope struct {
	Prompt      string       `json:"prompt"`
	Attachments []attachment `json:"attachments,omitempty"`
}

type attachment struct {
	Filename string `json:"filename"`
	Content  string `json:"content"` // padded base64
}

func main() {
	nc, err := nats.Connect(
		nats.DefaultURL,
		nats.Name("go-research-agent"),
		nats.MaxReconnects(-1), // reconnect forever; the fabric outlives us
	)
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	defer nc.Drain() // flush in-flight publishes before exit

	// Metadata advertised in $SRV.INFO — this is how callers see who you are.
	meta := map[string]string{
		"agent":            agentID,
		"owner":            ownerID,
		"name":             nameID,
		"session":          nameID,
		"protocol_version": protoVer,
		"instance_id":      instanceID,
	}

	// 1. Service MUST be named exactly "agents".
	svc, err := micro.AddService(nc, micro.Config{
		Name:        "agents",
		Version:     "0.1.0",
		Description: "Go research agent (protocol-compliant, no SDK)",
		Metadata:    meta,
	})
	if err != nil {
		log.Fatalf("add service: %v", err)
	}

	// 2. prompt endpoint on the canonical subject + queue group "agents".
	promptSubj := subject("prompt")
	err = svc.AddEndpoint("prompt",
		micro.HandlerFunc(handlePrompt(nc)),
		micro.WithEndpointSubject(promptSubj),
		micro.WithEndpointQueueGroup("agents"), // load-balance across instances
		micro.WithEndpointMetadata(map[string]string{
			"max_payload":    "1MB",
			"attachments_ok": "false",
		}),
	)
	if err != nil {
		log.Fatalf("add prompt endpoint: %v", err)
	}

	// 3.5. status endpoint: lets callers bootstrap liveness on demand.
	err = svc.AddEndpoint("status",
		micro.HandlerFunc(handleStatus),
		micro.WithEndpointSubject(subject("status")),
		micro.WithEndpointQueueGroup("agents"),
	)
	if err != nil {
		log.Fatalf("add status endpoint: %v", err)
	}

	// 6. Heartbeat beacon.
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); heartbeat(ctx, nc) }()

	log.Printf("agent up: instance=%s subject=%s", instanceID, promptSubj)

	// Graceful shutdown.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	log.Println("shutting down…")
	cancel()        // stop heartbeats
	_ = svc.Stop()  // deregister from discovery, stop accepting prompts
	wg.Wait()
}

// ---- Prompt handling ----

func handlePrompt(nc *nats.Conn) micro.HandlerFunc {
	return func(req micro.Request) {
		reply := req.Reply()
		if reply == "" {
			return // no reply subject => nothing to stream to
		}

		// Parse the envelope: JSON if it starts with '{', else plain text.
		var prompt string
		raw := req.Data()
		if len(raw) > 0 && raw[0] == '{' {
			var env requestEnvelope
			if err := json.Unmarshal(raw, &env); err != nil || env.Prompt == "" {
				streamError(nc, reply, 400, "malformed request: prompt required")
				return
			}
			prompt = env.Prompt
		} else {
			prompt = string(raw)
		}
		if prompt == "" {
			streamError(nc, reply, 400, "empty prompt")
			return
		}

		// Run the actual work concurrently so one slow prompt doesn't block
		// the others delivered to this instance.
		go streamAnswer(nc, reply, prompt)
	}
}

// streamAnswer does the real agent work and streams the result.
// Replace the body with your LLM/harness call, emitting tokens as they arrive.
func streamAnswer(nc *nats.Conn, reply, prompt string) {
	// 4. Mandatory first chunk: status/ack. Send this immediately so the
	//    caller knows you accepted the work.
	send(nc, reply, chunk{Type: "status", Data: "ack"})

	// ---- your work here ----
	// Stream response chunks as tokens/segments become available:
	for _, part := range generate(prompt) {
		send(nc, reply, chunk{Type: "response", Data: part})
	}

	// 5. Terminate: zero-byte message, NO headers. ALWAYS, on every path.
	_ = nc.Publish(reply, nil)
}

// generate is a stand-in for your model/harness. Swap in a real streaming call.
func generate(prompt string) []string {
	return []string{"You said: ", prompt}
}

// ---- status endpoint: reply with the same shape as a heartbeat ----

func handleStatus(req micro.Request) {
	body, _ := json.Marshal(heartbeatPayload())
	_ = req.Respond(body) // single reply is correct here (request/reply)
}

// ---- Heartbeats ----

func heartbeat(ctx context.Context, nc *nats.Conn) {
	subj := subject("hb") // agents.hb.{agent}.{owner}.{name}
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()

	emit := func() {
		b, _ := json.Marshal(heartbeatPayload())
		_ = nc.Publish(subj, b)
	}
	emit() // beat once immediately on startup

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			emit()
		}
	}
}

func heartbeatPayload() map[string]interface{} {
	return map[string]interface{}{
		"agent":       agentID,
		"owner":       ownerID,
		"name":        nameID,
		"session":     nameID,
		"instance_id": instanceID,
		"ts":          time.Now().UTC().Format(time.RFC3339),
		"interval_s":  30,
	}
}

// ---- Helpers ----

func subject(verb string) string {
	return "agents." + verb + "." + agentID + "." + ownerID + "." + nameID
}

func send(nc *nats.Conn, subj string, c chunk) {
	b, err := json.Marshal(c)
	if err != nil {
		return
	}
	_ = nc.Publish(subj, b)
}

// streamError emits a header-carrying error message, then the empty terminator.
func streamError(nc *nats.Conn, subj string, code int, msg string) {
	m := nats.NewMsg(subj)
	m.Header.Set("Nats-Service-Error-Code", strconv.Itoa(code))
	m.Header.Set("Nats-Service-Error", msg)
	_ = nc.PublishMsg(m)
	_ = nc.Publish(subj, nil) // terminator: zero-byte, no headers
}

// uuid import note: go get github.com/google/uuid
var _ = uuid.New
```

> Dependencies: `github.com/nats-io/nats.go`, `github.com/nats-io/nats.go/micro`, and `github.com/google/uuid` (only for a stable `instance_id` — swap in any unique-ID source you prefer, or NATS's own `nuid` package).

---

## Verify it without writing a caller

Once it's running, use the `nats` CLI to confirm each compliance point:

```bash
# 1. Discovery: your agent shows up under the "agents" service.
nats req '$SRV.PING.agents' '' --replies 0

# 2. Full info incl. endpoint subjects + your metadata.
nats req '$SRV.INFO.agents' ''

# 3. Prompt it and watch the stream (ack chunk, response chunks, then it closes).
nats req 'agents.prompt.go-research.acme.main' 'hello there'

# 4. Heartbeats landing every ~30s (and one immediately on startup).
nats sub 'agents.hb.>'

# 5. On-demand status (bootstraps liveness without waiting for a beat).
nats req 'agents.status.go-research.acme.main' ''
```

If `$SRV.INFO.agents` returns your service with the `prompt` endpoint and your `agent`/`owner`/`protocol_version` metadata, the TypeScript orchestrator's discovery will see exactly the same thing.

---

## Handling a mid-stream query (optional, human-in-the-loop)

If your agent needs to ask the caller something mid-task (e.g. a confirmation) without ending the stream, emit a `query` chunk and subscribe to its `reply_subject` for the single answer:

```go
func askCaller(nc *nats.Conn, reply, question string) (string, error) {
	answerInbox := nats.NewInbox()
	sub, err := nc.SubscribeSync(answerInbox)
	if err != nil {
		return "", err
	}
	defer sub.Unsubscribe()

	send(nc, reply, chunk{Type: "query", Data: map[string]string{
		"id":            uuid.NewString(),
		"reply_subject": answerInbox,
		"prompt":        question,
	}})

	msg, err := sub.NextMsg(2 * time.Minute) // wait for the human/policy
	if err != nil {
		return "", err
	}
	return string(msg.Data), nil
	// ...then keep streaming response chunks, and terminate as usual.
}
```

The caller publishes its answer **once** to `reply_subject`, and your stream continues. You still finish with the zero-byte terminator at the end.

---

## Compliance checklist

- [ ] Service name is exactly `agents`.
- [ ] `prompt` endpoint uses queue group `agents`.
- [ ] Metadata carries `agent`, `owner`, `name`/`session`, `protocol_version`.
- [ ] First chunk of every reply is `{"type":"status","data":"ack"}`.
- [ ] **Every** path (success and error) ends with a zero-byte, header-less terminator.
- [ ] Errors set `Nats-Service-Error-Code` / `Nats-Service-Error` headers *before* the terminator.
- [ ] Heartbeat published to `agents.hb.{agent}.{owner}.{name}` (~30s), plus one on startup.
- [ ] (Recommended) `status` endpoint replies with the heartbeat JSON shape.

Get these right and your Go service is **indistinguishable from an SDK-backed agent** to any caller. The TypeScript orchestrator discovers it via `$SRV`, prompts it, and consumes the chunk stream transparently — no Go SDK required. When the official Go SDK eventually ships, it will encapsulate exactly this boilerplate, so your protocol-level understanding carries straight over.

---

### A few things to keep in mind

- **Multi-tenancy comes from NATS accounts, not your code.** If you need tenant isolation, separate agents into different NATS accounts rather than filtering in the handler. (That's an infra/deployment concern — out of scope here.)
- **Horizontal scale is free:** run N copies of this binary with the same identity, and the `agents` queue group spreads prompts across them automatically.
- **State is optional and not in the transport.** The v0.3 protocol is stateless micro request/reply. If your agent needs durable memory or session handoff, layer JetStream/KV on top separately — it's not required for protocol compliance.
- **Pin and re-verify.** The 0.x protocol is unstable; new chunk `type`s may appear. On the *caller* side you must ignore unknown types — but as a host, just keep emitting the types you know and keep `protocol_version` accurate.
