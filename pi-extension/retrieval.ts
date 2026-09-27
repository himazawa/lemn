export function injectMemoryToUserTail(originalUserMessage: string, retrievedMemories: any[]): string {
  if (retrievedMemories.length === 0) return originalUserMessage;

  const memoryBlock = retrievedMemories
    .map(m => `- [${m.category.toUpperCase()} #${m.id}] ${m.summary}`)
    .join("\n");

  return `<retrieved_memory>\n${memoryBlock}\n</retrieved_memory>\n\nUser Query: ${originalUserMessage}`;
}
