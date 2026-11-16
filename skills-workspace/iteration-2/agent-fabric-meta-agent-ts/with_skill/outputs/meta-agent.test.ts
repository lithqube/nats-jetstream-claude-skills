// Self-check for the chunk-folding rule: response text accumulates, query is
// answered once, unknown chunk types are ignored. No NATS needed.
// Run: node --test (after tsc) or: npx tsx --test meta-agent.test.ts
import { test } from "node:test";
import assert from "node:assert/strict";
import { collectReply } from "./meta-agent.ts";

async function* fakeStream() {
  yield { type: "status", data: "ack" };
  yield { type: "response", text: "Hello, " };
  yield { type: "telemetry", data: { tokens: 3 } }; // unknown future type
  yield { type: "query", data: { id: "1", reply_subject: "_INBOX.x", prompt: "ok?" } };
  yield { type: "response", text: "world." };
}

test("collectReply folds responses, answers query once, ignores unknown types", async () => {
  const queries: string[] = [];
  const text = await collectReply(fakeStream() as never, (q) => queries.push(q.reply_subject));
  assert.equal(text, "Hello, world.");
  assert.deepEqual(queries, ["_INBOX.x"]);
});
