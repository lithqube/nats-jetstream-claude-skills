// Meta-agent over the Synadia Agent Protocol (NATS): discover the fleet,
// fan one prompt out to every agent, merge the replies.
//
// SDK: @synadia-ai/agents (caller side) + @nats-io/transport-node.
// APIs are 0.x — verify the installed version. The wire protocol is the stable
// contract (service name "agents", $SRV discovery, typed chunk stream:
// status / response / query, ending at a zero-byte terminator; ignore unknown
// chunk types). Nothing here constructs prompt subjects by hand — discover()
// hands back an agent whose prompt() addresses the discovered subject.

import { connect, type NatsConnection } from "@nats-io/transport-node";
import { Agents } from "@synadia-ai/agents";

// Typed chunk as surfaced by the caller SDK. `response` carries `.text`;
// `query` is a mid-stream question; any other `type` is future protocol we
// must tolerate, not crash on.
type Chunk =
  | { type: "status"; data: string }
  | { type: "response"; text: string }
  | { type: "query"; data: { id: string; reply_subject: string; prompt: string } }
  | { type: string; [k: string]: unknown }; // forward-compat: unknown types

// Minimal view of a discovered agent handle (SDK shape, 0.x).
interface DiscoveredAgent {
  agent: string;
  owner: string;
  name: string;
  prompt(text: string): Promise<AsyncIterable<Chunk>>;
}

const key = (a: { agent: string; owner: string; name: string }) =>
  `${a.agent}/${a.owner}/${a.name}`;

// Automated policy for mid-stream queries. Fanning out to a whole fleet can't
// block on a human per agent, so default-deny destructive asks. Swap for a real
// human-in-the-loop prompt when driving a single agent interactively.
const answerQuery = (_prompt: string): string => "no";

// Consume one agent's chunk stream into accumulated text.
// Branches on `type`: `response` appends, `query` is answered once on its reply
// subject (so the agent resumes), everything else is ignored (forward-compat).
// Pure w.r.t. the stream + onQuery callback, so it is unit-testable without NATS.
export async function collectReply(
  stream: AsyncIterable<Chunk>,
  onQuery: (q: { reply_subject: string; prompt: string }) => void,
): Promise<string> {
  let text = "";
  for await (const msg of stream) {
    if (msg.type === "response") text += (msg as { text?: string }).text ?? "";
    else if (msg.type === "query") onQuery((msg as Extract<Chunk, { type: "query" }>).data);
    // ignore unknown chunk types — the 0.x protocol adds types; don't throw
  }
  return text;
}

interface Reply {
  agent: string;
  text: string;
  error?: string;
}

// Prompt one agent with a timeout so an agent that died mid-fan-out is a failed
// branch, not a hang (a missing zero-byte terminator would otherwise block).
async function promptOne(
  nc: NatsConnection,
  agent: DiscoveredAgent,
  promptText: string,
  timeoutMs = 30_000,
): Promise<Reply> {
  const enc = new TextEncoder();
  const run = (async () => {
    const stream = await agent.prompt(promptText);
    const text = await collectReply(stream, (q) =>
      nc.publish(q.reply_subject, enc.encode(answerQuery(q.prompt))),
    );
    return { agent: key(agent), text };
  })();

  const timeout = new Promise<Reply>((resolve) =>
    setTimeout(
      () => resolve({ agent: key(agent), text: "", error: `timeout after ${timeoutMs}ms` }),
      timeoutMs,
    ),
  );

  try {
    return await Promise.race([run, timeout]);
  } catch (err) {
    return { agent: key(agent), text: "", error: String(err) };
  }
}

async function main() {
  const nc = await connect({ servers: process.env.NATS_URL ?? "nats://localhost:4222" });
  const agents = new Agents({ nc });

  // --- Discovery: who's on the bus right now ($SRV.PING/INFO.agents) ---
  const fleet = (await agents.discover()) as unknown as DiscoveredAgent[];
  console.log(`Discovered ${fleet.length} agent(s):`);
  for (const a of fleet) console.log(`  - ${key(a)}`);
  if (fleet.length === 0) {
    await nc.drain();
    return;
  }

  // --- Fan-out: one prompt to every agent, concurrently ---
  const promptText = process.argv.slice(2).join(" ") || "In one sentence: what are you good at?";
  console.log(`\nFanning out prompt to ${fleet.length} agent(s): ${JSON.stringify(promptText)}\n`);

  // Promise.all over per-branch promises that resolve (never reject) — one slow
  // or dead agent can't sink the merge.
  const replies = await Promise.all(fleet.map((a) => promptOne(nc, a, promptText)));

  // --- Merge: collect-all, keyed by identity ---
  for (const r of replies) {
    console.log(`=== ${r.agent} ===`);
    console.log(r.error ? `[error: ${r.error}]` : r.text);
    console.log();
  }

  await nc.drain();
}

// Entry point (skip when imported, e.g. by the test).
if (import.meta.url === `file://${process.argv[1]}`) {
  main().catch((err) => {
    console.error(err);
    process.exit(1);
  });
}
