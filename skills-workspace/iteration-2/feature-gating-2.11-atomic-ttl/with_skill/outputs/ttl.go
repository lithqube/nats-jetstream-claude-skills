package feature

import (
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
)

// EnsureTTLStream creates (or updates) a stream that honours per-message TTL.
//
// AllowMsgTTL is the second gate: without it the Nats-TTL header is SILENTLY
// IGNORED and the message never expires — there is no error to catch. It
// cannot be disabled once enabled, so turning it on is a one-way door.
//
// SubjectDeleteMarkerTTL is optional but usually wanted: when a message ages
// out, the server writes a tombstone message on the same subject
// (Nats-Marker-Reason: MaxAge) so watchers/consumers SEE the expiry instead of
// the value just disappearing. Leave it zero if you don't need that signal.
func EnsureTTLStream(js nats.JetStreamContext, name string, subjects []string) error {
	cfg := &nats.StreamConfig{
		Name:        name,
		Subjects:    subjects,
		Retention:   nats.LimitsPolicy,
		Storage:     nats.FileStorage,
		Replicas:    3,
		Discard:     nats.DiscardOld,
		AllowMsgTTL: true,            // REQUIRED for Nats-TTL to take effect (2.11+)
		SubjectDeleteMarkerTTL: 1 * time.Hour, // keep tombstones around for 1h
	}

	// Idempotent: add, or update an existing stream to enable TTL.
	if _, err := js.AddStream(cfg); err != nil {
		if _, uerr := js.UpdateStream(cfg); uerr != nil {
			return fmt.Errorf("ensure TTL stream %q: add: %w / update: %v", name, err, uerr)
		}
	}
	return nil
}

// PublishWithTTL publishes one message that expires ttl after it is stored.
// Resolution is 1s (the server floors sub-second values). Pass the stream's
// name so we fail loudly if the server is too old instead of letting the
// header be ignored.
//
// Caller must have verified SupportsPerMsgTTL(level) once at startup; this
// guard is the belt-and-braces for a misconfigured deployment.
func PublishWithTTL(js nats.JetStreamContext, level int, subject string, data []byte, ttl time.Duration) (*nats.PubAck, error) {
	if !SupportsPerMsgTTL(level) {
		return nil, fmt.Errorf("server api.level %d does not support per-message TTL (need 2.11+/level 1)", level)
	}
	if ttl < time.Second {
		return nil, fmt.Errorf("TTL resolution is 1s, got %s", ttl)
	}
	return js.PublishMsg(&nats.Msg{
		Subject: subject,
		Data:    data,
		Header: nats.Header{
			// Go-duration string; the server also accepts a bare integer = seconds.
			"Nats-TTL": []string{ttl.String()},
		},
	})
}
