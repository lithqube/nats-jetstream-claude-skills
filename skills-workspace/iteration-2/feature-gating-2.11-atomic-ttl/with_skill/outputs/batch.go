package feature

import (
	"fmt"
	"strconv"
	"time"

	"github.com/nats-io/nats.go"
)

// Atomic batch publish is a 2.12+ feature. It does NOT exist on NATS 2.11.
// The code below is written so it refuses to run on a server that can't honour
// it, rather than silently degrading to a non-atomic sequence of publishes
// (which is the dangerous outcome: three of five messages land, no error).
//
// Mechanism (server 2.12+, stream must set AllowAtomicPublish):
//   - Every message in the batch carries Nats-Batch-Id (shared) and
//     Nats-Batch-Sequence (1..N, strictly increasing).
//   - Only the FINAL message carries Nats-Batch-Commit: 1, and only it gets a
//     PubAck. The server holds the batch and applies it all-or-nothing.
//   - Non-commit messages must be sent WITHOUT waiting for an ack (they get
//     none). Hence core nc.PublishMsg for 1..N-1 and js.PublishMsg for the
//     commit.
//
// Hard limits to design against: 1000 msgs/batch, 50 in-flight batches/stream,
// a batch is abandoned after 10s of silence. There is a
// $JS.EVENT.ADVISORY.STREAM.BATCH_ABANDONED advisory — consume it, a batch API
// that can silently drop a batch is worse than none.
//
// What it does NOT buy you: it is atomic within ONE stream only. It is not a
// distributed transaction — it cannot make a DB commit atomic with the publish.
// In a transactional outbox it replaces the PUBLISH step only; you still need
// the outbox table and idempotent consumers (the relay can crash after the
// commit, before marking rows sent).

const maxBatchSize = 1000

// EnsureAtomicStream creates/updates a stream that accepts atomic batches.
// Fails on a server below 2.12 — AllowAtomicPublish is an unknown field there,
// and from 2.12 unknown fields are hard-rejected anyway, so there is no silent
// path.
func EnsureAtomicStream(js nats.JetStreamContext, level int, name string, subjects []string) error {
	if !SupportsAtomicBatch(level) {
		return fmt.Errorf("server api.level %d does not support atomic batch publish (need 2.12+/level 2); cannot create an atomic stream on this server", level)
	}
	cfg := &nats.StreamConfig{
		Name:               name,
		Subjects:           subjects,
		Retention:          nats.LimitsPolicy,
		Storage:            nats.FileStorage,
		Replicas:           3,
		Discard:            nats.DiscardOld,
		AllowAtomicPublish: true,
	}
	if _, err := js.AddStream(cfg); err != nil {
		if _, uerr := js.UpdateStream(cfg); uerr != nil {
			return fmt.Errorf("ensure atomic stream %q: add: %w / update: %v", name, err, uerr)
		}
	}
	return nil
}

// PublishAtomicBatch writes msgs as one all-or-nothing batch to a single
// stream. Returns the commit PubAck. nc is needed because the non-commit
// messages are sent on core NATS (no per-message ack).
//
// batchID must be unique per batch attempt. Subjects across msgs must all route
// to the same stream — atomicity does not span streams.
func PublishAtomicBatch(nc *nats.Conn, js nats.JetStreamContext, level int, batchID string, msgs []*nats.Msg) (*nats.PubAck, error) {
	if !SupportsAtomicBatch(level) {
		return nil, fmt.Errorf("server api.level %d does not support atomic batch publish (need 2.12+/level 2)", level)
	}
	if len(msgs) == 0 {
		return nil, fmt.Errorf("empty batch")
	}
	if len(msgs) > maxBatchSize {
		return nil, fmt.Errorf("batch of %d exceeds server limit of %d messages", len(msgs), maxBatchSize)
	}

	// Messages 1..N-1: core publish, no ack expected.
	for i := 0; i < len(msgs)-1; i++ {
		m := msgs[i]
		if m.Header == nil {
			m.Header = nats.Header{}
		}
		m.Header.Set("Nats-Batch-Id", batchID)
		m.Header.Set("Nats-Batch-Sequence", strconv.Itoa(i+1))
		if err := nc.PublishMsg(m); err != nil {
			return nil, fmt.Errorf("batch %s msg %d: %w", batchID, i+1, err)
		}
	}

	// Final message: carries the commit flag and is the only one that acks.
	last := msgs[len(msgs)-1]
	if last.Header == nil {
		last.Header = nats.Header{}
	}
	last.Header.Set("Nats-Batch-Id", batchID)
	last.Header.Set("Nats-Batch-Sequence", strconv.Itoa(len(msgs)))
	last.Header.Set("Nats-Batch-Commit", "1")

	ack, err := js.PublishMsg(last, nats.AckWait(10*time.Second))
	if err != nil {
		return nil, fmt.Errorf("batch %s commit: %w", batchID, err)
	}
	return ack, nil
}
