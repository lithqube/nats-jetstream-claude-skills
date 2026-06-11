# Making a Go Agent Speak the Synadia Agent Protocol

Since there's no official Synadia Go SDK for the agent protocol yet, the good news is that the protocol is "just NATS underneath." The TypeScript orchestrator discovers and talks to agents over plain NATS subjects, request/reply, and (for streaming) ordinary publishes plus an optional JetStream stream. The Go side already has a first-class, production-grade NATS client (`nats.go`) plus the **NATS Micro** framework (`micro` package), which implements exactly the service-registration + discovery + monitoring contract that an agent protocol layers on top of.

So your job is to:

1. Register your Go process as a **NATS service** (so it's discoverable via `$SRV.*` pings).
2. Expose the **agent endpoints/subjects** the orchestrator expects (`describe`/`info`, `prompt`/`invoke`).
3. Support **streaming** token output back to the caller while a request is in flight.
4. Emit **heartbeats / health** so the orchestrator knows the agent is alive.

Below is the reasoning for each piece and then a complete, runnable Go program.

> Note on the "protocol contract": because Synadia hasn't published a stable Go SDK, the exact subject names and JSON field names are the part you must confirm against whatever your TypeScript orchestrator actually sends. I've used the conventional layout (`agents.<id>.*` subjects, a JSON envelope with `id`/`type`/`payload`, SSE-style streaming chunks). Treat the subject/JSON constants near the top of the file as the **one place to adjust** if your orchestrator differs. Everything else — service registration, discovery, streaming mechanics, heartbeats — is mechanically the same regardless of the exact names.

---

## The four things your Go agent must do

### 1. Register as a discoverable service

NATS Micro adds a discovery/monitoring contract on top of NATS. When you create a service with the `micro` package, the runtime automatically subscribes your process to the reserved control subjects:

- `$SRV.PING` and `$SRV.PING.<name>` — discovery: "who's out there?"
- `$SRV.INFO.<name>` — returns the service's metadata and endpoints.
- `$SRV.STATS.<name>` — returns request counts, errors, processing time.

This is the mechanism the orchestrator uses to **discover** your agent without you writing any discovery code yourself. You attach `Metadata` to the service so the orchestrator can tell "this is an agent, here's its model, its capabilities, its protocol version."

### 2. The agent subjects

An agent typically exposes two logical operations:

- **describe / info** — a request/reply endpoint that returns the agent card (id, name, description, capabilities, input/output schema). The orchestrator calls this to know how to prompt the agent.
- **prompt / invoke** — the endpoint that actually does the work. The orchestrator sends a prompt; the agent runs it and replies (and/or streams).

A clean, queue-balanced subject layout:

```
agents.<agent-id>.describe     # request/reply: returns the agent card
agents.<agent-id>.prompt       # request/reply: run a prompt (non-streaming reply = final result)
agents.<agent-id>.prompt.stream # the caller supplies a reply/inbox subject we publish chunks to
agents.<agent-id>.heartbeat    # periodic liveness publishes (fire-and-forget)
```

Using a **queue group** on `prompt` means you can run N replicas of the same agent and NATS load-balances prompts across them — horizontal scaling for free.

### 3. Streaming

NATS request/reply is single-response by design, so for streaming you use the standard NATS streaming idiom:

- The orchestrator sends a prompt request and includes a **reply inbox** (either the automatic `msg.Reply` from `Request`, or an explicit inbox subject it created with `nc.NewInbox()` and subscribed to).
- The agent **publishes multiple messages** to that inbox: one per token/chunk, each tagged as `type: "chunk"`, followed by a terminal `type: "done"` (or `type: "error"`).
- The orchestrator reads from its subscription until it sees the terminal message.

This is exactly how SSE-over-NATS works and is what the TS side will be doing with an `AsyncIterable`. Each chunk is a small JSON envelope so the consumer can distinguish data from completion.

(If you need replay/durability — e.g. the orchestrator reconnects and wants to resume a long generation — you publish the chunks into a **JetStream** stream keyed by request id and let the consumer pull from it. For live interactive prompting, core NATS publishes to the reply inbox are lower-latency and simpler. The code below does the core-NATS streaming and notes where JetStream would slot in.)

### 4. Heartbeats / health

Two complementary mechanisms:

- **Passive health** comes free from Micro: the orchestrator can `$SRV.PING` / `$SRV.STATS` at any time and a healthy agent answers. If it doesn't answer the ping, it's considered down.
- **Active heartbeat**: many agent protocols want a push signal so the orchestrator doesn't have to poll. You publish a small status message to `agents.<agent-id>.heartbeat` (or a shared `agents.heartbeat` subject) every few seconds with `{ id, status, ts, inflight }`. If you want presence to survive orchestrator restarts and be queryable, publish heartbeats into a JetStream stream with a short max-age and `LimitsPolicy`, or use a KV bucket keyed by agent id with a TTL — the KV approach gives you "last seen" semantics cheaply.

---

## Complete Go implementation

```go
// go.mod requires:
//   github.com/nats-io/nats.go        (NATS client)
//   github.com/nats-io/nats.go/micro  (NATS Micro service framework)
//
//   go get github.com/nats-io/nats.go@latest

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/micro"
)

// -----------------------------------------------------------------------------
// Protocol constants — THIS is the block to align with your TS orchestrator.
// -----------------------------------------------------------------------------

const (
	agentID      = "go-research-agent"
	agentName    = "research_agent"
	agentVersion = "1.0.0"

	// Logical subjects. Adjust to whatever the orchestrator publishes to.
	subjDescribe  = "agents." + agentID + ".describe"
	subjPrompt    = "agents." + agentID + ".prompt"
	subjHeartbeat = "agents." + agentID + ".heartbeat"

	// Queue group lets you run N replicas and load-balance prompts.
	queueGroup = "agents-" + agentID

	heartbeatInterval = 5 * time.Second
)

// AgentCard is returned by the describe endpoint so the orchestrator knows
// how to talk to this agent. Mirror the field names your TS side expects.
type AgentCard struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Version      string   `json:"version"`
	Protocol     string   `json:"protocol"`
	Description  string   `json:"description"`
	Capabilities []string `json:"capabilities"`
	Streaming    bool     `json:"streaming"`
	InputSchema  any      `json:"input_schema,omitempty"`
}

// PromptRequest is the inbound payload on the prompt subject.
type PromptRequest struct {
	ID       string         `json:"id"`               // correlation id from the orchestrator
	Prompt   string         `json:"prompt"`           // the user/agent prompt
	Stream   bool           `json:"stream,omitempty"` // true => stream chunks back
	Metadata map[string]any `json:"metadata,omitempty"`
}

// StreamEnvelope is one frame published back to the caller's reply inbox.
// type is one of: "chunk" | "done" | "error".
type StreamEnvelope struct {
	ID    string `json:"id"`
	Type  string `json:"type"`
	Delta string `json:"delta,omitempty"`  // incremental text for "chunk"
	Final string `json:"result,omitempty"` // full result for "done"
	Error string `json:"error,omitempty"`  // message for "error"
}

// Heartbeat is the active liveness signal.
type Heartbeat struct {
	ID       string `json:"id"`
	Status   string `json:"status"` // "ok" | "busy" | "draining"
	TS       int64  `json:"ts"`
	Inflight int64  `json:"inflight"`
}

// inflight tracks how many prompts are currently being processed.
var inflight int64

func main() {
	url := os.Getenv("NATS_URL")
	if url == "" {
		url = nats.DefaultURL // nats://127.0.0.1:4222
	}

	nc, err := nats.Connect(url,
		nats.Name(agentID),
		nats.MaxReconnects(-1), // reconnect forever
		nats.ReconnectWait(time.Second),
		nats.DisconnectErrHandler(func(_ *nats.Conn, e error) {
			log.Printf("disconnected: %v", e)
		}),
		nats.ReconnectHandler(func(c *nats.Conn) {
			log.Printf("reconnected to %s", c.ConnectedUrl())
		}),
	)
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	defer nc.Drain()

	// -------------------------------------------------------------------------
	// 1 + 2. Register as a NATS Micro service and expose agent endpoints.
	//        This auto-subscribes us to $SRV.PING / $SRV.INFO / $SRV.STATS,
	//        which is how the orchestrator DISCOVERS us.
	// -------------------------------------------------------------------------
	svc, err := micro.AddService(nc, micro.Config{
		Name:        agentName,
		Version:     agentVersion,
		Description: "Go research agent (Synadia Agent Protocol compliant)",
		// Metadata travels in $SRV.INFO responses; the orchestrator reads it
		// to filter "agents" out of all services and learn capabilities.
		Metadata: map[string]string{
			"kind":         "agent",
			"agent.id":     agentID,
			"protocol":     "synadia-agent/0.1",
			"capabilities": "research,summarize",
			"streaming":    "true",
		},
		// Fired on any unhandled async error from the service's subscriptions.
		ErrorHandler: func(_ micro.Service, err *micro.NATSError) {
			log.Printf("service error on %q: %v", err.Subject, err.Description)
		},
	})
	if err != nil {
		log.Fatalf("add service: %v", err)
	}
	defer svc.Stop()

	// Group endpoints under a common prefix for tidy stats.
	root := svc.AddGroup("agents." + agentID)

	// describe: request/reply -> returns the agent card.
	if err := root.AddEndpoint("describe",
		micro.HandlerFunc(handleDescribe),
		micro.WithEndpointSubject("describe"),
	); err != nil {
		log.Fatalf("add describe: %v", err)
	}

	// prompt: request/reply, queue-balanced across replicas.
	// Non-streaming requests get one reply. Streaming requests get many
	// publishes to msg.Reply() ending in a "done"/"error" frame.
	if err := root.AddEndpoint("prompt",
		micro.HandlerFunc(func(req micro.Request) { handlePrompt(nc, req) }),
		micro.WithEndpointSubject("prompt"),
		micro.WithEndpointQueueGroup(queueGroup),
	); err != nil {
		log.Fatalf("add prompt: %v", err)
	}

	log.Printf("agent %q online: describe=%s prompt=%s", agentID, subjDescribe, subjPrompt)

	// -------------------------------------------------------------------------
	// 4. Active heartbeat loop.
	// -------------------------------------------------------------------------
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go heartbeatLoop(ctx, nc)

	// Block until SIGINT/SIGTERM, then drain gracefully.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Println("shutting down…")
}

// handleDescribe returns the agent card so the orchestrator can prompt us.
func handleDescribe(req micro.Request) {
	card := AgentCard{
		ID:           agentID,
		Name:         agentName,
		Version:      agentVersion,
		Protocol:     "synadia-agent/0.1",
		Description:  "Researches a topic and returns a summary.",
		Capabilities: []string{"research", "summarize"},
		Streaming:    true,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"prompt": map[string]any{"type": "string"},
			},
			"required": []string{"prompt"},
		},
	}
	// Respond() marshals to JSON and replies on req.Reply().
	if err := req.RespondJSON(card); err != nil {
		log.Printf("describe respond: %v", err)
	}
}

// handlePrompt runs the prompt. If req.Stream is set, we stream chunks to the
// caller's reply inbox; otherwise we send a single final reply.
func handlePrompt(nc *nats.Conn, req micro.Request) {
	atomic.AddInt64(&inflight, 1)
	defer atomic.AddInt64(&inflight, -1)

	var pr PromptRequest
	if err := json.Unmarshal(req.Data(), &pr); err != nil {
		// Micro's structured error: visible in $SRV.STATS too.
		_ = req.Error("400", "invalid prompt payload", nil)
		return
	}

	reply := req.Reply() // the inbox the orchestrator is listening on
	if reply == "" {
		log.Printf("prompt %s has no reply subject; dropping", pr.ID)
		return
	}

	// ---- Streaming path -----------------------------------------------------
	if pr.Stream {
		full := ""
		for _, tok := range runAgentStreaming(pr.Prompt) {
			full += tok
			frame, _ := json.Marshal(StreamEnvelope{
				ID: pr.ID, Type: "chunk", Delta: tok,
			})
			// Publish each chunk to the caller's inbox. The orchestrator
			// reads these off its subscription until it sees "done".
			if err := nc.Publish(reply, frame); err != nil {
				log.Printf("publish chunk: %v", err)
				return
			}
		}
		done, _ := json.Marshal(StreamEnvelope{ID: pr.ID, Type: "done", Final: full})
		_ = nc.Publish(reply, done)
		_ = nc.Flush()
		return
	}

	// ---- Non-streaming path -------------------------------------------------
	result := runAgent(pr.Prompt)
	out, _ := json.Marshal(StreamEnvelope{ID: pr.ID, Type: "done", Final: result})
	if err := req.Respond(out); err != nil {
		log.Printf("prompt respond: %v", err)
	}
}

// heartbeatLoop publishes liveness every heartbeatInterval. The orchestrator
// can also $SRV.PING us at any time (handled automatically by Micro).
func heartbeatLoop(ctx context.Context, nc *nats.Conn) {
	t := time.NewTicker(heartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			hb, _ := json.Marshal(Heartbeat{
				ID:       agentID,
				Status:   statusFromInflight(),
				TS:       time.Now().UnixMilli(),
				Inflight: atomic.LoadInt64(&inflight),
			})
			if err := nc.Publish(subjHeartbeat, hb); err != nil {
				log.Printf("heartbeat publish: %v", err)
			}
		}
	}
}

func statusFromInflight() string {
	if atomic.LoadInt64(&inflight) > 0 {
		return "busy"
	}
	return "ok"
}

// -----------------------------------------------------------------------------
// Replace these stubs with your real agent (LLM call, tool loop, etc.).
// -----------------------------------------------------------------------------

func runAgent(prompt string) string {
	return fmt.Sprintf("processed: %s", prompt)
}

func runAgentStreaming(prompt string) []string {
	// Pretend we're emitting tokens as they're generated.
	return []string{"pro", "cess", "ing: ", prompt}
}
```

---

## How the TypeScript orchestrator interacts with this

So you can verify each piece end-to-end, here's what the TS side does against the Go agent above (using `nats.ts` / `@nats-io/nats-core` plus `@nats-io/services` for discovery):

**Discover** — ping all services and read metadata:

```ts
// $SRV.PING -> every micro service answers with its INFO
const svc = nc.services; // from @nats-io/services
for await (const info of await svc.client().info()) {
  if (info.metadata?.kind === "agent") {
    console.log("found agent", info.metadata["agent.id"], info.metadata.capabilities);
  }
}
```

**Describe** — fetch the agent card:

```ts
const sc = StringCodec();
const r = await nc.request("agents.go-research-agent.describe", sc.encode(""), { timeout: 2000 });
const card = JSON.parse(sc.decode(r.data));
```

**Prompt (non-streaming)** — single request/reply:

```ts
const body = JSON.stringify({ id: crypto.randomUUID(), prompt: "summarize X" });
const r = await nc.request("agents.go-research-agent.prompt", sc.encode(body), { timeout: 30000 });
const { result } = JSON.parse(sc.decode(r.data));
```

**Prompt (streaming)** — subscribe to an inbox, publish with that inbox as the reply, iterate frames until `done`:

```ts
const inbox = nc.createInbox();
const sub = nc.subscribe(inbox);
const body = JSON.stringify({ id, prompt: "summarize X", stream: true });
nc.publish("agents.go-research-agent.prompt", sc.encode(body), { reply: inbox });

for await (const m of sub) {
  const f = JSON.parse(sc.decode(m.data));
  if (f.type === "chunk") process.stdout.write(f.delta);
  else if (f.type === "done") { console.log("\n--", f.result); break; }
  else if (f.type === "error") { console.error(f.error); break; }
}
sub.unsubscribe();
```

**Heartbeats** — subscribe to `agents.*.heartbeat` (or `agents.go-research-agent.heartbeat`) and track `ts`/`status` to know which agents are alive.

---

## Quick verification with the `nats` CLI

You don't need the orchestrator to smoke-test the Go agent:

```bash
# Discovery — does the agent answer the service ping?
nats micro ls
nats micro info research_agent

# Describe
nats req agents.go-research-agent.describe ''

# Non-streaming prompt
nats req agents.go-research-agent.prompt '{"id":"1","prompt":"hello"}'

# Watch heartbeats
nats sub 'agents.>'

# Live stats (request counts, errors, avg processing time)
nats micro stats research_agent
```

---

## Recommendations and gotchas

- **Pin the JSON/subject contract first.** The single biggest risk is field-name / subject mismatch with the TS orchestrator. Get one real prompt message off the wire (`nats sub 'agents.>'` while the orchestrator runs) and shape `PromptRequest`/`StreamEnvelope` to match it exactly. Everything else in this file is protocol-agnostic plumbing.
- **Use a queue group on `prompt`** (done above) so you can scale to N agent replicas and let NATS balance load. Don't put a queue group on `describe`/heartbeat if every replica should answer.
- **Always send a terminal frame** (`done` or `error`) on the streaming path, even on failure — otherwise the orchestrator's async iterator hangs until timeout.
- **Graceful shutdown:** `nc.Drain()` lets in-flight prompts finish and flushes pending publishes before the process exits; emit a final `status:"draining"` heartbeat if your orchestrator honors it.
- **Durability/replay:** if you need a long generation to survive an orchestrator reconnect, publish chunks into a JetStream stream subject like `agent_stream.<request-id>` (a `WorkQueue` or short-`MaxAge` `Limits` stream) and have the consumer pull from the last delivered sequence instead of relying on a transient core-NATS inbox.
- **Auth:** in production, connect with credentials (`nats.UserCredentials("agent.creds")`) and scope the agent's NATS account/permissions to only its `agents.<id>.>` subjects plus the `$SRV.*` it needs.
- **NATS Micro is the right primitive** even without an official agent SDK: it gives you discovery, INFO/STATS introspection, structured errors, and per-endpoint metrics for free, which is most of what an "agent protocol compliance" checklist asks for. You're only adding the agent-specific envelope and streaming convention on top.
