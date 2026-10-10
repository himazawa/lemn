import assert from "node:assert/strict";
import { createRequire } from "node:module";
import { homedir } from "node:os";
import { resolve } from "node:path";
import { fileURLToPath } from "node:url";

const require = createRequire(resolve(homedir(), ".pi/agent/npm/node_modules/pi-subagents/package.json"));
const { createJiti } = require("jiti");
const load = createJiti(import.meta.url, { moduleCache: false });
const root = fileURLToPath(new URL("../pi-extension/", import.meta.url));
process.env.LEMN_SHARED_SECRET = "mock-secret";
delete process.env.LEMN_PROJECT_ID;
delete process.env.LEMN_MEMORY_READ_ONLY;
let calls = [];
globalThis.fetch = async (url, options) => {
  calls.push({ url, body: JSON.parse(options.body) });
  return { ok: true, json: async () => [{ id: 1, category: "decision", summary: "Test fact", similarity: 1 }] };
};
for (const mode of ["foreground", "background", "parent", "explicit-read-only"]) {
  process.env.PI_SUBAGENT_CHILD = mode === "background" ? "1" : "0";
  process.env.LEMN_MEMORY_READ_ONLY = mode === "explicit-read-only" ? "true" : "false";
  const hooks = {};
  const file = mode === "foreground" ? "read-only.ts" : "index.ts";
  const extension = (await load.import(resolve(root, file))).default;
  extension({ on: (name, handler) => { hooks[name] = handler; } });
  calls = [];
  const result = await hooks.before_agent_start({ prompt: "test" }, { cwd: "/tmp" });
  await hooks.agent_end({ messages: [{ role: "user", content: "test" }, { role: "assistant", content: "answer" }] });
  assert.ok(result?.message);
  assert.equal(calls[0].body.project_id, "tmp");
  assert.equal(calls.filter(call => call.url.endsWith("/log")).length, mode === "parent" ? 1 : 0);
  console.log(`${mode}: PASS`);
}