/** Assert-based self-check for the pure merge step. Run: `tsx meta-agent.test.ts`. */
import assert from "node:assert/strict";
import { mergeReplies, type AgentReply } from "./meta-agent.ts";

const replies: AgentReply[] = [
  { id: "a1", name: "Alpha", ok: true, text: "hello" },
  { id: "a2", name: "Beta", ok: false, error: "timeout" },
];

const r = mergeReplies("hi", replies);
assert.equal(r.discovered, 2);
assert.equal(r.answered, 1); // failed agents don't count
assert.ok(r.merged.includes("Alpha") && r.merged.includes("hello"));
assert.ok(!r.merged.includes("Beta")); // and don't pollute the roll-up
console.log("ok");
