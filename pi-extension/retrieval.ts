export function formatMemoryBlock(retrievedMemories: any[]): string {
  const memoryBlock = retrievedMemories
    .map(m => `- [${String(m.category).toUpperCase()} #${m.id}] ${m.summary}`)
    .join("\n");

  return `<retrieved_memory>\nThese are previously confirmed facts about this project and user. Treat them as authoritative unless the user contradicts them.\n${memoryBlock}\n</retrieved_memory>`;
}
