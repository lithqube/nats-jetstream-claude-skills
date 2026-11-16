package natsfeatures

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/nats-io/nuid"
)

// --- 2.11 path: single-message envelope (all-or-nothing today) ---------------
//
// On server 2.11 there is NO server-side atomic multi-message batch. A single
// JetStream publish IS atomic, so pack the group into one message and publish
// once. Trade-offs: consumers must unpack, the group counts as one message for
// max_payload / MaxMsgSize, and a per-message TTL applies to the whole group.
// If those don't fit, upgrade to 2.12 and use PublishAtomicBatch2_12 below.

// EnvMsg is one logical message inside an atomic envelope.
type EnvMsg struct {
	Subject string          `json:"subject"`
	Data    json.RawMessage `json:"data"`
}

// PublishAtomicEnvelope writes the whole group as one message (all-or-nothing).
func PublishAtomicEnvelope(ctx context.Context, js jetstream.JetStream, subject string, msgs []EnvMsg) (*jetstream.PubAck, error) {
	if len(msgs) == 0 {
		return nil, fmt.Errorf("empty batch")
	}
	payload, err := json.Marshal(msgs)
	if err != nil {
		return nil, fmt.Errorf("marshal envelope: %w", err)
	}
	return js.Publish(ctx, subject, payload)
}

// UnpackEnvelope reverses PublishAtomicEnvelope on the consumer side.
func UnpackEnvelope(data []byte) ([]EnvMsg, error) {
	var msgs []EnvMsg
	if err := json.Unmarshal(data, &msgs); err != nil {
		return nil, fmt.Errorf("unmarshal envelope: %w", err)
	}
	return msgs, nil
}

// --- 2.12 path: native atomic batch (use after upgrading the server) ---------
//
// Enable AllowAtomicPublish: true on the stream (server 2.12+). Each message
// carries Nats-Batch-Id (same for the group), Nats-Batch-Sequence (1-based),
// and the final message carries Nats-Batch-Commit: 1. Only the commit message
// is acked; that ack confirms the whole group committed atomically. Messages
// may still carry their own Nats-TTL.
//
// Guard it so it can't be run against a 2.11 server by accident — on 2.11 the
// batch headers are ignored and you'd get a partially-written group.

// PublishAtomicBatch2_12 publishes msgs as one native atomic batch.
// nc is the raw connection (intermediate messages are sent without per-message
// acks); js is used only to wait on the commit ack.
func PublishAtomicBatch2_12(ctx context.Context, nc *nats.Conn, js jetstream.JetStream, subject string, msgs []EnvMsg) (*jetstream.PubAck, error) {
	if len(msgs) == 0 {
		return nil, fmt.Errorf("empty batch")
	}
	if err := requireServer212(nc); err != nil {
		return nil, err
	}

	batchID := nuid.Next()
	// Messages 1..N-1: publish without waiting for individual acks.
	for i := 0; i < len(msgs)-1; i++ {
		m := &nats.Msg{Subject: msgs[i].Subject, Data: msgs[i].Data, Header: nats.Header{}}
		m.Header.Set("Nats-Batch-Id", batchID)
		m.Header.Set("Nats-Batch-Sequence", fmt.Sprintf("%d", i+1))
		if err := nc.PublishMsg(m); err != nil {
			return nil, fmt.Errorf("batch msg %d: %w", i+1, err)
		}
	}
	// Final message commits the batch; its ack covers the whole group.
	last := msgs[len(msgs)-1]
	commit := &nats.Msg{Subject: last.Subject, Data: last.Data, Header: nats.Header{}}
	commit.Header.Set("Nats-Batch-Id", batchID)
	commit.Header.Set("Nats-Batch-Sequence", fmt.Sprintf("%d", len(msgs)))
	commit.Header.Set("Nats-Batch-Commit", "1")
	return js.PublishMsg(ctx, commit)
}

// requireServer212 fails fast if the connected server predates atomic batching.
func requireServer212(nc *nats.Conn) error {
	v := nc.ConnectedServerVersion() // e.g. "2.11.4"
	var maj, min int
	if _, err := fmt.Sscanf(v, "%d.%d", &maj, &min); err != nil {
		return fmt.Errorf("cannot parse server version %q: %w", v, err)
	}
	if maj < 2 || (maj == 2 && min < 12) {
		return fmt.Errorf("atomic batch publish needs NATS server 2.12+, connected to %s; use PublishAtomicEnvelope on 2.11", v)
	}
	return nil
}
