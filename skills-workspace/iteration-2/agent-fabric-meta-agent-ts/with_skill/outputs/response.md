# Meta-agent: discover + fan-out over the Synadia Agent Protocol

A meta-agent that discovers the AI agents currently registered on a NATS bus, fans a single
prompt out to all of them concurrently, and merges their replies — keyed by agent identity.

## Files
- `meta-agent.ts` — the meta-agent (discovery → fan-out → merge).
- `meta-agent.test.ts` — assert-based self-check for the chunk-folding logic (no NATS needed).

## Install / run (nothing here was executed)
```bash
npm i @synadia-ai/agents @nats-io/transport-node
# run against a reachable NATS server with some agents registered:
NATS_URL=nats://localhost:4222 npx tsx meta-agent.ts "your prompt here"
# self-check:
npx tsx --test meta-agent.test.ts
```

## How it maps to the protocol
- **Discovery, not hardcoded endpoints.** `agents.discover()` wraps `$SRV.PING/INFO.agents`;
  the returned handles already address the discovered `prompt` subject, so the code never
  builds `agents.prompt.{agent}.{owner}.{name}` by hand.
- **Fan-out + merge.** Each agent is prompted concurrently; `collectReply` consumes the typed
  chunk stream and accumulates `response` text. Merge strategy is **collect-all**, keyed by
  `agent/owner/name` (the natural choice for comparison/ensemble). Swap in race/quorum by
  changing how `replies` is awaited.
- **Forward-compat.** Unknown chunk `type`s are silently ignored (the 0.x protocol adds types).
- **Human-in-the-loop.** `query` chunks are branched out and answered once on their
  `reply_subject`. Fanning out to a fleet can't block on a human per agent, so the policy
  default-denies (`"no"`); for a single interactive agent, route `answerQuery` to a human.
- **Tolerate offline agents.** Each branch has a timeout and resolves (never rejects), so a
  dead agent with a missing zero-byte terminator is a failed branch, not a hang.

## Deliberately out of scope (per the task: focused, discovery + fan-out)
- Liveness/heartbeat tracking (`agents.hb.*`) — only needed to keep a long-lived target set
  fresh between prompts; discovery per run is enough here.
- Metadata filtering (`max_payload`, `attachments_ok`, targeting a subset by `agent`/`owner`) —
  one-liner `fleet.filter(...)` when you want a subset instead of the whole bus.
- Error-header handling (`429` + `retry_after_s` backoff/retry) — surfaced as a failed branch,
  not retried.
