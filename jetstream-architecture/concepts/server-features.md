# Server Feature Gating

What the server supports depends on its version, and several design-relevant features
arrived after 2.10. Read this before recommending a feature, so you gate it rather than
assuming it exists.

## Detect capability, do not parse a version string

Ask the server. `$JS.API.INFO` returns an `api.level` integer (`JetStreamAPIStats.Level`),
and that is what to branch on for whether the SERVER understands a feature.

**`api.level` is necessary, not sufficient.** It says what the server supports, not what a
given stream has enabled. Several features are gated a second time by stream config, and the
failure mode differs per feature rather than being a uniform error:

| Using | Also requires | If missing |
|---|---|---|
| `Nats-TTL` | `allow_msg_ttl: true` on the stream | header silently IGNORED, message never expires |
| Atomic batch headers | `allow_atomic` on the stream | publish rejected |
| `Nats-Incr` | `allow_msg_counter` | publish rejected |
| KV per-key TTL | bucket created with `markerTTL` | key expires with NO watcher event |

Check the stream config, not just the level. The two silent ones are the dangerous half:
a rejected publish tells you, an ignored TTL does not.

```
nats server report jetstream        # operator-facing
# or, in a client: jsm.getAccountInfo() -> api.level
```

Verified levels, read from `jetstream_versioning.go` at each release tag:

| Server | `api.level` |
|---|---|
| 2.10.x and earlier | file does not exist; treat as 0 |
| 2.11.0 – 2.11.17 | 1 |
| 2.12.0 – 2.12.4 | 2 |
| 2.12.5 – 2.12.15 | 3 |
| 2.14.0 – 2.14.6 | 4 |
| 2.15.0-RC | 5 |

Two traps:

- **The level bumped mid-patch-line**, 2 to 3 between 2.12.4 and 2.12.5. Any hardcoded
  version-to-level table you write will be wrong somewhere.
- **`Nats-Required-Api-Level` does not help you.** It was added in 2.12, so sending it to
  the older servers you want to exclude is a no-op. A non-integer value is rejected rather
  than ignored, and the failure is code 412 / `err_code` 10185. Match the numeric code, not
  the description.

## Support policy

A minor ships roughly every six months, and only two lines get patches: the latest and the
one before it. When a new minor ships, the oldest stops receiving patches. **2.13 does not
exist** — do not gate on it. 2.10 reached EOL in May 2025; 2.11 in April 2026.

## Feature gates

| Feature | Min server | Min level | Also needs |
|---|---|---|---|
| Per-message TTL (`Nats-TTL`), subject delete markers | 2.11.0 | 1 | `allow_msg_ttl` |
| KV per-key TTL | 2.11.0 | 1 | bucket `markerTTL` |
| Consumer pause (`pause_until`) | 2.11.0 | 1 | |
| Priority groups: `overflow`, `pinned_client` | 2.11.0 | 1 | pull consumer, explicit ack |
| Priority `prioritized` policy | 2.12.0 | 2 | pull consumer, explicit ack |
| Atomic batch publish | 2.12.0 | 2 | `allow_atomic` |
| Batch dedup (`Nats-Msg-Id` within a batch) | 2.12.1 | 2 | `allow_atomic` |
| Counter streams (`Nats-Incr`) | 2.12.0 | 2 | `allow_msg_counter` |
| Message scheduling (single / delayed) | 2.12.0 | 2 | `allow_msg_schedules` |
| Cron / repeating schedules | 2.14.0 | 2 | `allow_msg_schedules` |
| Fast-ingest batch publish | 2.14.0 | 4 | `allow_batched` |
| `AckFlowControl`, `$JS.API.CONSUMER.RESET` | 2.14.0 | 4 | |

The scheduling rows are the ones people get wrong. The stream CONFIG for scheduling is API
level 2, but the extra headers that make schedules repeating (`Nats-Schedule-Rollup`,
`Nats-Schedule-Source`, `Nats-Schedule-Time-Zone`) need a 2.14 SERVER. A 2.12 server accepts
the stream config and then does not understand the headers, so gate on the server version for
those, not on the level.

`subject_transforms`, `compression`, `first_seq`, `allow_direct` and
`discard_new_per_subject` are commonly assumed to be new. They were all present in 2.10.

## Per-message TTL (2.11+)

Header `Nats-TTL`, a Go duration (`"5m"`) or a bare integer read as seconds. Minimum
resolution 1s. Ignored unless the stream sets `allow_msg_ttl: true`, **which cannot be
disabled once enabled**.

The part that catches people: when a message ages out and `subject_delete_marker_ttl` is
set, the server writes a *new* message on the same subject carrying
`Nats-Marker-Reason: MaxAge` and `Nats-Rollup: sub`. KV maps that to a watcher operation of
`PURGE`. **Without a marker TTL on the bucket, an expiring key vanishes with no watcher
event at all** — a silent correctness bug for any cache-invalidation design.

In the JS client the support is uneven: `kv.create(k, data, ttl)` takes one,
**`kv.put()` does not**. That is deliberate — if the newest revision expired, an older
history entry would resurface.

## Consumer priority groups (2.11+)

Pull consumers only; push consumers error. Ack policy must be explicit. Max one group,
16 characters.

- **`overflow`** — pulls carry `min_pending` / `min_ack_pending`; either satisfied delivers.
  For shedding load to a secondary worker pool.
- **`pinned_client`** — the server picks one client, stamps `Nats-Pin-Id`, and a mismatched
  id gets a 423. This is the one worth reaching for: single-active-consumer with automatic
  failover, without inventing your own leader election.
- **`prioritized`** (2.12+) — pulls carry `priority` 0-9, lower served first.

**Do not use `failover`.** As of 2.14 the server silently ignores the field.

## Atomic batch publish (2.12+)

Headers `Nats-Batch-Id`, `Nats-Batch-Sequence`, `Nats-Batch-Commit`. Atomic **within one
stream**: either every message in the batch lands in that stream or none does.

Be clear about what that does not buy you. It is not a distributed transaction, so it cannot
make a database commit atomic with a publish, and it does not span streams. In a transactional
outbox it replaces the PUBLISH step only: you still need the outbox table to make the state
change and the intent-to-publish atomic in your database, and you still need idempotent
consumers, because the relay can crash after committing the batch and before marking the rows
sent. What it removes is the partial-publish window where three of five events landed.

Hard limits to design against: **1000 messages per batch, 50 in-flight batches per stream,
abandoned after 10s of silence.** `Nats-Expected-Last-Msg-Id` is rejected outright. There is
a `$JS.EVENT.ADVISORY.STREAM.BATCH_ABANDONED` advisory, and you should consume it — a batch
API that can silently lose a batch is worse than no batch API.

## Strict mode: the breaking change most likely to bite

**From 2.12, the server rejects unknown fields on `$JS.API.*` requests.** The history:

- 2.10: unknown fields silently ignored, nothing logged.
- 2.11: logged as a warning, then parsed leniently anyway.
- 2.12+: hard rejection, code 400 / `err_code` 10025.

So a client that has been sending a junk field since 2.10 got no signal for two years and
now fails outright. If you are reviewing a wrapper or an IaC layer, check it is not
round-tripping `_nats.ver` / `_nats.level` from a `StreamInfo` read back into a create call:
that is the classic form of this bug.

## Deduplication

The default window is 2 minutes and has not changed. One behaviour change in 2.14:
`Duplicates: 0` now genuinely means disabled on a mirror or sourced stream, where it was
previously coerced to 2m. On a normal stream 0 still means 2m.
