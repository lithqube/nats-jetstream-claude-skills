# Go (and any language without an SDK): Protocol-Compliant Agent

**There is no Synadia Agents Go SDK yet — it's planned.** Until then, make a Go agent first-class on the fabric by implementing the wire protocol directly over `nats.go`'s `micro` package. The same approach works for any language with a NATS client: the protocol is the contract, the SDK is just convenience.

Read `concepts/protocol.md` alongside this — every rule below comes from there.

## What "compliant" requires

To be discoverable and promptable by any caller (including TS/Python SDK callers), a Go agent must:

1. Register a `micro` service **named `agents`** (exact string — it's the discovery filter).
2. Serve the `prompt` endpoint on **NATS queue group `agents`**.
3. Advertise metadata: `agent`, `owner`, `session`, `protocol_version`.
4. Emit a mandatory **first chunk** `{"type":"status","data":"ack"}`, then `response` chunks.
5. **Terminate every stream with a zero-byte message and no headers.**
6. Beacon heartbeats to `agents.hb.{agent}.{owner}.{name}` (~30s).

## Install

```bash
go get github.com/nats-io/nats.go
go get github.com/nats-io/nats.go/micro
```

## Minimal compliant agent

Note: `micro`'s request/reply gives you one reply per request. The Agent Protocol streams chunks to the **reply subject** directly, then sends the terminator — so publish chunks to `req.Reply()` yourself rather than using a single `req.Respond`.

```go
package main

import (
	"encoding/json"
	"log"
	"os"
	"os/signal"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/micro"
)

const (
	agentID   = "echo"
	ownerID   = "demo"
	sessionID = "main"
	protoVer  = "0.3"
)

type chunk struct {
	Type string      `json:"type"`
	Data interface{} `json:"data"`
}

func main() {
	nc, err := nats.Connect("nats://localhost:4222")
	if err != nil {
		log.Fatal(err)
	}
	defer nc.Close()

	meta := map[string]string{
		"agent": agentID, "owner": ownerID,
		"session": sessionID, "protocol_version": protoVer,
	}

	// 1. Service MUST be named "agents".
	svc, err := micro.AddService(nc, micro.Config{
		Name:        "agents",
		Version:     "0.1.0",
		Description: "echo agent (protocol-compliant, no SDK)",
		Metadata:    meta,
	})
	if err != nil {
		log.Fatal(err)
	}

	// 2. prompt endpoint on the canonical subject + queue group "agents".
	promptSubj := "agents.prompt." + agentID + "." + ownerID + "." + sessionID
	err = svc.AddEndpoint("prompt",
		micro.HandlerFunc(handlePrompt(nc)),
		micro.WithEndpointSubject(promptSubj),
		micro.WithEndpointQueueGroup("agents"),
		micro.WithEndpointMetadata(map[string]string{
			"max_payload": "1MB", "attachments_ok": "false",
		}),
	)
	if err != nil {
		log.Fatal(err)
	}

	go heartbeat(nc) // 6. liveness beacon

	log.Printf("agent up: %s", promptSubj)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	<-sig
	_ = svc.Stop()
}

func handlePrompt(nc *nats.Conn) micro.HandlerFunc {
	return func(req micro.Request) {
		reply := req.Reply()
		if reply == "" {
			return // nothing to stream to
		}

		// Parse the envelope: JSON if it starts with '{', else plain text.
		prompt := string(req.Data())
		if len(prompt) > 0 && prompt[0] == '{' {
			var env struct {
				Prompt string `json:"prompt"`
			}
			if err := json.Unmarshal(req.Data(), &env); err != nil || env.Prompt == "" {
				streamError(nc, reply, 400, "malformed request")
				return
			}
			prompt = env.Prompt
		}

		// 4. Mandatory first chunk: status/ack.
		send(nc, reply, chunk{Type: "status", Data: "ack"})

		// ... do the work; stream one or more response chunks ...
		send(nc, reply, chunk{Type: "response", Data: "echo: " + prompt})

		// 5. Terminate: zero-byte message, NO headers.
		_ = nc.Publish(reply, nil)
	}
}

func send(nc *nats.Conn, subj string, c chunk) {
	b, _ := json.Marshal(c)
	_ = nc.Publish(subj, b)
}

// Error: header-carrying message, then the empty terminator.
func streamError(nc *nats.Conn, subj string, code int, msg string) {
	m := nats.NewMsg(subj)
	m.Header.Set("Nats-Service-Error-Code", itoa(code))
	m.Header.Set("Nats-Service-Error", msg)
	_ = nc.PublishMsg(m)
	_ = nc.Publish(subj, nil) // terminator
}

func heartbeat(nc *nats.Conn) {
	subj := "agents.hb." + agentID + "." + ownerID + "." + sessionID
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for range t.C {
		hb := map[string]interface{}{
			"agent": agentID, "owner": ownerID, "session": sessionID,
			"instance_id": "go-instance-1",
			"ts":          time.Now().UTC().Format(time.RFC3339),
			"interval_s":  30,
		}
		b, _ := json.Marshal(hb)
		_ = nc.Publish(subj, b)
	}
}

func itoa(i int) string { return string(rune('0'+i/100%10)) + string(rune('0'+i/10%10)) + string(rune('0'+i%10)) }
```

> The `itoa` above is a toy to keep the example dependency-free for a 3-digit code; use `strconv.Itoa` in real code.

## Calling agents from Go

A Go caller does discovery + streaming by hand:

```go
// Discover: who's on the fabric?
msg, _ := nc.Request("$SRV.PING.agents", nil, time.Second)
// (collect multiple responses with a subscription + timeout for the full fleet)

// Prompt: subscribe to a reply inbox, send, consume until the empty terminator.
inbox := nats.NewInbox()
sub, _ := nc.SubscribeSync(inbox)
_ = nc.PublishRequest(promptSubj, inbox, []byte("hello"))

for {
	m, err := sub.NextMsg(30 * time.Second)
	if err != nil {
		break
	}
	if len(m.Data) == 0 && len(m.Header) == 0 {
		break // zero-byte, header-less terminator => stream complete
	}
	if code := m.Header.Get("Nats-Service-Error-Code"); code != "" {
		log.Printf("agent error %s: %s", code, m.Header.Get("Nats-Service-Error"))
		continue
	}
	var c chunk
	_ = json.Unmarshal(m.Data, &c)
	switch c.Type {
	case "response":
		// c.Data is a string or {text, attachments}
	case "query":
		// human-in-the-loop: publish answer once to c.Data.reply_subject
	default:
		// ignore unknown types — forward-compat is required
	}
}
```

## Compliance checklist

- [ ] Service name is exactly `agents`.
- [ ] `prompt` endpoint uses queue group `agents`.
- [ ] Metadata carries `agent`, `owner`, `session`, `protocol_version`.
- [ ] First chunk is `{"type":"status","data":"ack"}`.
- [ ] Stream ends with a zero-byte, header-less message (success *and* error paths).
- [ ] Errors set `Nats-Service-Error-Code` / `Nats-Service-Error` headers before the terminator.
- [ ] Heartbeat to `agents.hb.{agent}.{owner}.{name}` (~30s).
- [ ] Caller learns endpoint subjects from `$SRV.INFO.agents`, not from identity (heartbeat subject excepted).

Get these right and your Go agent is indistinguishable from an SDK agent to any caller — a TS or Python meta-agent will discover and prompt it transparently. When the official Go SDK ships, it will encapsulate exactly this boilerplate.
