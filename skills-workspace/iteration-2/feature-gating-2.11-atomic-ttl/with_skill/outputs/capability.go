package feature

import (
	"encoding/json"
	"time"

	"github.com/nats-io/nats.go"
)

// Capability thresholds, from the JetStream API-level table.
//   2.10.x and earlier -> level 0 (no $JS.API.INFO level field)
//   2.11.0 - 2.11.17   -> level 1
//   2.12.0+            -> level 2+
//
// Gate on the level the SERVER reports, never on a parsed version string:
// the level bumped mid-patch-line (2 -> 3 between 2.12.4 and 2.12.5), so any
// hardcoded version->level table is wrong somewhere.
const (
	levelPerMsgTTL     = 1 // per-message Nats-TTL
	levelAtomicBatch   = 2 // Nats-Batch-* atomic publish
)

// apiInfo is the subset of the $JS.API.INFO response we branch on.
type apiInfo struct {
	API struct {
		Level int `json:"level"`
	} `json:"api"`
}

// APILevel asks the server what it supports. A 2.10 server has no "level"
// field, which unmarshals to 0 — the correct answer for "understands none of
// the post-2.10 features". Returns an error only if the request itself fails.
func APILevel(nc *nats.Conn) (int, error) {
	msg, err := nc.Request("$JS.API.INFO", nil, 5*time.Second)
	if err != nil {
		return 0, err
	}
	var info apiInfo
	if err := json.Unmarshal(msg.Data, &info); err != nil {
		return 0, err
	}
	return info.API.Level, nil
}

// SupportsPerMsgTTL reports whether the server understands Nats-TTL (2.11+).
// The stream must ALSO set AllowMsgTTL, or the header is silently ignored and
// the message never expires — check both, see EnsureTTLStream in ttl.go.
func SupportsPerMsgTTL(level int) bool { return level >= levelPerMsgTTL }

// SupportsAtomicBatch reports whether the server understands the Nats-Batch-*
// headers (2.12+). On 2.11 this is false: atomic batch publish does not exist
// on that server at all. See batch.go for what that means for your design.
func SupportsAtomicBatch(level int) bool { return level >= levelAtomicBatch }
