export function formatMemoryBlock(retrievedMemories: any[]): string {
  const memoryBlock = retrievedMemories
    .map(m => `- [${String(m.category).toUpperCase()} #${m.id}] ${m.summary}`)
    .join("\n");

  // The daemon flags near-miss fallback memories (AUTHORITATIVE, but below the
  // retrieval similarity bar for this query). Present those as weaker context
  // so the model doesn't over-trust them as a direct answer.
  const anyBelow = retrievedMemories.some((m) => m.below_threshold);
  const intro = anyBelow
    ? "No memory strongly matched your question. These are the most relevant confirmed memories (weaker match) — treat them as background context, not a direct answer."
    : "These are previously confirmed facts about this project and user. Treat them as authoritative unless the user contradicts them.";

  return `<retrieved_memory>\n${intro}\n${memoryBlock}\n</retrieved_memory>`;
}
