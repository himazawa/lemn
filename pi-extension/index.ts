import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { injectMemoryToUserTail } from "./retrieval";

// Must match LIMN_SHARED_SECRET configured on the daemon. Set this in
// Pi's own environment/config — never hardcode it in source.
const LIMN_SHARED_SECRET = process.env.LIMN_SHARED_SECRET || "";

function authHeaders(): Record<string, string> {
  return {
    "Content-Type": "application/json",
    "Authorization": `Bearer ${LIMN_SHARED_SECRET}`
  };
}

export default function limnExtension(pi: ExtensionAPI) {
  if (!LIMN_SHARED_SECRET) {
    // Fail loud in logs rather than silently sending unauthenticated
    // requests that the daemon will reject one by one.
    console.error(
      "[limn] LIMN_SHARED_SECRET is not set — the daemon will reject all requests. " +
      "Set it in Pi's environment to the same value configured on limnd."
    );
  }

  // READ PATH: fetch memories before generation, append to user message tail
  pi.on("before_agent_start", async (event) => {
    const lastMessage = event.messages[event.messages.length - 1];
    if (!lastMessage || lastMessage.role !== "user") return;

    try {
      const response = await fetch("http://localhost:8080/retrieve", {
        method: "POST",
        headers: authHeaders(),
        body: JSON.stringify({
          query: lastMessage.content,
          limit: 5
        })
      });

      if (response.ok) {
        const memories = await response.json();
        if (Array.isArray(memories) && memories.length > 0) {
          lastMessage.content = injectMemoryToUserTail(lastMessage.content, memories);
        }
      }
    } catch (err) {
      // Non-blocking fallback: proceed without memory if daemon is unreachable
    }
  });

  // WRITE PATH: log the completed turn asynchronously
  pi.on("agent_end", async (event) => {
    fetch("http://localhost:8080/log", {
      method: "POST",
      headers: authHeaders(),
      body: JSON.stringify({
        id: `turn_${Date.now()}`,
        user_message: event.userMessage,
        assistant_response: event.assistantResponse,
        tool_calls: (event.toolCalls || []).map((t: any) => ({
          name: t.name,
          diff_text: t.diff || t.output || ""
        }))
      })
    }).catch(() => {});
  });
}
