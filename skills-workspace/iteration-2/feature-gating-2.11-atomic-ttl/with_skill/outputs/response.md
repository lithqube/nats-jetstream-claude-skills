# Atomic batch publish + per-message TTL on NATS 2.11 (Go)

## The honest verdict first

Your two asks land on opposite sides of the 2.11 line:

| Feature | On your server (2.11)? | Needs |
|---|---|---|
| **Per-message TTL** (`Nats-TTL`) | ✅ Yes | server 2.11.0, api.level 1, stream `AllowMsgTTL: true` |
| **Atomic batch publish** (all-or-nothing group writes) | ❌ **No** | server **2.12.0**, api.level 2, stream `AllowAtomicPublish` |

Atomic batch publish (the `Nats-Batch-Id` / `Nats-Batch-Sequence` / `Nats-Batch-Commit` mechanism) was **introduced in server 2.12**. It does not exist on 2.11 in any form — there is no flag to turn on, no client option that backports it. So I can set up TTL for you today; atomic batching needs a server upgrade.

**Also worth knowing:** 2.11 reached end-of-life in April 2026. It's November 2026 now, so your production server is already past EOL and receiving no patches. Upgrading to 2.12.5+ (api.level 3) both unblocks atomic batching *and* gets you back onto a supported line. That's the recommendation.

## What I delivered

Four small files. Gate on what the **server** reports (`$JS.API.INFO` → `api.level`), never on a parsed version string — the level even bumped mid-patch-line (2→3 between 2.12.4 and 2.12.5), so a hardcoded version table is wrong somewhere.

- **`capability.go`** — `APILevel(nc)` + `SupportsPerMsgTTL` / `SupportsAtomicBatch`. One runtime probe at startup, branch on it.
- **`ttl.go`** — `EnsureTTLStream` (sets `AllowMsgTTL`, the second gate) and `PublishWithTTL`. **Works on your 2.11 server now.**
- **`batch.go`** — `EnsureAtomicStream` + `PublishAtomicBatch`, **gated to refuse on anything below 2.12**. Ready for the day you upgrade; until then both functions return a clear error instead of silently degrading.
- **`capability_test.go`** — asserts the gating table (2.11 → TTL yes / atomic no). `go test` with no server.

## The two gotchas that cause silent bugs

1. **TTL needs `AllowMsgTTL` on the stream.** Without it the `Nats-TTL` header is *silently ignored* and the message never expires — no error. `EnsureTTLStream` sets it. (It's a one-way door: can't be disabled once on.)
2. **Don't fake atomic batching on 2.11 with a loop of publishes.** That reintroduces exactly the partial-publish window atomic batching exists to close (three of five messages land, then a crash). That's why `PublishAtomicBatch` hard-fails below 2.12 rather than falling back.

## What atomic batch publish does *not* buy you (for when you do upgrade)

It's atomic **within one stream** — not a distributed transaction. It can't make a DB commit atomic with a publish, and it doesn't span streams. In a transactional-outbox design it replaces the *publish* step only: you still need the outbox table (to make the state change + intent-to-publish atomic in your DB) and still need idempotent consumers (the relay can crash after committing the batch, before marking rows sent). Design limits: 1000 msgs/batch, 50 in-flight batches/stream, abandoned after 10s of silence — consume the `$JS.EVENT.ADVISORY.STREAM.BATCH_ABANDONED` advisory.

## Your path

- **Today, on 2.11:** use `ttl.go`. Per-message TTL is fully supported.
- **For atomic batching:** upgrade to **2.12.5+** (also exits EOL). Then `EnsureAtomicStream` + `PublishAtomicBatch` light up with no code change — the gate flips on the level the server reports.

---
Skipped: nothing is compiled or run (no `go.mod`/network here); `AllowAtomicPublish` / `SubjectDeleteMarkerTTL` are the nats.go field names per the documented config — verify against your pinned client version, as field spellings can differ between client releases. Risk to flag: the batch code targets a server you don't yet run, so it's unexercised until you upgrade; and enabling `AllowMsgTTL` can't be undone on a stream.
