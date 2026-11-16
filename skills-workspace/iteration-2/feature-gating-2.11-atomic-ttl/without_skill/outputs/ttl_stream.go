// Package natsfeatures shows per-message TTL (NATS server 2.11, GA) and
// atomic batch publishing (see atomic_batch.go) in Go.
//
// Client: github.com/nats-io/nats.go v1.37.0+ (jetstream subpackage).
package natsfeatures

import (
	"context"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// CreateTTLStream creates (or updates) a stream that allows per-message TTL.
// AllowMsgTTL is a 2.11 feature; without it the server rejects Nats-TTL headers.
func CreateTTLStream(ctx context.Context, js jetstream.JetStream, name string, subjects []string) (jetstream.Stream, error) {
	return js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:        name,
		Subjects:    subjects,
		AllowMsgTTL: true,
		// Optional: leave a tombstone when a message expires, so a
		// last-value / KV-style consumer sees the deletion.
		// SubjectDeleteMarkerTTL: time.Hour,
	})
}

// PublishWithTTL publishes one message that the server deletes after ttl.
// Pass ttl <= 0 to publish without a TTL (normal retention applies).
func PublishWithTTL(ctx context.Context, js jetstream.JetStream, subject string, data []byte, ttl time.Duration) (*jetstream.PubAck, error) {
	if ttl <= 0 {
		return js.Publish(ctx, subject, data)
	}
	// WithMsgTTL just sets the Nats-TTL header; the raw header below works on
	// any client version if WithMsgTTL is unavailable:
	//   msg.Header.Set("Nats-TTL", ttl.String())
	return js.Publish(ctx, subject, data, jetstream.WithMsgTTL(ttl))
}

// PublishNeverExpire publishes a message exempt from the stream's MaxAge.
// Only meaningful on a stream configured with MaxAge; "never" overrides it.
func PublishNeverExpire(ctx context.Context, js jetstream.JetStream, subject string, data []byte) (*jetstream.PubAck, error) {
	msg := &nats.Msg{Subject: subject, Data: data, Header: nats.Header{}}
	msg.Header.Set("Nats-TTL", "never")
	return js.PublishMsg(ctx, msg)
}
