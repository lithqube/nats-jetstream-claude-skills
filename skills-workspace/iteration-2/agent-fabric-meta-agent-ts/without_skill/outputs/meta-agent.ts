/**
 * Meta-agent over the Synadia Agents SDK.
 *
 * Discovers every AI agent registered on the NATS agent fabric, fans a single
 * prompt out to all of them in parallel, and merges their replies.
 *
 * Packages (nothing here needs to run):
 *   @synadia-ai/agents        - agent client / discovery over the agents.* namespace
 *   @nats-io/transport-node   - NATS connection for Node
 */

import { connect, type NatsConnection } from "@nats-io/transport-node";
import { AgentClient, type AgentInfo } from "@synadia-ai/agents";

/** One agent's answer (or failure) to the fanned-out prompt. */
export interface AgentReply {
  id: string;
  name: string;
  ok: boolean;
  text?: string;
  error?: string;
}

export interface MergedResult {
  prompt: string;
  discovered: number;
  answered: number;
  replies: AgentReply[];
  /** Human-readable roll-up of the successful replies. */
  merged: string;
}

/** Pure merge step — kept pure so it is trivially testable. */
export function mergeReplies(prompt: string, replies: AgentReply[]): MergedResult {
  const ok = replies.filter((r) => r.ok);
  const merged = ok
    .map((r) => `## ${r.name} (${r.id})\n${r.text ?? ""}`)
    .join("\n\n");
  return {
    prompt,
    discovered: replies.length,
    answered: ok.length,
    replies,
    merged,
  };
}

/**
 * Discover all registered agents, prompt each one, and merge the answers.
 *
 * @param nc        live NATS connection
 * @param prompt    the single prompt to fan out
 * @param timeoutMs per-agent request timeout (default 30s)
 */
export async function fanOut(
  nc: NatsConnection,
  prompt: string,
  timeoutMs = 30_000,
): Promise<MergedResult> {
  const client = new AgentClient(nc);

  // --- Discovery: ask the agents.* fabric who is out there right now ---
  const agents: AgentInfo[] = await client.discover();
  console.log(`discovered ${agents.length} agent(s):`);
  for (const a of agents) console.log(`  - ${a.name} [${a.id}]`);

  // --- Fan-out: one prompt to every agent, concurrently ---
  // allSettled so a single slow/dead agent never blocks the merge.
  const replies: AgentReply[] = await Promise.all(
    agents.map(async (a): Promise<AgentReply> => {
      try {
        const res = await client.prompt(a.id, prompt, { timeout: timeoutMs });
        return { id: a.id, name: a.name, ok: true, text: res.text };
      } catch (err) {
        return {
          id: a.id,
          name: a.name,
          ok: false,
          error: err instanceof Error ? err.message : String(err),
        };
      }
    }),
  );

  return mergeReplies(prompt, replies);
}

async function main(): Promise<void> {
  const prompt = process.argv.slice(2).join(" ") || "Summarize NATS in one sentence.";
  const nc = await connect({ servers: process.env.NATS_URL ?? "nats://localhost:4222" });
  try {
    const result = await fanOut(nc, prompt);
    console.log(
      `\n${result.answered}/${result.discovered} agent(s) answered:\n\n${result.merged}`,
    );
  } finally {
    await nc.drain();
  }
}

// Run only when invoked directly, not when imported (e.g. by the self-check).
if (import.meta.url === `file://${process.argv[1]}`) {
  main().catch((e) => {
    console.error(e);
    process.exit(1);
  });
}
