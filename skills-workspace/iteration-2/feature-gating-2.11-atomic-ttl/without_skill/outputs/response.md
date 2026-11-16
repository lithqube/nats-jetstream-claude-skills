# Atomic batch publishing + per-message TTL on NATS 2.11 (Go)

## The one thing that changes your plan

On **server 2.11**, your two asks land on opposite sides of a version line:

| Feature | Available on 2.11? | How |
|---|---|---|
| **Per-message TTL** | ✅ Yes, GA in 2.11 | `AllowMsgTTL: true` on the stream + `Nats-TTL` header per message |
| **Atomic batch publish** (all-or-nothing group write) | ❌ **No** | Server-side atomic batching ships in **2.12** (`AllowAtomicPublish` + `Nats-Batch-*` headers). The 2.11 server has no transaction / multi-message commit. |

So this isn't "write some Go and it works." Atomic batch publishing is **feature-gated by server version**. If you deploy the 2.12 batch headers against a 2.11 server, intermediate messages get stored as ordinary messages and the "commit" never happens — you get a partially-written group, which is the exact opposite of what you wanted. There is no client-library flag that backports it.

You have two honest options for the atomic requirement on 2.11:

1. **Upgrade the server to 2.12** and use native atomic batch publishing. (Recommended if all-or-nothing across *distinct subjects/sequences* matters.) Code for this path is in `atomic_batch.go`, gated behind a version check.
2. **Stay on 2.11 and pack the group into one JetStream message.** A single publish is already atomic — JetStream either stores the whole message or none of it. You lose per-logical-message subjects and sequences, and you're bounded by `max_payload` / stream `MaxMsgSize`, but you get true all-or-nothing today. Code in `atomic_batch.go` (`PublishAtomicEnvelope`).

Per-message TTL works on 2.11 regardless; see `ttl_stream.go`.

---

## Per-message TTL (works on 2.11)

Two requirements:

1. Enable it on the stream: `AllowMsgTTL: true`. (Cannot be toggled on a message if the stream doesn't allow it — the publish is rejected.)
2. Set the `Nats-TTL` header per message. Value is a duration (`"30s"`, `"5m"`, `"1h"`) or `"never"`.

Notes:
- `Nats-TTL: never` is only useful on a stream that has a `MaxAge` — it exempts that single message from the age limit.
- TTL expiry removes the message like any limit; if you want a tombstone left behind for "last message per subject" consumers, set `SubjectDeleteMarkerTTL` on the stream.
- The convenience option `jetstream.WithMsgTTL(d)` (recent `nats.go`) just sets that header for you; the raw header works on any client version.

See `ttl_stream.go` for stream creation and a TTL publish.

---

## Atomic batch — native 2.12 path (for when you upgrade)

Enable `AllowAtomicPublish: true` on the stream. Then every message in the group carries:

- `Nats-Batch-Id`: same unique id for all messages in the group
- `Nats-Batch-Sequence`: `1`-based position within the group
- `Nats-Batch-Commit: 1`: **only** on the last message

You publish messages 1..N-1 without waiting for individual acks, and publish the final (commit) message and wait for its `PubAck`. That single ack confirms the whole group was committed atomically; if anything was wrong (gap in sequence, stream rejected one), nothing is stored. This composes with per-message TTL — each message can still carry its own `Nats-TTL`.

See `atomic_batch.go` → `PublishAtomicBatch2_12`.

---

## Atomic batch — 2.11 workaround

Pack the N logical messages into one envelope and publish once. One publish = one atomic write. See `atomic_batch.go` → `PublishAtomicEnvelope` and `UnpackEnvelope`, with a round-trip check in `atomic_batch_test.go`.

Trade-offs you must accept with the envelope approach:
- Consumers must unpack; they can't subscribe to the inner subjects independently.
- The whole group counts as one message for `max_payload` (default 1 MB; raise `max_payload` in server config and `MaxMsgSize` on the stream if needed).
- Per-message TTL applies to the envelope as a whole, not to individual inner messages.

If any of those break your use case, upgrade to 2.12 instead of fighting the envelope.

---

## Files

- `ttl_stream.go` — stream with `AllowMsgTTL`, plus a per-message TTL publish. Runs on 2.11.
- `atomic_batch.go` — both atomic paths: native 2.12 (version-gated) and the 2.11 envelope.
- `atomic_batch_test.go` — round-trip self-check for the envelope pack/unpack.

Client: `github.com/nats-io/nats.go` with the `jetstream` subpackage (use a recent release, v1.37.0+, so `AllowMsgTTL` / `WithMsgTTL` are present). The server version, not the client, is what gates atomic batching.
