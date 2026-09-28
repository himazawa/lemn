import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { formatMemoryBlock } from "./retrieval";
import { execFileSync } from "node:child_process";
import { basename } from "node:path";

// Must match LEMN_SHARED_SECRET configured on the daemon. Set this in
// Pi's own environment/config — never hardcode it in source.
const LEMN_SHARED_SECRET = process.env.LEMN_SHARED_SECRET || "";
const DAEMON_URL = process.env.LEMN_DAEMON_URL || "http://localhost:8080";

// Memories are scoped to this id; "global" ones are visible from every project.
// Resolved once at load from the git root, so worktrees of the same repo share
// a scope and a plain directory still gets a stable name.
const PROJECT_ID = resolveProjectId();

function resolveProjectId(): string {
  if (process.env.LEMN_PROJECT_ID) return process.env.LEMN_PROJECT_ID;
  try {
    const root = execFileSync("git", ["rev-parse", "--show-toplevel"], {
      encoding: "utf8",
      stdio: ["ignore", "pipe", "ignore"],
    }).trim();
    if (root) return basename(root);
  } catch {
    // not a git repo — fall through to the working directory name
  }
  return basename(process.cwd()) || "global";
}

function authHeaders(): Record<string, string> {
  return {
    "Content-Type": "application/json",
    "Authorization": `Bearer ${LEMN_SHARED_SECRET}`
  };
}

/** Concatenates the text parts of a message, ignoring thinking and tool calls. */
function textOf(content: unknown): string {
  if (typeof content === "string") return content;
  if (!Array.isArray(content)) return "";
  return content
    .filter((part: any) => part?.type === "text")
    .map((part: any) => part.text)
    .join("\n")
    .trim();
}

export default function lemnExtension(pi: ExtensionAPI) {
  if (!LEMN_SHARED_SECRET) {
    // Fail loud in logs rather than silently sending unauthenticated
    // requests that the daemon will reject one by one.
    console.error(
      "[lemn] LEMN_SHARED_SECRET is not set — the daemon will reject all requests. " +
      "Set it in Pi's environment to the same value configured on lemnd."
    );
  }

  // READ PATH: fetch memories before generation and inject them as a separate
  // message — Pi builds the prompt itself, so the user's text is not editable.
  pi.on("before_agent_start", async (event) => {
    if (!event.prompt) return;

    try {
      const response = await fetch(`${DAEMON_URL}/retrieve`, {
        method: "POST",
        headers: authHeaders(),
        body: JSON.stringify({
          query: event.prompt,
          project_id: PROJECT_ID,
          limit: 5
        })
      });
      if (!response.ok) return;

      const memories = await response.json();
      if (!Array.isArray(memories) || memories.length === 0) return;

      return {
        message: {
          customType: "lemn-memory",
          content: formatMemoryBlock(memories),
          display: false
        }
      };
    } catch (err) {
      // Non-blocking fallback: proceed without memory if daemon is unreachable
    }
  });

  // WRITE PATH: log the completed turn asynchronously. agent_end hands over the
  // whole run, so the turn is reassembled from its messages.
  pi.on("agent_end", async (event) => {
    const messages: any[] = (event.messages as any[]) ?? [];

    let userMessage = "";
    for (let i = messages.length - 1; i >= 0; i--) {
      if (messages[i]?.role === "user") {
        userMessage = textOf(messages[i].content);
        break;
      }
    }
    if (!userMessage) return;

    const assistantResponse = messages
      .filter((m) => m.role === "assistant")
      .map((m) => textOf(m.content))
      .filter(Boolean)
      .join("\n");

    // Tool evidence gates supersession, so pair each call with its result text.
    const toolCalls: { name: string; diff_text: string }[] = [];
    for (const message of messages) {
      if (message.role !== "assistant") continue;
      for (const part of message.content ?? []) {
        if (part?.type !== "toolCall") continue;
        const result = messages.find(
          (m) => m.role === "toolResult" && m.toolCallId === part.id
        );
        toolCalls.push({
          name: part.name,
          diff_text: result ? textOf(result.content) : JSON.stringify(part.arguments ?? {})
        });
      }
    }

    fetch(`${DAEMON_URL}/log`, {
      method: "POST",
      headers: authHeaders(),
      body: JSON.stringify({
        id: `turn_${Date.now()}`,
        project_id: PROJECT_ID,
        user_message: userMessage,
        assistant_response: assistantResponse,
        tool_calls: toolCalls
      })
    }).catch(() => {});
  });
}
